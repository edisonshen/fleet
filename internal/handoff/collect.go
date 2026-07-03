package handoff

// collect.go — Slice 3 Files Modified collector for the manual handoff
// doc. The coordinator's real file footprint is the plan docs it authored
// (docs/DESIGN-*.md, TASK-PLAN-*.md, and their gitignored *.html renders),
// read LIVE from the project repo's git working tree at handoff time — no
// synchronous prompt to a possibly-dead agent, no buffer to feed.
//
//	section          source                          path
//	-------          ------                          ----
//	Files Modified   git status --ignored=matching   CollectFilesModified (manual ONLY)
//
// (Next Steps + Open Questions come from tasks.md via CollectNextSteps /
// CollectOpenQuestions in enrich.go; Completed + Key Decisions come from the
// rolling checkpoint buffers via applyCheckpointToDoc — not from here.)
//
// The collector is best-effort: a non-git repo, a dead `git`, an empty
// repoDir, or empty output returns "" so the caller leaves the section's
// existing placeholder. Enrichment NEVER fails a handoff.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// sessionDocsMax caps how many "Docs (this session)" rows render in a
// handoff doc so a long-lived coord that authored a large number of plan
// docs can't bloat the section. Its own constant, INDEPENDENT of the
// recent_decisions env cap (resolve_checkpoint_decisions, default 10) —
// the two buffers cap separately (design: coord-state schema).
const sessionDocsMax = 20

// sessionDoc is one entry of coord-state.json:session_docs — a plan doc
// the coordinator authored or is actively implementing this session. The
// Go CLI (`fleet checkpoint doc`) is the only writer; the handoff
// collector below is the reader.
type sessionDoc struct {
	Path string `json:"path"`
	Role string `json:"role"`
	TS   string `json:"ts"`
}

// CollectSessionDocs renders the `## Docs (this session)` body from
// coord-state.json:session_docs — the plan docs THIS coordinator authored
// or is implementing, recorded live via `fleet checkpoint doc`. Each entry
// renders `- <role>: <path>`; the newest sessionDocsMax are shown with a
// `- … and N more` tail when the list overflows.
//
// This replaces the retired CollectFilesModified whole-repo git dump: the
// old collector rendered EVERY untracked doc under docs/ (every past
// coord's litter), while this shows only what the live coord touched.
//
// Never errors: a missing / malformed coord-state.json, an empty
// coordStatePath, or an absent/empty session_docs key returns "" so the
// caller keeps the section's placeholder. Enrichment NEVER fails a handoff.
func CollectSessionDocs(coordStatePath string) string {
	// TODO(tdd-green): implement.
	return ""
}

// CollectRecentDecisionsLive reads coord-state.json:recent_decisions LIVE
// (the same buffer the tick auto-producer AND `fleet checkpoint decision`
// both feed) and returns it as an ordered slice. The handoff paths prefer
// this over the tick-published coord-checkpoint.md value so an agent
// rationale logged out-of-band (no tick between the log and the handoff)
// still reaches Key Decisions — the motivating no-tick case.
//
// Never errors: a missing / malformed coord-state.json, an empty
// coordStatePath, or an absent/empty recent_decisions key returns nil so
// the caller keeps whatever the checkpoint lift already set.
func CollectRecentDecisionsLive(coordStatePath string) []string {
	// TODO(tdd-green): implement.
	return nil
}

// gitTimeout caps the `git status` shell-out, mirroring ghTimeout.
const gitTimeout = 10 * time.Second

// filesModifiedMax caps how many entries render under Files Modified so a
// large untracked subtree under docs/ (e.g. a scratch dir) can't dump
// thousands of bullet lines into the handoff doc. Mirrors the Python
// recent_decisions cap discipline; the successor re-reads the working tree
// anyway, so this section is operator-readable triage context.
const filesModifiedMax = 50

// gitRunner runs `git` in dir with args under a gitTimeout deadline,
// returning stdout. Production uses runGit; tests inject a fake so the
// non-git / failure paths are exercised deterministically.
var gitRunner = runGit

func runGit(dir string, args ...string) ([]byte, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not on PATH: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CollectFilesModified renders the coordinator's plan-doc footprint: the
// uncommitted/ignored entries under docs/ in repoDir, via `git -C
// <repoDir> status --porcelain --untracked-files=all --ignored=matching --
// docs/`. This pulls in tracked DESIGN-*.md / TASK-PLAN-*.md changes,
// freshly-authored (untracked) plan docs listed INDIVIDUALLY (not
// collapsed to a bare `docs/`), AND the gitignored docs/*.html companions
// (--ignored=matching includes ignored files matching the pathspec).
//
// Each `XY <path>` porcelain line renders `- <path> (<XY>)`, capped at
// filesModifiedMax entries with a `- … and N more` tail so a large untracked
// docs/ subtree can't bloat the doc. Never errors: a non-git repo, a `git`
// failure, an empty repoDir, or empty output returns "" and the caller keeps
// the placeholder.
//
// MANUAL PATH ONLY — synth.go is contractually subprocess-free, so it
// leaves Files Modified at its placeholder (the Open PRs precedent).
func CollectFilesModified(repoDir string) string {
	if strings.TrimSpace(repoDir) == "" {
		return ""
	}
	out, err := gitRunner(
		repoDir,
		"-C", repoDir,
		// core.quotePath=false: emit UTF-8 paths literally instead of
		// C-quoting non-ASCII bytes (a `docs/café.md` would otherwise render
		// as `"docs/caf\303\251.md"` gibberish in the handoff doc).
		"-c", "core.quotePath=false",
		"status", "--porcelain",
		// List untracked files INDIVIDUALLY. Without this, git collapses a
		// fully-untracked docs/ directory to a single `?? docs/` entry — and
		// a freshly-authored plan doc in an otherwise-clean docs/ is exactly
		// that case, so the successor would see `docs/` instead of the file.
		"--untracked-files=all",
		// Include the gitignored docs/*.html render companions.
		"--ignored=matching",
		"--", "docs/",
	)
	if err != nil || len(out) == 0 {
		return ""
	}
	var b strings.Builder
	rendered := 0
	total := 0
	for _, raw := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, "\n")
		if len(line) < 4 {
			// porcelain v1: "XY path" — 2 status chars + space + path.
			continue
		}
		code := strings.TrimSpace(line[:2])
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		total++
		if rendered >= filesModifiedMax {
			// Keep counting so the tail line reports the true overflow.
			continue
		}
		// Renamed entries render `orig -> new`; keep the whole tail.
		if rendered > 0 {
			b.WriteByte('\n')
		}
		rendered++
		fmt.Fprintf(&b, "- %s (%s)", path, code)
	}
	if rendered == 0 {
		return ""
	}
	if total > rendered {
		fmt.Fprintf(&b, "\n- … and %d more", total-rendered)
	}
	return b.String()
}
