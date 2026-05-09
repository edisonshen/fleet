---
name: coordinator
description: Per-project coordinator that owns tasks.md, dispatches workers under fleet dispatch, monitors PR/CI via gh, and raises hand to the operator only when human input is needed. Reads tasks.md (read-only via parse.py) and mutates exclusively through the fleet CLI (`fleet tasks set`, `fleet tasks note`, etc.) — Go remains the authoritative writer. One coordinator per project enforced via NB-flock on coordinator.lock. v0.2 single-worker mode by default; cap > 1 enabled when worktrees land in v0.2.x.
---

# coordinator

Per-project tick loop for Fleet v0.2. Replaces the operator-driven hand-pick-and-dispatch flow with an autonomous coordinator that watches a markdown task list and feeds workers through the existing v0.1 dispatch primitives.

The skill ships agent-side. Each Stop hook fire (or operator-triggered `fleet message <coord_id> ...`) runs `loop.tick()` once and exits. Restart equals resume from disk state — there is no daemon, no shared memory, no asyncio.

Source of truth: `docs/PLAN-v0.2-coordinator.md` (the what + why) and `docs/ENG-v0.2-coordinator.md` (the how — package shapes, sequence diagrams, perf budget, test plan). The skill mirrors the algorithm in those docs; deviations are bugs.

## Coord agent role

The Claude Code session running this skill is a **coordinator**, not a worker. The agent's job is to discuss design with the operator, file tasks, and dispatch workers. Implementation, testing, and any code-touching work goes to detached worker agents — never inline in the coord's own session.

This section mirrors the first-turn dispatch prompt (`coordSpawnPrompt` in `internal/tui/keys.go`) so the constraint survives context handoffs: a successor coord re-reads `SKILL.md` on its first turn but does NOT see the original spawn prompt.

**ROLE — discuss design with the operator, file tasks, dispatch workers. NEVER:**
- Edit code files (no Edit, Write, NotebookEdit on source code).
- Run tests (no `go test`, `pytest`, etc. — workers handle this).
- Implement features inline.
- Run any tool that mutates the project source tree.

**DELEGATE — for any implementation, testing, or code-touching work:**
1. Discuss design with the operator until aligned.
2. File a task via `fleet tasks add --project <project> --spec <body>`.
3. Promote the task with `fleet tasks promote <slug>` when ready.
4. The /coordinator skill auto-dispatches a worker on next tick.
5. Track progress via the supervisor loop.

**ALLOWED — the toolbox is intentionally narrow:**
- Read code files for design discussion (Read, Grep, Bash with non-mutating commands).
- Run fleet CLI: `fleet tasks {add,list,show,set,note,promote}`, `fleet workers list`, `fleet peek`, `fleet learnings`, `fleet standards show`.
- Run gh CLI for status: `gh pr view`, `gh pr checks`, `gh issue view`.
- Talk to the operator about design, scope, priority.

If the operator says "implement X" or "fix this bug", the right response is "that's worker work — let me file the task and dispatch", NOT to start editing files. The coord is a manager; a coord that does the work itself burns the operator's main context on what should be a detached session.

## Invocation

The coordinator agent is dispatched via existing `fleet dispatch`:

```bash
fleet dispatch fleet-coord-<project> \
  --project <project> \
  --cwd <repo-path> \
  --command "claude 'Run the /coordinator skill loop for project <project>.'"
```

Agent-side environment (set by `fleet dispatch`):

- `FLEET_AGENT_ID` — coord's 8-hex ID. Without it the skill exits silently (fleet-guard discipline).
- `FLEET_HOME` — defaults to `~/.fleet/`. Override for sandboxed tests.
- `FLEET_PROJECT` — set by the dispatch path; falls back to argv[0] when invoked manually.

The skill does NOT need a hook payload — `loop.main` reads `FLEET_PROJECT` (or argv) and runs one tick. The tick is short (target < 500ms p99 per ENG §8.1) and emits a JSON summary to stdout.

## Files this skill writes

| Path | Content | Atomicity |
|------|---------|-----------|
| `~/.fleet/projects/<p>/.locks/coordinator.lock` | flock target (zero-byte) | NB acquired per tick, released on exit |
| `~/.fleet/projects/<p>/coord-state.json` | `last_archive_scan_ts`, `worker_agent_ids`, `supervisor.<slug>` (nudged_at, escalated_at, consecutive_stuck_polls, last_phase) | tmp + rename + fsync |
| `~/.fleet/inbox/<worker_id>.md` | freshly built worker prompt for one dispatch, OR a stuck-idle nudge body (issue #79) | tmp + rename + fsync |

## Files this skill reads (config)

| Path | Content | Default |
|------|---------|---------|
| `~/.fleet/projects/<p>/coord-config.json` | `{"parallelism": <int 1..50>}` | `{"parallelism": 1}` (single-worker mode) |

`parallelism > 1` enables worktree-mode dispatch: each worker gets a
git worktree at `~/.fleet/projects/<p>/worktrees/<slug>/` branched
`worker/<slug>` off the repo's HEAD. Worker cwd is the worktree, not
the main repo. Worktrees are removed via `git worktree remove --force`
when the worker's task transitions to `in-review` (TASK_DONE_PR
sentinel) or `done` (CI merged via reconcile). The branch lives on so
the PR stays valid; only the working tree is freed.

The skill does NOT write `tasks.md` directly. Every mutation goes through `fleet tasks set <slug> <key>=<value>` or `fleet tasks note <slug> <text>`, so Go remains the only writer of the per-project task registry. parse.py is read-only inside the skill.

## Loop algorithm

```
1. NB-flock coordinator.lock
   on EWOULDBLOCK → log + exit 0  ("another tick in progress, skipping")

1.5. Orphan worktree cleanup (cap > 1 only):
     `git -C <repo> worktree prune` — drops registry entries whose dirs
     are missing (e.g. coord crashed mid-tick after `git worktree add`).
     Idempotent and best-effort; failures log to stderr but never abort
     the tick. cap=1 mode skips this step (worktrees never created).

2. tasks   = parse(tasks.md)              # parse.py — read-only mirror of internal/tasks
   stds   = `fleet standards show --merged --project <p>`
   learn  = `fleet learnings list --project <p> --limit 20`

3. Reconcile in-flight workers:
   for t in tasks where status in {in-progress, in-review}:
     if t.worker_pid is alive (kill -0):     skip
     elif t.pr_url:
       ci = gh pr checks <url> --json state,conclusion
       all green + merged       → status=done, clear worker
       all green + not merged   → status=in-review, raise "ready to merge"
       not mergeable            → status=todo, clear worker, note "rebase needed"
       failed                   → status=todo, clear worker, note "CI red <url>", raise
       pending                  → leave as-is until next tick
     else:
       status=todo, clear worker, note "worker died without PR"
   apply via `fleet tasks set/note`

4. Drain inbox archive:
   for f in ~/.fleet/inbox/archive/<coord_id>-*.md (newer than last_archive_scan_ts):
     for line in f:
       TASK_DONE_PR=<slug> <url>      → set pr_url, status=in-review
       BLOCKED_QUESTION=<slug> <txt>  → status=blocked, note BLOCKED_QUESTION
       WORKER_FAILED=<slug> <reason>  → status=todo, clear worker, note WORKER_FAILED
       NEW_TASK=<slug>                → wake-only (no mutation)
   persist last_archive_scan_ts to coord-state.json

5. Dispatch ready tasks under cap (default 1; coord-config.json overrides):
   active = count(status==in-progress)
   for t in sort_by_priority(tasks where status==ready and deps_satisfied):
     if active >= cap: break
     # cap > 1 only: skip on file-overlap with any in-flight worker.
     # Conservative — a task with no Files: line is treated as "could
     # touch anything" and is skipped while any worker is in flight.
     # Operators opt out per task by adding `Files: <path-with-ext>`
     # to Spec / Acceptance / Notes (a real path, not a wildcard;
     # the heuristic regex requires a file extension).
     if cap > 1 and _has_conflict_with_inflight(t, in_flight_after_dispatch): continue
     # cap > 1: per-slug git worktree under projects/<p>/worktrees/<slug>
     if cap > 1:
       wt = `fleet workers worktree-path --project <p> <slug>`
       `git -C <repo> worktree add <wt> -b worker/<slug>`
       worker_cwd = wt
     else:
       worker_cwd = repo
     prompt = build_worker_prompt(t, stds, learn, branch=worker/<slug>)
     agent_id = `fleet dispatch <slug> --project <p> --cwd <worker_cwd>`
     write_worker_inbox(agent_id, prompt)                # ~/.fleet/inbox/<agent>.md
     `fleet tasks set <slug> status=in-progress`
     `fleet tasks set <slug> branch=worker/<slug>`
     `fleet tasks set <slug> worktree=<wt>`               # cap > 1 only
     `fleet tasks note <slug> "dispatched as agent <agent_id>"`
     active += 1

6. Return TickResult{skipped, parsed, reconciled, drained, dispatched, raised, errors}

7. Supervisor loop (issue #79). After the initial reconcile/drain/dispatch pass, the coord
   keeps the lock and watches in-flight workers via cheap state.json mtime polling. When a
   worker's mtime advances, scoped reconcile fires for that one slug. Every Nth poll a
   sparse stuck-check pass walks all probes and runs the recovery ladder for any worker that
   has gone idle (heartbeat stale + counter ≥ threshold + tmux session alive).

   Recovery ladder (per worker, per stuck-check pass):
     no nudge yet            → write inbox `[OPERATOR] You appear idle. ...`; record nudged_at
     nudged + cooldown elapsed → `fleet tasks set <slug> status=blocked`; append note
                                  `STUCK_IDLE_ESCALATED: <reason>`; record escalated_at
     escalated + cooldown elapsed → `fleet workers update <slug> --phase blocked --reason ...`

   Env-overridable knobs:
     FLEET_COORD_POLL_INTERVAL_S    default 30 (0 → supervisor disabled, single-tick fallback)
     FLEET_COORD_STUCK_CHECK_EVERY  default 10 (every 10 polls = 5 min on 30 s base)
     FLEET_COORD_STUCK_THRESHOLD_S  default 180 (3 min stale heartbeat)
     FLEET_COORD_STUCK_POLLS        default 3 (consecutive stuck passes before recovery)
     FLEET_COORD_NUDGE_COOLDOWN_S   default 120
     FLEET_COORD_POLL_MAX_S         default 14400 (4 h hard cap)
```

The fleet CLI commands above came from Phase B (`fleet tasks {add,list,show,set,note,archive,promote}`, `fleet learnings {add,list,prune}`, `fleet standards {show,edit}`, `fleet workers {list,prune}`, `fleet peek <slug>`). Their argv contracts must stay stable for this skill — see `cmd/fleet/tasks.go` etc.

## Worker prompt template

Built fresh per dispatch by `dispatch.build_worker_prompt(task, project, standards_md, learnings_text)`. ENG §6.5 specifies the layout. Hard cap 16KB rendered; oversized prompts raise `PromptTooLargeError` and the loop records the failure rather than dispatch.

The rendered prompt is:

```
You are a Fleet worker for task: <slug>
Project: <project>
Branch: worker/<slug>

You are running as a Fleet-dispatched Claude session. The operator is NOT
watching this terminal — communicate progress via `fleet workers update
<slug> --phase <p>` after every phase boundary. Exit cleanly (Ctrl-D /
/exit) once you reach phase=done or phase=blocked; the coordinator polls
workers/<slug>/state.json to know when to advance the task.

State file:  ~/.fleet/projects/<p>/workers/<slug>/state.json
Output log:  ~/.fleet/projects/<p>/workers/<slug>/output.log

## Task
<### Spec body, verbatim from tasks.md>

## Acceptance
<### Acceptance body, verbatim>

## Standards (the bar — non-negotiable)
<merged content from `fleet standards show --merged`>

## Relevant prior learnings
<top entries from `fleet learnings list --limit=20`, ≤500 chars × 5>
(section omitted entirely when no learnings recorded)

## Required workflow
  fleet workers update <slug> --project <project> --phase branch
1. git checkout -b worker/<slug>
  ... (TDD red/green/refactor → review-claude → review-codex → push → done)

## Constraints
- Stay on this task. File incidental bugs (max 3/session, honor system).
- Do NOT edit tasks.md or standards.md directly.
- Stuck → fleet workers update <slug> --project <project> --phase blocked --reason "<one line>"
```

Every `fleet workers update` invocation in the rendered prompt includes `--project <project>` so heartbeats land in the right `~/.fleet/projects/<project>/workers/...` tree even when the worker's cwd basename differs from the project name.

`dispatch.write_worker_inbox(agent_id, prompt)` drops the rendered prompt at `~/.fleet/inbox/<agent_id>.md`. fleet-guard's SessionStart hook reads that file on the worker's first turn and injects it as `[OPERATOR] <body>`.

## Sentinel grammar (worker → coord)

Workers write status reports to their own `state.json` in v0.2's revised contract (ENG §6.2). The coord watches via the inbox archive when a worker uses `fleet message <coord_id>` to bubble status. Each archive file MUST contain at most one sentinel per line, scoped by task slug:

```
TASK_DONE_PR=<slug> <pr-url>            # worker post-PR-push
BLOCKED_QUESTION=<slug> <one-line text> # worker stuck, needs operator
WORKER_FAILED=<slug> <reason>           # worker hit unrecoverable error
NEW_TASK=<slug>                         # operator (`fleet tasks add`) wake
```

Slug-keyed payloads are the C2 invariant (worker status reports never mix). The drain logic mutates only the task whose slug matches; unknown slugs are silently ignored. `_parse_sentinel` lives in `loop.py` for the canonical grammar.

## Reconcile + raise-hand

When a task is in-progress or in-review and its worker is no longer alive (worker_pid dead AND `workers/<slug>/state.json` either missing, stale, or terminal), the coord decides the next status from two signals in order:

1. **State.json terminal phase (in-progress only).** If the worker exited with `phase=done` + `pr_url`, flip to `in-review`, transcribe the PR URL onto tasks.md, raise "worker shipped". `phase=blocked` + reason → flip to `blocked`, raise. `phase=failed` → requeue to `todo`, clear pr_url, raise. The terminal-phase branch is gated to `status=in-progress` so subsequent ticks (already in-review) drive CI checks instead of re-flipping.
2. **PR URL + `gh pr checks` (in-review or fallback).** When the task is in-review or when in-progress fell through (no terminal state), the coord queries CI:
   - All green + merged → `status=done`.
   - All green + not merged → `status=in-review`, raise ("ready to merge").
   - Not mergeable → `status=todo`, clear worker, note "rebase needed" (keeps pr_url — same branch, different rebase).
   - Failed → `status=todo`, clear worker, **clear pr_url** (next worker opens a NEW PR), raise "CI red".
   - Pending → leave as-is.
   - No PR URL at all → `status=todo`, clear worker, note "worker died without PR".

`gh pr checks` is hit synchronously per tick. ENG §8.2 caches results on `coord-state.json:pr_check_cache` for 5 minutes; v0.2.0 ships without the cache (300ms cost is invisible in idle ticks). v0.2.1 may add it.

## Two-coord race

Second coord on the same project hits NB-flock EWOULDBLOCK. It logs + exits cleanly (`TickResult.skipped=True, reason="lock-busy"`). Operator notices via `fleet status` showing two coord rows; one is no-op. Cleanup: `fleet rm <id>` for the redundant coord.

## Auto-idle-stop (deferred)

PLAN §6 specifies coord auto-stops after 4h of zero active tasks. v0.2.0 does NOT implement this — the coord ticks until manually killed. v0.2.x adds the idle-streak tracking on `coord-state.json` and the clean-exit path.

## Coordinator handoff (C1)

The coord agent is itself under fleet-guard supervision. At 50%/70% context, fleet-guard hands it off. Because tasks.md is the source of truth and `coord-state.json` carries last_archive_scan_ts on disk, the successor coord re-reads tasks.md on its first tick, reconciles in-flight workers via `worker_pid` alive-check, and resumes. ENG §5.6 walks through this in detail.

The skill's tick lifecycle is intentionally lean (no in-process state survives across hook fires) so handoff is clean by construction.

## Failure modes

| Failure | Behavior |
|---------|----------|
| tasks.md parse error | `tick()` returns `skipped=True, reason="parse-error"` and records the error. Coord logs to stderr; operator fixes manually. |
| Two coordinators race | NB-flock makes the second exit cleanly. |
| `fleet dispatch` exits non-zero | Recorded in `TickResult.errors`; the candidate task stays in ready, retried next tick. |
| `fleet tasks set` exits non-zero | Recorded; partial mutations possible (e.g. status set but note not). Next reconcile catches it. |
| `gh pr checks` errors / not installed | `_CIResult(error=...)` — caller leaves the task as-is until next tick. |
| Inbox archive scan finds nothing | Tick proceeds normally. |
| Slug mismatch in a sentinel | Logged via `errors[]`; no mutation. |
| Prompt over hard cap | `PromptTooLargeError` — task NOT dispatched; recorded in errors. Operator shrinks standards/learnings/spec. |

## Tools used

- `python3` ≥ 3.9 (stdlib only — `subprocess`, `pathlib`, `re`, `tempfile`, `fcntl`, `json`).
- `fleet` binary on PATH (provides Phase B CLI: `fleet tasks ...`, `fleet learnings ...`, `fleet standards ...`, `fleet dispatch`, `fleet message`).
- `gh` binary on PATH for PR-status checks. Optional: when missing, the reconcile path skips CI evaluation and leaves PR'd tasks as in-review.

## Hook bindings

Unlike fleet-guard, this skill is NOT bound to Claude Code hooks via `~/.claude/settings.json`. It runs as a normal slash-skill the coord agent invokes on its own (`/coordinator`). The Stop hook still drives the cadence — the coord's own assistant turns trigger Stop, fleet-guard ticks, and at the natural sleep boundary the coord runs `/coordinator` again.

## Module layout

| File | Purpose |
|------|---------|
| `SKILL.md` | This document. Frontmatter + invocation + loop-in-prose + worker prompt template. |
| `loop.py` | One-tick driver. Public entry: `tick(project, ...)` and `main(argv)`. |
| `parse.py` | Python mirror of `internal/tasks` — read-only inside the skill, byte-equal with Go. |
| `dispatch.py` | Worker prompt assembly + `fleet dispatch` caller + inbox stub writer. |
| `conflict.py` | File-overlap heuristic for cap > 1 (default cap=1 never exercises this). Optimistic on no-paths inputs. |
| `loop._has_conflict_with_inflight` | Conservative loop-side wrapper: a task with no `Files:` line is treated as matching every in-flight task. Operators opt out per task by adding `Files: <real-path>`. |

## Tests

```
python3 -m pytest skills/coordinator/tests/ -v
```

Critical cases (mirrored from ENG §7.2):

- `test_parse.py::test_round_trip_byte_equal` — every fixture in `internal/tasks/testdata/` round-trips byte-equal in Python (CI gate against parser drift).
- `test_loop.py::test_tick_drains_per_task_sentinels` — C2 invariant: two slugs, two archive files, no cross-mutation.
- `test_loop.py::test_tick_no_op_under_lock_held` — second tick exits cleanly under lock contention.
- `test_loop.py::test_slug_mismatch_sentinel_ignored` — unknown slug logs WARN but mutates nothing.
- `test_loop.py::test_tick_skips_on_parse_error` — corrupt tasks.md doesn't crash the coord.
- `test_dispatch.py::test_dispatch_worker_invokes_correct_argv` — exact argv to `fleet dispatch`.
- `test_dispatch.py::test_write_worker_inbox_atomic_and_under_fleet_home` — inbox stub is tmp+rename.
- `test_conflict.py::*` — file-overlap heuristic positive + negative cases.
- `test_loop_conflict.py::*` — loop-side conservative wrapper at cap > 1: skip overlapping tasks, allow disjoint, conservative skip on no-Files candidates, cap=1 bypass.

The fleet binary itself is never invoked in unit tests; subprocess.run is mocked. End-to-end coverage lives in `cmd/fleet/coordinator_integration_test.go` (Phase D).

## Why these design choices

- **Skill is Python, parser ships in Go and Python.** Two parsers mean a CI-gated byte-equality contract. Removing one would force the other side to call out (Go shelling to Python skill = brittle; Python shelling to a Go binary on every tick = expensive). PLAN §"Code: skill side vs Go side".
- **Mutations through CLI, not Python writes.** Single writer per aggregate (STATE.md A2). Skill stays read-only on disk; locking and validation live in Go. Adding a parallel Python writer would require porting state.LockProjectState semantics.
- **One tick per invocation, no daemon.** Stateless reentry over markdown-as-truth is the Ralph rule. Restart equals resume; coord-state.json is the only across-tick state, and it's a couple of timestamps. Every per-task fact lives in tasks.md, every per-worker fact lives in workers/<slug>/state.json or agents/<id>.json.
- **NB-flock per tick, not per coord lifetime.** Flock auto-releases when the Python process exits, so re-acquiring on every tick is the documented pattern (ENG §4.3). Survives coord restarts and handoffs without leaking the lock.
