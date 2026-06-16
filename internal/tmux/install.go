package tmux

import (
	"sync"
	"testing"
)

// Backend is the set of tmux operations the function-table seam routes
// through. The default backend is the real `tmux(1)` subprocess (the
// realXxx funcs); tests swap in an in-process memory fake via Install so
// routed unit tests never exec a real tmux. Every shelling export in
// tmux.go has a corresponding method here — a 5-op subset would leave the
// heavily-used SessionAlive/Available/Attach/CapturePane ops shelling out
// and defeat the "0 real tmux" gate.
//
// SessionName is intentionally absent: it is a pure formatter with no
// subprocess, so it is not part of the swappable surface.
type Backend interface {
	Available() error
	HasSession(session string) bool
	SessionAlive(session string) (bool, error)
	Spawn(session, cwd string, command, extraEnv []string) error
	Attach(session string) error
	SendKeys(session string, keys ...string) error
	CapturePane(session string) ([]byte, error)
	SetStatusHint(session, hint string) error
	Kill(session string) error
	ListSessions() ([]string, error)
	ListSessionsWithCreated() ([]SessionInfo, error)
}

// installMu serializes Install/restore so a test that forgets t.Parallel
// constraints can't interleave a half-swapped table with another's. The
// seam is process-global state; a test that swaps it must own it for the
// duration. Tests that install the fake are therefore NOT parallel-safe
// against each other within a package unless they coordinate — the
// install helper documents this.
var installMu sync.Mutex

// Install swaps the package function table to route every shelling op
// through b, and returns a restore func that puts the real backend back.
// It panics if called outside `go test` (the seam is a test-only swap
// mechanism; production must always use the real tmux backend).
//
// Callers should defer the returned restore (or register it with
// t.Cleanup) so the real backend is restored even on test failure/panic.
//
//	caller ──▶ tmux.Spawn(...) ──▶ spawnFn ──▶ b.Spawn (while installed)
//	restore() ──▶ spawnFn ──▶ realSpawn (back to subprocess)
func Install(b Backend) (restore func()) {
	if !testing.Testing() {
		panic("tmux.Install: refusing to swap the tmux backend outside go test")
	}
	if b == nil {
		panic("tmux.Install: nil backend")
	}
	installMu.Lock()
	prev := struct {
		available               func() error
		hasSession              func(string) bool
		sessionAlive            func(string) (bool, error)
		spawn                   func(string, string, []string, []string) error
		attach                  func(string) error
		sendKeys                func(string, ...string) error
		capturePane             func(string) ([]byte, error)
		setStatusHint           func(string, string) error
		kill                    func(string) error
		listSessions            func() ([]string, error)
		listSessionsWithCreated func() ([]SessionInfo, error)
	}{
		available: availableFn, hasSession: hasSessionFn, sessionAlive: sessionAliveFn,
		spawn: spawnFn, attach: attachFn, sendKeys: sendKeysFn, capturePane: capturePaneFn,
		setStatusHint: setStatusHintFn, kill: killFn, listSessions: listSessionsFn,
		listSessionsWithCreated: listSessionsWithCreatedFn,
	}

	availableFn = b.Available
	hasSessionFn = b.HasSession
	sessionAliveFn = b.SessionAlive
	spawnFn = b.Spawn
	attachFn = b.Attach
	sendKeysFn = b.SendKeys
	capturePaneFn = b.CapturePane
	setStatusHintFn = b.SetStatusHint
	killFn = b.Kill
	listSessionsFn = b.ListSessions
	listSessionsWithCreatedFn = b.ListSessionsWithCreated

	var once sync.Once
	return func() {
		once.Do(func() {
			availableFn = prev.available
			hasSessionFn = prev.hasSession
			sessionAliveFn = prev.sessionAlive
			spawnFn = prev.spawn
			attachFn = prev.attach
			sendKeysFn = prev.sendKeys
			capturePaneFn = prev.capturePane
			setStatusHintFn = prev.setStatusHint
			killFn = prev.kill
			listSessionsFn = prev.listSessions
			listSessionsWithCreatedFn = prev.listSessionsWithCreated
			installMu.Unlock()
		})
	}
}

// Compile-time drift guard: if a realXxx signature changes, realBackend
// stops satisfying Backend and the build fails before any test runs.
var _ Backend = realBackend{}

// realBackend adapts the package-level realXxx funcs to the Backend
// interface. Used only by the compile-time assertion above and available
// to tests that want to delegate to the real tmux for a subset of ops.
type realBackend struct{}

func (realBackend) Available() error                         { return realAvailable() }
func (realBackend) HasSession(s string) bool                 { return realHasSession(s) }
func (realBackend) SessionAlive(s string) (bool, error)      { return realSessionAlive(s) }
func (realBackend) Spawn(s, c string, cmd, e []string) error { return realSpawn(s, c, cmd, e) }
func (realBackend) Attach(s string) error                    { return realAttach(s) }
func (realBackend) SendKeys(s string, k ...string) error     { return realSendKeys(s, k...) }
func (realBackend) CapturePane(s string) ([]byte, error)     { return realCapturePane(s) }
func (realBackend) SetStatusHint(s, h string) error          { return realSetStatusHint(s, h) }
func (realBackend) Kill(s string) error                      { return realKill(s) }
func (realBackend) ListSessions() ([]string, error)          { return realListSessions() }
func (realBackend) ListSessionsWithCreated() ([]SessionInfo, error) {
	return realListSessionsWithCreated()
}

// RealBackend returns a Backend that delegates to the real `tmux(1)`
// subprocess. Parity tests install the fake, then compare its observable
// behavior against RealBackend under tmuxtest.RequireTmux.
func RealBackend() Backend { return realBackend{} }
