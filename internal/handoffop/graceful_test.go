//go:build linux || darwin

// Tests for the in-process graceful handoff (DESIGN-handoff-drain-storm-
// leak §3(A), PR3). Deterministic — injectable seams, no real spawn / disk
// / clock. Channels + call-order slices observe convergence; no time.Sleep
// timing assertions.
//
//	T11  end-to-end: spawns EXACTLY ONE standby; writes doc + checkpoint
//	     BEFORE the barrier; barrier names the captured epoch.
//	T43  barrier ordering + atomicity: a checkpoint write error aborts
//	     BEFORE the barrier — no handoff-complete-<epoch>.json on disk; a
//	     spawn / drain failure likewise leaves no barrier.
package handoffop

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
)

// gracefulRecorder captures the ordered side effects of a GracefulHandoff
// run so a test can assert ordering + counts.
type gracefulRecorder struct {
	mu          sync.Mutex
	order       []string          // step names in call order
	writes      map[string][]byte // path -> bytes (atomic writes)
	standbyRuns int
}

func newGracefulRecorder() *gracefulRecorder {
	return &gracefulRecorder{writes: map[string][]byte{}}
}

func (r *gracefulRecorder) note(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, s)
}

func (r *gracefulRecorder) write(path string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes[path] = append([]byte(nil), data...)
	r.order = append(r.order, "write:"+path)
	return nil
}

func (r *gracefulRecorder) snapshot() ([]string, map[string][]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.order...)
	w := map[string][]byte{}
	for k, v := range r.writes {
		w[k] = v
	}
	return out, w, r.standbyRuns
}

func oldRecForGraceful() *agent.Record {
	rec := agent.New("oldcoord1")
	rec.Project = "projects-fleet"
	rec.TaskID = "coord"
	return rec
}

const (
	gracefulDocPath        = "/tmp/fleet-graceful-test/doc.md"
	gracefulCheckpointPath = "/tmp/fleet-graceful-test/checkpoint.json"
	gracefulBarrierPath    = "/tmp/fleet-graceful-test/.locks/handoff-complete-7.json"
)

// T11: end-to-end graceful handoff. Spawns exactly one standby, writes doc
// + checkpoint BEFORE the barrier, and the barrier names the captured epoch.
func TestGracefulHandoff_EndToEnd_OneStandby_BarrierLast(t *testing.T) {
	rec := newGracefulRecorder()
	wantTimeout := 3 * time.Minute
	var gotTimeout time.Duration
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# handoff doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte(`{"state":"snapshot"}`),
		StandbyTimeout: wantTimeout,
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(timeout time.Duration) error {
			gotTimeout = timeout
			rec.mu.Lock()
			rec.standbyRuns++
			rec.mu.Unlock()
			rec.note("spawn-standby")
			return nil
		},
		WriteAtomic:   rec.write,
		DrainInFlight: func() error { rec.note("drain"); return nil },
		CurrentEpoch:  func(string) (int64, bool) { return 7, true },
		BarrierPath:   func(_ string, epoch int64) (string, error) { return gracefulBarrierPath, nil },
	}

	if err := GracefulHandoff(in, deps); err != nil {
		t.Fatalf("GracefulHandoff: %v", err)
	}

	order, writes, standby := rec.snapshot()

	if standby != 1 {
		t.Errorf("standby spawned %d times, want exactly 1", standby)
	}
	if gotTimeout != wantTimeout {
		t.Errorf("standby timeout = %s, want %s", gotTimeout, wantTimeout)
	}
	// The barrier must be the LAST write, and it must come after the doc +
	// checkpoint writes AND the drain.
	idx := func(s string) int {
		for i, v := range order {
			if v == s {
				return i
			}
		}
		return -1
	}
	iSpawn := idx("spawn-standby")
	iDoc := idx("write:" + gracefulDocPath)
	iCkpt := idx("write:" + gracefulCheckpointPath)
	iDrain := idx("drain")
	iBarrier := idx("write:" + gracefulBarrierPath)
	if iSpawn != 0 {
		t.Errorf("standby was not spawned first; order=%v", order)
	}
	if iDoc >= iBarrier || iCkpt >= iBarrier || iDrain >= iBarrier {
		t.Errorf("barrier not last: doc=%d ckpt=%d drain=%d barrier=%d order=%v",
			iDoc, iCkpt, iDrain, iBarrier, order)
	}
	if iBarrier != len(order)-1 {
		t.Errorf("barrier write is not the final side effect; order=%v", order)
	}
	// Barrier names the captured epoch.
	if b := string(writes[gracefulBarrierPath]); !strings.Contains(b, `"epoch":7`) {
		t.Errorf("barrier body missing epoch 7: %q", b)
	}
	if !strings.Contains(string(writes[gracefulBarrierPath]), `"old_agent":"oldcoord1"`) {
		t.Errorf("barrier body missing old_agent: %q", writes[gracefulBarrierPath])
	}
}

// T43: a checkpoint write error aborts BEFORE the barrier — no barrier is
// written. (The atomic .tmp->rename guarantee lives in state.WriteAtomic;
// here we assert the ORDERING invariant: the barrier is never reached if an
// earlier durable step fails.)
func TestGracefulHandoff_CheckpointError_NoBarrier(t *testing.T) {
	rec := newGracefulRecorder()
	wantErr := errors.New("disk full")
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte("{}"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { rec.standbyRuns++; return nil },
		WriteAtomic: func(path string, data []byte) error {
			if path == gracefulCheckpointPath {
				return wantErr // torn checkpoint
			}
			return rec.write(path, data)
		},
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(_ string, _ int64) (string, error) { return gracefulBarrierPath, nil },
	}

	err := GracefulHandoff(in, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected checkpoint error surfaced, got %v", err)
	}
	_, writes, _ := rec.snapshot()
	if _, ok := writes[gracefulBarrierPath]; ok {
		t.Errorf("BARRIER was written despite a checkpoint failure — barrier ordering violated")
	}
}

// T43(b): a drain failure aborts before the barrier — no barrier.
func TestGracefulHandoff_DrainError_NoBarrier(t *testing.T) {
	rec := newGracefulRecorder()
	wantErr := errors.New("drain wedged")
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte("{}"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby:  func(time.Duration) error { return nil },
		WriteAtomic:   rec.write,
		DrainInFlight: func() error { return wantErr },
		CurrentEpoch:  func(string) (int64, bool) { return 7, true },
		BarrierPath:   func(_ string, _ int64) (string, error) { return gracefulBarrierPath, nil },
	}
	err := GracefulHandoff(in, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected drain error surfaced, got %v", err)
	}
	_, writes, _ := rec.snapshot()
	if _, ok := writes[gracefulBarrierPath]; ok {
		t.Errorf("BARRIER written despite drain failure")
	}
}

// codex PR3 iter-2 [P2]: a post-spawn failure REAPS the standby (a standby
// left polling after a half-done handoff could acquire the lease without the
// completed checkpoint + barrier).
func TestGracefulHandoff_PostSpawnAbort_ReapsStandby(t *testing.T) {
	rec := newGracefulRecorder()
	wantErr := errors.New("checkpoint disk full")
	var reaped int32
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte("{}"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { rec.standbyRuns++; return nil },
		ReapStandby:  func() error { reaped++; return nil },
		WriteAtomic: func(path string, data []byte) error {
			if path == gracefulCheckpointPath {
				return wantErr
			}
			return rec.write(path, data)
		},
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(_ string, _ int64) (string, error) { return gracefulBarrierPath, nil },
	}
	err := GracefulHandoff(in, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the post-spawn error surfaced, got %v", err)
	}
	if reaped != 1 {
		t.Errorf("ReapStandby ran %d times after a post-spawn abort, want 1", reaped)
	}
	_, writes, _ := rec.snapshot()
	if _, ok := writes[gracefulBarrierPath]; ok {
		t.Errorf("barrier written despite the abort")
	}
}

// On the SUCCESS path the standby is NOT reaped (it goes on to acquire).
func TestGracefulHandoff_Success_DoesNotReapStandby(t *testing.T) {
	rec := newGracefulRecorder()
	var reaped int32
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte("{}"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { rec.standbyRuns++; return nil },
		ReapStandby:  func() error { reaped++; return nil },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(_ string, _ int64) (string, error) { return gracefulBarrierPath, nil },
	}
	if err := GracefulHandoff(in, deps); err != nil {
		t.Fatalf("GracefulHandoff: %v", err)
	}
	if reaped != 0 {
		t.Errorf("ReapStandby ran %d times on the success path, want 0", reaped)
	}
}

// T43(c): a standby spawn failure aborts before ANY write — no doc, no
// checkpoint, no barrier (the graceful path has no receiver).
func TestGracefulHandoff_SpawnError_NothingWritten(t *testing.T) {
	rec := newGracefulRecorder()
	wantErr := errors.New("tmux down")
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte("{}"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { return wantErr },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(_ string, _ int64) (string, error) { return gracefulBarrierPath, nil },
	}
	err := GracefulHandoff(in, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected spawn error surfaced, got %v", err)
	}
	_, writes, _ := rec.snapshot()
	if len(writes) != 0 {
		t.Errorf("writes happened despite spawn failure: %v", writes)
	}
}

// No lease epoch -> refuse before doing anything destructive.
func TestGracefulHandoff_NoEpoch_Refuses(t *testing.T) {
	rec := newGracefulRecorder()
	in := GracefulHandoffInputs{OldRec: oldRecForGraceful()}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { rec.standbyRuns++; return nil },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 0, false }, // no lease
		BarrierPath:  func(_ string, _ int64) (string, error) { return gracefulBarrierPath, nil },
	}
	if err := GracefulHandoff(in, deps); err == nil {
		t.Fatal("expected refusal when no lease epoch is held")
	}
	if _, _, standby := rec.snapshot(); standby != 0 {
		t.Errorf("standby spawned despite no-epoch refusal (count=%d)", standby)
	}
}

// nil SpawnStandby is a programming error -> refuse.
func TestGracefulHandoff_NilSpawnSeam_Refuses(t *testing.T) {
	in := GracefulHandoffInputs{OldRec: oldRecForGraceful()}
	deps := GracefulHandoffDeps{
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
	}
	if err := GracefulHandoff(in, deps); err == nil {
		t.Fatal("expected refusal when SpawnStandby seam is nil")
	}
}

// ---------------------------------------------------------------------------
// PR3 — completion phase (DESIGN-coord-spawn-unified-standby §5b / §6 PR3):
// after the barrier is durable the handoff COMPLETES in-process —
// retire OLD (releases the lease) → poll the lock winner → inject the doc.
// The winner reaps losing sibling standbys itself on acquire (coord-run's
// sweepStaleCompetitors, PR2); the orchestrator only addresses the winner.
// ---------------------------------------------------------------------------

func orderIdx(order []string, s string) int {
	for i, v := range order {
		if v == s {
			return i
		}
	}
	return -1
}

// Completion ordering: barrier FIRST (durable), then retire OLD, then
// deliver. A retire that ran before the barrier would let OLD exit with no
// "all clean" signal for the drain verifier; a delivery before retire would
// poll a lock OLD still holds.
func TestGracefulHandoff_Completion_RetireAfterBarrier_ThenDeliver(t *testing.T) {
	rec := newGracefulRecorder()
	winner := agent.New("winner01")
	var delivered *agent.Record
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { rec.note("spawn-standby"); return nil },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(string, int64) (string, error) { return gracefulBarrierPath, nil },
		RetireOld:    func() error { rec.note("retire-old"); return nil },
		DeliverDocToWinner: func() (*agent.Record, error) {
			rec.note("deliver")
			delivered = winner
			return winner, nil
		},
	}
	if err := GracefulHandoff(in, deps); err != nil {
		t.Fatalf("GracefulHandoff with completion seams: %v", err)
	}
	order, _, _ := rec.snapshot()
	iBarrier := orderIdx(order, "write:"+gracefulBarrierPath)
	iRetire := orderIdx(order, "retire-old")
	iDeliver := orderIdx(order, "deliver")
	if iBarrier == -1 || iRetire == -1 || iDeliver == -1 {
		t.Fatalf("missing completion steps; order=%v", order)
	}
	if iBarrier >= iRetire || iRetire >= iDeliver {
		t.Fatalf("completion out of order (want barrier < retire < deliver): %v", order)
	}
	if delivered == nil || delivered.ID != "winner01" {
		t.Fatalf("doc not delivered to the winner: %+v", delivered)
	}
}

// A retire failure aborts BEFORE delivery. The barrier stays on disk (it is
// truthful — doc + checkpoint are durable) and the standby is NOT reaped:
// post-barrier the standby/lazy-attach recovery is the vehicle that finishes
// the handoff, so tearing it down would strand the project.
func TestGracefulHandoff_Completion_RetireError_NoDeliver_NoReap(t *testing.T) {
	rec := newGracefulRecorder()
	wantErr := errors.New("old coord kill failed")
	reaped := 0
	deliverCalls := 0
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { return nil },
		ReapStandby:  func() error { reaped++; return nil },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(string, int64) (string, error) { return gracefulBarrierPath, nil },
		RetireOld:    func() error { return wantErr },
		DeliverDocToWinner: func() (*agent.Record, error) {
			deliverCalls++
			return nil, nil
		},
	}
	err := GracefulHandoff(in, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("retire error not surfaced (want errors.Is): %v", err)
	}
	if deliverCalls != 0 {
		t.Errorf("delivery ran %d times after a retire failure, want 0", deliverCalls)
	}
	if reaped != 0 {
		t.Errorf("standby reaped %d times post-barrier, want 0 (it is the recovery vehicle)", reaped)
	}
	_, writes, _ := rec.snapshot()
	if _, ok := writes[gracefulBarrierPath]; !ok {
		t.Errorf("barrier missing after a post-barrier retire failure (it must stay)")
	}
}

// A delivery failure propagates (errors.Is preserved) so the caller keeps the
// durable doc/queue PENDING — the next attach/drain re-delivers (§5c). The
// standby is NOT reaped: the winner is the live coord.
func TestGracefulHandoff_Completion_DeliverError_Propagates_NoReap(t *testing.T) {
	rec := newGracefulRecorder()
	wantErr := errors.New("owner poll timed out")
	reaped := 0
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { return nil },
		ReapStandby:  func() error { reaped++; return nil },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(string, int64) (string, error) { return gracefulBarrierPath, nil },
		RetireOld:    func() error { return nil },
		DeliverDocToWinner: func() (*agent.Record, error) {
			return nil, wantErr
		},
	}
	err := GracefulHandoff(in, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("delivery error not surfaced (want errors.Is): %v", err)
	}
	if reaped != 0 {
		t.Errorf("standby reaped %d times after a delivery failure, want 0 (winner is the live coord)", reaped)
	}
}

// The completion seams come as a PAIR. Retiring OLD without a delivery seam
// would orphan the doc behind a dead coord; delivering without retiring
// would poll a lock OLD never releases. Refuse before any side effect.
func TestGracefulHandoff_Completion_SeamsMustPair(t *testing.T) {
	for name, deps := range map[string]GracefulHandoffDeps{
		"retire-only": {
			SpawnStandby: func(time.Duration) error { t.Error("spawned despite invalid seams"); return nil },
			CurrentEpoch: func(string) (int64, bool) { return 7, true },
			RetireOld:    func() error { return nil },
		},
		"deliver-only": {
			SpawnStandby:       func(time.Duration) error { t.Error("spawned despite invalid seams"); return nil },
			CurrentEpoch:       func(string) (int64, bool) { return 7, true },
			DeliverDocToWinner: func() (*agent.Record, error) { return nil, nil },
		},
	} {
		in := GracefulHandoffInputs{OldRec: oldRecForGraceful()}
		if err := GracefulHandoff(in, deps); err == nil {
			t.Errorf("%s: expected refusal when completion seams are not paired", name)
		}
	}
}

// Nil completion seams = the original prepare-only stub contract: return
// right after the barrier (OLD self-retires; drain verifier finishes).
// Pinned so the PR3 completion phase stays opt-in for legacy callers.
func TestGracefulHandoff_PrepareOnly_NoCompletionSeams_StopsAtBarrier(t *testing.T) {
	rec := newGracefulRecorder()
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# doc\n"),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func(time.Duration) error { return nil },
		WriteAtomic:  rec.write,
		CurrentEpoch: func(string) (int64, bool) { return 7, true },
		BarrierPath:  func(string, int64) (string, error) { return gracefulBarrierPath, nil },
	}
	if err := GracefulHandoff(in, deps); err != nil {
		t.Fatalf("prepare-only GracefulHandoff: %v", err)
	}
	order, _, _ := rec.snapshot()
	if i := orderIdx(order, "write:"+gracefulBarrierPath); i != len(order)-1 {
		t.Fatalf("barrier is not the final side effect in prepare-only mode; order=%v", order)
	}
}

// ---------------------------------------------------------------------------
// GracefulSwapEligible — the route gate for converging the live-coord
// handoff paths onto the barrier (§6 PR3). True only when failover is on,
// OLD is the project's CURRENT healthy lease owner (a live-coord handoff,
// not a dead-coord recovery) and the doc will be auto-delivered.
// ---------------------------------------------------------------------------

func TestGracefulSwapEligible(t *testing.T) {
	old := oldRecForGraceful()
	swapOwner := func(t *testing.T, fn func(string) (coordlock.Owner, bool)) {
		t.Helper()
		orig := gracefulEligibleOwnerFn
		gracefulEligibleOwnerFn = fn
		t.Cleanup(func() { gracefulEligibleOwnerFn = orig })
	}
	ownerIsOld := func(p string) (coordlock.Owner, bool) {
		if p != old.Project {
			t.Fatalf("owner consulted for project %q, want %q", p, old.Project)
		}
		return coordlock.Owner{AgentID: old.ID, PID: 4242, PidStart: 9}, true
	}

	t.Run("live owner + auto-resume -> eligible", func(t *testing.T) {
		swapOwner(t, ownerIsOld)
		if !GracefulSwapEligible(old, true) {
			t.Fatal("want eligible when OLD owns the lease and auto-resume is on")
		}
	})
	t.Run("owner is someone else -> not eligible", func(t *testing.T) {
		swapOwner(t, func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{AgentID: "other666", PID: 1, PidStart: 1}, true
		})
		if GracefulSwapEligible(old, true) {
			t.Fatal("want not-eligible when another agent owns the lease")
		}
	})
	t.Run("no healthy owner -> not eligible (dead-coord recovery path)", func(t *testing.T) {
		swapOwner(t, func(string) (coordlock.Owner, bool) { return coordlock.Owner{}, false })
		if GracefulSwapEligible(old, true) {
			t.Fatal("want not-eligible with no healthy lease owner")
		}
	})
	t.Run("auto-resume off -> not eligible (nothing to inject)", func(t *testing.T) {
		swapOwner(t, ownerIsOld)
		if GracefulSwapEligible(old, false) {
			t.Fatal("want not-eligible when auto-resume is off")
		}
	})
	t.Run("failover off -> not eligible", func(t *testing.T) {
		swapOwner(t, ownerIsOld)
		t.Setenv("FLEET_LEASE_FAILOVER", "0")
		if GracefulSwapEligible(old, true) {
			t.Fatal("want not-eligible with FLEET_LEASE_FAILOVER=0")
		}
	})
	t.Run("nil / projectless record -> not eligible", func(t *testing.T) {
		swapOwner(t, func(string) (coordlock.Owner, bool) {
			t.Error("owner consulted for an invalid record")
			return coordlock.Owner{}, false
		})
		if GracefulSwapEligible(nil, true) {
			t.Fatal("want not-eligible for a nil record")
		}
		bare := agent.New("noproj01")
		if GracefulSwapEligible(bare, true) {
			t.Fatal("want not-eligible for a projectless record")
		}
	})
}

// ---------------------------------------------------------------------------
// GracefulCoordSwap — the production composition the live-coord handoff
// paths route through: barrier (doc already durable on disk) → retire OLD
// via AtomicCoordSwap → deliver to the lock winner. Returns the winner.
// ---------------------------------------------------------------------------

func TestGracefulCoordSwap_BarrierThenSwapThenDeliver_ReturnsWinner(t *testing.T) {
	rec := newGracefulRecorder()
	oldRec := oldRecForGraceful()
	newRec := agent.New("standby01")
	newRec.Project = oldRec.Project
	winner := agent.New("winner01")

	origDeps := gracefulSwapDepsFn
	origSwap := atomicCoordSwapFn
	t.Cleanup(func() {
		gracefulSwapDepsFn = origDeps
		atomicCoordSwapFn = origSwap
	})
	gracefulSwapDepsFn = func() GracefulHandoffDeps {
		return GracefulHandoffDeps{
			WriteAtomic:  rec.write,
			CurrentEpoch: func(string) (int64, bool) { return 7, true },
			BarrierPath:  func(string, int64) (string, error) { return gracefulBarrierPath, nil },
		}
	}
	var gotSwap AtomicCoordSwapInputs
	atomicCoordSwapFn = func(in AtomicCoordSwapInputs, _ io.Writer) (AtomicCoordSwapResult, error) {
		rec.note("atomic-swap")
		gotSwap = in
		return AtomicCoordSwapResult{}, nil
	}

	got, err := GracefulCoordSwap(oldRec, newRec, gracefulDocPath,
		1500*time.Millisecond,
		func() (*agent.Record, error) { rec.note("deliver"); return winner, nil },
		io.Discard)
	if err != nil {
		t.Fatalf("GracefulCoordSwap: %v", err)
	}
	if got == nil || got.ID != winner.ID {
		t.Fatalf("winner = %+v, want %s", got, winner.ID)
	}
	order, writes, _ := rec.snapshot()
	iBarrier := orderIdx(order, "write:"+gracefulBarrierPath)
	iSwap := orderIdx(order, "atomic-swap")
	iDeliver := orderIdx(order, "deliver")
	if iBarrier == -1 || iSwap == -1 || iDeliver == -1 ||
		iBarrier >= iSwap || iSwap >= iDeliver {
		t.Fatalf("want barrier < atomic-swap < deliver; order=%v", order)
	}
	// The doc is already durable on disk (the caller wrote it before spawn);
	// GracefulCoordSwap must NOT rewrite it.
	if _, ok := writes[gracefulDocPath]; ok {
		t.Errorf("handoff doc rewritten; it was already durable")
	}
	if gotSwap.Project != oldRec.Project || gotSwap.OldRec != oldRec ||
		gotSwap.AlreadySpawnedNewRec != newRec ||
		gotSwap.GraceWindow != 1500*time.Millisecond {
		t.Errorf("AtomicCoordSwap inputs wrong: %+v", gotSwap)
	}
}

func TestGracefulCoordSwap_SwapError_Propagates_NoDeliver(t *testing.T) {
	rec := newGracefulRecorder()
	oldRec := oldRecForGraceful()
	newRec := agent.New("standby01")
	newRec.Project = oldRec.Project
	wantErr := errors.New("orphan survived")
	deliverCalls := 0

	origDeps := gracefulSwapDepsFn
	origSwap := atomicCoordSwapFn
	t.Cleanup(func() {
		gracefulSwapDepsFn = origDeps
		atomicCoordSwapFn = origSwap
	})
	gracefulSwapDepsFn = func() GracefulHandoffDeps {
		return GracefulHandoffDeps{
			WriteAtomic:  rec.write,
			CurrentEpoch: func(string) (int64, bool) { return 7, true },
			BarrierPath:  func(string, int64) (string, error) { return gracefulBarrierPath, nil },
		}
	}
	atomicCoordSwapFn = func(AtomicCoordSwapInputs, io.Writer) (AtomicCoordSwapResult, error) {
		return AtomicCoordSwapResult{}, wantErr
	}

	got, err := GracefulCoordSwap(oldRec, newRec, gracefulDocPath, 0,
		func() (*agent.Record, error) { deliverCalls++; return nil, nil },
		io.Discard)
	if !errors.Is(err, wantErr) {
		t.Fatalf("swap error not surfaced via errors.Is (callers branch on swap sentinels): %v", err)
	}
	if got != nil {
		t.Errorf("winner = %+v on the error path, want nil", got)
	}
	if deliverCalls != 0 {
		t.Errorf("delivery ran %d times after a swap failure, want 0", deliverCalls)
	}
}
