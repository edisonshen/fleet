# coordinator — reference

Detail the coordinator reads on demand. The runtime contract is `SKILL.md`;
this file holds the sections a coord does not need on every turn.

## Decision log

Key Decisions in the handoff is fed **automatically** — you do not have to
remember a logging step. Three producers write the durable per-coord
`session_decisions` buffer in `coord-state.json` (cap 50, stamped with your
agent id, deduped by text; tick noise never evicts it):

1. **Your own state changes.** `fleet tasks set <slug> status=… / priority=… /
   parked=…` and `fleet tasks promote` from your shell self-record one line
   (`set foo status ready → in-progress`, `promoted foo todo → ready`). No-op
   sets and bookkeeping fields (`pr_url`, `worker_pid`, …) are skipped.
2. **The tick's material transitions.** Reconciled → in-review, worker
   finished, requeued worker-failed, parked blocked-question, raised hand.
   Dispatches stay in the rolling `recent_decisions` only — Active Subagents /
   Status already narrate them.
3. **Operator conversation.** fleet-guard's Stop hook records the last
   operator prompt and your final reply as one line
   (`operator: defer cache, do auth first → coord: parked cache-1234, …`) —
   once per operator turn. A decision that lives only in chat still reaches
   the successor.

`fleet checkpoint decision "<what> — <why>"` remains for a rationale none of
the above can infer (e.g. `"Stopped rebase of PR #224 — superseded PR for an
operator-paused task"`); it writes the same buffer plus the rolling one.

The handoff doc's `## Status` block (task → status, PR state from
`pr-watches.json`, next job; then one `Next job:` line) is derived from
`tasks.md` + `coord-state.json` — keep `fleet tasks set` / `fleet checkpoint
next-step` current and the successor's first screen is accurate without any
extra work at handoff time. The same per-task line lands in each in-flight
task's `notes` in `tasks.md` at handoff.

## Step 7 — PR-TRACK

Shepherd every PR you own. Watch for terminal close/merge, CI failure, BEHIND,
DIRTY, and CHANGES_REQUESTED.

**The durable PR-watch is now the source of truth, not a hand-armed shell loop.**
Every tick derives a watch for each owned, non-terminal task with a `pr_url`
(state on disk at `~/.fleet/projects/<project>/pr-watches.json`, keyed by PR
number) and probes it — so a PR you own can never go unwatched across
compaction / handoff / restart, and "CI green" never ends a watch (only MERGED
or CLOSED does). The tick:
- on **MERGED**: flips ALL backing tasks `done` and prunes the watch (worktree
  reap rides the existing gc backstop);
- on **CLOSED without merge / orphaned PR / definitive 404**: raises hand;
- on **STALE (head not up-to-date under strict protection) / BEHIND / DIRTY /
  CI-fail / CHANGES_REQUESTED**: surfaces the event in the tick result.

A background `until ...; do sleep 30; done` loop may still be used **only** as an
optional wake-accelerator that triggers a tick sooner — correctness never
depends on it (the next tick re-derives the watch regardless).

Actions (PR2 will auto-dispatch these; until then act on the surfaced event):
- CI red: dispatch a fix-subagent on the same branch.
- BEHIND/DIRTY/STALE: dispatch a rebase-subagent in an isolated worktree.
- Substantive conflict or design feedback: raise hand.
- Merged: Step 8 (the watch already flipped the task `done`).
- Closed without merge: raise hand.

## Remote control (native, default-on)

Remote control is Claude-Code-only in v1 (a codex-dominant coord has no RC; the flag is engine-gated at every inject site). Remote control is NATIVE: `fleet dispatch --coord-spawn` (and the handoff / drain replacement paths) bake `claude --remote-control "fleet-coord-<id>-<project>"` into the coord's own claude argv, so mobile / claude.ai pairing is live the moment the coord starts. There is NO standalone `claude remote-control` listener daemon, NO per-tick respawn (`remote_control.spawn_daemon_if_needed` is a retired no-op shim), and NO send-keys injection. The gate is opt-OUT: the per-project `~/.fleet/projects/<p>/rc-disabled` marker (written by `fleet rc down`) suppresses the flag on the next coord spawn.

Workers and in-session subagents NEVER carry the flag — every inject site is gated on coord-ness (`--coord-spawn` / coord-spawn marker). That call-site carve-out is the architectural fix that retires the 5,620-mobile-push reviewer-loop hazard: there is no listener to respawn and no path that attaches RC to a reviewer loop.

Operator commands:
- `fleet rc up <project>` — re-enable: remove the rc-disabled opt-out marker (takes effect on next coord spawn).
- `fleet rc down <project>` — disable: write the opt-out marker + reap any legacy (pre-native) listener. A LIVE coord keeps its RC session until exit/handoff — `fleet handoff <coord-id>` respawns it without RC.
- `fleet rc connect <project>` — DEPRECATED no-op (native startup replaced the send-keys attach).
- `fleet rc status [<project>] [--healthy]` — observability; enabled = no opt-out marker.
- `fleet rc list` — enumerate projects with RC DISABLED (the exceptions).
- `fleet rc reset [<project>]` — emergency: reap legacy listener state + corrupt rc files (opt-out markers preserved).

## Files

Writes:
- `~/.fleet/projects/<project>/coord-state.json`
- `~/.fleet/projects/<project>/workflow.md`
- `~/.fleet/inbox/<agent_id>.md`
- approved docs under project `docs/` during PLAN-DOC/TASK-PLAN-DOC

Reads:
- `~/.fleet/projects/<project>/tasks.md`
- `~/.fleet/projects/<project>/coord-config.json`
- worker `state.json` files
- merged standards and learnings via Fleet CLI

Do not write `tasks.md` directly. Use Fleet CLI mutations only.

## Sentinels

Inbox archive lines:

```text
TASK_DONE_PR=<slug> [gen=<n>] <pr-url>
BLOCKED_QUESTION=<slug> [gen=<n>] <one-line text>
WORKER_FAILED=<slug> [gen=<n>] <reason>
NEW_TASK=<slug>
```

Slug mismatch means ignore and log. A sentinel mutates only its own slug.

State-mutating sentinels (TASK_DONE_PR / BLOCKED_QUESTION / WORKER_FAILED) SHOULD
carry the dispatch generation as `gen=<n>` immediately after the slug, where `<n>`
is the worker's `--dispatch-generation` value (the coord-owned per-slug fence
token). The coord corroborates `gen` against the slug's current task-row
`dispatch_generation` and SKIPS all terminal side effects on a mismatch — so a
stale prior-attempt sentinel can never reap a re-dispatched slug's live worktree
(DESIGN-coord-worktree-lifecycle §3). A sentinel that omits `gen=` is treated as a
pre-migration tokenless signal: trusted only while the slug has not been
re-dispatched. `NEW_TASK` is a wake-only sentinel and carries no token.

## Non-git Projects

Same phases, no branch/commit/push/PR. Reviewer runs both resolved slots
through `review_slot.py` as raw structured reviews (a Codex helper is not
offered here — `codex review` needs a diff base; a Claude helper still is),
records both slots passed (or `single-engine-degraded`), and the finisher
writes `phase=done` directly with a diff summary.

## Failure Modes

- Parse error: skip tick and report parse error.
- Lease fenced (`fleet lease-check` exit 3): skip the tick — no mutation, no
  dispatch — emit a loud stderr diagnostic, and STAY ALIVE (tick reason
  `lease-fenced`). A fence verdict NEVER kills the session
  (DESIGN-coord-lease-false-fence-prevention): the tick's `lease-check
  --reacquire` renews our own expired ACTIVE (rival-free) lease in place at
  the same epoch, so a tick fence means a rival takeover is live, an
  abandoned takeover awaits a successor, or a transient — the next tick
  re-checks. (Non-tick lease-check callers are read-only and may fence on
  a bare expiry; only the tick renews.) Session teardown of a genuinely
  superseded coord belongs to handoff/drain/gc, never to the fence path.
  The duplicate-coord lock-busy self-exit below is a SEPARATE route and
  stays.
- Lock busy: skip tick, no mutation. EXCEPTION (coord-self-exit-when-it-6014):
  if a *different live* coord holds `coordinator.lock` AND the coordinator LEASE
  (`fleet coord-owner`) does not name this session — neither the live active owner
  nor an in-flight handoff successor — (i.e. we are not the project's intended/
  successor coord), this session is a duplicate that would otherwise idle forever. It
  emits a stderr diagnostic and `main()` runs `tmux kill-session -t fleet-<coord_id>`
  to tear down its own session, self-healing to one coord. The lock holder and the
  intended successor coord (the lease names them) never self-exit.
- Prompt too large: leave task ready/todo and report error.
- Worker died without PR: requeue and note.
- CI red: requeue/fix-subagent path.
- Rebase conflict requiring business semantics: raise hand.
- Reviewer tries to bypass terminal review fields: workers state validator
  rejects push/done phase.

## Module Map

- `loop.py` — tick driver and dispatch block emission.
- `parse.py` — read-only Python task parser mirror.
- `dispatch.py` — worker/reviewer prompt builders and inbox writes.
- `finisher.py` — in-tick push + PR (or non-git done note) on `review-done`.
- `workflow_state.py` — atomic `workflow.md` writer.
- `handoff_resume.py` — successor coord resume helper.
- `register_subagent.py` — records the host engine's subagent handle
  (Claude `subagent_id` / Codex `spawn_agent` task path).
- `reviewcfg.py` — resolves alpha/beta slots from the dominant engine +
  helper availability; holds the code-review and plan-review model ids.
- `remote_control.py`, `supervisor.py`, `reaper.py`, `worktree.py` — runtime
  helpers.

## Tests

```bash
python3 -m pytest skills/coordinator/tests/ -q
```

Use targeted tests while editing this skill:

```bash
python3 -m pytest skills/coordinator/tests/test_skill_md.py -q
go test ./internal/tui -run CoordSpawnPrompt
```

## Hook Bindings

This skill is not bound directly to Codex/Claude hooks. It runs as a slash-skill
or via the coordinator spawn prompt. Fleet-guard hooks may cause future ticks by
resuming the coord session, but `loop.py` stays stateless across invocations.

## Non-git review slots

Non-git projects: Claude slots run a raw structured review; Codex slots run
  `codex exec --output-schema` (there is no diff base for `codex review`).
  `review_slot.py` picks this per slot; the reviewer never calls engines
  directly.
