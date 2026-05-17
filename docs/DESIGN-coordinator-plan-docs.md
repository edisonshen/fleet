# Coordinator Plan Docs

Approved at: 2026-05-17T18:20:25Z

## Summary

Fleet coordinators previously moved directly from an approved chat plan to task
splitting:

```text
DISCUSS -> SPLIT -> TASK LIST -> IMPLEMENT -> PR-TRACK -> DONE
```

That loses the plan as a durable artifact, and it gives workers only a task row
instead of a durable task-level brief. The corrected workflow inserts mandatory
`PLAN-DOC` and `TASK-PLAN-DOC` gates:

```text
DISCUSS -> PLAN-DOC -> SPLIT -> TASK LIST -> TASK-PLAN-DOC -> IMPLEMENT -> PR-TRACK -> DONE
```

The coordinator saves approved implementation plans to the project's docs
folder before filing tasks. It then saves a worker-ready task plan doc before
promoting any task to implementation. These docs become durable sources for
handoffs, review, and future projdoc publishing.

## Design Decisions

1. The coordinator may write plan docs, but may not implement code inline.
   This is a narrow mutation exception to the coordinator role boundary.
2. The gate applies only to implementation plans that lead to tasks. Casual
   discussion, status checks, and exploratory Q&A do not need docs.
3. The default target is the active project's `docs/` folder when one exists.
   If the project has no clear docs location, the coordinator asks before
   writing.
4. Markdown is the source of truth. Implementation plans use
   `DESIGN-<kebab-topic>.md`; task plans use `TASK-PLAN-<slug>.md`.
5. If the project has a renderer such as `scripts/render-design-doc.py`, the
   coordinator also writes the matching `.html` next to each Markdown file.
6. If the design doc save/render fails, the coordinator raises hand and does
   not proceed to task splitting.
7. The task plan must be worker-visible before promotion. The coordinator
   either embeds the plan in Spec/Acceptance or appends a link such as
   `fleet tasks note --project <project> <slug> --section spec "Task plan: docs/TASK-PLAN-<slug>.md"`.
8. If a task plan doc save/render fails, the task stays unpromoted and no
   worker is dispatched for it.

## Task Split

1. Update `skills/coordinator/SKILL.md` so handed-off coordinators know the
   eight-step flow and the narrow plan-doc write exceptions.
2. Update `internal/tui/keys.go` so freshly spawned coordinators receive the
   same PLAN-DOC and TASK-PLAN-DOC gates in their first-turn prompt.
3. Make fresh dashboard-spawned coordinators start in the registered project
   repo path from `meta.json` so relative `docs/` writes land in the target
   checkout.
4. Update operator-facing docs in `docs/COORDINATOR-WORKFLOW.md` and
   `README.md`.
5. Add tests pinning the skill text, spawn prompt markers, and project cwd
   resolution.
6. Pin that TASK-PLAN-DOC is linked into worker-visible task text before
   promotion.
7. Add this design doc and render its HTML companion.

## Test Plan

- `python3 -m pytest skills/coordinator/tests/test_skill_md.py -q`
- `go test ./internal/tui -run CoordSpawnPrompt`
- `go build ./...`
- `python3 -m pytest skills/ -q`

## Assumptions

- A coordinator writes the approved plan doc itself; no planner worker is
  needed just to persist the design.
- The docs folder is an approved write target for design and task plan docs
  only.
- `fleet project add` metadata is the authoritative repo path for a fresh
  coord. Same-project worker records may point at worktrees, so they are only a
  legacy fallback when `meta.json` is absent.
- Rendering HTML is best-effort when the project has a renderer, but a render
  failure blocks task splitting or task promotion until fixed or explicitly
  waived by the operator.
- Existing installed coordinator skill copies should be refreshed after the
  source change lands.
