package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// maybeAutoInit installs the fleet-guard skill silently if it's missing.
// First-run UX: an operator who runs `fleet dispatch X` or `fleet`
// (TUI) without first running `fleet init` still gets auto-handoff,
// without an explicit setup step.
//
// Behavior:
//   - If ~/.claude/skills/fleet-guard/main.py exists, do nothing.
//   - Otherwise print a "first run — installing" notice and call
//     runInit(force=false). runInit's own output (wrote / skip /
//     registered lines) goes to stdout so the operator sees what
//     happened on first run.
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

	mainPath := filepath.Join(claudeHome, "skills", "fleet-guard", "main.py")
	if _, err := os.Stat(mainPath); err == nil {
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
