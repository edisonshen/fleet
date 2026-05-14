package main

// session_cap.go — the FLEET_MAX_SESSIONS spawn-time guardrail.
//
// Why this file exists: even after the orphan-tmux leak plug (Subagent
// #1) and the atomic coord-swap invariant (Subagent #2), nothing in
// the codebase says "stop spawning when there are already too many
// fleet-* tmux sessions on this host." Without a hard cap, a future
// bug that introduces a similar leak could OOM the operator's machine
// again before anyone notices. The cap doesn't FIX leaks; it makes
// them loud — operator gets a clear error pointing at the prune
// command and the rm command, instead of a slow-motion OOM.
//
// Wired into two CLI entry points that spawn fleet-<id> sessions:
//
//   - cmd/fleet/dispatch.go (runDispatch) — `fleet dispatch <task>`
//     and `fleet dispatch coord-<project> --coord-spawn`.
//   - cmd/fleet/handoff.go (runHandoff) — `fleet handoff <id>`. The
//     `--force-replacement` flag bypasses the precheck for operators
//     who are mid-cleanup and need the successor up first.
//
// internal/handoffop's Resume entrypoint (auto-drain from fleet-guard)
// is also wired through enforceSessionCap so background auto-handoffs
// can't push the count past the cap. That call sits in
// internal/handoffop/Resume; this file owns the cmd/fleet layer.
//
// Best-effort, not a strict mutex (codex iter expectation): two
// concurrent `fleet dispatch` invocations can both pass the count
// check and push the total to max+1 in a race. Acceptable for v1 —
// the cap's job is to stop runaway accumulation (60+ sessions), not
// to enforce an exact ceiling. Documented in the helper.

import (
	"fmt"
	"io"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// SessionCapBypassReason is the symbolic reason a particular spawn
// path bypassed the cap. Only used in log/banner output so operators
// can see WHY their spawn was let through despite the count being
// over.
type SessionCapBypassReason string

const (
	// SessionCapBypassForceReplacement: the operator passed
	// --force-replacement on a handoff. They're actively doing
	// cleanup; the cap would block the very command meant to make
	// room. Trust the operator's explicit override.
	SessionCapBypassForceReplacement SessionCapBypassReason = "force-replacement"
)

// sessionListFn / sessionExistsFn are package-level vars so tests can
// inject fakes. Production uses tmux.ListSessions + a closure over
// agentRecordExists (defined in maintenance.go). Production wiring
// happens at first use via the helpers in this file.
var (
	sessionListFn   func() ([]string, error)
	sessionExistsFn func(id string) bool
)

// productionSessionListFn returns tmux.ListSessions. Wrapping in a
// thunk keeps the test-overrideable var clean of an init() that would
// run for every test that imports this package.
func productionSessionListFn() func() ([]string, error) {
	return tmux.ListSessions
}

// productionSessionExistsFn wraps agentRecordExists (from
// maintenance.go) for the same reason.
func productionSessionExistsFn() func(id string) bool {
	return agentRecordExists
}

// enforceSessionCap is the spawn-time guardrail. Returns a non-nil
// error when the current count of fleet-<id> tmux sessions is at or
// above FLEET_MAX_SESSIONS. The error message lists the live + orphan
// breakdown and points at the maintenance prune-orphan-tmux command
// (Subagent #1) plus `fleet rm <id>` as the two operator-facing
// escape valves.
//
// When `bypass` is non-empty, the precheck is skipped and the bypass
// reason is logged to stderr. Used by handoff's --force-replacement
// flag (operator escape hatch — when they're DOING the cleanup and
// need a successor up first).
//
// When the tmux list probe itself fails (binary missing, broken
// socket), the gate logs to stderr and returns nil — refusing to
// spawn on a transient probe failure would be worse than letting
// one spawn through. The brief calls out this best-effort semantic
// explicitly.
//
// stderr may be nil — tests pass nil to assert pure return value.
func enforceSessionCap(stderr io.Writer, bypass SessionCapBypassReason) error {
	if bypass != "" {
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr,
				"note: skipping FLEET_MAX_SESSIONS precheck (bypass=%s)\n",
				bypass)
		}
		return nil
	}
	listFn := sessionListFn
	if listFn == nil {
		listFn = productionSessionListFn()
	}
	existsFn := sessionExistsFn
	if existsFn == nil {
		existsFn = productionSessionExistsFn()
	}
	counts, err := state.CountFleetSessions(listFn, existsFn)
	if err != nil {
		// Probe failure (tmux unreachable). Don't block — log and
		// proceed. The cap is a safety net, not a load-bearing gate;
		// blocking on probe errors would risk locking the operator
		// out of their own fleet during transient tmux issues.
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr,
				"warning: FLEET_MAX_SESSIONS precheck could not enumerate tmux sessions (%v); proceeding without cap enforcement\n",
				err)
		}
		return nil
	}
	max := state.MaxSessions(stderr)
	if counts.Total() < max {
		return nil
	}
	// Multiline message renders cleanly under cobra's `error: <err>`
	// prefix at process exit. Drop trailing punctuation to satisfy
	// staticcheck ST1005; body still reads as three operator-facing
	// directives.
	return fmt.Errorf(
		"fleet: refusing to spawn — already at FLEET_MAX_SESSIONS=%d tmux sessions (%d live, %d orphan)\n"+
			"  run `fleet maintenance prune-orphan-tmux --kill` to reap orphans, or `fleet rm <id>` for retired agents\n"+
			"  override with FLEET_MAX_SESSIONS=<higher>; consider running `fleet status` first",
		max, counts.Live, counts.Orphan)
}
