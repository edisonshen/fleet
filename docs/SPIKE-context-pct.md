# Week 0 Feasibility Spike: Reading Claude Code Context %

**Status:** OPEN
**Owner:** edisonshen
**Started:** 2026-04-16
**Decision deadline:** Before Week 1 begins

The entire Fleet thesis depends on whether a Claude Code skill can read the host agent's `context_pct` reliably enough to drive Yellow (50%) and Red (75%) handoff thresholds. This spike answers that question before any `fleet` binary code is written.

> **Heads-up from design review:** the design referenced a `PostResponse` hook. Claude Code's actual hook surface is `Stop`, `PostToolUse`, `PreCompact`, `UserPromptSubmit`, `SessionStart`, `SessionEnd`, `Notification`. The spike's first job is to map the design's intent onto the real hook names — most likely `Stop` (fires when Claude finishes its response).

## Three Questions

### 1. Availability

Does any Claude Code hook payload expose current token usage or context-window %?

**Method:**
- Read Claude Code hook reference docs end-to-end.
- Write a one-line `Stop` hook that dumps the full payload to a file.
- Run a real session with tool use, file reads, and a long task. Inspect the dumped payloads for token counts.
- If the payload does not include tokens, check whether `transcript_path` lets us reconstruct token usage by parsing the transcript JSONL.

**Pass:** Token data is exposed (in payload or via transcript) and is current within one turn.
**Fail:** No token signal available.

**Result:** _TBD_

### 2. Latency

If exposed, is `context_pct` available within 500ms of the hook firing? The TUI polls at 1s; anything slower than 500ms means stale reads.

**Method:** Time the hook handler from invocation to JSON write across 20 turns.

**Pass:** ≤500ms p95.
**Fail:** >500ms or unreliable.

**Result:** _TBD_

### 3. Proxy accuracy (only if 1 or 2 fails)

Measure proxy accuracy over 5 real Claude Code sessions including tool use, file reads, and long-context tasks.

**Proxy formula:**
```
tokens_estimate = system_prompt_tokens
                + sum(message_tokens)
                + sum(tool_result_tokens)
                + sum(file_read_tokens)
```
Token counts via `tiktoken` at turn boundaries, scraped from the transcript JSONL.

**Ground truth:** `/context` output at the same turn boundary.

**Pass:** Proxy stays within ±20% of ground truth across all 5 sessions.
**Fail:** Worse than ±20%.

**Result:** _TBD_

## Decision Matrix

| (1) Availability | (2) Latency | (3) Proxy | Path |
|------------------|-------------|-----------|------|
| PASS | PASS | n/a | **Full spec.** Use hook data directly. |
| PASS | FAIL | PASS | **Proxy mode.** Mark `context_source: "proxy"` in health JSON. README notes health is estimated. |
| FAIL | n/a | PASS | **Proxy mode.** Same as above. |
| FAIL | n/a | FAIL | **Pivot.** See narrowed scope below. |

## Pivot path (narrowed v1 if all three fail)

If hook-based context measurement is impossible AND proxy is too noisy:

- Drop automatic Yellow/Red triggers. The handoff system becomes entirely operator-triggered (TUI `h` key or `fleet handoff <id>`).
- Reframe the thesis from "self-healing fleet" to "parallelism dashboard with one-key handoff."
- Keep: TUI, deploy, attach, peek, message, broadcast, manual handoff, structured handoff doc format, lifecycle probe.
- Drop: 50%/75% thresholds, context-pct column in the TUI (or show "?"), `fleet-guard`'s automatic handoff trigger (skill becomes optional / advisory).
- Re-record demo gif emphasizing parallelism + clean handoffs rather than self-healing.

## Week 1 implementation pre-decisions (from eng review)

These are not part of the spike itself but are pinned here so they're not lost:

- **Env injection:** `fleet deploy` must use `tmux new-session -e FLEET_AGENT_ID=... -e FLEET_EXTRA_CLAUDE_MD=...` explicitly. Do not rely on shell env inheritance — tmux has its own per-session env, and user `.zshrc` can override.
- **Handoff filename collision:** use `handoffs/<id>-<utc-iso>-<short-uuid>.md`. UTC + UUID suffix prevents same-second collision and is portable across machines.
- **fsnotify on macOS:** measure rename-event reliability on darwin during the spike. If unreliable on macOS for `~/.fleet/queue/`, default to always-poll on darwin and use fsnotify on linux only.

## Final Decision

**TBD** — fill this in once questions 1-3 are answered.

Commit this doc with the answer before opening any Week 1 PR.
