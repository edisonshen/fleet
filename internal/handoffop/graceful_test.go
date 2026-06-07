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
	"strings"
	"sync"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
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
	in := GracefulHandoffInputs{
		OldRec:         oldRecForGraceful(),
		HandoffDocPath: gracefulDocPath,
		HandoffDoc:     []byte("# handoff doc\n"),
		CheckpointPath: gracefulCheckpointPath,
		Checkpoint:     []byte(`{"state":"snapshot"}`),
	}
	deps := GracefulHandoffDeps{
		SpawnStandby: func() error {
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
		SpawnStandby: func() error { rec.standbyRuns++; return nil },
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
		SpawnStandby:  func() error { return nil },
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
		SpawnStandby: func() error { return wantErr },
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
		SpawnStandby: func() error { rec.standbyRuns++; return nil },
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
