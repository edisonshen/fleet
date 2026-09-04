---
name: fleet-guard
description: Watches the host Claude Code agent's context window. Writes health JSON to ~/.fleet/agents/<id>.json on every turn, triggers structured handoffs at 40% (Yellow) and 50% (Red) thresholds, delivers operator messages from ~/.fleet/inbox/<id>.md, and enqueues handoff requests at ~/.fleet/queue/ for the fleet binary to drain. Agent-side half of Fleet (producer); the fleet TUI / `fleet drain` is the consumer.
---

# fleet-guard

Agent-side half of Fleet. The `fleet` binary watches what this skill writes; together they form the parallel-agent control plane.

Spike `docs/SPIKE-context-pct.md` closed PASS on 2026-04-28 (full spec). This skill ships with the auto-handoff trigger as designed.

## Hook bindings

Registration lives in `~/.claude/settings.json` and is written by `fleet init`. The skill is a passive consumer of three Claude Code hooks:

| Hook | When | Skill action |
|------|------|--------------|
| `Stop` | After every assistant turn | Update health JSON (sets `needs_input=true`), deliver inbox, evaluate handoff thresholds |
| `PreCompact` | Just before context compaction | Emergency handoff: write doc + queue immediately, no MILESTONE wait |
| `SessionStart` | Session begins (resume or fresh) | Deliver any pending operator inbox message |
| `UserPromptSubmit` | Operator submits a prompt to the agent | Clear `needs_input` (agent transitions waiting → working) |
| `PreToolUse` | Before every tool call | Coordinator delegation guard (`coordguard.py`): in a `FLEET_ROLE=coord` session, deny `Edit`/`Write`/`MultiEdit`/`NotebookEdit` outside `docs/`, and `Bash` that runs test suites, mutates git, or writes files. Agent-tool subagents (payload carries `agent_id`) are exempt. No-op for workers. |

The original DESIGN.md referenced a `PostResponse` hook; that hook does not exist in Claude Code. `Stop` is the real binding and is what the spike validated (67 fires, 100% transcript availability, p95 18ms).

## Required environment

Set by `fleet dispatch` and inherited via `tmux new-session -e`:

- `FLEET_AGENT_ID` — the 8-hex-char ID. Without it, the skill exits silently (the agent is not under Fleet supervision).
- `FLEET_HOME` — defaults to `~/.fleet/`. Override for sandboxed tests and CI.
- `FLEET_ROLE` — `coord` or `worker`, stamped by `fleet dispatch`. Enables the PreToolUse delegation guard and the coord role reminder on `SessionStart`. Falls back to the agent record's `is_coord` when unset.
- `FLEET_COORD_GUARD=off` — escape hatch: log coord violations to `~/.fleet/coord-violations/<id>.jsonl` but do not deny.

## Required tools

- `python3` ≥ 3.9 (stdlib only — no pip dependencies for the runtime path; pytest is dev-only). Annotations use `from __future__ import annotations` so PEP 604 (`X | None`) and PEP 585 (`dict[str, int]`) syntax stays string-form for parser compat.
- `tmux` — `capture-pane -t <session> -p` is how the skill grabs recent agent output for the handoff doc body and detects `MILESTONE` markers in Yellow path.

## Hook payload contract

### `Stop`

Stdin is a JSON payload containing at least `transcript_path`, `session_id`, and `hook_event_name`. The skill walks the transcript JSONL to find the most-recent `message.usage` and `message.model`, then computes `context_pct = (input + cache_read + cache_creation) / CONTEXT_LIMITS[model] * 100` (`output_tokens` excluded — they don't carry into the next turn's context). The model lookup table mirrors `spike/stop-hook.py:CONTEXT_LIMITS`; an unknown model id defaults to a 1,000,000-token window with a loud stderr flag naming the unresolved id (operator directive 2026-06-10: "if you dont know you should flag it, and set default value to 1 million, dont let it silo" — a silently-null `context_pct` disables the 40/50 auto-handoff with no warning). A transcript with no `message.usage` blocks also flags loudly (transcript path + "no usage found") instead of freezing silently; `main.py` additionally emits a `context_pct unresolved` stderr diagnostic on any Stop fire that would write a null.

The `payload.model` field is unreliable on `Stop` (often empty). Authoritative model name comes from the transcript walk (`message.model` on the most-recent `assistant` line), matching `spike/stop-hook.py:118-141`.

When the Stop hook needs to inject text into the agent's next turn (operator inbox or `HANDOFF REQUESTED`), the skill emits a JSON response: `{"decision": "block", "reason": <concatenated injections>}`. Claude Code refuses to stop and treats `reason` as the next assistant-side input.

Plain stdout from a Stop hook is NOT auto-injected into the conversation — verified empirically across all transcripts on disk. The original v0.1 contract assumed otherwise and shipped broken: auto-yellow agents stayed stuck because the agent never saw the injection. The JSON form is the documented mechanism.

The skill emits at most two concatenated injections per fire, joined by a blank line, in this order:

1. Operator inbox message — `[OPERATOR] <body>` (if `~/.fleet/inbox/<id>.md` is present).
2. `HANDOFF REQUESTED` — Yellow-threshold prompt directing the agent to wrap with `MILESTONE` on its own line so the next turn can write a clean handoff doc.

### `PreCompact`

Stdin is the same JSON shape minus token deltas. Stdout is ignored (the compaction is already in motion). The skill writes a handoff doc + queue file unconditionally — better an emergency handoff than a lossy compaction.

### `SessionStart`

Stdin contains `session_id` and resumption metadata. The skill checks for a pending inbox message and emits it via stdout (same `[OPERATOR] <body>` shape). No threshold evaluation on this hook — context is fresh.

### `UserPromptSubmit`

Stdin contains the operator-submitted prompt and metadata. Stdout is ignored (the prompt is already being processed). The skill clears `needs_input=false` on the agent record — the agent has transitioned from waiting → working. Pairs with `Stop`, which sets `needs_input=true` after every assistant turn.

## Handoff thresholds

| context_pct | State | Action |
|-------------|-------|--------|
| < 40% | Green | Update health JSON only. |
| ≥ 40% | Yellow | Inject `HANDOFF REQUESTED` once. Mark `handoff_type: "auto-yellow"` and `handoff_type_at: <RFC3339>` in agent record. On subsequent fires, grep tmux pane for `MILESTONE` on its own line; when found, write doc + queue with `handoff_type: "auto-yellow"`. If MILESTONE never lands and `handoff_type_at` is older than 30 min (or missing — legacy migration), re-inject `HANDOFF REQUESTED` and bump the timestamp. |
| ≥ 50% | Red | Hard force: write doc + queue immediately with `handoff_type: "auto-red"`. No MILESTONE wait. |

`PreCompact` writes its handoff doc with `handoff_type: "precompact"`.

Legal `handoff_type` values are defined in `internal/handoff/handoff.go:30-36`: `"manual"` (Week 4a operator path — skill never writes this), `"auto-yellow"`, `"auto-red"`, `"precompact"`. The skill MUST write one of the three auto values; any other string will fail downstream readers.

Thinking modes (`plan`, `review`, `fix`) emit a banner reminder via inbox but never auto-trigger — those modes are the operator's deliberate space and a forced handoff would corrupt the work. (`agent.Record.Mode` enum: `"execute" | "plan" | "fix" | "review"`.)

## Files written

### `~/.fleet/agents/<id>.json` — health record

Schema matches `internal/agent.Record` (SchemaVersion=1). Atomic write via `.tmp` + rename.

The skill ONLY owns these fields and may overwrite them on every fire:

- `context_pct`, `context_source`
- `last_activity_ts`
- `blocked`, `blocked_reason`, `blocked_since`
- `needs_input`
- `inbox_pending`
- `handoff_type` (only when transitioning Green → Yellow / Red / PreCompact; never to clear)

EVERY OTHER field (`schema_version`, `id`, `pid`, `tmux_session`, `engine`, `role`, `mode`, `task_id`, `project`, `review_round`, `last_handoff_path`, `handoff_number`, `cwd`, `command`, `spawned_at`) is owned by `fleet dispatch` / `fleet handoff` and MUST be preserved by reading the existing record first and merging. In particular, `mode` is the field this same manifest branches on (Green/Yellow/Red logic skips for `plan`/`review`/`fix`) — overwriting it would corrupt the very state we're reading. Implementation note for step 2 (`health.py`): merge by field name, not by replacing the whole struct.

### `~/.fleet/handoffs/<id>-<utc-iso>-<short-uuid>.md` — handoff doc

Frontmatter shape MUST match `internal/handoff.Render` byte-for-byte. Order and quoting matter — the chain reader and the resume probe parse this with simple line-prefix matching, not a full YAML parser:

```
---
agent_id: "<8-hex>"
task_id: "<slug>"
project: "<name>"
context_pct_at_handoff: <float> | null
previous_handoff: "<path>" | null
handoff_number: <int>
timestamp: "<RFC3339 UTC>"
handoff_type: "auto-yellow" | "auto-red" | "precompact"
---

## Completed
<body>

## Key Decisions
<body>

## Docs (this session)
<body>

## Open Questions
<body>

## Next Steps (prioritized)
<body>
```

All string values are double-quoted (Go `%q` form) so YAML metacharacters in operator-supplied values (colons, newlines) cannot inject. The skill's Python writer reproduces this exactly.

Body sections that the skill cannot populate use the canonical placeholder string from `internal/handoff.go:Placeholder`: `_(operator-triggered handoff — fill in before resuming)_`. Do NOT invent alternate sentinels — 4a's chain reader and any future loader recognize only this exact string. Per plan D3, the tmux-pane capture from `capture-pane -t <session>` is dumped into one of the existing sections (e.g., "Completed") rather than added as a new section or marked with a custom placeholder.

### `~/.fleet/queue/spawn-fresh-<old_id>.json` — drain trigger

Schema is `queue.SpawnFresh` in `internal/queue/queue.go:49-62`:

```json
{
  "schema_version": 1,
  "old_agent_id": "<8-hex>",
  "handoff_doc": "<absolute path to the doc above>",
  "project": "<name>",
  "task_id": "<slug>",
  "new_agent_id": "<8-hex pre-allocated>",
  "new_session": "fleet-<new_agent_id>",
  "enqueued_at": "<RFC3339 UTC>"
}
```

Both `new_agent_id` AND `new_session` MUST be pre-allocated by the skill before queueing. The crash-recovery probe in `fleet handoff` (and the soon-to-arrive `fleet drain`) reads both: `new_agent_id` identifies the successor record on retry; `new_session` is what the probe checks against `tmux has-session` to decide whether the previous spawn actually completed before the crash. Omitting `new_session` makes the recovery probe silently no-op and produces orphaned auto-handoffs.

### `~/.fleet/inbox/archive/<id>-<ts>.md` — archived operator messages

## Files read

- `~/.fleet/agents/<id>.json` — to preserve fields the skill doesn't own (e.g., spawn metadata).
- `~/.fleet/inbox/<id>.md` — operator messages (one-shot; archived after delivery).
- Transcript JSONL at `payload.transcript_path` — token usage source.
- Tmux pane via `tmux capture-pane -t <session> -p` — recent activity + MILESTONE detection.

## Failure mode

The skill MUST NEVER block the agent's turn. All exceptions are logged to stderr and swallowed; the hook returns 0 unconditionally. A skill crash leaves the agent running with stale health data — the TUI's polling fallback (1s) is the safety net.

## Install

`fleet init` (added in Week 4b+4c) writes the skill files to `~/.claude/skills/fleet-guard/` and merges hook registrations (`Stop`, `PreCompact`, `SessionStart`) into `~/.claude/settings.json`.

For development:

```sh
ln -s "$(pwd)/skills/fleet-guard" ~/.claude/skills/fleet-guard
# then manually add Stop/PreCompact/SessionStart entries to ~/.claude/settings.json
```
