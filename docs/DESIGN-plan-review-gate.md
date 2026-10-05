# Plan review

## Problem

The task-plan review SOP in `skills/coordinator/SKILL.md` asks the coord to
get every `docs/TASK-PLAN-<slug>.md` dual-reviewed before promote, but
there was no tool for it:

- `review_slot.py` can only review a diff / working tree, so a plan review
  is an ad-hoc prompt with no stable output.
- Results are not persisted, so nobody can tell whether the plan that is
  about to be promoted is the plan that was reviewed.

Target: the same shape as Devin's Plan mode + Devin Review — a Markdown
plan and a structured review of that plan (Bugs / Flags / Security). Like
Devin, the review is advisory: promote is not gated on it, and the
operator's approval stays the SOP step it already is.

## Flow

1. Coord writes `docs/TASK-PLAN-<slug>.md` and links it from the task
   (`Task plan: docs/TASK-PLAN-<slug>.md`).
2. Reviewer subagents run
   `review_slot.py --plan <doc> --project <p> --slug <slug> --both ...`.
   The record lands at `~/.fleet/projects/<p>/plan-reviews/<slug>.json`
   with the sha256 of the reviewed bytes.
   The prompt includes `REVIEW.md` / `AGENTS.md` files whose directory
   covers the repo root, the plan's dir, or any path the plan names
   (files under `.agents/`, `.devin/`, `.cursor/`, `.github/` scope to
   the parent), root-most first so deeper rules read as more specific.
3. The coord applies fixes and re-runs reviews until neither slot reports
   a P0/P1; an edit after review shows as a `doc_sha256` mismatch.

## Review record (`plan-reviews/<slug>.json`)

```json
{
  "schema": 1,
  "project": "p",
  "slug": "p-0001",
  "doc": "/abs/repo/docs/TASK-PLAN-p-0001.md",
  "doc_sha256": "…",
  "reviewed_at": "2026-10-05T18:00:00Z",
  "exit": 0,
  "clean": true,
  "slots": {"alpha": {"engine": "codex", "model": "…", "exit": 0,
                      "skip_reason": null, "error": null},
            "beta":  {"engine": "claude", "model": "…", "exit": 0,
                      "skip_reason": null, "error": null}},
  "findings": [{"severity": "P2", "category": "flag",
                "section": "Steps", "summary": "…", "slot": "alpha"}]
}
```

`clean` is `exit == 0` from `review_slot.py`, so the existing review rules
apply unchanged: a P0/P1 in either slot, a blocked slot, or a skipped beta
anchor makes the record not clean. A skipped alpha helper does not.

## Non-goals

- Auto-running reviews on doc save; the coord still dispatches reviewers.
- Gating `fleet tasks promote` on the review or a recorded approval.
- Reviewing embedded (unlinked) plans: the record needs a doc file to hash.
