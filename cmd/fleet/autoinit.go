package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	fleet "github.com/edisonshen/fleet"
)

// maybeAutoInit installs the bundled skills silently if any embedded
// file is missing from ~/.claude/skills/<skill>/. First-run UX: an
// operator who runs `fleet dispatch X` or `fleet` (TUI) without first
// running `fleet init` still gets auto-handoff (fleet-guard) and the
// coordinator skill on disk, without an explicit setup step.
//
// Behavior:
//   - If every file embedded by every fleet.SkillFS() entry exists
//     at the install path, do nothing.
//   - Otherwise — including the partial-install case where a prior
//     run wrote main.py but crashed before the sibling files, AND
//     the v0.1→v0.2 upgrade case where fleet-guard exists but
//     coordinator is missing — print a "first run — installing"
//     notice and call runInit(force=false). runInit's per-file
//     "skip (up to date)" / "wrote:" output is idempotent: present
//     files are left alone, missing ones are written.
//   - On runInit error, print a warning to stdout but do NOT fail —
//     basic dispatch still works without the skill installed; only
//     auto-handoff and coord-skill are disabled. The operator can
//     rerun `fleet init` manually to retry.
//
// claudeHomeOverride is for tests; production passes "".
func maybeAutoInit(stdout io.Writer, claudeHomeOverride string) {
	claudeHome := claudeHomeOverride
	if claudeHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// No home dir means runInit would fail too. Skip silently;
			// downstream code will surface the real error if it
			// matters.
			return
		}
		claudeHome = filepath.Join(home, ".claude")
	}

	if allSkillsFullyInstalled(claudeHome) {
		return
	}

	_, _ = fmt.Fprintln(stdout, "fleet: first run — installing bundled skills...")
	if err := runInit(stdout, false, claudeHomeOverride); err != nil {
		_, _ = fmt.Fprintf(stdout,
			"fleet: skill install warning: %v\n", err)
		_, _ = fmt.Fprintln(stdout,
			"fleet: continuing without auto-handoff — run `fleet init` manually to retry")
	}
}

// allSkillsFullyInstalled returns true iff every fleet.SkillFS() entry
// is on disk byte-equal to the embedded source AND fleet-guard's hooks
// are wired in settings.json. This is the v0.2 generalization of the
// v0.1 fleet-guard-only check: a v0.1 install with fleet-guard but no
// coordinator must trigger autoinit so the new skill lands.
func allSkillsFullyInstalled(claudeHome string) bool {
	for name, fsys := range fleet.SkillFS() {
		skillRoot := filepath.Join(claudeHome, "skills", name)
		if !skillFilesPresentFS(fsys, skillRoot) {
			return false
		}
	}
	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	return hooksRegistered(claudeHome, mainPath)
}

// skillFilesPresentFS returns true iff every file in fsys exists
// under skillRoot AND has the same byte content as the embedded
// copy. The byte-compare is what self-heals an upgraded fleet
// binary: when a bundled skill ships new code (e.g., UserPromptSubmit
// handler in main.py, or a new sentinel kind in loop.py), an existing
// install with stale files is detected here and runInit rewrites the
// mismatched bytes. Without the compare, autoinit would see "files
// exist" and skip the refresh, leaving new hooks/skill paths firing
// against stale code with no handler.
//
// Any read or walk error counts as "not present" so the autoinit
// path repeats idempotent work rather than leaving an install in
// a half-upgraded state.
//
// Generalized in v0.2 to accept any embedded fs.FS so the autoinit
// path checks every bundled skill (fleet-guard + coordinator + future)
// uniformly.
func skillFilesPresentFS(fsys fs.FS, skillRoot string) bool {
	complete := true
	walkErr := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			complete = false
			return fs.SkipAll
		}
		if d.IsDir() {
			return nil
		}
		embedded, readErr := fs.ReadFile(fsys, path)
		if readErr != nil {
			complete = false
			return fs.SkipAll
		}
		installed, statErr := os.ReadFile(filepath.Join(skillRoot, path))
		if statErr != nil || !bytes.Equal(installed, embedded) {
			complete = false
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return false
	}
	return complete
}

// hooksRegistered returns true iff settings.json contains a hook
// command pointing at mainPath under every event in hookEvents
// (Stop / PreCompact / SessionStart). Missing file, unparseable JSON,
// missing hooks key, or a missing event entry all return false so
// auto-init re-runs and mergeHookRegistrations can re-apply the
// (idempotent) merge. Mirrors the same matching the merge does, so
// re-running adds nothing on a healthy install.
func hooksRegistered(claudeHome, mainPath string) bool {
	settingsPath := filepath.Join(claudeHome, "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return false
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return false
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		return false
	}
	command := "/usr/bin/env python3 " + mainPath
	for _, event := range hookEvents {
		if !hookEntryPresent(hooks, event, command) {
			return false
		}
	}
	return true
}

// hookEntryPresent walks the same nested shape that ensureHookEntry
// writes (hooks.<event>: [{ "hooks": [{ "command": "..." }] }]) and
// reports whether any sub-entry has command == want.
func hookEntryPresent(hooks map[string]any, event, want string) bool {
	arr, _ := hooks[event].([]any)
	for _, raw := range arr {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		sub, _ := entry["hooks"].([]any)
		for _, h := range sub {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, _ := hm["command"].(string); cmd == want {
				return true
			}
		}
	}
	return false
}
