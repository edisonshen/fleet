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

// maybeAutoInit installs the fleet-guard skill silently if any
// embedded file is missing from ~/.claude/skills/fleet-guard/. First-
// run UX: an operator who runs `fleet dispatch X` or `fleet` (TUI)
// without first running `fleet init` still gets auto-handoff, without
// an explicit setup step.
//
// Behavior:
//   - If every file embedded by FleetGuardFS exists at the install
//     path, do nothing.
//   - Otherwise — including the partial-install case where a prior
//     run wrote main.py but crashed before the sibling files —
//     print a "first run — installing" notice and call
//     runInit(force=false). runInit's per-file "skip (exists)" /
//     "wrote:" output is idempotent: present files are left alone,
//     missing ones are written.
//   - On runInit error, print a warning to stdout but do NOT fail —
//     basic dispatch still works without the skill installed; only
//     auto-handoff is disabled. The operator can rerun `fleet init`
//     manually to retry.
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

	skillRoot := filepath.Join(claudeHome, "skills", "fleet-guard")
	if skillFullyInstalled(claudeHome, skillRoot) {
		return
	}

	_, _ = fmt.Fprintln(stdout, "fleet: first run — installing fleet-guard skill...")
	if err := runInit(stdout, false, claudeHomeOverride); err != nil {
		_, _ = fmt.Fprintf(stdout,
			"fleet: skill install warning: %v\n", err)
		_, _ = fmt.Fprintln(stdout,
			"fleet: continuing without auto-handoff — run `fleet init` manually to retry")
	}
}

// skillFullyInstalled returns true iff (a) every embedded file is on
// disk under skillRoot AND (b) ~/.claude/settings.json registers the
// fleet-guard hook command for every required event. A partial
// install (files written but settings.json merge crashed, or the
// reverse) is the codex iter-2 P2 case: the previous file-only check
// missed the second failure mode and the operator was stuck without
// auto-handoff forever.
//
// Any walk error or settings.json read/parse failure is treated as
// "not fully installed" — better to repeat idempotent work than
// silently leave the install broken.
func skillFullyInstalled(claudeHome, skillRoot string) bool {
	if !skillFilesPresent(skillRoot) {
		return false
	}
	mainPath := filepath.Join(skillRoot, "main.py")
	return hooksRegistered(claudeHome, mainPath)
}

// skillFilesPresent returns true iff every file in the embedded
// FleetGuardFS exists under skillRoot AND has the same byte content
// as the embedded copy. The byte-compare is what self-heals an
// upgraded fleet binary: when the bundled fleet-guard ships new
// hook handlers (e.g., UserPromptSubmit), an existing install with
// stale main.py is detected here and runInit's installSkillFiles
// rewrites the mismatched files. Without the compare, autoinit would
// see "files exist" and skip the refresh, leaving new hooks firing
// against a dispatcher that has no handler for them.
//
// Any read or walk error counts as "not present" so the autoinit
// path repeats idempotent work rather than leaving an install in
// a half-upgraded state.
func skillFilesPresent(skillRoot string) bool {
	fsys := fleet.FleetGuardFS()
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
