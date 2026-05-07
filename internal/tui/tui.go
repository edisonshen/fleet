package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"

	"github.com/edisonshen/fleet/internal/state"
)

// Run launches the dashboard. After [a] attach, control returns here
// when the operator detaches — the TUI relaunches automatically so
// `Ctrl-b d` brings them back to the dashboard, not their shell.
//
// Implementation: tmux runs as a subprocess (was: syscall.Exec
// replacement), so tui.Run can wait for it and loop back into a
// fresh bubbletea program. The altscreen is torn down between
// iterations, which avoids tmux fighting bubbletea for the
// terminal — this was the original reason for syscall.Exec; the
// subprocess approach preserves that property by running tmux only
// AFTER prog.Run() returns.
func Run(version string) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}
	for {
		session, err := runOnce(version)
		if err != nil {
			return err
		}
		if session == "" {
			return nil // user quit with [q] or ctrl+c
		}
		if err := runTmuxAttach(session); err != nil {
			// Detach failure (session died mid-attach, etc.) is
			// non-fatal — fall through to re-launch the TUI so the
			// operator can inspect or clean up.
			fmt.Fprintf(os.Stderr, "warning: tmux attach %s: %v\n", session, err)
		}
	}
}

// runOnce starts one bubbletea program iteration. Returns the tmux
// session the operator wants to attach to, or "" if they quit.
func runOnce(version string) (string, error) {
	model := New(version)
	prog := tea.NewProgram(model, tea.WithAltScreen())

	// Wire fsnotify on agents/. On any event (CREATE/WRITE/REMOVE/
	// RENAME), Send an fsEventMsg into the bubbletea event loop.
	stop, err := startWatcher(prog)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"warning: fsnotify unavailable, falling back to 1s polling: %v\n", err)
	} else {
		defer stop()
	}

	finalModel, err := prog.Run()
	if err != nil {
		return "", err
	}
	if m, ok := finalModel.(Model); ok {
		return m.PendingAttach(), nil
	}
	return "", nil
}

// runTmuxAttach runs `tmux attach -t <session>` as a child process
// and returns when the operator detaches (Ctrl-b d) or the session
// terminates. stdin/stdout/stderr are wired to fleet's terminal so
// tmux drives the screen directly.
func runTmuxAttach(session string) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	args := []string{"attach", "-t", session}
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		args = append([]string{"-S", sock}, args...)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// startWatcher returns a stop func and an error. On success, the
// watcher goroutine is running and Sends two message types into the
// bubbletea program:
//
//   - fsEventMsg for changes under ~/.fleet/agents/ — drives a refresh
//     of the agent list (cursor stays in bounds, archives disappear).
//   - queueEventMsg for changes under ~/.fleet/queue/ — drives an
//     auto-drain so a fleet-guard auto-handoff queue file landing on
//     disk is processed without operator intervention.
//
// If fsnotify is unsupported on this platform or the watcher can't be
// created, returns the error so the caller can fall back to polling
// (which is wired separately via tickCmd in model.go). Polling does
// NOT cover queue drain — without fsnotify, queue files only process
// when the operator runs `fleet drain` manually or the next handoff
// happens. The TUI's banner surfaces this case via the agent list
// (no records appearing) but it's a known limitation.
func startWatcher(prog *tea.Program) (func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	root, err := state.Root()
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	agentsDir := filepath.Join(root, "agents")
	queueDir := filepath.Join(root, "queue")
	projectsDir := filepath.Join(root, "projects")
	if err := w.Add(agentsDir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch %s: %w", agentsDir, err)
	}
	// projects/ holds per-project coord state for the v0.2 Variant A
	// dashboard. Top-level watch only — catches new project dirs being
	// created; per-project file changes ride on the 1s polling tick.
	// Recursive watching every workers/<slug>/ + tasks.md would explode
	// the watcher count on a multi-project setup; the dashboard scan is
	// cheap (single-digit ms) and the tick cadence is fine for an ops
	// console. Missing dir is non-fatal: Bootstrap creates it, but a
	// fresh install before the first dispatch leaves it absent.
	projectsWatched := true
	if err := w.Add(projectsDir); err != nil {
		projectsWatched = false
		// Quietly fall back to polling; this is expected on fresh
		// installs and not worth a stderr line.
	}
	// queue/ may not exist yet on a fresh install — Bootstrap creates
	// it, but defend against an environment where the operator deleted
	// it manually. fsnotify.Add fails on missing dirs; treat that as a
	// non-fatal warning and let the polling tick + manual `fleet drain`
	// still work.
	queueWatched := true
	if err := w.Add(queueDir); err != nil {
		queueWatched = false
		fmt.Fprintf(os.Stderr,
			"warning: queue/ watcher unavailable (%v) — auto-drain disabled, run `fleet drain` manually\n",
			err)
	}

	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				dir := filepath.Dir(ev.Name)
				switch {
				case dir == agentsDir:
					// Filter to .json so .tmp sidecars during atomic
					// writes don't fire spurious refreshes.
					if filepath.Ext(ev.Name) != ".json" {
						continue
					}
					prog.Send(fsEventMsg{})
				case dir == queueDir && queueWatched:
					// Same .json filter as agents/. spawn-fresh-*.json
					// is the only real shape; anything else is noise.
					if filepath.Ext(ev.Name) != ".json" {
						continue
					}
					prog.Send(queueEventMsg{})
				case dir == projectsDir && projectsWatched:
					// New project dir created — refresh the dashboard
					// snapshot. Per-project file changes fall through
					// to the 1s polling tick (see tickMsg in
					// model.go).
					prog.Send(fsEventMsg{})
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	return func() {
		close(done)
		_ = w.Close()
	}, nil
}
