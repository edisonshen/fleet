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

// checkpointNextStepsMax caps coord-state.json:session_next_steps — the
// explicit free-text Next Steps buffer written by `fleet checkpoint
// next-step`. Its OWN constant (design: coord-state schema); the reader-side
// Next Steps cap (nextStepsLimit) bounds the COMBINED explicit+auto render.
const checkpointNextStepsMax = 10

// sessionTasksMax caps coord-state.json:session_tasks — the auto Next Steps
// buffer (promoted/dispatched slugs). Byte-identical cap to the Python tick
// writer (dispatch._SESSION_TASKS_MAX) so the two co-writers agree.
const sessionTasksMax = 30

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
	cmd.AddCommand(newCheckpointNextStepCmd())
	return cmd
}

func newCheckpointNextStepCmd() *cobra.Command {
	var project, slug string
	cmd := &cobra.Command{
		Use:   "next-step [--slug <slug>] <text>",
		Short: "Record a free-text Next Step for the handoff (session-scoped)",
		Long: `next-step records a free-text line the coordinator plans to do next but
has NOT queued as a task (e.g. "revive codex-engine-mvp — replan closed PR").
It renders under the handoff's Next Steps as ` + "`- [explicit] <text>`" + `,
scoped to THIS coordinator's session (foreign-generation entries are
filtered). Optional --slug exact-dedups the line against the auto block
(a promoted/dispatched task with the same slug renders once, explicit wins).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheckpointNextStep(project, slug, args[0])
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: cwd basename)")
	cmd.Flags().StringVar(&slug, "slug", "", "optional task slug for exact dedup against the auto block")
	return cmd
}

// Security fix (review iter-1): the successor coord's resume parser
// (skills/coordinator/handoff_resume.py) walks the handoff doc via Python's
// str.splitlines(), which treats U+2028 (LINE SEPARATOR), U+2029 (PARAGRAPH
// SEPARATOR), U+0085 (NEL), \v, \f, and \x1c-\x1e as line breaks — NOT just
// \r/\n. A next-step string carrying one of those runes passed the old
// \r\n-only flatten untouched, yet split into multiple logical lines on the
// Python reader, letting free text forge a `## Active Subagents` / `## Open
// PRs` header BEFORE the real ones (Next Steps renders at position 6, ahead
// of Active Subagents at 7 and Open PRs at 8 — see internal/handoff/handoff.go
// Render). Because both Python section scanners break on the first line
// starting `## ` once inside a section, a forged header + forged entries
// followed by a forged next header would swallow the parse and the real
// Active Subagents / Open PRs sections would never be read — silently
// dropping in-flight worker resume + substituting attacker-chosen PR URLs.
// flattenLineBreaks replaces every rune Python's str.splitlines() treats as
// a line terminator with a space, matching the successor's actual notion of
// "line" (not just Go's \r\n) so no forged section header can survive.
func flattenLineBreaks(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\v', '\f', // ASCII: CR, LF, vertical tab, form feed
			'\u001c', '\u001d', '\u001e', // File/Group/Record separator
			'\u0085', // NEL (Next Line)
			'\u2028', // LINE SEPARATOR
			'\u2029': // PARAGRAPH SEPARATOR
			return ' '
		default:
			return r
		}
	}, s)
}

// runCheckpointNextStep appends {text, slug?, coord_id, ts} to
// session_next_steps, deduped by exact text (last wins → tail) and capped to
// the newest checkpointNextStepsMax.
func runCheckpointNextStep(project, slug, text string) error {
	// Flatten every line-break rune the successor coord's resume parser
	// recognizes (see flattenLineBreaks) — identical guard as the decision /
	// doc buffers. The handoff renders the explicit block body as a fixed
	// `## H\n%s\n\n` block with no per-line fencing, so an embedded line
	// break could forge a `## header` the successor coord reads as trusted.
	// This sanitization is load-bearing, not cosmetic.
	flat := strings.TrimSpace(flattenLineBreaks(text))
	if flat == "" {
		return errors.New("next-step text must be non-empty")
	}
	slug = strings.TrimSpace(flattenLineBreaks(slug))
	return withCoordState(project, func(cs map[string]any) {
		steps := toSlice(cs["session_next_steps"])
		// Dedupe by exact text — drop any prior entry so the new one wins AND
		// moves to the newest (tail) position.
		kept := steps[:0]
		for _, e := range steps {
			m, ok := e.(map[string]any)
			if ok && m["text"] == flat {
				continue
			}
			kept = append(kept, e)
		}
		entry := map[string]any{
			"text": flat,
			"ts":   time.Now().UTC().Format(time.RFC3339),
		}
		if slug != "" {
			entry["slug"] = slug
		}
		// Generation stamp: coord-state.json survives succession, so the
		// reader (CollectNextSteps) drops entries stamped by a DIFFERENT
		// coord. Empty FLEET_AGENT_ID (operator shell) leaves the entry
		// unstamped → unfiltered, mirroring runCheckpointDoc.
		if id := os.Getenv("FLEET_AGENT_ID"); id != "" {
			entry["coord_id"] = id
		}
		kept = append(kept, entry)
		if len(kept) > checkpointNextStepsMax {
			kept = kept[len(kept)-checkpointNextStepsMax:]
		}
		cs["session_next_steps"] = kept
	})
}

// appendSessionTask records slug into coord-state.json:session_tasks (the
// auto Next Steps buffer), deduped by slug (coord_id/ts refreshed, moved to
// tail) and capped to sessionTasksMax. Stamps FLEET_AGENT_ID as coord_id.
// Takes coordinator.lock via withCoordState.
//
// `fleet tasks promote` / `fleet tasks set` call this SEQUENTIALLY AFTER
// releasing the tasks lock (never nested) so the coordinator→state lock order
// the live tick uses is never inverted here (state→coordinator would AB-BA vs
// a coincident `fleet tasks set`).
//
// FAIL-FAST (codex review P1): the acquire is a single non-blocking LOCK_NB
// attempt, NOT a bounded wait. `fleet tasks set status=…/parked=…` (and
// `promote`) can be spawned BY THE LIVE COORD TICK — loop.py's
// _apply_reconcile / _set_parked / reaper requeue shell out to `fleet tasks
// set` WHILE the tick holds coordinator.lock. A blocking acquire there would
// stall the child for the full timeout on EVERY such transition. Fail-fast
// returns instantly; a lock-contention miss is CORRECT because a tick-owned
// slug was already stamped at its dispatch seam (loop.py _record_session_task)
// and the reader overlays live tasks.md status, so the status change still
// renders. Only a MANUAL `fleet tasks set` (no tick, lock free) needs — and
// gets — the append. Contention returns nil (silent, expected); only a
// genuine I/O error propagates so the caller can log it.
func appendSessionTask(project, slug string) error {
	err := withCoordStateTimeout(project, 0, func(cs map[string]any) {
		recordSessionTaskEntry(cs, slug, os.Getenv("FLEET_AGENT_ID"))
	})
	if err != nil && errors.Is(err, state.ErrLockTimeout) {
		return nil // coord tick (or a coincident writer) owns the lock; skip
	}
	return err
}

// recordSessionTaskEntry mutates cs["session_tasks"] in place: dedupe by
// slug (drop any prior entry so coord_id/ts refresh + the entry moves to the
// tail), append {slug, coord_id, ts}, cap to sessionTasksMax. Keys are
// byte-identical to the Python writer (dispatch.record_session_task) so the
// shared buffer round-trips across both co-writers.
func recordSessionTaskEntry(cs map[string]any, slug, coordID string) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return
	}
	tasksBuf := toSlice(cs["session_tasks"])
	kept := tasksBuf[:0]
	for _, e := range tasksBuf {
		m, ok := e.(map[string]any)
		if ok && m["slug"] == slug {
			continue
		}
		kept = append(kept, e)
	}
	entry := map[string]any{
		"slug": slug,
		"ts":   time.Now().UTC().Format(time.RFC3339),
	}
	if coordID != "" {
		entry["coord_id"] = coordID
	}
	kept = append(kept, entry)
	if len(kept) > sessionTasksMax {
		kept = kept[len(kept)-sessionTasksMax:]
	}
	cs["session_tasks"] = kept
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
		entry := map[string]any{
			"path": path,
			"role": role,
			"ts":   time.Now().UTC().Format(time.RFC3339),
		}
		// Generation stamp: coord-state.json survives coord succession, so
		// the reader (CollectSessionDocs) filters entries stamped by a
		// DIFFERENT coord — the same guard loadCheckpointIfFresher applies
		// to the checkpoint. An empty FLEET_AGENT_ID (operator shell) leaves
		// the entry unstamped → unfiltered, mirroring the checkpoint guard's
		// empty-coord_id handling.
		if id := os.Getenv("FLEET_AGENT_ID"); id != "" {
			entry["coord_id"] = id
		}
		kept = append(kept, entry)
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
		// Generation stamp for the LIVE read. recent_decisions is a plain-
		// strings buffer shared with the Python tick producer (per-entry
		// stamping would break the coord-checkpoint.md round-trip), so the
		// stamp is a top-level sibling key: the last CLI writer's coord
		// generation. CollectRecentDecisionsLive suppresses the live
		// override when this stamp belongs to a different coord — the
		// checkpoint fallback then applies its own coord_id guard. The tick
		// preserves unknown keys through load-mutate-save, so the stamp
		// rides through heartbeats untouched. Empty FLEET_AGENT_ID
		// (operator shell) leaves any prior stamp in place rather than
		// erasing attribution.
		if id := os.Getenv("FLEET_AGENT_ID"); id != "" {
			cs["recent_decisions_owner"] = id
		}
	})
}

// withCoordState resolves the project, takes coordinator.lock (bounded by
// coordLockTimeout), and applies mutate to a read coord-state.json dict, then
// atomically writes it back. Load-mutate-save the WHOLE dict (never
// reconstruct) so sibling keys the tick owns ride through untouched. Used by
// the operator/agent-invoked provenance writers (`fleet checkpoint …`) that
// are NEVER called from inside a live tick, so blocking-with-timeout is safe.
func withCoordState(project string, mutate func(map[string]any)) error {
	return withCoordStateTimeout(project, coordLockTimeout, mutate)
}

// withCoordStateTimeout is withCoordState with a caller-chosen lock timeout.
// A non-positive timeout means a single fail-fast LOCK_NB attempt (see
// state.LockCoordinatorTimeout) — used by appendSessionTask, which can be
// invoked by a `fleet tasks set/promote` child process the LIVE COORD TICK
// spawned WHILE the tick itself already holds coordinator.lock (a separate
// open file description → genuine self-contention). Blocking there would
// stall every dispatch/requeue transition for the full timeout; fail-fast
// skips instantly (codex review P1).
func withCoordStateTimeout(project string, timeout time.Duration, mutate func(map[string]any)) error {
	proj, err := resolveProject(project)
	if err != nil {
		return err
	}
	pdir, err := state.ProjectDir(proj)
	if err != nil {
		return err
	}
	csPath := filepath.Join(pdir, "coord-state.json")

	release, err := state.LockCoordinatorTimeout(proj, timeout)
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
