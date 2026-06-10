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
	"strings"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// FailoverEnvVar gates the whole lease path. As of PR4 the default is ON:
// the lease/STONITH/RPO machinery is live unless FLEET_LEASE_FAILOVER is
// EXPLICITLY disabled (=0/false/off/no). PR1-PR3 kept it OFF-by-default
// while the fencing + RPO coverage was incomplete; PR4 completes that
// coverage and flips the default so the headline failover is supported.
const FailoverEnvVar = "FLEET_LEASE_FAILOVER"

// ErrFailoverDisabled is returned by lease entry points when
// FLEET_LEASE_FAILOVER is EXPLICITLY disabled (=0/false/off/no). With the
// PR4 flip this is now the reversibility escape hatch ("spawn exactly as
// pre-lease"), not the default. Surface-don't-silo: callers get a typed
// refusal so they take the legacy bare-child path rather than silently
// no-op the lease.
var ErrFailoverDisabled = errors.New(
	"coordlock: lease failover is explicitly disabled (FLEET_LEASE_FAILOVER=0); " +
		"running the legacy bare-child path")

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
	// ppid returns a process's parent pid + ok. Production: ppidOf. Used by
	// the skill-side ownership proof (LeaseCheckByAncestor) to walk the
	// getppid chain. nil in defaultLeaseConfig falls back to the real
	// ppidOf at the call site; tests inject a fake tree.
	ppid func(pid int) (int, bool)
	// boot returns the current boot id. Production: bootID.
	boot func() string
	// killStub fences-then-kills the old holder. PR1 shipped a no-op
	// default (the kernel-flock release is what the takeover waits on);
	// PR2 injects the authenticated KillCoordIfIdentityMatches via
	// AcquireLeaseWithKill. fencerEpoch is the epoch the candidate fenced
	// TO (old+1) so the kill can epoch-gate the STONITH. The no-op
	// default ignores it.
	killStub func(owner identity, fencerEpoch int64) error
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
		ppid: ppidOf,
		// Default no-op kill: correct when no authenticated kill is
		// injected because TakeOver's flock acquire is kernel-gated on the
		// old holder actually dying. PR2's AcquireLeaseWithKill swaps in
		// the real KillCoordIfIdentityMatches. Tests override this to
		// simulate the kill.
		killStub: func(identity, int64) error { return nil },
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

// FailoverEnabled reports whether the lease/STONITH/RPO failover path is
// selected. As of PR4 the default is ON: it is enabled unless
// FLEET_LEASE_FAILOVER is EXPLICITLY one of the disable tokens
// (0/false/off/no, case-insensitive). Unset/empty/any other value -> ON.
//
// This is the SINGLE source of truth for the flag's tri-state semantics —
// the cmd/fleet + internal/spawn mirrors call this exported helper so the
// "default ON, =0 still off, reversible" contract can never drift across
// the four former copies (codex: drift between flag parsers reopens the
// half-fenced window the flip is supposed to close).
func FailoverEnabled() bool {
	return parseFailover(os.Getenv(FailoverEnvVar))
}

// parseFailover is the pure tri-state parse: explicit disable token -> OFF;
// everything else (including unset/empty) -> ON. Kept separate so a test
// can drive it without touching the environment.
func parseFailover(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// failoverEnabled is the unexported alias the lease internals use.
func failoverEnabled() bool { return FailoverEnabled() }

// AcquireLease tries to become the leader for project. It is the
// production entry point and refuses unless FLEET_LEASE_FAILOVER is on.
//
//	acquired=true,  lease!=nil  -> caller is the leader (start heartbeat)
//	acquired=false, lease==nil  -> a healthy live leader exists; stand down
//	err!=nil                    -> something went wrong (or failover off)
//
// AcquireLease uses the PR1 no-op kill stub for the takeover STONITH
// step. PR2's coord-run supervisor calls AcquireLeaseWithKill instead so
// a hung old holder is actually reaped via the authenticated
// internal/coord.KillCoordIfIdentityMatches primitive. AcquireLease is
// retained for callers (and tests) that only need the lock semantics.
func AcquireLease(project, agentID string) (lease *Lease, acquired bool, err error) {
	return AcquireLeaseWithKill(project, agentID, nil)
}

// KillTarget is the exported identity the takeover STONITH hands to the
// injected kill callback. coordlock cannot import internal/coord (that
// package imports coordlock — a cycle), so the authenticated kill
// primitive is injected here as a callback rather than called directly.
// The callback re-validates every field against the live agent record
// immediately before signaling (PID-reuse + exe-path + epoch gate) — the
// fields below are the takeover's view, not a license to kill blindly.
type KillTarget struct {
	// Pid + PidStart identify the OLD coord-run supervisor process the
	// takeover fenced. The kill callback MUST confirm the live process at
	// Pid still has start-time PidStart before signaling (PID reuse).
	Pid      int
	PidStart int64
	// AgentID + Project scope the target to one project's coord. The kill
	// callback refuses if the live agent record's project != Project.
	AgentID string
	Project string
	// FencerEpoch is the epoch the takeover candidate just fenced TO
	// (old+1). The kill callback signals only if the target's recorded
	// epoch is absent or < FencerEpoch — an epoch-gated STONITH so a
	// stale candidate can never shoot a newer leader.
	FencerEpoch int64
}

// AcquireLeaseWithKill is AcquireLease plus an injected authenticated
// kill callback for the takeover STONITH step. kill==nil falls back to
// the PR1 no-op stub (lock-only semantics; the takeover then relies on
// the old holder dying on its own + the kernel flock release, which is
// the correct behavior when no real kill primitive is wired). Refuses
// unless FLEET_LEASE_FAILOVER is on.
//
// The supervisor (cmd/fleet/coord.go) passes a closure that calls
// internal/coord.KillCoordIfIdentityMatches — the single shared
// authenticated coord-kill primitive (DESIGN §B.5). Threading it as a
// callback (not a hard import) keeps the lease primitive free of any
// dependency on the coord package.
func AcquireLeaseWithKill(project, agentID string, kill func(KillTarget) error) (*Lease, bool, error) {
	if !failoverEnabled() {
		return nil, false, ErrFailoverDisabled
	}
	cfg := defaultLeaseConfig()
	if kill != nil {
		// Adapt the exported KillTarget callback into the internal
		// killStub seam. fencerEpoch is l.epoch — the epoch the takeover
		// fenced TO (old+1) — so the kill primitive can epoch-gate the
		// STONITH (signal only if the target's epoch is < ours).
		cfg.killStub = func(target identity, fencerEpoch int64) error {
			return kill(KillTarget{
				Pid:         target.Pid,
				PidStart:    target.PidStart,
				AgentID:     target.AgentID,
				Project:     target.Project,
				FencerEpoch: fencerEpoch,
			})
		}
	}
	return acquireLease(project, agentID, cfg)
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
	// CLEAN RELEASE in progress (codex PR2 iter-12 [P2]): the epoch reads
	// `released` but the flock is STILL busy — the old supervisor demoted
	// the record in Release() and is dropping the flock + running its
	// coord.Cleanup (archive, tmux kill) right now. Do NOT takeOver/STONITH
	// it: SIGTERMing a supervisor mid-cleanup would interrupt the exact
	// archive/reap the design guarantees. Instead BOUNDED-RETRY the flock
	// (the fd drops in milliseconds when Release() closes it; the kernel
	// also frees it if the releaser then crashes). On success take the
	// normal free-flock fast path (CAS to active conditioned on the
	// released epoch). On budget exhaustion, fall through to takeOver — a
	// `released` record whose holder never drops the flock is itself a hung
	// holder.
	if rec.State == stateReleased {
		f2, got2, ferr := l.retryFlock()
		if ferr != nil {
			return false, false, ferr
		}
		if got2 {
			l.stampFlockBody(f2)
			ok, cerr := l.casToActiveAfterFlock(rec.Epoch)
			if cerr != nil {
				_ = releaseFlock(f2)
				return false, false, cerr
			}
			if !ok {
				_ = releaseFlock(f2)
				return false, false, nil // epoch moved -> retry
			}
			l.flock = f2
			return true, true, nil
		}
		// Releaser still holds the flock past the budget -> genuinely hung;
		// fall through to takeOver below.
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
	// Our OWN in-progress fencing record is always resumable by us — this
	// is the retry after a serializer-busy release in casFencingToActive
	// (we released the flock + are retrying our own takeover). Without this
	// a candidate would stand down on its own fresh record (live candidate
	// = self) and leave NO active leader until TTL.
	if rec.Candidate.equal(l.self) {
		return true
	}
	if rec.BootID != l.boot {
		return true // previous boot -> stale
	}
	if l.cfg.nowMono()-rec.RenewedAtMono > int64(l.cfg.ttl) {
		return true // stalled past TTL
	}
	// Fresh record from ANOTHER candidate: resumable only if it is dead.
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
		// l.epoch is the epoch we just fenced TO (old+1) — pass it as the
		// fencer epoch so the injected kill can epoch-gate the STONITH.
		if kerr := l.cfg.killStub(target, l.epoch); kerr != nil {
			_ = l.markFencedNotAcquired(old)
			return false, false, fmt.Errorf("coordlock.TakeOver: kill failed for pid=%d: %w", target.Pid, kerr)
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
	// still exactly ours — never stomp a successor's record. This is what
	// makes outstanding tokens invalid IMMEDIATELY (state != active).
	//
	// The demote is RETRIED across transient epoch.lock contention because
	// leaving the record `active` after we drop the flock would keep a
	// racing goroutine's token valid until TTL — the exact guarantee we
	// promise. Epoch.lock writers hold it only for one fast write, so a
	// bounded retry reliably wins. If it ultimately cannot demote, SURFACE
	// it (surface-don't-silo) rather than silently leaving a stale-active
	// record; the TTL is then the fallback bound.
	if l.flock != nil {
		// guaranteed is true ONLY when no stale-active record of OURS
		// remains — either we demoted it, or it was already not ours (a
		// successor took over). A real read/write fault or exhausted retries
		// leave the guarantee UNMET and must surface (surface-don't-silo).
		guaranteed := false
		var lastErr error
	demoteLoop:
		for attempt := 0; attempt < 5; attempt++ {
			ok, err := l.withEpochLock(func() (bool, error) {
				cur, rerr := readEpoch(l.paths.epoch)
				if rerr != nil {
					return false, rerr
				}
				if cur.State != stateActive || cur.Epoch != l.epoch || !cur.Owner.equal(l.self) {
					return false, nil // not ours anymore -> nothing to demote
				}
				cur.State = stateReleased
				return true, l.writeEpochLocked(cur)
			})
			switch {
			case errors.Is(err, errSerializerBusy):
				lastErr = err
				continue // transient -> retry
			case err != nil:
				// A real read/write fault (perms, disk, corrupt epoch). The
				// record MAY still be active -> guarantee unmet; stop
				// retrying (a fault won't fix itself) and surface below.
				lastErr = err
				break demoteLoop
			default:
				// ok==true: we demoted it. ok==false: already not ours.
				// Either way no stale-active record of OURS remains.
				guaranteed = true
				_ = ok
				break demoteLoop
			}
		}
		if !guaranteed {
			fmt.Fprintf(os.Stderr,
				"coordlock: WARNING: could not demote released lease for project %q "+
					"(%v); outstanding tokens may stay valid until TTL (~%s). "+
					"agent=%s epoch=%d\n",
				l.self.Project, lastErr, l.cfg.ttl, l.self.AgentID, l.epoch)
		}
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

// CurrentActiveOwnerPID reports the supervisor PID recorded as the
// current ACTIVE lease owner for project, or (0,false) if there is no
// active owner (no epoch file, or its state != active). The
// authenticated kill primitive (internal/coord) uses it as the epoch
// gate's executable form: a STONITH target is refused if it IS the
// current active owner (never shoot the live leader); any other coord
// for this project is stale by construction once we hold the lease.
// Best-effort + read-only — it takes no lock (a torn read degrades to
// (0,false), i.e. "no active owner", which makes the kill primitive
// refuse rather than fire on stale data).
func CurrentActiveOwnerPID(project string) (pid int, ok bool) {
	paths, err := resolvePaths(project)
	if err != nil {
		return 0, false
	}
	rec, err := readEpoch(paths.epoch)
	if err != nil {
		return 0, false
	}
	if rec.State != stateActive || rec.Owner.Pid <= 0 {
		return 0, false
	}
	return rec.Owner.Pid, true
}

// CurrentOwner reports the full active lease owner tuple for project, or
// ok=false if there is no readable ACTIVE owner. It is the delivery-side
// companion to CurrentActiveOwnerPID: callers that need to type a handoff
// resume prompt must address the agent record named by the lease owner, not a
// preselected standby that may have lost the lock race.
func CurrentOwner(project string) (Owner, bool) {
	paths, err := resolvePaths(project)
	if err != nil {
		return Owner{}, false
	}
	rec, err := readEpoch(paths.epoch)
	if err != nil {
		return Owner{}, false
	}
	if rec.State != stateActive || rec.Owner.Pid <= 0 || rec.Owner.AgentID == "" {
		return Owner{}, false
	}
	return Owner{
		AgentID:       rec.Owner.AgentID,
		PID:           rec.Owner.Pid,
		PidStart:      rec.Owner.PidStart,
		EngineStamped: rec.Owner.AgentID != "" && rec.Owner.PidStart > 0,
	}, true
}

// CurrentEpoch returns the project's current on-disk fencing epoch (the
// monotonic token) and ok=false if no epoch record exists / is unreadable.
// It is the value the graceful handoff stamps into its
// handoff-complete-<epoch>.json barrier (PR3) so the drain verifier can
// confirm the barrier belongs to the lease generation it is about to reap.
// Read-only, takes no lock; a torn read degrades to (0, false).
func CurrentEpoch(project string) (epoch int64, ok bool) {
	paths, err := resolvePaths(project)
	if err != nil {
		return 0, false
	}
	rec, err := readEpoch(paths.epoch)
	if err != nil {
		return 0, false
	}
	return rec.Epoch, true
}

// BarrierPath returns the absolute path of the graceful-handoff completion
// barrier for (project, epoch): <project>/.locks/handoff-complete-<epoch>.json.
// The OLD coord writes it (atomic .tmp->fsync->rename) ONLY after the
// handoff doc + checkpoint are fsynced; the drain verifier waits for it
// before a GRACEFUL kill (the safety-net kill may fire pre-barrier). Both
// producers resolve the path through this one helper so they never drift.
func BarrierPath(project string, epoch int64) (string, error) {
	paths, err := resolvePaths(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(paths.flock),
		fmt.Sprintf("handoff-complete-%d.json", epoch)), nil
}

// LeaderPresent reports whether a coordinator lease is currently held by
// a HEALTHY leader OR is mid a FRESH in-progress takeover for project —
// i.e. whether a duplicate spawn should stand down. It applies the SAME
// predicate AcquireLease uses (codex PR2 iter-11 [P2]), not a bare
// state==active check:
//   - state==active AND same boot AND within TTL AND owner pid+pid_start
//     alive  -> healthy leader present, true;
//   - state==fencing AND the candidate is fresh (live, in-budget) -> a
//     legitimate takeover is in progress, true (the new leader is coming);
//   - otherwise (no record, stale/expired active, dead owner, abandoned
//     fencing, fenced_not_acquired, released) -> false (stealable / no
//     leader).
//
// Read-only, takes no lock; a torn/missing read degrades to false. Used by
// cmd/fleet's coordLeaderCheck to disambiguate a clean stand-down from a
// real supervisor failure.
func LeaderPresent(project string) bool {
	return leaderPresentWithCfg(project, defaultLeaseConfig())
}

// leaderPresentWithCfg is the seam-injected core of LeaderPresent so
// tests drive a deterministic clock / pid-liveness / boot id.
func leaderPresentWithCfg(project string, cfg leaseConfig) bool {
	paths, err := resolvePaths(project)
	if err != nil {
		return false
	}
	// Throwaway lease carrying the seams so holderHealthy /
	// transientResumable / flockHolderRecoverable apply the same logic the
	// acquire path does. self is zero (we only read foreign records);
	// transientResumable's "is this MY fencing record" short-circuit can
	// never fire on a zero self vs a real candidate.
	l := &Lease{cfg: cfg, paths: paths, boot: cfg.boot()}

	rec, err := readEpoch(paths.epoch)
	if err != nil {
		// MISSING epoch — mirror the acquire path's busy-flock booting check
		// (codex PR3 iter-3 [P2]). A holder can grab coordinator.flock and not
		// yet have written coordinator.epoch (it is still booting). Returning
		// false here would misclassify that fresh booting leader as "no leader"
		// — making a duplicate spawn report a failure instead of a clean
		// stand-down. So: if the flock is BUSY and its body vouches for a
		// FRESH same-boot live holder (flockHolderRecoverable == false), a
		// leader is present. A free flock, or a stale/dead/hung body, is no
		// healthy leader.
		if !errors.Is(err, os.ErrNotExist) {
			return false // unreadable epoch (not just "absent") -> conservative
		}
		f, gotFlock, ferr := tryFlock(paths.flock)
		if ferr != nil {
			return false
		}
		if gotFlock {
			// We acquired it -> nobody holds it -> no leader. Release at once.
			_ = releaseFlock(f)
			return false
		}
		// Busy flock, no epoch yet: present iff the holder is a fresh booting
		// one (not recoverable/stealable).
		return !l.flockHolderRecoverable()
	}
	switch rec.State {
	case stateActive:
		return l.holderHealthy(rec)
	case stateFencing:
		// A fresh, live, in-budget takeover (NOT resumable by a newcomer)
		// means a legitimate successor is mid-acquire -> treat as present.
		return !l.transientResumable(rec)
	default:
		// fenced_not_acquired / released / unknown -> no live leader.
		return false
	}
}

// PidStartNanos exports the platform pid-start reader so the
// authenticated kill primitive (internal/coord.KillCoordIfIdentity-
// Matches) and the coord-run supervisor can stamp + re-validate a
// supervisor identity using the SAME PID-reuse-safe start-time source the
// lease uses internally. Two calls for one live pid return the same
// value; a recycled pid returns a different value — equality is what
// defeats PID reuse. The unit is platform-relative ns-since-boot
// (Linux) / Unix ns (darwin); only ever compared for equality on the
// same boot, never interpreted as an absolute time. ok=false means the
// pid is dead/unreadable.
func PidStartNanos(pid int) (start int64, ok bool) {
	st, err := pidStartNanos(pid)
	if err != nil {
		return 0, false
	}
	return st, true
}
