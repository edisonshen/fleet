package handoffop

// AtomicCoordSwap is the single serialized entry point for replacing a
// project's coordinator on the NON-graceful fallback path (a bare/legacy
// successor, or auto-resume off). The coord-spawn marker is GONE
// (TASK-PLAN-coord-lease-sole-identity D3): the coordinator LEASE is the sole
// identity. The identity COMMIT is now ASYNC — the winning standby bumps the
// epoch to NEW only when it acquires the flock freed by retiring OLD.
//
// This helper's job in the lease world: spawn/probe NEW alive, RESERVE the
// handoff on OLD's still-owned lease (naming NEW so a contender that observes
// the freed lease WAITS for NEW instead of spawning a duplicate — coordreconcile
// rule (a)), then RETIRE OLD (/exit + grace + Kill + archive). ReserveHandoff is
// a SOFT TTL guard (a record-only CAS, not a hard rename): ok=false (no lease
// record / bare coord) degrades benignly to the winner-delivery guarantee, and
// the reservation self-heals if NEW never acquires (it expires and discovery
// re-resolves). There is NO synchronous commit point here; steps 4-6 never fence
// or kill a live coord.
//
// Cases currently wired in production callers:
//   1. Auto-handoff 50% (Yellow) — fleet-guard MILESTONE drain via
//      internal/handoffop.retireOldAgent (non-graceful branch).
//   2. Auto-handoff 70% (Red) — same drain path.
//   3. Manual `fleet handoff <id>` — cmd/fleet/handoff.go runHandoff
//      + resumeHandoff (crash-recovery), non-graceful branch.
//   5. Engine swap — implicit, same path as case 3 plus new Engine.
//
// The PRIMARY live-coord path is the GRACEFUL route (GracefulCoordSwap): a
// lease-wrapped standby completes through the barrier + winner-delivery. This
// helper is the fallback for a successor that is NOT lease-wrapped.
//
// Case 4 (dead-coord resume via TUI [a] — OldIsDead=true) is in the helper's
// contract but NOT wired in this PR's dispatch.go integration. External callers
// can still invoke the helper with OldIsDead=true; it's exercised by unit tests.
// When OldIsDead=true, steps 4-6 are SKIPPED (no live OLD to reserve against or
// retire).
//
// Worker swap is OUT OF SCOPE for this helper. Worker swap needs task-row
// CompareAndSwap under LockProjectState.
//
// ASCII flow:
//
//   step 1 ─────►  flock(swap.lock)                            (serialize same-project swaps)
//   step 2 ─────►  spawn.Spawn(newRecSpec) → NEW alive         (skipped when AlreadySpawnedNewRec)
//   step 3.a ───►  WaitForReadyToPrompt(NEW)
//   step 3.b ───►  SessionAlive(NEW)  ◄─ ambiguous-error: preserve; dead: drop + OLD keeps lease
//   step 4 ─────►  create the write-once handoff journal naming NEW +      (soft guard; skipped if
//                  barrierID (PR-2 D3) — a no-op if the GRACEFUL producer    oldIsDead; a create
//                  (graceful_unix's CreateHandoffJournal seam) already made  failure only logs)
//                  this exact entry durable BEFORE this helper ran
//   step 5.a ───►  SendKeys(OLD, "/exit"); sleep grace          (skipped if oldIsDead)
//   step 5.b ───►  tmux.Kill(OLD)                               (skipped if oldIsDead)
//   step 5.c ───►  SessionAlive(OLD)  ──── alive=true → FAILURE MODE 5 (surface, never re-kill)
//   step 6 ─────►  oldRec.Archive()                             (skipped if oldIsDead)
//   step 7 ─────►  release swap.lock; return
//   (COMMIT is the winning standby's first-tick journal+barrier delete once it acquires the freed flock)

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// DefaultSwapGraceWindow is the time between SendKeys("/exit") and Kill
// on the OLD session. 10s matches the operator-visible grace expectation
// documented in DESIGN-subagent-lifecycle.md.
const DefaultSwapGraceWindow = 10 * time.Second

// ErrSwapInProgress is reserved for non-blocking flock acquisition. The
// current implementation uses a blocking flock so callers don't see
// this; the constant is here so a future LOCK_NB path can return it
// without an API break.
var ErrSwapInProgress = errors.New("atomic coord swap: another swap holds the lock")

// ErrOrphanSurvived is returned when step 5.c reports OLD's tmux
// session is STILL alive after the kill (FAILURE MODE 5). The marker
// commit has already succeeded, so the invariant holds — NEW owns
// per the marker. OLD's tmux session is now an orphan process,
// archived from the records side, and reapable by `fleet maintenance
// prune-orphan-tmux`. The wrapping caller surfaces this to the operator
// via a red banner / inbox alert.
//
// The string includes "orphan tmux" so operator log greps key on it the
// same way they grep for "still alive" in the pre-AtomicCoordSwap world.
type ErrOrphanSurvived struct {
	OldSession string
	OldAgentID string
	NewAgentID string
	Project    string
	Inner      error
}

func (e *ErrOrphanSurvived) Error() string {
	// Codex review iter-7 [P1]: helper no longer archives OLD on
	// this path; operator action is `fleet rm <oldAgentID>` after
	// confirming OLD dead (not the prior "prune-orphan-tmux will
	// reap" wording, which was true when we archived).
	return fmt.Sprintf(
		"atomic coord swap: swap to successor %s in progress but OLD tmux session %s for project %q survived kill (OLD record preserved; operator: confirm OLD dead then `fleet rm %s`): %v",
		e.NewAgentID, e.OldSession, e.Project, e.OldAgentID, e.Inner)
}

// Is allows errors.Is(err, ErrOrphanSurvivedSentinel) without callers
// having to type-assert the struct. The sentinel is a separate package-
// level value so callers can match on the class.
func (e *ErrOrphanSurvived) Is(target error) bool {
	return target == ErrOrphanSurvivedSentinel
}

// ErrOrphanSurvivedSentinel is the sentinel for errors.Is matching
// against any *ErrOrphanSurvived value.
var ErrOrphanSurvivedSentinel = errors.New("atomic coord swap: orphan survived")

// ErrOldKillProbeAmbiguous is returned when step 5.c's tmux.SessionAlive
// probe of OLD comes back (alive=false, err!=nil) — i.e., the kill may
// have succeeded but the probe couldn't confirm. The marker has already
// committed to NEW (step 4). We DO NOT archive OLD's record on this
// path because OLD might still be alive on a different tmux socket
// holding coordinator.lock — archiving would hide the live coord from
// `fleet status` and the dashboard, while the new coord (which can't
// take coordinator.lock) exits.
//
// On this error: marker is at NEW; OLD's record stays on disk;
// operator triages via TUI. If OLD is genuinely dead, prune-orphan-tmux
// won't see it (no orphan because the record points at a non-existent
// session — sweeper keys on the inverse: orphan session, no live
// record). The expected operator action is `fleet rm <OLD>` once they
// confirm OLD is gone.
//
// Codex review iter-4 [P1]: the plan's "going forward is safer than
// backward" bias didn't account for the cross-socket case where
// archiving on ambiguous probe strands the project with no live coord.
type ErrOldKillProbeAmbiguous struct {
	OldSession string
	OldAgentID string
	NewAgentID string
	Project    string
	ProbeErr   error
}

func (e *ErrOldKillProbeAmbiguous) Error() string {
	// Use OldAgentID (not OldSession) in the suggested fleet rm
	// command — `fleet rm` takes an agent ID, not a tmux session
	// name. Codex review iter-5 [P3]: following the prior message
	// with the session name verbatim would fail and leave the stale
	// record in place.
	return fmt.Sprintf(
		"atomic coord swap: swap to successor %s in progress but post-kill probe of OLD session %s for project %q was ambiguous: %v (OLD record preserved; operator: confirm OLD dead, then `fleet rm %s`)",
		e.NewAgentID, e.OldSession, e.Project, e.ProbeErr, e.OldAgentID)
}

func (e *ErrOldKillProbeAmbiguous) Is(target error) bool {
	return target == ErrOldKillProbeAmbiguousSentinel
}

// ErrOldKillProbeAmbiguousSentinel is the sentinel for errors.Is
// matching against any *ErrOldKillProbeAmbiguous value.
var ErrOldKillProbeAmbiguousSentinel = errors.New("atomic coord swap: old-kill probe ambiguous")

// AtomicCoordSwapInputs collects the parameters for one swap. Mirrors
// the plan's INPUTS block.
type AtomicCoordSwapInputs struct {
	// Project — used to locate coord-spawn marker + swap.lock.
	Project string

	// OldRec — current coord record (may be alive or proven-dead).
	OldRec *agent.Record

	// NewRecSpec — spawn.Options for the replacement. The helper does
	// not mutate this; PreAllocatedID, OldRecord, NewDocPath, etc. are
	// the caller's responsibility.
	//
	// Ignored when AlreadySpawnedNewRec is non-nil.
	NewRecSpec spawn.Options

	// AlreadySpawnedNewRec, when non-nil, instructs the helper to
	// SKIP steps 2-3 (spawn + readiness probe) and proceed directly
	// to step 4 (marker commit) using this record. This is the entry
	// point for callers that already wrote a crash-recovery queue
	// journal and ran their own spawn before invoking the helper
	// (e.g. `fleet handoff` and the auto-drain path, which pre-allocate
	// IDs and journal them BEFORE spawning).
	//
	// When set, the caller is responsible for having already:
	//   - verified the NEW tmux session is alive (step 3.b equivalent),
	//   - written the NEW agent record to disk,
	//   - waited for the pane to stabilize if applicable.
	// The helper still runs step 1 (precondition + flock) before the
	// commit so the marker invariant is preserved.
	AlreadySpawnedNewRec *agent.Record

	// OldIsDead — caller asserts oldRec.session is gone (case 4 only).
	// When true, steps 5 + 6 are SKIPPED entirely — no kill, no
	// archive. Operator clears OLD via TUI [x]. This matches
	// cmd/fleet/dispatch.go's existing dead-coord-resume semantics
	// (see commentary at line 709 onward).
	OldIsDead bool

	// GraceWindow — duration between SendKeys("/exit") and Kill on
	// OLD. Default DefaultSwapGraceWindow (10s) if zero. Ignored when
	// OldIsDead=true.
	GraceWindow time.Duration

	// BarrierID names this handoff cycle's completion barrier + is stamped
	// into the write-once handoff journal at step 4 (PR-2 D3, Decision 7).
	// The graceful route (GracefulHandoff) allocates it up front and threads
	// it here so the barrier file it already wrote and the journal name the
	// SAME barrier (coordlock.HandoffBarrierPath). Empty => this helper mints
	// a fresh id (the bespoke non-graceful branch, which has no separate
	// barrier writer).
	BarrierID string
}

// AtomicCoordSwapResult carries what the caller needs after a successful
// swap: the new record (spawn-resolved), whether OLD was archived (false
// for dead-old case 4), and any non-fatal warnings emitted during step
// 5/6 (so the caller's UI can render them as banners).
type AtomicCoordSwapResult struct {
	NewRec       *agent.Record
	OldArchived  bool
	OrphanedKill bool // FAILURE MODE 5 hit but invariant held; ErrOrphanSurvived also returned
}

// AtomicCoordSwap executes the 7-step contract above. The marker rename
// is the single atomic commit point.
//
// Returns:
//
//   - on success: result populated, err == nil. Marker is at NEW, NEW is
//     alive. If OldIsDead=false, OLD is killed and archived. If
//     OldIsDead=true, OLD's record is preserved (operator clears via [x]).
//
//   - on rollback (steps 1–3 fail): err describes the failure. Marker
//     unchanged, OLD remains the coord. NEW (if spawned in step 2) is
//     killed + record removed before return.
//
//   - on FAILURE MODE 5 (step 5.c reports OLD still alive after kill):
//     returns *ErrOrphanSurvived with the OLD session embedded. Marker
//     is at NEW (committed). OLD's record IS archived (so prune-orphan-tmux
//     reaps the session on its next sweep). An inbox alert is dropped at
//     ~/.fleet/inbox/<NEW.ID>.md.
//
// Caller is responsible for higher-level concerns: handoff doc, queue
// journal, prompt delivery, retire-aware status output. The helper owns
// only the atomic owner-flip + retire bookkeeping.
//
// stderr is for the [P0] FAILURE MODE 5 log line. nil-tolerant.
func AtomicCoordSwap(in AtomicCoordSwapInputs, stderr io.Writer) (AtomicCoordSwapResult, error) {
	res := AtomicCoordSwapResult{}

	if in.Project == "" {
		return res, errors.New("atomic coord swap: Project is required")
	}
	if in.OldRec == nil {
		return res, errors.New("atomic coord swap: OldRec is required")
	}

	graceWindow := in.GraceWindow
	if graceWindow <= 0 {
		graceWindow = DefaultSwapGraceWindow
	}

	// Step 1.a — acquire swap.lock on this project.
	release, lerr := acquireSwapLock(in.Project)
	if lerr != nil {
		return res, fmt.Errorf("atomic coord swap: acquire swap.lock: %w", lerr)
	}
	defer release()

	// Steps 2-3 — spawn the replacement + readiness probe. When the
	// caller pre-spawned (AlreadySpawnedNewRec set), we skip the
	// spawn + readiness wait (caller already did those) but STILL
	// run step 3.b's NEW probe under the lock. This is the codex
	// iter-7 [P2] fix: callers' inline probe code (e.g.,
	// handoffop.retireOldAgent) only logs probe errors and proceeds,
	// so without this gate a transient probe-failure could mask a
	// NEW that died between the caller's wait and our handoff reserve.
	var newRec *agent.Record
	if in.AlreadySpawnedNewRec != nil {
		newRec = in.AlreadySpawnedNewRec
		// Step 3.b re-probe NEW alive — same contract as the
		// internal-spawn path. Probe error preserves NEW for
		// operator inspection; definitive dead surfaces clearly. On
		// either failure OLD still owns the lease (no reservation yet),
		// so discovery keeps resolving to OLD.
		switch alive, perr := sessionAliveFn(newRec.TmuxSession); {
		case perr != nil:
			return res, fmt.Errorf(
				"atomic coord swap: probe pre-spawned replacement %s session %s failed: %w (caller's spawn handling preserves the record for operator inspection; OLD %s still owns the lease)",
				newRec.ID, newRec.TmuxSession, perr, in.OldRec.ID)
		case !alive:
			// Pre-spawned NEW is dead. Clean it up via the helper's
			// shared DropReplacementRecord so the caller's record +
			// session both go away cleanly. OLD keeps the lease.
			if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
				return res, fmt.Errorf(
					"atomic coord swap: pre-spawned replacement %s tmux session %s already exited AND cleanup failed: %w (OLD %s still owns the lease)",
					newRec.ID, newRec.TmuxSession, dropErr, in.OldRec.ID)
			}
			return res, fmt.Errorf(
				"atomic coord swap: pre-spawned replacement %s tmux session %s already exited before the swap reserved a handoff; OLD %s still owns the lease",
				newRec.ID, newRec.TmuxSession, in.OldRec.ID)
		}
	} else {
		// Step 2 — spawn. spawn.Spawn rolls back its own state.json
		// write failure (kills the orphan tmux session before
		// returning). On error here OLD still owns the lease and no
		// observable change has happened.
		spawned, serr := spawnFn(in.NewRecSpec)
		if serr != nil {
			return res, fmt.Errorf("atomic coord swap: spawn replacement: %w", serr)
		}
		newRec = spawned

		// Step 3.a — wait for NEW's pane to stabilize.
		// spawn.WaitForReadyToPrompt is best-effort: convergence
		// failure (didn't stabilize in MaxWait) returns an error but
		// the pane may still be usable. We don't fail the swap on
		// convergence failure alone — Step 3.b's tristate
		// SessionAlive probe is the authoritative liveness gate.
		if werr := waitForReadyToPromptFn(newRec.TmuxSession); werr != nil {
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: readiness poll for %s did not converge: %v (probe-checking liveness)\n",
					newRec.TmuxSession, werr)
			}
		}

		// Step 3.b — re-probe NEW after the readiness wait. Catches
		// the "NEW dies during boot" window. The tristate is critical:
		//
		//   - alive=true,  err=nil → continue.
		//   - alive=false, err=nil → definitively dead → roll back NEW.
		//   - alive=false, err!=nil → probe ambiguous → preserve NEW
		//     record + tmux for operator inspection (no rollback).
		switch alive, perr := sessionAliveFn(newRec.TmuxSession); {
		case perr != nil:
			return res, fmt.Errorf(
				"atomic coord swap: probe replacement %s session %s failed: %w (NEW record preserved for operator inspection; OLD %s still owns the lease)",
				newRec.ID, newRec.TmuxSession, perr, in.OldRec.ID)
		case !alive:
			if dropErr := DropReplacementRecord(newRec.TmuxSession, newRec.ID, stderr); dropErr != nil {
				return res, fmt.Errorf(
					"atomic coord swap: replacement %s tmux session %s exited during readiness wait AND cleanup failed: %w (OLD %s still owns the lease; investigate)",
					newRec.ID, newRec.TmuxSession, dropErr, in.OldRec.ID)
			}
			return res, fmt.Errorf(
				"atomic coord swap: replacement %s tmux session %s exited during readiness wait; OLD %s still owns the lease",
				newRec.ID, newRec.TmuxSession, in.OldRec.ID)
		}
	}
	res.NewRec = newRec

	// Step 4 — CREATE the write-once handoff journal on OLD's still-held flock
	// (PR-2 D3, Decision 7; replaces the deleted epoch ReserveHandoff). While
	// OLD still holds the flock, O_EXCL-create coordinator.handoff.json naming
	// NEW as the successor (+ NEW's process handle for the mid-boot-vs-abandoned
	// liveness decision + the barrier id) so a contender that observes the freed
	// flock after we retire OLD WAITS for the booting NEW (coordreconcile Resolve
	// rule) instead of spawning a duplicate.
	//
	// SOFT guarantee: flock-exclusivity — never two coordinator.flock holders —
	// is the REAL no-duplicate-coordinator guard; the journal only makes the
	// common handoff window spawn-free without a timer. So a create failure
	// never rolls back NEW or aborts the swap — it only logs. Skipped on
	// OldIsDead (no live OLD; the dead-coord recovery has no successor to name).
	//
	// codex PR2-review [P1]: on the GRACEFUL route this journal is already
	// durable by the time we get here — GracefulHandoff's CreateHandoffJournal
	// seam creates it BEFORE the completion barrier is written (the barrier
	// must never appear on disk before the journal entry that lets drain's
	// BarrierExists (journal-first) find it). Re-running createHandoffJournalResolving
	// against our OWN just-created entry would misreport it as a live
	// in-flight collision (Decision 7's O_EXCL-collision arm has no
	// "same successor, already mine" case) and log a spurious warning. Skip
	// the redundant create when an existing journal already names this exact
	// successor+barrier; the bespoke non-graceful callers never pre-create, so
	// this is a normal create for them, unchanged.
	if !in.OldIsDead {
		barrierID := in.BarrierID
		if barrierID == "" {
			barrierID = coordlock.NewBarrierID()
		}
		want := buildHandoffJournal(in.Project, in.OldRec, newRec, barrierID)
		alreadyDurable := false
		if existing, ok, rerr := coordlock.ReadHandoffJournal(in.Project); rerr == nil && ok &&
			existing.SuccessorID == want.SuccessorID && existing.BarrierID == want.BarrierID {
			alreadyDurable = true
		}
		if !alreadyDurable {
			if cerr := createHandoffJournalResolving(want); cerr != nil && stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: create handoff journal for project %s successor %s failed: %v (proceeding; flock-exclusivity + winner-delivery still hold)\n",
					in.Project, newRec.ID, cerr)
			}
		}
	}

	// Step 5 — retire OLD. Asymmetric on OldIsDead.
	//
	// OldIsDead=true (case 4 — dead-coord resume): SKIP kill entirely.
	// Cross-socket probe ambiguity means we can't be 100% sure OLD is
	// gone, and operator's already-told-us-it's-dead has to be enough.
	// SKIPPING kill avoids racing a possibly-alive OLD on another socket
	// and yanking a healthy coord. The lease (winner's async epoch bump) is
	// the authoritative identity either way.
	//
	// OldIsDead=false: normal /exit + grace + Kill + post-probe.
	//
	// On FAILURE MODE 5 the helper returns *ErrOrphanSurvived directly
	// from inside the switch, so we don't need an "orphaned" guard to
	// suppress step 6 — the early-return handles it.
	if !in.OldIsDead {
		// 5.a — graceful exit hint. ErrNoSession means OLD already
		// died between our handoff reserve and now; not fatal.
		if serr := tmuxSendKeysFn(in.OldRec.TmuxSession, "/exit", "Enter"); serr != nil &&
			!errors.Is(serr, tmux.ErrNoSession) {
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: send-keys /exit to %s: %v\n",
					in.OldRec.TmuxSession, serr)
			}
		}
		if graceWindow > 0 {
			sleepFn(graceWindow)
		}
		// 5.b — Kill is idempotent (tmux.Kill returns nil for already-
		// dead sessions). On error we drop into 5.c to disambiguate.
		killErr := tmuxKillFn(in.OldRec.TmuxSession)

		// 5.c — confirm dead.
		alive, perr := sessionAliveFn(in.OldRec.TmuxSession)
		switch {
		case perr != nil:
			// Probe ambiguous. The handoff to NEW is reserved (step 4)
			// but NOT yet committed (NEW acquires the freed lease async).
			// We CANNOT proceed to archive OLD because the cross-socket
			// case (OLD alive on a different tmux socket holding
			// coordinator.lock) would hide a live coord from `fleet
			// status` while NEW loses the lock race — project would have
			// no visible coord (codex review iter-4 [P1]).
			//
			// Instead: preserve OLD's record, drop an inbox alert,
			// log [P1] to stderr, return ErrOldKillProbeAmbiguous.
			// Operator action: confirm OLD dead (e.g., via
			// `fleet maintenance prune-orphan-tmux` or inspecting
			// other sockets), then `fleet rm <OLD>`.
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"[P1] atomic coord swap: post-kill probe for OLD %s ambiguous: %v (handoff to %s reserved; OLD record preserved for operator triage)\n",
					in.OldRec.TmuxSession, perr, newRec.ID)
			}
			if alertErr := dropOrphanIncidentAlert(newRec.ID, in.OldRec.ID, in.OldRec.TmuxSession, in.Project); alertErr != nil {
				if stderr != nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: atomic coord swap: drop ambiguous-probe incident alert: %v\n",
						alertErr)
				}
			}
			return res, &ErrOldKillProbeAmbiguous{
				OldSession: in.OldRec.TmuxSession,
				OldAgentID: in.OldRec.ID,
				NewAgentID: newRec.ID,
				Project:    in.Project,
				ProbeErr:   perr,
			}
		case alive:
			// FAILURE MODE 5. The handoff to NEW is reserved (step 4).
			// OLD's tmux session survived our kill — provably still
			// running. We PRESERVE OLD's record (do NOT archive) so:
			//
			//   - `fleet status` still shows OLD; operator sees the
			//     live coord they need to triage.
			//   - The cross-socket case (OLD truly holds
			//     coordinator.lock and NEW will lose the race +
			//     exit) is recoverable: OLD record + tmux + the handoff
			//     reservation give the operator enough state to reason
			//     about what happened.
			//
			// Previously we archived OLD here so `fleet maintenance
			// prune-orphan-tmux` could reap the tmux session
			// automatically. Codex review iter-7 [P1]: that tradeoff
			// was wrong — auto-reaping lost the operator-visible
			// "live OLD record" signal, and on the cross-socket case
			// the project ended up with the reservation naming NEW
			// (which may exit losing the lock race) and OLD archived —
			// no visible live coord.
			//
			// Operator action: confirm OLD truly dead (kill manually
			// via tmux if needed), then `fleet rm <OLD_ID>`.
			if alertErr := dropOrphanIncidentAlert(newRec.ID, in.OldRec.ID, in.OldRec.TmuxSession, in.Project); alertErr != nil {
				if stderr != nil {
					_, _ = fmt.Fprintf(stderr,
						"warning: atomic coord swap: drop orphan incident alert: %v\n",
						alertErr)
				}
			}
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"[P0] atomic coord swap: FAILURE MODE 5: handoff to %s reserved for project %s but OLD tmux session %s survived kill (kill_err=%v); OLD record PRESERVED for operator triage — `fleet rm %s` after confirming OLD dead\n",
					newRec.ID, in.Project, in.OldRec.TmuxSession, killErr, in.OldRec.ID)
			}
			res.OrphanedKill = true
			return res, &ErrOrphanSurvived{
				OldSession: in.OldRec.TmuxSession,
				OldAgentID: in.OldRec.ID,
				NewAgentID: newRec.ID,
				Project:    in.Project,
				Inner:      killErr,
			}
		default:
			// alive=false, err=nil on the CURRENT tmux socket.
			//
			// Codex iter-16 [P1]: this does NOT prove OLD is globally
			// dead — `tmux.SessionAlive` only sees the active socket
			// (FLEET_TMUX_SOCKET). If OLD was running on a different
			// tmux socket, kill-session on our socket would fail (or
			// silently succeed because there's nothing to kill HERE),
			// and post-probe just confirms "nothing here." Archiving
			// OLD in step 6 would then strand a still-live coord on
			// the other socket (the same cross-socket hazard FAILURE
			// MODE 5 / the perr != nil branch already guard against).
			//
			// Distinguisher:
			//   - killErr == nil: tmux.Kill found the session AND
			//     killed it (or it was already dead before the kill,
			//     in which case the swap was already a no-op for OLD
			//     and archiving is safe — there is no other socket to
			//     consider; the session lived here and is gone now).
			//     Continue to step 6.
			//   - killErr != nil: tmux.Kill could NOT confirm a kill
			//     ran. Combined with post-probe "not on our socket"
			//     this is ambiguous — OLD may be alive on a different
			//     socket. Return ErrOldKillProbeAmbiguous, preserve
			//     OLD record, surface to operator.
			//
			// Codex iter-17 [P1] (deferred — known scope limitation):
			// The killErr==nil branch still has a residual cross-socket
			// blind spot. `tmux.Kill` returns nil when its pre-probe
			// SessionAlive (current socket) says "not alive" — including
			// the case where OLD has ALWAYS been on a different socket.
			// In that case Kill is a no-op on the current socket, returns
			// nil, post-probe also says "not alive" here, and step 6
			// archives OLD while it is still globally live on the other
			// socket. Distinguishing that case requires recording
			// OLD's originating FLEET_TMUX_SOCKET on agent.Record at
			// spawn time and checking it here — a schema change that
			// cross-cuts spawn + state + every probe site (out of scope
			// for this PR; matches the PR #149 cross-socket cap
			// precedent of documenting the limitation rather than
			// re-architecting). Multi-socket deployment is rare in
			// practice; on a single-socket fleet (the default), this
			// branch is correct. Tracked for a follow-up PR.
			if killErr != nil {
				if stderr != nil {
					_, _ = fmt.Fprintf(stderr,
						"[P1] atomic coord swap: kill %s failed (%v) but post-probe says session not on current socket — cannot confirm OLD dead globally (cross-socket coord possible); preserving OLD record for operator triage\n",
						in.OldRec.TmuxSession, killErr)
				}
				if alertErr := dropOrphanIncidentAlert(newRec.ID, in.OldRec.ID, in.OldRec.TmuxSession, in.Project); alertErr != nil {
					if stderr != nil {
						_, _ = fmt.Fprintf(stderr,
							"warning: atomic coord swap: drop cross-socket-ambiguity incident alert: %v\n",
							alertErr)
					}
				}
				return res, &ErrOldKillProbeAmbiguous{
					OldSession: in.OldRec.TmuxSession,
					OldAgentID: in.OldRec.ID,
					NewAgentID: newRec.ID,
					Project:    in.Project,
					ProbeErr:   killErr,
				}
			}
		}
	}

	// Step 6 — archive OLD record. SKIPPED on OldIsDead=true (operator
	// clears via TUI [x]; cross-socket ambiguity means we can't be sure
	// archiving is safe). FAILURE MODE 5 already returned earlier and
	// archived OLD there.
	if !in.OldIsDead {
		// v2 schema: stamp successor_id + cause=handoff on the archive so
		// the chain resolver (cmd/fleet/attach.go) lands operators on the
		// live tail. Newly-spawned successor record already carries
		// predecessor_id from internal/spawn.
		if arcErr := in.OldRec.ArchiveWithHandoff(newRec.ID); arcErr != nil {
			// Kill is confirmed dead in step 5. Falling back to
			// os.Remove on the live record keeps `fleet status` from
			// showing a stale entry. If even Remove fails, return
			// error — the live record is stuck and a retry would
			// load it as the new "coord OLD."
			path, perr := state.AgentPath(in.OldRec.ID)
			if perr != nil {
				return res, fmt.Errorf(
					"atomic coord swap: archive OLD %s failed (%w) AND could not resolve live path (%w); handoff reserved to NEW=%s",
					in.OldRec.ID, arcErr, perr, newRec.ID)
			}
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return res, fmt.Errorf(
					"atomic coord swap: archive OLD %s failed (%w) AND remove failed (%w); handoff reserved to NEW=%s but OLD record stuck — clean up agents/%s.json manually",
					in.OldRec.ID, arcErr, rmErr, newRec.ID, in.OldRec.ID)
			}
			if stderr != nil {
				_, _ = fmt.Fprintf(stderr,
					"warning: atomic coord swap: archive %s: %v (live record removed instead, archive copy lost)\n",
					in.OldRec.ID, arcErr)
			}
		}
		// res.OldArchived reflects "OLD record is no longer at the
		// live agents/ path", true whether Archive succeeded or the
		// os.Remove fallback ran. Callers use this to decide whether
		// `fleet status` will still show OLD.
		res.OldArchived = true
	}

	// Step 7 — defer'd release() will run. Return.
	return res, nil
}

// acquireSwapLock takes a project-scoped flock at
// ~/.fleet/projects/<project>/.locks/swap.lock and returns a release
// function. The .locks/ dir is lazily MkdirAll'd to support the
// fresh-project case (matches LockProjectState's pattern).
//
// Held for the lifetime of one swap. Other swap attempts for the same
// project block until release. Different projects proceed in parallel.
//
// Process-scoped flock; goroutines in the same process do NOT serialize
// via this. Matches the v0.1 flock semantics in state.LockProject.
func acquireSwapLock(project string) (func(), error) {
	path, err := swapLockPath(project)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir swap.lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open swap.lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock swap.lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// swapLockPath returns ~/.fleet/projects/<project>/.locks/swap.lock.
//
// Distinct from LockProject's lock path (~/.fleet/projects/.locks/<name>.lock)
// and from LockProjectState's path (~/.fleet/projects/<name>/.locks/state.lock).
// AtomicCoordSwap intentionally uses its own lock so concurrent
// `fleet handoff <coord>` and `fleet tasks set` operations on the same
// project don't serialize against each other — only same-project SWAP
// attempts serialize.
func swapLockPath(project string) (string, error) {
	dir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(dir), ".locks", "swap.lock"), nil
}

// dropOrphanIncidentAlert writes an operator-facing incident JSON file
// at ~/.fleet/incidents/coord-swap-<utcStamp>-<oldSession>.json. The
// TUI dashboard scans this directory and renders a per-project banner;
// the file is structured (mirroring the macOS jetsam observer's shape)
// so future tooling can grep / aggregate.
//
// Codex review iter-6 [P2]: previously this dropped a markdown alert
// at ~/.fleet/inbox/<newID>.md, but that path is the agent's command
// channel — fleet-guard's inbox.py relays the file content into the
// agent's NEXT TURN. Sending a swap-failure alert there means the
// replacement agent reads "you left an orphan tmux session" as input,
// which corrupts the agent's task focus and can overwrite legitimate
// operator messages queued in the same inbox file. ~/.fleet/incidents/
// is the right channel: not parsed by any agent, scanned by the TUI
// dashboard for operator-visible badges.
//
// Best-effort — the [P0]/[P1] stderr logs are the load-bearing alert
// channel; this is supplementary for the TUI side.
func dropOrphanIncidentAlert(newAgentID, oldAgentID, oldSession, project string) error {
	dir, err := state.IncidentsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir incidents dir: %w", err)
	}
	now := time.Now().UTC()
	// Path: incidents/coord-swap-<utc-iso>-<old-session>.json. Stable,
	// sortable, unique enough that two ambiguous-probe events on the
	// same project don't collide.
	filename := fmt.Sprintf("coord-swap-%s-%s.json",
		now.Format("20060102T150405Z"), oldSession)
	path := filepath.Join(dir, filename)
	// Body is JSON-ish so future TUI badge code can parse fields
	// rather than scraping markdown. Manual JSON to avoid pulling
	// encoding/json into a hot path the helper barely uses.
	//
	// Codex review iter-9 [P2]: the "investigate via prune-orphan-tmux"
	// hint was wrong post-iter-7 (the helper now preserves OLD's
	// record, so prune-orphan-tmux's "session exists, no live record"
	// rule doesn't match). Updated to point operators at `fleet rm`
	// after they manually confirm OLD dead.
	body := fmt.Sprintf(
		`{"kind":"coord-swap-orphan","timestamp_utc":%q,"project":%q,"new_agent_id":%q,"old_agent_id":%q,"old_session":%q,"summary":"coord-swap left OLD tmux session alive (or probe ambiguous); confirm OLD dead manually (e.g. inspect other tmux sockets / tmux kill-session) then `+"`fleet rm %s`"+`"}`,
		now.Format(time.RFC3339), project, newAgentID, oldAgentID, oldSession, oldAgentID)
	return state.WriteAtomic(path, []byte(body+"\n"))
}

// Package-level seams for unit-testable dependency injection. Production
// values point at the real subsystems; tests override per-case to
// simulate failure modes without spinning up real tmux / spawn.
//
// These are vars (not interface arguments) so the caller's signature
// stays single-method. The trade-off: tests must save+restore the
// pre-test value in t.Cleanup. The handoffop package's existing
// tmuxKillFn / tmuxSessionAliveFn pattern (in replacement_cleanup.go)
// uses the same seam approach.
var (
	spawnFn                = spawn.Spawn
	waitForReadyToPromptFn = spawn.WaitForReadyToPrompt
	sessionAliveFn         = tmux.SessionAlive
	tmuxSendKeysFn         = tmux.SendKeys
	sleepFn                = time.Sleep
	// tmuxKillFn is declared in replacement_cleanup.go; we reuse it.
)

// buildHandoffJournal builds the write-once handoff journal body naming NEW as
// the successor (PR-2 D3, Decision 7). The successor's coord-run SUPERVISOR is
// the flock holder, so its stamped supervisor identity (pid + pid_start) is the
// kernel liveness signal that decides mid-boot (alive) vs abandoned (dead) —
// never a TTL. The respawn spec (Command/Cwd, primitive) lets an Abandon
// recovery re-spawn from the journal alone. coordlock stamps the successor's
// boot id at Create (same boot as the just-spawned successor).
func buildHandoffJournal(project string, oldRec, newRec *agent.Record, barrierID string) coordlock.HandoffJournal {
	j := coordlock.HandoffJournal{
		Project:          project,
		SuccessorID:      newRec.ID,
		BarrierID:        barrierID,
		SuccessorSession: newRec.TmuxSession,
		RespawnCommand:   newRec.Command,
		RespawnCwd:       newRec.Cwd,
	}
	if oldRec != nil {
		j.OldAgentID = oldRec.ID
	}
	if newRec.SupervisorPID > 0 {
		j.SuccessorPID = newRec.SupervisorPID
		j.SuccessorPidStart = newRec.SupervisorPidStart
	}
	return j
}

// createHandoffJournalResolving creates the journal O_EXCL, resolving a
// leftover from a prior cycle FIRST (Decision 7 Create — never a blind
// overwrite). On ErrHandoffJournalExists it reads the leftover and:
//
//   - committed-stale (the leftover names the CURRENT live flock holder — a
//     prior handoff that completed but never cleared its journal) -> delete + retry;
//   - dead successor (process gone) -> delete per Abandon + retry;
//   - live-not-holding (a real in-flight handoff to a DIFFERENT live successor)
//     -> DO NOT clobber; return an error (the caller warns; flock-exclusivity
//     still bounds any duplicate to one transient stand-down).
func createHandoffJournalResolving(j coordlock.HandoffJournal) error {
	err := coordlock.CreateHandoffJournal(j)
	if !errors.Is(err, coordlock.ErrHandoffJournalExists) {
		return err
	}
	existing, ok, rerr := coordlock.ReadHandoffJournal(j.Project)
	if rerr != nil {
		return fmt.Errorf("resolve leftover handoff journal: read: %w", rerr)
	}
	if !ok {
		// Vanished between Create and Read — retry once.
		return coordlock.CreateHandoffJournal(j)
	}
	committedStale := false
	if owner, present := coordlock.LiveOwner(j.Project); present && owner.AgentID != "" &&
		owner.AgentID == existing.SuccessorID {
		committedStale = true
	}
	if committedStale || !coordlock.HandoffSuccessorAlive(existing) {
		if derr := coordlock.DeleteHandoffJournal(j.Project); derr != nil {
			return fmt.Errorf("resolve leftover handoff journal: delete: %w", derr)
		}
		return coordlock.CreateHandoffJournal(j)
	}
	return fmt.Errorf(
		"resolve leftover handoff journal: names a live in-flight successor %s (not overwriting)",
		existing.SuccessorID)
}
