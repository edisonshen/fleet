# DESIGN — Fleet local debug logs (for agent/LLM consumption)

- **Status:** APPROVED 2026-06-27 (operator approved simplified scope; dual-reviewed over 5 rounds + research + final consistency confirm)
- **Scope (v1, deliberately small):** one append lib function — `internal/fleetlog` (Go) + a Python emit helper in `skills/coordinator/` — called explicitly at coord-tick / worker / key-CLI action sites; a 3-day prune via a once/day coord-tick call. **No reader command, no redaction, no `fleet gc` KindLogs** (all deferred / cut — see Not doing).
- **Priority:** P1 (operator-named 2026-06-26)
- **Depends-on:** none (shares tick call-sites with handoff Slice 2/3 — see "Land order")
- **PR-base:** main

## The problem (plain English)

When Fleet misbehaves — a handoff storms, a drain leaks, a coord spawns a
duplicate, a worker dies without a PR — there is **almost no durable record of
what happened**. You reconstruct the story from tmux scrollback (which scrolls
away) and `state.json` files (which show the *current* state, never the sequence
that produced it). By the time you look, the evidence is gone.

The operator asked for this directly (2026-06-26):

> *"we need detailed log files, it could be local, it help us to debug in the
> future. we can keep the log for 3 days."*

> *"this log is not for human to read, it helps agent or llm to understand what
> happens."*

**The consumer is an agent/LLM, not a human.** A successor coordinator, a
debugging subagent, or the coord itself reads the log to reconstruct *what
happened, what was decided, what failed*. That single constraint drives every
choice below: the format is structured for machine parsing and each event is
self-describing enough for an LLM to infer causality from a `jq`/`grep` slice —
no external schema needed to read it.

There is a `~/.fleet/logs/` directory today — Fleet creates it on init
(`state.go:33`) — but **nothing writes to it.** It is an empty promise.

## How it works today

Fleet's diagnostics are ad-hoc and ephemeral. There is **no shared logging
helper**; every site writes its own `Fprintf(os.Stderr, ...)` (Go) or
`sys.stderr.write(...)` (Python), straight to a tmux pane that forgets it.

```
what happens                         where it goes              survives?
------------                         -------------              --------
coord tick (dispatch/reconcile/      sys.stderr → tmux pane     NO (scrolls away)
  sentinel/PR-watch decisions)
worker spawn / phase / exit          state.json (current only)  partial (no history)
CLI command run (drain/gc/handoff)   stdout/stderr → terminal   NO
OOM / jetsam kill                    ~/.fleet/incidents/*.json   yes (one file/kill)
```

```
   coord (python) ──stderr──▶ tmux pane ──▶ (scrolls away, gone)
   worker (go)    ──stderr──▶ tmux pane ──▶ (gone)
   fleet CLI      ──stdout/stderr──▶ terminal ──▶ (gone)

   ~/.fleet/logs/  ◀── nothing writes here
```

## What goes wrong

When the operator (or a debugging agent) asks "what happened?", the answer is
archaeology: grep state files for clues, guess at timing, hope a tmux session is
still alive. The 2026-06-07 handoff-storm bug (16 handoff docs, 81 leaked drains
over 3 days) needed live process inspection to diagnose *because there was no log
to read*. Recurring lifecycle bugs are expensive precisely because the causal
sequence is never recorded — and a successor coord that could otherwise
self-diagnose from a log instead starts blind.

## The fix

**Every Fleet process appends a structured event stream to its own file under
`~/.fleet/logs/` via one small lib function; an agent reads the raw JSONL
(`jq`/`grep`) to reconstruct what happened; files older than 3 days are pruned.**
v1 is the **writer + retention only** — the schema and per-process files are
designed so a `fleet logs` retrieval command *can* be added later, but it is not
in v1.

Design decisions, each grounded in prior art (research briefs 2026-06-26):

### 1. Per-process files, merged at read time (not one shared file)

The naive design — one shared daily file, every process appends — is **fragile
across a heterogeneous Go + Python + fork-happy-CLI tool**. POSIX does *not*
guarantee non-interleaving of concurrent regular-file appends (the `PIPE_BUF`
atomicity rule is pipes-only; buffered writers split a record across syscalls;
NFS races). Holding "one unbuffered `write` per record, on a local FS, in two
languages" as an invariant is a standing footgun.

Instead, **each process owns its own file**, fingerprinted by pid **and process
start time** (`pid_start`) so same-day PID reuse can never collide — the repo
already carries a `pid_start` fingerprint for exactly this reuse hazard
(`drain_runrecord.go`):

```
~/.fleet/logs/fleet-<YYYY-MM-DD>-<component>-<pid>-<pid_start>.jsonl
   e.g.  fleet-2026-06-27-coord-40212-738291.jsonl
         fleet-2026-06-27-worker-51883-738410.jsonl
         fleet-2026-06-27-cli-51999-738455.jsonl
```

No two processes ever write the same file (the `pid_start` suffix makes a reused
pid a distinct file) → **zero write contention, no flock, no rotation race,
crash-safe** (a killed process just stops appending to *its* file). Any merge
across files happens only at **read** time — a `jq`/`sort` one-liner today, or
the deferred `fleet logs` command later — sorting on `(ts, comp, pid, seq)`. This
is the standard answer when multi-writer contention is the concern.

```
   coord  ──▶ fleet-DATE-coord-PID.jsonl   ─┐
   worker ──▶ fleet-DATE-worker-PID.jsonl  ─┼─▶ read raw: jq/grep/sort by ts
   cli    ──▶ fleet-DATE-cli-PID.jsonl     ─┘   (a `fleet logs` cmd is a later add)
```

### 2. JSONL, one self-describing event per line, OTel-GenAI-aligned schema

One JSON object per line (JSONL/NDJSON): self-describing (keys travel with
values, so the reader needs no external column schema), `grep`/`jq`-able, and
partial-tail-safe (a truncated last line never corrupts the rest). For an LLM
reader this is the consensus structured-log format.

Every line carries a fixed envelope + correlation keys + causal pointer, reusing
**OpenTelemetry GenAI attribute names** where a model/tool call is involved (free
interop; names the LLM has seen in pretraining):

```json
{"ts":"2026-06-27T05:13:51.357212Z","seq":418,"type":"decision","lvl":"info",
 "comp":"coord","pid":40212,"proj":"projects-fleet","agent":"1492d6f6",
 "gen":1,"session":"1492d6f6","dispatch_id":"dc146798","slug":"slice-1-4",
 "pr":240,"caused_by":"evt-0417","id":"evt-0418",
 "msg":"dispatched reviewer for slice-1-4 because phase=review-pending",
 "data":{"phase":"review-pending"}}
```

| field | meaning |
|-------|---------|
| `ts` | RFC3339 UTC, microsecond — the merge key |
| `seq` | per-process monotonic counter — stable tie-break in the merge |
| `type` | event kind from a **closed vocabulary** (below) — the reader's primary filter |
| `lvl` | `debug`/`info`/`warn`/`error` — cheap severity filter for token-bounded reads |
| `comp`/`pid` | component (`coord`/`worker`/`cli`) + os pid |
| `proj`/`agent`/`gen` | project, 8-hex agent id, dispatch generation (disambiguates successor coords) |
| `session`/`dispatch_id` | coord lifeline (= OTel `conversation.id`); per-dispatch id workers echo (= `trace_id` role, links a worker's lines to the coord decision that spawned it) |
| `slug`/`pr` | task slug, PR number — correlation keys, omit when absent |
| `caused_by`/`id` | this event's id + the triggering event's id — gives the LLM **causal links** to replay the chain |
| `msg` | one self-describing clause written for an LLM ("dispatched X because Y"), not a terse code |
| `data` | small structured payload (≤2 KB, size-capped) — details |
| `gen_ai.*` | for model/tool events, **OTel-inspired** names: `gen_ai.tool.name`, `gen_ai.usage.{input,output}_tokens`, `gen_ai.response.finish_reasons` (these are real OTel GenAI semconv keys but currently marked *moved/experimental* — pin the semconv version we copied), plus `error.type` (current/stable) |

### 3. Closed event vocabulary, with `decision` first-class

A small fixed `type` set the LLM can pattern-match and filter without parsing
prose:

```
coord.start  coord.handoff  coord.resume  coord.tick  decision
dispatch.worker  worker.start  tool.call  tool.result  model.call
state.transition  pr.opened  pr.status  task.completed  worker.failed
cli.start  cli.finish  error  cleanup
```

`decision` is the load-bearing one: *"chose X over Y because Z."* The agent-log
literature is unanimous that this — *why* the agent acted — is the highest-value,
least-standardized signal a successor needs, and the thing existing tools capture
worst. Fleet authors `decision` events deliberately at every real coord choice
(dispatch, raise-hand, rebase-vs-wait, park, flip-done).

> **Synergy (not coupling):** the coord's `decision` events overlap with the
> handoff doc's **Key Decisions** section (Slice 3 of the handoff design). Same
> signal, two consumers (live log vs. handoff snapshot). The two PRs stay
> decoupled now; a later pass may let the handoff lift Key Decisions straight
> from the log. Noted so we don't build two divergent decision vocabularies.

### 4. No redaction — log raw (operator decision 2026-06-27)

The events fleet authors are **structural orchestration metadata** — `slug`,
`pr`, `phase`, `rc`, `type`, ids, timestamps. None of those is a secret or PII;
this is not LLM-prompt/response logging. So v1 does **no redaction** and logs
values as-is. This is a deliberate simplification: no scrub pass, no regex set,
no category labels.

**Accepted tradeoff (stated, not buried):** the one place external text enters an
event is a **failure/error field** that may interpolate `git`/`gh` **stderr** —
which can occasionally echo a tokenized remote URL
(`https://x-access-token:ghp_…@github.com/…`). With raw logging that token lands
in the file, and since the log feeds a successor agent it could reach that
agent's context. The operator accepts this because the log is **local-only**
(never shipped anywhere) and **pruned after 3 days**. If this ever bites, the
cheap mitigation is the construction discipline — log error *categories*
(`error.type`) instead of raw stderr — added as a follow-up, not v1.

- **Cap `data`** fields (truncate > ~2 KB with an elision marker) — a size bound,
  not a secret control; large payloads are referenced by path, never inlined.

### Reading the logs (v1: raw files; no reader command)

v1 ships **only the writer** — the lib function below. There is **no `fleet logs`
command**. The files are plain JSONL, so an agent or operator reads them
directly: `cat`/`grep`/`jq` across `~/.fleet/logs/fleet-<date>-*.jsonl`, e.g.
`jq -c 'select(.slug=="slice-1-4")' ~/.fleet/logs/*.jsonl`. Because every line is
self-describing and carries correlation keys (`slug`/`pr`/`agent`/…), a raw
grep/jq already reconstructs a task's or PR's story.

A dedicated `fleet logs` retrieval command (merge-sort across per-process files,
filter by key/type/level, token-bounded `--tail`/`--summary` for an LLM reader)
is a **deferred follow-up** — nice once there's volume, not needed to start.
Keeping v1 to "append + read raw" is the operator's explicit scope: *a simple lib
function, an event per action, the agent/LLM schema — that's it.*

### Retention — simple 3-day prune

Files are date-stamped in the name, so no rotation library and no
"rotate-while-fd-held" race. A `PruneOlderThan(72h)` deletes
`logs/fleet-*.jsonl` whose filename date is > 3 days old, triggered **one** way:

- **One throttled call per day from the coord tick** — the regularly-running
  long-lived process that produces most logs. A `.last-prune` mtime guard means
  it readdir-scans at most once a day.

**Why not `fleet gc`?** gc runs unreliably — only `--kinds=worktrees` every ~20
ticks from a live coord, plus on `fleet dispatch`/`status` and manual runs; never
on a timer. Depending on it for the 3-day prune would be the same
unreliable-trigger trap. The coord that *writes* the logs (its own events + the
workers/CLIs it spawns) is the natural thing to *prune* them. A mostly-idle
install with no coord running produces almost no logs, so a few stale files
until the next coord run is acceptable for a 3-day local debug log. No per-CLI
hook, no gc Kind — deliberately simple.

### Location

Default `~/.fleet/logs/` — Fleet centralizes all state under one root
(`agents/`, `queue/`, `inbox/`, `worktrees/`), and splitting just logs out
fragments that. But **honor `XDG_STATE_HOME`** when set (the XDG spec's
designated home for logs) → `$XDG_STATE_HOME/fleet/logs`, via the log-specific
`fleetlog.Dir()` resolver — *not* by changing `state.Root()` (which would
relocate the whole state tree). The one genuinely arguable location call;
documented, not hand-waved.

## Implementation detail (for engineers)

### Emitter — Go (`internal/fleetlog`)

- `Log(comp, evt, lvl, fields...)`: builds the envelope (stamps `ts`, `seq` from
  a process-local atomic counter, `pid`), applies the 2 KB `data` cap,
  `json.Marshal`es to a `[]byte`, appends `'\n'`, and does **one** `f.Write` to
  the process's own file opened `O_WRONLY|O_APPEND|O_CREATE`. Single-syscall
  write of pre-marshaled bytes — no buffered writer.
- Path: `fleetlog.Dir()/fleet-<date>-<comp>-<pid>-<pid_start>.jsonl`, where
  `fleetlog.Dir()` is a **log-specific resolver**: `$XDG_STATE_HOME/fleet/logs`
  when `XDG_STATE_HOME` is set, else `state.Root()/logs`. Do **not** extend
  `state.Root()` to honor `XDG_STATE_HOME` — `state.Root()` (`state.go:40`, only
  honors `FLEET_HOME`) roots the *entire* state tree (`agents/`, `queue/`,
  `handoffs/`, …); relocating all of it would be wrong, and the doc only wants
  logs under XDG. `MkdirAll` defensively; **swallow all errors** — logging must
  never fail a command (best-effort).
- File handle is opened once per process and reused (re-open on date rollover).

### Emitter — Python (`skills/coordinator/fleetlog.py`)

- Mirror the envelope + 2 KB cap. **Raw fd, single syscall:** `fd =
  os.open(path, os.O_WRONLY|os.O_APPEND|os.O_CREAT, 0o644)`; `os.write(fd,
  line_bytes)` with the newline already in `line_bytes`. **Not** `open(...,"a")`
  (buffered text I/O fragments lines). Short write → dropped, not retried.
- **Dir via a log-specific XDG-aware resolver** that mirrors Go's `fleetlog.Dir()`:
  `$XDG_STATE_HOME/fleet/logs` if `XDG_STATE_HOME` is set, else
  `_resolve_home()/logs` (`_resolve_home` at `loop.py:2734`). Do **not** use bare
  `_resolve_home` — it ignores `XDG_STATE_HOME`, so Go (XDG-aware) and Python
  would write to different dirs when XDG is set, breaking the one-`jq`-reads-both
  byte-compat invariant. Do not broaden `_resolve_home` itself.

### Call sites — explicit calls, no framework magic

The lib function is called **explicitly** wherever a meaningful action happens.
No cobra-hook auto-instrumentation, no `main()` exit-code plumbing (that
machinery was the source of two review P1s and fights the "keep it simple" goal).
Each call is one line at the action site.

- **Coord (Python):** call `fleetlog.log(...)` at the tick's action sites — the
  loop already iterates `for action in dispatched/reconciled/…` and owns the data
  for the line. Dispatch, reconcile done-transition, sentinel apply, PR-watch
  event, decision, `coord.tick` start/end. (These are the same sites the handoff
  Slice-3 decision-recorder uses — one explicit call each, independent.)
- **Worker (Go):** `workers.WriteState` (phase transition, `workers.go:299`) and
  `spawn.Spawn` (worker spawn).
- **CLI (Go):** an explicit `fleetlog.Log("cli.start", …)` at the top of the few
  commands worth tracing (`dispatch`, `drain`, `handoff`, `attach`, `gc`) + a
  `defer` for `cli.finish` (duration; `rc` best-effort from the RunE result).
  Explicit and per-command — **no** root `PersistentPreRunE`/`main()` wrapper, no
  exit-code mapping. Adding a call to another command later is one line. (v1 need
  not cover all 22 subcommands; the orchestration story lives in the coord +
  worker events.)

### Retention

- `internal/fleetlog.PruneOlderThan(72h)`: list `logs/fleet-*.jsonl`, parse the
  date from the filename, unlink those > 3 days. Called **once/day from the coord
  tick**, guarded by a `logs/.last-prune` mtime marker (skip the readdir if
  pruned < ~24 h ago). No `fleet gc` Kind, no per-CLI hook. Best-effort; errors
  swallowed.

### Land order (cross-doc seam)

Logging shares tick call-sites with handoff Slice 2/3, but logging is
**fire-and-forget** (a best-effort line, no return-value contract), while Slice 3
decision-recording is **durable state**. They don't fight: land independently;
whichever PR lands second rebases. State this in both task plans.

## Test plan

| # | Scenario | Expected |
|---|----------|----------|
| 1 | Go `Log` one event | own `fleet-<date>-<comp>-<pid>-<pid_start>.jsonl` line, full envelope, valid JSON |
| 2 | Python `Log` one event | byte-compatible envelope; same reader parses Go + Python lines |
| 3 | emit when logs dir missing | dir auto-created; line written; no error returned |
| 4 | emit when dir unwritable | caller gets no error; nothing crashes |
| 5 | `data` field > 2 KB | truncated at the cap with an elision marker (size bound; no scrubbing — values logged raw by design) |
| 6 | two processes emit concurrently (stress) | each writes its OWN file; no interleave possible |
| 7 | Python uses raw `os.write` not buffered `open("a")` | a line is one syscall; no fragmentation under a forking stress test |
| 8 | events cross UTC midnight | process rolls to next day's filename |
| 9 | coord tick runs | `coord.tick` start+end with counts; one line per dispatch/reconcile/sentinel/pr-watch event |
| 10 | worker phase transition | a `state.transition` line (slug, from→to) |
| 11 | CLI command (`fleet drain`/`handoff`) | explicit `cli.start` + `defer` `cli.finish` (duration) line |
| 12 | coord-tick daily prune, a 4-day-old file present | file deleted; `.last-prune` mtime guard skips the scan if run again < 1 day |
| 13 | direct `PruneOlderThan(72h)` | deletes >3d files, keeps recent, writes `.last-prune` |
| 14 | `XDG_STATE_HOME` set | logs land under `$XDG_STATE_HOME/fleet/logs`; unset → `~/.fleet/logs` |

## Assumptions

- `~/.fleet/logs` (or `$XDG_STATE_HOME/fleet/logs`) is a local POSIX filesystem.
- Per-process file count is small (a handful live at once); reading raw with
  `jq`/`grep` is fine at a 3-day window.
- Best-effort logging is acceptable: under disk-full / unwritable conditions
  events may be dropped silently rather than failing the command.
- Tests that assert emit/prune paths under `FLEET_HOME` must **clear
  `XDG_STATE_HOME`** (an ambient value would silently redirect logs via
  `fleetlog.Dir()` and pollute the assertions).

## Not doing

- **No `fleet logs` reader command in v1** — read the raw JSONL with `jq`/`grep`;
  a retrieval command (merge/filter/`--summary`) is a deferred follow-up.
- **No redaction** — values logged raw (operator decision; local-only + 3-day
  TTL). Construction discipline (log error *categories*, not raw stderr) is the
  cheap follow-up if it ever bites.
- **No CLI auto-instrumentation** — explicit `fleetlog.Log` calls per command, not
  a cobra-hook/`main()`-wrapper framework (that machinery generated two review
  P1s for no real benefit).
- **No `fleet gc` KindLogs** — retention is the once/day coord-tick prune only;
  gc runs too unreliably (worktrees-scoped, ~every 20 ticks / dispatch+status /
  manual) to be the trigger.
- No single shared log file / no flock — per-process files make both moot.
- No rotation library (lumberjack / TimedRotatingFileHandler) — they break under
  multiple processes; date-in-filename + prune replaces them.
- No remote/centralized shipping — local only.
- No OTLP/trace export in v1 (schema is OTel-aligned so it stays possible later).
- No retrofit of `incidents/` or `drain-runs/` writers — the new log is additive.
- No inline large payloads (diffs/file bodies) — referenced by path only.
