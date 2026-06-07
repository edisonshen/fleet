//go:build linux || darwin

// The lease primitive depends on platform_{linux,darwin}.go for
// pid_start / monotonic-clock / boot-id reads. Gate the whole file (and
// its tests) to those two GOOS values so non-linux/darwin Unix targets
// (e.g. FreeBSD) don't compile a lease.go whose platform hooks are
// undefined. PR1's failover path is dev-only behind FLEET_LEASE_FAILOVER;
// adding more platforms is a later PR if ever needed.

package coordlock

// lease.go — the three-file coordinator lease primitive (PR1 of the
// DESIGN-handoff-drain-storm-leak stack). PRIMITIVE ONLY: nothing here is
// wired into the live coord/handoff/drain paths (that is PR2-PR6).
// Everything is gated behind FLEET_LEASE_FAILOVER (default OFF), so this
// merge does not change production behavior.
//
// A lease = lock + TTL + heartbeat + fencing token. The current flock has
// the lock but none of the other three, so it detects a *dead* holder
// (kernel releases on death) but NOT a *hung-but-alive* one — that gap is
// the incident. This adds the three missing pieces as three files:
//
//	coordinator.flock        LIFETIME EXCLUSION. Plain flock inode, NEVER
//	                         renamed. Kernel releases it the instant the
//	                         holder process DIES. NB-acquired.
//	coordinator.epoch        FENCING DATA. Never flocked, never the lock.
//	                         {epoch,pid,pid_start,host,state,owner,
//	                          candidate,renewed_at_mono/_wall,boot_id}.
//	                         Atomic-written (.tmp -> fsync -> rename).
//	                         `epoch` is the fencing token.
//	coordinator.epoch.lock   STABLE serializer inode, NEVER renamed.
//	                         flock'd to serialize EVERY write to
//	                         coordinator.epoch (heartbeat AND takeover CAS).
//
// Takeover order is fixed: fence (epoch) -> kill (STONITH) -> acquire
// (flock). Never the reverse — a hung-but-alive holder still owns the
// flock, so something must advance independently of its lifetime (the
// epoch). PR1 ships the kill step as a STUB seam (killStub); the real
// authenticated KillCoordIfIdentityMatches lands in PR2.
//
//	AcquireLease ── LOCK_NB flock ──┬─ free  -> CAS active (release-on-CAS-fail)
//	                                └─ busy  -> read epoch ─┬─ healthy -> acquired=false
//	                                                        └─ hung    -> TakeOver
//	TakeOver     ── fence (epoch+1, state=fencing, owner=OLD) -> killStub(OLD)
//	                -> LOCK_NB flock -> CAS active (release-on-CAS-fail)

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// FailoverEnvVar gates the whole lease path. While unset/empty the lease
// library refuses to acquire (returns ErrFailoverDisabled) — PR1-PR3 keep
// it dev-only/unsupported; PR4 flips the default per the design.
const FailoverEnvVar = "FLEET_LEASE_FAILOVER"

// ErrFailoverDisabled is returned by lease entry points when
// FLEET_LEASE_FAILOVER is not enabled. Surface-don't-silo: callers get a
// typed, explanatory refusal rather than a silent no-op.
var ErrFailoverDisabled = errors.New(
	"coordlock: lease failover is disabled (set FLEET_LEASE_FAILOVER=1 to enable; " +
		"unsupported until the DESIGN-handoff-drain-storm-leak stack lands)")

// Lease state machine. `active` is the only state in which a holder may
// serve / heartbeat / hold the flock as owner. The two transient states
// belong to an in-progress takeover (see the two-phase rule).
const (
	stateActive            = "active"              // a live leader holds the flock and heartbeats
	stateFencing           = "fencing"             // a candidate fenced the old epoch, mid-takeover
	stateFencedNotAcquired = "fenced_not_acquired" // takeover could not kill/acquire; doctor recovery (PR6)
	stateReleased          = "released"            // holder cleanly released; tokens invalid immediately
)

// Lease tuning. TTL >= 3x heartbeat AND >= worst-case pause + clock skew
// (Kleppmann/Patroni). PR1 ships defaults; later PRs may make them
// configurable. Tests inject their own via newLeaseConfig.
const (
	defaultHeartbeat = 10 * time.Second
	defaultTTL       = 30 * time.Second
	// flockRetryBudget bounds how long a takeover candidate retries the
	// post-kill flock acquire (old holder slow to die) before escalating
	// to fenced_not_acquired.
	defaultFlockRetryBudget = 5 * time.Second
)

// epochRecord is the on-disk fencing data (coordinator.epoch). It is data
// only — never flocked. Atomic-written via .tmp -> fsync -> rename.
type epochRecord struct {
	Epoch     int64    `json:"epoch"`
	State     string   `json:"state"` // active | fencing | fenced_not_acquired
	Owner     identity `json:"owner"`
	Candidate identity `json:"candidate,omitempty"` // set only during a takeover
	Host      string   `json:"host"`
	BootID    string   `json:"boot_id"`
	// renewed_at is stored as raw CLOCK_MONOTONIC ns (jump-immune, used
	// for TTL) plus a wall-clock stamp (diagnostics ONLY — never compared
	// for TTL). The mono value is comparable only within BootID.
	RenewedAtMono int64 `json:"renewed_at_mono"`
	RenewedAtWall int64 `json:"renewed_at_wall"`
}

// identity is the full identity tuple of a lease holder/candidate. Both
// pid AND pid_start are required for PID-reuse-safe liveness.
type identity struct {
	Pid      int    `json:"pid"`
	PidStart int64  `json:"pid_start"`
	AgentID  string `json:"agent_id"`
	Project  string `json:"project"`
}

func (i identity) equal(o identity) bool {
	return i.Pid == o.Pid && i.PidStart == o.PidStart &&
		i.AgentID == o.AgentID && i.Project == o.Project
}

// leaseConfig holds the tunables + injectable seams so tests stay
// deterministic (no time.Sleep timing assertions): a fake monotonic
// clock, an injectable pid-liveness probe, an injectable boot-id, and the
// kill-stub seam (PR2 swaps in the real authenticated kill).
type leaseConfig struct {
	heartbeat        time.Duration
	ttl              time.Duration
	flockRetryBudget time.Duration

	// nowMono returns monotonic ns. Production: monotonicNanos.
	nowMono func() int64
	// pidStart returns a process start time + ok. Production wraps
	// pidStartNanos: ok=false means the pid is dead/unreadable.
	pidStart func(pid int) (int64, bool)
	// boot returns the current boot id. Production: bootID.
	boot func() string
	// killStub fences-then-kills the old holder. PR1 STUB seam — default
	// is a no-op that reports success (the kernel-flock release is what
	// the takeover actually waits on; PR2 wires the authenticated kill).
	killStub func(owner identity) error
}

func defaultLeaseConfig() leaseConfig {
	return leaseConfig{
		heartbeat:        defaultHeartbeat,
		ttl:              defaultTTL,
		flockRetryBudget: defaultFlockRetryBudget,
		nowMono:          monotonicNanos,
		pidStart: func(pid int) (int64, bool) {
			st, err := pidStartNanos(pid)
			if err != nil {
				return 0, false
			}
			return st, true
		},
		boot: bootID,
		// PR1 stub: the real KillCoordIfIdentityMatches lands in PR2.
		// A no-op is correct for PR1 because TakeOver's flock acquire is
		// kernel-gated on the old holder actually dying; the stub just
		// marks the seam. Tests override this to simulate the kill.
		killStub: func(identity) error { return nil },
	}
}

// Lease is a held coordinator lease. The flock fd is held for the lease's
// lifetime; Release() (or process death) frees it.
type Lease struct {
	cfg   leaseConfig
	paths leasePaths
	self  identity
	host  string
	boot  string

	flock *os.File // held coordinator.flock fd

	epoch int64 // the epoch this lease owns (== on-disk while active)

	stopHB chan struct{} // closed by Release to stop the heartbeat goroutine
	hbDone chan struct{} // closed by the heartbeat goroutine on exit
}

// leasePaths resolves the three files under the project's .locks/ dir.
type leasePaths struct {
	flock     string // coordinator.flock
	epoch     string // coordinator.epoch
	epochLock string // coordinator.epoch.lock
}

func resolvePaths(project string) (leasePaths, error) {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return leasePaths{}, err
	}
	lockDir := filepath.Join(filepath.Clean(pdir), ".locks")
	return leasePaths{
		flock:     filepath.Join(lockDir, "coordinator.flock"),
		epoch:     filepath.Join(lockDir, "coordinator.epoch"),
		epochLock: filepath.Join(lockDir, "coordinator.epoch.lock"),
	}, nil
}

// failoverEnabled reports whether FLEET_LEASE_FAILOVER selects the lease
// path. Any non-empty, non-"0"/"false" value enables it.
func failoverEnabled() bool {
	v := os.Getenv(FailoverEnvVar)
	return v != "" && v != "0" && v != "false"
}

// AcquireLease tries to become the leader for project. It is the
// production entry point and refuses unless FLEET_LEASE_FAILOVER is on.
//
//	acquired=true,  lease!=nil  -> caller is the leader (start heartbeat)
//	acquired=false, lease==nil  -> a healthy live leader exists; stand down
//	err!=nil                    -> something went wrong (or failover off)
func AcquireLease(project, agentID string) (lease *Lease, acquired bool, err error) {
	if !failoverEnabled() {
		return nil, false, ErrFailoverDisabled
	}
	return acquireLease(project, agentID, defaultLeaseConfig())
}

// acquireLease is the failover-gate-free core, parameterized by config so
// tests inject deterministic seams. See AcquireLease for the contract.
func acquireLease(project, agentID string, cfg leaseConfig) (*Lease, bool, error) {
	if project == "" {
		return nil, false, fmt.Errorf("coordlock.AcquireLease: project must not be empty")
	}
	paths, err := resolvePaths(project)
	if err != nil {
		return nil, false, fmt.Errorf("coordlock.AcquireLease: resolve paths: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		return nil, false, fmt.Errorf("coordlock.AcquireLease: mkdir lock dir: %w", err)
	}

	self, err := selfIdentity(project, agentID, cfg)
	if err != nil {
		return nil, false, err
	}
	l := &Lease{cfg: cfg, paths: paths, self: self, host: hostname(), boot: cfg.boot()}

	// Bound the acquire/takeover convergence loop so a pathological
	// release-on-CAS-fail ping-pong can't spin forever. Each iteration is
	// either a successful flock+CAS (returns) or observes another
	// candidate's progress (which is finite).
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		done, acquired, err := l.tryAcquireOnce()
		if errors.Is(err, errSerializerBusy) {
			// Transient: epoch.lock was busy this pass. Re-evaluate from a
			// clean flock state rather than failing the whole acquire.
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if done {
			if acquired {
				return l, true, nil
			}
			return nil, false, nil
		}
		// Not done: a CAS lost / takeover needed and we should re-evaluate
		// from a clean flock state. Loop.
	}
	return nil, false, fmt.Errorf("coordlock.AcquireLease: did not converge after %d attempts (contended)", maxAttempts)
}

// tryAcquireOnce runs one acquire attempt.
//
//	done=true,  acquired=true  -> l is the active leader (flock held, CAS done)
//	done=true,  acquired=false -> healthy live leader exists; stand down
//	done=false                 -> caller should retry (CAS lost / converging)
func (l *Lease) tryAcquireOnce() (done, acquired bool, err error) {
	f, gotFlock, err := tryFlock(l.paths.flock)
	if err != nil {
		return false, false, err
	}
	if gotFlock {
		// Stamp identity+clock into the flock body so a candidate can
		// recover us if we hang before writing the epoch (P2 window).
		l.stampFlockBody(f)
		// FREE-FLOCK FAST PATH: the previous holder is GONE — holding the
		// flock is proof of that (the kernel releases on death, and an
		// explicit Release() frees it; either way no other process holds
		// the lifetime-exclusion lock now). Snapshot the epoch we observed
		// at acquire, then CAS to active conditioned on the on-disk epoch
		// still equalling that snapshot. The snapshot-CAS (not an owner-
		// liveness heuristic) is what distinguishes the two cases:
		//   - clean release/death: epoch unchanged -> CAS succeeds, even if
		//     the recorded owner's PID is still briefly alive (it released).
		//   - acquire-to-epoch window race (T37): a candidate bumped the
		//     epoch via TakeOver -> snapshot mismatch -> CAS fails -> we
		//     HARD-release the flock and retry (never held by a non-active
		//     owner).
		observed, rerr := readEpoch(l.paths.epoch)
		switch {
		case errors.Is(rerr, os.ErrNotExist):
			observed = epochRecord{Epoch: 0} // first-ever leader
		case rerr != nil:
			_ = releaseFlock(f)
			return false, false, fmt.Errorf("coordlock: read epoch snapshot: %w", rerr)
		}
		// Don't barge into a FRESH in-progress takeover: the old holder
		// died (flock free) but the first candidate may not have reacquired
		// the flock yet — its fencing record is fresh and its candidate PID
		// alive. Promoting to active here would steal a healthy takeover and
		// make that candidate time out. Only a FENCING record gets this
		// gate; a fenced_not_acquired record is a gave-up escalation and the
		// flock is free, so we simply promote (CAS to active) and recover.
		if observed.State == stateFencing {
			if !l.transientResumable(observed) {
				_ = releaseFlock(f)
				return true, false, nil // let the live candidate finish
			}
		}
		ok, err := l.casToActiveAfterFlock(observed.Epoch)
		if err != nil {
			_ = releaseFlock(f)
			return false, false, err
		}
		if !ok {
			_ = releaseFlock(f)
			return false, false, nil
		}
		l.flock = f
		return true, true, nil
	}

	// Flock is busy: a live holder owns it. Read the epoch to decide
	// healthy (stand down) vs hung (take over).
	rec, err := readEpoch(l.paths.epoch)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Busy flock but NO epoch record: a holder that grabbed the
			// flock and hasn't written the epoch yet. Don't stand down
			// indefinitely (that would make a holder hung in this window
			// unrecoverable). Inspect the flock-body stamp: a FRESH
			// same-boot live holder is legitimately booting -> stand down;
			// a stale/dead/cross-boot body is hung -> take over via a
			// synthetic record naming the flock-body holder as owner.
			if !l.flockHolderRecoverable() {
				return true, false, nil
			}
			return l.takeOver(l.syntheticRecordFromFlockBody())
		}
		return false, false, fmt.Errorf("coordlock: read epoch: %w", err)
	}
	if l.holderHealthy(rec) {
		return true, false, nil // healthy live leader -> stand down
	}
	// A transient (fencing / fenced_not_acquired) record means another
	// candidate is mid-takeover. Do NOT barge in and re-fence a FRESH
	// in-progress takeover whose candidate is still alive and within its
	// retry budget — that makes multiple candidates invalidate each
	// other's CAS and thrash. Only a FENCING record gets the freshness
	// gate (let a live, in-budget takeover finish). A fenced_not_acquired
	// record is NOT in progress: a prior candidate already GAVE UP after
	// failing to acquire the flock, so a new candidate must immediately
	// re-attempt recovery rather than wait out the TTL silently. So only
	// gate stand-down on fencing; fenced_not_acquired always falls through
	// to takeOver (which re-fences/kills/acquires or re-escalates loudly).
	if rec.State == stateFencing {
		if !l.transientResumable(rec) {
			return true, false, nil // fresh in-progress takeover -> stand down
		}
	}
	// Hung-but-alive holder (flock still held, renewed_at frozen > TTL), a
	// stalled fencing record, or a fenced_not_acquired escalation. Run the
	// takeover state machine; it returns the same done/acquired contract.
	return l.takeOver(rec)
}

// transientResumable reports whether a FENCING record may be resumed by a
// new candidate. True only if the in-progress takeover looks abandoned:
// its renewed_at is past TTL (mono, same boot), OR it is from a previous
// boot, OR its candidate PID is dead. A fresh record with a live candidate
// within budget is NOT resumable (let the first takeover complete).
// fenced_not_acquired is handled separately (always resumable) — it is a
// gave-up escalation, not an in-progress takeover.
func (l *Lease) transientResumable(rec epochRecord) bool {
	if rec.BootID != l.boot {
		return true // previous boot -> stale
	}
	if l.cfg.nowMono()-rec.RenewedAtMono > int64(l.cfg.ttl) {
		return true // stalled past TTL
	}
	// Fresh record: resumable only if the candidate driving it is dead.
	return !l.pidAlive(rec.Candidate)
}

// holderHealthy reports whether the recorded holder is a healthy active
// leader: state==active AND same boot AND within TTL (monotonic) AND
// pid+pid_start alive. Any failing clause => stealable.
func (l *Lease) holderHealthy(rec epochRecord) bool {
	if rec.State != stateActive {
		return false
	}
	if rec.BootID != l.boot {
		// Record is from a previous boot — its monotonic stamp is
		// meaningless; treat as expired/stealable (P3).
		return false
	}
	elapsed := l.cfg.nowMono() - rec.RenewedAtMono
	if elapsed > int64(l.cfg.ttl) {
		return false // TTL expired -> hung
	}
	return l.pidAlive(rec.Owner)
}

// pidAlive reports whether id's process is the same live process recorded
// at acquire — pid AND pid_start together (kill(pid,0) alone is
// PID-reuse-unsafe; T4).
func (l *Lease) pidAlive(id identity) bool {
	if id.Pid <= 0 {
		return false
	}
	st, ok := l.cfg.pidStart(id.Pid)
	if !ok {
		return false // pid gone
	}
	return st == id.PidStart // recycled pid -> start-time mismatch -> dead
}

// casToActiveAfterFlock writes {epoch=observedEpoch+1, state=active,
// owner=me} under coordinator.epoch.lock, but ONLY if the on-disk epoch
// still equals observedEpoch (the value the caller read at flock acquire).
// A mismatch means a candidate advanced the epoch in our acquire-to-CAS
// window -> ok=false WITHOUT writing; the caller HARD-releases the flock
// and retries (release-on-CAS-fail). Holding the flock is the proof of
// leadership, so no owner-liveness heuristic is needed here.
func (l *Lease) casToActiveAfterFlock(observedEpoch int64) (bool, error) {
	return l.withEpochLock(func() (bool, error) {
		cur, err := readEpoch(l.paths.epoch)
		switch {
		case errors.Is(err, os.ErrNotExist):
			cur = epochRecord{Epoch: 0} // first-ever leader
		case err != nil:
			return false, err
		}
		if cur.Epoch != observedEpoch {
			return false, nil // someone advanced the epoch in our window
		}
		l.epoch = cur.Epoch + 1
		return true, l.writeEpochLocked(epochRecord{
			Epoch: l.epoch,
			State: stateActive,
			Owner: l.self,
		})
	})
}

// takeOver runs the fence -> kill -> acquire state machine against a
// hung-but-alive holder, serialized by coordinator.epoch.lock. Returns
// the tryAcquireOnce done/acquired contract.
//
//  1. Under epoch.lock, re-read; if the holder became healthy or the epoch
//     moved, stand down (done depends on which). Else CAS to fencing with
//     epoch+1, owner=OLD holder (or the recorded OLD for a resumed
//     fencing record), candidate=me.
//  2. killStub(OLD) — PR1 no-op seam; PR2 authenticated kill. The kill
//     releases the OLD coord-run's kernel flock on death.
//  3. LOCK_NB coordinator.flock (now free), then CAS to active/owner=me.
//     On CAS fail, release flock + retry (release-on-CAS-fail).
func (l *Lease) takeOver(rec epochRecord) (done, acquired bool, err error) {
	// Determine the OLD holder we fence + kill. For a fresh active record
	// it is rec.Owner; for a resumed fencing/fenced record it is the
	// recorded Owner (the original OLD holder, preserved by the two-phase
	// rule), NOT the dead candidate.
	old := rec.Owner
	// priorCandidate is the candidate recorded in a fencing/fenced record
	// we are RESUMING — it may be the process that actually holds the flock
	// (it acquired it and hung before casFencingToActive), even if its
	// best-effort flock-body stamp is missing/stale. We must include it in
	// the kill targets so a hung resumed-candidate is reaped (else
	// retryFlock just times out into fenced_not_acquired).
	priorCandidate := rec.Candidate

	// Phase 1: fence under epoch.lock.
	fenced, err := l.withEpochLock(func() (bool, error) {
		cur, rerr := readEpoch(l.paths.epoch)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				// No epoch file. Only the acquire-to-epoch-window takeover
				// (synthetic rec.Epoch==0, owner=flock-body holder) may
				// proceed here, fencing from epoch 0. Any other caller saw
				// a record that has since vanished -> bail + retry.
				if rec.Epoch != 0 {
					return false, nil
				}
				cur = epochRecord{Epoch: 0}
			} else {
				return false, rerr
			}
		}
		// Another candidate/heartbeat moved the epoch past what we saw.
		if cur.Epoch != rec.Epoch {
			return false, nil // stand down; outer loop re-reads
		}
		// Holder became healthy under the lock? Stand down. (A real record
		// only; the synthetic no-epoch case has no on-disk owner to vouch.)
		if cur.State == stateActive && l.holderHealthy(cur) {
			return false, nil
		}
		// Owner is the recorded OLD holder — for a fresh active record the
		// live-but-hung leader; for a resumed fencing/fenced record the
		// two-phase rule preserved the original OLD holder (never the dead
		// candidate). For the synthetic no-epoch case there is no on-disk
		// owner, so fall back to the flock-body holder passed in via rec.
		if cur.Owner.Pid != 0 {
			old = cur.Owner
		} // else keep `old` = rec.Owner (the flock-body holder)
		// If resuming a transient record, the on-disk candidate is the
		// prior takeover driver that may hold the flock — prefer it over the
		// (possibly stale) value passed in via rec.
		if (cur.State == stateFencing || cur.State == stateFencedNotAcquired) && cur.Candidate.Pid != 0 {
			priorCandidate = cur.Candidate
		}
		l.epoch = cur.Epoch + 1
		return true, l.writeEpochLocked(epochRecord{
			Epoch:     l.epoch,
			State:     stateFencing,
			Owner:     old, // two-phase: OLD stays owner until we hold the flock
			Candidate: l.self,
		})
	})
	if err != nil {
		return false, false, err
	}
	if !fenced {
		// Lost the fence race or holder recovered. Let the outer loop
		// re-read and decide (it may now stand down or retry).
		return false, false, nil
	}

	// Phase 2: kill (PR1 stub). On stub error, leave fenced_not_acquired
	// for doctor (PR6) — surface, don't silently stall.
	//
	// Whoever actually HOLDS coordinator.flock is what blocks our acquire,
	// and that is not always `old`: when resuming a stale fencing record
	// whose previous candidate acquired the flock + stamped it before
	// hanging, the live flock holder is that candidate, not the original
	// owner. Kill every distinct live identity that could be holding the
	// flock — the recorded owner, the prior fencing candidate (the flock
	// holder when resuming a stale takeover, even if its body stamp is
	// missing), AND the current flock-body holder — so a hung resumed
	// candidate is reaped too (else retryFlock just times out).
	for _, target := range l.killTargets(old, priorCandidate) {
		if kerr := l.cfg.killStub(target); kerr != nil {
			_ = l.markFencedNotAcquired(old)
			return false, false, fmt.Errorf("coordlock.TakeOver: kill stub failed for pid=%d: %w", target.Pid, kerr)
		}
	}

	// Phase 3: acquire the (now-free) flock, then CAS to active.
	f, gotFlock, err := l.retryFlock()
	if err != nil {
		return false, false, err
	}
	if !gotFlock {
		// Old holder did not die within the retry budget. Mark
		// fenced_not_acquired and escalate (PR6 doctor). Surface, don't
		// stall silently.
		_ = l.markFencedNotAcquired(old)
		return true, false, fmt.Errorf(
			"coordlock.TakeOver: fenced old holder pid=%d but could not acquire flock within %s; "+
				"run `fleet doctor` to recover", old.Pid, l.cfg.flockRetryBudget)
	}

	l.stampFlockBody(f) // recovery stamp before we CAS to active
	ok, err := l.casFencingToActive(f)
	if err != nil {
		_ = releaseFlock(f)
		return false, false, err
	}
	if !ok {
		// Epoch moved again under us — release + retry (release-on-CAS-fail).
		_ = releaseFlock(f)
		return false, false, nil
	}
	l.flock = f
	return true, true, nil
}

// casFencingToActive promotes our fencing record to active/owner=me, but
// ONLY if the on-disk epoch is still our fencing epoch (no later candidate
// bumped it). On mismatch returns ok=false WITHOUT writing.
func (l *Lease) casFencingToActive(_ *os.File) (bool, error) {
	return l.withEpochLock(func() (bool, error) {
		cur, err := readEpoch(l.paths.epoch)
		if err != nil {
			return false, err
		}
		if cur.Epoch != l.epoch {
			return false, nil // someone advanced past our fencing epoch
		}
		return true, l.writeEpochLocked(epochRecord{
			Epoch: l.epoch, // keep our fencing epoch; we are the same authority
			State: stateActive,
			Owner: l.self,
		})
	})
}

// markFencedNotAcquired records the typed escalation state so `fleet
// doctor` (PR6) can offer operator-confirmed recovery — never a silent
// stall. Best-effort: it keeps our fencing epoch + the OLD owner.
func (l *Lease) markFencedNotAcquired(old identity) error {
	_, err := l.withEpochLock(func() (bool, error) {
		cur, rerr := readEpoch(l.paths.epoch)
		if rerr == nil && cur.Epoch != l.epoch {
			return false, nil // a later candidate took over — leave it
		}
		return true, l.writeEpochLocked(epochRecord{
			Epoch:     l.epoch,
			State:     stateFencedNotAcquired,
			Owner:     old,
			Candidate: l.self,
		})
	})
	return err
}

// Heartbeat starts a goroutine that renews the lease until ctx-equivalent
// stop (Release) or self-demotion. It is an EPOCH-PRESERVING CAS: under
// coordinator.epoch.lock it re-reads the epoch and writes ONLY if
// epoch+pid+pid_start still equal ours, updating ONLY renewed_at. If the
// epoch advanced past ours (a candidate fenced us), it STOPS and
// self-demotes — it does NOT roll the epoch back. (Closes the
// zombie-heartbeat-rollback P0.)
func (l *Lease) Heartbeat() {
	if l.stopHB != nil {
		return // already heartbeating
	}
	l.stopHB = make(chan struct{})
	l.hbDone = make(chan struct{})
	go l.heartbeatLoop()
}

func (l *Lease) heartbeatLoop() {
	defer close(l.hbDone)
	t := time.NewTicker(l.cfg.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stopHB:
			return
		case <-t.C:
			ok, err := l.heartbeatOnce()
			if errors.Is(err, errSerializerBusy) {
				// Benign: another writer held epoch.lock for this whole
				// tick. Do NOT demote — skip this tick and renew on the
				// next one (TTL >= 3x heartbeat, so a skipped tick is safe).
				continue
			}
			if err != nil || !ok {
				// A real write fault, or we were fenced (ok=false) —
				// self-demote by stopping the heartbeat. The Release path
				// (and the kernel flock release on exit) finishes it.
				return
			}
		}
	}
}

// heartbeatOnce performs one epoch-preserving CAS renew. ok=false means
// "self-demote" (epoch moved / no longer owner); err is a write fault.
func (l *Lease) heartbeatOnce() (bool, error) {
	return l.withEpochLock(func() (bool, error) {
		cur, err := readEpoch(l.paths.epoch)
		if err != nil {
			return false, err
		}
		// Write ONLY if still exactly ours.
		if cur.State != stateActive || cur.Epoch != l.epoch || !cur.Owner.equal(l.self) {
			return false, nil // fenced / not ours -> self-demote, do NOT write
		}
		// Update ONLY renewed_at; never the epoch, never from cached state.
		cur.RenewedAtMono = l.cfg.nowMono()
		cur.RenewedAtWall = time.Now().UnixNano()
		cur.Host = l.host
		cur.BootID = l.boot
		return true, l.writeEpochLocked(cur)
	})
}

// Release stops the heartbeat, DEMOTES the epoch record to `released`, then
// closes the flock fd (the kernel also releases the flock on process
// death). Demoting the record before dropping the flock is what makes any
// still-outstanding LeaseToken.StillOwned() return false IMMEDIATELY (the
// state is no longer `active`) — without it a token held by a racing
// goroutine would keep validating until TTL/successor, admitting a
// non-holder's mutation. Idempotent. Cleanup-as-last-step: callers
// `defer lease.Release()` so it runs on every exit path.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	if l.stopHB != nil {
		close(l.stopHB)
		<-l.hbDone
		l.stopHB = nil
	}
	// Demote the record to `released` while we still hold the flock (so a
	// successor can't have advanced the epoch under us yet). Only if it is
	// still exactly ours — never stomp a successor's record. Best-effort:
	// the kernel flock release below is the hard guarantee; this just makes
	// token invalidation immediate.
	if l.flock != nil {
		_, _ = l.withEpochLock(func() (bool, error) {
			cur, err := readEpoch(l.paths.epoch)
			if err != nil {
				return false, err //nolint:nilerr // best-effort; flock release below is the guarantee.
			}
			if cur.State != stateActive || cur.Epoch != l.epoch || !cur.Owner.equal(l.self) {
				return false, nil // not ours anymore -> leave it
			}
			cur.State = stateReleased
			return true, l.writeEpochLocked(cur)
		})
	}
	if l.flock != nil {
		_ = releaseFlock(l.flock)
		l.flock = nil
	}
}

// Token returns the full-identity LeaseToken for the central lease-gated
// mutation APIs (PR4). It carries the epoch + identity, not just the epoch
// — an epoch-only check would let a future/non-owner token through.
func (l *Lease) Token() LeaseToken {
	return LeaseToken{
		Epoch:    l.epoch,
		Project:  l.self.Project,
		Pid:      l.self.Pid,
		PidStart: l.self.PidStart,
		AgentID:  l.self.AgentID,
		paths:    l.paths,
		boot:     l.boot,
		cfg:      l.cfg,
	}
}

// LeaseToken is a capability proving lease ownership at a point in time.
// StillOwned() re-reads the epoch and confirms the holder is STILL us.
type LeaseToken struct {
	Epoch    int64
	Project  string
	Pid      int
	PidStart int64
	AgentID  string

	paths leasePaths
	boot  string
	cfg   leaseConfig
}

// StillOwned reports whether this token still owns the active lease:
// state==active AND epoch==tok.Epoch AND owner==tok identity AND the
// record is NOT self-expired (same boot AND renewed_at within TTL). This
// is the boundary check the central mutation APIs (PR4) use to reject a
// woken zombie BEFORE it mutates.
//
// The self-expiry clause is load-bearing (Patroni "only act if I still
// own the lease", Kleppmann): a leader that paused past its own TTL and
// wakes BEFORE any candidate has fenced it would otherwise still read its
// own active epoch+owner and pass — yet it has provably missed its renewal
// window and a takeover may be imminent. Rejecting on stale renewed_at /
// cross-boot makes a paused holder self-demote at the boundary rather than
// race a takeover.
func (t LeaseToken) StillOwned() bool {
	rec, err := readEpoch(t.paths.epoch)
	if err != nil {
		return false
	}
	if rec.State != stateActive {
		return false
	}
	if rec.Epoch != t.Epoch {
		return false
	}
	if rec.BootID != t.boot {
		return false // record from a previous boot -> mono stamp meaningless
	}
	if t.cfg.nowMono()-rec.RenewedAtMono > int64(t.cfg.ttl) {
		return false // self-expired: missed our own TTL renewal window
	}
	want := identity{Pid: t.Pid, PidStart: t.PidStart, AgentID: t.AgentID, Project: t.Project}
	return rec.Owner.equal(want)
}

// --- helpers: flock, epoch I/O, identity ---

// flockBody is the identity+timestamp stamped into coordinator.flock's
// body immediately after a successful acquire. It is the recovery signal
// for the acquire-to-epoch window: a holder that grabs the flock and then
// hangs BEFORE writing coordinator.epoch leaves no epoch record, so the
// only fact a candidate has is this body. A body whose Mono is older than
// the TTL (same boot) — or whose pid is dead / from another boot — marks a
// holder hung in the window, recoverable via takeover. A fresh body is a
// legitimate booting holder (stand down). Best-effort: flock alone
// enforces exclusion; the body is a recovery aid, never the lock.
type flockBody struct {
	Pid      int    `json:"pid"`
	PidStart int64  `json:"pid_start"`
	BootID   string `json:"boot_id"`
	Mono     int64  `json:"mono"`
}

// tryFlock opens (creating if absent) path and takes a single LOCK_NB
// exclusive flock. gotFlock=false on EWOULDBLOCK (a live holder exists).
// The fd is returned ONLY when gotFlock=true; otherwise it is closed. It
// does NOT stamp the body — the lease methods stamp via stampFlockBody so
// the stamp carries the lease's full identity + clock.
func tryFlock(path string) (f *os.File, gotFlock bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lerr == nil {
		return f, true, nil
	}
	_ = f.Close()
	if lerr == syscall.EWOULDBLOCK { //nolint:errorlint // bare errno from syscall.Flock.
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("flock %s: %w", path, lerr)
}

// stampFlockBody writes this lease's identity + boot + mono into the held
// flock fd's body, immediately after acquire (before writing the epoch).
// Best-effort.
func (l *Lease) stampFlockBody(f *os.File) {
	b, err := json.Marshal(flockBody{
		Pid: l.self.Pid, PidStart: l.self.PidStart, BootID: l.boot, Mono: l.cfg.nowMono(),
	})
	if err != nil {
		return
	}
	if e := f.Truncate(0); e != nil {
		return
	}
	if _, e := f.Seek(0, 0); e != nil {
		return
	}
	_, _ = f.Write(b)
	_ = f.Sync()
}

// syntheticRecordFromFlockBody builds the takeOver input for the
// acquire-to-epoch-window case (busy flock, no epoch record). Epoch 0 is
// the sentinel the fence closure recognizes to fence-from-zero; Owner is
// the flock-body holder so the kill-step targets the right PID. A missing/
// unparseable body yields a zero owner — takeOver will still fence + (in
// PR2) STONITH by other means; the kill stub is a no-op in PR1.
func (l *Lease) syntheticRecordFromFlockBody() epochRecord {
	rec := epochRecord{Epoch: 0, State: stateFencing}
	b, err := os.ReadFile(l.paths.flock)
	if err != nil || len(b) == 0 {
		return rec
	}
	var body flockBody
	if json.Unmarshal(b, &body) != nil {
		return rec
	}
	rec.Owner = identity{Pid: body.Pid, PidStart: body.PidStart, Project: l.self.Project}
	return rec
}

// killTargets returns the distinct live identities a takeover must kill to
// free coordinator.flock: the recorded OLD owner, any extra identities the
// caller passes (the prior fencing candidate when resuming a stale
// takeover), and the current flock-body holder. The candidate is included
// EXPLICITLY because stampFlockBody is best-effort — the candidate may hold
// the flock with a missing/stale body, and missing it would leave the
// takeover to time out into fenced_not_acquired. Dead/empty/self identities
// are dropped — the kill primitive (PR2) re-validates again before
// signaling, but we avoid handing it provably-irrelevant targets.
func (l *Lease) killTargets(old identity, extra ...identity) []identity {
	targets := make([]identity, 0, 3)
	add := func(id identity) {
		if id.Pid <= 0 || id.Pid == l.self.Pid || !l.pidAlive(id) {
			return
		}
		for _, t := range targets {
			if t.Pid == id.Pid && t.PidStart == id.PidStart {
				return // already queued
			}
		}
		targets = append(targets, id)
	}
	add(old)
	for _, e := range extra {
		add(e)
	}
	// The flock-body holder (whoever stamped coordinator.flock last).
	if b, err := os.ReadFile(l.paths.flock); err == nil && len(b) > 0 {
		var body flockBody
		if json.Unmarshal(b, &body) == nil && body.Pid > 0 {
			add(identity{Pid: body.Pid, PidStart: body.PidStart, Project: l.self.Project})
		}
	}
	// If none are live (all already dead), still hand the kill primitive
	// the recorded owner so the PR1 stub / PR2 kill has a stable target;
	// the flock is already free in that case so retryFlock will win anyway.
	if len(targets) == 0 {
		targets = append(targets, old)
	}
	return targets
}

// flockHolderRecoverable reports whether a BUSY flock with NO epoch record
// (a holder that hung in the acquire-to-epoch window) is recoverable via
// takeover. It reads the flock body: recoverable if the body is missing/
// unparseable, from another boot, its pid is dead, or its Mono is older
// than the TTL. A fresh same-boot body with a live pid is a legitimate
// booting holder -> NOT recoverable (stand down).
func (l *Lease) flockHolderRecoverable() bool {
	b, err := os.ReadFile(l.paths.flock)
	if err != nil || len(b) == 0 {
		return true // no body to vouch for the holder -> recoverable
	}
	var body flockBody
	if json.Unmarshal(b, &body) != nil {
		return true // unparseable stamp -> recoverable
	}
	if body.BootID != l.boot {
		return true // previous boot
	}
	if !l.pidAlive(identity{Pid: body.Pid, PidStart: body.PidStart}) {
		return true // holder pid dead
	}
	return l.cfg.nowMono()-body.Mono > int64(l.cfg.ttl) // hung past TTL
}

// retryFlock polls LOCK_NB on coordinator.flock until acquired or the
// retry budget elapses (old holder slow to die after the kill).
func (l *Lease) retryFlock() (*os.File, bool, error) {
	deadline := time.Now().Add(l.cfg.flockRetryBudget)
	for {
		f, ok, err := tryFlock(l.paths.flock)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return f, true, nil
		}
		if !time.Now().Before(deadline) {
			return nil, false, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func releaseFlock(f *os.File) error {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

// errSerializerBusy means coordinator.epoch.lock was held by another
// writer for the whole bounded acquire budget. It is a TRANSIENT outcome,
// NOT a self-demotion: the heartbeat must keep renewing (skip this tick),
// while acquire/takeover treat it as "stand down + retry on the next
// pass". Distinguishing it from fn()'s ok=false (real demotion / fenced)
// closes the spurious-heartbeat-stop hole (codex iter-6).
var errSerializerBusy = errors.New("coordlock: epoch.lock serializer busy")

// epochLockBudget bounds how long withEpochLock polls LOCK_NB for the
// serializer before returning errSerializerBusy. Epoch-lock writers hold
// it only for one fast atomic write, so a short budget reliably wins it
// under benign contention without blocking forever. A var (not const) only
// so a test can shrink it; production never reassigns it.
var epochLockBudget = 2 * time.Second

// withEpochLock runs fn while holding coordinator.epoch.lock (the stable
// serializer inode, NEVER renamed). The flock is LOCK_NB, polled until the
// epochLockBudget elapses (writers hold it briefly). On budget exhaustion
// it returns errSerializerBusy — a transient signal callers distinguish
// from a real demotion (fn returning ok=false). It NEVER blocks forever.
func (l *Lease) withEpochLock(fn func() (bool, error)) (bool, error) {
	f, err := os.OpenFile(l.paths.epochLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, fmt.Errorf("open epoch.lock: %w", err)
	}
	defer func() { _ = f.Close() }()

	deadline := time.Now().Add(epochLockBudget)
	for {
		lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if lerr == nil {
			defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
			return fn()
		}
		if lerr != syscall.EWOULDBLOCK { //nolint:errorlint // bare errno.
			return false, fmt.Errorf("flock epoch.lock: %w", lerr)
		}
		if !time.Now().Before(deadline) {
			return false, errSerializerBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readEpoch reads + unmarshals coordinator.epoch. Returns os.ErrNotExist
// (unwrapped via errors.Is) when the file is absent.
func readEpoch(path string) (epochRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return epochRecord{}, err
	}
	var rec epochRecord
	if uerr := json.Unmarshal(b, &rec); uerr != nil {
		return epochRecord{}, fmt.Errorf("unmarshal epoch %s: %w", path, uerr)
	}
	return rec, nil
}

// writeEpochLocked atomically writes coordinator.epoch (.tmp -> fsync ->
// rename). MUST be called only while holding coordinator.epoch.lock (it
// does not take the lock itself — withEpochLock does). It fills in the
// host/boot/renewed_at fields the caller did not set so every write is a
// complete record.
func (l *Lease) writeEpochLocked(rec epochRecord) error {
	if rec.Host == "" {
		rec.Host = l.host
	}
	if rec.BootID == "" {
		rec.BootID = l.boot
	}
	if rec.RenewedAtMono == 0 {
		rec.RenewedAtMono = l.cfg.nowMono()
	}
	if rec.RenewedAtWall == 0 {
		rec.RenewedAtWall = time.Now().UnixNano()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal epoch: %w", err)
	}
	tmp := l.paths.epoch + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open epoch tmp: %w", err)
	}
	if _, werr := f.Write(b); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write epoch tmp: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync epoch tmp: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close epoch tmp: %w", cerr)
	}
	if rerr := os.Rename(tmp, l.paths.epoch); rerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename epoch: %w", rerr)
	}
	return nil
}

// selfIdentity builds the caller's identity, reading its own pid_start.
func selfIdentity(project, agentID string, cfg leaseConfig) (identity, error) {
	pid := os.Getpid()
	st, ok := cfg.pidStart(pid)
	if !ok {
		return identity{}, fmt.Errorf("coordlock: cannot read own pid_start for pid %d", pid)
	}
	return identity{Pid: pid, PidStart: st, AgentID: agentID, Project: project}, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown-host"
	}
	return h
}
