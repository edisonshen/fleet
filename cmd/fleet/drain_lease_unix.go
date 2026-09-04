//go:build linux || darwin

// drain_lease_unix.go — the BOUNDED, lease-aware drain path
// (DESIGN-handoff-drain-storm-leak §3(D), PR3). It replaces the legacy
// "hold the per-agent flock across the entire Resume" drain (the
// forever-held lock + 81-process leak) with a drain that:
//
//   - NEVER holds a lock across Resume / tmux / spawn / kill (the root-cause
//     line drain.go:101-106 is gone on this path);
//   - STANDS DOWN (exit 0) when a healthy leader holds the flock and no
//     handoff is pending (nothing to do);
//   - keys the whole routing on the FLOCK + the write-once handoff journal
//     (PR-2 D9b′ — the deleted epoch is gone): `barrierUp` reads the journal's
//     barrier_id, `leaderBusy` is a live flock holder (busy⇒owner). On a
//     graceful barrier it completes ONLY on a POSITIVE successor identity
//     (flock body agent_id == the queue's pre-allocated successor, D9b), then
//     reaps OLD's already-exited supervisor through the ONE authenticated
//     coord.KillCoordIfIdentityMatches primitive (a benign no-op on a corpse);
//   - recovers WITHOUT a kill (PR-2 D8): there is NO autonomous fence→kill
//     takeover. When OLD is PROVABLY DEAD the flock is already free, so a fresh
//     lease-wrapped successor is cold-spawned into it; a hung-but-ALIVE OLD is
//     never signaled — drain surfaces `run 'fleet rm <id>'` and preserves the
//     queue for retry.
//
//	┌──────────────────────────────────────────────────────────────────┐
//	│ drainOneLeaseAwareWith(req)                                        │
//	│   barrier present (journal)    -> graceful: positive-id successor  │
//	│                                   completes; else no-kill recover  │
//	│   flock busy (live holder)     -> stale-queue stand-down / legacy  │
//	│                                   Resume fallback (no barrier yet)  │
//	│   flock free (OLD released/died) -> wait for successor; else       │
//	│                                     no-kill dead-recover / surface │
//	└──────────────────────────────────────────────────────────────────┘
//
// Build-tagged linux||darwin (the flock lease primitive is). Other Unix
// targets compile drain_lease_other.go's stub (lease unsupported).

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coord"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordrepo"
	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/handoffop"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/spawn"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// leaseDrainEnabled reports whether this platform supports the lease-aware
// drain path.
func leaseDrainEnabled() bool {
	return coordlock.LeaseSupported()
}

// drainLeaseDeps are the injectable seams that keep drainOneLeaseAware
// deterministic under test (no real lease reads, no real kill, no real
// Resume, no real clock). Production callers use defaultDrainLeaseDeps().
type drainLeaseDeps struct {
	// LeaderPresent reports whether a live process HOLDS the flock for project
	// (busy⇒owner, D9a). Production: coordlock.LeaderPresent.
	LeaderPresent func(project string) bool
	// LiveOwner reports the live flock holder's identity (agent_id + pid),
	// busy⇒owner. Used for the D9b POSITIVE successor-identity gate: a handoff
	// is complete only when the flock body's agent_id equals the queue's
	// pre-allocated successor id (req.NewAgentID) — never a bare busy flock.
	// Production: coordlock.LiveOwner.
	LiveOwner func(project string) (coordlock.Owner, bool)
	// ActiveOwnerPID returns the supervisor pid of the current live flock holder
	// (ok=false if free). Used only to tell "OLD is still the flock holder,
	// finishing its exit" from "OLD released". Production:
	// coordlock.CurrentActiveOwnerPID.
	ActiveOwnerPID func(project string) (int, bool)
	// BarrierExists reports whether this handoff cycle's completion barrier is
	// on disk, resolved via the write-once handoff journal's barrier_id (PR-2
	// D3/D9b′ — no longer keyed on the deleted epoch). Production: a journal read
	// + stat through coordlock.HandoffBarrierPath.
	BarrierExists func(project string) bool
	// LoadAgent loads an agent record (for OLD's supervisor identity).
	// Production: agent.Load.
	LoadAgent func(id string) (*agent.Record, error)
	// KillCoord reaps OLD's coord-run supervisor through the authenticated
	// gate. Production: coord.KillCoordIfIdentityMatches. Used only to reap an
	// already-exited OLD's lingering supervisor (a benign no-op on a corpse) —
	// PR-2 (D8) removed the autonomous fence→kill takeover; a hung-but-alive
	// flock holder is surfaced for `fleet rm`, never signaled.
	KillCoord func(coord.KillTarget) error
	// SupervisorAlive reports whether pid is STILL the same live process
	// recorded at spawn — pid + pid_start identity, the pidAlive standard
	// (a bare-pid probe false-alives on reuse). It backs the KP7
	// dead-targets-only escalation gate: takeoverAndRecover proceeds only
	// when OLD's supervisor is provably dead. Contract: pid gone -> false
	// (provably dead); start-time mismatch -> false (reused pid, the
	// recorded process is dead); recorded pidStart==0 -> true (identity
	// unprovable, NEVER escalate on it). Production wraps
	// coordlock.PidStartNanos.
	SupervisorAlive func(pid int, pidStart int64) bool
	// Resume runs the cold-spawn fallback for a live-leader handoff that has no
	// barrier yet (the graceful producer hasn't run). Production: handoffop.Resume.
	// It is run under a SHORT per-agent LockAgent (Resume's contract requires
	// it) — but only when OLD is still the LIVE leader (no hung swap), so the
	// forever-hold class does not apply. The graceful/hung COORD path never
	// calls Resume; it kills/escalates lock-free.
	Resume func(req queue.SpawnFresh, queuePath string, graceMillis int, stdout, stderr io.Writer) error
	// LockAgent serializes the cold Resume against a concurrent drain so two
	// drains cannot both spawn a replacement (codex PR3 iter-4 [P1]).
	// Production: state.LockAgent. Returns a release func.
	LockAgent func(agentID string) (func(), error)
	// RecoverSpawn brings up a FRESH lease-wrapped successor AFTER a safety-net
	// takeover has fenced+killed a hung OLD (codex PR3 iter-7 [P1]). It must
	// spawn from the CACHED old record (captured BEFORE the takeover archived
	// it), using the unified coord-run --standby path so the successor
	// acquires + heartbeats the now-free lease. preAllocatedID is the queue's pre-allocated successor id
	// (req.NewAgentID) so journal/doc/remote-control setup keyed to it still
	// correlates to the live replacement (codex PR3 iter-10 [P2]); empty means
	// generate a fresh id. nil OldRec means "could not cache OLD" -> the seam
	// surfaces a recovery instruction instead of spawning blind. disableAutoResume
	// is the EFFECTIVE per-handoff policy (the queue's DisableAutoResume override
	// resolved against OLD's baseline — codex PR3 iter-14 [P2]); it gates both the
	// spawn record's policy and whether recoverHandoffTail types the resume prompt.
	RecoverSpawn func(oldRec *agent.Record, docPath, preAllocatedID string, disableAutoResume bool, stdout, stderr io.Writer) error
	// DeliverPending delivers the still-pending handoff doc to the CONFIRMED
	// healthy successor BEFORE drainGraceful cleans the queue (codex
	// PR3-completion iter-2 [P1]). The barrier proves doc durability, not
	// DELIVERY — a GracefulCoordSwap completion that failed/crashed between
	// retiring OLD and injecting the doc leaves the queue as the only durable
	// "successor never got its prompt" signal; deleting it without delivering
	// would strand the winner blank (§5c). Production:
	// drainGracefulDeliverPending (at-most-once via the lock-owner sentinel;
	// errors preserve the queue). oldRec may be nil (already archived).
	DeliverPending func(req queue.SpawnFresh, oldRec *agent.Record, stdout, stderr io.Writer) error
	// BarrierPoll is the interval between barrier-existence checks while
	// waiting (bounded) for a graceful handoff to complete. 0 = default.
	BarrierPoll time.Duration
	// Self is the caller's pid (never target self in a kill). Production:
	// os.Getpid.
	Self func() int
}

// ErrEscalatedToTakeOver is declared in drain.go (the all-platform file) so
// runDrain can reference it on every GOOS. The lease-aware path here returns
// it when a slow/hung handoff is handed to the safety-net takeover.

const defaultBarrierPoll = 200 * time.Millisecond

func defaultDrainLeaseDeps() drainLeaseDeps {
	return drainLeaseDeps{
		LeaderPresent:  coordlock.LeaderPresent,
		LiveOwner:      coordlock.LiveOwner,
		ActiveOwnerPID: coordlock.CurrentActiveOwnerPID,
		BarrierExists: func(project string) bool {
			// PR-2 (D3/D9b′): the barrier is named by the write-once handoff
			// journal's barrier_id (the epoch is gone). Read the journal, resolve
			// the barrier through the SAME helper the producer used, then stat it.
			j, ok, err := coordlock.ReadHandoffJournal(project)
			if err != nil || !ok || j.BarrierID == "" {
				return false
			}
			p, err := coordlock.HandoffBarrierPath(project, j.BarrierID)
			if err != nil {
				return false
			}
			_, statErr := os.Stat(p)
			return statErr == nil
		},
		LoadAgent:       agent.Load,
		KillCoord:       coord.KillCoordIfIdentityMatches,
		SupervisorAlive: supervisorAliveByStart,
		Resume:          handoffop.Resume,
		LockAgent:       state.LockAgent,
		RecoverSpawn:    productionRecoverSpawn,
		DeliverPending:  drainGracefulDeliverPending,
		BarrierPoll:     defaultBarrierPoll,
		Self:            os.Getpid,
	}
}

// productionRecoverSpawn brings up a FRESH lease-wrapped successor after a
// takeover (codex PR3 iter-7 [P1]). It spawns from the CACHED old record via
// the unified coord-run --standby path so the successor acquires the
// now-free lease. Coord cwd resolves through the shared
// project-repo resolver (same as the dispatch recovery path), never the old
// coord's stale stored Cwd. Surface-don't-silo on any gap: a missing record /
// unresolvable repo prints a concrete recovery command rather than spawning
// blind.
func productionRecoverSpawn(oldRec *agent.Record, docPath, preAllocatedID string, disableAutoResume bool, stdout, stderr io.Writer) error {
	if oldRec == nil {
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: takeover reaped the hung coord but its record could not be cached; "+
				"run `fleet dispatch --coord-spawn` (or press [a] in the TUI) to bring up a replacement\n")
		return fmt.Errorf("fleet drain: recover-spawn: no cached old record")
	}
	if len(oldRec.Command) == 0 {
		return fmt.Errorf("fleet drain: recover-spawn: old coord %s has no stored command", oldRec.ID)
	}

	// ADOPT an already-spawned successor instead of re-spawning (codex PR4
	// [P1]/[P2]). A prior recovery pass may have spawned the replacement with
	// THIS preAllocatedID and then failed only at prompt delivery, preserving
	// the queue for retry. Re-running spawn with the same id would collide
	// with the live session. So: if the pre-allocated successor record exists
	// AND its tmux session is alive, skip the spawn and go straight to (idempotent)
	// prompt delivery. This makes a preserved-queue retry safe.
	if preAllocatedID != "" {
		if existing, lerr := agent.Load(preAllocatedID); lerr == nil && existing != nil {
			if alive, _ := tmux.SessionAlive(existing.TmuxSession); alive {
				_, _ = fmt.Fprintf(stdout,
					"fleet drain: adopting already-spawned successor %s for %s (no re-spawn)\n",
					existing.ID, oldRec.Project)
				return recoverHandoffTail(oldRec, existing, docPath, disableAutoResume, stdout, stderr)
			}
		}
	}

	spawnCwd := oldRec.Cwd
	if spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project) {
		resolved, rerr := coordrepo.ResolveProjectRepo(oldRec.Project, true)
		if rerr != nil {
			return fmt.Errorf("fleet drain: recover-spawn: resolve repo for project %q: %w", oldRec.Project, rerr)
		}
		spawnCwd = resolved
	}
	rec, err := spawn.Spawn(spawn.Options{
		OldRecord:  oldRec,
		NewDocPath: docPath,
		Cwd:        spawnCwd,
		Command:    oldRec.Command,
		// EFFECTIVE per-handoff policy (queue override resolved against OLD's
		// baseline by the caller), NOT oldRec's baseline — honors
		// `fleet handoff --no-auto-resume` / `--auto-resume` on the escalated
		// recovery path the same way handoffop.Resume does (codex PR3 iter-14 [P2]).
		DisableAutoResume: disableAutoResume,
		// Reuse the queue's pre-allocated successor id so any handoff
		// journal/doc/remote-control setup keyed to it correlates to the live
		// replacement (codex PR3 iter-10 [P2]). Empty -> spawn.Spawn allocates.
		PreAllocatedID: preAllocatedID,
		LeaderCheck:    coordLeaderCheck,
		ActiveOwnerPID: leaseActiveOwnerPID,
		StandbyTimeout: spawn.DefaultStandbyTimeout,
	})
	if err != nil {
		return fmt.Errorf("fleet drain: recover-spawn replacement for %s: %w", oldRec.ID, err)
	}
	_, _ = fmt.Fprintf(stdout,
		"fleet drain: cold-spawned lease-wrapped successor %s for %s after takeover\n", rec.ID, oldRec.Project)
	return recoverHandoffTail(oldRec, rec, docPath, disableAutoResume, stdout, stderr)
}

// Seams for recoverHandoffTail / deliverRecoverResumePrompt, kept injectable so
// the recover-tail tests stay deterministic (no real tmux send / marker FS).
var (
	recoverWaitForReadyFn       = spawn.WaitForReadyToPrompt
	recoverSessionAliveFn       = tmux.SessionAlive
	recoverSendPromptVerifiedFn = spawn.SendPromptKeysVerified
	recoverCurrentOwnerFn       = coordlock.CurrentOwner
)

// effectiveDisableAutoResume resolves the EFFECTIVE per-handoff auto-resume
// policy the same way handoffop.Resume does (codex PR3 iter-14 [P2]): the
// queue's DisableAutoResume *bool override wins when set; otherwise fall back to
// OLD's baseline DisableAutoResume. A nil cachedOld (record could not be cached)
// falls back to the override or false. Honors `fleet handoff --no-auto-resume` /
// `--auto-resume` on the escalated takeover-recovery path.
// NOTE: this resolves the PERSISTED baseline only — the v1-schema send
// suppression is a TRANSIENT per-handoff gate applied at the delivery sites
// (drainGracefulDeliverPending), NOT here (codex PR3-completion iter-10
// [P2]): this value flows into RecoverSpawn -> spawn.Options.DisableAutoResume
// and would otherwise permanently flip a recovered successor's baseline to
// opt-out just because the queue file predates the auto-resume schema.
func effectiveDisableAutoResume(req queue.SpawnFresh, cachedOld *agent.Record) bool {
	if req.DisableAutoResume != nil {
		return *req.DisableAutoResume
	}
	if cachedOld != nil {
		return cachedOld.DisableAutoResume
	}
	return false
}

// recoverHandoffTail runs the same post-spawn handoff TAIL the normal recovery
// path runs, plus PR4's verified prompt-delivery semantics. Without it the
// takeover-recovered successor is INERT: a freshly-spawned coord does nothing
// until it is told to resume from the handoff doc, so we deliver
// handoff.ResumePrompt(docPath) to the lease OWNER (the winning standby) once
// its pane is ready, with at-most-once retry handling. Discovery no longer needs
// a marker repair — the successor becomes the coord by acquiring the lease
// (the epoch bump IS the identity), and attach/TUI resolve it via the lease.
//
// The prompt delivery returns errors so the queue is preserved when the
// replacement would otherwise be left idle.
func recoverHandoffTail(oldRec *agent.Record, rec *agent.Record, docPath string, disableAutoResume bool,
	stdout, stderr io.Writer) error {
	if spawn.IsCoordSpawn(oldRec.TaskID, oldRec.Project) {
		return deliverRecoverResumePrompt(rec, docPath, disableAutoResume, true, stdout, stderr)
	}
	return deliverRecoverResumePrompt(rec, docPath, disableAutoResume, false, stdout, stderr)
}

// deliverRecoverResumePrompt delivers the resume prompt to a recovered
// successor so it READS the handoff doc and continues the in-flight work
// (codex PR4 [P1]). The safety-net takeover path bypasses handoffop.Resume/
// retireOldAgent, so without this the new coord would start blank.
//
// At-most-once + no-strand semantics (codex PR4 [P2]):
//
//   - AT-MOST-ONCE: a per-successor sentinel file
//     (.locks/resume-prompt-sent-<newID>) is written BEFORE the send. On an
//     adopt-retry (queue.Delete failed after a prior successful send) the
//     sentinel short-circuits, so the same doc prompt is never re-submitted.
//     This is the at-most-once marker the other handoff paths get from
//     queue.Delete ordering; the recovery path can't reorder around
//     queue.Delete (it runs in the caller), so it carries its own marker.
//   - NO-STRAND on readiness timeout: WaitForReadyToPrompt failing does NOT
//     hard-fail. If the tmux session is still alive, we WARN and send anyway
//     (keys buffer in the pty until Claude is ready) — matching
//     retireOldAgent. Only a DEAD session returns an error (queue preserved).
//
// Skips silently when the doc is empty or auto-resume is disabled (a
// non-claude wrapper must not receive "Read your handoff doc..." as garbage).
func deliverRecoverResumePrompt(rec *agent.Record, docPath string, disableAutoResume bool, lockOwnerDelivery bool,
	stdout, stderr io.Writer) error {
	if docPath == "" && !lockOwnerDelivery {
		return nil
	}
	if lockOwnerDelivery {
		prompt := ""
		if !disableAutoResume && docPath != "" {
			prompt = handoff.ResumePrompt(docPath)
		}
		// At-most-once for the lock-owner branch too (codex iter-15 P2): if a
		// prior pass already delivered and only takeoverAndRecover's queue.Delete
		// failed, the adopt-retry re-enters here. Without the sentinel guard the
		// owner gets the same prompt again. Key on rec.ID (the queue entry), same
		// as the direct path below.
		ownerSentinel, sErr := resumePromptSentinelPath(rec.Project, rec.ID)
		if prompt != "" && sErr == nil && ownerSentinel != "" {
			if _, statErr := os.Stat(ownerSentinel); statErr == nil {
				return nil // already delivered on a prior pass
			}
		}
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = recoverCurrentOwnerFn
		deps.WaitReady = recoverWaitForReadyFn
		deps.SessionAlive = recoverSessionAliveFn
		deps.SendVerified = recoverSendPromptVerifiedFn
		_, err := handoffdelivery.DeliverToCurrentOwner(handoffdelivery.Options{
			Project: rec.Project,
			Prompt:  prompt,
			Stdout:  stdout,
			Stderr:  stderr,
		}, deps)
		if err != nil {
			return err
		}
		if prompt == "" {
			return nil
		}
		// Mark AFTER the verified send (DeliverToCurrentOwner only returns nil
		// once SendVerified confirmed submission) so a crash here re-delivers a
		// benign duplicate rather than stranding.
		if sErr == nil && ownerSentinel != "" {
			_ = os.WriteFile(ownerSentinel, []byte(docPath+"\n"), 0o644)
		}
		_, _ = fmt.Fprintf(stdout, "fleet drain: delivered resume prompt to recovered lock owner for %s\n", rec.Project)
		return nil
	}
	if disableAutoResume {
		return nil
	}
	sentinel, serr := resumePromptSentinelPath(rec.Project, rec.ID)
	if serr == nil {
		if _, statErr := os.Stat(sentinel); statErr == nil {
			// Already delivered on a prior pass; do not re-send (at-most-once).
			return nil
		}
	}
	if werr := recoverWaitForReadyFn(rec.TmuxSession); werr != nil {
		// Readiness poll didn't converge. If the session is DEAD, fail so the
		// queue is preserved. If it is ALIVE (slow startup / onboarding
		// screen / custom wrapper), warn and send anyway — the keys buffer.
		alive, probeErr := recoverSessionAliveFn(rec.TmuxSession)
		if !alive && probeErr == nil {
			return fmt.Errorf(
				"fleet drain: recovered successor %s session %s is dead; cannot deliver resume prompt "+
					"(queue preserved for retry; doc: %s)", rec.ID, rec.TmuxSession, docPath)
		}
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: recovered successor %s not visibly ready (%v) but session alive; sending resume prompt anyway\n",
			rec.ID, werr)
	}
	// Use the VERIFIED send (codex PR4 [P2]): SendPromptKeys returns nil even
	// when the prompt is left sitting UNSUBMITTED in the input box (its
	// contract is "attach anyway"). For an unattended recovery there is no
	// operator to press Enter, so an unsubmitted prompt == not delivered.
	submitted, serr := recoverSendPromptVerifiedFn(rec.TmuxSession, handoff.ResumePrompt(docPath))
	if serr != nil {
		return fmt.Errorf(
			"fleet drain: resume prompt to recovered successor %s failed: %w "+
				"(queue preserved for retry; doc: %s)", rec.ID, serr, docPath)
	}
	if !submitted {
		// Reached tmux but the prompt didn't submit. Do NOT mark the sentinel
		// and DO return an error so the queue is preserved — a later drain
		// adopts the live successor and re-attempts delivery rather than
		// leaving it idle.
		return fmt.Errorf(
			"fleet drain: resume prompt to recovered successor %s reached tmux but was not submitted "+
				"(queue preserved for retry; doc: %s)", rec.ID, docPath)
	}
	// Mark AFTER a VERIFIED-SUBMITTED send (codex PR4 [P2]). The sentinel
	// means "definitely delivered". A crash between this mark and
	// queue.Delete would re-send on retry — a benign, idempotent duplicate
	// ("read your doc and continue") — which is preferable to a silent strand.
	if sentinel != "" {
		_ = os.WriteFile(sentinel, []byte(docPath+"\n"), 0o644)
	}
	_, _ = fmt.Fprintf(stdout, "fleet drain: delivered resume prompt to recovered successor %s\n", rec.ID)
	return nil
}

// resumePromptSentinelPath returns the at-most-once marker path for a
// recovered successor's resume prompt, under the project's .locks/ dir.
func resumePromptSentinelPath(project, newID string) (string, error) {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(pdir, ".locks", "resume-prompt-sent-"+newID), nil
}

// drainOneLeaseAware is the production entry point for the bounded drain.
func drainOneLeaseAware(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer) error {

	return drainOneLeaseAwareWith(req, path, graceMillis, resumeTimeoutMillis,
		stdout, stderr, defaultDrainLeaseDeps())
}

// drainOneLeaseAwareWith is the seam-injected core (see drainLeaseDeps).
func drainOneLeaseAwareWith(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	d = fillDrainLeaseDeps(d)
	project := req.Project
	if project == "" {
		// Defensive: drainOne only routes COORD handoffs here (they always
		// carry a project), but if a projectless queue reaches this seam we
		// cannot stand-down/verify (the lease machinery is per-project). Fall
		// back to a cold Resume under a SHORT per-agent lock (coldResume holds
		// LockAgent — Resume's serialization contract).
		return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
	}

	timeout := time.Duration(resumeTimeoutMillis) * time.Millisecond
	deadline := time.Now().Add(timeout)

	// PR-2 (D9b′): key the whole routing on the FLOCK + the write-once handoff
	// journal, not the deleted epoch. barrierUp reads the journal's barrier_id;
	// leaderBusy is a live flock holder (busy⇒owner). A free flock ⇒ OLD
	// released/died.
	barrierUp := d.BarrierExists(project)
	leaderBusy := d.LeaderPresent(project)

	// BARE-COORD GUARD (codex PR4 [P1]) — runs BEFORE the barrier/leader
	// branch decision. The OLD coord being drained may be a BARE
	// (non-lease-wrapped, SupervisorPID==0) successor from an earlier legacy
	// handoff; the project's stale epoch (and any leftover
	// handoff-complete-<epoch>.json barrier) belongs to an OLDER, already-
	// released coord generation, NOT to OLD. Routing such a handoff through
	// the lease branches is wrong twice over:
	//   - barrierUp -> drainGraceful -> drainReapOld REFUSES (OLD has no
	//     supervisor identity to authenticate a kill) -> queue stranded forever;
	//   - default  -> safety-net takeover acquires the orphan flock + spawns a
	//     successor WITHOUT killing the live bare OLD -> TWO coordinators.
	// A bare coord (!LeaseWrapped, SupervisorPID==0) holds NO lease and is
	// NEVER the recorded lease owner (the owner is always a coord-run with a
	// SupervisorPID). So a bare OLD's handoff must ALWAYS complete via the
	// legacy coldResume (loads OLD by id, retires/kills it via Resume) —
	// regardless of whether some OTHER coord-run currently holds the project
	// lease (codex PR4 [P1]/[P2]).
	//
	// Do NOT route LeaseWrapped && SupervisorPID==0 here. That is an
	// unstamped coord-run supervisor whose identity could not be recorded; it
	// may still hold the lease. It must fall through to the lease-aware path,
	// where drainReapOld/takeoverAndRecover fail closed because STONITH cannot
	// authenticate the target.
	// Routing it through the lease branches is wrong in every case:
	//   - leaderHealthy (a different coord-run leads) -> drainLiveLeaderFallback
	//     polls a barrier OLD will never write, then legacy-Resumes anyway
	//     (best case) or spawns a duplicate;
	//   - barrierUp on a stale epoch -> drainGraceful -> drainReapOld REFUSES
	//     (no supervisor identity) -> queue stranded;
	//   - default -> safety-net takeover acquires the orphan flock + RecoverSpawn
	//     without killing the live bare OLD -> TWO coordinators.
	if oldRec, lerr := d.LoadAgent(req.OldAgentID); lerr == nil &&
		oldRec != nil && oldRec.SupervisorPID == 0 && !oldRec.LeaseWrapped {
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: %s is a bare (non-lease-wrapped) coord; completing via legacy resume "+
				"(holds no lease — no takeover/graceful-reap/standby-wait)\n", req.OldAgentID)
		return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
	}

	switch {
	case barrierUp:
		// Graceful path: the OLD coord wrote the completion barrier (named by
		// the journal's barrier_id) — doc + checkpoint are durable. Finish by
		// waiting for the EXPECTED successor (positive identity == req.NewAgentID)
		// to take the freed flock, then reap OLD's already-exited supervisor
		// (benign) + clean. If OLD is a hung-alive flock holder that never
		// yields, drainGraceful escalates to the no-kill recovery (surface
		// `fleet rm`).
		return drainGraceful(req, path, deadline, stdout, stderr, d)

	case leaderBusy:
		// A live process HOLDS the flock and no completion barrier exists yet.
		//   - OLD already archived -> the handoff already completed; the queue
		//     file is a stale leftover. STAND DOWN (clean the queue, spawn
		//     nothing) — the true "nothing to drain" case (T13).
		//   - OLD still live -> a handoff was requested (the queue exists). A
		//     GracefulHandoff producer may be about to write the barrier, so we
		//     POLL (bounded) for it FIRST — racing straight to legacy Resume
		//     would spawn a SECOND successor while the standby is already
		//     polling. Only if no barrier appears do we fall back to the proven
		//     legacy Resume (the requested /exit+kill retire) to complete it.
		return drainLiveLeaderFallback(req, path, deadline, graceMillis, resumeTimeoutMillis, stdout, stderr, d)

	default:
		// The flock is FREE — no live holder. OLD released or died (a graceful
		// handoff writes the barrier BEFORE releasing, so a free flock with no
		// barrier is a non-graceful release/crash, or a cold start where no
		// coord ever held the flock). Recover WITHOUT a kill (D8): wait bounded
		// for the expected successor to acquire the free flock; else dead-recover
		// (cold-spawn a fresh successor / surface `fleet rm` if OLD is somehow
		// still alive but not holding). Never fences a live coord.
		return drainFreeFlockRecover(req, path, deadline, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
	}
}

// drainGraceful finishes a graceful handoff once the completion barrier is up.
// PR-2 (D9b): the handoff is COMPLETE only when the EXPECTED successor — the
// flock body's agent_id == the queue's pre-allocated successor id
// (req.NewAgentID) — holds the flock. A bare/booting/foreign busy flock is NOT
// a healthy successor (the R4/B hazard). Until the expected successor acquires,
// wait bounded (OLD may still be finishing its exit); on timeout dead-recover
// via takeoverAndRecover (no kill — D8: it cold-spawns a dead OLD's successor,
// or surfaces `fleet rm` for a hung-alive OLD). Holds NO lock.
func drainGraceful(req queue.SpawnFresh, path string, deadline time.Time,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	oldRec, err := d.LoadAgent(req.OldAgentID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// OLD already archived — but that is NOT proof the expected successor
		// acquired: coord-run RELEASES the flock and THEN archives OLD, so there
		// is a window where the flock is free and unowned. Deleting the queue
		// here would strand the project coordless. Wait (bounded) for the
		// EXPECTED successor (positive identity) before declaring done. We cannot
		// RecoverSpawn here (OLD's record is archived — nothing to recover FROM),
		// so on timeout PRESERVE the queue + surface. Never a silent coordless
		// delete.
		if d.waitForHealthySuccessor(req.Project, req.NewAgentID, deadline) {
			if derr := d.DeliverPending(req, nil, stdout, stderr); derr != nil {
				return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
					req.OldAgentID, derr, path)
			}
			_, _ = fmt.Fprintf(stdout,
				"fleet drain: %s retired and the expected successor holds the flock; cleaning queue\n", req.OldAgentID)
			return queue.Delete(path)
		}
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: %s archived (barrier present) but the expected successor has NOT acquired the flock within the budget; "+
				"preserving queue %s for retry (a standby may still be mid-acquire; run `fleet doctor` if it persists)\n",
			req.OldAgentID, path)
		return fmt.Errorf("fleet drain: %s retired but the expected successor does not lead %s yet; queue preserved for retry",
			req.OldAgentID, req.Project)
	case err != nil:
		return fmt.Errorf("fleet drain: load old agent %s: %w", req.OldAgentID, err)
	}

	// Positive-identity completion (D9b): the handoff is done ONLY when the
	// flock body names the EXPECTED successor (req.NewAgentID). A bare/booting/
	// foreign busy flock never satisfies this, so it never triggers the
	// queue-delete (the R4/B fix).
	if _, ok := d.healthySuccessorPresent(req.Project, req.NewAgentID); ok {
		if derr := d.DeliverPending(req, oldRec, stdout, stderr); derr != nil {
			return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
				oldRec.ID, derr, path)
		}
		// The expected successor leads (so OLD already released + exited). Reap
		// OLD's lingering supervisor (a benign no-op on the already-dead OLD) and
		// clean the queue.
		return drainReapOld(oldRec, req, path, stdout, stderr, d)
	}

	// The expected successor has NOT taken over yet. If OLD still holds the flock
	// it is finishing its exit (surface that for diagnostics). Wait bounded for
	// the expected successor either way.
	if ownerPid, ownerOK := d.ActiveOwnerPID(req.Project); ownerOK && ownerPid == oldRec.SupervisorPID {
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: %s wrote the barrier and still holds the flock; waiting for its self-release and the successor's acquire\n",
			oldRec.ID)
	} else {
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: %s released the flock but the expected successor does not hold it yet; waiting for a confirmed successor\n",
			oldRec.ID)
	}
	if d.waitForHealthySuccessor(req.Project, req.NewAgentID, deadline) {
		if derr := d.DeliverPending(req, oldRec, stdout, stderr); derr != nil {
			return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
				oldRec.ID, derr, path)
		}
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: the expected successor acquired the flock for %s; reaping the old supervisor and finishing\n",
			req.Project)
		return drainReapOld(oldRec, req, path, stdout, stderr, d)
	}
	// Timeout: the expected successor never acquired. Dead-recover WITHOUT a kill
	// (D8): takeoverAndRecover cold-spawns if OLD is provably dead, or surfaces
	// `fleet rm` if OLD is a hung-alive flock holder. Never fences a live coord.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: %s did not yield to the expected successor within the budget; attempting no-kill recovery\n",
		oldRec.ID)
	return takeoverAndRecover(req, path, oldRec, stdout, stderr, d)
}

// drainGracefulDeliverPending delivers the still-pending handoff doc to the
// CONFIRMED healthy successor BEFORE drainGraceful cleans the queue (codex
// PR3-completion iter-2 [P1]). The barrier proves doc + checkpoint
// DURABILITY, not DELIVERY: the GracefulCoordSwap completion can fail or
// crash between retiring OLD and injecting the doc, leaving the queue as the
// only durable signal that the successor never got its resume prompt.
// Deleting the queue here without delivering would consume that signal and
// strand the winner blank (§5c: "doc stays pending; next drain re-delivers").
//
// At-most-once via deliverRecoverResumePrompt's lock-owner sentinel
// (.locks/resume-prompt-sent-<id>): a pass that delivered but crashed before
// queue.Delete re-enters here and short-circuits. A doc the barrier
// completion ALREADY injected (delivery ok, only queue.Delete lost) has no
// sentinel and is re-sent once — the benign §5c duplicate, preferred over a
// silent strand. Errors propagate so the caller preserves the queue.
//
// oldRec may be nil (OLD already archived); the auto-resume policy then
// falls back to the queue override alone. The sentinel keys on the queue's
// journaled successor id (stable across passes), else OLD's id.
func drainGracefulDeliverPending(req queue.SpawnFresh, oldRec *agent.Record,
	stdout, stderr io.Writer) error {
	id := req.NewAgentID
	if id == "" {
		id = req.OldAgentID
	}
	rec := &agent.Record{ID: id, Project: req.Project}
	policyRec := oldRec
	if policyRec == nil {
		// OLD already archived: recover the inherited auto-resume baseline
		// from the ARCHIVE (codex PR3-completion iter-4 [P2]). A handoff
		// that opted out (DisableAutoResume, no queue override) must not get
		// a resume prompt typed into a successor that was meant to stay
		// idle. Best-effort: a missing archive falls back to the queue
		// override / default, same as a nil cachedOld elsewhere.
		if arc, aerr := agent.LoadArchive(req.OldAgentID); aerr == nil {
			policyRec = arc
		}
	}
	// v1 queues predate auto-resume; handoffop.Resume / resumeHandoff treat
	// them as autoResume=false (`req.SchemaVersion >= 2` gates the send).
	// This drain delivery must agree (codex PR3-completion iter-6 [P2]) or an
	// upgraded drain types a resume prompt while cleaning a lingering v1
	// queue. TRANSIENT send gate only — never persisted (iter-10 [P2]).
	disable := effectiveDisableAutoResume(req, policyRec) || req.SchemaVersion < 2
	return deliverRecoverResumePrompt(rec, req.HandoffDoc,
		disable, true, stdout, stderr)
}

// healthySuccessorPresent reports whether the EXPECTED successor holds the flock
// for project (PR-2 D9b — the R4/B fix). "Healthy" is a POSITIVE IDENTITY match:
// the flock body's agent_id must equal expectedSuccessorID (the queue's
// pre-allocated successor id, req.NewAgentID). A bare/booting/foreign busy flock
// — held by a coord that has NOT committed the expected handoff — is NOT healthy,
// so it never triggers the queue-delete + delivery. An EMPTY expectedSuccessorID
// (a legacy/v1 queue that never carried a successor id) maps to not-healthy
// (never a match of "" == body.agent_id, which a torn/identity-less body could
// satisfy). Returns (ownerPid, true) only on a positive identity match.
func (d drainLeaseDeps) healthySuccessorPresent(project, expectedSuccessorID string) (int, bool) {
	if expectedSuccessorID == "" {
		return 0, false
	}
	owner, ok := d.LiveOwner(project)
	if !ok || owner.AgentID == "" || owner.AgentID != expectedSuccessorID {
		return 0, false
	}
	return owner.PID, true
}

// waitForHealthySuccessor polls (bounded by deadline) for the EXPECTED successor
// (positive identity == expectedSuccessorID) to hold the flock for project.
// Returns true the instant it is observed; false if the deadline passes with
// none. Holds no lock.
func (d drainLeaseDeps) waitForHealthySuccessor(project, expectedSuccessorID string, deadline time.Time) bool {
	for {
		if _, ok := d.healthySuccessorPresent(project, expectedSuccessorID); ok {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		wait := d.BarrierPoll
		if rem := time.Until(deadline); rem < wait {
			wait = rem
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}
}

// drainReapOld reaps OLD's coord-run supervisor through the ONE authenticated
// kill primitive (NO unguarded kill) after the EXPECTED successor already holds
// the flock — so OLD has released it and exited, making this a benign no-op on
// a corpse in the common case. T42: a recycled PID is refused by the primitive
// (start-time mismatch); clause-4 refuses to shoot the current flock owner (the
// successor). Then cleans the queue.
func drainReapOld(oldRec *agent.Record, req queue.SpawnFresh, path string,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	if oldRec.SupervisorPID <= 0 {
		// No supervisor identity recorded — cannot authenticate a kill.
		// Surface-don't-silo: refuse rather than an unguarded kill.
		return fmt.Errorf(
			"fleet drain: old coord %s has no recorded supervisor identity; cannot reap safely (queue %s preserved)",
			req.OldAgentID, path)
	}
	if oldRec.SupervisorPID == d.Self() {
		return fmt.Errorf("fleet drain: refusing to reap self (pid %d)", oldRec.SupervisorPID)
	}

	if err := d.KillCoord(coord.KillTarget{
		Pid:      oldRec.SupervisorPID,
		PidStart: oldRec.SupervisorPidStart,
		AgentID:  oldRec.ID,
		Project:  oldRec.Project,
	}); err != nil {
		// A refusal (T42 PID-reuse, exe-path, would-shoot-the-successor) is
		// surfaced — never silently dropped. The queue file is preserved so a
		// later pass can retry once the ambiguity clears.
		return fmt.Errorf("fleet drain: graceful reap of old coord %s refused/failed: %w (queue %s preserved)",
			oldRec.ID, err, path)
	}
	_, _ = fmt.Fprintf(stdout, "fleet drain: reaped old coord %s (graceful, successor leads)\n", oldRec.ID)
	// OLD is dead -> kernel released the flock (the successor already holds it).
	// Clean the queue file.
	if err := queue.Delete(path); err != nil {
		return fmt.Errorf("fleet drain: reaped %s but queue delete failed: %w", oldRec.ID, err)
	}
	return nil
}

// drainFreeFlockRecover handles a coord handoff when the flock is FREE — OLD
// released or died (PR-2 D8, replacing the deleted barrier-wait-then-fence-kill
// escalation). A graceful handoff writes the barrier BEFORE releasing the flock,
// so a free flock with no barrier is a non-graceful release/crash (or a cold
// start where no coord ever held the flock). It polls (bounded) for the barrier
// (in case a graceful producer is mid-write) and for the EXPECTED successor
// acquiring the freed flock; on the deadline it dead-recovers via
// takeoverAndRecover — which cold-spawns a successor when OLD is provably dead
// (the flock is already free, so NO kill is needed) or surfaces `fleet rm` when
// OLD is somehow still alive. NEVER fences a live coord.
func drainFreeFlockRecover(req queue.SpawnFresh, path string, deadline time.Time,
	graceMillis, resumeTimeoutMillis int, stdout, stderr io.Writer, d drainLeaseDeps) error {

	project := req.Project
	// Cache OLD's record up front: a later load could race OLD's own exit-time
	// archive. The cached record is what RecoverSpawn spawns from. A load failure
	// is non-fatal: RecoverSpawn surfaces a recovery instruction when it is nil.
	var cachedOld *agent.Record
	if rec, lerr := d.LoadAgent(req.OldAgentID); lerr == nil {
		cachedOld = rec
	}
	for {
		if d.BarrierExists(project) {
			// A graceful barrier appeared while we waited — finish the graceful
			// path (positive-identity successor completion).
			return drainGraceful(req, path, deadline, stdout, stderr, d)
		}
		// The expected successor may have already acquired the freed flock.
		if _, ok := d.healthySuccessorPresent(project, req.NewAgentID); ok {
			if derr := d.DeliverPending(req, cachedOld, stdout, stderr); derr != nil {
				return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
					req.OldAgentID, derr, path)
			}
			_, _ = fmt.Fprintf(stdout,
				"fleet drain: the expected successor acquired the flock for %s; handoff complete, cleaning queue\n", project)
			return queue.Delete(path)
		}
		if !time.Now().Before(deadline) {
			break
		}
		wait := d.BarrierPoll
		if rem := time.Until(deadline); rem < wait {
			wait = rem
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}

	// Deadline passed with no barrier and no successor — OLD released/died and no
	// standby acquired. Dead-recover WITHOUT a kill (D8): takeoverAndRecover
	// cold-spawns a successor from the cached record when OLD is provably dead
	// (the flock is already free), or surfaces `fleet rm` if OLD is somehow still
	// alive but not holding the flock.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: flock free for %s with no barrier and no successor; attempting no-kill recovery\n",
		project)
	return takeoverAndRecover(req, path, cachedOld, stdout, stderr, d)
}

// takeoverAndRecover is the NO-KILL safety-net recovery (PR-2 D8). It replaces
// the deleted fence→kill takeover: it NEVER signals a live process. It proceeds
// only past the KP7 dead-targets-only gate (OLD PROVABLY DEAD by pid+pid_start),
// then — because a dead coord-run was kernel-freed of the flock on death — brings
// up a FRESH lease-wrapped successor from the CACHED old record straight into the
// already-free flock, deleting the queue on success. A LIVE / unauthenticatable
// OLD aborts (surface `fleet rm`, preserve the queue).
//
// LOCK DISCIPLINE (codex PR3 iter-11/14 [P1]): the SHORT per-agent lock wraps
// ONLY the RecoverSpawn critical section, never a kill (there is none). It
// serializes duplicate recovery: two concurrent drains that both reach here
// (the flock is free, so both see a stealable lease) contend on the bounded
// LockAgent; the post-lock queue + flock-owner re-check makes only ONE
// RecoverSpawn, and the loser sees the queue gone / the successor leading and
// stands down. The lock is BOUNDED (state.acquireBoundedAt — never a bare
// LOCK_EX), so a contender times out instead of hanging. Shared by the
// graceful-past-budget and free-flock recovery escalations. Returns
// ErrEscalatedToTakeOver on success (a non-fatal processed outcome).
func takeoverAndRecover(req queue.SpawnFresh, path string, cachedOld *agent.Record,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	project := req.Project

	// Pre-takeover stand-down check (lock-free): if a peer drain already
	// recovered (queue gone), there is nothing to take over. Cheap early exit;
	// the authoritative duplicate guard is the post-takeover locked re-check.
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stdout, "fleet drain: %s already recovered by a concurrent drain; nothing to do\n", project)
		return nil
	}

	// KP7 DEAD-TARGETS-ONLY PRECONDITION (DESIGN-coord-no-auto-kill). This
	// escalation was reached via a staleness/timeout heuristic (barrier
	// timeout, no-successor budget) — reachable autonomously from the
	// fleet-guard 40/50 auto-handoff — and a staleness heuristic may never
	// fence or kill a LIVE coordinator. Proceed only when OLD's supervisor
	// is PROVABLY dead by pid + pid_start identity:
	//   - record unloadable (cachedOld nil)     -> not provably dead -> abort;
	//   - no recorded supervisor identity        -> not provably dead -> abort;
	//   - SupervisorAlive(pid, pid_start) true   -> LIVE               -> abort.
	// Abort = loud diagnostic + fleetlog event + drain failure through the
	// existing "did not confirm the old coord is gone" path: the queue is
	// preserved for retry, everyone stays alive, and the operator recourse
	// is `fleet handoff <id>` / `fleet rm <id>`.
	abortEscalation := func(why string) error {
		msg := fmt.Sprintf(
			"fleet drain: NOT escalating takeover for %s: %s; refusing to fence/kill (queue %s preserved for retry; if the old coord is truly wedged run `fleet handoff %s` or `fleet rm %s`)",
			project, why, path, req.OldAgentID, req.OldAgentID)
		_, _ = fmt.Fprintln(stderr, msg)
		fleetlog.Log(fleetlog.CompCLI, "coord.quarantine", "warn", fleetlog.Fields{
			Proj:  project,
			Agent: req.OldAgentID,
			Msg:   msg,
			Data:  map[string]any{"reason": "live-old-coord"},
		})
		return fmt.Errorf("fleet drain: takeover for %s did not confirm the old coord is gone (%s)",
			project, why)
	}
	switch {
	case cachedOld == nil:
		return abortEscalation("old coord record could not be loaded (not provably dead)")
	case cachedOld.SupervisorPID <= 0:
		return abortEscalation("old coord has no recorded supervisor identity (not provably dead)")
	case d.SupervisorAlive(cachedOld.SupervisorPID, cachedOld.SupervisorPidStart):
		return abortEscalation(fmt.Sprintf(
			"old coord supervisor pid=%d is still alive (pid+pid_start identity)",
			cachedOld.SupervisorPID))
	}

	// PR-2 (D8): NO fence→kill takeover any more. The KP7 gate above already
	// proved OLD is PROVABLY DEAD (it aborts + surfaces `fleet rm` for a live
	// OLD), and a dead coord-run's supervisor was kernel-freed of the flock on
	// death (and its own `defer Cleanup` archived its record + reaped its
	// session). So the flock is already FREE and stealable — we recover a fresh
	// successor directly, never signaling anything. Take the SHORT per-agent
	// lock to serialize RecoverSpawn against a concurrent drain (only ONE
	// recovers; the loser sees the queue gone and stands down).
	release, lerr := d.LockAgent(req.OldAgentID)
	if lerr != nil {
		return fmt.Errorf("fleet drain: lock agent %s for takeover recovery: %w", req.OldAgentID, lerr)
	}
	defer release()

	// Re-check the queue UNDER the lock: two concurrent drains can both observe
	// the flock free and both pass the KP7 dead-check above (OLD's death frees
	// the flock for everyone at once, not just the first observer) — this is
	// exactly why LockAgent + the post-lock re-check exist. A peer drain that
	// won the lock race may have already RecoverSpawned and deleted the queue.
	// If it is gone, stand down — recovering again would duplicate the
	// successor (codex PR3 iter-11 [P1]).
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		_, _ = fmt.Fprintf(stdout, "fleet drain: %s already recovered by a concurrent drain; nothing to do\n", project)
		return nil
	}

	// Re-check the flock OWNER under the lock before spawning: a graceful WARM
	// STANDBY may have been polling all along and acquired the freed flock. If
	// the EXPECTED successor (positive identity == req.NewAgentID) already leads,
	// RecoverSpawn would duplicate the coord or collide on the pre-allocated
	// session/id. A confirmed expected successor means the handoff is complete —
	// clean the queue and stand down instead of cold-spawning a redundant one.
	if op, ok := d.healthySuccessorPresent(project, req.NewAgentID); ok {
		// The warm standby that acquired during the takeover gap never got
		// the doc through THIS pass — deliver it before the queue is cleaned
		// (codex PR3-completion iter-3 [P1]; sentinel dedupes re-sends).
		if derr := d.DeliverPending(req, cachedOld, stdout, stderr); derr != nil {
			return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
				req.OldAgentID, derr, path)
		}
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: the expected successor (pid %d) acquired the flock for %s; standing down (no recovery spawn)\n",
			op, project)
		if derr := queue.Delete(path); derr != nil {
			return fmt.Errorf(
				"fleet drain: successor leads %s after recovery but queue delete failed (%w); rerun fleet drain to clean it",
				project, derr)
		}
		return ErrEscalatedToTakeOver
	}

	// OLD is provably dead (KP7 gate) so the flock is free + stealable. Bring up
	// a FRESH lease-wrapped successor from the CACHED old record via the unified
	// standby path so it acquires the now-free flock (codex PR3 iter-7 [P1]). We
	// do NOT route through handoffop.Resume: that loads OLD by id (racing the
	// archive).
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: old coord for %s is provably dead; recovering a lease-wrapped successor into the free flock\n", project)
	if err := d.RecoverSpawn(cachedOld, req.HandoffDoc, req.NewAgentID, effectiveDisableAutoResume(req, cachedOld), stdout, stderr); err != nil {
		// Surface the recovery-spawn failure. Leave the queue in place so the
		// dead-coord recovery path (next dispatch / TUI [a]) retries against the
		// now-stealable flock.
		return fmt.Errorf("fleet drain: recovery for %s failed: %w", project, err)
	}
	// Successor recovered. The queue file's handoff request is fulfilled by the
	// recovery spawn; delete it so a later drain doesn't re-escalate. A delete
	// FAILURE is surfaced as an error (codex PR3 iter-11 [P2]).
	if derr := queue.Delete(path); derr != nil {
		return fmt.Errorf(
			"fleet drain: recovered %s but queue delete failed (%w); rerun fleet drain to clean it",
			project, derr)
	}
	return ErrEscalatedToTakeOver
}

// drainLiveLeaderFallback handles a coord handoff queue file while a healthy
// leader still holds the lease and no completion barrier exists. If OLD is
// already archived the handoff completed and the queue is a stale leftover
// (stand down, clean it). Otherwise the graceful producer has not written a
// barrier yet, so complete the handoff via the LEGACY Resume path under a
// short per-agent lock (codex PR3 iter-5 [P1]) rather than stranding the
// queue. coldResume provides the locked Resume.
func drainLiveLeaderFallback(req queue.SpawnFresh, path string, deadline time.Time,
	graceMillis, resumeTimeoutMillis int, stdout, stderr io.Writer, d drainLeaseDeps) error {

	oldRec, err := d.LoadAgent(req.OldAgentID)
	if errors.Is(err, state.ErrNotFound) {
		// OLD already archived -> OLD's retire completed, but that is NOT
		// proof the doc was DELIVERED (codex PR3-completion iter-3 [P1]): a
		// GracefulCoordSwap that retired OLD but failed delivery leaves this
		// exact shape once the standby acquires (the epoch advanced, so the
		// barrier check upstream missed OLD's barrier). Deliver the pending
		// doc to the live leader before cleaning the queue — the at-most-once
		// sentinel short-circuits if a prior pass already delivered; a doc the
		// swap itself already injected is re-sent once (the benign §5c
		// duplicate, preferred over consuming the only durable inbox).
		if derr := d.DeliverPending(req, nil, stdout, stderr); derr != nil {
			return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
				req.OldAgentID, derr, path)
		}
		_, _ = fmt.Fprintf(stdout,
			"fleet drain: coord live for %s and %s already retired; cleaning stale queue\n",
			req.Project, req.OldAgentID)
		return queue.Delete(path)
	}

	// POLL (bounded) for the graceful barrier before falling back (codex PR3
	// iter-12 [P2]): a GracefulHandoff producer may have spawned a standby and
	// be about to write the barrier (named by the journal's barrier_id). If it
	// appears, finish the graceful path (which never spawns a second successor).
	// Only on timeout do we fall back to legacy Resume.
	for time.Now().Before(deadline) {
		if d.BarrierExists(req.Project) {
			return drainGraceful(req, path, deadline, stdout, stderr, d)
		}
		// Also bail early if the EXPECTED successor already took over the flock.
		if _, ok := d.healthySuccessorPresent(req.Project, req.NewAgentID); ok {
			break
		}
		wait := d.BarrierPoll
		if rem := time.Until(deadline); rem < wait {
			wait = rem
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	// The EXPECTED successor already leads (positive identity == req.NewAgentID,
	// D9b): a graceful standby has acquired the flock (its barrier was cleaned on
	// commit, so the barrier check above missed it). OLD's record just hasn't
	// been archived yet. The handoff IS complete — clean the stale queue, do NOT
	// run legacy Resume (which would spawn a SECOND replacement). A bare/foreign
	// busy flock never matches the expected id, so it never triggers this delete.
	if err == nil && oldRec != nil {
		if ownerPid, ok := d.healthySuccessorPresent(req.Project, req.NewAgentID); ok {
			// Same delivery-before-delete contract as drainGraceful (codex
			// PR3-completion iter-3 [P1]): the successor leading does not
			// prove the doc landed; the sentinel dedupes a re-send.
			if derr := d.DeliverPending(req, oldRec, stdout, stderr); derr != nil {
				return fmt.Errorf("fleet drain: deliver pending handoff doc to %s's successor: %w (queue %s preserved)",
					oldRec.ID, derr, path)
			}
			_, _ = fmt.Fprintf(stdout,
				"fleet drain: the expected successor already leads %s (flock owner pid %d); handoff complete, cleaning queue\n",
				req.Project, ownerPid)
			return queue.Delete(path)
		}
	}
	// OLD still present and still the leader -> complete the handoff the proven
	// way. Delegate to the LEGACY drain (LockAgent + handoffop.Resume): it
	// spawns + retires the successor exactly as production does today.
	//
	// SCOPE NOTE (codex PR4 [P1], accepted limitation): the successor spawned
	// here is NOT lease-wrapped — it is the legacy `claude` shape. Under the
	// lease-supported path that means a live `fleet handoff` completed via THIS
	// fallback yields a successor that coordinates but does
	// not hold/heartbeat the coordinator lease. This is SAFE, not a
	// correctness break: the successor's `/coordinator` tick reads "no healthy
	// active owner" (OLD released the lease on retire) and PROCEEDS (the
	// lease-check's no-lease-in-play branch) — identical to flag-off behavior.
	// What is lost is lease HA (heartbeat + STONITH) on that one successor
	// until it itself hands off through the warm-standby path. The
	// lease-PRESERVING transfer is GracefulHandoff (the OLD coord spawns its
	// own `coord-run --standby` successor that polls + acquires + receives the
	// resume prompt post-acquire after OLD exits). Wiring GracefulHandoff's
	// TRIGGER into the producer — and the standby's post-acquire prompt
	// delivery — is the remaining PR3-completion follow-up, deliberately NOT
	// bolted onto spawnAndRetire here (a standby successor cannot receive the
	// resume prompt through the spawn-then-retire send path, so doing so would
	// strand the successor idle — a worse failure than the bare-but-functional
	// successor). Tracked as a follow-up; this fallback stays the proven
	// legacy completion so no live handoff regresses on activation.
	//
	// coldResume runs d.Resume (production: handoffop.Resume) under d.LockAgent
	// (production: state.LockAgent) — byte-equivalent to drainOneLegacy.
	_, _ = fmt.Fprintf(stderr,
		"fleet drain: coord handoff pending for %s with no barrier yet; completing via legacy resume\n",
		req.Project)
	return coldResume(req, path, graceMillis, resumeTimeoutMillis, stdout, stderr, d)
}

// coldResume runs handoffop.Resume to complete a coord handoff (the live-leader
// fallback) or cold-spawn a successor (stealable no-epoch lease). It holds a
// SHORT per-agent LockAgent (Resume's contract; two concurrent drains must not
// both spawn).
//
// It HONORS resumeTimeoutMillis (codex PR3 iter-14 [P1]): even with a healthy
// leader present, handoffop.Resume can hang inside tmux probes / spawn /
// readiness waits, which would block `fleet drain` past `--resume-timeout-ms`
// and reintroduce the drain-storm class this PR exists to bound. So Resume runs
// in a goroutine bounded by the timeout; on timeout coldResume RETURNS (surface-
// don't-silo) rather than blocking.
//
// Abandoned-goroutine safety (the reason it was synchronous before): on timeout
// the goroutine keeps running but holds the BOUNDED per-agent LockAgent until
// Resume returns. A concurrent/later drain that contends for that lock times out
// (state.acquireBoundedAt never bare-LOCK_EXes) and stands down — so no two
// drains ever both spawn. `fleet drain` is a short-lived CLI: the goroutine dies
// with the process. The release runs whenever Resume finally returns (or never,
// if the process exits first — harmless, the kernel frees the flock).
//
// resumeTimeoutMillis <= 0 means "no timeout" (run synchronously) — preserves
// the legacy contract for callers that pass 0.
func coldResume(req queue.SpawnFresh, path string, graceMillis, resumeTimeoutMillis int,
	stdout, stderr io.Writer, d drainLeaseDeps) error {

	release, err := d.LockAgent(req.OldAgentID)
	if err != nil {
		return fmt.Errorf("fleet drain: lock agent %s for cold resume: %w", req.OldAgentID, err)
	}

	if resumeTimeoutMillis <= 0 {
		defer release()
		return d.Resume(req, path, graceMillis, stdout, stderr)
	}

	done := make(chan error, 1)
	go func() {
		// release runs when Resume returns (whether before or after the timeout),
		// so the lock is held for exactly the duration Resume needs it.
		defer release()
		done <- d.Resume(req, path, graceMillis, stdout, stderr)
	}()

	timer := time.NewTimer(time.Duration(resumeTimeoutMillis) * time.Millisecond)
	defer timer.Stop()
	select {
	case rerr := <-done:
		return rerr
	case <-timer.C:
		// Resume ran past the budget. Return without blocking; the goroutine
		// keeps the bounded lock until Resume unwinds (a contending drain times
		// out + stands down, never a duplicate spawn). Surface so the operator
		// sees the slow handoff (fleet doctor / a later drain can retry).
		//
		// The wrapped ErrResumeBackgrounded sentinel tells runDrain this is
		// in-progress, NOT failed — the handoff usually completes in the
		// background, and counting it failed produced a false "every pending
		// handoff failed" exit 1 (DESIGN-handoff-lifecycle-hardening bug A).
		_, _ = fmt.Fprintf(stderr,
			"fleet drain: resume for %s exceeded the %dms budget; returning (the handoff completes in the background or a later drain retries)\n",
			req.Project, resumeTimeoutMillis)
		return fmt.Errorf("fleet drain: resume for %s exceeded the %dms resume-timeout budget: %w",
			req.Project, resumeTimeoutMillis, ErrResumeBackgrounded)
	}
}

func fillDrainLeaseDeps(d drainLeaseDeps) drainLeaseDeps {
	def := defaultDrainLeaseDeps()
	if d.LeaderPresent == nil {
		d.LeaderPresent = def.LeaderPresent
	}
	if d.LiveOwner == nil {
		d.LiveOwner = def.LiveOwner
	}
	if d.ActiveOwnerPID == nil {
		d.ActiveOwnerPID = def.ActiveOwnerPID
	}
	if d.BarrierExists == nil {
		d.BarrierExists = def.BarrierExists
	}
	if d.LoadAgent == nil {
		d.LoadAgent = def.LoadAgent
	}
	if d.KillCoord == nil {
		d.KillCoord = def.KillCoord
	}
	if d.SupervisorAlive == nil {
		d.SupervisorAlive = def.SupervisorAlive
	}
	if d.Resume == nil {
		d.Resume = def.Resume
	}
	if d.LockAgent == nil {
		d.LockAgent = def.LockAgent
	}
	if d.RecoverSpawn == nil {
		d.RecoverSpawn = def.RecoverSpawn
	}
	if d.DeliverPending == nil {
		d.DeliverPending = def.DeliverPending
	}
	if d.BarrierPoll == 0 {
		d.BarrierPoll = def.BarrierPoll
	}
	if d.Self == nil {
		d.Self = def.Self
	}
	return d
}
