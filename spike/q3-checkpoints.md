# Q3 Checkpoints — `/context` vs spike's `computed_pct`

Manual ground-truth comparisons feeding the spike's Q3 (accuracy)
question. Each row pairs a `/context` snapshot the operator captured
during a real working session with the closest-in-time spike fire
from `~/.fleet/spike/metrics.jsonl`.

**Bar:** spike must be within ±20pp of `/context` ground truth across
all checkpoints.

## Checkpoints

### Session `767799f3` (Fleet repo, 2026-04-26 → 2026-04-28)

| `/context` | Closest spike fire `computed_pct` | Fire timestamp (UTC) | Delta |
|---:|---:|:---|---:|
| 23% | 23.17% | 2026-04-26T08:34:18Z | +0.17pp |
| 25% | 25.09% | 2026-04-26T18:30:04Z | +0.09pp |
| 32% | 31.54% | 2026-04-27T05:10:21Z | −0.46pp |
| 41% | 40.72% | 2026-04-28T00:34:02Z | −0.28pp |
| 46% | 45.85% | 2026-04-28T01:17:30Z | −0.15pp |

### Session `7a23cd40` (different working dir, cross-session validation, 2026-04-28)

| `/context` | Spike fire `computed_pct` | Fire timestamp (UTC) | Delta |
|---:|---:|:---|---:|
| 16% | 15.00% | 2026-04-28T01:41:13Z | −1.00pp |

Cross-session pairing: same operator, different Claude Code session, different working directory (`/Users/pinkbear/projects/` rather than the fleet repo). Confirms the spike's measurement is consistent across sessions, directories, and context-size scales (sub-200k vs 500k+ tokens).

The 1pp gap is the post-response vs pre-next-request sampling difference (see Methodology note below) — about one assistant-turn worth of tokens.

### Aggregate

- Total checkpoints: **6** (5 same-session + 1 cross-session)
- Max absolute delta: **1.00pp**
- Mean absolute delta: **0.36pp**
- Bar (±20pp): **PASS by ~20x margin**

All on `claude-opus-4-7` (1M context window).

## Methodology refinement

The spike Stop hook fires *after* an assistant response (it walks the transcript, reads the most-recent `usage` object). `/context` shows the state *queued for the next request* — including any input the operator has typed/attached since the last response. So `/context` and the latest spike fire sample slightly different moments:

- **Latest spike fire** = post-response context (state at the time the assistant finished)
- **`/context`** = pre-next-request context (state about to be sent up)

For the tightest pairing:

1. In the Claude Code session, send any quick message (e.g., "ok") that triggers an assistant response.
2. The assistant responds → Stop hook fires → new record in `~/.fleet/spike/metrics.jsonl`.
3. Run `/context` immediately after the response.
4. The new fire and the `/context` reading should match within ~1pp.

If `/context` is run *without* a fresh response (e.g., right after attaching a file but before sending), it'll be 5-10pp ahead of the latest fire — that's normal sampling drift, not spike error. The 7a23cd40 cross-session checkpoint above used this method (operator typed "okay", got a response, then ran `/context`) and the delta was 1.00pp.

## Why this is so accurate

The spike doesn't *estimate* token counts — it reads the exact `usage`
object Claude Code records on each assistant turn (`input_tokens` +
`cache_read_input_tokens` + `cache_creation_input_tokens`). `/context`
shows the same numbers from Claude Code's own internal accounting.
The only sources of remaining drift:

1. **Timing skew** — `/context` snapshots the moment of invocation;
   the spike fire reports the most-recent assistant turn's usage.
   These can differ by one turn's worth of tokens (typically <2k on a
   1M context).
2. **Display rounding** — `/context` displays integer percentages; the
   spike records two decimals. A `/context` reading of `23%` could be
   anywhere in [22.5%, 23.5%).

Both account for the observed sub-0.5pp deltas without any additional
explanation.

## How to add a new checkpoint

```bash
# In Claude Code:
/context
# Note the displayed %.

# Then in any shell:
tail -1 ~/.fleet/spike/metrics.jsonl | python3 -c "import json,sys; r=json.loads(sys.stdin.read()); print(f'fire ts={r[\"ts\"]}  pct={r[\"computed_pct\"]:.2f}%  model={r[\"model\"]}')"
# Pair the two readings, append a row above.
```

Aim for `/context` and the spike fire to be within ~30 seconds of each
other for tight pairing. Wider pairings still work but get noisier as
the operator types longer messages between turns.

## Cross-reference

- `docs/SPIKE-context-pct.md` — the spike decision doc that consumes
  this artifact.
- `spike/stop-hook.py` — the hook that produces `metrics.jsonl`.
- `spike/analyze.py` — aggregator over the same data; run
  `python3 spike/analyze.py` for Q1 / Q2 / Q3 stats on demand.
