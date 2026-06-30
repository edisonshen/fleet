# TASK PLAN — fleet local debug logs

- **Slug:** `implement-fleet-local-de-eb7e`  ·  **Priority:** P1  ·  **PR-base:** main
- **Parent design:** `docs/DESIGN-fleet-debug-logs.md` (APPROVED 2026-06-27) — read it first; this plan is the execution wrapper.
- **Status:** ready for dual review → promote

## TL;DR

Fleet writes nothing durable today, so debugging a handoff storm / drain leak /
worker death is archaeology. Add **one append lib function** that every Fleet
process calls at each meaningful action to append a JSONL event (agent/LLM
schema) to **its own** file under `~/.fleet/logs/`. A successor agent reads the
raw JSONL with `jq`/`grep`. Files older than 3 days are pruned. No reader
command, no redaction, no cobra-hook magic — the operator's "simple lib function,
event per action, that's it."

```
BEFORE                                   AFTER
------                                   -----
coord/worker/cli ─stderr─▶ tmux (gone)   each proc ─Log()─▶ fleet-DATE-comp-pid-pidstart.jsonl
state.json = current only, no history    JSONL event stream, 3-day retained, jq-readable
```

Dataflow:

```
  fleetlog.Log(comp, evt, lvl, fields…)
     → build envelope {ts,seq,type,lvl,comp,pid,proj,agent,…,caused_by,id,msg,data}
     → one O_APPEND write to THIS process's fleet-<date>-<comp>-<pid>-<pidstart>.jsonl
  read:  jq -c 'select(.slug=="x")' ~/.fleet/logs/*.jsonl
  prune: daily coord-tick call deletes files >3d (no gc dependency)
```

## Goal / success criteria

- A Go `internal/fleetlog` package + a Python `skills/coordinator/fleetlog.py`
  helper, each exposing a one-call append that never fails the caller
  (best-effort; swallow all errors).
- Both write **byte-compatible** JSONL envelopes (same schema/field names) to
  per-process files, so one `jq` reads Go + Python lines uniformly.
- Explicit `Log(...)` calls wired at the action sites below.
- 3-day retention that actually fires (a once-per-day throttled prune from the coord tick; no gc dependency).
- Unit + integration tests per the test plan. `go build ./... && go test -race
  ./... && golangci-lint run ./...` and `python3 -m pytest skills/ -q` green.

## Deliverables / file scope

| Area | Files |
|------|-------|
| Go emitter | `internal/fleetlog/fleetlog.go` (+ `_test.go`): `Log()`, `Dir()` (XDG-aware), `PruneOlderThan()`, per-process file path w/ `pid_start` |
| Python emitter | `skills/coordinator/fleetlog.py` (+ test): mirror envelope; raw `os.open`+`os.write`; **its own log-specific `dir()` — `XDG_STATE_HOME/fleet/logs` if set, else `_resolve_home()/logs`** (mirrors Go's `fleetlog.Dir()`; do NOT use bare `_resolve_home`, which ignores XDG → would diverge from Go and break byte-compat) |
| Retention | `fleetlog.PruneOlderThan(72h)` (date-from-filename unlink) + a **once/day throttled call from the coord tick** (`loop.py`), guarded by a `logs/.last-prune` mtime marker. **No `fleet gc` KindLogs** — gc runs unreliably (worktrees-scoped, only ~every 20 ticks / on dispatch+status / manual); the coord that writes the logs also prunes them. |
| Call sites | `skills/coordinator/loop.py` (tick action sites + daily prune call), `internal/workers` (`WriteState`), `internal/spawn` (`Spawn`), a few `cmd/fleet` RunEs (`drain`/`handoff`/`dispatch`/`gc`/`attach`) — explicit `Log` + `defer` finish |

Schema, event vocabulary, correlation keys, location rules, per-process-file
rationale: **all specified in the parent design** — do not re-derive.

## Execution order

1. Go `internal/fleetlog`: envelope + `Dir()` + `Log()` + `PruneOlderThan()` + tests (the core; nothing depends on wiring).
2. Python `fleetlog.py` mirroring the envelope (byte-compatible) + test.
3. Retention: `PruneOlderThan` + the once/day throttled coord-tick call + test.
4. Call-site wiring (coord tick, worker, spawn, key CLIs) + the daily coord-tick prune call.
5. Full verify + integration test (a real tick emits the expected lines; cross-language jq read).

## Test plan (Setup / Input / Expected)

**Philosophy: drive REAL triggers, then verify the whole log lifecycle in one
test** (schema + count + dir + cross-language), rather than many granular units.
`T1` is the centerpiece lifecycle test; the rest cover behaviors a lifecycle run
can't easily force. All tests set `FLEET_HOME=<tmpdir>` and **clear
`XDG_STATE_HOME`** unless the case sets it. "the file" =
`<dir>/fleet-<today>-<comp>-<pid>-<pidstart>.jsonl`.

**T1 — LIFECYCLE (the main test): real triggers → schema + count + dir + Python.**
- Setup: test home with `tasks.md` holding 1 `ready` task; fake-tmux so dispatch is inert.
- Input — fire one of each real trigger:
  1. run a `loop.py` coord tick that dispatches the task (**Python** emitter → `coord.tick` start+end, `dispatch.worker`/`decision`);
  2. `workers.WriteState(slug, "review-pending")` (**Go** worker emitter → `state.transition`);
  3. run a wired CLI command `fleet drain` (**Go** CLI emitter → `cli.start` + `cli.finish`).
- Expected — assert the lifecycle in one place:
  1. **Schema** — every emitted line validates against the envelope: required keys present, types correct, `type` ∈ the closed vocabulary, `ts` RFC3339 sub-second, `id` non-empty.
  2. **Count** — total lines == the expected number of triggered events (e.g. `coord.tick`×2 + `dispatch.worker`×1 + `state.transition`×1 + `cli.start`×1 + `cli.finish`×1); and the `dispatch`/`state.transition` lines carry the right `slug`, `state.transition` carries `data.from`/`data.to`, `cli.start` `data.argv` contains `"drain"`, `cli.finish` `data.rc` present.
  3. **Dir + filenames** — lines live under `fleetlog.Dir()` in per-process files named `fleet-<date>-<comp>-<pid>-<pidstart>.jsonl`, with the right `comp` per source (`coord`/`worker`/`cli`).
  4. **Python emitter + cross-language parity** — the Python (coord) lines and the Go (worker/CLI) lines are **all** present and byte-compatible: one `jq -c '.type' <dir>/*.jsonl` parses every line from both languages with the identical key set (proves the Python emitter matches Go).

**T2 — best-effort: logging never fails the caller.**
- Setup: (a) logs dir unwritable (`chmod 0500`); (b) emit forced to fail during a tick.
- Input: `Log(...)` once in (a); run one tick in (b).
- Expected: (a) returns without error/panic, nothing written; (b) the tick completes and returns its normal JSON result — the emit error never propagates.

**T3 — `data` cap + logged raw (no redaction).**
- Setup: tmp home.
- Input: `Log(... Fields{data:{"blob": <3000 chars>, "tok":"ghp_EXAMPLETOKEN…"}} ...)`.
- Expected: line ≤ ~2 KB + envelope; `data.blob` truncated with an elision marker; `data.tok` written **verbatim** (no scrub — pins "log raw"); valid JSON.

**T4 — concurrency / durability (no torn lines, per-process isolation).**
- Setup: shared tmp home.
- Input: Go — 1 process × 50 goroutines × 200 `Log`; Python — fork 20 children × 500 `log`.
- Expected: each process writes only its own per-pid file; **every** line across all files independently parses (no torn/interleaved line — pins per-process O_APPEND + Python raw `os.open`/`os.write`, not buffered `open("a")`). Also covers date rollover: a `Log` straddling injected UTC-midnight lands in the next-day file.

**T5 — retention prunes >3 days, throttled to once/day.**
- Setup: `logs/` with a 4-day-old file (by filename date) + a today file; `.last-prune` mtime varied.
- Input: (i) direct `fleetlog.PruneOlderThan(72h)`; (ii) a coord tick with `.last-prune` 10 min ago; (iii) a coord tick with `.last-prune` 25 h ago.
- Expected: (i)+(iii) → 4-day file unlinked, today file kept, `.last-prune` updated; (ii) → throttle skips, no readdir, 4-day file still present.

## Non-goals (do NOT build)

- **No `fleet logs` reader command** — read raw JSONL with `jq`/`grep`; reader is a deferred follow-up.
- **No redaction / secret-scrub** — values logged raw (local-only + 3-day TTL).
- **No cobra-hook / `main()`-wrapper auto-instrumentation** — explicit per-command `Log` calls only.
- **No `fleet gc` KindLogs** — retention is the once/day coord-tick prune only (gc runs too unreliably to depend on).
- **No rotation library, no flock, no shared file** — per-process files + date-prune.
- Do not refactor unrelated coord/worker code; logging calls are additive and fire-and-forget.

## Notes for the worker

- Logging is **fire-and-forget**: a `Log` error must never fail a tick or command.
- **`fleet attach` emits only `cli.start`** — a successful attach `execve`-replaces
  the process (`attach.go` / `tmux.go`), so a deferred `cli.finish` never runs on
  success (by design, not a bug). `cli.finish` for `attach` fires only on the
  error-return path. Don't add a test asserting attach's finish-on-success.
- **Python dir resolver is XDG-aware** (see deliverables) — a tiny log-specific
  helper, not `_resolve_home`. This keeps Go and Python writing to the same dir
  so tests T9 (byte-compat) and T8/T14 (XDG) hold.
- The handoff Slice-3 work touches the same coord-tick action sites for *decision recording*; keep the `Log` calls independent (no shared return contract). If that PR lands first, rebase; logging adds its own lines.
- Follow the standard SOP: worker writes code+tests and exits at review-pending; reviewer runs `/review` + codex; finisher pushes + opens PR (base `main`).
