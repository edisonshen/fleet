# Postmortem: orphan tmux session leak → Mac OOM crash

**Incident date:** 2026-05-13 (detection 2026-05-14)
**Severity:** P0 — operator's machine OOM-crashed and force-rebooted twice
**Status:** Fix in flight (PRs #N1, #N2, #N3 — backfill once landed)
**Author:** Coordinator session, 2026-05-14
**Reviewers:** TBD

## TL;DR

`tmux.HasSession()` is ambiguous — it returns `false` for both "session missing" AND "probe failed" (transport error). Six call sites in the handoff replacement-cleanup paths treated a flaky `false` as "session is gone" and deleted the agent's `state.json`. The tmux session, with its full `claude` (+ child `codex`/`sh -c`) process, kept running orphaned. Over ~12 hours after the operator's last reboot, 68 such orphans accumulated, each holding ~150MB+ resident — together exceeding system RAM and forcing OOM reboots.

## Timeline

| When (UTC) | Event |
|---|---|
| 2026-05-12 → 2026-05-13 | Fleet running normally across 9 projects, fleet-guard auto-handoffs firing on the 50%/70% context thresholds (≈ one handoff every ~10 min/coord under load). |
| 2026-05-13 (yesterday) | Operator's Mac OOM-crashes and reboots. Twice. |
| 2026-05-14 ~15:00 UTC | Operator opens fresh Claude Code session in `~/projects`. Asks "stop all the agent working." |
| 2026-05-14 ~15:05 UTC | `fleet status` reports 9 live agents. `tmux ls \| grep fleet-` reports **76** sessions. After `fleet rm` on the 9 known agents, **67 orphan tmux sessions** remain with no matching `state.json` in either live (`~/.fleet/agents/`) or archive (`~/.fleet/agents/archive/`). |
| 2026-05-14 ~15:10 UTC | Manual `tmux kill-session` sweep on the 67 orphans. 14 stray `fleet-coord-*` worker processes still detached from tmux; SIGKILL'd. Total memory pressure relieved. |
| 2026-05-14 ~15:15 UTC | Codex `exec` investigation identifies root cause: ambiguous `tmux.HasSession()` returning `false` on probe errors → unpaired `os.Remove` of state.json without `tmux.Kill`. Six leak sites named. |
| 2026-05-14 ~15:20 UTC | Subagent #1 dispatched: immediate leak plug + `fleet maintenance prune-orphan-tmux` sweeper. |
| 2026-05-14 ~15:25 UTC | Subagent #2 dispatched: atomic coord-swap invariant on every replacement path + worker/subagent post-task cleanup gate. |
| 2026-05-14 ~15:30 UTC | Subagent #3 dispatched: hard session-count limit + visibility guardrails. |

## Root cause

`tmux.HasSession()` (internal/tmux/tmux.go:118) returns `bool`:
- `true` — session exists.
- `false` — session does not exist OR probe failed (tmux server hiccup, socket EAGAIN, transient error).

The handoff replacement-cleanup paths used `HasSession` as a precondition for "the new tmux session crashed at startup; clean up its state.json":

```go
if !tmux.HasSession(newRec.TmuxSession) {
    if path, perr := state.AgentPath(newRec.ID); perr == nil {
        _ = os.Remove(path)
    }
    // …error return…
}
```

When the probe was flaky (a real condition under load — fleet-guard auto-handoffs hammer tmux with multiple `tmux ls` / `has-session` calls per second), `HasSession` returned `false` even though the session was very much alive. The code deleted the agent record. The tmux session kept running. No archive entry was written, so the orphan was invisible to `fleet status`.

Leak sites:
- `internal/handoffop/handoffop.go:102`
- `internal/handoffop/handoffop.go:322`
- `internal/handoffop/handoffop.go:452`
- `cmd/fleet/handoff.go:329`
- `cmd/fleet/handoff.go:558`
- `cmd/fleet/handoff.go:670`

A tristate primitive (`tmux.SessionAlive(session) (alive bool, err error)`) already exists in the codebase — it was used correctly in *some* paths and ignored in others. The bug was inconsistent use of the safe API.

### Why it accumulated fast

Across 9 active project coordinators, fleet-guard's 50%/70% auto-handoff fired frequently. Each handoff that hit a probe-flake left one orphan tmux session. At a leak rate of ~5/hour, 12 hours of post-reboot operation produced ~60 orphans — enough to consume system RAM on a 16-GB Mac (each `claude` process ~150–300 MB resident; 68 orphans × ~200 MB ≈ 13 GB before counting codex children).

### Why it was invisible

`fleet status` reads from `~/.fleet/agents/*.json`. With state.json deleted, orphan sessions never appeared in any fleet UI. The first signal was the OS killing things.

## What went well

- **Codex `exec` nailed the root cause in one pass** with a structured prompt — under 5 minutes from "investigate this" to file:line attribution across six sites.
- **Subagent dispatch contract worked.** Three parallel subagents with clear, non-overlapping scopes were running within 30 minutes of detection.
- **Spawn rollback was already correct.** `internal/spawn/spawn.go:725-731` kills tmux when state.json write fails — so the *creation* side wasn't the leak.
- **`fleet rm` was already correct.** Used the safer `SessionAlive`/`Kill` flow.

## What went wrong

- **Two APIs for the same question.** `HasSession` (ambiguous bool) and `SessionAlive` (tristate) coexisted; reviewers didn't catch the unsafe one slipping back into new code.
- **No visibility on orphan accumulation.** No metric, no log, no banner. Required an OS-level OOM crash to surface.
- **No upper bound on session count.** Fleet would happily accumulate sessions until RAM ran out.
- **No post-task tmux cleanup gate for workers.** Workers that finished their task left their tmux session running indefinitely, contributing to the count.
- **No CI test for the atomicity invariant.** "After handoff, exactly one tmux session per active agent" was an unwritten contract.

## Resolution

Three PRs:

**PR #N1 — Immediate leak plug (Subagent #1).**
- Replace `tmux.HasSession()` with `tmux.SessionAlive()` (tristate) at all six leak sites.
- Extract a `internal/handoffop/replacement_cleanup.go` helper centralizing the safe kill-then-remove pattern so future code can't bypass it.
- Add `fleet maintenance prune-orphan-tmux` subcommand (dry-run default, `--kill` to actually reap) — operator-triggered safety net for catching survivors.
- Regression tests pinning the leak sites.

**PR #N2 — Atomic coord swap + worker cleanup gate (Subagent #2).**
- Audit every coord-replacement code path (manual handoff, fleet-guard auto-handoff, dead-coord resume, `fleet rm`). Enforce the invariant: at any observable moment, either OLD or NEW is the live coord for that project — never both, never neither.
- Centralize the swap orchestrator. Roll back new spawn on any failure; refuse to archive old until kill is confirmed.
- Worker/subagent post-task cleanup gate: `tmux.Kill` + `SessionAlive`-confirmed-dead + `state.json` archive is a hard precondition for flipping a task to a terminal status. If any of those steps fails, the task stays in its prior status. `--keep-session` escape hatch for debug.
- Regression tests for both pieces.

**PR #N3 — Session-count limit + visibility (Subagent #3).**
- Hard cap: refuse to spawn (`fleet dispatch`, `fleet handoff`) when total `fleet-*` tmux session count ≥ `FLEET_MAX_SESSIONS` (default **30**, env-overridable). Error message points to `fleet maintenance prune-orphan-tmux` and `fleet rm <id>`.
- `fleet status` warning banner when count ≥ 80% of cap.
- Optionally: surface tmux count + orphan count in the TUI dashboard footer so operators see growth before it bites.

## Lessons learned

1. **Tristate primitives belong in the API surface.** Any function that probes a remote resource should return `(value, err)`, never bare `bool`. The bool form invites "false means not-found" assumptions that collapse silently under probe error. Audit other `Has*` / `Exists*` shapes in the codebase.
2. **Every resource-create needs a paired resource-destroy gate.** Spawn writes state.json + creates tmux session. Any code path that removes one must remove the other or have a *proven* precondition that the other is already gone.
3. **Accumulation without visibility is a slow-motion outage.** A hard cap with a clear error is much better than slow OOM. The cap doesn't fix the leak — it surfaces it.
4. **Codex `exec` is the right tool for root-cause investigation.** Faster and more accurate than `codex review` for narrow questions where you can describe the symptom precisely.
5. **Worker tmux sessions are real state.** Treating them as fire-and-forget creates the same class of leak workers introduced. Cleanup must be a completion gate, not a follow-up.

## Action items

| # | Item | Owner | PR |
|---|---|---|---|
| 1 | Replace `HasSession` with `SessionAlive` at all leak sites | Subagent #1 | #N1 |
| 2 | Add `fleet maintenance prune-orphan-tmux` sweeper | Subagent #1 | #N1 |
| 3 | Atomic coord-swap invariant on every replacement path | Subagent #2 | #N2 |
| 4 | Worker post-task tmux cleanup as completion gate | Subagent #2 | #N2 |
| 5 | Hard `FLEET_MAX_SESSIONS` cap (default 30) | Subagent #3 | #N3 |
| 6 | `fleet status` warning banner at 80% of cap | Subagent #3 | #N3 |
| 7 | Audit remaining `Has*` / `Exists*` APIs for bool-vs-tristate hazard | follow-up | — |
| 8 | Add lint rule or codereview-bot pattern flagging `os.Remove(state.AgentPath(...))` not paired with `tmux.Kill` | follow-up | — |
