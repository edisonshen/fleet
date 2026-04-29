package main

import (
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
	if skillFullyInstalled(skillRoot) {
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

// skillFullyInstalled returns true iff every file embedded by
// FleetGuardFS has a counterpart on disk under skillRoot. A partial
// install (main.py present but a sibling missing because the previous
// run crashed mid-loop, or an operator manually deleted one file)
// returns false so maybeAutoInit re-runs runInit and lets its
// idempotent "skip (exists)" / "wrote:" loop restore the missing
// pieces. Any walk error (permission denied, embedded-FS corruption)
// is treated as "not fully installed" — better to repeat work than
// silently leave the install broken.
func skillFullyInstalled(skillRoot string) bool {
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
		if _, statErr := os.Stat(filepath.Join(skillRoot, path)); statErr != nil {
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
