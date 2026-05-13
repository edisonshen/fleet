package main

// dispatch_recovery.go — dead-coord detection + synth-handoff wiring
// for `fleet dispatch coord-<project> --project <project> --coord-spawn`
// run against a project whose previous coord left a stale record on disk
// (pid dead AND tmux session gone).
//
// The flow is opt-in via the dead-pid+dead-tmux probe at dispatch entry.
// A live coord — even one whose tmux session is unhappy — is NEVER
// hijacked; the operator might just be on a different tmux server.
//
// Steps the caller wires in:
//
//	1. findRecoveryCandidate(taskID, project, records, pidAlive, sessionAlive)
//	   → dead coord record OR nil
//	2. writeRecoveryHandoffDoc(deadRec, now)
//	   → docPath, err
//	3. spawn.Spawn(spawn.Options{OldRecord: deadRec, NewDocPath: docPath, ...})
//	4. archive deadRec (the caller's existing post-spawn cleanup path)
//
// Step 3's OldRecord branch in spawn.Spawn already does the lineage
// inheritance: handoff_number ++, last_handoff_path = NewDocPath, taskID
// and project carried forward. No new spawn options required.

import (
	"fmt"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/state"
)

// pidAliveFn lets tests stub the kill(0) probe used by
// findRecoveryCandidate. Production wraps the same syscall the
// workers package uses (kill(pid, 0)); the indirection is a var on
// the helper rather than a function arg so the dispatch entry can
// call findRecoveryCandidate with the default probe in one line.
type pidAliveFn func(pid int) bool

// sessionAliveProbe is the tmux liveness probe used by
// findRecoveryCandidate. tmux.HasSession in production; tests stub
// with a closure over a fixed name set.
type sessionAliveProbe func(session string) bool

// findRecoveryCandidate walks the live agent records (output of
// agent.List) looking for a dead coord matching the given task_id +
// project. "Dead" = pid is NOT alive AND tmux session is NOT alive.
//
// Returns the first match (the slice order is agent.List order, which
// is filesystem-iteration order — not stable across runs but stable
// within one run, and the multiple-dead-coords case is itself an
// edge case the caller is expected to clean up post-spawn via
// archive of the predecessor).
//
// Returns nil when no record matches. Production callers MUST NOT
// substitute a recovery synth for any nil case — that would steal a
// fresh dispatch's identity (e.g. an operator typo'd task name that
// happens to look like a coord sentinel).
//
// The pidAliveFn + sessionAliveProbe arguments exist to keep this
// pure-function — no syscall, no exec — so tests can run without
// touching tmux or the kernel. Production passes pidAlive +
// tmux.HasSession.
func findRecoveryCandidate(
	taskID, project string,
	records []*agent.Record,
	pidAlive pidAliveFn,
	sessionAlive sessionAliveProbe,
) *agent.Record {
	for _, r := range records {
		if r == nil {
			continue
		}
		if r.TaskID != taskID || r.Project != project {
			continue
		}
		// Alive pid → bail. We won't replace a live coord even if its
		// tmux session looks unhappy — the operator might be on a
		// different tmux server, and a duplicate dispatch would split
		// the coord's lock body across two agents.
		if r.PID > 0 && pidAlive(r.PID) {
			continue
		}
		// Alive tmux + dead pid is the zombie-shell case (issue #65
		// territory). It's NOT a recovery candidate: a fresh spawn
		// would race a live shell on the same session name. The
		// existing fail-loud handling in the dispatch path handles it
		// via attach failure rather than synth-handoff recovery.
		if r.TmuxSession != "" && sessionAlive(r.TmuxSession) {
			continue
		}
		return r
	}
	return nil
}

// pidAlive is the production pidAliveFn. Mirrors workers.IsAlive (the
// existing kill(pid, 0) probe) — duplicated here rather than imported
// to avoid the workers → handoff dep cycle (workers already depends
// on state; dispatch already depends on workers via the CLI; pulling
// in the lifecycle package would tangle the import graph).
//
// pid <= 0 returns false (nothing to probe). EPERM ("process exists,
// I lack signal permission") returns true — the question is "alive",
// not "kill-able".
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if err == syscall.EPERM {
		return true
	}
	return false
}

// writeRecoveryHandoffDoc synthesizes a recovery-synth handoff doc
// for deadRec from on-disk state and writes it to
// ~/.fleet/handoffs/<dead-id>-<UTC-stamp>.md (the same path layout
// real handoffs use, via state.HandoffPath). Returns the doc path so
// the caller can pass it as spawn.Options.NewDocPath.
//
// Side effect: the dead agent's last_handoff_path is updated to point
// at the synth doc. Why on the DEAD record (which will be archived
// shortly): the doc's chain field is "previous_handoff", and the
// successor reads its predecessor's record at spawn time to populate
// that frontmatter. Without updating the dead record, the chain link
// to the synth doc is lost the moment the dead record is archived.
//
// The handoff doc itself does NOT carry the "previous_handoff: <synth>"
// chain pointer — its previous_handoff IS the synth, so it inherits
// the synth's chain (which may be nil for a never-handoff'd coord).
func writeRecoveryHandoffDoc(deadRec *agent.Record, ts time.Time) (string, error) {
	if deadRec == nil {
		return "", fmt.Errorf("writeRecoveryHandoffDoc: nil deadRec")
	}
	doc, err := handoff.SynthesizeRecovery(deadRec.ID, deadRec.Project, ts)
	if err != nil {
		return "", fmt.Errorf("synthesize recovery doc: %w", err)
	}
	// Chain link: the synth doc's previous_handoff is whatever the dead
	// coord itself last inherited from. nil for a never-handed-off
	// coord. Mirrors handoff.NewManualStub's behavior so the chain
	// invariant survives the recovery.
	doc.PreviousPath = deadRec.LastHandoffPath
	doc.Number = deadRec.HandoffNumber

	docPath, err := state.HandoffPath(deadRec.ID, ts)
	if err != nil {
		return "", fmt.Errorf("resolve handoff doc path: %w", err)
	}
	if err := handoff.Write(doc, docPath); err != nil {
		return "", fmt.Errorf("write handoff doc: %w", err)
	}
	// Update the dead record's last_handoff_path so the chain stays
	// intact across the archive that follows. We rewrite the file
	// atomically via agent.Record.Write — state.WriteAtomic handles
	// the tmp+rename. A crash between Write and the caller's archive
	// is benign: the record file is consistent, and the next dispatch
	// will re-detect dead-pid+dead-tmux and re-run this path
	// (idempotent — the synth doc filename uses a per-run random
	// suffix so we don't overwrite, but the orphan is small and the
	// successor only cares about the last_handoff_path pointer).
	deadRec.LastHandoffPath = &docPath
	if err := deadRec.Write(); err != nil {
		return "", fmt.Errorf("update dead record last_handoff_path: %w", err)
	}
	return docPath, nil
}
