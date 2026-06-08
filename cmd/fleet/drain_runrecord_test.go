package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// readDrainRunRecord reads this-process's run-record from
// ~/.fleet/drain-runs/<pid>.json, or fails the test if absent.
func readDrainRunRecord(t *testing.T) (drainRunRecord, string) {
	t.Helper()
	dir, err := state.DrainRunsDir()
	if err != nil {
		t.Fatalf("DrainRunsDir: %v", err)
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read run-record %s: %v", path, rerr)
	}
	var rec drainRunRecord
	if jerr := json.Unmarshal(data, &rec); jerr != nil {
		t.Fatalf("unmarshal run-record: %v", jerr)
	}
	return rec, path
}

func drainRunRecordExists(t *testing.T) bool {
	t.Helper()
	dir, err := state.DrainRunsDir()
	if err != nil {
		t.Fatalf("DrainRunsDir: %v", err)
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid())+".json")
	_, serr := os.Stat(path)
	return serr == nil
}

// TDR1 — drain writes a run-record with correct pid_start on start and
// DELETES it on clean Stop (the deferred-cleanup happy path).
func TestDrainRunRecord_TDR1_WriteThenDeleteOnStop(t *testing.T) {
	setupFleetHome(t)
	// Pin the proc-start fingerprint deterministically (no real `ps`).
	prevStart := drainProcStartFn
	drainProcStartFn = func(int) string { return "FINGERPRINT-1" }
	t.Cleanup(func() { drainProcStartFn = prevStart })

	h, err := startDrainRunRecord(0)
	if err != nil {
		t.Fatalf("startDrainRunRecord: %v", err)
	}
	t.Cleanup(h.Stop) // belt: even if an assert fails mid-test

	rec, _ := readDrainRunRecord(t)
	if rec.Pid != os.Getpid() {
		t.Fatalf("run-record pid = %d, want %d", rec.Pid, os.Getpid())
	}
	if rec.PidStart != "FINGERPRINT-1" {
		t.Fatalf("run-record pid_start = %q, want FINGERPRINT-1", rec.PidStart)
	}
	if rec.HeartbeatAt.IsZero() || rec.StartedAt.IsZero() {
		t.Fatalf("run-record timestamps must be set: %+v", rec)
	}

	h.Stop()
	if drainRunRecordExists(t) {
		t.Fatalf("run-record must be DELETED on Stop (deferred cleanup)")
	}
	// Stop is idempotent — a second call must not panic / error.
	h.Stop()
}

// TDR2 — cleanup runs on the failure path. We simulate runDrain hitting
// an error after the record was written: the deferred Stop still deletes
// the record. Modeled by writing the record, then invoking Stop from a
// deferred closure inside a function that "fails".
func TestDrainRunRecord_TDR2_DeleteOnFailurePath(t *testing.T) {
	setupFleetHome(t)
	prevStart := drainProcStartFn
	drainProcStartFn = func(int) string { return "FP" }
	t.Cleanup(func() { drainProcStartFn = prevStart })

	func() {
		h, err := startDrainRunRecord(0)
		if err != nil {
			t.Fatalf("startDrainRunRecord: %v", err)
		}
		defer h.Stop() // the LAST step — must run even though we "fail" below
		if !drainRunRecordExists(t) {
			t.Fatalf("record should exist during the run")
		}
		// Simulate the drain failing partway through (return early). The
		// deferred Stop runs on this failure path.
	}()

	if drainRunRecordExists(t) {
		t.Fatalf("run-record must be deleted by the deferred Stop on the failure path")
	}
}

// TDR3 — the heartbeat goroutine refreshes heartbeat_at while running.
// Beat() advances heartbeat_at to the current (seamed) clock; the
// PROGRESS model means a drain that does NOT call Beat (wedged inside a
// blocking section) leaves heartbeat_at frozen → the gc reaper catches
// it. Deterministic via the drainRunNow seam (no Sleep-timing).
func TestDrainRunRecord_BeatAdvances_NoBeatFreezes(t *testing.T) {
	setupFleetHome(t)
	prevStart := drainProcStartFn
	drainProcStartFn = func(int) string { return "fp" }
	t.Cleanup(func() { drainProcStartFn = prevStart })
	prevNow := drainRunNow
	t.Cleanup(func() { drainRunNow = prevNow })

	t0 := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	drainRunNow = func() time.Time { return t0 }

	h, err := startDrainRunRecord(0)
	if err != nil {
		t.Fatalf("startDrainRunRecord: %v", err)
	}
	t.Cleanup(h.Stop)

	rec, _ := readDrainRunRecord(t)
	if !rec.HeartbeatAt.Equal(t0) {
		t.Fatalf("initial heartbeat = %v, want %v", rec.HeartbeatAt, t0)
	}

	// A progress checkpoint at t0+30s advances the heartbeat.
	t1 := t0.Add(30 * time.Second)
	drainRunNow = func() time.Time { return t1 }
	h.Beat()
	rec, _ = readDrainRunRecord(t)
	if !rec.HeartbeatAt.Equal(t1) {
		t.Fatalf("after Beat heartbeat = %v, want %v", rec.HeartbeatAt, t1)
	}
	if !rec.StartedAt.Equal(t0) {
		t.Fatalf("started_at must stay fixed across Beat; got %v", rec.StartedAt)
	}

	// Clock keeps advancing but NO Beat (drain wedged) → heartbeat frozen
	// at t1, so it will go stale past the gc TTL.
	drainRunNow = func() time.Time { return t1.Add(10 * time.Minute) }
	rec, _ = readDrainRunRecord(t)
	if !rec.HeartbeatAt.Equal(t1) {
		t.Fatalf("without Beat the heartbeat must stay frozen at %v; got %v (a timer would have advanced it — that's the bug this guards)", t1, rec.HeartbeatAt)
	}
}

// Beat on a nil handle (start-record write failed) is a safe no-op.
func TestDrainRunRecord_BeatNilSafe(t *testing.T) {
	var h *drainRunHandle
	h.Beat() // must not panic
	h.Stop() // must not panic
}

// Empty fingerprint (codex iter-11 [P2]): if pid_start can't be captured,
// startDrainRunRecord refuses to write a reuse-unsafe record — it returns
// an error (non-fatal at the call site) and writes NO file.
func TestDrainRunRecord_EmptyFingerprint_NoRecord(t *testing.T) {
	setupFleetHome(t)
	prevStart := drainProcStartFn
	drainProcStartFn = func(int) string { return "" } // capture failed
	t.Cleanup(func() { drainProcStartFn = prevStart })

	h, err := startDrainRunRecord(0)
	if err == nil {
		t.Fatal("empty fingerprint must return an error (skip reuse-unsafe record)")
	}
	if h != nil {
		t.Fatal("no handle should be returned when the record is skipped")
	}
	if drainRunRecordExists(t) {
		t.Fatal("no run-record file may be written with an empty fingerprint")
	}
}

// GraceMillis is recorded so the reaper can extend the TTL (codex iter-11
// [P2]).
func TestDrainRunRecord_GraceRecorded(t *testing.T) {
	setupFleetHome(t)
	prevStart := drainProcStartFn
	drainProcStartFn = func(int) string { return "fp" }
	t.Cleanup(func() { drainProcStartFn = prevStart })

	h, err := startDrainRunRecord(600000) // 10min grace
	if err != nil {
		t.Fatalf("startDrainRunRecord: %v", err)
	}
	t.Cleanup(h.Stop)
	rec, _ := readDrainRunRecord(t)
	if rec.GraceMillis != 600000 {
		t.Fatalf("grace_ms = %d, want 600000", rec.GraceMillis)
	}
	// The drain's PID-resolve budget is recorded so the reaper derives the
	// TTL from THIS drain's env, not the gc process's (codex iter-14 [P2]).
	if rec.PidResolveMillis <= 0 {
		t.Fatalf("pid_resolve_ms must be recorded (>0); got %d", rec.PidResolveMillis)
	}
}
