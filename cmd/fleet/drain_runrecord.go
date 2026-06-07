package main

// drain_runrecord.go — the `fleet drain` run-record lifecycle
// (DESIGN-handoff-drain-storm-leak.md §3.D / impl item 8). Each live
// drain writes a tiny record ~/.fleet/drain-runs/<pid>.json on start,
// heartbeats it while running, and DELETES it on exit. The gc
// KindDrainProcs classifier keys off the record's heartbeat to prove a
// drain is wedged (vs. inferring it from raw `ps` state, which can kill
// a legitimate long recovery).
//
//	start  ──▶ write <pid>.json {pid, pid_start, started_at, heartbeat_at}
//	   │       spawn heartbeat goroutine (refresh heartbeat_at every ~Ns)
//	   ▼
//	run drain work ──────────────────────────────────────────────┐
//	   │                                                          │
//	   ▼  defer (runs on happy AND failure/panic path):           │
//	stop heartbeat ──▶ delete <pid>.json  ◀───────────────────────┘
//
// The delete is DEFERRED so it runs on the failure path too
// (fleet-owns-its-resources: cleanup is the LAST step, via defer, on
// every exit). A drain that crashes hard (SIGKILL) can't run the defer —
// that is exactly the leaked-record case the gc classifier reaps when
// the heartbeat goes stale.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// drainRunRecord is the on-disk shape. Mirrors gc.DrainRun's serialized
// fields (the gc package reads what this writes). pid_start is the
// process start-time fingerprint so the reaper is PID-reuse-safe.
type drainRunRecord struct {
	Pid         int       `json:"pid"`
	PidStart    string    `json:"pid_start"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	// GraceMillis is the configured /exit→Kill grace window for this drain.
	// The reaper adds it to the per-record effective TTL so a legitimately
	// long grace sleep inside Resume doesn't look wedged (codex iter-11 [P2]).
	GraceMillis int `json:"grace_ms,omitempty"`
}

// drainRunHandle owns one live run-record. Beat() advances heartbeat_at
// from a PROGRESS point; Stop() (idempotent) deletes the record.
//
// PROGRESS-driven, NOT a free-running timer (codex iter-5 [P2]): a
// background goroutine ticking the heartbeat would keep a drain "fresh"
// even while its MAIN thread is wedged forever inside a blocking
// LockAgent / tmux call — exactly the incident this reaper exists to
// catch. By advancing heartbeat_at only at the work loop's progress
// checkpoints (between queue files, where the producer is provably
// making forward progress), a drain stuck inside the long blocking
// section stops beating, its heartbeat goes stale past the gc TTL, and
// the steady-state classifier reaps it. The heartbeat reflects PROGRESS,
// not mere liveness.
type drainRunHandle struct {
	path string
	rec  drainRunRecord
	once sync.Once
}

// drainRunNow is the clock seam (tests pin it). Production = time.Now.
var drainRunNow = time.Now

// drainProcStartFn reads the running process's start-time fingerprint.
// Seam for tests (production = procStartTimeForSelf). Returns "" when
// unreadable — the record still serializes (the reaper falls back to a
// bare liveness probe for an empty fingerprint).
var drainProcStartFn = procStartTimeForSelf

// startDrainRunRecord writes ~/.fleet/drain-runs/<pid>.json with the
// initial heartbeat. graceMillis is the per-drain /exit→Kill grace window
// (recorded so the reaper can extend this drain's effective TTL past a
// legitimately-long configured grace — codex iter-11 [P2]). Returns a
// handle whose Beat() advances the heartbeat at progress points and whose
// Stop() deletes the record.
//
// A write failure is returned but NON-fatal at the call site (the drain
// still runs; it just isn't gc-reapable via the record path). An EMPTY
// start fingerprint is treated the SAME way (codex iter-11 [P2]): a
// reuse-unsafe record (empty pid_start → reaper falls back to bare
// liveness) is worse than no record, so we refuse to write one and return
// a nil handle (Beat/Stop no-op) — the drain runs un-reapable-via-record,
// exactly as on a write failure.
func startDrainRunRecord(graceMillis int) (*drainRunHandle, error) {
	dir, err := state.DrainRunsDir()
	if err != nil {
		return nil, err
	}
	pid := os.Getpid()
	fp := drainProcStartFn(pid)
	if fp == "" {
		// No reuse-safe identity — do not create a kill-target record.
		return nil, fmt.Errorf("drain run-record: could not capture pid_start fingerprint for pid %d (skipping reuse-unsafe record)", pid)
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, mkErr)
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	now := drainRunNow()
	rec := drainRunRecord{
		Pid:         pid,
		PidStart:    fp,
		StartedAt:   now,
		HeartbeatAt: now,
		GraceMillis: graceMillis,
	}
	if werr := writeDrainRunRecord(path, rec); werr != nil {
		return nil, werr
	}
	return &drainRunHandle{path: path, rec: rec}, nil
}

// Beat advances heartbeat_at to now and rewrites the record. Call it
// from the work loop's PROGRESS checkpoints only (NOT a timer) so a drain
// wedged inside a blocking section cannot keep itself fresh. Best-effort:
// a write error is ignored — a persistently-failing write just freezes
// the heartbeat, which the gc classifier reaps (fail-safe). No-op on a
// nil handle (the start-record write failed; the drain still runs).
func (h *drainRunHandle) Beat() {
	if h == nil {
		return
	}
	h.rec.HeartbeatAt = drainRunNow()
	_ = writeDrainRunRecord(h.path, h.rec)
}

// Stop deletes the run-record. Idempotent (safe to call from a defer even
// if the caller also calls it explicitly). The delete is ENOENT-tolerant.
// This is the LAST cleanup step of the drain orchestration and runs on
// the failure path via defer.
func (h *drainRunHandle) Stop() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		if err := os.Remove(h.path); err != nil && !os.IsNotExist(err) {
			// Best-effort: a failed delete leaves a record whose heartbeat
			// is now frozen → the gc classifier reaps it on the next sweep.
			_ = err
		}
	})
}

// writeDrainRunRecord atomically writes the record (.tmp → rename) so a
// concurrent gc read never sees a torn file.
func writeDrainRunRecord(path string, rec drainRunRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal drain run-record: %w", err)
	}
	tmp := path + ".tmp"
	if werr := os.WriteFile(tmp, data, 0o644); werr != nil {
		return fmt.Errorf("write %s: %w", tmp, werr)
	}
	if rerr := os.Rename(tmp, path); rerr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", path, rerr)
	}
	return nil
}

// procStartTimeForSelf reads this process's start fingerprint. MUST stay
// byte-identical to internal/gc's procStartFingerprint so the reaper's
// comparison holds: prefer the Linux sub-second /proc/<pid>/stat
// starttime ("linux-stat:"), fall back to `ps lstart` ("lstart:") on
// darwin. The Linux source defeats same-second PID reuse (codex [P2]).
// "" on any probe failure (the reaper then falls back to a bare liveness
// probe for an empty fingerprint).
func procStartTimeForSelf(pid int) string {
	if hi, ok := linuxProcStarttimeSelf(pid); ok {
		return "linux-stat:" + hi
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return "lstart:" + s
}

// linuxProcStarttimeSelf mirrors internal/gc.linuxProcStarttime — field
// 22 (starttime) of /proc/<pid>/stat. (value, false) on any non-Linux /
// read / parse failure.
func linuxProcStarttimeSelf(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	s := string(data)
	closeParen := strings.LastIndexByte(s, ')')
	if closeParen < 0 || closeParen+2 >= len(s) {
		return "", false
	}
	fields := strings.Fields(s[closeParen+2:])
	if len(fields) < 20 {
		return "", false
	}
	st := fields[19]
	if _, perr := strconv.ParseUint(st, 10, 64); perr != nil {
		return "", false
	}
	return st, true
}
