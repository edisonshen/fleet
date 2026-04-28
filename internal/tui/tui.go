package tui

import (
	"fmt"
	"os"
	"path/filepath"

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

	_, err = prog.Run()
	return err
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
