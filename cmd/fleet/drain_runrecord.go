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

// drainHeartbeatInterval is how often the run-record's heartbeat_at is
// refreshed. Must be well under the gc classifier's TTL
// (gc.drainHeartbeatTTL = 2min) so a healthy drain never looks stale.
const drainHeartbeatInterval = 10 * time.Second

// drainRunRecord is the on-disk shape. Mirrors gc.DrainRun's serialized
// fields (the gc package reads what this writes). pid_start is the
// process start-time fingerprint so the reaper is PID-reuse-safe.
type drainRunRecord struct {
	Pid         int       `json:"pid"`
	PidStart    string    `json:"pid_start"`
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
}

// drainRunHandle owns one live run-record + its heartbeat goroutine.
// Stop() (idempotent) stops the heartbeat and deletes the record.
type drainRunHandle struct {
	path string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// drainRunNow is the clock seam (tests pin it). Production = time.Now.
var drainRunNow = time.Now

// drainProcStartFn reads the running process's start-time fingerprint.
// Seam for tests (production = procStartTimeForSelf). Returns "" when
// unreadable — the record still serializes (the reaper falls back to a
// bare liveness probe for an empty fingerprint).
var drainProcStartFn = procStartTimeForSelf

// startDrainRunRecord writes ~/.fleet/drain-runs/<pid>.json and starts
// the heartbeat goroutine. Returns a handle whose Stop() deletes the
// record. A write failure is returned but is NON-fatal at the call site
// (the drain still runs; it just isn't gc-reapable via the record path).
func startDrainRunRecord() (*drainRunHandle, error) {
	dir, err := state.DrainRunsDir()
	if err != nil {
		return nil, err
	}
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, mkErr)
	}
	pid := os.Getpid()
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	now := drainRunNow()
	rec := drainRunRecord{
		Pid:         pid,
		PidStart:    drainProcStartFn(pid),
		StartedAt:   now,
		HeartbeatAt: now,
	}
	if werr := writeDrainRunRecord(path, rec); werr != nil {
		return nil, werr
	}
	h := &drainRunHandle{
		path: path,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go h.heartbeatLoop(rec)
	return h, nil
}

// heartbeatLoop refreshes heartbeat_at every drainHeartbeatInterval
// until Stop() closes h.stop. Best-effort: a write error is ignored (the
// next tick retries; a persistently-failing write surfaces as a stale
// heartbeat the gc classifier reaps — fail-safe).
func (h *drainRunHandle) heartbeatLoop(rec drainRunRecord) {
	defer close(h.done)
	t := time.NewTicker(drainHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			rec.HeartbeatAt = drainRunNow()
			_ = writeDrainRunRecord(h.path, rec)
		}
	}
}

// Stop halts the heartbeat and deletes the run-record. Idempotent (safe
// to call from a defer even if the caller also calls it explicitly). The
// delete is ENOENT-tolerant. This is the LAST cleanup step of the drain
// orchestration and runs on the failure path via defer.
func (h *drainRunHandle) Stop() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		close(h.stop)
		<-h.done // wait for the heartbeat goroutine to exit (no orphan writes)
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
