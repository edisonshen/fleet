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

Applies to every dispatched worker.
