package watchchan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newChan builds a Channel rooted at a fresh temp dir.
func newChan(t *testing.T) (*Channel, string) {
	t.Helper()
	dir := t.TempDir()
	c := New(dir)
	return c, filepath.Join(dir, DirName)
}

func readMsgFile(t *testing.T, path string) Message {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

// Test 1 — multi-producer no-clobber (one process): 50 goroutines append
// concurrently → 50 distinct files, all parse, none lost.
func TestAppendEvent_MultiProducerNoClobber(t *testing.T) {
	c, msgsDir := newChan(t)
	const n = 50
	var wg sync.WaitGroup
	paths := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := c.AppendEvent(Message{
				Kind:      KindWorkerDelta,
				WatcherID: WatcherID("w"),
				Key:       fmt.Sprintf("worker-%d-EXIT", i),
			})
			paths[i], errs[i] = p, err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(msgsDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	got := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			got++
		}
	}
	if got != n {
		t.Fatalf("want %d event files, got %d (clobber/loss)", n, got)
	}
	// every path is distinct
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Fatalf("duplicate filename %s — clobber", p)
		}
		seen[p] = true
	}
}

// Test 2 — same-nanos tie within a process: two appends forced to the same
// unix_nanos get distinct seq → two files, no clobber.
func TestAppendEvent_SameNanosDistinctSeq(t *testing.T) {
	c, msgsDir := newChan(t)
	restore := nowNanos
	nowNanos = func() int64 { return 1700000000000000000 }
	defer func() { nowNanos = restore }()

	p1, err := c.AppendEvent(Message{Kind: KindWorkerDelta, WatcherID: WatcherID("w"), Key: "a"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.AppendEvent(Message{Kind: KindWorkerDelta, WatcherID: WatcherID("w"), Key: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("same-nanos appends clobbered: %s", p1)
	}
	entries, _ := os.ReadDir(msgsDir)
	cnt := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			cnt++
		}
	}
	if cnt != 2 {
		t.Fatalf("want 2 files for same-nanos distinct-seq, got %d", cnt)
	}
}

// Test 3 — cross-process no-clobber: two watcher_ids (distinct pids), same
// unix_nanos, same seq → distinct filenames; watcher_id carries the pid.
func TestEventFilename_CrossProcessDisambiguation(t *testing.T) {
	const nanos int64 = 1700000000000000000
	const seq uint64 = 0
	idA := "agent-111" // pid 111
	idB := "agent-222" // pid 222
	fa := eventFilename(nanos, idA, seq)
	fb := eventFilename(nanos, idB, seq)
	if fa == fb {
		t.Fatalf("cross-process same nanos+seq must differ: %s == %s", fa, fb)
	}
	// WatcherID embeds this process's pid.
	id := WatcherID("base")
	if !strings.Contains(id, fmt.Sprintf("%d", os.Getpid())) {
		t.Fatalf("WatcherID %q must embed pid %d", id, os.Getpid())
	}
}

// Test 7 — heartbeat coalescing + watcher restart: 100 probes for one
// worker (incl. a simulated pid change) → exactly one hb-<worker>.json with
// the latest epoch, never 100 files.
func TestWriteHeartbeat_CoalescesAcrossWatcherRestart(t *testing.T) {
	c, msgsDir := newChan(t)
	const worker = "wkr1"
	var lastEpoch int64
	for i := 0; i < 100; i++ {
		// simulate a watcher restart at i==50 by switching watcher_id.
		wid := "watcherA-100"
		if i >= 50 {
			wid = "watcherB-200"
		}
		lastEpoch = int64(i)
		_, err := c.WriteHeartbeat(worker, Message{
			Kind:      KindHeartbeat,
			WatcherID: wid,
			EpochID:   lastEpoch,
			Key:       "hb-" + worker,
		})
		if err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	hbs := heartbeatFiles(t, msgsDir)
	if len(hbs) != 1 {
		t.Fatalf("want exactly 1 heartbeat file (coalesced), got %d: %v", len(hbs), hbs)
	}
	m := readMsgFile(t, hbs[0])
	if m.EpochID != lastEpoch {
		t.Fatalf("heartbeat epoch want %d (latest), got %d", lastEpoch, m.EpochID)
	}
	if filepath.Base(hbs[0]) != "hb-"+worker+".json" {
		t.Fatalf("heartbeat filename want hb-%s.json, got %s", worker, filepath.Base(hbs[0]))
	}
}

// Test 8 — stale-heartbeat reap: RemoveHeartbeat reaps on worker-gone;
// ReapOrphanHeartbeats reaps any hb whose worker is not live.
func TestHeartbeatReap(t *testing.T) {
	c, msgsDir := newChan(t)
	for _, w := range []string{"alive", "dead", "orphan"} {
		if _, err := c.WriteHeartbeat(w, Message{Kind: KindHeartbeat, WatcherID: "wa-1", Key: "hb-" + w}); err != nil {
			t.Fatal(err)
		}
	}
	// direct reap of a known-gone worker
	if err := c.RemoveHeartbeat("dead"); err != nil {
		t.Fatalf("RemoveHeartbeat: %v", err)
	}
	// gc backstop: only "alive" is live → orphan reaped too
	removed, err := c.ReapOrphanHeartbeats(map[string]bool{"alive": true})
	if err != nil {
		t.Fatalf("ReapOrphanHeartbeats: %v", err)
	}
	if len(removed) != 1 || filepath.Base(removed[0]) != "hb-orphan.json" {
		t.Fatalf("want orphan reaped, got %v", removed)
	}
	hbs := heartbeatFiles(t, msgsDir)
	if len(hbs) != 1 || filepath.Base(hbs[0]) != "hb-alive.json" {
		t.Fatalf("only hb-alive should remain, got %v", hbs)
	}
}

// Test 9 — cap drops oldest events with a log line each; heartbeats are
// exempt (not counted, never dropped).
func TestCap_DropsOldestEventsHeartbeatsExempt(t *testing.T) {
	dir := t.TempDir()
	c := New(dir)
	c.cap = 5
	var logged []string
	c.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	msgsDir := filepath.Join(dir, DirName)

	// stream heartbeats for several workers (these must survive the cap).
	for _, w := range []string{"a", "b", "c"} {
		if _, err := c.WriteHeartbeat(w, Message{Kind: KindHeartbeat, WatcherID: "w-1", Key: "hb"}); err != nil {
			t.Fatal(err)
		}
	}
	// append 8 events; cap is 5 → 3 oldest dropped, 3 log lines.
	for i := 0; i < 8; i++ {
		if _, err := c.AppendEvent(Message{Kind: KindWorkerDelta, WatcherID: WatcherID("w"), Key: fmt.Sprintf("e%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	events := eventFilesIn(t, msgsDir)
	if len(events) != 5 {
		t.Fatalf("cap=5: want 5 events remaining, got %d", len(events))
	}
	if len(logged) != 3 {
		t.Fatalf("want 3 drop log lines, got %d: %v", len(logged), logged)
	}
	hbs := heartbeatFiles(t, msgsDir)
	if len(hbs) != 3 {
		t.Fatalf("heartbeats must be cap-exempt: want 3, got %d", len(hbs))
	}
}

// Test 11 — concurrent heartbeat writers (two processes): always parses,
// exactly one hb file, surviving epoch is one of the writers'.
func TestWriteHeartbeat_ConcurrentWritersNoTear(t *testing.T) {
	c, msgsDir := newChan(t)
	const worker = "wkr"
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.WriteHeartbeat(worker, Message{
				Kind:      KindHeartbeat,
				WatcherID: fmt.Sprintf("w-%d", i),
				EpochID:   int64(i),
				Key:       "hb-" + worker,
			})
		}(i)
	}
	wg.Wait()
	hbs := heartbeatFiles(t, msgsDir)
	if len(hbs) != 1 {
		t.Fatalf("want exactly 1 hb file under concurrency, got %d", len(hbs))
	}
	m := readMsgFile(t, hbs[0]) // must not be torn
	if m.EpochID < 0 || m.EpochID >= n {
		t.Fatalf("surviving epoch %d out of writer range", m.EpochID)
	}
}

// Test 10 (Go half) — versioned wire + pr_event schema. AppendEvent a
// rebase-shaped pr_event, reparse, assert every field + payload keys.
func TestAppendEvent_PREventSchema(t *testing.T) {
	c, _ := newChan(t)
	p, err := c.AppendEvent(Message{
		Kind:      KindPREvent,
		WatcherID: WatcherID("watch"),
		EpochID:   7,
		Key:       "pr-239-deadbeef-BEHIND",
		Payload: map[string]any{
			"head_sha":   "deadbeef",
			"base_sha":   "cafef00d",
			"event":      "BEHIND",
			"detail_key": "rebase",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// re-read as a generic map to assert exact wire keys.
	data, _ := os.ReadFile(p)
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "kind", "watcher_id", "epoch_id", "key", "payload", "ts"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("wire missing key %q: %v", k, raw)
		}
	}
	if raw["v"].(float64) != float64(SchemaVersion) {
		t.Fatalf("v want %d, got %v", SchemaVersion, raw["v"])
	}
	pl := raw["payload"].(map[string]any)
	for _, k := range []string{"head_sha", "base_sha", "event", "detail_key"} {
		if _, ok := pl[k]; !ok {
			t.Fatalf("payload missing %q: %v", k, pl)
		}
	}
}

// TestGoldenFixtureParses — the committed cross-language golden fixture
// (also read by the Python suite) must parse into Message with all fields.
func TestGoldenFixtureParses(t *testing.T) {
	golden := filepath.Join("..", "..", "skills", "coordinator", "tests", "testdata", "pr_event_golden.json")
	data, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("golden parse: %v", err)
	}
	if m.Kind != KindPREvent || m.V != SchemaVersion {
		t.Fatalf("golden shape: kind=%q v=%d", m.Kind, m.V)
	}
	if m.Payload["base_sha"] == nil || m.Payload["head_sha"] == nil {
		t.Fatalf("golden payload missing base/head sha: %v", m.Payload)
	}
}

// --- test helpers ---

func heartbeatFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, HeartbeatPrefix) && strings.HasSuffix(n, ".json") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

func eventFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, HeartbeatPrefix) {
			continue
		}
		if strings.HasSuffix(n, ".json") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}
