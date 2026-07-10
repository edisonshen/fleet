//go:build linux || darwin

// The lease primitive depends on platform_{linux,darwin}.go for
// pid_start / monotonic-clock / boot-id reads. Gate the whole file (and
// its tests) to those two GOOS values so non-linux/darwin Unix targets
// (e.g. FreeBSD) don't compile a lease.go whose platform hooks are
// undefined. Adding more platforms is a later PR if ever needed.

package coordlock

// lease.go — the three-file coordinator lease primitive (PR1 of the
// DESIGN-handoff-drain-storm-leak stack). PRIMITIVE ONLY: nothing here is
// wired into the live coord/handoff/drain paths (that is PR2-PR6).
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

	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/state"
)

// Lease state machine. `active` is the only state in which a holder may
// serve / heartbeat / hold the flock as owner. The two transient states
// belong to an in-progress takeover (see the two-phase rule).
const (
	stateStarting          = "starting"            // holder acquired the flock but its /coordinator loop is not up yet
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
	// defaultStartingTTL bounds how long a lease may sit in `starting`
	// (acquired the flock, /coordinator not yet up + flipped to active)
	// before an attach/spawn resolver may SUPERSEDE the stale record and
	// spawn fresh (D2 `starting-wedged` backstop). It is generously longer
	// than worst-case coord boot (10-30s to type the skill + first tick) so
	// a slow-but-healthy boot is never superseded. This is NOT a kill
	// timeout: supersession is a pure record CAS (see SupersedeStartingLease)
	// that touches no process — the wedged session lingers as a visible
	// zombie for `fleet gc`'s dead/live split (no-auto-kill invariant).
	defaultStartingTTL = 120 * time.Second
	// defaultHandoffTTL bounds how long a `handoff{successor,expires}` record
	// (written by an OLD coord as it releases the lease, D3) reserves the
	// freed lease for the named successor. A contender that sees a valid
	// handoff record within TTL waits for the successor; past TTL the record
	// is stale (the successor died mid-boot, #247) and the lease is free.
	defaultHandoffTTL = 90 * time.Second
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
	// Handoff, when non-nil, records that the owner is releasing the lease
	// to a named successor (D3: the lease epoch — not the deleted
	// coord-spawn marker — is the handoff identity-commit point). A
	// contender that observes a free lease AND a non-expired Handoff record
	// waits for SuccessorID until ExpiresAtMono; past the deadline the
	// record is stale (successor died mid-boot) and the lease is free.
	// Same-boot-only: cross-boot (BootID mismatch) reads it as expired.
	Handoff *handoffInfo `json:"handoff,omitempty"`
}

// handoffInfo is the successor reservation an OLD coord stamps into the
// epoch record as it releases the lease during a graceful handoff (D3).
// It replaces the four roles of the deleted coord-spawn marker's
// handoff-commit path: the identity commit is now the lease epoch bump the
// winning successor performs, and this record only reserves the freed lease
// for the named successor for a bounded window.
type handoffInfo struct {
	SuccessorID   string `json:"successor_id"`
	ExpiresAtMono int64  `json:"expires_at_mono"`
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
	// startingTTL bounds the `starting` state before the resolver may
	// supersede a wedged never-flipped owner (D2). handoffTTL bounds a
	// successor reservation (D3). Both default from the package consts;
	// tests inject short values to drive the TTL boundaries deterministically.
	startingTTL time.Duration
	handoffTTL  time.Duration
	// startAsStarting makes a fresh acquire write state=`starting` instead of
	// `active` (D2 two-phase startup). Set true ONLY on the real-coordinator
	// acquire (AcquireLeaseWithKill); the takeover-only path and the internal
	// test entry keep it false (acquire straight to active).
	startAsStarting bool

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
	// reacquire gates the own-expired-no-rival IN-PLACE RENEWAL in
	// leaseCheckByAncestorWithCfg (codex iter-2 [P1]). False (the default)
	// keeps the check read-only — only LeaseCheckByAncestorReacquire (the
	// coordinator tick's `lease-check --reacquire`) sets it. A check must
	// never renew the lease as a side effect for non-tick callers.
	reacquire bool
	// boot returns the current boot id. Production: bootID.
	boot func() string
	// killStub fences-then-kills the old holder. PR1 shipped a no-op
	// default (the kernel-flock release is what the takeover waits on);
	// PR2 injects the authenticated KillCoordIfIdentityMatches via
	// AcquireLeaseWithKill. fencerEpoch is the epoch the candidate fenced
	// TO (old+1) so the kill can epoch-gate the STONITH. The no-op
	// default ignores it.
	killStub func(owner identity, fencerEpoch int64) error

	// emit is the observability seam (DESIGN-coord-lease-false-fence-
	// prevention piece 2). When set, the lease routes its lifecycle events
	// (lease.acquire / lease.renew / lease.renew.fail / lease.release)
	// through it instead of the default fleetlog sink; tests inject a
	// capturing/lock-checking seam. nil (the default) => the production
	// fleetlog emitter (emitEvent). Best-effort: an emit MUST never block or
	// error the lease path, and MUST run OUTSIDE the short coordinator.epoch.
	// lock critical section (the PR #241 wedge class).
	emit func(evt string, data map[string]any)

	// logLifecycle gates the ownership-transition events (lease.acquire /
	// lease.release). Only a REAL coordinator lease should log them; the
	// drain safety-net takeover (AcquireLeaseTakeover) acquires just long
	// enough to fence+kill a hung holder then releases WITHOUT ever running a
	// coordinator, so emitting acquire/release there would show a phantom
	// coordinator lifecycle during drain recovery — exactly the window
	// operators inspect (codex P2). defaultLeaseConfig sets it true; the
	// takeover-only path sets it false. renew/renew.fail are unaffected: they
	// only fire from the heartbeat loop, which a takeover-only caller never
	// starts.
	logLifecycle bool

	// stampErr is a TEST-ONLY fault-injection seam for the fail-closed
	// flock-body stamp (D3): when non-nil, stampFlockBody returns it without
	// touching the fd, so a test can prove every acquire call site releases the
	// flock and fails the acquire rather than holding it body-less (T4).
	// defaultLeaseConfig leaves it nil (no injection).
	stampErr error
}

func defaultLeaseConfig() leaseConfig {
	return leaseConfig{
		heartbeat:        defaultHeartbeat,
		ttl:              defaultTTL,
		flockRetryBudget: defaultFlockRetryBudget,
		startingTTL:      defaultStartingTTL,
		handoffTTL:       defaultHandoffTTL,
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
		// A real coordinator lease logs its ownership transitions by default;
		// the takeover-only path overrides this to false (codex P2).
		logLifecycle: true,
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

	// liveAbort carries the KP6 pre-fence gate's detection out of takeOver
	// (DESIGN-coord-no-auto-kill): the prospective kill targets found ALIVE
	// when a staleness-triggered takeover stood down instead of fencing.
	// Set only on the abort path; read by acquireLeaseDetect after the
	// convergence loop returns not-acquired.
	liveAbort []identity

	stopHB chan struct{} // closed by Release to stop the heartbeat goroutine
	hbDone chan struct{} // closed by the heartbeat goroutine on exit

	// renewSampler throttles lease.renew logging so a healthy 10s heartbeat
	// does not emit 6 lines/min; state lives on the lease across beats.
	renewSampler renewSampler
}

// renewSampleInterval bounds lease.renew logging: a healthy renew fires every
// heartbeat (10s), but logging each is noise, so we log at most one
// lease.renew per minute. Failures (lease.renew.fail) are never sampled.
const renewSampleInterval = time.Minute

// renewSampler decides whether a successful renew should be logged. The first
// success always logs; each later success logs only after
// renewSampleInterval has elapsed on the injected monotonic clock. Pure +
// stateful so it is driven directly by a test rather than through the live
// heartbeat goroutine.
type renewSampler struct {
	started      bool
	lastEmitMono int64
}

func (s *renewSampler) shouldEmit(nowMono int64, interval time.Duration) bool {
	if !s.started || nowMono-s.lastEmitMono >= int64(interval) {
		s.started = true
		s.lastEmitMono = nowMono
		return true
	}
	return false
}

// emitEvent routes one lease lifecycle event to the observability sink. The
// cfg.emit seam (tests) wins; otherwise the production fleetlog emitter tags
// it comp=coord with the lease's project/agent for correlation. Best-effort:
// a nil seam + a fleetlog write fault are both swallowed, never blocking the
// lease path. Callers MUST invoke this OUTSIDE coordinator.epoch.lock.
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

// emitRenewResult logs the outcome of one heartbeat CAS, OUTSIDE the epoch
// lock (the heartbeat loop calls it after heartbeatOnce returns). A failure —
// a write fault OR a fenced/epoch-moved self-demote (ok=false) — always logs
// lease.renew.fail with the reason; a success logs lease.renew at most once
// per renewSampleInterval.
func (l *Lease) emitRenewResult(ok bool, err error) {
	if err != nil || !ok {
		reason := "fenced-or-epoch-moved"
		if err != nil {
			reason = err.Error()
		}
		l.emitEvent("lease.renew.fail", "warn", map[string]any{
			"epoch":  l.epoch,
			"reason": reason,
		})
		return
	}
	if l.renewSampler.shouldEmit(l.cfg.nowMono(), renewSampleInterval) {
		l.emitEvent("lease.renew", "info", map[string]any{"epoch": l.epoch})
	}
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

// LeaseSupported reports whether this build supports the coordinator
// lease/STONITH/RPO path. On linux/darwin the answer is always true; other
// platforms compile lease_owner_other.go's false stub because the pid-start /
// monotonic-clock primitives are not available there.
func LeaseSupported() bool {
	return true
}

// AcquireLease tries to become the leader for project. It is the
// production entry point on lease-capable platforms.
//
//	acquired=true,  lease!=nil  -> caller is the leader (start heartbeat)
//	acquired=false, lease==nil  -> a healthy live leader exists; stand down
//	err!=nil                    -> something went wrong
//
// AcquireLease uses the PR1 no-op kill stub for the takeover STONITH
// step. PR2's coord-run supervisor calls AcquireLeaseWithKill instead so
// a hung old holder is actually reaped via the authenticated
// internal/coord.KillCoordIfIdentityMatches primitive. AcquireLease is
// retained for callers (and tests) that only need the lock semantics.
func AcquireLease(project, agentID string) (lease *Lease, acquired bool, err error) {
	l, acquired, _, err := AcquireLeaseWithKill(project, agentID, nil)
	return l, acquired, err
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

// LiveHolder identifies one prospective takeover kill target the KP6
// pre-fence gate (DESIGN-coord-no-auto-kill) found ALIVE. When any
// prospective target is live, the takeover writes NOTHING and stands
// down; the detection is returned so cmd/fleet can report/quarantine
// (the standby poll loop owns dedup + emission). coordlock itself stays
// log-free.
type LiveHolder struct {
	Pid      int
	PidStart int64
	AgentID  string
	Project  string
}

// AcquireLeaseWithKill is AcquireLease plus an injected authenticated
// kill callback for the takeover STONITH step. kill==nil falls back to
// the PR1 no-op stub (lock-only semantics; the takeover then relies on
// the old holder dying on its own + the kernel flock release, which is
// the correct behavior when no real kill primitive is wired).
//
// Return shape (KP6): live is non-empty ONLY on the pre-fence stand-down
// — a staleness-triggered takeover found a prospective kill target still
// ALIVE, wrote nothing, and returned the same acquired=false / nil-error
// stand-down shape a healthy leader produces. Callers that poll
// (standbyPollUntilAcquired) keep polling; the detection is theirs to
// report. acquired=false with live==nil is the ordinary healthy-leader
// stand-down.
//
// The supervisor (cmd/fleet/coord.go) passes a closure that calls
// internal/coord.KillCoordIfIdentityMatches — the single shared
// authenticated coord-kill primitive (DESIGN §B.5). Threading it as a
// callback (not a hard import) keeps the lease primitive free of any
// dependency on the coord package.
func AcquireLeaseWithKill(project, agentID string, kill func(KillTarget) error) (*Lease, bool, []LiveHolder, error) {
	return acquireLeaseWithKill(project, agentID, kill, true /* logLifecycle */)
}

// AcquireLeaseTakeover is the drain safety-net variant of AcquireLeaseWithKill:
// it fences+kills a hung holder then the caller Releases immediately WITHOUT
// running a coordinator or heartbeat. It SUPPRESSES the lease.acquire /
// lease.release lifecycle events so fleetlog does not show a phantom
// coordinator lifecycle during drain recovery (codex P2) — the exact window
// operators inspect. Everything else (fence/kill/detect) is identical.
func AcquireLeaseTakeover(project, agentID string, kill func(KillTarget) error) (*Lease, bool, []LiveHolder, error) {
	return acquireLeaseWithKill(project, agentID, kill, false /* logLifecycle */)
}

// acquireLeaseWithKill is the shared lease-acquire core. logLifecycle=true is
// the real-coordinator path (AcquireLeaseWithKill); false is the takeover-only
// path (AcquireLeaseTakeover), which suppresses ownership-transition logs.
func acquireLeaseWithKill(project, agentID string, kill func(KillTarget) error, logLifecycle bool) (*Lease, bool, []LiveHolder, error) {
	cfg := defaultLeaseConfig()
	cfg.logLifecycle = logLifecycle
	// The real-coordinator acquire (logLifecycle=true) starts in `starting`
	// and the supervisor flips to `active` via Activate once /coordinator is
	// up (D2). The takeover-only path (logLifecycle=false) fences+releases
	// without running a coordinator, so it acquires straight to `active`.
	cfg.startAsStarting = logLifecycle
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
	return acquireLeaseDetect(project, agentID, cfg)
}

// acquireLease is the internal core with the legacy 3-value
// shape (the internal test-suite entry point). Detection is dropped —
// callers that need the KP6 live-holder result use acquireLeaseDetect.
func acquireLease(project, agentID string, cfg leaseConfig) (*Lease, bool, error) {
	l, acquired, _, err := acquireLeaseDetect(project, agentID, cfg)
	return l, acquired, err
}

// acquireLeaseDetect is the core acquisition path, parameterized by
// config so tests inject deterministic seams. See AcquireLease for the
// base contract and AcquireLeaseWithKill for the live-holder detection
// contract (KP6).
func acquireLeaseDetect(project, agentID string, cfg leaseConfig) (*Lease, bool, []LiveHolder, error) {
	if project == "" {
		return nil, false, nil, fmt.Errorf("coordlock.AcquireLease: project must not be empty")
	}
	paths, err := resolvePaths(project)
	if err != nil {
		return nil, false, nil, fmt.Errorf("coordlock.AcquireLease: resolve paths: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		return nil, false, nil, fmt.Errorf("coordlock.AcquireLease: mkdir lock dir: %w", err)
	}

	self, err := selfIdentity(project, agentID, cfg)
	if err != nil {
		return nil, false, nil, err
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
			return nil, false, nil, err
		}
		if done {
			if acquired {
				// lease.acquire: ownership transition (OUTSIDE epoch.lock —
				// the acquire CAS already released it). Epoch is the one we
				// now own. Suppressed on the takeover-only path (codex P2).
				if l.cfg.logLifecycle {
					l.emitEvent("lease.acquire", "info", map[string]any{"epoch": l.epoch})
				}
				return l, true, nil, nil
			}
			// Stand-down. liveAbort is non-nil ONLY when the KP6 pre-fence
			// gate aborted a takeover on a live prospective target (mapped
			// to this same done/!acquired shape so pollers keep polling —
			// NEVER to retry semantics, which would exhaust this loop into
			// the "did not converge" error and exit a standby's poll loop
			// via its error branch).
			return nil, false, liveHoldersFrom(l.liveAbort), nil
		}
		// Not done: a CAS lost / takeover needed and we should re-evaluate
		// from a clean flock state. Loop.
	}
	return nil, false, nil, fmt.Errorf("coordlock.AcquireLease: did not converge after %d attempts (contended)", maxAttempts)
}

// liveHoldersFrom projects the internal identity tuples into the
// exported detection shape.
func liveHoldersFrom(ids []identity) []LiveHolder {
	if len(ids) == 0 {
		return nil
	}
	out := make([]LiveHolder, 0, len(ids))
	for _, id := range ids {
		out = append(out, LiveHolder(id)) // identical fields modulo json tags
	}
	return out
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
		// Stamp identity into the flock body — the SOLE identity source under
		// the flock-only lease. Fail-closed (D6): a failed stamp releases the
		// flock and fails the acquire so no reader ever sees a body-less holder.
		if serr := l.stampFlockBody(f); serr != nil {
			_ = releaseFlock(f)
			return false, false, fmt.Errorf("coordlock: stamp flock body: %w", serr)
		}
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
	// STARTING holder (D2): a coord acquired the flock and is booting its
	// /coordinator loop (has not flipped to `active` yet). If its supervisor
	// pid is ALIVE, STAND DOWN — never takeOver — even past startingTTL. A
	// live starting holder is either healthy-booting or wedged, and the
	// no-auto-kill invariant forbids fencing/killing a LIVE process on a
	// staleness heuristic. The wedged case is handled at the RECORD level by
	// the attach/spawn resolver (SupersedeStartingLease bumps the epoch so
	// the wedged Activate fails closed) + a fresh `--standby` that polls
	// behind this held flock and wins when the zombie dies. Only a starting
	// record whose owner is DEAD falls through to recovery (its flock would
	// normally already be free; defensive).
	if rec.State == stateStarting && l.pidAlive(rec.Owner) {
		return true, false, nil
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
			if serr := l.stampFlockBody(f2); serr != nil {
				_ = releaseFlock(f2)
				return false, false, fmt.Errorf("coordlock: stamp flock body (released-retry): %w", serr)
			}
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
	// = self) and leave NO active leader until TTL. Guarded on a REAL self:
	// the lease-check probe builds its Lease with a zero self identity, and
	// a zero Candidate (malformed record) must not silently match it — the
	// zero-candidate case falls through to the pidAlive clause below, which
	// reads Pid<=0 as dead (same verdict, honest path).
	if l.self.Pid > 0 && rec.Candidate.equal(l.self) {
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

// ownExpiredRival reports whether an own-ancestor record shows a LIVE rival
// takeover on the own-lease-expired branch: state==fencing AND (the record
// is fresh/in-budget, OR its candidate pid is still alive). A live candidate
// hung past TTL is STILL a rival — transientResumable calls the record stale
// on gap>TTL alone, but a live candidate can resume and run its kill phase,
// so only a fencing record with a DEAD candidate is re-acquirable. Shared by
// the lease-check probe (BOTH branches: own-ancestor re-acquire gate and the
// non-ancestor takeover fence) and reacquireOwnExpired's CAS re-read so the
// rival predicates can never drift.
func (l *Lease) ownExpiredRival(rec epochRecord) bool {
	return rec.State == stateFencing &&
		(!l.transientResumable(rec) || l.pidAlive(rec.Candidate))
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
		return true, l.writeEpochLocked(l.freshLeaseRecord(l.epoch, l.startState(), l.self))
	})
}

// freshLeaseRecord builds the epoch record a fresh acquire/claim writes,
// stamping BootID + RenewedAt* + Host so the record reads as WITHIN-TTL from
// birth. This is load-bearing for a `starting` record (D2): unlike an
// `active` record — which the heartbeat goroutine re-stamps within a
// heartbeat interval of acquire — a `starting` record is NEVER heartbeat-
// stamped before Activate (the heartbeat starts only AFTER the flip). Without
// these fields a fresh starter carries BootID="" + RenewedAtMono=0, so
// CurrentStarting/leaseClaimable read it as past-startingTTL IMMEDIATELY, and
// a concurrent attach/spawn during a normal boot would supersede/overwrite a
// perfectly healthy starter instead of waiting (codex D2 iter-2 [P1]).
func (l *Lease) freshLeaseRecord(epoch int64, state string, owner identity) epochRecord {
	return epochRecord{
		Epoch:         epoch,
		State:         state,
		Owner:         owner,
		Host:          l.host,
		BootID:        l.boot,
		RenewedAtMono: l.cfg.nowMono(),
		RenewedAtWall: time.Now().UnixNano(),
	}
}

// startState returns the state a fresh acquire writes when it becomes the
// leader: `starting` for the real-coordinator path (D2 two-phase startup —
// the supervisor flips to `active` via Activate once its /coordinator loop
// is up), or `active` for the takeover-only path (AcquireLeaseTakeover, which
// fences+releases without ever running a coordinator) and the internal
// test-suite entry (default cfg). Gated by cfg.startAsStarting.
func (l *Lease) startState() string {
	if l.cfg.startAsStarting {
		return stateStarting
	}
	return stateActive
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

	// Reset any prior round's detection; set only by this call's gate.
	l.liveAbort = nil
	var liveDetected []identity

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
		// KP6 PRE-FENCE LIVENESS GATE (DESIGN-coord-no-auto-kill). A
		// staleness heuristic (TTL expiry) brought us here — it may fence,
		// report, and quarantine, but it may never kill OR EVEN FENCE a
		// LIVE coordinator. Probe every prospective Phase-2 kill target
		// (recorded owner, prior fencing candidate, flock-body holder)
		// BEFORE writeEpochLocked: any alive -> write NOTHING and stand
		// down (the untouched leader re-heartbeats on wake; the standby
		// keeps polling and self-heals). Placement is load-bearing: a
		// post-fence abort would leave the live leader permanently fenced
		// (5a90's live-candidate-is-rival rule blocks its re-acquire) and
		// inflate the epoch every poll round.
		if lh := l.liveProspectiveTargets(old, priorCandidate); len(lh) > 0 {
			liveDetected = lh
			return false, nil // no fencing write of any kind
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
	if len(liveDetected) > 0 {
		// KP6 abort: a prospective kill target is ALIVE. Map to the
		// STAND-DOWN shape (done=true, acquired=false, nil error) — the
		// same shape healthy-leader-present produces — so acquireLease
		// returns acquired=false/err=nil and a standby keeps polling.
		// Retry semantics here would exhaust the 8-attempt convergence
		// loop into a "did not converge" ERROR and exit the standby via
		// the poll loop's error branch. No markFencedNotAcquired: nothing
		// was fenced.
		l.liveAbort = liveDetected
		return true, false, nil
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

	// Recovery stamp before we CAS to active. Fail-closed (D6): a failed stamp
	// releases the just-acquired flock and fails the takeover.
	if serr := l.stampFlockBody(f); serr != nil {
		_ = releaseFlock(f)
		return false, false, fmt.Errorf("coordlock: stamp flock body (takeover): %w", serr)
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
		// keep our fencing epoch; we are the same authority
		return true, l.writeEpochLocked(l.freshLeaseRecord(l.epoch, l.startState(), l.self))
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
				// Not a renewal FAILURE (no fault, no fence), so no emit.
				continue
			}
			// Observability (OUTSIDE the epoch.lock — heartbeatOnce already
			// released it): log the outcome. Success is sampled <=1/min;
			// failure/fence is always logged with the reason.
			l.emitRenewResult(ok, err)
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
		// Write ONLY if still exactly ours. (No higher-epoch adoption: the
		// only same-epoch writer besides us is reacquireOwnExpired, which
		// is restricted to expired-ACTIVE records at OUR epoch — a bumped
		// epoch always means a rival fenced us; codex iter-4 [P1].)
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

// Activate performs the starting->active flip: the LAST startup step (D2).
// A coord acquires the lease as `starting` (having done zero coord work) and
// calls Activate once its /coordinator loop is up, IMMEDIATELY before its
// first tick. The flip is an epoch-preserving CAS that FAILS CLOSED — the
// same guard heartbeatOnce uses: it writes state=active ONLY if the on-disk
// record still names us at our epoch in `starting`. A superseded starter (a
// resolver bumped the epoch via SupersedeStartingLease while we were wedged,
// T6f) finds the record no longer ours -> ok=false, NO write. The caller MUST
// self-demote + exit BEFORE running any tick on ok=false; two owners can
// never coexist because the superseded starter can never flip over the
// replacement's record.
//
//	ok=true  -> we are now the active leader; start the heartbeat + first tick.
//	ok=false -> superseded (or already not starting); self-demote + exit.
//	err!=nil -> a write fault; treat as fatal (surface + exit).
func (l *Lease) Activate() (bool, error) {
	return l.withEpochLock(func() (bool, error) {
		cur, err := readEpoch(l.paths.epoch)
		if err != nil {
			return false, err
		}
		if cur.State != stateStarting || cur.Epoch != l.epoch || !cur.Owner.equal(l.self) {
			return false, nil // superseded / not ours -> fail closed, do NOT write
		}
		cur.State = stateActive
		cur.RenewedAtMono = l.cfg.nowMono()
		cur.RenewedAtWall = time.Now().UnixNano()
		cur.Host = l.host
		cur.BootID = l.boot
		return true, l.writeEpochLocked(cur)
	})
}

// SupersedeStartingLease fences a WEDGED `starting` owner (D2 backstop). If
// the on-disk record is still `starting` at observedEpoch, it bumps the epoch
// so the wedged owner's Activate flip fails closed (T6f). It is a PURE RECORD
// CAS — it signals NO process (no-auto-kill invariant): the wedged session
// lingers as a visible zombie for `fleet gc`'s dead/live split, and a fresh
// `--standby` (spawned by the resolver) wins the lease when the zombie's
// still-held flock releases on its death. The owner tuple is preserved so the
// acquire path's live-starting stand-down keeps protecting the live zombie
// until then.
//
//	ok=true  -> WE superseded the wedged starter at observedEpoch (epoch bumped).
//	ok=false -> benign race: the record flipped to active/released, or ANOTHER
//	            resolver already advanced the epoch past observedEpoch; the
//	            caller MUST re-resolve rather than assume a standby is still
//	            needed. A stale-snapshot true here would make Resolve emit
//	            SpawnStandby beside an already-running replacement (codex D2
//	            iter-3 [P2]).
func SupersedeStartingLease(project string, observedEpoch int64) (ok bool, err error) {
	return supersedeStartingWithCfg(project, observedEpoch, defaultLeaseConfig())
}

func supersedeStartingWithCfg(project string, observedEpoch int64, cfg leaseConfig) (bool, error) {
	paths, rerr := resolvePaths(project)
	if rerr != nil {
		return false, rerr
	}
	l := &Lease{cfg: cfg, paths: paths, host: hostname(), boot: cfg.boot()}
	return l.withEpochLock(func() (bool, error) {
		cur, err := readEpoch(l.paths.epoch)
		if err != nil {
			return false, err
		}
		// Only a supersede WE performed at the observed epoch returns true. A
		// record that already moved past observedEpoch (another resolver
		// bumped it, or a replacement went active/released) is NOT our
		// supersede: return false so Resolve re-resolves against the fresh
		// state instead of spawning a standby from a stale snapshot.
		if cur.State != stateStarting || cur.Epoch != observedEpoch {
			return false, nil // flipped/released/advanced/raced -> re-resolve
		}
		cur.Epoch = observedEpoch + 1
		// Keep State=starting + the same Owner so the acquire path's
		// live-starting stand-down still protects the live zombie's held
		// flock; keep the (stale) renewed_at so the record stays past-TTL.
		return true, l.writeEpochLocked(cur)
	})
}

// ReserveHandoff stamps a successor reservation into the CURRENT epoch record
// (D3 identity commit moves off the deleted coord-spawn marker onto the
// lease). An OLD coord that is handing off calls it BEFORE releasing the lease
// so a contender that observes the freed lease waits for successorID until the
// reservation expires (rule (a)) instead of spawning a duplicate. The real
// identity COMMIT is the epoch bump the winning successor performs on acquire
// (which writes a fresh record WITHOUT the Handoff field — clearing it, T5);
// this reservation is only the bounded gap-window guard.
//
// Record-only CAS (no flock): it edits the current record in place, so it must
// be called while OLD still owns the lease. ok=false when there is no readable
// record to stamp (the caller proceeds without a reservation — a benign
// degrade to the winner-delivery guarantee).
func ReserveHandoff(project, successorID string, ttl time.Duration) (ok bool, err error) {
	return reserveHandoffWithCfg(project, successorID, ttl, defaultLeaseConfig())
}

func reserveHandoffWithCfg(project, successorID string, ttl time.Duration, cfg leaseConfig) (bool, error) {
	if successorID == "" {
		return false, fmt.Errorf("coordlock.ReserveHandoff: successorID required")
	}
	paths, rerr := resolvePaths(project)
	if rerr != nil {
		return false, rerr
	}
	if ttl <= 0 {
		ttl = cfg.handoffTTL
	}
	l := &Lease{cfg: cfg, paths: paths, host: hostname(), boot: cfg.boot()}
	return l.withEpochLock(func() (bool, error) {
		cur, err := readEpoch(l.paths.epoch)
		if err != nil {
			return false, nil // no record to stamp -> degrade to winner-delivery
		}
		cur.Handoff = &handoffInfo{
			SuccessorID:   successorID,
			ExpiresAtMono: l.cfg.nowMono() + int64(ttl),
		}
		return true, l.writeEpochLocked(cur)
	})
}

// ClaimStartingRecord writes a `starting` epoch record naming agentID as the
// to-be owner BEFORE that agent's coord-run supervisor has acquired the flock
// (D2 spawn-serialization — replaces the deleted marker's spawn-in-flight
// role). It is a RECORD-ONLY CAS (no flock, owner pid unknown pre-spawn): it
// writes ONLY when the lease is currently CLAIMABLE (free / stealable / no
// live boot in flight / no valid handoff reservation), so it never clobbers a
// live owner or an in-progress takeover. The spawned coord-run then acquires
// the free flock, CASes the epoch forward, and Activates. A concurrent
// resolver that observes this record WAITs (a boot is in flight) instead of
// spawning a duplicate — closing the session-spawn→acquire boot window the
// deleted coord-spawn marker used to bridge.
//
//	ok=true  -> claimed (a `starting` record now names agentID).
//	ok=false -> a live owner / booting starter / valid handoff exists; the
//	            caller must NOT spawn (attach/wait instead).
func ClaimStartingRecord(project, agentID string) (ok bool, err error) {
	return claimStartingWithCfg(project, agentID, defaultLeaseConfig())
}

func claimStartingWithCfg(project, agentID string, cfg leaseConfig) (bool, error) {
	paths, rerr := resolvePaths(project)
	if rerr != nil {
		return false, rerr
	}
	if err := os.MkdirAll(filepath.Dir(paths.flock), 0o755); err != nil {
		return false, err
	}
	self := identity{AgentID: agentID, Project: project} // pid unknown pre-spawn
	l := &Lease{cfg: cfg, paths: paths, self: self, host: hostname(), boot: cfg.boot()}
	return l.withEpochLock(func() (bool, error) {
		cur, rerr := readEpoch(l.paths.epoch)
		switch {
		case errors.Is(rerr, os.ErrNotExist):
			cur = epochRecord{Epoch: 0}
		case rerr != nil:
			return false, rerr
		}
		if !l.leaseClaimable(cur) {
			return false, nil
		}
		l.epoch = cur.Epoch
		// Stamp BootID + RenewedAt* so the pre-spawn claim reads as
		// within-startingTTL from birth (codex D2 iter-2 [P1]); otherwise a
		// second concurrent claim would read the fresh record as past-TTL and
		// overwrite it, defeating the spawn-serialization this record exists
		// for.
		return true, l.writeEpochLocked(l.freshLeaseRecord(cur.Epoch, stateStarting, self))
	})
}

// leaseClaimable reports whether a fresh spawn may claim the lease as
// `starting` given the current record — i.e. there is no live owner, no live
// boot in flight, no in-progress takeover, and no valid handoff reservation.
func (l *Lease) leaseClaimable(cur epochRecord) bool {
	switch cur.State {
	case stateActive:
		return !l.holderHealthy(cur) // a healthy active owner blocks a claim
	case stateStarting:
		// A same-boot within-startingTTL record is a live boot in flight —
		// UNLESS its owner pid was stamped (the pre-spawn claim window closed:
		// a real coord-run acquired) and that pid is PROVABLY dead. That is a
		// pre-activation crash (SIGKILL before Activate); the flock is already
		// free and there is nothing left to protect (no-auto-kill guards LIVE
		// processes only), so it is claimable NOW rather than after the full
		// startingTTL. This keeps leaseClaimable consistent with the resolver's
		// ownerConfirmedDead fall-through (codex D2 iter-3 [P1]); without it a
		// pre-activation crash blocks attach/spawn recovery for ~120s. A
		// pre-spawn claim record (pid unset) stays unclaimable within TTL, so
		// spawn-serialization is preserved.
		withinTTL := cur.BootID == l.boot &&
			l.cfg.nowMono()-cur.RenewedAtMono <= int64(l.cfg.startingTTL)
		ownerConfirmedDead := cur.Owner.Pid > 0 && !l.pidAlive(cur.Owner)
		if cur.Owner.Pid > 0 && !ownerConfirmedDead {
			// The owner pid IS stamped and IS live — a wedged-but-alive
			// starter, TTL-independent. This primitive must NEVER clobber a
			// live process (no-auto-kill): the only sanctioned path to
			// dethrone a live wedged starter is the resolver's
			// Supersede+SpawnStandby record-CAS-first sequence (T6s), not a
			// raw claim. Without this guard, any direct or racing caller of
			// ClaimStartingRecord could overwrite a live starter's record —
			// bypassing the standby handoff entirely (codex D3 iter-5 [P2]).
			return false
		}
		return !withinTTL || ownerConfirmedDead
	case stateFencing:
		return l.transientResumable(cur) // a fresh in-progress takeover blocks
	default: // released / fenced_not_acquired / empty(epoch 0)
		if cur.Handoff != nil && cur.Handoff.SuccessorID != "" &&
			cur.BootID == l.boot && l.cfg.nowMono() <= cur.Handoff.ExpiresAtMono {
			return false // a valid successor reservation blocks a fresh claim
		}
		return true
	}
}

// reacquireOwnExpired re-acquires the caller's own expired lease IN PLACE at
// the SAME epoch (DESIGN-coord-lease-false-fence-prevention piece 1). It is
// the lease-check side's answer to a rival-free stall: prev is the probe
// snapshot leaseCheckByAncestorWithCfg read (owner == the caller's ancestor,
// state expired `active` ONLY — the supervisor's heartbeat goroutine never
// stops on an expired-active record, so a same-epoch refresh restores
// normal renewal. A stale `fencing` record is NEVER re-acquired even with a
// dead candidate (codex iter-4 [P1]): the incumbent's heartbeat self-demoted
// permanently the moment it saw state!=active, so re-activating the record
// would leave a lease no goroutine renews — expiring every TTL until a
// standby steals leadership from a still-healthy coord. An abandoned
// takeover instead FENCES and waits for a successor to resume it via
// transientResumable).
//
// The write is the heartbeatOnce CAS shape — state→active, refresh
// renewed_at_mono/_wall, owner unchanged, epoch UNCHANGED — under
// coordinator.epoch.lock via writeEpochLocked. No epoch bump: the incumbent
// supervisor's heartbeat goroutine self-demotes permanently on an epoch it
// doesn't recognize (heartbeatOnce), so bumping would convert a transient
// renewal stall into a guaranteed loss of the in-process heartbeat. No
// takeOver() machinery: there is no rival to kill and the flock never left
// the supervisor. Split-brain safety without the bump: a racing candidate's
// takeOver re-checks holderHealthy inside its own epoch-locked fence closure,
// which sees this write; both writers serialize on coordinator.epoch.lock.
//
// CAS precondition, pinned: under the lock, re-read and require owner + epoch
// unchanged since the probe AND the record still one of the two re-acquirable
// shapes. On a failed precondition, branch on WHAT the re-read shows:
//
//	fresh `active`, same owner+epoch -> our OWN heartbeat recovered in the
//	    probe->CAS window -> no write, no fence: proceed (nil).
//	anything else (rival bumped/flipped it) -> no write: fence verdict for
//	    what the record now shows. Never a bare-proceed on an expired
//	    record, never a retry loop (a wrong fence costs one skipped tick).
//
// Returns nil => the tick may proceed (re-acquired, or found healthy).
func (l *Lease) reacquireOwnExpired(prev epochRecord) error {
	var verdict error // nil => proceed; set => the fence verdict to return
	_, err := l.withEpochLock(func() (bool, error) {
		cur, rerr := readEpoch(l.paths.epoch)
		if rerr != nil {
			return false, rerr
		}
		if cur.Epoch != prev.Epoch || !cur.Owner.equal(prev.Owner) {
			// A rival advanced the lease in the probe->CAS window.
			verdict = fmt.Errorf("%w: %s: lease moved during re-acquire (epoch %d -> %d, state=%s)",
				ErrNotLeaseOwner, fenceTagOwnExpiredRival, prev.Epoch, cur.Epoch, cur.State)
			return false, nil
		}
		switch {
		case cur.State == stateActive && cur.BootID == l.boot &&
			l.cfg.nowMono()-cur.RenewedAtMono <= int64(l.cfg.ttl):
			// Our own heartbeat recovered mid-window: the lease is healthy
			// again. Do not write, do not fence — proceed.
			return false, nil
		case cur.State == stateReleased || cur.State == stateFencedNotAcquired:
			verdict = fmt.Errorf("%w: %s: our lease was released during re-acquire (state=%s); never resurrect",
				ErrNotLeaseOwner, fenceTagOwnReleased, cur.State)
			return false, nil
		case l.ownExpiredRival(cur):
			verdict = fmt.Errorf("%w: %s: a takeover rival appeared during re-acquire (candidate pid=%d)",
				ErrNotLeaseOwner, fenceTagOwnExpiredRival, cur.Candidate.Pid)
			return false, nil
		case cur.State != stateActive:
			// stateFencing included: even a dead-candidate fencing record is
			// not re-acquirable (the incumbent heartbeat is gone; codex
			// iter-4 [P1]) — and any unrecognized state fences too.
			verdict = fmt.Errorf("%w: %s: lease state %q is not re-acquirable",
				ErrNotLeaseOwner, fenceTagOwnExpiredRival, cur.State)
			return false, nil
		case cur.BootID != l.boot:
			// Cross-boot record (mirror of the probe's gate, codex iter-2
			// [P2]): pid_start is only comparable within a boot, so the
			// ownership proof does not hold — never re-stamp a previous
			// boot's record as ours.
			verdict = fmt.Errorf("%w: %s: lease record is from a previous boot (%q); not re-acquirable",
				ErrNotLeaseOwner, fenceTagOwnExpiredRival, cur.BootID)
			return false, nil
		case !l.pidAlive(cur.Owner):
			// The owner supervisor died in the probe->CAS window. Publishing
			// a fresh `active` record for a corpse would make holderHealthy
			// readers (LeaderPresent, the STONITH never-shoot-the-live-leader
			// gate) report a dead leader for up to TTL. Fence: skip this
			// tick; the next probe sees ancestorIsOwner=false and routes to
			// the takeover path.
			verdict = fmt.Errorf("%w: %s: recorded owner pid=%d died during re-acquire; not resurrecting a dead owner's lease",
				ErrNotLeaseOwner, fenceTagOwnExpiredRival, cur.Owner.Pid)
			return false, nil
		}
		// Still the one re-acquirable shape (expired active, same boot,
		// live owner): same-identity, same-epoch refresh. Candidate is
		// cleared defensively. Host is PRESERVED: this process's Lease has
		// host unset (lease-check builds it bare), so the heartbeatOnce
		// shape's `cur.Host = l.host` would blank it. BootID re-stamp is a
		// same-boot no-op (cross-boot records fenced above); kept so the
		// written record is always complete.
		cur.State = stateActive
		cur.Candidate = identity{}
		cur.BootID = l.boot
		cur.RenewedAtMono = l.cfg.nowMono()
		cur.RenewedAtWall = time.Now().UnixNano()
		return true, l.writeEpochLocked(cur)
	})
	switch {
	case errors.Is(err, errSerializerBusy):
		// A writer held coordinator.epoch.lock for the whole budget — very
		// likely a rival mid-write. NEVER bare-proceed on an expired record:
		// fence this tick and re-check next tick.
		return fmt.Errorf("%w: %s: epoch serializer busy during re-acquire (a writer is mid-flight); skipping this tick",
			ErrNotLeaseOwner, fenceTagOwnExpiredRival)
	case err != nil:
		return fmt.Errorf("coordlock.LeaseCheck: re-acquire own expired lease: %w", err)
	}
	return verdict
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
	// Only a lease that actually held the flock is a real ownership release;
	// a never-acquired lease or a second (idempotent) Release must not emit a
	// spurious lease.release. Captured before the flock is dropped below.
	heldFlock := l.flock != nil
	if l.stopHB != nil {
		close(l.stopHB)
		<-l.hbDone
		l.stopHB = nil
	}
	// Demote the record to `released` while we still hold the flock (so a
	// successor can't have advanced the epoch under us yet). Only if it is
	// still exactly ours — never stomp a successor's record. This is what
	// makes outstanding tokens invalid IMMEDIATELY (state != active). Both
	// `active` (a normal leader release) AND `starting` (D2: a coord that
	// acquired the lease but exited on a PRE-activation failure path —
	// child.Start() failed, or Activate() errored/was superseded — before
	// the starting->active flip) are demoted. Without the `starting` case a
	// crashed pre-activation boot leaves the epoch file `starting`, which
	// LeaseRecordActive now counts as a live lease generation, so handoff
	// delivery keeps the doc pending until startingTTL even though the owner
	// is already gone (codex D2 iter-1 [P1] — fleet-owns-its-resources: the
	// failure path must reap the record it wrote).
	//
	// The demote is RETRIED across transient epoch.lock contention because
	// leaving the record `active` after we drop the flock would keep a
	// racing goroutine's token valid until TTL — the exact guarantee we
	// promise. Epoch.lock writers hold it only for one fast write, so a
	// bounded retry reliably wins. If it ultimately cannot demote, SURFACE
	// it (surface-don't-silo) rather than silently leaving a stale-active
	// record; the TTL is then the fallback bound.
	// demoted records whether the epoch record was cleanly demoted out of
	// `active` (or already not ours). It gates the lease.release emit below:
	// on a fault path where demotion failed, the on-disk lease may still be
	// active with valid outstanding tokens until TTL, so the log must say
	// demoted=false rather than report a clean release (codex P3). Defaults
	// true for the no-flock case (nothing to demote), but the emit only fires
	// when the flock was actually held.
	demoted := true
	// superseded is true when the demote found the record already belongs to a
	// SUCCESSOR (epoch moved / owner changed) — leadership transferred during
	// the successor's fencing, which already logged its own lease.acquire.
	// Emitting our lease.release here too would add a spurious SECOND
	// ownership-transition event after the successor's acquire, muddying
	// exactly the failover sequence these logs exist to diagnose (codex P2).
	// So it gates the emit below; false on the ordinary self-release + fault
	// paths, which still emit.
	superseded := false
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
				if (cur.State != stateActive && cur.State != stateStarting) ||
					cur.Epoch != l.epoch || !cur.Owner.equal(l.self) {
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
				// ok==true: we demoted OUR active record (a genuine self-
				// release). ok==false: the record already moved to a successor
				// — leadership transferred during fencing, so we did NOT
				// release a live coordinator lease (codex P2). Either way no
				// stale-active record of OURS remains (guarantee met).
				guaranteed = true
				superseded = !ok
				break demoteLoop
			}
		}
		demoted = guaranteed
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
	// lease.release: ownership transition, emitted AFTER the flock drops so
	// the emit is unambiguously outside every lock (the demote loop already
	// released epoch.lock each iteration). Only for a lease that truly held
	// the flock. `demoted` distinguishes a clean release (record left the
	// active state) from a fault path where the record may still be active
	// with valid tokens until TTL (codex P3) — the log must not overstate
	// success on exactly the path operators inspect during a bad handoff.
	// Best-effort. Suppressed on: the takeover-only path (a drain safety-net
	// that fenced+released never ran a coordinator, so it must not show a
	// phantom coordinator lifecycle); and when a SUCCESSOR already took the
	// lease (superseded), whose own lease.acquire already logged the transition
	// — a second event here would double-count the failover (codex P2).
	if heldFlock && l.cfg.logLifecycle && !superseded {
		l.emitEvent("lease.release", "info", map[string]any{
			"epoch": l.epoch, "demoted": demoted,
		})
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

// flockBody is the identity stamped into coordinator.flock's body
// immediately after a successful acquire. Under the flock-only lease
// (DESIGN-coord-lease-flock-only) it is the SOLE identity source: the
// ownership readers probe the flock with LOCK_SH and, when busy, read this
// body for the owner's agent_id + pid. AgentID + Project were added in PR-1
// (D2). A holder whose pid is dead / from another boot marks a holder hung
// in the acquire-to-epoch window, recoverable via takeover
// (flockHolderRecoverable). Mono is retained for schema stability but is no
// longer read for a TTL decision (PR-1 D4 deleted that clause — a live
// same-boot holder is never "recoverable" on staleness). Best-effort: flock
// exclusion is kernel-enforced; the body is only for identity, never the lock.
type flockBody struct {
	Pid      int    `json:"pid"`
	PidStart int64  `json:"pid_start"`
	AgentID  string `json:"agent_id"`
	Project  string `json:"project"`
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

// stampFlockBody writes this lease's identity (agent_id + project + pid tuple)
// + boot + mono into the held flock fd's body, immediately after acquire.
// Under the flock-only lease the body is the SOLE identity source, so the
// stamp is FAIL-CLOSED (D6): it returns an error on any failure and the caller
// releases the flock + fails the acquire rather than leave a body-less held
// flock a concurrent reader would see as identity-pending forever.
//
// Write-before-truncate (D3, identity-read hardening): write the full body at
// offset 0 FIRST, then Truncate to its length. The old order (Truncate(0) then
// Write) left a torn window where a concurrent identity reader (CurrentOwner)
// could see an EMPTY body on a held flock. Writing first means a grow is
// atomic-enough that a reader sees a complete body; only a shrink leaves stale
// trailing bytes for the microseconds before the truncate, which degrades to
// identity-pending (poll again), never to "no owner".
func (l *Lease) stampFlockBody(f *os.File) error {
	if l.cfg.stampErr != nil {
		return fmt.Errorf("stampFlockBody: %w", l.cfg.stampErr) // test-only fault injection (D3/T4)
	}
	b, err := json.Marshal(flockBody{
		Pid: l.self.Pid, PidStart: l.self.PidStart,
		AgentID: l.self.AgentID, Project: l.self.Project,
		BootID: l.boot, Mono: l.cfg.nowMono(),
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

// liveProspectiveTargets enumerates the prospective STONITH target set —
// the recorded OLD owner, the prior fencing candidate, and the current
// flock-body holder (the same set killTargets hands Phase 2) — with
// exactly ONE filter change vs killTargets: the dead-pid drop is REMOVED
// (liveness is what we are probing, not a filter). The pid<=0 and self
// exclusions are RETAINED: probing self would abort every takeover
// including the resume-own-fencing retry where the prior candidate IS
// self. Returns the subset found ALIVE (pid+pid_start via the injectable
// seam); empty means every prospective target is provably dead and the
// takeover may proceed. Called inside the epoch.lock closure BEFORE the
// fencing write (KP6, DESIGN-coord-no-auto-kill).
func (l *Lease) liveProspectiveTargets(old identity, extra ...identity) []identity {
	candidates := make([]identity, 0, 3)
	add := func(id identity) {
		if id.Pid <= 0 || id.Pid == l.self.Pid {
			return
		}
		for _, t := range candidates {
			if t.Pid == id.Pid && t.PidStart == id.PidStart {
				return // already queued
			}
		}
		candidates = append(candidates, id)
	}
	add(old)
	for _, e := range extra {
		add(e)
	}
	if b, err := os.ReadFile(l.paths.flock); err == nil && len(b) > 0 {
		var body flockBody
		if json.Unmarshal(b, &body) == nil && body.Pid > 0 {
			add(identity{Pid: body.Pid, PidStart: body.PidStart, Project: l.self.Project})
		}
	}
	var live []identity
	for _, t := range candidates {
		if l.pidAlive(t) {
			live = append(live, t)
		}
	}
	return live
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

// flockHolderRecoverable reports whether a BUSY flock is recoverable via
// takeover. It reads the flock body: recoverable ONLY if the body is missing/
// unparseable, from another boot, or its pid is dead. A same-boot body with a
// live pid is a legitimate holder -> NOT recoverable (stand down), even if it
// has been quiet for a long time.
//
// PR-1 (D4) DELETED the old TTL clause (`nowMono-body.Mono > ttl`). That clause
// made a live-but-quiet holder look recoverable — the literal incident bug: a
// busy coord heads-down in a long task stopped stamping/renewing and was
// wrongly treated as recoverable, letting an attach spawn a duplicate beside
// it. A holder is now recoverable iff it is provably GONE (missing / cross-boot
// / pid-dead), never merely quiet. Mono is retained in the body for schema
// stability but no longer drives a recovery decision.
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
	return false // live same-boot holder -> NOT recoverable (D4: no TTL clause)
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
	// Parent-dir fsync (D1): make the rename itself durable so a crash
	// immediately after this write can never lose the epoch record. The
	// deleted coord-spawn marker got this durability via state.WriteAtomic;
	// the lease is now the SOLE owner record, so it must carry the same
	// guarantee. Best-effort — a dir-fsync failure on exotic filesystems
	// must not fail an otherwise-committed rename.
	if dir, derr := os.Open(filepath.Dir(l.paths.epoch)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
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

// flockBodyOwner reports the current owner of project's coordinator.flock by
// probing it (flock-only lease, DESIGN-coord-lease-flock-only). See
// flockBodyOwnerWithCfg.
func flockBodyOwner(project string) (Owner, bool) {
	return flockBodyOwnerWithCfg(project, defaultLeaseConfig())
}

// flockBodyOwnerWithCfg is the single flock-body ownership reader — the read
// truth for every ownership reader in the flock-only lease (D1). It probes
// coordinator.flock with a SHARED non-blocking lock (LOCK_SH|LOCK_NB), the
// load-bearing choice:
//
//   - EWOULDBLOCK ⇒ a live holder's LOCK_EX conflicts ⇒ owner=true. Kernel-
//     proven liveness: a held LOCK_EX means the holder process is alive (the
//     kernel frees the flock on death; fleet's flock fd is O_CLOEXEC so exec'd
//     children can't keep it held). Identity comes from the body's agent_id +
//     pid when parseable; a torn / empty / old-schema (no agent_id) body is
//     STILL owner=true with AgentID="" — never downgraded (busy is the truth;
//     the body is only for identity).
//   - acquired (no LOCK_EX holder) ⇒ owner=false; release the shared probe
//     immediately. This correctly reads a released-but-alive holder (graceful
//     handoff Release() drops the LOCK_EX though the process lives) AND a dead
//     holder as free — a body-pid check alone could not.
//   - ENOENT / unreadable ⇒ owner=false (free) — never created, no crash.
//
// LOCK_SH (not LOCK_EX) so concurrent readers coexist and never make each
// other misread "busy"; the shared probe releases in microseconds. cfg is
// accepted for parity with the sibling *WithCfg readers so callers thread the
// same seam — the probe itself needs no clock/boot/pid seam because the kernel
// is the liveness oracle.
func flockBodyOwnerWithCfg(project string, cfg leaseConfig) (Owner, bool) {
	_ = cfg
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
		return Owner{}, false // unexpected flock fault -> conservative "free"
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

// CurrentActiveOwnerPID reports the supervisor PID of the current coordinator
// lease owner for project, or (0,false) if the flock is free / its body has no
// pid. It is a SPAWN-GATE reader (D5): it reads the flock body (busy⇒owner),
// NOT the epoch, and does NOT gate on agent_id. The authenticated kill
// primitive (internal/coord) uses it as the "never shoot the live leader"
// gate: a STONITH target is refused if it IS the current owner. A torn body
// (no pid) degrades to (0,false), which makes the kill primitive refuse rather
// than fire on stale data. Best-effort + read-only.
func CurrentActiveOwnerPID(project string) (pid int, ok bool) {
	owner, present := flockBodyOwner(project)
	if !present || owner.PID <= 0 {
		return 0, false
	}
	return owner.PID, true
}

// CurrentOwner reports the full active lease owner tuple for project, or
// ok=false if there is no readable HEALTHY active owner. It is the delivery-side
// companion to CurrentActiveOwnerPID: callers that need to type a handoff
// resume prompt must address the agent record named by the lease owner, not a
// preselected standby that may have lost the lock race.
//
// It is the DELIVERY reader (D5): it reads the flock body (busy⇒owner) and
// ADDITIONALLY requires a readable agent_id to type a resume prompt. A busy
// flock whose body is identity-less (old-binary / torn body) is
// owner-present-but-identity-PENDING → ok=false, so DeliverToCurrentOwner keeps
// polling for the identity rather than reading "no owner" or typing into a
// corpse. A free flock → ok=false (no owner).
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

// LiveOwner reports the coordinator lease owner for project whenever the flock
// is HELD by a live process (D1 busy⇒owner). It is the resolver's
// (internal/coordreconcile) attach/spawn gate: a live flock holder — busy,
// booting, or version-skewed — is STILL the coord and MUST NOT be spawned
// beside (the no-auto-kill invariant; the incident fix). It is a SPAWN-GATE
// reader (D5): it does NOT gate on agent_id, so a busy flock with an
// identity-less body is still an owner (ok=true, Owner.AgentID==""); the attach
// verdict (coordreconcile.Resolve) then distinguishes AgentID present ⇒ Attach
// from identity-less ⇒ Wait.
//
// ok=false only when the flock is FREE (kernel-freed on the holder's death, or
// dropped by a graceful Release()). Read-only; a torn body degrades to
// ok=true / identity-pending, never to ok=false.
func LiveOwner(project string) (Owner, bool) {
	return liveOwnerWithCfg(project, defaultLeaseConfig())
}

func liveOwnerWithCfg(project string, cfg leaseConfig) (Owner, bool) {
	return flockBodyOwnerWithCfg(project, cfg)
}

// CurrentHandoff returns the NON-expired successor reservation for project,
// if the current owner stamped one while releasing the lease (D3). ok=false
// when there is no readable record, no handoff sub-record, the reservation
// is from a previous boot, or it has expired (successor died mid-boot →
// the lease is free and #247's respawn proceeds). Read-only; a torn read
// degrades to ok=false.
func CurrentHandoff(project string) (Handoff, bool) {
	return currentHandoffWithCfg(project, defaultLeaseConfig())
}

func currentHandoffWithCfg(project string, cfg leaseConfig) (Handoff, bool) {
	paths, err := resolvePaths(project)
	if err != nil {
		return Handoff{}, false
	}
	rec, err := readEpoch(paths.epoch)
	if err != nil {
		return Handoff{}, false
	}
	if rec.Handoff == nil || rec.Handoff.SuccessorID == "" {
		return Handoff{}, false
	}
	// Same-boot only: the monotonic deadline is meaningless across boots, so
	// a reservation from a previous boot is treated as expired.
	if rec.BootID != cfg.boot() {
		return Handoff{}, false
	}
	if cfg.nowMono() > rec.Handoff.ExpiresAtMono {
		return Handoff{}, false
	}
	return Handoff{SuccessorID: rec.Handoff.SuccessorID}, true
}

// CurrentStarting reports whether project's lease is currently in `starting`
// state and, if so, the owner tuple + liveness + whether it is within its
// startingTTL. ok=false when the record is missing/unreadable or its state
// is not `starting`. OwnerLive is same-boot-gated (see liveOwnerWithCfg):
// pid+pid_start equality alone is only reuse-safe within the SAME boot, so a
// cross-boot record's owner is reported dead regardless of the raw pidAlive
// result. Read-only; a torn read degrades to ok=false.
func CurrentStarting(project string) (StartingStatus, bool) {
	return currentStartingWithCfg(project, defaultLeaseConfig())
}

func currentStartingWithCfg(project string, cfg leaseConfig) (StartingStatus, bool) {
	paths, err := resolvePaths(project)
	if err != nil {
		return StartingStatus{}, false
	}
	rec, err := readEpoch(paths.epoch)
	if err != nil {
		return StartingStatus{}, false
	}
	if rec.State != stateStarting {
		return StartingStatus{}, false
	}
	l := &Lease{cfg: cfg, paths: paths, boot: cfg.boot()}
	sameBoot := rec.BootID == cfg.boot()
	withinTTL := sameBoot && cfg.nowMono()-rec.RenewedAtMono <= int64(cfg.startingTTL)
	return StartingStatus{
		Owner: Owner{
			AgentID:       rec.Owner.AgentID,
			PID:           rec.Owner.Pid,
			PidStart:      rec.Owner.PidStart,
			EngineStamped: rec.Owner.AgentID != "" && rec.Owner.PidStart > 0,
		},
		// Cross-boot record -> the owner is provably dead (a reboot kills
		// every process), regardless of what a bare pid+pid_start equality
		// on Linux's boot-relative starttime might coincidentally match
		// (codex-adversarial review finding, same root cause as
		// liveOwnerWithCfg above).
		OwnerLive: sameBoot && l.pidAlive(rec.Owner),
		WithinTTL: withinTTL,
		Epoch:     rec.Epoch,
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

// LeaseRecordActive reports whether a readable epoch record exists on disk in a
// NON-terminal state (active, starting, or fencing) — i.e. a lease generation
// is live for project even if its current owner is momentarily unhealthy/stale,
// mid-boot (D2 two-phase startup), or a takeover is mid-flight. It is the "is
// this a real lease, or a legacy/bare coord that never wrote an epoch"
// discriminator for handoff delivery (codex iter-22 [P1]): CurrentOwner
// suppresses a stale/dead active owner (and a `starting` owner, since it isn't
// active yet) as ok=false, but that is NOT the same as "no lease exists".
// Delivery must keep the doc PENDING for a healthy takeover OR a coord that is
// still booting when a lease record is present, and only direct-send (legacy
// fallback) when there is genuinely no lease record. `starting` was added
// alongside D2 (two-phase startup, TASK-PLAN-coord-lease-sole-identity): without
// it, a handoff-delivery poll that lands entirely inside a real coord's
// starting->active boot window would misread the live boot as "no lease at
// all" and trigger the legacy direct-send fallback instead of staying pending
// for the booting owner. Read-only; a missing/torn/terminal record degrades to
// false.
func LeaseRecordActive(project string) bool {
	// Flock-only (D5): a live lease generation exists iff the flock is HELD by a
	// live process. Delivery keeps a doc PENDING while a coord holds the flock
	// (busy) — a healthy holder or one still booting — and direct-sends (legacy
	// fallback) only when the flock is genuinely FREE (no owner). Kernel-proven
	// liveness replaces the epoch-state enumeration; agent_id is not required.
	_, present := flockBodyOwner(project)
	return present
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

// LeaderPresent reports whether a coordinator lease is currently held by a
// live process for project — i.e. whether a duplicate spawn should stand down.
// Flock-only (D5): present iff the flock is HELD (busy⇒owner, kernel-proven
// liveness), regardless of agent_id or epoch state. A free flock ⇒ no leader.
//
// Accepted P2 (see task plan): collapsing onto flock-busy drops the old
// stateFencing special case, so during a takeover (old released, new not yet
// acquired) LeaderPresent can read a transient free flock ⇒ false for a few ms.
// This fails CONSERVATIVE (a brief "no leader" during a handoff, which fleet
// drain already tolerates) and is the correct flock-only reading.
//
// Read-only; a free/unreadable flock degrades to false. Used by cmd/fleet's
// coordLeaderCheck to disambiguate a clean stand-down from a supervisor failure.
func LeaderPresent(project string) bool {
	return leaderPresentWithCfg(project, defaultLeaseConfig())
}

// leaderPresentWithCfg is the seam-threaded core of LeaderPresent. cfg is
// passed through to the flock probe for sibling-reader parity (the probe needs
// no seam — the kernel is the liveness oracle).
func leaderPresentWithCfg(project string, cfg leaseConfig) bool {
	_, present := flockBodyOwnerWithCfg(project, cfg)
	return present
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
