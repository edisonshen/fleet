package spawn

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestSpawn_StampsIsCoordForCoordSpawn is test 4 of DESIGN-coord-idle-
// exempt: a coord spawn (TaskID == "coord-<project>") must persist
// rec.IsCoord = true; a worker spawn (real task slug) must leave it
// false. The Python idle-archive sweep reads is_coord as its explicit
// exemption signal — if the stamp is missing on coords, the coord falls
// back to the task_id convention (Signal 1) but the explicit field is
// the one this test pins.
func TestSpawn_StampsIsCoordForCoordSpawn(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)

	// Coord spawn: TaskID == "coord-<project>". Cwd must be non-empty
	// for a coord (TestSpawn_CoordEmptyCwdErrors), so pass an explicit
	// dir.
	cwd := t.TempDir()
	coord, err := Spawn(Options{
		TaskID:  "coord-myproj",
		Project: "myproj",
		Cwd:     cwd,
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(coord.TmuxSession) })

	if !coord.IsCoord {
		t.Errorf("coord spawn: rec.IsCoord=false, want true (%+v)", coord)
	}
	// Persisted projection must survive the round-trip to disk.
	loadedCoord, err := agent.Load(coord.ID)
	if err != nil {
		t.Fatalf("Load coord: %v", err)
	}
	if !loadedCoord.IsCoord {
		t.Errorf("loaded coord: IsCoord=false, want true")
	}

	// Worker spawn: a real task slug, NOT "coord-<project>".
	worker, err := Spawn(Options{
		TaskID:  "real-feature-slug",
		Project: "myproj",
		Command: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Spawn worker: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(worker.TmuxSession) })

	if worker.IsCoord {
		t.Errorf("worker spawn: rec.IsCoord=true, want false (%+v)", worker)
	}
	loadedWorker, err := agent.Load(worker.ID)
	if err != nil {
		t.Fatalf("Load worker: %v", err)
	}
	if loadedWorker.IsCoord {
		t.Errorf("loaded worker: IsCoord=true, want false")
	}
}

// TestCoordTaskIDPrefix_PythonDriftGuard is test 5 of DESIGN-coord-idle-
// exempt: the Python idle-archive sweep re-types the coord task_id
// convention as f"coord-{project}", while the Go side owns it as
// coordTaskIDPrefix. If either side changes the literal without the
// other, the two-signal exemption silently loses Signal 1 (the fallback
// that protects legacy coords with no is_coord field). This test reads
// the Python source and asserts the literal it uses agrees with the Go
// constant — it fails the moment one side drifts alone.
func TestCoordTaskIDPrefix_PythonDriftGuard(t *testing.T) {
	// Locate skills/coordinator/supervisor.py relative to this package
	// (internal/spawn) — repo root is two levels up.
	pyPath := filepath.Join("..", "..", "skills", "coordinator", "supervisor.py")
	src, err := os.ReadFile(pyPath)
	if err != nil {
		t.Fatalf("read %s: %v", pyPath, err)
	}

	// The Python exemption clause must build the coord task_id as
	// f"coord-{project}". We require the exact prefix literal inside an
	// f-string interpolating {project}. Matching "coord-{project}" pins
	// the prefix to coordTaskIDPrefix ("coord-").
	//
	// Match only on a CODE line, never a comment. The same literal also
	// appears in the explanatory comment above the comparison; a plain
	// whole-file grep would stay green if a future edit deleted the live
	// comparison but kept the comment — defeating the very drift this
	// test guards. So we strip Python comment lines first and require the
	// literal to survive on actual code.
	wantLiteral := coordTaskIDPrefix + "{project}" // "coord-{project}"
	pat := regexp.MustCompile(`f"` + regexp.QuoteMeta(wantLiteral) + `"`)
	foundOnCode := false
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // comment line — not load-bearing
		}
		if pat.MatchString(line) {
			foundOnCode = true
			break
		}
	}
	if !foundOnCode {
		t.Errorf("supervisor.py drift: expected the idle-archive exemption "+
			"to compare task_id against f%q ON A CODE LINE (pinned to Go "+
			"coordTaskIDPrefix=%q). A match only inside a comment does not "+
			"count. If you changed the coord task_id convention on one side, "+
			"change both.",
			wantLiteral, coordTaskIDPrefix)
	}
}
