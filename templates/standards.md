---
schema: v1
---

# Standards

## Testing

- TDD required: a failing test on disk before the implementation.
- Tests use stdlib `testing` only — no testify, no ginkgo.
- All bug fixes carry a regression test that fails on the parent commit.
- Integration tests preferred over heavy mocking when feasible.

## Code review

- Reviewers must be ruthless; no rubber stamps.
- Run /review and /codex review; fix every P0/P1 finding before merging.
- Both reviewers report clean OR every flagged item carries a documented "wontfix" rationale.

## Async waits (polling external state)

When a task depends on an external state change you do not control (PR
merging, CI completing, deploy finishing, file appearing), do **not** use
`sleep N && check` chains, do **not** ask the operator to ping you, and
do **not** fall back to fixed-interval foreground polling that burns the
prompt cache.

Use the harness's background-bash + task-notification path:

```bash
# Bash tool, run_in_background: true
until <single check that returns truthy when done>; do sleep 30; done
echo "DONE at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

The harness fires a `<task-notification>` when the loop exits, so the
agent resumes the moment the condition flips — no foreground waits, no
operator hand-holding, no cache thrash.

- Choose poll interval to match the event's real cadence (30s for PR
  merges, 5–10s for fast CI, 60s+ for slow deploys).
- One `until` loop per condition. If you need to react to multiple
  independent signals, dispatch parallel background bashes — each
  notifies independently.
- Set a hard `timeout` on the Bash call (default 10 min is usually
  enough for one wait; pick something proportional to the slowest
  realistic completion time).
- Never sleep > 270s in a single foreground tool call — past 5 min you
  blow the prompt cache. Background `until` loops are exempt because
  they don't hold the cache.

Common recipes:

```bash
# Wait for a PR to merge (poll every 30s, 1h cap):
until gh pr view <N> --repo <owner/repo> --json state -q '.state' \
    | grep -q MERGED; do sleep 30; done
echo "MERGED at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

```bash
# Wait for CI checks to all pass (poll every 30s):
until gh pr checks <N> --repo <owner/repo> --json bucket \
    -q '[.[] | .bucket] | all(. == "pass")' | grep -q true; do
  sleep 30
done
echo "CI GREEN at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

### PR shepherding

The recipes above wait for a single terminal event (`MERGED`, `CI GREEN`).
A PR you own does **not** stop at a single terminal event — between open
and merge it can flip BEHIND (a sibling PR landed), DIRTY (conflict),
CI-red (transient or real), or CHANGES_REQUESTED (review feedback). If
you only wake on `state==MERGED` you sleep through every actionable
state and the PR rots.

**Shepherd the PR — don't watch it.** One `until` loop per open PR,
backgrounded, waking on **any actionable state**, then re-spawning after
each action so the PR is always under an active watch until it merges or
closes.

**Polling pattern (one loop per PR being tracked):**

```bash
# Bash tool, run_in_background: true, timeout: harness max (10 min)
until gh pr view <N> --repo <owner/repo> --json \
        state,mergeStateStatus,statusCheckRollup,reviewDecision \
        -q '
          .state != "OPEN"
          or .mergeStateStatus == "BEHIND"
          or .mergeStateStatus == "DIRTY"
          or ([.statusCheckRollup[]?.conclusion] | any(. == "FAILURE"))
          or .reviewDecision == "CHANGES_REQUESTED"
        ' | grep -q true; do sleep 30; done
echo "WAKE at $(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

On wake, fetch the same JSON once and dispatch by which predicate fired.
After acting (and pushing if applicable), **re-spawn the same poll** —
the PR is back to "waiting on next change" until it reaches a terminal
state. Re-spawn after each timeout too: the harness 10-min cap is a
safety net, not a "PR is done" signal.

**Per-state action matrix:**

| Wake reason | Action | Cap | Escalate to operator on |
|-------------|--------|-----|-------------------------|
| TERMINAL (MERGED) | Update task to done; dispatch any gated follow-ups | — | — |
| TERMINAL (CLOSED-without-merge) | Update task; surface to operator (was the work abandoned?) | — | always |
| BEHIND | Dispatch rebase-shepherd on isolated worktree; `--force-with-lease` push | — | non-trivial conflict during rebase |
| DIRTY (conflict) | Same shepherd — resolve markdown conflicts as additive (keep both sides); abort + escalate on substantive Go-code conflicts | — | substantive merge logic |
| CI fail | Dispatch fix-subagent against same branch with failure log | 3 attempts | after cap, or if root cause is unclear |
| CHANGES_REQUESTED | Analyze comments; address straightforward (typo / style) inline; raise-hand for scope-change | — | substantive design feedback |

**Worktree safety — do not skip.** Always rebase or fix CI in an
isolated git worktree, not the shared checkout:

```bash
git worktree add /tmp/fleet-<task>-<pr> <branch>
# … rebase or fix in /tmp/fleet-<task>-<pr> …
git worktree remove /tmp/fleet-<task>-<pr>
```

Concurrent agents on the shared checkout cause cwd flips and stash
interference (observed live in fleet's own dogfooding). One worktree per
shepherd action, removed when done.

**Re-spawn loop:**

- After a successful action, re-spawn the active poll for that PR.
- If the action returns BLOCKED (operator escalation), do NOT re-spawn
  — the operator unblocks first.
- If the loop times out (harness 10-min cap) and the PR is still open,
  re-spawn the same poll. The cap is a watchdog, not a state change.

**One loop per PR.** Do not write a custom diff-detect script that
multiplexes N PRs through a single foreground watcher — each PR gets
its own background `until` loop, the harness fires `<task-notification>`
per loop independently, and the per-PR action is decoupled from the
others. A custom multi-PR watcher serializes wake latency and loses the
harness's per-loop notification primitive.

Applies to every dispatched worker.
