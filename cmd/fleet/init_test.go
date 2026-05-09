package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

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
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

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
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

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
	if !strings.Contains(out.String(), "skip (up to date)") {
		t.Errorf("expected 'skip (up to date)' in second-run output, got:\n%s", out.String())
	}
}

// TestRunInit_StaleFilesAutoRefresh — the new contract (codex review
// iter-3 P1): when an installed skill file's bytes differ from the
// embedded copy, the next runInit (even without --force) refreshes
// it. This is the upgrade path: a fleet binary that adds a hook
// handler must auto-heal the stale main.py without operator action.
//
// Trade-off: operator-edited skill files are not preserved. fleet
// owns the bundled skill files; customization belongs in a fork or
// outside the fleet-guard install path.
func TestRunInit_StaleFilesAutoRefresh(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

	// First install.
	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	if err := os.WriteFile(mainPath, []byte("# stale from old fleet binary"), 0o644); err != nil {
		t.Fatalf("plant stale main.py: %v", err)
	}

	// Without --force, the stale file is auto-refreshed back to the
	// embedded byte stream — the byte-compare path triggers.
	var out bytes.Buffer
	if err := runInit(&out, false, claudeHome); err != nil {
		t.Fatalf("auto-heal runInit: %v", err)
	}
	if !strings.Contains(out.String(), "refresh (stale)") {
		t.Errorf("expected 'refresh (stale)' message for drift, got:\n%s", out.String())
	}
	got, _ := os.ReadFile(mainPath)
	want, _ := fs.ReadFile(fleet.FleetGuardFS(), "main.py")
	if !bytes.Equal(got, want) {
		t.Errorf("stale main.py not refreshed to embedded content")
	}
}

// TestRunInit_PythonFilesAreExecutable — main.py is invoked via shebang
// in some hook runners; the install must mark it executable.
func TestRunInit_PythonFilesAreExecutable(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
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
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
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

// TestRunInit_RegistersAllHookEvents — every entry in hookEvents
// needs a settings.json registration pointing at main.py. A regression
// that dropped one would silently break that hook's behavior (e.g.
// missing UserPromptSubmit leaves needs_input stuck at true).
func TestRunInit_RegistersAllHookEvents(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
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
	for _, ev := range []string{"Stop", "PreCompact", "SessionStart", "UserPromptSubmit"} {
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
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))
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

// TestRunInit_InstallsCoordinatorSkill — v0.2 PR 12 contract: every
// skill returned by fleet.SkillFS() must land at
// ~/.claude/skills/<name>/. A regression that dropped coordinator from
// the install loop would leave the operator's coord agent without
// /coordinator on disk; the skill would silently degrade to "command
// not found" inside the running coord.
func TestRunInit_InstallsCoordinatorSkill(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	coordRoot := filepath.Join(claudeHome, "skills", "coordinator")
	for _, want := range []string{"SKILL.md", "loop.py", "parse.py", "dispatch.py", "conflict.py", "worktree.py", "remote_control.py", "supervisor.py"} {
		got := filepath.Join(coordRoot, want)
		info, err := os.Stat(got)
		if err != nil {
			t.Errorf("coordinator skill file missing: %s (%v)", want, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("coordinator skill file empty: %s", want)
		}
	}
	// Byte-equality with the embedded source so a copy-pipeline
	// transformation (BOM, line-ending normalization) fails the test
	// before it bites the operator.
	fsys := fleet.CoordinatorFS()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, _ := fs.ReadFile(fsys, path)
		got, readErr := os.ReadFile(filepath.Join(coordRoot, path))
		if readErr != nil {
			t.Errorf("read installed coordinator/%s: %v", path, readErr)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("coordinator/%s: installed differs from embedded", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRunInit_InstallsAllBundledSkills — wider contract test: the
// install loop walks fleet.SkillFS() so a future skill addition (just
// dropping it into embed.go's map) lands automatically. If a future
// skill goes missing from the install path, this test catches it
// without per-skill assertions cluttering the suite.
func TestRunInit_InstallsAllBundledSkills(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	t.Setenv("FLEET_HOME", filepath.Join(tmp, ".fleet"))

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	for name, fsys := range fleet.SkillFS() {
		skillRoot := filepath.Join(claudeHome, "skills", name)
		err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if _, err := os.Stat(filepath.Join(skillRoot, path)); err != nil {
				t.Errorf("skill %s: %s missing after install (%v)", name, path, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunInit_SeedsStandardsTemplate — ~/.fleet/standards.md must land
// after init when missing, with v1 frontmatter and the canonical
// "# Standards" H1 so `fleet standards show` produces parseable output
// immediately. Operator can edit it freely afterward; --upgrade
// preserves edits (covered in TestRunInit_UpgradePreservesStandards).
func TestRunInit_SeedsStandardsTemplate(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	stdPath := filepath.Join(fleetHome, "standards.md")
	got, err := os.ReadFile(stdPath)
	if err != nil {
		t.Fatalf("standards.md not seeded: %v", err)
	}
	body := string(got)
	if !strings.HasPrefix(body, "---\nschema: v1\n---\n") {
		t.Errorf("seed missing v1 frontmatter; got prefix %q", body[:32])
	}
	if !strings.Contains(body, "# Standards") {
		t.Errorf("seed missing '# Standards' H1:\n%s", body)
	}
	if !strings.Contains(body, "## Testing") {
		t.Errorf("seed missing '## Testing' section:\n%s", body)
	}
	if !strings.Contains(body, "## Code review") {
		t.Errorf("seed missing '## Code review' section:\n%s", body)
	}
	// Async waits section is the v0.5+ baseline. Without it, every
	// freshly-spawned worker re-discovers the foreground-sleep / operator-
	// ping anti-patterns issue #105 calls out. Pin it so a future template
	// edit can't silently regress the contract.
	if !strings.Contains(body, "## Async waits") {
		t.Errorf("seed missing '## Async waits' section:\n%s", body)
	}
	if !strings.Contains(body, "run_in_background") {
		t.Errorf("Async waits section missing run_in_background recipe:\n%s", body)
	}
	if !strings.Contains(body, "until") {
		t.Errorf("Async waits section missing until-loop pattern:\n%s", body)
	}
}

// TestStandardsTemplate_EmbedsAsyncWaitsSection — issue #105 baseline:
// the embedded template bytes (the source of truth `seedStandardsTemplate`
// writes on first init) carry the `## Async waits` recipe so every
// fleet operator inherits it without manual editing. Mirrors the
// per-element checks in TestRunInit_SeedsStandardsTemplate but bypasses
// the install path so a regression in the embed.go directive surfaces
// independently of init plumbing.
func TestStandardsTemplate_EmbedsAsyncWaitsSection(t *testing.T) {
	body := string(fleet.StandardsTemplate())
	for _, want := range []string{
		"## Async waits",
		"run_in_background: true",
		"until <single check that returns truthy when done>",
		"task-notification",
		"270s",
		"gh pr view",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("StandardsTemplate missing %q:\n%s", want, body)
		}
	}
}

// TestRunInit_UpgradePreservesStandards — the load-bearing rule of
// `fleet init --upgrade`: skill files refresh from the binary, but a
// hand-edited ~/.fleet/standards.md is preserved verbatim. Without
// this guard, every binary upgrade would silently overwrite the
// operator's bar with the canned template.
func TestRunInit_UpgradePreservesStandards(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	stdPath := filepath.Join(fleetHome, "standards.md")

	// Operator hand-edits the standards file.
	customBody := "---\nschema: v1\n---\n\n# Standards\n\n## Testing\n\nMy custom rules. Not the template.\n"
	if err := os.WriteFile(stdPath, []byte(customBody), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second `fleet init --upgrade` (force=true) must NOT clobber the
	// hand-edit. The skill files DO get refreshed (covered by the
	// fleet-guard tests above); standards.md stays put.
	var out bytes.Buffer
	if err := runInit(&out, true, claudeHome); err != nil {
		t.Fatalf("upgrade runInit: %v", err)
	}
	got, err := os.ReadFile(stdPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != customBody {
		t.Errorf("--upgrade clobbered hand-edited standards.md:\nwant: %q\n got: %q",
			customBody, string(got))
	}
	if !strings.Contains(out.String(), "skip (existing): "+stdPath) {
		t.Errorf("expected 'skip (existing)' message for preserved standards.md, got:\n%s", out.String())
	}
}

// TestRunInit_IsIdempotentAcrossSkills — re-running init after a clean
// install must produce zero writes for either skill AND for
// standards.md. Catches regressions where one skill's install path
// trampled the idempotency contract.
func TestRunInit_IsIdempotentAcrossSkills(t *testing.T) {
	tmp := t.TempDir()
	claudeHome := filepath.Join(tmp, ".claude")
	fleetHome := filepath.Join(tmp, ".fleet")
	t.Setenv("FLEET_HOME", fleetHome)

	if err := runInit(&bytes.Buffer{}, false, claudeHome); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	// Snapshot all installed bytes.
	first := snapshotInstall(t, claudeHome, fleetHome)

	var out bytes.Buffer
	if err := runInit(&out, false, claudeHome); err != nil {
		t.Fatalf("second runInit: %v", err)
	}
	second := snapshotInstall(t, claudeHome, fleetHome)
	if !bytes.Equal(first, second) {
		t.Errorf("install bytes drifted across idempotent runs:\nout=%s", out.String())
	}
	// Coordinator skip messages must be present too — verifies the
	// install loop actually iterated over coordinator on the second
	// run (otherwise we'd silently regress to fleet-guard-only).
	if !strings.Contains(out.String(), "skills/coordinator") {
		t.Errorf("second-run output missing coordinator path; install loop may have skipped it:\n%s", out.String())
	}
}

// snapshotInstall reads every installed file under claudeHome's
// skills/ tree and ~/.fleet/standards.md into one deterministic blob.
// Used by idempotency tests to assert no bytes drifted across re-runs.
func snapshotInstall(t *testing.T, claudeHome, fleetHome string) []byte {
	t.Helper()
	var b bytes.Buffer
	skillsDir := filepath.Join(claudeHome, "skills")
	err := filepath.Walk(skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fmt.Fprintf(&b, "FILE %s\n", path)
		b.Write(data)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stdPath := filepath.Join(fleetHome, "standards.md")
	if data, err := os.ReadFile(stdPath); err == nil {
		fmt.Fprintf(&b, "FILE %s\n", stdPath)
		b.Write(data)
	}
	return b.Bytes()
}
