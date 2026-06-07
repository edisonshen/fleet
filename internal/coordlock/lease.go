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
		// FREE-FLOCK FAST PATH: the previous holder is gone (kernel
		// released the flock on death). CAS the epoch to active; on ANY
		// CAS failure, HARD-release the flock and retry (the flock is
		// never held by a non-active owner).
		ok, err := l.casToActiveAfterFlock(f)
		if err != nil {
			_ = releaseFlock(f)
			return false, false, err
		}
		if !ok {
			// A candidate advanced the epoch between our flock-acquire and
			// our CAS (e.g. it saw the brief flock-busy + still-OLD epoch
			// and ran TakeOver). Release + retry — no engine/heartbeat.
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
		// No readable epoch but a live flock holder: a coord booting
		// (acquired flock, not yet wrote epoch) — treat as a healthy
		// in-progress holder and stand down rather than racing it.
		if errors.Is(err, os.ErrNotExist) {
			return true, false, nil
		}
		return false, false, fmt.Errorf("coordlock: read epoch: %w", err)
	}
	if l.holderHealthy(rec) {
		return true, false, nil // healthy live leader -> stand down
	}
	// Hung-but-alive holder (flock still held, renewed_at frozen > TTL).
	// Run the takeover state machine; it returns the same done/acquired
	// contract.
	return l.takeOver(rec)
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

// casToActiveAfterFlock writes {epoch=prev+1, state=active, owner=me}
// under coordinator.epoch.lock, but ONLY if the epoch file still reads
// what it read before our flock acquire (no candidate advanced it). On
// epoch advance it returns ok=false WITHOUT writing — the caller must
// release the flock and retry (release-on-CAS-fail).
func (l *Lease) casToActiveAfterFlock(_ *os.File) (bool, error) {
	return l.withEpochLock(func() (bool, error) {
		rec, err := readEpoch(l.paths.epoch)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// First-ever leader (no epoch file): start at epoch 1.
			rec = epochRecord{Epoch: 0}
		case err != nil:
			return false, err
		default:
			// An existing record. If it is already active+healthy+ours? It
			// cannot be — we hold the flock, so any prior active holder is
			// dead. But a CANDIDATE may have bumped the epoch to fencing/
			// active in our window. If state is active with a DIFFERENT
			// live owner, another candidate won the race -> CAS fail.
			if rec.State == stateActive && !rec.Owner.equal(l.self) && l.pidAlive(rec.Owner) {
				return false, nil
			}
		}
		l.epoch = rec.Epoch + 1
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

	// Phase 1: fence under epoch.lock.
	fenced, err := l.withEpochLock(func() (bool, error) {
		cur, rerr := readEpoch(l.paths.epoch)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				return false, nil // epoch vanished — bail, retry from scratch
			}
			return false, rerr
		}
		// Another candidate/heartbeat moved the epoch past what we saw.
		if cur.Epoch != rec.Epoch {
			return false, nil // stand down; outer loop re-reads
		}
		// Holder became healthy under the lock? Stand down.
		if l.holderHealthy(cur) {
			return false, nil
		}
		// Owner is always the recorded OLD holder — for a fresh active
		// record it is the live-but-hung leader; for a resumed
		// fencing/fenced record the two-phase rule preserved the original
		// OLD holder (never the dead candidate). Either way: cur.Owner.
		old = cur.Owner
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
	if kerr := l.cfg.killStub(old); kerr != nil {
		_ = l.markFencedNotAcquired(old)
		return false, false, fmt.Errorf("coordlock.TakeOver: kill stub failed for owner pid=%d: %w", old.Pid, kerr)
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
			if err != nil || !ok {
				// Either a write error or we were fenced — self-demote by
				// stopping the heartbeat. The caller's Release path (and
				// the kernel flock release on exit) finishes the demotion.
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

// Release stops the heartbeat and closes the flock fd (the kernel also
// releases the flock on process death). Idempotent. Cleanup-as-last-step:
// callers `defer lease.Release()` so it runs on every exit path.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	if l.stopHB != nil {
		close(l.stopHB)
		<-l.hbDone
		l.stopHB = nil
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
// state==active AND epoch==tok.Epoch AND owner==tok identity. False for a
// fenced candidate or a stale holder. This is the boundary check the
// central mutation APIs (PR4) use to reject a woken zombie.
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
	want := identity{Pid: t.Pid, PidStart: t.PidStart, AgentID: t.AgentID, Project: t.Project}
	return rec.Owner.equal(want)
}

// --- helpers: flock, epoch I/O, identity ---

// tryFlock opens (creating if absent) path and takes a single LOCK_NB
// exclusive flock. gotFlock=false on EWOULDBLOCK (a live holder exists).
// The fd is returned ONLY when gotFlock=true; otherwise it is closed.
func tryFlock(path string) (f *os.File, gotFlock bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", path, err)
	}
	lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lerr == nil {
		// Stamp pid for diagnostics (best-effort, not load-bearing).
		_ = f.Truncate(0)
		if _, serr := f.Seek(0, 0); serr == nil {
			_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
		}
		return f, true, nil
	}
	_ = f.Close()
	if lerr == syscall.EWOULDBLOCK { //nolint:errorlint // bare errno from syscall.Flock.
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("flock %s: %w", path, lerr)
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

// withEpochLock runs fn while holding coordinator.epoch.lock (the stable
// serializer inode, NEVER renamed). The flock is LOCK_NB: losing it means
// another candidate/heartbeat is mid-write, so we return ok=false (stand
// down) rather than blocking — the caller re-reads on the next pass.
func (l *Lease) withEpochLock(fn func() (bool, error)) (bool, error) {
	f, err := os.OpenFile(l.paths.epochLock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, fmt.Errorf("open epoch.lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	if lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lerr != nil {
		if lerr == syscall.EWOULDBLOCK { //nolint:errorlint // bare errno.
			return false, nil // serializer busy -> stand down
		}
		return false, fmt.Errorf("flock epoch.lock: %w", lerr)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
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
