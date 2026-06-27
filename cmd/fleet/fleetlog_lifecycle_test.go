package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/fleetlog"
	"github.com/edisonshen/fleet/internal/workers"
)

// TestFleetlogLifecycle is the centerpiece integration test (TASK-PLAN T1).
// It drives ONE of each REAL trigger across both languages, then asserts the
// whole debug-log lifecycle in one place:
//
//  1. a real loop.py coord tick that dispatches a ready task
//     (Python emitter -> coord.tick start+end, dispatch.worker)
//  2. workers.WriteState(slug, review-pending)
//     (Go worker emitter -> state.transition)
//  3. the wired `fleet drain` command
//     (Go CLI emitter -> cli.start + cli.finish)
//
// Assertions: (1) every line validates against the envelope schema; (2) the
// line count matches the triggered events and carries the right correlation
// keys; (3) lines live under fleetlog.Dir() in per-process files named
// fleet-<date>-<comp>-<pid>-<pidstart>.jsonl with the right comp per source;
// (4) the Python (coord) and Go (worker/CLI) lines are all present and a
// single jq reads every line — proving cross-language byte parity.
func TestFleetlogLifecycle(t *testing.T) {
	python := pythonBin(t)
	repoRoot := repoRoot(t)
	skillDir := filepath.Join(repoRoot, "skills", "coordinator")
	driver := filepath.Join(repoRoot, "cmd", "fleet", "testdata", "fleetlog_tick_driver.py")

	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	t.Setenv("XDG_STATE_HOME", "") // ambient XDG would redirect Dir()
	t.Setenv("FLEET_AGENT_ID", "")
	logDir := filepath.Join(home, "logs")
	for _, d := range []string{
		filepath.Join(home, "projects", "projects-fleet", ".locks"),
		filepath.Join(home, "inbox", "archive"),
		filepath.Join(home, "agents"),
		filepath.Join(home, "queue"),
		logDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	const project = "projects-fleet"
	const slug = "lifecycle-task"

	// --- Trigger 1: a real Python coord tick (coord.tick x2 + dispatch.worker)
	cmd := exec.Command(python, driver, skillDir, home, home, project, slug)
	cmd.Env = append(os.Environ(), "FLEET_HOME="+home, "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("coord tick driver failed: %v\n%s", err, out)
	}

	// --- Trigger 2: a real Go worker phase transition (state.transition)
	if err := workers.WriteState(project, slug, &workers.State{
		Slug:    slug,
		Project: project,
		Phase:   workers.PhaseReviewPending,
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	// --- Trigger 3: a real wired CLI command (cli.start + cli.finish)
	dc := newDrainCmd()
	dc.SetArgs([]string{})
	dc.SetOut(os.NewFile(0, os.DevNull))
	dc.SetErr(os.NewFile(0, os.DevNull))
	_ = dc.Execute() // drain may exit nil (no queue files) or err; both emit start+finish

	// ---------- assertions over the merged log dir ----------
	if got := fleetlog.Dir(); got != logDir {
		t.Fatalf("fleetlog.Dir()=%q, want %q", got, logDir)
	}
	lines, files := readLogDir(t, logDir)

	// (1) schema: every line validates.
	tsRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)
	for _, m := range lines {
		for _, k := range []string{"ts", "seq", "type", "lvl", "comp", "pid", "id"} {
			if _, ok := m[k]; !ok {
				t.Errorf("line missing key %q: %v", k, m)
			}
		}
		if ts, _ := m["ts"].(string); !tsRe.MatchString(ts) {
			t.Errorf("ts %q not RFC3339 microsecond UTC", ts)
		}
		if typ, _ := m["type"].(string); !fleetlog.Types[typ] {
			t.Errorf("type %q not in closed vocabulary", typ)
		}
		if id, _ := m["id"].(string); id == "" {
			t.Errorf("empty id: %v", m)
		}
	}

	// (2) count + correlation keys.
	byType := map[string][]map[string]any{}
	for _, m := range lines {
		typ, _ := m["type"].(string)
		byType[typ] = append(byType[typ], m)
	}
	wantCounts := map[string]int{
		"coord.tick":       2,
		"dispatch.worker":  1,
		"state.transition": 1,
		"cli.start":        1,
		"cli.finish":       1,
	}
	for typ, want := range wantCounts {
		if got := len(byType[typ]); got != want {
			t.Errorf("type %q: got %d lines, want %d", typ, got, want)
		}
	}
	if dw := byType["dispatch.worker"]; len(dw) == 1 && dw[0]["slug"] != slug {
		t.Errorf("dispatch.worker slug=%v, want %q", dw[0]["slug"], slug)
	}
	if st := byType["state.transition"]; len(st) == 1 {
		if st[0]["slug"] != slug {
			t.Errorf("state.transition slug=%v, want %q", st[0]["slug"], slug)
		}
		data, _ := st[0]["data"].(map[string]any)
		if data["to"] != string(workers.PhaseReviewPending) {
			t.Errorf("state.transition data.to=%v, want %q", data["to"], workers.PhaseReviewPending)
		}
		if _, ok := data["from"]; !ok {
			t.Errorf("state.transition missing data.from: %v", data)
		}
	}
	if cs := byType["cli.start"]; len(cs) == 1 {
		data, _ := cs[0]["data"].(map[string]any)
		argv, _ := data["argv"].([]any)
		if len(argv) == 0 || argv[0] != "drain" {
			t.Errorf("cli.start data.argv must contain drain: %v", data)
		}
	}
	if cf := byType["cli.finish"]; len(cf) == 1 {
		data, _ := cf[0]["data"].(map[string]any)
		if _, ok := data["rc"]; !ok {
			t.Errorf("cli.finish missing data.rc: %v", data)
		}
	}

	// (3) per-process files with the right comp per source.
	nameRe := regexp.MustCompile(`^fleet-\d{4}-\d{2}-\d{2}-(coord|worker|cli)-\d+-\d+\.jsonl$`)
	comps := map[string]bool{}
	for _, f := range files {
		m := nameRe.FindStringSubmatch(f)
		if m == nil {
			t.Errorf("file %q does not match per-process pattern", f)
			continue
		}
		comps[m[1]] = true
	}
	for _, want := range []string{"coord", "worker", "cli"} {
		if !comps[want] {
			t.Errorf("missing a %q-component log file; got files %v", want, files)
		}
	}

	// (4) cross-language parity: one jq reads EVERY line (Python + Go).
	assertJQReadsAll(t, logDir, len(lines))
}

// readLogDir parses every JSONL line under dir; fails on any torn/invalid
// line. Returns the parsed lines and the file basenames.
func readLogDir(t *testing.T, dir string) ([]map[string]any, []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	var lines []map[string]any
	var files []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		files = append(files, e.Name())
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
			lines = append(lines, m)
		}
	}
	return lines, files
}

// assertJQReadsAll runs `jq -c .type <dir>/*.jsonl` (the design's stated
// cross-language read) and asserts it parses every line. Skipped when jq is
// absent so the suite stays green on jq-less hosts (the Go json parse above
// already covers parity).
func assertJQReadsAll(t *testing.T, dir string, wantLines int) {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Logf("jq not found; skipping the jq cross-language read (Go parse already verified parity)")
		return
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	args := append([]string{"-c", ".type"}, matches...)
	out, err := exec.Command(jq, args...).Output()
	if err != nil {
		t.Fatalf("jq failed to read merged logs (a non-parseable line from either emitter): %v", err)
	}
	got := 0
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln != "" {
			got++
		}
	}
	if got != wantLines {
		t.Errorf("jq read %d lines, want %d (Python+Go merged)", got, wantLines)
	}
}

func pythonBin(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"python3", "python"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	t.Skip("python3 not found; skipping cross-language lifecycle test")
	return ""
}
