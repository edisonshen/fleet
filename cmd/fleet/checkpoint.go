package main

// fleet checkpoint {doc,decision} — the coordinator's provenance writer for
// the curated-handoff sections. Each subcommand takes coordinator.lock and
// read-modify-writes ~/.fleet/projects/<project>/coord-state.json atomically,
// so a doc/decision the agent records while alive survives into its handoff
// doc (Docs (this session) + Key Decisions) even when the coord later dies.
//
// This is the ONLY writer of coord-state.json:session_docs and a SECOND
// writer of recent_decisions (alongside the Python tick auto-producer). The
// Python tick just PRESERVES both keys through its load-mutate-save of the
// whole dict; it never mutates them.
//
//	fleet checkpoint doc --role <authored|implementing> <path>
//	   └─ lock coordinator.lock → RMW session_docs (dedupe by path, cap 20)
//	fleet checkpoint decision "<what> — <why>"
//	   └─ lock coordinator.lock → RMW recent_decisions (cap = decisions env)
//
// Best-effort discipline lives at the READER (the handoff collectors degrade
// to a placeholder). The WRITER surfaces failures (bad role, lock contention)
// so a missed record is visible to the agent, not silently swallowed.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/state"
)

// checkpointSessionDocsMax caps coord-state.json:session_docs. Its OWN
// constant, independent of the recent_decisions env cap — the two buffers
// cap separately (design: coord-state schema). Mirrors the reader-side
// handoff.sessionDocsMax; kept as a local const so the two layers don't take
// a cross-package dependency just to share the number 20.
const checkpointSessionDocsMax = 20

// checkpointDefaultDecisions is the recent_decisions cap when
// FLEET_COORD_CHECKPOINT_DECISIONS is unset/invalid — matches
// dispatch.resolve_checkpoint_decisions() so the Go CLI and the Python tick
// producer cap the shared buffer identically.
const checkpointDefaultDecisions = 10

// coordLockTimeout bounds the coordinator.lock acquire in `fleet checkpoint`.
// A coord mid-tick holds the lock for its whole pass; failing fast (with a
// short bounded retry) keeps the agent's turn responsive and lets it retry,
// rather than blocking. Mirrors register_subagent._take_coord_lock's
// LOCK_NB + brief-retry budget.
const coordLockTimeout = 2 * time.Second

func newCheckpointCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Record coordinator provenance (session docs / decisions) into coord-state.json",
		Long: `checkpoint records the outgoing coordinator's provenance so its handoff
doc carries curated context instead of a whole-repo git dump.

  fleet checkpoint doc --role authored     docs/DESIGN-foo.md
  fleet checkpoint doc --role implementing docs/TASK-PLAN-foo.md
  fleet checkpoint decision "Stopped rebase of PR #224 — superseded, task paused"

Both take coordinator.lock and atomically read-modify-write
~/.fleet/projects/<project>/coord-state.json.`,
	}
	cmd.AddCommand(newCheckpointDocCmd())
	cmd.AddCommand(newCheckpointDecisionCmd())
	return cmd
}

func newCheckpointDocCmd() *cobra.Command {
	var project, role string
	cmd := &cobra.Command{
		Use:   "doc --role <authored|implementing> <path>",
		Short: "Record a plan doc this coordinator authored or is implementing",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckpointDoc(project, role, args[0])
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&role, "role", "", "doc role: authored | implementing")
	return cmd
}

func newCheckpointDecisionCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "decision <text>",
		Short: "Append an agent-rationale line to Key Decisions (recent_decisions)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckpointDecision(project, args[0])
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: cwd basename)")
	return cmd
}

// runCheckpointDoc appends {path, role, ts} to session_docs, deduped by path
// (last role wins) and capped to the newest checkpointSessionDocsMax.
func runCheckpointDoc(project, role, path string) error {
	switch role {
	case "authored", "implementing":
	default:
		return fmt.Errorf("--role must be authored or implementing, got %q", role)
	}
	// Flatten CR/LF to spaces BEFORE trimming. A doc path is rendered
	// verbatim into the handoff doc as `- <role>: <path>`; an embedded
	// newline would forge a fake `## Section` header (or duplicate a real
	// one) inside a document the successor coord reads as trusted
	// instructions. Mirrors runCheckpointDecision's identical flatten of the
	// decision text — Render concatenates fixed `## H\n%s\n\n` blocks with no
	// per-section fencing, so sanitizing at the writer is the load-bearing
	// guard.
	path = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(path, "\r", " "), "\n", " "))
	if path == "" {
		return errors.New("doc path must be non-empty")
	}
	return withCoordState(project, func(cs map[string]any) {
		docs := toSlice(cs["session_docs"])
		// Dedupe by path — drop any prior entry so the new role wins AND the
		// entry moves to the newest (tail) position.
		kept := docs[:0]
		for _, e := range docs {
			m, ok := e.(map[string]any)
			if ok && m["path"] == path {
				continue
			}
			kept = append(kept, e)
		}
		kept = append(kept, map[string]any{
			"path": path,
			"role": role,
			"ts":   time.Now().UTC().Format(time.RFC3339),
		})
		if len(kept) > checkpointSessionDocsMax {
			kept = kept[len(kept)-checkpointSessionDocsMax:]
		}
		cs["session_docs"] = kept
	})
}

// runCheckpointDecision appends one flattened line to recent_decisions,
// capped to the FLEET_COORD_CHECKPOINT_DECISIONS limit (shared with the
// Python tick producer).
func runCheckpointDecision(project, text string) error {
	flat := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " "))
	if flat == "" {
		return errors.New("decision text must be non-empty")
	}
	capN := resolveCheckpointDecisions()
	return withCoordState(project, func(cs map[string]any) {
		raw := toStringSlice(cs["recent_decisions"])
		raw = append(raw, flat)
		if capN > 0 && len(raw) > capN {
			raw = raw[len(raw)-capN:]
		}
		// Re-marshal as []any so the round-trip type matches the tick writer.
		out := make([]any, len(raw))
		for i, s := range raw {
			out[i] = s
		}
		cs["recent_decisions"] = out
	})
}

// withCoordState resolves the project, takes coordinator.lock, and applies
// mutate to a read coord-state.json dict, then atomically writes it back.
// Load-mutate-save the WHOLE dict (never reconstruct) so sibling keys the
// tick owns ride through untouched.
func withCoordState(project string, mutate func(map[string]any)) error {
	proj, err := resolveProject(project)
	if err != nil {
		return err
	}
	pdir, err := state.ProjectDir(proj)
	if err != nil {
		return err
	}
	csPath := filepath.Join(pdir, "coord-state.json")

	release, err := state.LockCoordinatorTimeout(proj, coordLockTimeout)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	defer release()

	cs := readCoordStateDict(csPath)
	mutate(cs)
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal coord-state.json: %w", err)
	}
	if werr := state.WriteAtomic(csPath, data); werr != nil {
		return fmt.Errorf("write coord-state.json: %w", werr)
	}
	return nil
}

// readCoordStateDict loads coord-state.json into a dict. A missing or
// malformed file yields an empty dict — mirroring register_subagent's
// _read_coord_state (the tick writer resets a corrupt file the same way);
// the lock guarantees no concurrent writer is mid-flight, so the only way to
// read garbage is genuine corruption, which the next tick would also reset.
func readCoordStateDict(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// toSlice coerces a JSON value to []any (nil / wrong-type → empty slice).
func toSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// toStringSlice coerces a JSON value to []string, dropping non-string
// elements (a hand-corrupted buffer never poisons the append).
func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// resolveCheckpointDecisions mirrors dispatch.resolve_checkpoint_decisions():
// FLEET_COORD_CHECKPOINT_DECISIONS as a non-negative int, default 10 on
// unset/invalid/negative. 0 means "unbounded" (the Python producer's
// `cap > 0` guard).
func resolveCheckpointDecisions() int {
	raw := os.Getenv("FLEET_COORD_CHECKPOINT_DECISIONS")
	if raw == "" {
		return checkpointDefaultDecisions
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return checkpointDefaultDecisions
	}
	return n
}
