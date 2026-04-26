# Fleet Week-0 Spike — Stop-hook probe

Purpose: answer the three questions in `docs/SPIKE-context-pct.md` from
real Claude Code session data, not docs alone.

## What's here

- `stop-hook.py` — runs on every Claude Code Stop event. Reads the
  payload, walks `transcript_path`, computes `context_pct`, and appends
  one record per fire to `~/.fleet/spike/metrics.jsonl`. Saves each
  session's first raw payload to `~/.fleet/spike/payloads/<sid>.json`.
- `analyze.py` — reads `metrics.jsonl` and prints a verdict on Q1
  (coverage), Q2 (latency), and the data needed for Q3 (manual accuracy
  check vs. `/context`).

## Install

The hook is registered in `~/.claude/settings.json`:

```json
"hooks": {
  "Stop": [
    { "hooks": [{ "type": "command", "command": "/usr/bin/python3 /Users/pinkbear/projects/fleet/spike/stop-hook.py" }] }
  ]
}
```

Settings load at Claude Code session start. **Restart your Claude Code
session (or start a new one) for the hook to take effect.**

## Use the spike

1. Restart Claude Code.
2. Use it normally for a few hours — coding, reading files, running
   tools. The longer the session, the more useful the data.
3. Optional: run `/context` a few times during long sessions and write
   down the percentage shown. We'll cross-check it against the hook's
   computed value for Q3 (accuracy).
4. After enough data accumulates, run:

   ```
   python3 spike/analyze.py
   ```

   The output will say whether Q1 and Q2 pass, and dump the most-recent
   computed percentages so you can compare them with whatever you wrote
   down from `/context`.

## What "enough data" means

- **For Q1 (coverage):** ≥ 50 hook fires across 2-3 sessions of mixed
  work. Need enough variety to know if any assistant turn type is
  missing usage data.
- **For Q2 (latency):** ≥ 100 fires, ideally including at least one
  long session with ≥ 1 MB transcript. Latency at small transcript
  size doesn't predict latency at production scale.
- **For Q3 (accuracy):** 5+ manual `/context` snapshots across at
  least 2 sessions, ideally at varying context fill levels (10%,
  40%, 70%, 90%).

A focused afternoon of normal Claude Code use will hit all three.

## Uninstall

Remove the `hooks` block from `~/.claude/settings.json` and restart
Claude Code. The data under `~/.fleet/spike/` is preserved unless you
delete it.

## What gets written, where

```
~/.fleet/spike/
├── metrics.jsonl                         # one record per Stop fire
├── payloads/
│   └── <session_id>.json                 # first raw payload per session
└── transcript-snippets/
    └── <session_id>-<fire_id>.json       # last `usage` object per fire
```

`metrics.jsonl` is append-only and small (~300 bytes per fire). Safe to
let accumulate indefinitely.

## Failure modes the hook handles

- Payload doesn't parse → record `error: payload_parse`, exit 0.
- `transcript_path` missing or unreadable → record
  `error: transcript_path_missing`, exit 0.
- Transcript walk fails → record `error: transcript_walk: <reason>`,
  exit 0.
- No `usage` object found → record `error: no_usage_lines_in_transcript`,
  exit 0.
- Anything else → swallow, never block the host.

The hook is not allowed to take down the host. Worst case, metrics are
missing for that fire and the operator sees no `context_pct` update.
