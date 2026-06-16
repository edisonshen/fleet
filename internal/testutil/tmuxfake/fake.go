package tmuxfake

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/tmux"
)

// Fake is an in-process, memory-backed implementation of tmux.Backend.
// It records sessions and key sequences so routed unit tests exercise the
// real Fleet code paths (spawn / handoff / dispatch) WITHOUT ever execing
// a tmux(1) subprocess — the lever that takes CI under 3 minutes.
//
// Observable parity with the real backend is enforced by the parity
// contract test (parity_test.go): for the ops Fleet routes, Fake must
// match real tmux's success/error/idempotency semantics.
//
// Concurrency: the seam Fake backs is process-global, but a single Fake
// can still see concurrent calls if the code under test spawns goroutines
// (the readiness poll calls CapturePane from one). All state is guarded by
// mu.
type Fake struct {
	mu       sync.Mutex
	sessions map[string]*fakeSession

	// available, when non-nil, overrides Available()'s return (default
	// nil == tmux present and new enough). Set via SetAvailable to test
	// the version-gate error path without a real old tmux.
	available    error
	availableSet bool

	// captureOutput maps session name -> bytes CapturePane returns. A
	// session with no entry returns empty (a live-but-blank pane), which
	// matches real tmux's `capture-pane -p` on a fresh shell.
	captureOutput map[string][]byte

	// statusHints records the last SetStatusHint(hint) per session so a
	// test can assert the hint was applied.
	statusHints map[string]string

	// failSpawn, when non-nil, makes Spawn return it (negative-path
	// coverage for spawn failures without an unwritable real socket).
	failSpawn error

	// clock supplies session-created timestamps; defaults to time.Now.
	clock func() time.Time
}

type fakeSession struct {
	cwd     string
	command []string
	env     []string
	created time.Time
	// keys records every SendKeys arg, flattened in call order, so a test
	// can assert what was typed into the session.
	keys []string
}

// NewFake returns an empty Fake with no sessions.
func NewFake() *Fake {
	return &Fake{
		sessions:      make(map[string]*fakeSession),
		captureOutput: make(map[string][]byte),
		statusHints:   make(map[string]string),
		clock:         time.Now,
	}
}

// Compile-time guard: Fake must satisfy tmux.Backend.
var _ tmux.Backend = (*Fake)(nil)

// InstallFake creates a Fake, installs it as the process tmux backend for
// the duration of the test, and registers t.Cleanup to restore the real
// backend (on pass, fail, or panic — cleanup is the last orchestration
// step). Returns the Fake so the test can pre-seed sessions / assert
// recorded keys.
//
// Tests using InstallFake are NOT t.Parallel-safe against each other: the
// seam is process-global and tmux.Install serializes via a mutex held for
// the test's duration. A parallel test that also installs would block on that
// mutex until the first test's restore runs (serialization, not a lock-
// ordering deadlock), so InstallFake-using tests stay serial within a package.
func InstallFake(t *testing.T) *Fake {
	t.Helper()
	f := NewFake()
	restore := tmux.Install(f)
	t.Cleanup(restore)
	// The spawn path resolves the engine pid by execing `tmux list-panes`
	// + `ps` directly (NOT through the tmux.Backend seam). With the fake
	// installed there is no real session/process, so the production
	// resolver would burn the whole FLEET_PID_RESOLVE_S budget before
	// falling back. Swap in the in-process resolver so routed spawns
	// resolve instantly. Restored before the tmux restore (LIFO Cleanup).
	t.Cleanup(spawn.SetFakeResolver())
	return f
}

// --- tmux.Backend implementation ---

// Available reports tmux availability. Default: nil (present + new
// enough). Override with SetAvailable.
func (f *Fake) Available() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.availableSet {
		return f.available
	}
	return nil
}

// HasSession reports whether the named session exists. Mirrors real
// tmux's bool-only probe (no error channel).
func (f *Fake) HasSession(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sessions[session]
	return ok
}

// SessionAlive mirrors realSessionAlive's tristate. The fake never has a
// transport failure, so it returns (true,nil) when the session exists and
// (false,nil) — definitive dead — when it does not. It never returns the
// (false, err) "ambiguous probe" case; that path is real-tmux-only and is
// covered by the real-backend smoke tests.
func (f *Fake) SessionAlive(session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sessions[session]
	return ok, nil
}

// Spawn creates a detached session. Mirrors realSpawn's validation:
// empty command is rejected; a duplicate session name is rejected the way
// real tmux rejects `new-session -d -s <existing>` ("duplicate session").
func (f *Fake) Spawn(session, cwd string, command, extraEnv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(command) == 0 {
		return fmt.Errorf("tmux.Spawn: empty command")
	}
	if f.failSpawn != nil {
		return f.failSpawn
	}
	if _, ok := f.sessions[session]; ok {
		return fmt.Errorf("tmux new-session %s: duplicate session: %s", session, session)
	}
	f.sessions[session] = &fakeSession{
		cwd:     cwd,
		command: append([]string(nil), command...),
		env:     append([]string(nil), extraEnv...),
		created: f.clock(),
	}
	// Fresh pane: drop any capture bytes a prior same-named session
	// seeded but never cleared (e.g. a test that re-spawns without an
	// intervening Kill). Mirrors real tmux starting the new session with
	// a blank pane (codex iter-2 [P3]).
	delete(f.captureOutput, session)
	return nil
}

// Attach is a no-op success when the session exists, ErrNoSession
// otherwise — the in-process fake cannot replace the process image, so it
// records the intent without execing. Tests that need real attach
// semantics use the real backend under RequireTmux.
func (f *Fake) Attach(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[session]; !ok {
		return fmt.Errorf("%w: %s", tmux.ErrNoSession, session)
	}
	return nil
}

// SendKeys records keys against the session. Returns ErrNoSession if the
// session is gone — matching realSendKeys, whose cleanup-path callers
// ignore that error and proceed to Kill.
func (f *Fake) SendKeys(session string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[session]
	if !ok {
		return fmt.Errorf("%w: %s", tmux.ErrNoSession, session)
	}
	s.keys = append(s.keys, keys...)
	return nil
}

// CapturePane returns the pre-seeded output for the session (empty if
// none seeded — a live blank pane). Returns ErrNoSession if the session
// does not exist, matching realCapturePane's pre-flight SessionAlive
// check that surfaces a dead session as ErrNoSession.
func (f *Fake) CapturePane(session string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[session]; !ok {
		return nil, fmt.Errorf("%w: %s", tmux.ErrNoSession, session)
	}
	out := f.captureOutput[session]
	return append([]byte(nil), out...), nil
}

// SetStatusHint records the hint for the session. Best-effort like the
// real op; records even if the session does not exist (real tmux's
// set-option is global-ish and the callers ignore errors).
func (f *Fake) SetStatusHint(session, hint string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusHints[session] = hint
	return nil
}

// Kill removes the session. Idempotent: a missing session returns nil,
// matching realKill (SessionAlive false -> nil). Also drops any seeded
// capture output for the name so a later Spawn reusing the same session
// starts with a fresh (blank) pane — real tmux gives the replacement a
// new pane, so retaining stale bytes would let a no-output / prompt-
// delivery assertion observe a prior session's content (codex iter-2 [P3]).
func (f *Fake) Kill(session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, session)
	delete(f.captureOutput, session)
	return nil
}

// ListSessions returns existing session names, sorted for determinism.
func (f *Fake) ListSessions() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.sessions))
	for name := range f.sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// ListSessionsWithCreated returns existing sessions paired with their
// recorded creation time, sorted by name for determinism.
func (f *Fake) ListSessionsWithCreated() ([]tmux.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	infos := make([]tmux.SessionInfo, 0, len(f.sessions))
	for name, s := range f.sessions {
		infos = append(infos, tmux.SessionInfo{Name: name, Created: s.created})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// --- test-control helpers (not part of tmux.Backend) ---

// SetAvailable overrides what Available returns (nil == present).
func (f *Fake) SetAvailable(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.available = err
	f.availableSet = true
}

// SetCaptureOutput pre-seeds the bytes CapturePane returns for a session.
func (f *Fake) SetCaptureOutput(session string, out []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captureOutput[session] = append([]byte(nil), out...)
}

// SetFailSpawn makes the next (and subsequent) Spawn calls return err.
func (f *Fake) SetFailSpawn(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSpawn = err
}

// Seed inserts a live session directly (bypassing Spawn validation) so a
// test can start from a populated server.
func (f *Fake) Seed(session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[session]; !ok {
		f.sessions[session] = &fakeSession{created: f.clock()}
	}
}

// Keys returns a copy of the key sequences recorded for a session.
func (f *Fake) Keys(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[session]
	if !ok {
		return nil
	}
	return append([]string(nil), s.keys...)
}

// StatusHint returns the last hint recorded for a session.
func (f *Fake) StatusHint(session string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusHints[session]
}

// SessionCommand returns the command recorded for a spawned session.
func (f *Fake) SessionCommand(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[session]
	if !ok {
		return nil
	}
	return append([]string(nil), s.command...)
}

// SessionEnv returns the extraEnv recorded for a spawned session.
func (f *Fake) SessionEnv(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[session]
	if !ok {
		return nil
	}
	return append([]string(nil), s.env...)
}

// Count returns the number of live sessions.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}
