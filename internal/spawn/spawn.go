// Package spawn is the shared "create a new agent in a tmux session
// and write its record" code path used by both `fleet dispatch` (no
// origin) and `fleet handoff` (origin = OldRecord).
//
// Centralizing here means the chain-field logic (HandoffNumber,
// LastHandoffPath, HandoffType) lives in one place. Dispatch and
// handoff differ only in whether they pass an OldRecord.
//
// Failure mode: if the tmux session comes up but the agent record
// write fails, kill the orphan session before returning. Operators
// must never see a "ghost" session with no record (or a record
// pointing at a dead session).
package spawn

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/tmux"
)

// SendInitialPrompt timing knobs. Production needs to ride out
// claude code's startup animation (logo + spinner before the input
// box appears) without staking the delivery on a single fixed sleep
// — custom engine wrappers (per oldRec.Command) can take longer than
// any one number we'd pick. Instead: poll the pane content; once it
// stops changing for stableWindow we treat the agent as ready.
//
// FLEET_INITIAL_PROMPT_STABLE_MS / FLEET_INITIAL_PROMPT_MAX_MS let
// tests pin small values so the suite doesn't pay multi-second
// real-world waits; production uses the constants below.
const (
	defaultInitialPromptStableWindow = 500 * time.Millisecond
	defaultInitialPromptMaxWait      = 30 * time.Second
	initialPromptPollInterval        = 100 * time.Millisecond
)

func initialPromptStableWindow() time.Duration {
	return envDuration("FLEET_INITIAL_PROMPT_STABLE_MS",
		defaultInitialPromptStableWindow)
}

func initialPromptMaxWait() time.Duration {
	return envDuration("FLEET_INITIAL_PROMPT_MAX_MS",
		defaultInitialPromptMaxWait)
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if s := os.Getenv(key); s != "" {
		if ms, err := strconv.Atoi(s); err == nil && ms >= 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return fallback
}

// SendInitialPrompt waits for the tmux session's pane to stabilize
// (claude code's startup animation finished, input box rendered),
// then types prompt + Enter. Used by both handoff entry points
// (cmd/fleet/handoff.go's inline retire and handoffop.retireOldAgent)
// to make the replacement agent autonomously pick up its predecessor's
// work.
//
// Centralizing the wait+send pair in one helper means crash-recovery's
// retireOldAgent calls the SAME code as the happy path — so the prompt
// gets delivered exactly once even when a previous run crashed between
// spawn and retire. (codex review iter-1 P1: fixed sleep + crash
// window were two separate ways to silently drop the prompt.)
//
// Empty prompt is a silent no-op so callers can pass
// handoff.ResumePrompt(docPath) without nil-checking docPath.
//
// Best-effort: a tmux error returns nil-or-error to the caller, but
// the caller should not roll back the spawn — the agent record +
// session are valid, and the operator can attach + type the prompt
// manually if the auto-resume failed.
func SendInitialPrompt(session, prompt string) error {
	if prompt == "" {
		return nil
	}
	if err := waitForPaneStable(session,
		initialPromptStableWindow(),
		initialPromptMaxWait()); err != nil {
		// Stability poll didn't converge before maxWait. Send anyway
		// — keys land in tmux's pty buffer and claude consumes them
		// once it's ready. Better to over-send than to skip.
		_, _ = fmt.Fprintf(os.Stderr,
			"warning: initial-prompt readiness poll for %s did not converge: %v (sending anyway)\n",
			session, err)
	}
	return tmux.SendKeys(session, prompt, "Enter")
}

// waitForPaneStable polls tmux capture-pane every
// initialPromptPollInterval; returns nil when the pane content has
// not changed for at least stableWindow, or an error if maxWait
// elapses without convergence.
//
// "Stable" is a coarse heuristic for "agent is idle waiting for
// input" — works for any wrapper that prints a startup banner then
// settles, regardless of whether it's claude, codex, or a custom
// shell. Empty captures count toward stability (codex review iter-4
// P2): wrappers that `clear` the screen at startup leave the pane
// blank-but-idle, and gating on len(cur) > 0 would mean those
// wrappers never converge and every handoff stalls for the full
// maxWait.
func waitForPaneStable(session string, stableWindow, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	var prev []byte
	first := true
	stableSince := time.Time{}
	for {
		cur, err := tmux.CapturePane(session)
		if err != nil {
			return err
		}
		// First iteration always counts as "changed" — we have nothing
		// to compare against. Subsequent iterations: equal to prev =>
		// stable, otherwise reset the stable timer.
		if !first && bytes.Equal(cur, prev) {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= stableWindow {
				return nil
			}
		} else {
			stableSince = time.Time{}
		}
		prev = cur
		first = false
		if time.Now().After(deadline) {
			return fmt.Errorf("pane did not stabilize within %s", maxWait)
		}
		time.Sleep(initialPromptPollInterval)
	}
}

// Options control a single Spawn call.
//
// OldRecord nil → fresh dispatch (uses TaskID, Project, no chain).
// OldRecord non-nil → handoff replacement (inherits TaskID/Project/
// Engine/Role/Mode from old, increments HandoffNumber, sets
// LastHandoffPath = NewDocPath if non-empty).
type Options struct {
	OldRecord *agent.Record

	// NewDocPath is the path to the handoff doc this new agent
	// inherits. Only meaningful when OldRecord is non-nil. Stored on
	// the new record's LastHandoffPath so the *next* handoff can
	// build the chain forward.
	NewDocPath string

	// TaskID and Project are used only for fresh dispatch. Ignored
	// when OldRecord is non-nil — the handoff inherits those fields.
	TaskID  string
	Project string

	// Cwd is the working directory for the spawned tmux session.
	// Empty inherits the caller's cwd.
	Cwd string

	// Command is the argv of the agent process inside tmux. Defaults
	// to {"claude"} via the dispatch CLI; spawn.Spawn does not
	// default — callers must pass it explicitly so the contract is
	// obvious.
	Command []string

	// PreAllocatedID, if non-empty, overrides the agent.NewID()
	// fresh-allocation. Handoff uses this to journal the successor
	// ID BEFORE spawning, closing the crash window between spawn
	// and journal-write. Empty (the dispatch path) means generate
	// a fresh ID inside Spawn.
	PreAllocatedID string
}

// Spawn creates a fresh agent (or a handoff replacement, if
// opts.OldRecord is set), brings up its tmux session, and writes
// the agent record. Returns the populated record.
//
// The caller is responsible for killing the OLD agent's tmux session
// (graceful /exit + grace + Kill) and archiving the OLD record.
// Spawn handles only the *new* agent.
//
// On any failure after tmux.Spawn succeeds (e.g., record write
// rejected by disk), the orphan tmux session is killed before
// returning so dispatch is exactly-once from the operator's view.
func Spawn(opts Options) (*agent.Record, error) {
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("spawn.Spawn: Command required")
	}

	id := opts.PreAllocatedID
	if id == "" {
		id = agent.NewID()
	}
	session := tmux.SessionName(id)
	rec := agent.New(id)
	rec.TmuxSession = session
	rec.PID = os.Getpid()

	// Capture the resolved cwd so `fleet handoff` can place the
	// replacement in the same project checkout even when invoked
	// from a different shell. Empty opts.Cwd means "inherit caller"
	// — resolve via os.Getwd(). Relative paths from --cwd get
	// canonicalized via filepath.Abs so the record always stores
	// an absolute path, immune to "next handoff invoked from a
	// different shell" wrong-tree spawns.
	//
	// Resolution failures (deleted cwd, unreadable parent) abort
	// here rather than silently writing a record with empty/relative
	// Cwd — that would later trip the "legacy record with no stored
	// cwd" guard at handoff time AND let tmux launch the agent in
	// the tmux server's cwd, not the operator's checkout.
	cwd := opts.Cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current working directory: %w", err)
		}
		cwd = wd
	} else if !filepath.IsAbs(cwd) {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return nil, fmt.Errorf("canonicalize cwd %q: %w", cwd, err)
		}
		cwd = abs
	}
	rec.Cwd = cwd

	// Capture the launch command so `fleet handoff` preserves any
	// custom engine/wrapper the operator dispatched with.
	rec.Command = append([]string(nil), opts.Command...)

	// FLEET_AGENT_ID is propagated into the agent's process env so
	// fleet-guard (4b/c) can identify which agent record to update
	// without round-tripping via tmux session name parsing.
	extraEnv := []string{"FLEET_AGENT_ID=" + id}

	if opts.OldRecord != nil {
		// Inherit task identity from outgoing agent.
		rec.TaskID = opts.OldRecord.TaskID
		rec.Project = opts.OldRecord.Project
		// Inherit engine + role + mode so the replacement runs in the
		// same configuration. v1.1 engine adapter relies on this for
		// per-agent engine continuity.
		if opts.OldRecord.Engine != "" {
			rec.Engine = opts.OldRecord.Engine
		}
		if opts.OldRecord.Role != "" {
			rec.Role = opts.OldRecord.Role
		}
		if opts.OldRecord.Mode != "" {
			rec.Mode = opts.OldRecord.Mode
		}
		// Chain: handoff_number = old + 1, prev_path = doc just written.
		rec.HandoffNumber = opts.OldRecord.HandoffNumber + 1
		if opts.NewDocPath != "" {
			rec.LastHandoffPath = &opts.NewDocPath
		}
		// Mark the spawn origin so the TUI can render the transition.
		manualType := handoff.TypeManual
		rec.HandoffType = &manualType
	} else {
		rec.TaskID = opts.TaskID
		rec.Project = opts.Project
	}

	// Pass the canonicalized cwd (not opts.Cwd) so the tmux session
	// actually starts in the directory we recorded on rec.Cwd.
	// Otherwise a relative --cwd resolved to one path here could
	// resolve to a different one inside tmux (especially with an
	// existing tmux server), and a future handoff would land the
	// replacement in the wrong checkout.
	if err := tmux.Spawn(session, cwd, opts.Command, extraEnv); err != nil {
		return nil, err
	}
	// Best-effort: pin a "Ctrl-b d to detach" hint into this session's
	// status bar so operators see it persistently while attached.
	// Failure is silent — TUI keybind hints + the wrapped command's
	// in-session banner are fallback discovery paths.
	_ = tmux.SetStatusHint(session, "[Ctrl-b d to detach]")
	if err := rec.Write(); err != nil {
		// Orphan rollback: tmux session up, record missing → operator
		// would see a ghost session in `tmux ls` with no `fleet status`
		// entry. Kill the session so spawn is all-or-nothing.
		_ = tmux.Kill(session)
		return nil, fmt.Errorf("write agent record (orphan tmux session killed): %w", err)
	}
	// NOTE: the handoff resume prompt is typed by the caller's retire
	// path (handoffop.retireOldAgent / cmd/fleet/handoff.go step 11b)
	// via SendInitialPrompt, NOT here. Keeping it out of Spawn means
	// crash recovery's "replacement spawned, retire interrupted"
	// branch — which goes through retireOldAgent directly without
	// re-spawning — still delivers the prompt. See codex review
	// iter-1 P1 / iter-2 P2.
	return rec, nil
}
