# Skill Feedback Loop

How Fleet's skills (`fleet-guard`, `fleet-planner`) observe their own
performance and propose improvements. Closed loop: observe → measure →
suggest. The "auto-update" piece never auto-edits SKILL.md — it opens a
PR a human reviews.

---

## Problem

Fleet ships two skills as load-bearing components:

1. **`fleet-guard`** — fires handoffs at 50%/70% context thresholds,
   relays operator messages, writes agent health JSON.
2. **`fleet-planner`** — runs a planner session against a project,
   produces tasks the operator dispatches. (Phase 2 of `docs/FLOW.md`.)

Both rely on prompt design choices that are guesses today. fleet-guard's
50%/70% thresholds come from Hermes/OpenClaw cross-reference (see
`docs/DECISIONS.md` 2026-04-21). The handoff doc template borrows from
prior art. We don't know if these numbers and templates are right for
real operator workloads until we observe outcomes.

Today's failure mode: a tuned-wrong skill ships and degrades silently.
The next agent re-asks a question the previous one already answered. The
planner proposes 8 tasks, only 2 actually get dispatched. Without
observation, we can't see this happening — let alone fix it.

The closed loop turns the skill from fire-and-forget into something
that learns from its own track record.

---

## Design philosophy

Three principles, in order of importance:

1. **Observe before suggesting.** Skills write per-execution events to
   an append-only log. The read-only data flow is enough to be useful
   even if we never build the suggesting half. Tier 1 alone earns its
   keep.

2. **Suggest, never auto-edit.** SKILL.md is the source of truth for
   skill behavior. Automated edits are off the table. The loop proposes
   via PR; the operator decides. Same posture as gstack's review-log
   pattern. Self-modifying prompts are a known footgun — you can degrade
   a working skill faster than you improve a broken one.

3. **Local first.** Skill events live in `~/.fleet/skill-events/`, same
   posture as `~/.fleet/agents/`. No telemetry leaves the operator's
   machine. Aggregation runs locally on demand
   (`fleet skills feedback`).

---

## Architecture

### Three-tier signal flow

```
  Skill execution         Event log               Outcome backfill        Aggregator
  ───────────────         ─────────               ────────────────        ──────────
  (fleet-guard fires) ──► writes event JSONL ──► fleet binary writes ──► reads event +
                          (~/.fleet/                outcome JSONL line     outcome lines,
                           skill-events/            back-keyed by          groups by
                           <skill>-                 event_id when          skill+reason,
                           <yyyy-mm-dd>.jsonl)      downstream signal      computes stats,
                                                    arrives                surfaces patterns
```

### Why a new file type, not an extension to `progress/<task-id>.jsonl`

`progress/<task-id>.jsonl` is keyed by *task*. A skill execution is
keyed by *skill + agent*. Mixing them would force every reader to
filter every record. Separate aggregates, separate files — same posture
as `agents/<id>.json` vs `progress/<task-id>.jsonl`.

A skill event also outlives the task it fired against (handoff docs are
30d, progress logs are 30d, but skill-events are kept long enough to
inform aggregation across many tasks — see retention below). Lifetimes
differ; storage differs.

### Event schema

```json
{
  "schema_version": 1,
  "event_id": "evt-7f3a2b1c",
  "ts": "2026-04-26T14:32:14Z",
  "skill": "fleet-guard",
  "skill_version": "0.0.1",
  "fire_reason": "context_50pct",
  "agent_id": "a4",
  "task_id": "auth-token-refresh",
  "project": "rainier",
  "model": "claude-opus-4-7",
  "inputs": {
    "context_pct": 51.2,
    "mode": "execute",
    "review_round": null
  },
  "outputs": {
    "action": "handoff_queued",
    "handoff_doc_path": "~/.fleet/handoffs/a4-2026-04-26T143214Z-7f3a.md",
    "handoff_doc_size_bytes": 2842
  }
}
```

`event_id` is the join key for outcome backfill — see next section.

`fire_reason` is the primary aggregation axis. Per skill, the controlled
vocabulary is:

- **fleet-guard:** `context_50pct`, `context_70pct`, `inbox_message`,
  `operator_handoff` (`[h]` keypress), `mode_change`
- **fleet-planner:** `task_planned`, `task_split`, `task_dropped`,
  `plan_committed`

Adding new reasons is a schema_version bump (additive — old aggregator
ignores unknown reasons, doesn't crash).

### Outcome attribution

The event is written when the skill fires. The outcome is written
*later* by the Fleet binary, when the downstream signal arrives. Two
separate writers, joined by `event_id`.

This is critical because outcome attribution often spans agents — the
fleet-guard event fires in agent `a4`, but the outcome ("did the next
agent succeed?") is determined by what agent `a5` does over the next
few turns. `a4` doesn't have visibility into `a5`. The Fleet binary
does.

**Outcome JSONL schema** (sibling file, same dir):

```json
{
  "schema_version": 1,
  "event_id": "evt-7f3a2b1c",
  "ts": "2026-04-26T14:48:02Z",
  "outcome": {
    "next_agent_completed": true,
    "next_agent_id": "a5",
    "next_agent_handoff_count": 0,
    "task_terminal_state": "done",
    "elapsed_s": 942
  }
}
```

For fleet-planner outcomes:

```json
{
  "event_id": "evt-...",
  "outcome": {
    "tasks_committed": 8,
    "tasks_dispatched_within_24h": 5,
    "tasks_completed": 3,
    "tasks_dropped": 2,
    "tasks_pending": 3
  }
}
```

**When does the Fleet binary write outcomes?** On every TUI tick (1s
during interactive sessions, hourly otherwise via `fleet maintenance`),
the binary scans pending events whose downstream signal can now be
resolved:

- For fleet-guard `context_*pct` events: when the replacement agent
  either reaches `done` (positive outcome) or itself fires another
  handoff within N=2 turns (churn outcome) or task hits `unhealthy`
  status (negative outcome).
- For fleet-planner events: when 24h have elapsed since plan commit
  (snapshot of dispatch + completion state) AND when each planned task
  reaches a terminal state (`done` / `dropped`).

Outcomes can be *partial* (some tasks done, some pending) — write
what's known, leave the rest null.

**Idempotency:** outcome writer reads the existing outcome line for an
event_id (if present) before writing. Updates use `.tmp` + rename on a
sidecar JSONL, then atomic merge. Aggregator joins on event_id, last
outcome wins.

### Aggregator: `fleet skills feedback`

Manual command. Reads the last N days of events + outcomes for a given
skill. Default window 14 days.

```
$ fleet skills feedback fleet-guard --days 14

fleet-guard — last 14 days
─────────────────────────────────────────────────────────
Total fires:          47

By fire_reason:
  context_50pct       28   (60%)
  context_70pct        4   ( 9%)
  inbox_message       12   (26%)
  operator_handoff     3   ( 6%)

Outcomes for context_50pct (28 fires, 26 resolved):
  next agent completed task        18 (69%)
  next agent churned within 2 turns 5 (19%)
  next agent reached unhealthy      2 ( 8%)
  pending                           3 (—)

  ⚠  19% churn rate is above the 10% target. Consider:
     - handoff template may be losing context
     - 50% threshold may be firing too early on long sessions
     - manual review: ./fleet-guard --inspect-events fleet-guard --reason context_50pct --outcome churned

Median handoff doc size:  2.4 KB
P95 handoff doc size:     4.8 KB
```

Output is human-readable plus `--json` for machine consumers (scripts,
the suggest-edit loop in Tier 4).

### Suggest-edit loop (Tier 4, deferred to v1.2)

Optional. Aggregator findings can be fed into a one-shot LLM call:

> "Here are 28 fleet-guard `context_50pct` fires. 5 next-agents churned
> within 2 turns asking the same questions the previous agent already
> answered. Here is the current handoff doc template. Propose a
> one-paragraph addition to the 'Open Questions' section that would
> reduce churn. Output: a unified diff against `skills/fleet-guard/SKILL.md`
> plus a 3-line summary of the evidence."

Result lands as an auto-opened GitHub PR against
`skills/fleet-guard/SKILL.md`. PR body includes:

- The candidate diff
- Aggregator output that triggered it (raw counts, sample
  churned-handoff event_ids with paths to handoff doc files)
- The LLM call's prompt + response (for audit trail)
- Suggested test plan

Operator reviews. Merges, edits, or closes. **Never auto-merges.**
**Never bypasses the PR step.**

Cost ceiling: aggregator calls Tier 4 at most once per week per skill,
gated by the operator running `fleet skills feedback --propose`. ~$1-2
per cycle on Opus.

---

## File layout

```
~/.fleet/
└── skill-events/
    ├── fleet-guard-2026-04-26.jsonl      # event records (append-only)
    ├── fleet-guard-2026-04-26.outcomes.jsonl  # outcome backfill (append-only)
    ├── fleet-planner-2026-04-26.jsonl
    └── fleet-planner-2026-04-26.outcomes.jsonl
```

One file per skill per UTC date. New files open at midnight UTC by the
hourly maintenance tick. Files older than the retention window are
deleted by the same prune pass that handles `progress/*.jsonl` — no
archive tier (matches Fleet's existing retention pattern: keep what's
useful, drop the rest).

---

## STATE.md writer-table extension

Add to the writer table in `docs/STATE.md`:

| Aggregate | Single writer | Readers | Atomic pattern |
|-----------|---------------|---------|----------------|
| `skill-events/<skill>-<date>.jsonl` | the skill itself (fleet-guard / fleet-planner) | fleet binary (aggregator, outcome writer), `fleet skills feedback` | `O_APPEND` with line < PIPE_BUF |
| `skill-events/<skill>-<date>.outcomes.jsonl` | fleet binary (outcome backfill) | aggregator | `O_APPEND` with line < PIPE_BUF, last-write-wins per event_id |

Both use `O_APPEND`-with-bounded-line-size — same pattern as
`progress/<task-id>.jsonl`. Each line < `PIPE_BUF` (4KB on macOS, 4KB on
Linux) so concurrent appends are atomic per POSIX.

---

## Retention

Add to the retention table in `docs/STATE.md` A4b:

| Path | Retention | Reason |
|------|-----------|--------|
| `skill-events/<skill>-<date>.jsonl` | 30 days from last write | matches `progress/*.jsonl`; one writer per file, simple uniform window |
| `skill-events/<skill>-<date>.outcomes.jsonl` | 30 days from last write | paired with events |

Same 30-day window as `progress/*.jsonl`. One uniform retention rule
across all per-task / per-skill audit trails is cheaper to reason about
than a tiered scheme. If aggregation later proves it needs longer
windows for trend detection, raise the number — but don't pre-optimize.

---

## Phasing

| Tier | Scope | Lands in |
|---|---|---|
| 1 | Event writer in fleet-guard + fleet-planner. Schema_version 1. | v1 (alongside skill landing — Week 4 / Phase 2) |
| 2 | Outcome writer in fleet binary. Backfill on TUI tick + maintenance. | v1 |
| 3 | `fleet skills feedback` aggregator command (text + JSON output) | v1.1 |
| 4 | Suggest-edit LLM loop. Auto-opens PR against SKILL.md. | v1.2 |

**Tiers 1 + 2 are cheap to add while the skills are being built.**
Adding them later requires touching skill code that's already shipped.
Tiers 3 + 4 can ride later releases — instrumentation alone is useful
even without aggregation tooling. Operators can grep the JSONL by hand.

**Why instrument from day one rather than retrofit:** the early-data
window is when calibration matters most. Operators dogfooding v1
(Week 6) generate the most informative usage data — fresh users
exercising the system in unscripted ways. Missing that window means
launching v0.1 without the data we need to tune v0.2.

---

## What this is NOT

- **Not a remote telemetry system.** Events stay on the operator's
  machine. No "phone home." No analytics endpoint. Aggregation is
  manual and local.
- **Not a self-modifying skill.** Edits to SKILL.md happen via PR
  reviewed by a human. Always. Tier 4's LLM call proposes — it never
  commits.
- **Not generic skill observability.** Scope is Fleet's own skills
  (fleet-guard, fleet-planner). Generalizing to observe arbitrary
  skills (gstack, MCP tools, custom user skills) is v2+ scope and a
  different project shape.
- **Not a replacement for evals.** `T1` in TODOS.md (eval suite for
  fleet-guard prompt regressions) covers replay-against-fixtures
  testing. Skill feedback covers in-the-wild outcome attribution.
  Different tools, different bugs caught.

---

## Open questions

- [ ] Outcome attribution for fleet-planner: tasks may be dropped for
      reasons unrelated to plan quality (operator scope change, repo
      changed direction). How do we distinguish "bad plan" from
      "operator shifted priorities"? Probably: surface raw stats, let
      the human reading the aggregator output make the call. Don't try
      to auto-label as "good" or "bad."
- [ ] Should outcomes also capture *operator action* signals
      (operator manually canceled the dispatched task vs let it run)?
      That's a richer signal but requires more TUI plumbing
      (`[shift]+[c]` keypress logged as event).
- [ ] Retention 30 days matches `progress/*.jsonl` for uniformity. If
      v1.1 aggregator runs show trend detection needs more history,
      raise the window per-skill. Don't pre-optimize.
- [ ] Cross-skill correlations (fleet-planner produces tasks that get
      dispatched and then immediately churn through fleet-guard
      handoffs — is the plan or the threshold to blame?). Defer to
      Tier 4+; needs a multi-skill aggregator query language.
- [ ] Per-fire LLM cost for Tier 4 — capping at weekly per-skill
      bounds it, but operators with many active projects could still
      see costs accumulate. Consider a `--dry-run` mode that runs the
      aggregation and shows what would be proposed without making the
      LLM call.

---

## Cross-references

- `docs/DESIGN.md` — overall architecture, three-tier memory, agent
  health primitive.
- `docs/STATE.md` — writer table (extend per above), retention table
  (extend per above), atomic write conventions.
- `docs/FLOW.md` Phase 2 — fleet-planner introduction.
- `docs/FLOW.md` Phase 5 — handoff walkthrough (fleet-guard fire
  pathway).
- `docs/DECISIONS.md` 2026-04-21 — 50%/70% threshold cross-reference
  (the choices this loop will validate or refute).
- `TODOS.md` F9 — tracker entry for this work.
- `TODOS.md` T1 — eval suite (complement, not substitute).
