package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"

	"github.com/edisonshen/fleet/internal/state"
)

// Run starts the bubbletea program with the agents/ directory under
// fsnotify supervision. The polling tick (model.go's tickCmd) covers
// the case where fsnotify misbehaves; both signals trigger the same
// agent-list reload.
func Run(version string) error {
	if _, err := state.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap ~/.fleet: %w", err)
	}

	model := New(version)
	prog := tea.NewProgram(model, tea.WithAltScreen())

	// Wire fsnotify on agents/. On any event (CREATE/WRITE/REMOVE/
	// RENAME), Send an fsEventMsg into the bubbletea event loop.
	// The watcher goroutine runs for the program's lifetime; tea.Quit
	// closes prog.Send's downstream so the goroutine exits when its
	// next send blocks (or when the channel is closed by program exit).
	stop, err := startWatcher(prog)
	if err != nil {
		// Non-fatal: polling fallback still works. Log to stderr via
		// the program's message stream so it surfaces in the TUI's
		// error line, then proceed.
		fmt.Fprintf(os.Stderr, "warning: fsnotify unavailable, falling back to 1s polling: %v\n", err)
	} else {
		defer stop()
	}

	finalModel, err := prog.Run()
	if err != nil {
		return err
	}

	// Post-program: if [a] was pressed, exec `tmux attach` so it
	// replaces this process. Doing it here (after bubbletea's
	// altscreen has been torn down) avoids the conflict between
	// tmux's terminal control and bubbletea's render loop.
	if m, ok := finalModel.(Model); ok {
		if session := m.PendingAttach(); session != "" {
			return execTmuxAttach(session)
		}
	}
	return nil
}

// execTmuxAttach replaces the current process with `tmux attach -t
// <session>`. After this returns (which only happens on error),
// control is back in tui.Run's caller.
func execTmuxAttach(session string) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	args := []string{"tmux", "attach", "-t", session}
	if sock := os.Getenv("FLEET_TMUX_SOCKET"); sock != "" {
		args = []string{"tmux", "-S", sock, "attach", "-t", session}
	}
	return syscall.Exec(bin, args, os.Environ())
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
	if err := w.Add(agentsDir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch %s: %w", agentsDir, err)
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
