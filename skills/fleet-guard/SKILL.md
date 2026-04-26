---
name: fleet-guard
description: Watches the host Claude Code agent's context window. Writes health JSON to ~/.fleet/agents/<id>.json, triggers structured handoffs at 50%/70% thresholds, delivers operator messages from ~/.fleet/inbox/<id>.md.
version: 0.0.1
---

# fleet-guard

This skill is the agent-side half of Fleet. The `fleet` binary watches what this skill writes. Together they form the parallel-agent control plane.

> **Status: stub.** Week 0 spike has not been completed. Before this skill does anything load-bearing, the spike at `docs/SPIKE-context-pct.md` must answer whether we can read context_pct from a Claude Code hook payload at all.

## What this skill will do (post-spike)

On every agent turn, after Claude finishes responding:

1. **Read agent identity** from the `FLEET_AGENT_ID` env var. Without it, exit silently — the agent is not under Fleet supervision.
2. **Measure context.** Either read directly from the hook payload (if Week 0 spike Q1+Q2 pass) or estimate via the proxy formula (if only Q3 passes).
3. **Write health JSON** to `~/.fleet/agents/<id>.json`:
   ```json
   {
     "id": "<agent_id>",
     "pid": <claude_pid>,
     "tmux_session": "fleet-<agent_id>",
     "task_id": "<task_slug>",
     "project": "<project_name>",
     "context_pct": <0-100>,
     "context_source": "hook" | "proxy",
     "last_activity_ts": "<ISO 8601>",
     "blocked": <bool>,
     "blocked_reason": "<string or null>",
     "blocked_since": "<ISO 8601 or null>",
     "needs_input": <bool>,
     "inbox_pending": <bool>
   }
   ```
4. **Check inbox** at `~/.fleet/inbox/<id>.md`. If present, inject contents into the next turn's context as "Message from operator: ..." then move file to `~/.fleet/inbox/archive/`.
5. **Trigger handoff at thresholds:**
   - context_pct ≥ 50% (Yellow): append a handoff-recommended note to the task file's Progress section. Agent decides whether to act.
   - context_pct ≥ 75% (Red): write `~/.fleet/queue/handoff-<id>.json` with the handoff doc path. Block the agent from continuing work until the handoff doc is filled.

## Hook bindings (TBD post-spike)

The design referenced `PostResponse` — that hook does not exist. Likely real bindings:

| Design intent | Real Claude Code hook |
|---------------|----------------------|
| After every agent turn | `Stop` |
| Before context compaction | `PreCompact` |
| Session start (deliver pending inbox) | `SessionStart` |

Spike will confirm.

## Open questions

- Does the `Stop` hook payload include token counts? (Spike Q1.)
- Latency: when can the hook write the health JSON before the TUI polls? (Spike Q2.)
- If proxy mode: what's the right system_prompt_tokens baseline? (Spike Q3.)

## Install (manual, dev mode)

```sh
ln -s "$(pwd)/skills/fleet-guard" ~/.claude/skills/fleet-guard
```

In v0.2+, `fleet init` will install this automatically.
