package fleetlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupLogHome points FLEET_HOME at a fresh tmpdir and clears
// XDG_STATE_HOME (an ambient value would silently redirect Dir()).
func setupLogHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", "")
	return filepath.Join(tmp, "logs")
}

// readLines reads + JSON-decodes every line of every .jsonl in dir.
func readLines(t *testing.T, dir string) []map[string]any {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir %s: %v", dir, err)
	}
	var out []map[string]any
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if ln == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(ln), &m); err != nil {
				t.Fatalf("line %q in %s not valid JSON: %v", ln, e.Name(), err)
			}
			out = append(out, m)
		}
	}
	return out
}

var tsRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)

// assertEnvelope checks a line carries the required envelope keys/types
// and a type from the closed vocabulary.
func assertEnvelope(t *testing.T, m map[string]any) {
	t.Helper()
	for _, k := range []string{"ts", "seq", "type", "lvl", "comp", "pid", "id"} {
		if _, ok := m[k]; !ok {
			t.Errorf("line missing required key %q: %v", k, m)
		}
	}
	ts, _ := m["ts"].(string)
	if !tsRe.MatchString(ts) {
		t.Errorf("ts %q is not RFC3339 microsecond UTC", ts)
	}
	typ, _ := m["type"].(string)
	if !Types[typ] {
		t.Errorf("type %q not in closed vocabulary", typ)
	}
	if id, _ := m["id"].(string); id == "" {
		t.Errorf("id must be non-empty: %v", m)
	}
}

// T1 (Go core): one Log writes a full, valid envelope to its own
// per-process file named fleet-<date>-<comp>-<pid>-<pidstart>.jsonl.
func TestLogWritesEnvelopeToOwnFile(t *testing.T) {
	dir := setupLogHome(t)
	id := Log(CompWorker, "state.transition", "info", Fields{
		Proj: "projects-fleet", Slug: "slug-1",
		Msg:  "worker slug-1 phase tdd-green -> review-pending",
		Data: map[string]any{"from": "tdd-green", "to": "review-pending"},
	})
	if id == "" {
		t.Fatal("Log returned empty id")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 log file, got %d: %v", len(entries), entries)
	}
	nameRe := regexp.MustCompile(`^fleet-\d{4}-\d{2}-\d{2}-worker-\d+-\d+\.jsonl$`)
	if !nameRe.MatchString(entries[0].Name()) {
		t.Errorf("filename %q does not match per-process pattern", entries[0].Name())
	}
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	m := lines[0]
	assertEnvelope(t, m)
	if m["comp"] != "worker" || m["type"] != "state.transition" || m["slug"] != "slug-1" {
		t.Errorf("envelope fields wrong: %v", m)
	}
	data, _ := m["data"].(map[string]any)
	if data["from"] != "tdd-green" || data["to"] != "review-pending" {
		t.Errorf("data wrong: %v", data)
	}
}

// T2 (Go): logging into an unwritable dir returns no error and never
// panics — the caller is unaffected.
func TestLogBestEffortUnwritableDir(t *testing.T) {
	dir := setupLogHome(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	// Must not panic; returns an id regardless.
	if id := Log(CompCLI, "cli.start", "info", Fields{Msg: "x"}); id == "" {
		t.Fatal("Log returned empty id on unwritable dir")
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".jsonl") {
			t.Errorf("nothing should be written to an unwritable dir, found %s", f.Name())
		}
	}
}

// T3 (Go): a data string > 2 KB is truncated with an elision marker; a
// short token-shaped value is written VERBATIM (no scrub — pins "log raw").
func TestLogDataCapAndRawValues(t *testing.T) {
	dir := setupLogHome(t)
	blob := strings.Repeat("A", 3000)
	tok := "ghp_EXAMPLETOKEN0123456789"
	Log(CompCoord, "decision", "info", Fields{
		Msg:  "chose X over Y",
		Data: map[string]any{"blob": blob, "tok": tok},
	})
	lines := readLines(t, dir)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	data, _ := lines[0]["data"].(map[string]any)
	gotBlob, _ := data["blob"].(string)
	if len(gotBlob) > dataCap+len(elision) {
		t.Errorf("blob not capped: len=%d", len(gotBlob))
	}
	if !strings.HasSuffix(gotBlob, elision) {
		t.Errorf("blob missing elision marker: ...%q", gotBlob[len(gotBlob)-20:])
	}
	if data["tok"] != tok {
		t.Errorf("token must be logged verbatim (no scrub), got %v", data["tok"])
	}
}

// T4 (Go): one process, many goroutines — every line independently parses
// (no torn/interleaved record) and all land in this process's single file.
func TestLogConcurrentNoTornLines(t *testing.T) {
	dir := setupLogHome(t)
	const goroutines, perG = 50, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				Log(CompWorker, "tool.call", "debug", Fields{
					Slug: "s", Msg: strings.Repeat("payload-", 20),
					Data: map[string]any{"g": g, "i": i},
				})
			}
		}(g)
	}
	wg.Wait()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("one process must write exactly one file, got %d", len(entries))
	}
	lines := readLines(t, dir) // readLines fails on any torn/invalid line
	if len(lines) != goroutines*perG {
		t.Fatalf("want %d lines, got %d", goroutines*perG, len(lines))
	}
}

// T4 (date rollover): a Log straddling injected UTC midnight lands in the
// next day's file.
func TestLogDateRollover(t *testing.T) {
	dir := setupLogHome(t)
	base := time.Date(2026, 6, 27, 23, 59, 59, 0, time.UTC)
	old := nowFn
	t.Cleanup(func() { nowFn = old })

	nowFn = func() time.Time { return base }
	Log(CompCoord, "coord.tick", "info", Fields{Msg: "before midnight"})
	nowFn = func() time.Time { return base.Add(2 * time.Second) } // 2026-06-28
	Log(CompCoord, "coord.tick", "info", Fields{Msg: "after midnight"})

	want := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		want[e.Name()[:len("fleet-2026-06-27")]] = true
	}
	if !want["fleet-2026-06-27"] || !want["fleet-2026-06-28"] {
		t.Errorf("expected both day files, got %v", want)
	}
}

// T5 (Go): PruneOlderThan deletes >3-day files, keeps recent ones.
func TestPruneOlderThan(t *testing.T) {
	dir := setupLogHome(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	old := nowFn
	t.Cleanup(func() { nowFn = old })
	nowFn = func() time.Time { return now }

	mk := func(date string) string {
		n := "fleet-" + date + "-coord-1-1.jsonl"
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
		return n
	}
	stale := mk("2026-06-23") // 4 days old -> prune
	keep3 := mk("2026-06-24") // exactly 3 days -> keep
	today := mk("2026-06-27") // today -> keep
	other := "notes.txt"      // non-matching -> ignore
	if err := os.WriteFile(filepath.Join(dir, other), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	PruneOlderThan(72 * time.Hour)

	exists := func(n string) bool {
		_, err := os.Stat(filepath.Join(dir, n))
		return err == nil
	}
	if exists(stale) {
		t.Errorf("%s (4d) should be pruned", stale)
	}
	if !exists(keep3) {
		t.Errorf("%s (exactly 3d) should be kept", keep3)
	}
	if !exists(today) {
		t.Errorf("%s (today) should be kept", today)
	}
	if !exists(other) {
		t.Errorf("non-matching %s should be untouched", other)
	}
}

// T8/T14 (Go): XDG_STATE_HOME redirects Dir() to $XDG/fleet/logs.
func TestDirHonorsXDGStateHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", filepath.Join(tmp, "fhome"))
	xdg := filepath.Join(tmp, "xdg")
	t.Setenv("XDG_STATE_HOME", xdg)
	want := filepath.Join(xdg, "fleet", "logs")
	if got := Dir(); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got := Dir(); got != filepath.Join(tmp, "fhome", "logs") {
		t.Errorf("Dir() without XDG = %q, want FLEET_HOME/logs", got)
	}
}

// CLIStart emits cli.start then cli.finish(rc) with argv carrying the
// command name.
func TestCLIStartFinish(t *testing.T) {
	dir := setupLogHome(t)
	finish := CLIStart("drain")
	finish(nil)
	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (start+finish), got %d", len(lines))
	}
	var start, fin map[string]any
	for _, m := range lines {
		switch m["type"] {
		case "cli.start":
			start = m
		case "cli.finish":
			fin = m
		}
	}
	if start == nil || fin == nil {
		t.Fatalf("missing cli.start/cli.finish: %v", lines)
	}
	sd, _ := start["data"].(map[string]any)
	argv, _ := sd["argv"].([]any)
	if len(argv) == 0 || argv[0] != "drain" {
		t.Errorf("cli.start argv must contain drain: %v", sd)
	}
	fd, _ := fin["data"].(map[string]any)
	if _, ok := fd["rc"]; !ok {
		t.Errorf("cli.finish must carry rc: %v", fd)
	}
	if fin["caused_by"] != start["id"] {
		t.Errorf("cli.finish.caused_by %v must equal cli.start.id %v", fin["caused_by"], start["id"])
	}
}
