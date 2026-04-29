package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleet "github.com/edisonshen/fleet"
)

// TestRunInit_WritesAllSkillFiles verifies that every file embedded by
// the //go:embed directive at the repo root lands at the expected path
// under the redirected ~/.claude/. If the embedded set diverges from
// what tests expect, this fails loud rather than producing a silently
// incomplete install on operator machines.
func TestRunInit_WritesAllSkillFiles(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	var out bytes.Buffer
	if err := runInit(&out, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	skillRoot := filepath.Join(claudeHome, "skills", "fleet-guard")
	expected := walkExpectedSkillFiles(t)
	for _, name := range expected {
		got := filepath.Join(skillRoot, name)
		info, err := os.Stat(got)
		if err != nil {
			t.Errorf("skill file not installed: %s (%v)", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("skill file is empty: %s", name)
		}
	}
}

// TestRunInit_FilesByteEqualEmbedded — the load-bearing assertion. If a
// build pipeline ever copies/transforms skill files (line-ending
// normalization, BOM injection, etc.) the byte-equal check fails and the
// operator gets a hint before runtime.
func TestRunInit_FilesByteEqualEmbedded(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	skillRoot := filepath.Join(claudeHome, "skills", "fleet-guard")
	fsys := fleet.FleetGuardFS()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(skillRoot, path))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("byte mismatch for %s: installed differs from embedded", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunInit_IsIdempotent — running twice must not duplicate settings.json
// hook entries and must skip already-installed files.
func TestRunInit_IsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	settingsPath := filepath.Join(claudeHome, "settings.json")
	first, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}

	var out bytes.Buffer
	if err := runInit(&out, false, claudeHome); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
	second, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("re-read settings.json: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("idempotent re-run changed settings.json:\nbefore: %s\nafter:  %s",
			first, second)
	}
	if !strings.Contains(out.String(), "skip (exists)") {
		t.Errorf("expected 'skip (exists)' in second-run output, got:\n%s", out.String())
	}
}

// TestRunInit_ForceOverwritesFiles — --force must overwrite an
// operator-edited skill file. The test plants stale content and asserts
// the second pass with force=true restores the embedded byte stream.
func TestRunInit_ForceOverwritesFiles(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")

	// First install.
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	if err := os.WriteFile(mainPath, []byte("# corrupted by operator"), 0o644); err != nil {
		t.Fatalf("plant corrupt main.py: %v", err)
	}

	// Without force, the corrupted file is preserved.
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("non-force runInit: %v", err)
	}
	got, _ := os.ReadFile(mainPath)
	if !bytes.Equal(got, []byte("# corrupted by operator")) {
		t.Error("non-force runInit overwrote the operator's edited file")
	}

	// With force, the embedded version is restored.
	if err := runInit(&bytes.Buffer{}, true, claudeHome); err != nil {
		t.Fatalf("force runInit: %v", err)
	}
	got, _ = os.ReadFile(mainPath)
	want, _ := fs.ReadFile(fleet.FleetGuardFS(), "main.py")
	if !bytes.Equal(got, want) {
		t.Errorf("force runInit did not restore embedded main.py")
	}
}

// TestRunInit_PythonFilesAreExecutable — main.py is invoked via shebang
// in some hook runners; the install must mark it executable.
func TestRunInit_PythonFilesAreExecutable(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	info, err := os.Stat(mainPath)
	if err != nil {
		t.Fatalf("stat main.py: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("main.py not executable: mode=%v", info.Mode())
	}
}

// TestRunInit_PreservesExistingHooks — the spike's stop-hook.py is wired
// in the user's settings.json and must NOT be wiped by `fleet init`.
// We seed a settings.json with an existing Stop hook and assert it
// survives alongside the new fleet-guard registration.
func TestRunInit_PreservesExistingHooks(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	pre := map[string]any{
		"some_other_setting": "preserved",
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/usr/bin/python3 /existing/script.py",
						},
					},
				},
			},
		},
	}
	preBytes, _ := json.MarshalIndent(pre, "", "  ")
	if err := os.WriteFile(filepath.Join(claudeHome, "settings.json"),
		append(preBytes, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	merged, err := os.ReadFile(filepath.Join(claudeHome, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("parse merged settings: %v", err)
	}
	if out["some_other_setting"] != "preserved" {
		t.Errorf("non-hook setting lost: %v", out["some_other_setting"])
	}
	stop, _ := out["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 2 {
		t.Errorf("expected 2 Stop hook entries (existing + fleet-guard), got %d", len(stop))
	}
	// Verify the existing entry's command is still present somewhere.
	body := string(merged)
	if !strings.Contains(body, "/existing/script.py") {
		t.Error("existing hook command was wiped by merge")
	}
	if !strings.Contains(body, "fleet-guard/main.py") {
		t.Error("fleet-guard hook not registered")
	}
}

// TestRunInit_RegistersAllThreeHookEvents — Stop / PreCompact / SessionStart
// all need entries pointing at main.py. A regression that registered only
// Stop would silently disable the inbox-on-resume path.
func TestRunInit_RegistersAllThreeHookEvents(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	merged, err := os.ReadFile(filepath.Join(claudeHome, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatal(err)
	}
	hooks, ok := out["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks map missing")
	}
	for _, ev := range []string{"Stop", "PreCompact", "SessionStart"} {
		arr, ok := hooks[ev].([]any)
		if !ok || len(arr) == 0 {
			t.Errorf("hook event %q missing or empty", ev)
		}
	}
}

// TestRunInit_RefusesCorruptHooksField — a hand-edited settings.json
// that has 'hooks' as something other than a JSON object (an array,
// a string, etc.) MUST cause runInit to error out instead of silently
// overwriting with an empty map. Otherwise an operator's misformatted
// file is wiped and they get a confusing "missing hooks" experience.
func TestRunInit_RefusesCorruptHooksField(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// 'hooks' as an array — invalid for our merge but plausible for a
	// hand-edit that confused arrays of events with the keyed object.
	corrupt := `{"hooks": ["Stop", "PreCompact"]}` + "\n"
	if err := os.WriteFile(filepath.Join(claudeHome, "settings.json"),
		[]byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runInit(&bytes.Buffer{}, false, claudeHome)
	if err == nil {
		t.Fatal("expected error on corrupt hooks field; got nil")
	}
	if !strings.Contains(err.Error(), "expected JSON object") {
		t.Errorf("error did not explain the type mismatch: %v", err)
	}

	// Original file untouched — operator can repair manually.
	got, _ := os.ReadFile(filepath.Join(claudeHome, "settings.json"))
	if string(got) != corrupt {
		t.Errorf("corrupt settings.json was modified: got %q want %q",
			string(got), corrupt)
	}
}

// walkExpectedSkillFiles returns the relative paths that //go:embed bound
// at compile time. Tests use this as the source of truth for "what should
// be installed" so a stray new file in the embed doesn't go untested.
func walkExpectedSkillFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := fs.WalkDir(fleet.FleetGuardFS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("FleetGuardFS contains zero files; //go:embed lost the skill")
	}
	return files
}
