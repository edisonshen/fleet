---
schema: v1
---

# Standards

## Implementation

- **Delete-and-rewrite over patch-on-patch.** When a change touches existing logic, rewrite the affected function/unit cleanly — delete the old, write the new — rather than bolting another branch onto the old shape. Layered patches breed dead branches and unreadable archaeology. (A small, clean addition to an already-clean function is a clean write, not a patch — the target is accumulated cruft, not every edit.)
- **Delete orphaned/subsumed tests.** When a change makes an existing test redundant or obsolete, delete it and fold its coverage into the new (usually table-driven) test — never leave dead tests beside new ones. The task plan must list "Tests removed" (with the reason) and "KEEP (retained behavior)" so the implementer neither over- nor under-deletes. Dead tests are a leak.

## Testing

- TDD required: a failing test on disk before the implementation.
- Tests use stdlib `testing` only — no testify, no ginkgo.
- All bug fixes carry a regression test that fails on the parent commit.
- Integration tests preferred over heavy mocking when feasible.
- Test SHAPE matters as much as coverage: a matrix of near-identical single-scenario functions is a review liability. Consolidate scenarios that share a driver into ONE table test (Go table-driven / `pytest.mark.parametrize`), one row per case, and factor setup into a per-package builder/harness. New case = new row, not new function.
- Test one CONTRACT, not the implementation: no assertion a legitimate refactor would break. One integration test at a real boundary beats N unit tests re-pinning the same behavior through mocks.
- Budget: test LOC should stay within ~1.5x the production LOC it covers unless a specific case justifies more. If a PR's tests dwarf its logic, that's a signal to consolidate, not a badge.
- Every test must name the bug it catches — in the function/row name or a one-line comment. A test whose failure wouldn't tell you what broke is noise; delete or rename it.

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

## Post-completion discipline

Agent-tool subagents have no kill signal from the parent. A subagent that
emits its §7 return contract and KEEPS working (opens bonus PRs, amends
the branch, expands scope) is a CLAUDE.md §8 violation — and the parent
agent has no way to stop it except after the fact.

PR #124 (closed) was the motivating case: a README-rewrite subagent
finished an 8-bullet task, returned the §7 contract, then opened a
separate PR adding bullet #9 it "noticed in the code." The operator
caught the drift only because the bonus PR appeared in `gh pr list`.

The contract for every dispatched worker:

> After you emit the §7 return contract, your work for this dispatch is
> COMPLETE. You may NOT:
>
>   - open additional PRs
>   - file additional bugs / tasks unless explicitly invited
>   - amend, push, or rebase any branch
>   - take ANY further action on this codebase
>
> If during the work you noticed valid follow-up ideas, do NOT do that
> work yourself. The §7 list is the closed scope. File a P3 ticket via
> `fleet tasks add --priority P3 --slug <short> "<one-liner>"` so the
> operator triages it. Bonus content violates CLAUDE.md §8.

Coordinator enforcement (defense in depth):

- Worker prompts (skills/coordinator/dispatch.py) carry this contract
  verbatim in the "Post-completion contract" section.
- On phase=done the coord writes a subagent archive receipt at
  `~/.fleet/projects/<project>/subagents/<slug>.json` with
  `archived_at` set.
- Every tick the coord re-probes each archived subagent's worker
  branch via `gh pr list --head <branch>` and appends any PR opened
  AFTER `archived_at` to the record's `post_archive_artifacts` list.
- The TUI dashboard renders "⚠ post-archive activity" on the project
  row when any archived subagent has non-empty
  `post_archive_artifacts`. The operator decides per-flag whether to
  close the bonus PR or accept it.

The receipt + audit is informational; the dispatch-prompt language is
load-bearing. A subagent that ignores the contract still gets flagged,
but flagging is a backstop — the contract language is what keeps the
subagent inside its lane in the first place.
