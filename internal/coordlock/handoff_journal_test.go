//go:build linux || darwin

package coordlock

// handoff_journal_test.go — the write-once handoff journal primitives
// (DESIGN-coord-lease-flock-only Decision 7). These cover the journal in
// isolation; the producer/consumer wiring is tested in internal/handoffop and
// the attach/Resolve path in internal/coordreconcile.
//
//   - Create is O_EXCL (T8 collision primitive) + fsync-durable.
//   - Delete is idempotent and also reaps the barrier (T14 self-heal).
//   - The mid-boot-vs-abandoned discriminator is the successor PROCESS's
//     liveness (pid + pid_start + boot_id), NEVER elapsed time.

import (
	"os"
	"path/filepath"
	"testing"
)

// mkJournal builds a deterministic journal naming a successor whose process
// liveness the caller controls via the fakeLiveness seam (successor boot pinned
// to the testCfg boot id "test-boot-1").
func mkJournal(project, succID, barrierID string, succPID int, succStart int64) HandoffJournal {
	return HandoffJournal{
		Project:           project,
		OldAgentID:        "old01",
		SuccessorID:       succID,
		BarrierID:         barrierID,
		SuccessorPID:      succPID,
		SuccessorPidStart: succStart,
		SuccessorBootID:   "test-boot-1",
	}
}

// TestHandoffJournal_CreateReadDeleteRoundTrip: Create writes O_EXCL; a second
// Create collides with ErrHandoffJournalExists (T8 primitive); Delete is
// idempotent (T14 — a failed first-tick delete self-heals on retry).
func TestHandoffJournal_CreateReadDeleteRoundTrip(t *testing.T) {
	setupHome(t)
	const project = "hj-roundtrip"
	j := mkJournal(project, "succ01", "bar-1", 4242, 222222)

	if err := CreateHandoffJournal(j); err != nil {
		t.Fatalf("CreateHandoffJournal: %v", err)
	}
	got, ok, err := ReadHandoffJournal(project)
	if err != nil || !ok {
		t.Fatalf("ReadHandoffJournal: ok=%v err=%v", ok, err)
	}
	if got.SuccessorID != "succ01" || got.BarrierID != "bar-1" || got.SuccessorPID != 4242 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// T8 primitive: a second Create hits the leftover O_EXCL ⇒ signals the
	// caller to resolve+retry, never blind-overwrites.
	if err := CreateHandoffJournal(mkJournal(project, "succ02", "bar-2", 5555, 333333)); err != ErrHandoffJournalExists {
		t.Fatalf("second Create err = %v, want ErrHandoffJournalExists", err)
	}
	// The on-disk journal is untouched by the failed collision Create.
	if got, _, _ := ReadHandoffJournal(project); got.SuccessorID != "succ01" {
		t.Fatalf("collision Create clobbered the journal: %+v", got)
	}

	if err := DeleteHandoffJournal(project); err != nil {
		t.Fatalf("DeleteHandoffJournal: %v", err)
	}
	if _, ok, _ := ReadHandoffJournal(project); ok {
		t.Fatal("journal still present after Delete")
	}
	// T14: idempotent — a redundant delete (retried each tick) is not an error.
	if err := DeleteHandoffJournal(project); err != nil {
		t.Fatalf("second DeleteHandoffJournal (idempotent): %v", err)
	}
}

// TestHandoffJournal_CreateValidation: the required fields are enforced.
func TestHandoffJournal_CreateValidation(t *testing.T) {
	setupHome(t)
	cases := []struct {
		name string
		j    HandoffJournal
	}{
		{"no_project", HandoffJournal{SuccessorID: "s", BarrierID: "b"}},
		{"no_successor", HandoffJournal{Project: "p", BarrierID: "b"}},
		{"no_barrier", HandoffJournal{Project: "p", SuccessorID: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CreateHandoffJournal(tc.j); err == nil {
				t.Fatalf("CreateHandoffJournal(%+v) = nil, want validation error", tc.j)
			}
		})
	}
}

// TestHandoffJournal_DeleteReapsBarrier (T14): Delete removes the barrier named
// by the journal's own barrier_id, not just the journal file — a
// journal-then-barrier partial delete would orphan a barrier no one can name.
func TestHandoffJournal_DeleteReapsBarrier(t *testing.T) {
	setupHome(t)
	const project = "hj-barrier"
	const barrierID = "bar-xyz"
	if err := CreateHandoffJournal(mkJournal(project, "succ01", barrierID, 4242, 222222)); err != nil {
		t.Fatalf("CreateHandoffJournal: %v", err)
	}
	bp, err := HandoffBarrierPath(project, barrierID)
	if err != nil {
		t.Fatalf("HandoffBarrierPath: %v", err)
	}
	if err := os.WriteFile(bp, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write barrier: %v", err)
	}
	if err := DeleteHandoffJournal(project); err != nil {
		t.Fatalf("DeleteHandoffJournal: %v", err)
	}
	if _, serr := os.Stat(bp); !os.IsNotExist(serr) {
		t.Fatalf("barrier not reaped by Delete: stat err=%v", serr)
	}
}

// TestHandoffBarrierPath: named by the barrier_id; empty id is rejected.
func TestHandoffBarrierPath(t *testing.T) {
	setupHome(t)
	if _, err := HandoffBarrierPath("p", ""); err == nil {
		t.Fatal("empty barrier id must be rejected")
	}
	p, err := HandoffBarrierPath("p", "bar-1")
	if err != nil {
		t.Fatalf("HandoffBarrierPath: %v", err)
	}
	if filepath.Base(p) != "handoff-complete-bar-1.json" {
		t.Fatalf("barrier path base = %q, want handoff-complete-bar-1.json", filepath.Base(p))
	}
}

// TestHandoffSuccessorAlive: the mid-boot-vs-abandoned discriminator is pid +
// pid_start + boot_id — never a timer.
func TestHandoffSuccessorAlive(t *testing.T) {
	const succPID, succStart = 4242, int64(222222)
	base := mkJournal("p", "succ01", "bar-1", succPID, succStart)
	cases := []struct {
		name string
		mut  func(*HandoffJournal)
		live func(*fakeLiveness)
		want bool
	}{
		{"alive_same_boot", nil, func(l *fakeLiveness) { l.set(succPID, succStart) }, true},
		{"dead_pid_gone", nil, func(l *fakeLiveness) {}, false},
		{"recycled_pid_start_mismatch", nil, func(l *fakeLiveness) { l.set(succPID, 999999) }, false},
		{"cross_boot", func(j *HandoffJournal) { j.SuccessorBootID = "OTHER-BOOT" }, func(l *fakeLiveness) { l.set(succPID, succStart) }, false},
		{"no_pid", func(j *HandoffJournal) { j.SuccessorPID = 0 }, func(l *fakeLiveness) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := base
			if tc.mut != nil {
				tc.mut(&j)
			}
			live := newFakeLiveness()
			tc.live(live)
			cfg := testCfg(&fakeClock{}, live)
			if got := handoffSuccessorAliveWithCfg(j, cfg); got != tc.want {
				t.Fatalf("handoffSuccessorAliveWithCfg = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyFreeFlockHandoff: none / in-flight (alive) / abandoned (dead) /
// torn (read error). The caller has already established the flock is FREE.
func TestClassifyFreeFlockHandoff(t *testing.T) {
	const succPID, succStart = 4242, int64(222222)

	t.Run("no_journal_is_none", func(t *testing.T) {
		setupHome(t)
		cfg := testCfg(&fakeClock{}, newFakeLiveness())
		disp, _, err := classifyFreeFlockHandoffWithCfg("hj-none", cfg)
		if err != nil || disp != HandoffNone {
			t.Fatalf("disp=%v err=%v, want HandoffNone", disp, err)
		}
	})

	t.Run("alive_is_in_flight", func(t *testing.T) {
		setupHome(t)
		const project = "hj-inflight"
		if err := CreateHandoffJournal(mkJournal(project, "succ01", "bar-1", succPID, succStart)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		live := newFakeLiveness()
		live.set(succPID, succStart)
		disp, j, err := classifyFreeFlockHandoffWithCfg(project, testCfg(&fakeClock{}, live))
		if err != nil || disp != HandoffInFlight || j.SuccessorID != "succ01" {
			t.Fatalf("disp=%v id=%q err=%v, want HandoffInFlight succ01", disp, j.SuccessorID, err)
		}
	})

	t.Run("dead_is_abandoned", func(t *testing.T) {
		setupHome(t)
		const project = "hj-abandoned"
		if err := CreateHandoffJournal(mkJournal(project, "succ01", "bar-1", succPID, succStart)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// successor NOT in liveness ⇒ dead.
		disp, _, err := classifyFreeFlockHandoffWithCfg(project, testCfg(&fakeClock{}, newFakeLiveness()))
		if err != nil || disp != HandoffAbandoned {
			t.Fatalf("disp=%v err=%v, want HandoffAbandoned", disp, err)
		}
	})

	t.Run("torn_journal_errors", func(t *testing.T) {
		setupHome(t)
		const project = "hj-torn"
		path, err := handoffJournalPath(project)
		if err != nil {
			t.Fatalf("handoffJournalPath: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := classifyFreeFlockHandoffWithCfg(project, testCfg(&fakeClock{}, newFakeLiveness())); err == nil {
			t.Fatal("torn journal must surface a read error (caller preserves the queue on ambiguity)")
		}
	})
}

// TestCurrentHandoffSuccessor: names the successor only while its process is
// alive (the flock-only replacement for the epoch's CurrentHandoff). Uses the
// real pid probe (this test process is provably alive).
func TestCurrentHandoffSuccessor(t *testing.T) {
	selfStart, ok := PidStartNanos(os.Getpid())
	if !ok {
		t.Skip("cannot read own pid_start")
	}

	t.Run("alive_names_successor", func(t *testing.T) {
		setupHome(t)
		const project = "chs-alive"
		j := mkJournal(project, "succ01", "bar-1", os.Getpid(), selfStart)
		j.SuccessorBootID = "" // let Create stamp this boot so the real probe matches
		if err := CreateHandoffJournal(j); err != nil {
			t.Fatalf("Create: %v", err)
		}
		id, ok := CurrentHandoffSuccessor(project)
		if !ok || id != "succ01" {
			t.Fatalf("CurrentHandoffSuccessor = %q ok=%v, want succ01", id, ok)
		}
	})

	t.Run("dead_names_nothing", func(t *testing.T) {
		setupHome(t)
		const project = "chs-dead"
		// A pid that is (almost certainly) not this test's live successor.
		if err := CreateHandoffJournal(mkJournal(project, "succ01", "bar-1", 999999, 424242)); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if id, ok := CurrentHandoffSuccessor(project); ok {
			t.Fatalf("CurrentHandoffSuccessor named %q for a dead successor, want ok=false", id)
		}
	})

	t.Run("no_journal_names_nothing", func(t *testing.T) {
		setupHome(t)
		if id, ok := CurrentHandoffSuccessor("chs-none"); ok {
			t.Fatalf("CurrentHandoffSuccessor named %q with no journal, want ok=false", id)
		}
	})
}
