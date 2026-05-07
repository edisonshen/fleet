package main

// fleet peek <slug> [--follow] [--logs] — operator's narrow window into
// one worker. Default prints the parsed state.json shape (the same
// fields `workers list` shows but for a single worker, plus blocked
// reason / pr_url / exit). --logs tails output.log; --follow keeps
// reading until the worker reaches a terminal phase or the operator
// hits Ctrl-C.
//
// Implementation notes:
//   - We do NOT shell out to `tail -f`. Doing so would make the binary
//     depend on tail's BSD/GNU dialect quirks (--follow, -F, -f options
//     differ across platforms). A simple poll-and-print loop with a
//     bounded sleep is dependency-free and behaves identically across
//     macOS / Linux.
//   - --follow on a missing state.json waits for it to appear (up to
//     followTimeout). This matches the coordinator-launches-worker
//     race: operator runs `peek --follow <slug>` while coord is still
//     setting up the dir.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/workers"
)

// pollInterval is the cadence of state-watch reads. Short enough that
// human-perceptible phase changes feel live; long enough that the loop
// barely registers in CPU usage.
const pollInterval = 500 * time.Millisecond

// followTimeout caps how long --follow waits for a missing state.json
// to appear before giving up. Coords typically seed the dir within
// hundreds of ms; 30s is generous.
const followTimeout = 30 * time.Second

type peekOpts struct {
	project string
	follow  bool
	logs    bool
}

func newPeekCmd() *cobra.Command {
	opts := &peekOpts{}
	cmd := &cobra.Command{
		Use:   "peek <slug>",
		Short: "Inspect one worker's state.json (and optionally tail output.log)",
		Long: `peek prints the worker's state.json (parsed: slug, phase, pid,
pr_url, started_at, updated_at, blocked_reason, exit). Pass --logs to
also tail the worker's output.log; --follow keeps printing state and
log updates until the worker reaches done | blocked | failed (or the
operator hits Ctrl-C).

Workers are NOT Fleet agents (they run as ` + "`claude --print`" + `
subprocesses launched by the coordinator), so they don't show up in
` + "`fleet status`" + ` or ` + "`fleet attach`" + `. peek is the
operator-side window.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPeek(opts, args[0], cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().BoolVarP(&opts.follow, "follow", "f", false, "stream state + (with --logs) log updates until terminal phase")
	cmd.Flags().BoolVar(&opts.logs, "logs", false, "also print output.log (with --follow, tails)")
	return cmd
}

func runPeek(opts *peekOpts, slug string, stdout, stderr io.Writer) error {
	project, err := resolveProject(opts.project)
	if err != nil {
		return err
	}
	if !opts.follow {
		return peekOnce(project, slug, opts.logs, stdout)
	}
	// Set up Ctrl-C cancellation. The signal handler restores default
	// behavior on stop so a stuck second Ctrl-C still kills the process.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return peekFollow(ctx, project, slug, opts.logs, stdout, stderr)
}

// peekOnce reads state.json (and optionally output.log) once and
// returns. Looks up the active worker dir first; falls back to the
// archive (codex iter-2 P2) so a slug `workers list --all` advertises
// is always inspectable. ENOENT in BOTH locations is a hard error.
func peekOnce(project, slug string, withLogs bool, stdout io.Writer) error {
	st, dir, err := readWorkerStateAnywhere(project, slug)
	if err != nil {
		if errors.Is(err, workers.ErrNotFound) {
			return fmt.Errorf("no worker %q in project %q (try `fleet workers list --all --project %s`)", slug, project, project)
		}
		return fmt.Errorf("read state: %w", err)
	}
	if err := writeStateBlock(stdout, st); err != nil {
		return err
	}
	if withLogs {
		logPath := filepath.Join(dir, "output.log")
		if err := dumpLog(stdout, logPath); err != nil {
			// Missing log is informational, not fatal — workers may not
			// have written anything yet (or the archive path is partial).
			if errors.Is(err, os.ErrNotExist) {
				_, _ = fmt.Fprintf(stdout, "\n(no output.log yet)\n")
				return nil
			}
			return fmt.Errorf("read output.log: %w", err)
		}
	}
	return nil
}

// readWorkerStateAnywhere returns the worker state from the active
// directory if present, otherwise from the most-recent archive entry
// for that slug. Second return is the directory the state was read
// from (so callers can locate sibling files like output.log).
//
// Archive directories are named `<slug>-YYYYMMDD-HHMMSS[-N]`; we pick
// the lexicographically-largest match (which equals "newest" since the
// stamp is fixed-width). ErrNotFound when neither path resolves.
func readWorkerStateAnywhere(project, slug string) (*workers.State, string, error) {
	// Try the active dir first — same code path as workers.ReadState.
	st, err := workers.ReadState(project, slug)
	switch {
	case err == nil:
		dir, derr := state.WorkerDir(project, slug)
		if derr != nil {
			return nil, "", derr
		}
		// state.WorkerDir returns a trailing-separator path; strip for
		// downstream filepath.Join.
		dir = strings.TrimSuffix(dir, string(filepath.Separator))
		return st, dir, nil
	case errors.Is(err, workers.ErrNotFound):
		// fall through to archive lookup
	default:
		return nil, "", err
	}
	return readArchivedWorkerState(project, slug)
}

// readArchivedWorkerState scans workers/archive/ for the newest entry
// whose name starts with "<slug>-" and whose suffix is the canonical
// archive stamp. Returns ErrNotFound if no match.
func readArchivedWorkerState(project, slug string) (*workers.State, string, error) {
	pdir, err := state.ProjectDir(project)
	if err != nil {
		return nil, "", err
	}
	archRoot := filepath.Join(pdir, "workers", "archive")
	entries, err := os.ReadDir(archRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", workers.ErrNotFound
		}
		return nil, "", fmt.Errorf("readdir archive: %w", err)
	}
	prefix := slug + "-"
	var newest string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Lexicographic order = stamp order (fixed-width
		// YYYYMMDD-HHMMSS plus optional -N salt).
		if name > newest {
			newest = name
		}
	}
	if newest == "" {
		return nil, "", workers.ErrNotFound
	}
	dir := filepath.Join(archRoot, newest)
	stPath := filepath.Join(dir, "state.json")
	data, err := os.ReadFile(stPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", workers.ErrNotFound
		}
		return nil, "", fmt.Errorf("read archive state: %w", err)
	}
	var st workers.State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, "", fmt.Errorf("unmarshal archive state: %w", err)
	}
	return &st, dir, nil
}

// peekFollow polls state.json every pollInterval, prints on change,
// and (when withLogs is true) appends new log bytes since the last
// read. Exits when:
//   - phase reaches done | blocked | failed
//   - context is canceled (Ctrl-C)
//   - state.json never appears within followTimeout AT STARTUP
//   - state.json disappears after first read AND archive has the slug
//     (codex iter-2 P1: a fast-finishing worker the coord archives
//     between two polls would otherwise time out at 30s)
func peekFollow(ctx context.Context, project, slug string, withLogs bool, stdout, stderr io.Writer) error {
	logPath, err := workerLogPath(project, slug)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(followTimeout)

	var lastPhase workers.Phase
	var lastUpdated time.Time
	var logOffset int64
	first := true
	seenOnce := false

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		st, err := workers.ReadState(project, slug)
		switch {
		case errors.Is(err, workers.ErrNotFound):
			if seenOnce {
				// Worker dir disappeared after we'd already seen it —
				// the coord must have archived. Promote to a one-shot
				// archive read + clean exit so the operator gets a
				// final terminal-phase render rather than the 30s
				// startup-timeout error (codex iter-2 P1).
				archSt, archDir, archErr := readArchivedWorkerState(project, slug)
				if archErr != nil {
					if errors.Is(archErr, workers.ErrNotFound) {
						_, _ = fmt.Fprintf(stderr, "note: worker %q vanished and has no archive entry yet — coord may still be archiving\n", slug)
						return nil
					}
					return archErr
				}
				if !first {
					_, _ = fmt.Fprintln(stdout, "---")
				}
				if werr := writeStateBlock(stdout, archSt); werr != nil {
					return werr
				}
				if withLogs {
					_, _ = tailLogSince(stdout, filepath.Join(archDir, "output.log"), logOffset)
				}
				return nil
			}
			// Wait for the worker dir to appear. After followTimeout,
			// give up so a typo'd slug doesn't hang the operator
			// indefinitely.
			if time.Now().After(deadline) {
				return fmt.Errorf("no worker %q in project %q after %s of waiting", slug, project, followTimeout)
			}
			// fall through to ticker wait below
		case err != nil:
			_, _ = fmt.Fprintf(stderr, "warning: read state: %v\n", err)
		default:
			seenOnce = true
			// Reset deadline once we've seen state at least once — a
			// long-running worker shouldn't be terminated because it
			// stops updating for 30s.
			deadline = time.Now().Add(followTimeout)

			// Print the state block when phase or updated_at changed
			// (or on the first iteration).
			if first || st.Phase != lastPhase || !st.UpdatedAt.Equal(lastUpdated) {
				if !first {
					_, _ = fmt.Fprintln(stdout, "---")
				}
				if err := writeStateBlock(stdout, st); err != nil {
					return err
				}
				lastPhase = st.Phase
				lastUpdated = st.UpdatedAt
				first = false
			}

			if withLogs {
				next, err := tailLogSince(stdout, logPath, logOffset)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					_, _ = fmt.Fprintf(stderr, "warning: read log: %v\n", err)
				}
				logOffset = next
			}

			// Terminal-phase exit. We want the final state block to be
			// the LAST thing printed, hence the early return after the
			// print above.
			if isTerminalPhase(st.Phase) {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// isTerminalPhase mirrors internal/workers.isTerminalPhase (which is
// unexported). v0.2 uses the same enum on both sides; if it diverges
// in v0.3 the exporter wins.
func isTerminalPhase(p workers.Phase) bool {
	switch p {
	case workers.PhaseDone, workers.PhaseBlocked, workers.PhaseFailed:
		return true
	}
	return false
}

// writeStateBlock prints a human-readable rendering of the state.json
// — an indented JSON dump preserves all fields but is more readable
// than the raw bytes (json.MarshalIndent expands maps and slices).
func writeStateBlock(w io.Writer, st *workers.State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n")
	return err
}

// dumpLog writes the entire output.log to w. Missing file returns
// os.ErrNotExist (caller handles gracefully).
func dumpLog(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}

// tailLogSince reads from offset to EOF and writes to w. Returns the
// new offset (caller passes back on next tick) so we don't re-print
// already-seen bytes. Missing file returns (offset, os.ErrNotExist).
func tailLogSince(w io.Writer, path string, offset int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		// Truncation? Reset and read everything. (Workers don't
		// truncate their own logs in v0.2, but coords might rotate in
		// v0.3 — defensive.)
		offset = 0
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return offset, err
		}
	}
	n, err := io.Copy(w, f)
	return offset + n, err
}

// workerLogPath returns the canonical output.log path for a worker.
func workerLogPath(project, slug string) (string, error) {
	dir, err := state.WorkerDir(project, slug)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "output.log"), nil
}
