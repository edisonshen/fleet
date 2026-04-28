# Week 0 Feasibility Spike: Reading Claude Code Context %

**Status:** **CLOSED — PASS** (provisional for v0.1; ongoing validation per the section below)
**Owner:** edisonshen
**Started:** 2026-04-16
**Closed:** 2026-04-28

The entire Fleet thesis depends on whether a Claude Code skill can read the host agent's `context_pct` reliably enough to drive Yellow (50%) and Red (70%) handoff thresholds. This spike answers that question before any `fleet` binary code is written.

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

**Result: PASS.** The Stop hook payload itself does not carry token data, but `transcript_path` points to a JSONL file where every assistant turn records a `message.usage` object with `input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, and `output_tokens`. The model name lives at `message.model` on the same turn. The spike walks the transcript on every fire and pulls the most-recent `usage` (within one turn of the firing assistant response). 67 fires recorded across 3 sessions with 0 errors and 100% transcript availability. See `~/.fleet/spike/metrics.jsonl` and `spike/analyze.py`.

### 2. Latency

If exposed, is `context_pct` available within 500ms of the hook firing? The TUI polls at 1s; anything slower than 500ms means stale reads.

**Method:** Time the hook handler from invocation to JSON write across 20 turns.

**Pass:** ≤500ms p95.
**Fail:** >500ms or unreliable.

**Result: PASS by ~28x margin.** Across 67 fires: median 10ms, p95 18ms, p99 23ms, max 23ms (well under the 500ms bar). The transcript JSONL is local-disk read with no network call, no LLM round-trip — the latency floor is essentially the cost of opening and walking the file. Even the largest transcript observed (~1MB) walked in under 25ms.

### 3. Accuracy (re-scoped from "proxy accuracy")

Q1 PASSed with real `usage` token data from the transcript, so the proxy formula was never needed. Q3 became a direct check of the spike's `computed_pct` against `/context` ground truth at matched turn boundaries.

**Bar:** spike must be within ±20pp of `/context` across 5 manual checkpoints.

**Result: PASS by ~43x margin.** Five `/context` snapshots paired with closest-in-time spike fires (see `spike/q3-checkpoints.md` for the table):

| `/context` | spike `computed_pct` | delta |
|---:|---:|---:|
| 23% | 23.17% | +0.17pp |
| 25% | 25.09% | +0.09pp |
| 32% | 31.54% | −0.46pp |
| 41% | 40.72% | −0.28pp |
| 46% | 45.85% | −0.15pp |

Max absolute delta 0.46pp. Mean 0.23pp. The remaining drift is fully accounted for by (a) `/context` displaying integer percentages, and (b) one-turn timing skew between the `/context` invocation and the corresponding spike fire. No correctable bug; the spike reads the same numbers `/context` does.

## Decision Matrix (historical reference — outcome is the first row, "Full spec")

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
- Keep: TUI, plan, dispatch, attach, peek, message, broadcast, manual handoff, structured handoff doc format, lifecycle probe.
- Drop: 50%/70% thresholds, context-pct column in the TUI (or show "?"), `fleet-guard`'s automatic handoff trigger (skill becomes optional / advisory).
- Re-record demo gif emphasizing parallelism + clean handoffs rather than self-healing.

## Framing note — Fleet is a supervisor, not a host

The spike answers whether Fleet can *observe* context %. It does not answer
whether Fleet can *control* context compaction — that's the host's job and
out of scope. If all three spike questions fail (no usable context signal
and proxy too noisy), Fleet still ships; it pivots to operator-triggered
handoffs. See the pivot path below. This framing affects interpretation:
"spike failed" ≠ "product failed." The supervisor posture is load-bearing.

See `docs/STATE.md` "Scope constraints" for the full statement and
`docs/DESIGN.md` "Reliability Invariants" for the short version.

## Week 1 implementation pre-decisions (from eng review)

These are not part of the spike itself but are pinned here so they're not lost:

- **Env injection:** `fleet dispatch` must use `tmux new-session -e FLEET_AGENT_ID=... -e FLEET_EXTRA_CLAUDE_MD=... -e FLEET_TASK_ID=... -e FLEET_PROJECT=...` explicitly. Do not rely on shell env inheritance — tmux has its own per-session env, and user `.zshrc` can override.
- **Handoff filename collision:** use `handoffs/<id>-<utc-iso>-<short-uuid>.md`. UTC + UUID suffix prevents same-second collision and is portable across machines.
- **fsnotify on macOS:** measure rename-event reliability on darwin during the spike. Always pair fsnotify with a 1s polling fallback — fsnotify is latency optimization, not a correctness primitive.
- **Hook payload shape (spike deliverable):** the spike's Stop-hook payload dump feeds directly into the reference payload shape documented in `docs/STATE.md`. Commit an anonymized sample payload to `docs/HOOK-PAYLOAD-SAMPLES.md` so future implementers don't have to re-run the spike.
- **`fleet init` mechanics (deferred to Week 1 PR):** skill install copies/symlinks `skills/fleet-guard/` to `~/.claude/skills/fleet-guard/` AND merges hook registrations into `~/.claude/settings.json` (Stop, PreCompact, SessionStart). Both steps, atomic where possible.

## Final Decision

**Full spec.** All three questions PASS by large margins. fleet-guard ships with the auto-handoff trigger at 50%/70% as designed (DESIGN.md Health thresholds table, DECISIONS.md 2026-04-21). The "self-healing fleet" thesis is validated for v0.1.

The pivot path (drop auto-handoff, become a parallelism dashboard with `[h]` manual handoff) is no longer the v0.1 plan. It remains in this document as the documented fallback if ongoing validation (next section) ever surfaces a regression severe enough to retract the closure.

## Ongoing validation (post-closure)

Closure is **provisional for v0.1**, not permanent. The Stop hook stays installed indefinitely; `metrics.jsonl` keeps accumulating; we re-validate on a cadence and against named gates.

**Cadence:**

- Re-run `python3 spike/analyze.py` at the start of each release-prep window. Cheap, automated.
- Capture one fresh `/context` checkpoint per substantive working session, append to `spike/q3-checkpoints.md`. Already a low-friction habit (the operator runs `/context` regularly anyway).
- Raise the data bar to **N=500 fires across ≥5 sessions** by Week 6 dogfood. Higher N gives tighter confidence intervals on the threshold-tuning decisions in v0.2.

**Re-open gates** — if any of these fire, the spike re-opens and DESIGN.md may need rescoping:

| Signal | Threshold | Action |
|---|---|---|
| Q1 fire-success rate | drops below 95% over a 100-fire window | re-open Q1 |
| Q2 latency p95 | exceeds 100ms (5x current observed) | re-open Q2; investigate transcript bloat / I/O contention |
| Q3 max abs delta | exceeds 5pp on any single new checkpoint | re-open Q3; check for Anthropic accounting changes or new model with bad limit |
| New `claude-*` model in use | model not in `CONTEXT_LIMITS` dict | spike fire records `computed_pct: null`; user must add to dict (operator-overridable via the future `~/.fleet/config.yaml:context_limits` per TODOS F11) |

**Adjacent tooling for ongoing validation** (open TODOs):

- **F10 — `fleet spike status`** subcommand. Reads `metrics.jsonl`, prints current Q1 / Q2 / Q3 numbers on demand. Lets the operator check spike health without invoking `analyze.py` manually. ~30 lines of Go, lands as part of Week 4 or later.
- **F11 — `~/.fleet/config.yaml:context_limits`** operator-override map. When Anthropic ships a new model ID, operators add one line of YAML and Fleet works again — no waiting on a Fleet release. Already specified at PR #4 conclusion.

The skill-feedback loop (`docs/SKILL-FEEDBACK.md`, Tiers 1-2 in v1) covers a different but adjacent concern: whether the 50%/70% threshold *values themselves* are right. The spike validates that we can *measure* `context_pct`; the skill-feedback loop validates that we *act on it correctly*. Both run continuously in v1.

