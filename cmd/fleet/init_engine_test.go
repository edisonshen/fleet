package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/edisonshen/fleet"
)

// The install surface follows the dominant engine: `fleet init` puts the
// skills where the SELECTED engine's CLI discovers them and merges hooks
// into THAT engine's hooks file. A codex-only operator must never need
// ~/.claude, and a claude-only operator must never see ~/.codex appear.

func TestResolveInstallPaths_PerEngine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		engine        string
		wantSkillHome string
		wantHooks     string
	}{
		{"", filepath.Join(home, ".claude"), filepath.Join(home, ".claude", "settings.json")},
		{"claude-code", filepath.Join(home, ".claude"), filepath.Join(home, ".claude", "settings.json")},
		{"codex", filepath.Join(home, ".agents"), filepath.Join(home, ".codex", "hooks.json")},
		// Unknown engine names never invent a third skill home.
		{"bogus", filepath.Join(home, ".claude"), filepath.Join(home, ".claude", "settings.json")},
	}
	for _, tc := range cases {
		t.Setenv(FleetEngineEnv, tc.engine)
		got, err := resolveInstallPaths("")
		if err != nil {
			t.Fatalf("engine %q: %v", tc.engine, err)
		}
		if got.skillHome != tc.wantSkillHome || got.hooksFile != tc.wantHooks {
			t.Errorf("engine %q: got (%s, %s) want (%s, %s)",
				tc.engine, got.skillHome, got.hooksFile, tc.wantSkillHome, tc.wantHooks)
		}
	}
}

// A test override keeps everything under one temp dir but still uses the
// engine's hooks basename so codex tests exercise the hooks.json path.
func TestResolveInstallPaths_OverrideUsesEngineHooksBasename(t *testing.T) {
	override := t.TempDir()
	t.Setenv(FleetEngineEnv, "codex")
	got, err := resolveInstallPaths(override)
	if err != nil {
		t.Fatal(err)
	}
	if got.skillHome != override {
		t.Errorf("skillHome = %s want override %s", got.skillHome, override)
	}
	if want := filepath.Join(override, "hooks.json"); got.hooksFile != want {
		t.Errorf("hooksFile = %s want %s", got.hooksFile, want)
	}
	if got.engine != "codex" {
		t.Errorf("engine = %q want codex", got.engine)
	}
}

func TestRunInit_CodexInstallsSkillsAndMergesHooksJSON(t *testing.T) {
	tmp := t.TempDir()
	agentsHome := filepath.Join(tmp, ".agents")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
	t.Setenv(FleetEngineEnv, "codex")
	if err := os.MkdirAll(agentsHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing unrelated content in hooks.json must survive the merge.
	pre := map[string]any{
		"description": "operator hooks",
		"hooks": map[string]any{
			"PostToolUse": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": "/usr/bin/python3 /existing/post.py"},
			}}},
		},
	}
	preBytes, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(filepath.Join(agentsHome, "hooks.json"), preBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInit(&out, false, agentsHome); err != nil {
		t.Fatalf("runInit: %v\n%s", err, out.String())
	}

	for name := range fleet.SkillFS() {
		if _, err := os.Stat(filepath.Join(agentsHome, "skills", name, "SKILL.md")); err != nil {
			t.Errorf("skill %s not installed under codex skill home: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(agentsHome, "settings.json")); err == nil {
		t.Error("codex init must not write a Claude-style settings.json")
	}

	merged, err := os.ReadFile(filepath.Join(agentsHome, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got["description"] != "operator hooks" {
		t.Errorf("unrelated top-level key lost: %v", got["description"])
	}
	hooks, _ := got["hooks"].(map[string]any)
	if post, _ := hooks["PostToolUse"].([]any); len(post) != 1 {
		t.Errorf("pre-existing PostToolUse entry lost: %v", hooks["PostToolUse"])
	}
	mainPath := filepath.Join(agentsHome, "skills", "fleet-guard", "main.py")
	for _, ev := range hookEvents {
		if !hookEntryPresent(hooks, ev, "/usr/bin/env python3 "+mainPath) {
			t.Errorf("hook %s not registered in hooks.json", ev)
		}
	}
	if !strings.Contains(out.String(), "hooks.json") && !strings.Contains(out.String(), "registered:") {
		t.Errorf("expected hook registration output, got:\n%s", out.String())
	}
}

func TestMaybeAutoInit_CodexIdempotentAndNamesEngine(t *testing.T) {
	tmp := t.TempDir()
	agentsHome := filepath.Join(tmp, ".agents")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
	t.Setenv(FleetEngineEnv, "codex")

	var first bytes.Buffer
	maybeAutoInit(&first, agentsHome)
	if !strings.Contains(first.String(), "installing bundled skills for codex") {
		t.Errorf("first run should install for codex, got:\n%s", first.String())
	}
	if !hooksRegistered(filepath.Join(agentsHome, "hooks.json"),
		filepath.Join(agentsHome, "skills", "fleet-guard", "main.py")) {
		t.Error("codex hooks.json not registered after autoinit")
	}

	var second bytes.Buffer
	maybeAutoInit(&second, agentsHome)
	if second.Len() != 0 {
		t.Errorf("second autoinit should be a no-op, got:\n%s", second.String())
	}
}

// Switching engines installs into the other skill home without touching
// the first: both installs can coexist on a dual-subscription host.
func TestMaybeAutoInit_EnginesInstallSideBySide(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FLEET_HOME", filepath.Join(home, ".fleet"))

	t.Setenv(FleetEngineEnv, "claude-code")
	maybeAutoInit(&bytes.Buffer{}, "")
	t.Setenv(FleetEngineEnv, "codex")
	maybeAutoInit(&bytes.Buffer{}, "")

	for _, p := range []string{
		filepath.Join(home, ".claude", "skills", "coordinator", "SKILL.md"),
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".agents", "skills", "coordinator", "SKILL.md"),
		filepath.Join(home, ".codex", "hooks.json"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "settings.json")); err == nil {
		t.Error("codex install leaked a settings.json into ~/.agents")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "hooks.json")); err == nil {
		t.Error("claude install leaked a hooks.json into ~/.claude")
	}
}
