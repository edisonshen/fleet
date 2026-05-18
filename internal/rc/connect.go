package rc

import (
	"errors"
	"fmt"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
	"github.com/edisonshen/fleet/internal/workers"
)

// ConnectOpts controls Connect's target selection.
type ConnectOpts struct {
	// CoordID — if set, Connect targets this agent's tmux session.
	// Otherwise Connect falls through to the lock-body holder >
	// coord-spawn-marker holder > require --coord chain.
	CoordID string
}

// ConnectResult is the structured outcome cmd/fleet emits as JSON.
type ConnectResult struct {
	Outcome     string `json:"outcome"`
	CoordID     string `json:"coord_id,omitempty"`
	TmuxSession string `json:"tmux_session,omitempty"`
	Retried     bool   `json:"retried,omitempty"`
	Warn        string `json:"warn,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Connect drives the in-session `/remote-control` slash-command in
// the project's coord tmux pane, via the same submit-verified send-
// keys pattern internal/spawn uses for initial prompts (codex round
// 4-6: 100ms poll / 500ms stable / 30s timeout / 1.5s buffer / ONE
// retry / manual-Enter fallback).
//
// Outcomes:
//   - connected (first verify) → OutcomeConnected, retried=false.
//   - connected (after retry)  → OutcomeConnected, retried=true.
//   - connected (manual fallback) → OutcomeConnected, warn=
//     "prompt_unsubmitted_after_retry — operator press Enter in
//     coord pane to submit /remote-control".
//   - not_enabled → OutcomeNotEnabled, "marker absent".
//   - absent → OutcomeAbsent, "no live coord for project".
func Connect(project string, opts ConnectOpts) (ConnectResult, error) {
	if project == "" {
		return ConnectResult{Outcome: OutcomeError}, errors.New("rc.Connect: empty project")
	}

	if !Enabled(project) {
		return ConnectResult{Outcome: OutcomeNotEnabled}, fmt.Errorf("rc.Connect: marker absent for project %q (run `fleet rc up %s` first)", project, project)
	}

	// codex round-5 P1 + round-6 P2: verify the listener PID is
	// alive AND argv/cwd-matched before driving /remote-control
	// into the coord pane. Without both, a daemon crash, PID=0
	// state record from a spawn failure, or PID-reuse by an
	// unrelated process makes Connect silently report "connected"
	// while the slash command finds no real daemon to attach to.
	if st, sErr := ReadState(project); sErr == nil {
		if st.PID <= 0 {
			return ConnectResult{
					Outcome: OutcomeNotEnabled,
					Error:   fmt.Sprintf("rc.Connect: rc-state.json for project %q has no recorded listener PID (likely a prior spawn failure); run `fleet rc up %s` to respawn", project, project),
				},
				fmt.Errorf("rc.Connect: rc-state.json for project %q has PID=%d; run `fleet rc up %s` to respawn", project, st.PID, project)
		}
		if !workers.IsAlive(st.PID) {
			return ConnectResult{
					Outcome: OutcomeNotEnabled,
					Error:   fmt.Sprintf("rc.Connect: recorded listener PID %d for project %q is not alive (daemon crashed or machine rebooted); run `fleet rc up %s` to respawn", st.PID, project, project),
				},
				fmt.Errorf("rc.Connect: recorded listener PID %d for project %q is not alive; run `fleet rc up %s` to respawn", st.PID, project, project)
		}
		prefix := st.SessionPrefix
		if prefix == "" {
			prefix = SessionPrefix
		}
		if !verifyPIDIsListener(st.PID, prefix, st.WorkingDir) {
			return ConnectResult{
					Outcome: OutcomeNotEnabled,
					Error:   fmt.Sprintf("rc.Connect: recorded listener PID %d for project %q is alive but argv/cwd does not match recorded session_prefix %q + working_dir %q (likely PID reuse or stale state); run `fleet rc up %s` to respawn", st.PID, project, prefix, st.WorkingDir, project),
				},
				fmt.Errorf("rc.Connect: recorded listener PID %d for project %q does not match recorded argv/cwd; run `fleet rc up %s` to respawn", st.PID, project, project)
		}
	} else if errors.Is(sErr, ErrStateMissing) {
		return ConnectResult{
				Outcome: OutcomeNotEnabled,
				Error:   fmt.Sprintf("rc.Connect: rc-state.json missing for project %q (marker present but no listener record); run `fleet rc up %s` to recover", project, project),
			},
			fmt.Errorf("rc.Connect: rc-state.json missing for project %q; run `fleet rc up %s` to recover", project, project)
	} else {
		// codex round-7 P2: corrupt rc-state.json. Same recovery
		// path as `fleet rc reset` (codex R4-2). Refuse rather than
		// silently driving /remote-control into the coord pane —
		// we have no way to prove a listener exists or matches.
		return ConnectResult{
				Outcome: OutcomeNotEnabled,
				Error:   fmt.Sprintf("rc.Connect: rc-state.json for project %q is malformed (%v); run `fleet rc reset %s` then `fleet rc up %s` to recover", project, sErr, project, project),
			},
			fmt.Errorf("rc.Connect: rc-state.json for project %q is malformed: %v", project, sErr)
	}

	rec, err := selectTarget(project, opts.CoordID)
	if err != nil {
		return ConnectResult{Outcome: OutcomeAbsent, Error: err.Error()}, err
	}
	if rec.TmuxSession == "" {
		return ConnectResult{Outcome: OutcomeAbsent, Error: "target coord has no tmux session recorded"},
			fmt.Errorf("rc.Connect: target coord %q has empty TmuxSession", rec.ID)
	}

	// Best-effort readiness wait. Failure here doesn't block the send
	// — spawn.go's contract is "send anyway with a warning" so we
	// match (the manual-fallback path will catch a truly unready
	// pane).
	_ = readyWaitFn(rec.TmuxSession)

	submitted, sendErr := sendFn(rec.TmuxSession, "/remote-control")
	if sendErr != nil {
		return ConnectResult{Outcome: OutcomeError, CoordID: rec.ID, TmuxSession: rec.TmuxSession, Error: sendErr.Error()}, sendErr
	}

	out := ConnectResult{
		Outcome:     OutcomeConnected,
		CoordID:     rec.ID,
		TmuxSession: rec.TmuxSession,
	}
	if !submitted {
		// spawn.SendPromptKeysVerified's submitted=false means the
		// post-retry capture still shows the prompt text — i.e. the
		// "prompt_unsubmitted_after_retry" manual-fallback case
		// (codex round 7 label fidelity).
		out.Warn = "prompt_unsubmitted_after_retry — operator press Enter in coord pane to submit /remote-control"
		out.Retried = true
	}
	return out, nil
}

// selectTarget implements DESIGN §"fleet rc connect — drives the in-
// session /remote-control" step 2 target selection (codex round 3
// P1.2). Authoritative order:
//
//  1. --coord <id> override (callers pass CoordID).
//  2. coord-spawn-marker holder for project (boot-window canonical).
//  3. fail: "multiple coords; specify --coord <id>" (operator listing).
//
// NB: v0.12 omits the lock-body-holder branch from the design doc;
// that requires deeper TUI/coord-state.json integration. The marker
// + --coord chain covers the post-spawn-marker boot window (always
// for ~10-minute window). Lock-body holder lands when the dispatch-
// lifecycle PR3 refactors coord-state.json into a load-bearing
// per-coord record. Tracked as a P3 in WIP.
func selectTarget(project, coordID string) (*agent.Record, error) {
	records, err := agentListFn()
	if err != nil {
		return nil, fmt.Errorf("rc.Connect: list agents: %w", err)
	}

	// (1) explicit --coord <id>
	if coordID != "" {
		for _, r := range records {
			if r != nil && r.ID == coordID {
				if r.Project != "" && r.Project != project {
					return nil, fmt.Errorf("rc.Connect: --coord %q is for project %q, not %q", coordID, r.Project, project)
				}
				if !sessionAliveFn(r.TmuxSession) {
					return nil, fmt.Errorf("rc.Connect: --coord %q is dead (tmux session %q gone)", coordID, r.TmuxSession)
				}
				return r, nil
			}
		}
		return nil, fmt.Errorf("rc.Connect: --coord %q not found in agent list", coordID)
	}

	// (2) coord-spawn-marker holder.
	//
	// codex round-9 P2: validate TaskID before accepting the marker
	// target. A stale or hand-edited marker pointing at a live worker
	// would otherwise have /remote-control keystrokes sent into the
	// worker pane — the fallback at step (3) already filters workers,
	// apply the same gate here.
	coordTaskID := "coord-" + project
	markerID := state.ReadCoordSpawnMarker(project)
	if markerID != "" {
		for _, r := range records {
			if r == nil || r.ID != markerID {
				continue
			}
			if r.Project != "" && r.Project != project {
				continue
			}
			if r.TaskID != coordTaskID {
				return nil, fmt.Errorf("rc.Connect: coord-spawn-marker for project %q points at agent %q whose TaskID is %q (not coord); run `fleet dispatch --coord-spawn ...` to spawn a real coord", project, markerID, r.TaskID)
			}
			if !sessionAliveFn(r.TmuxSession) {
				return nil, fmt.Errorf("rc.Connect: coord-spawn-marker points at %q but its tmux session is dead", markerID)
			}
			return r, nil
		}
	}

	// (3) fail with diagnostic — list candidates.
	//
	// codex round-3 P2: filter on the coord TaskID convention
	// "coord-<project>" so the fallback NEVER targets a worker
	// pane. If a project has live workers but no live coord,
	// `fleet rc connect` would otherwise send `/remote-control`
	// keystrokes into the worker pane and report success — a
	// silo'd misroute we surface as a clear refusal instead.
	// (coordTaskID declared at top of selectTarget.)
	var alive []*agent.Record
	var nonCoordAlive int
	for _, r := range records {
		if r == nil || r.Project != project {
			continue
		}
		if !sessionAliveFn(r.TmuxSession) {
			continue
		}
		if r.TaskID != coordTaskID {
			nonCoordAlive++
			continue
		}
		alive = append(alive, r)
	}
	if len(alive) == 0 {
		if nonCoordAlive > 0 {
			return nil, fmt.Errorf(
				"rc.Connect: no live coord for project %q (%d live non-coord agent(s) found; run `fleet dispatch --coord-spawn ...` to spawn a coord first)",
				project, nonCoordAlive)
		}
		return nil, fmt.Errorf("rc.Connect: no live coord for project %q (run `fleet dispatch --coord-spawn ...` first)", project)
	}
	if len(alive) == 1 {
		// Single live coord, no marker — accept it. Operator's
		// intent is unambiguous.
		return alive[0], nil
	}
	return nil, fmt.Errorf("rc.Connect: multiple coords for project %q; specify --coord <id> (alive: %s)", project, joinAgentIDs(alive))
}

func joinAgentIDs(records []*agent.Record) string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	// Use simple comma-join; cmd/fleet can format better if needed.
	if len(ids) == 0 {
		return ""
	}
	out := ids[0]
	for i := 1; i < len(ids); i++ {
		out += "," + ids[i]
	}
	return out
}

// agentListFn / sessionAliveFn / readyWaitFn / sendFn are test seams
// so unit tests can drive Connect without a real tmux server. T1
// (real-tmux E2E) overrides via SetSendFnForTest + a fake spawn that
// returns a fake tmux session name.

var agentListFn = func() ([]*agent.Record, error) { return agent.List() }

// sessionAliveFn probes whether a tmux session is actually alive
// via `tmux has-session`. selectTarget uses this to filter coord
// candidates BEFORE deciding zero/one/multiple — without a real
// probe (codex round-7 P2), stale agent records survived alongside
// the live coord and made `fleet rc connect` falsely refuse with
// "multiple coords" while marker-pointed-at-dead targets passed
// through until send-keys eventually failed. Tests stub for
// determinism.
var sessionAliveFn = func(session string) bool {
	if session == "" {
		return false
	}
	return tmux.HasSession(session)
}

// readyWaitFn polls the tmux pane for stability before sending. Test
// seam — tests bypass with a no-op. Production wires
// spawn.WaitForReadyToPrompt.
var readyWaitFn = func(session string) error {
	return spawn.WaitForReadyToPrompt(session)
}

// sendFn does the submit-verified send. Production wires
// spawn.SendPromptKeysVerified directly so the 100ms-poll / 500ms-
// stable / 30s-timeout / 1.5s-buffer / ONE-retry / manual-fallback
// contract is shared with initial-prompt sends (no divergence).
var sendFn = func(session, prompt string) (bool, error) {
	return spawn.SendPromptKeysVerified(session, prompt)
}

// SetConnectFnsForTest installs all four Connect seams atomically;
// returns a restore func tests defer. Bundled because Connect tests
// almost always swap them as a set.
func SetConnectFnsForTest(
	list func() ([]*agent.Record, error),
	alive func(string) bool,
	ready func(string) error,
	send func(string, string) (bool, error),
) func() {
	prevList, prevAlive, prevReady, prevSend := agentListFn, sessionAliveFn, readyWaitFn, sendFn
	if list != nil {
		agentListFn = list
	}
	if alive != nil {
		sessionAliveFn = alive
	}
	if ready != nil {
		readyWaitFn = ready
	}
	if send != nil {
		sendFn = send
	}
	return func() {
		agentListFn = prevList
		sessionAliveFn = prevAlive
		readyWaitFn = prevReady
		sendFn = prevSend
	}
}

// IsAlive re-exports workers.IsAlive so callers (sweeper, tests)
// don't need to import workers indirectly.
func IsAlive(pid int) bool { return workers.IsAlive(pid) }
