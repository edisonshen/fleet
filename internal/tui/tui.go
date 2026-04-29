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
// watcher goroutine is running and will Send fsEventMsg into the
// bubbletea program for any change under ~/.fleet/agents/.
//
// If fsnotify is unsupported on this platform or the watcher can't be
// created, returns the error so the caller can fall back to polling
// (which is wired separately via tickCmd in model.go).
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
	if err := w.Add(agentsDir); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("watch %s: %w", agentsDir, err)
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
				// Filter: only react to .json file events. fsnotify
				// fires events on the directory itself sometimes, and
				// on .tmp.<pid> sidecars during atomic writes — both
				// are noise.
				if filepath.Ext(ev.Name) != ".json" {
					continue
				}
				prog.Send(fsEventMsg{})
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
				// fsnotify error: fall through. Polling tick still
				// covers us. We don't surface this to the user — too
				// noisy on the platforms that fire spurious errors.
			}
		}
	}()

	return func() {
		close(done)
		_ = w.Close()
	}, nil
}
