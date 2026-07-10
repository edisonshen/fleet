//go:build linux || darwin

// The lease primitive depends on platform_{linux,darwin}.go for
// pid_start / monotonic-clock / boot-id reads. Gate the whole file (and
// its tests) to those two GOOS values so non-linux/darwin Unix targets
// (e.g. FreeBSD) don't compile a lease.go whose platform hooks are
// undefined.

package coordlock

// lease.go — the FLOCK-ONLY coordinator lease (DESIGN-coord-lease-flock-only).
//
// A live process holding coordinator.flock IS the coordinator — nothing to
// renew, nothing to lapse, no TTL, no heartbeat, no `starting` state, no
// generation counter. PR-2 deleted the epoch WRITE path (the machinery that
// manufactured the lapse-prone state PR-1 stopped reading):
//
//	coordinator.flock   ← THE lease. Body stamped on acquire with the owner's
//	                      identity {pid, pid_start, boot_id, agent_id, project}.
//	                      Kernel-released the instant the holder DIES.
//	coordinator.epoch        ← read-only vestige (CurrentEpoch / dormant
//	                            LeaseToken.StillOwned); deleted in PR-4.
//	coordinator.handoff.json ← the write-once handoff journal (handoff_journal.go,
//	                            Decision 7) — carries "a successor is coming"
//	                            across the release→acquire window, epoch-free.
//
//	AcquireLease ── LOCK_EX|LOCK_NB flock ──┬─ free  -> stamp body -> YOU ARE THE COORD
//	                                        └─ busy  -> a LIVE holder owns it -> stand down
//
// Every ownership reader (CurrentOwner / LiveOwner / CurrentActiveOwnerPID /
// LeaderPresent / LeaseRecordActive) is a LOCK_SH busy-probe of the flock +
// a best-effort body read for identity — the flock is the single source of
// truth. There is no takeover / STONITH-from-the-lease: a hung-but-alive
// holder is cleared by `fleet rm` (Decision 3), and a dead holder already
// freed the flock (kernel-on-death), so a successor just acquires it.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/state"
)

// stateActive is the only lease state string still read post-PR-2 — by the
// dormant LeaseToken.StillOwned (PR-3 reworks it to a flock-fd check; it has
// zero production callers today). The starting/fencing/released states, their
// write path, and the epoch generation counter are all gone (flock-only).
const stateActive = "active"

// defaultTTL is retained only for the dormant LeaseToken.StillOwned self-expiry
// clause (PR-3 replaces StillOwned's epoch read with a flock-fd check). It is
// no longer a liveness gate for any ownership reader.
const defaultTTL = 30 * time.Second

// epochRecord is the READ-ONLY vestige of the deleted fencing record. PR-2
// stopped WRITING it; only CurrentEpoch (dead exported code) and the dormant
// LeaseToken.StillOwned still read it, and PR-4 deletes the type + the
// coordinator.epoch(.lock) files entirely. Trimmed to the fields those two
// readers touch.
type epochRecord struct {
	Epoch         int64    `json:"epoch"`
	State         string   `json:"state"`
	Owner         identity `json:"owner"`
	BootID        string   `json:"boot_id"`
	RenewedAtMono int64    `json:"renewed_at_mono"`
}

// identity is the full identity tuple of a lease holder. Both pid AND pid_start
// are required for PID-reuse-safe liveness.
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

// leaseConfig holds the injectable seams so tests stay deterministic: a fake
// monotonic clock, an injectable pid-liveness probe, an injectable ppid walk
// (the skill-side ownership proof), an injectable boot id, an observability
// seam, and a fail-closed stamp fault-injection hook.
type leaseConfig struct {
	// ttl bounds the dormant LeaseToken.StillOwned self-expiry clause only.
	ttl time.Duration
	// nowMono returns monotonic ns. Production: monotonicNanos.
	nowMono func() int64
	// pidStart returns a process start time + ok. ok=false means dead/unreadable.
	pidStart func(pid int) (int64, bool)
	// ppid returns a process's parent pid + ok. Used by the skill-side ownership
	// proof (LeaseCheckByAncestor) to walk the getppid chain.
	ppid func(pid int) (int, bool)
	// boot returns the current boot id. Production: bootID.
	boot func() string
	// emit is the observability seam. When set, lease lifecycle events route
	// through it instead of fleetlog; tests inject a capturing seam. nil => the
	// production fleetlog emitter. Best-effort: never blocks or errors the path.
	emit func(evt string, data map[string]any)
	// stampErr is a TEST-ONLY fault-injection seam for the fail-closed flock
	// body stamp (D6/T13): when non-nil, stampFlockBody returns it without
	// touching the fd, so a test can prove every acquire site releases the flock
	// and fails the acquire rather than holding it body-less.
	stampErr error
}

func defaultLeaseConfig() leaseConfig {
	return leaseConfig{
		ttl:     defaultTTL,
		nowMono: monotonicNanos,
		pidStart: func(pid int) (int64, bool) {
			st, err := pidStartNanos(pid)
			if err != nil {
				return 0, false
			}
			return st, true
		},
		boot: bootID,
		ppid: ppidOf,
	}
}

// Lease is a held coordinator lease. The flock fd is held for the lease's
// lifetime; Release() (or process death) frees it.
type Lease struct {
	cfg   leaseConfig
	paths leasePaths
	self  identity
	boot  string

	flock *os.File // held coordinator.flock fd

	// epoch is retained (always 0 post-PR-2 — no CAS ever bumps it) only so the
	// dormant LeaseToken carries a field PR-3 reworks. Not a liveness gate.
	epoch int64
}

// emitEvent routes one lease lifecycle event to the observability sink. The
// cfg.emit seam (tests) wins; otherwise the production fleetlog emitter tags it
// comp=coord with the lease's project/agent. Best-effort.
func (l *Lease) emitEvent(evt, lvl string, data map[string]any) {
	if l.cfg.emit != nil {
		l.cfg.emit(evt, data)
		return
	}
	fleetlog.Log(fleetlog.CompCoord, evt, lvl, fleetlog.Fields{
		Proj:  l.self.Project,
		Agent: l.self.AgentID,
		Data:  data,
	})
}

// leasePaths resolves the lease files under the project's .locks/ dir.
type leasePaths struct {
	flock     string // coordinator.flock
	epoch     string // coordinator.epoch (read-only vestige; PR-4 deletes)
	epochLock string // coordinator.epoch.lock (unused vestige; PR-4 deletes)
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

// LeaseSupported reports whether this build supports the coordinator lease. On
// linux/darwin it is always true; other platforms compile the false stub.
func LeaseSupported() bool {
	return true
}

// AcquireLease tries to become the coordinator for project (flock-only, D1).
//
//	acquired=true,  lease!=nil  -> caller holds the flock; it IS the coordinator
//	acquired=false, lease==nil  -> a LIVE holder owns the flock; stand down
//	err!=nil                    -> something went wrong
//
// Acquiring the flock IS becoming the coordinator: there is no `starting`
// state, no Activate flip, no heartbeat. A busy flock means a live holder (the
// kernel frees it on death), so the caller stands down (a standby keeps
// polling via standbyPollUntilAcquired; a normal coord exits 0).
func AcquireLease(project, agentID string) (lease *Lease, acquired bool, err error) {
	return acquireLease(project, agentID, defaultLeaseConfig())
}

// acquireLease is the seam-injected core so tests drive a deterministic
// clock / pid probe / boot id / stamp-fault seam.
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
	l := &Lease{cfg: cfg, paths: paths, self: self, boot: cfg.boot()}
	done, acquired, err := l.tryAcquireOnce()
	if err != nil {
		return nil, false, err
	}
	if !done {
		// Flock-only never returns done=false (no CAS to lose), but keep the
		// contract explicit: treat it as a stand-down.
		return nil, false, nil
	}
	if acquired {
		l.emitEvent("lease.acquire", "info", nil)
		return l, true, nil
	}
	return nil, false, nil
}

// tryAcquireOnce runs the one flock acquire (D1 / D9f acquire-path collapse).
//
//	done=true, acquired=true  -> l holds the flock; it IS the coordinator
//	done=true, acquired=false -> a LIVE holder owns the flock; stand down
//
// The decision keys on the FLOCK ALONE — no epoch read, no takeover, no
// fencing. A busy flock (EWOULDBLOCK on LOCK_EX|LOCK_NB) means a live holder
// (the kernel frees the flock on death, so a busy flock can never be a corpse).
func (l *Lease) tryAcquireOnce() (done, acquired bool, err error) {
	f, gotFlock, err := tryFlock(l.paths.flock)
	if err != nil {
		return false, false, err
	}
	if !gotFlock {
		// Busy flock ⇒ a live holder owns it ⇒ stand down (D1). Never spawn
		// beside it; never read the epoch (D9f).
		return true, false, nil
	}
	// Got the flock ⇒ YOU ARE THE COORDINATOR. Stamp identity into the body —
	// the SOLE identity source. Fail-closed (D6): a failed stamp releases the
	// flock and fails the acquire so no reader ever sees a body-less holder.
	if serr := l.stampFlockBody(f); serr != nil {
		_ = releaseFlock(f)
		return false, false, fmt.Errorf("coordlock: stamp flock body: %w", serr)
	}
	l.flock = f
	return true, true, nil
}

// Release drops the flock fd (the kernel also releases it on process death) and
// emits lease.release. Idempotent. Cleanup-as-last-step: callers
// `defer lease.Release()` so it runs on every exit path. Under the flock-only
// lease there is no epoch record to demote — closing the fd is the release, and
// it flips ownership atomically the instant the fd is gone.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	heldFlock := l.flock != nil
	if l.flock != nil {
		_ = releaseFlock(l.flock)
		l.flock = nil
	}
	if heldFlock {
		l.emitEvent("lease.release", "info", nil)
	}
}

// Token returns the full-identity LeaseToken for the (dormant) central
// lease-gated mutation APIs (rpo.go). PR-3 reworks StillOwned to a flock-fd
// check; today these have zero production callers.
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
// DORMANT post-PR-2 (StillOwned reads the no-longer-written epoch, so it
// returns false unconditionally) — it has zero production callers; PR-3
// reworks it to hold-the-flock-fd + body-names-me. Retained so rpo.go's
// GuardWithLease/WriteCheckpoint/ReplayIntents (also dormant) still compile.
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

// StillOwned re-reads the epoch and confirms the holder is still us. DORMANT:
// PR-2 stopped writing the epoch, so this returns false unconditionally; PR-3
// reworks it. No production caller today.
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
		return false
	}
	if t.cfg.nowMono()-rec.RenewedAtMono > int64(t.cfg.ttl) {
		return false
	}
	want := identity{Pid: t.Pid, PidStart: t.PidStart, AgentID: t.AgentID, Project: t.Project}
	return rec.Owner.equal(want)
}

// --- helpers: flock, epoch read, identity ---

// flockBody is the identity stamped into coordinator.flock's body immediately
// after a successful acquire — the SOLE identity source. The ownership readers
// probe the flock with LOCK_SH and, when busy, read this body for agent_id +
// pid. The PR-1 TTL clock (`Mono`) is GONE (PR-2 deleted its only reader,
// flockHolderRecoverableTTL): a held flock is a live holder by kernel proof, so
// no timestamp drives any decision. Best-effort — flock exclusion is
// kernel-enforced; the body is only for identity.
type flockBody struct {
	Pid      int    `json:"pid"`
	PidStart int64  `json:"pid_start"`
	AgentID  string `json:"agent_id"`
	Project  string `json:"project"`
	BootID   string `json:"boot_id"`
}

// tryFlock opens (creating if absent) path and takes a single LOCK_NB exclusive
// flock. gotFlock=false on EWOULDBLOCK (a live holder exists). The fd is
// returned ONLY when gotFlock=true; otherwise it is closed.
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

// stampFlockBody writes this lease's identity into the held flock fd's body,
// immediately after acquire. FAIL-CLOSED (D6): it returns an error on any
// failure and the caller releases the flock + fails the acquire rather than
// leave a body-less held flock a concurrent reader would see as
// identity-pending forever.
//
// Write-before-truncate (identity-read hardening): write the full body at
// offset 0 FIRST, then Truncate to its length, so a concurrent identity reader
// never sees an EMPTY body on a held flock.
func (l *Lease) stampFlockBody(f *os.File) error {
	if l.cfg.stampErr != nil {
		return fmt.Errorf("stampFlockBody: %w", l.cfg.stampErr) // test-only fault injection
	}
	b, err := json.Marshal(flockBody{
		Pid: l.self.Pid, PidStart: l.self.PidStart,
		AgentID: l.self.AgentID, Project: l.self.Project,
		BootID: l.boot,
	})
	if err != nil {
		return fmt.Errorf("stampFlockBody: marshal: %w", err)
	}
	if _, e := f.Seek(0, 0); e != nil {
		return fmt.Errorf("stampFlockBody: seek: %w", e)
	}
	n, e := f.Write(b)
	if e != nil {
		return fmt.Errorf("stampFlockBody: write: %w", e)
	}
	if e := f.Truncate(int64(n)); e != nil {
		return fmt.Errorf("stampFlockBody: truncate: %w", e)
	}
	if e := f.Sync(); e != nil {
		return fmt.Errorf("stampFlockBody: sync: %w", e)
	}
	return nil
}

func releaseFlock(f *os.File) error {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

// readEpoch reads + unmarshals coordinator.epoch. Retained read-only for
// CurrentEpoch + the dormant LeaseToken.StillOwned (PR-4 deletes it). Returns
// os.ErrNotExist (unwrapped via errors.Is) when the file is absent.
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

// selfIdentity builds the caller's identity, reading its own pid_start.
func selfIdentity(project, agentID string, cfg leaseConfig) (identity, error) {
	pid := os.Getpid()
	st, ok := cfg.pidStart(pid)
	if !ok {
		return identity{}, fmt.Errorf("coordlock: cannot read own pid_start for pid %d", pid)
	}
	return identity{Pid: pid, PidStart: st, AgentID: agentID, Project: project}, nil
}

// flockBodyOwner reports the current owner of project's coordinator.flock by
// probing it. See flockBodyOwnerWithCfg.
func flockBodyOwner(project string) (Owner, bool) {
	return flockBodyOwnerWithCfg(project, defaultLeaseConfig())
}

// flockBodyOwnerWithCfg is the single flock-body ownership reader — the read
// truth for every ownership reader in the flock-only lease (D1). It probes
// coordinator.flock with a SHARED non-blocking lock (LOCK_SH|LOCK_NB):
//
//   - EWOULDBLOCK ⇒ a live holder's LOCK_EX conflicts ⇒ owner=true. Kernel-
//     proven liveness (the kernel frees the flock on death; fleet's fd is
//     O_CLOEXEC so exec'd children can't keep it held). Identity comes from the
//     body's agent_id + pid; a torn / empty / no-pid body is STILL owner=true
//     with AgentID="" — never downgraded (busy is the truth; the body is only
//     for identity).
//   - acquired (no LOCK_EX holder) ⇒ owner=false; release the shared probe at
//     once. Correctly reads a released-but-alive holder (graceful Release()
//     drops LOCK_EX though the process lives) AND a dead holder as free.
//   - ENOENT / unreadable ⇒ owner=false (free).
//
// PR-2 (D9c) DELETED the PR-1 epoch-identity bridge: identity now comes ONLY
// from the flock body's own agent_id (which every PR-1+ coord stamps). An
// old-binary straggler holding an identity-less body reads owner=true /
// AgentID="" (identity-pending); `fleet attach` recovers it via the agent
// record (FindLiveCoord, D9e), never a spawn-beside.
func flockBodyOwnerWithCfg(project string, _ leaseConfig) (Owner, bool) {
	paths, err := resolvePaths(project)
	if err != nil {
		return Owner{}, false
	}
	f, err := os.OpenFile(paths.flock, os.O_RDONLY, 0)
	if err != nil {
		return Owner{}, false // ENOENT (never created) or unreadable -> free
	}
	defer func() { _ = f.Close() }()
	lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if lerr == nil {
		// Acquired the shared lock ⇒ no exclusive holder ⇒ free. Release at once
		// so a real LOCK_EX acquire is never delayed by a lingering reader.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return Owner{}, false
	}
	if lerr != syscall.EWOULDBLOCK { //nolint:errorlint // bare errno from syscall.Flock.
		// An unexpected flock errno reads as "free". For a spawn gate that is the
		// fail-OPEN direction, but it cannot produce a duplicate coordinator: the
		// real acquire (LOCK_EX|LOCK_NB) hits the same errno and fails-closed too,
		// so a spuriously-triggered spawn stands down at the kernel lock.
		return Owner{}, false
	}
	// Busy: a live LOCK_EX holder exists ⇒ owner=true regardless of the body.
	// Read the body best-effort for identity (agent_id + pid tuple).
	owner := Owner{}
	if b, rerr := os.ReadFile(paths.flock); rerr == nil && len(b) > 0 {
		var body flockBody
		if json.Unmarshal(b, &body) == nil {
			owner = Owner{
				AgentID:       body.AgentID,
				PID:           body.Pid,
				PidStart:      body.PidStart,
				EngineStamped: body.AgentID != "" && body.PidStart > 0,
			}
		}
	}
	return owner, true
}

// CurrentActiveOwnerPID reports the supervisor PID of the current LIVE flock
// holder (busy⇒owner), or (0,false) when the flock is free. Repointed onto the
// flock in PR-2 (D9a): the kill gate's clause-4 "never shoot the current owner"
// and gc's dead/live split now read the flock, not the no-longer-written active
// epoch. busy⇒owner is CORRECT for these consumers — never reap/shoot a live
// flock holder. Read-only; a torn body degrades to (pid=0 -> false), which
// makes the kill primitive REFUSE rather than fire on stale data.
func CurrentActiveOwnerPID(project string) (pid int, ok bool) {
	owner, present := flockBodyOwner(project)
	if !present || owner.PID <= 0 {
		return 0, false
	}
	return owner.PID, true
}

// CurrentOwner reports the full active lease owner tuple for project, or
// ok=false if there is no readable owner WITH an identity. It is the DELIVERY
// reader: a busy flock whose body is identity-less (old-binary / torn) is
// owner-present-but-identity-PENDING → ok=false, so delivery keeps polling for
// the identity rather than reading "no owner" or typing into a corpse. A free
// flock → ok=false.
func CurrentOwner(project string) (Owner, bool) {
	return currentOwnerWithCfg(project, defaultLeaseConfig())
}

func currentOwnerWithCfg(project string, cfg leaseConfig) (Owner, bool) {
	owner, present := flockBodyOwnerWithCfg(project, cfg)
	if !present || owner.AgentID == "" || owner.PID <= 0 {
		return Owner{}, false // free, or identity-pending -> keep polling
	}
	return owner, true
}

// LiveOwner reports the coordinator lease owner whenever the flock is HELD by a
// live process (D1 busy⇒owner). It is the resolver's (internal/coordreconcile)
// attach/spawn gate: a live flock holder — busy, booting, or version-skewed —
// is STILL the coord and MUST NOT be spawned beside. It does NOT gate on
// agent_id, so a busy flock with an identity-less body is still an owner
// (ok=true, Owner.AgentID==""); Resolve distinguishes AgentID present ⇒ Attach
// from identity-less ⇒ recover-via-record. ok=false only when the flock is FREE.
func LiveOwner(project string) (Owner, bool) {
	return liveOwnerWithCfg(project, defaultLeaseConfig())
}

func liveOwnerWithCfg(project string, cfg leaseConfig) (Owner, bool) {
	return flockBodyOwnerWithCfg(project, cfg)
}

// CurrentEpoch returns the project's current on-disk epoch. DEAD exported code
// post-PR-2 (nothing writes the epoch, so it reports the last stale value or
// ok=false); retained until PR-4 deletes the epoch entirely. Read-only.
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

// LeaseRecordActive reports whether a live lease generation exists for project.
// PR-2 (D9f) COLLAPSED it to a pure flock-busy read — the epoch clause (which
// could stay true forever on a leftover starting/fencing record once the write
// path stopped cleaning it) is gone. Delivery (DeliverToCurrentOwner) uses this
// to keep a handoff doc PENDING for a booting owner vs direct-send to a bare
// coord: a held flock (booting or healthy) ⇒ true; a free flock ⇒ false
// (legacy/bare coord that never held the flock).
func LeaseRecordActive(project string) bool {
	_, present := flockBodyOwner(project)
	return present
}

// LeaderPresent reports whether a coordinator lease is currently held by a live
// process for project — i.e. whether a duplicate spawn should stand down.
// Repointed onto the flock in PR-2 (D9a): busy⇒present, free⇒absent. Its
// active-health consumers (drain, spawn) are reworked so busy⇒owner does not
// reintroduce the R4/B queue-delete hazard (drain gates on positive successor
// identity, D9b; spawn correctly stands down on any busy holder, D9f). The PR-1
// TTL-retaining epoch-missing fallback (flockHolderRecoverableTTL) is deleted.
// Read-only.
func LeaderPresent(project string) bool {
	return leaderPresentWithCfg(project, defaultLeaseConfig())
}

func leaderPresentWithCfg(project string, cfg leaseConfig) bool {
	_, present := flockBodyOwnerWithCfg(project, cfg)
	return present
}

// PidStartNanos exports the platform pid-start reader so the authenticated kill
// primitive (internal/coord.KillCoordIfIdentityMatches) and the coord-run
// supervisor can stamp + re-validate a supervisor identity using the SAME
// PID-reuse-safe start-time source the lease uses internally. ok=false means
// the pid is dead/unreadable.
func PidStartNanos(pid int) (start int64, ok bool) {
	st, err := pidStartNanos(pid)
	if err != nil {
		return 0, false
	}
	return st, true
}
