package projects

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteGlobalCoordConfigEngine_PreservesSiblings(t *testing.T) {
	home := t.TempDir()
	cfgPath := GlobalCoordConfigPath(home)
	if err := os.WriteFile(cfgPath, []byte(`{"review_effort": "medium", "parallelism": 3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteGlobalCoordConfigEngine(home, "codex"); err != nil {
		t.Fatalf("WriteGlobalCoordConfigEngine: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if cfg["engine"] != "codex" || cfg["review_effort"] != "medium" || cfg["parallelism"] != float64(3) {
		t.Errorf("cfg = %v; want engine=codex with siblings intact", cfg)
	}
	if got := ReadGlobalCoordConfigEngine(home); got != "codex" {
		t.Errorf("ReadGlobalCoordConfigEngine = %q want codex", got)
	}
	// Rewrite to the same value is a no-op (mtime-stable, no temp debris).
	before, _ := os.Stat(cfgPath)
	if err := WriteGlobalCoordConfigEngine(home, "codex"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(cfgPath)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("idempotent rewrite touched the file")
	}
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.Name() != "coord-config.json" {
			t.Errorf("unexpected debris in fleet home: %s", e.Name())
		}
	}
}

func TestWriteGlobalCoordConfigEngine_CreatesHomeAndFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nested", ".fleet")
	if err := WriteGlobalCoordConfigEngine(home, "claude-code"); err != nil {
		t.Fatalf("WriteGlobalCoordConfigEngine: %v", err)
	}
	if got := ReadGlobalCoordConfigEngine(home); got != "claude-code" {
		t.Errorf("ReadGlobalCoordConfigEngine = %q want claude-code", got)
	}
	if err := WriteGlobalCoordConfigEngine("", "codex"); err == nil {
		t.Error("empty home accepted")
	}
	if err := WriteGlobalCoordConfigEngine(home, ""); err == nil {
		t.Error("empty engine accepted")
	}
}

func TestWriteGlobalCoordConfigEngine_MalformedFileReplaced(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(GlobalCoordConfigPath(home), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteGlobalCoordConfigEngine(home, "codex"); err != nil {
		t.Fatalf("WriteGlobalCoordConfigEngine over malformed file: %v", err)
	}
	if got := ReadGlobalCoordConfigEngine(home); got != "codex" {
		t.Errorf("ReadGlobalCoordConfigEngine = %q want codex", got)
	}
}

func TestResolveEngineChoice_GlobalBeatsProject(t *testing.T) {
	home := t.TempDir()
	if got := ResolveEngineChoice(home, "p"); got != "" {
		t.Errorf("nothing persisted: got %q want empty", got)
	}
	if got := ResolveEngineChoice("", "p"); got != "" {
		t.Errorf("empty home: got %q want empty", got)
	}

	projDir := filepath.Join(home, "projects", "p")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CoordConfigPath(home, "p"), []byte(`{"engine": "claude-code"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveEngineChoice(home, "p"); got != "claude-code" {
		t.Errorf("project stamp only: got %q want claude-code", got)
	}

	if err := WriteGlobalCoordConfigEngine(home, "codex"); err != nil {
		t.Fatal(err)
	}
	if got := ResolveEngineChoice(home, "p"); got != "codex" {
		t.Errorf("global choice must beat project stamp: got %q want codex", got)
	}
	if got := ResolveEngineChoice(home, "other"); got != "codex" {
		t.Errorf("global choice applies to every project: got %q want codex", got)
	}
}
