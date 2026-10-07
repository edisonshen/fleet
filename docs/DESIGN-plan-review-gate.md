# Plan review gate

## Problem

The task-plan review SOP in `skills/coordinator/SKILL.md` asks the coord to
get every `docs/TASK-PLAN-<slug>.md` dual-reviewed before promote, and to
promote only with operator approval. Nothing enforces either step:

- `review_slot.py` can only review a diff / working tree, so a plan review
  is an ad-hoc prompt with no stable output.
- Results are not persisted, so nobody can tell whether the plan that is
  about to be promoted is the plan that was reviewed.
- `fleet tasks promote` only checks `status=todo`.
- The TUI shows nothing about plan reviews.

Target: the same shape as Devin's Plan mode + Devin Review — a Markdown
plan, a structured review of that plan (Bugs / Flags / Security), and an
explicit approval before any implementation starts.

## Flow

1. Coord writes `docs/TASK-PLAN-<slug>.md` and links it from the task
   (`Task plan: docs/TASK-PLAN-<slug>.md`).
2. Reviewer subagents run
   `review_slot.py --plan <doc> --project <p> --slug <slug> --both ...`.
   The record lands at `~/.fleet/projects/<p>/plan-reviews/<slug>.json`
   with the sha256 of the reviewed bytes.
3. Operator runs `fleet tasks approve <slug>`; it only succeeds when the
   latest review is clean and its sha256 matches the doc on disk, and it
   records the approval against that same sha256.
4. `fleet tasks promote <slug>` refuses unless doc, review and approval
   all agree on the current sha256. `--force <reason>` overrides and is
   recorded.
5. The TUI shows a per-task plan badge and the findings grouped by
   severity and category in the task detail overlay.
6. Reviewers also read scoped `REVIEW.md` / `AGENTS.md` instruction files.

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

`clean` is `exit == 0` from `review_slot.py`, so the existing gate rules
apply unchanged: a P0/P1 in either slot, a blocked slot, or a skipped beta
anchor makes the record not clean. A skipped alpha helper does not.

## Approval record (`plan-reviews/<slug>.approval.json`)

`{doc, doc_sha256, approved_by, approved_at, forced, reason}`. Written by
`fleet tasks approve` (refused from a coord shell, `FLEET_ROLE=coord`) or
by `fleet tasks promote --force <reason>` (`forced: true`).

## Non-goals

- Auto-running reviews on doc save; the coord still dispatches reviewers.
- Reviewing embedded (unlinked) plans: the gate needs a doc file to hash.
