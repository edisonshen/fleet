# Fleet standards baseline

This is the canonical content of `~/.fleet/standards.md` that every
operator inherits on first `fleet init`. The bytes shipped on disk live
in [`templates/standards.md`](../templates/standards.md), embedded into
the fleet binary via `//go:embed templates/standards.md` (see
[`embed.go`](../embed.go)). `cmd/fleet/init.go::seedStandardsTemplate`
writes the embedded bytes to `~/.fleet/standards.md` only when the file
does not already exist — operator hand-edits are preserved across
`fleet init --upgrade`.

## What ships

The baseline has three sections, each scoped to a recurring failure
mode the coordinator's worker prompts kept stepping on:

- **Testing** — TDD ratchet, stdlib `testing` only, regression test
  for every bug fix.
- **Code review** — ruthless reviewers, P0/P1 fixes before merge,
  documented "wontfix" rationale for anything left open.
- **Async waits** — background-bash + `task-notification` recipe for
  polling external state changes (PR merges, CI completion, deploy
  finish, file arrival). Codifies the pattern issue
  [#105](https://github.com/edisonshen/fleet/issues/105) calls out so
  workers stop re-discovering foreground-sleep chains and operator-
  ping anti-patterns.

## How workers see it

The coordinator skill inlines the merged standards into every dispatched
worker prompt at `## Standards (the bar — non-negotiable)` (see
[`skills/coordinator/SKILL.md`](../skills/coordinator/SKILL.md) "Worker
prompt template"). The merge is per-H2 with project-level overrides
winning over global, so an operator can refine an individual section in
`~/.fleet/projects/<p>/standards.md` without forking the whole file.

## Why the Async waits section matters

Workers without this guidance drop into one of three failure modes:

1. **Foreground sleep chains** — `sleep 60 && check && sleep 60 && …`
   blows the 5-min prompt cache and serializes wait time into the
   agent's context window.
2. **Operator pings** — agent finishes its piece, then asks "tell me
   when X is done" and stops. Forces the operator to play scheduler.
3. **Fixed-interval cron** — overkill for one-shot waits and burns
   scheduler slots.

The blessed pattern leverages the harness's existing primitive: a
`Bash(run_in_background=true)` call running an `until <check>; do sleep
30; done` loop fires a `<task-notification>` to the agent the moment the
loop exits, no foreground waits, no operator scheduler role, no
prompt-cache thrash.

This was verified live (issue #105 PR Phase 1):

- File-removal poll: `until [ ! -f /tmp/marker ]; do sleep 2; done`
  exited within one poll interval (< 2s wake latency) of the marker
  being removed; harness fired `<task-notification>` on exit.
- `gh pr view` poll: `until gh pr view <N> --json state -q '.state' |
  grep -q MERGED; do sleep 30; done` exited cleanly on the first probe
  against an already-merged PR; harness fired the notification.

The harness's "long leading `sleep` commands are blocked" rule (270s
foreground cap on a single tool call) does not apply here because
background `until` loops do not hold the prompt cache — they sleep,
poll, sleep, poll, and the agent's main turn is free to do other work
or simply idle until the notification lands.

## Operator workflow

```sh
fleet init                       # writes the baseline if no standards.md exists
fleet standards show             # render the merged (global + per-project) view
fleet standards show --global    # only the global file
fleet standards edit             # opens $EDITOR on the per-project file
fleet standards edit --global    # opens $EDITOR on the global file
```

The render path is `internal/standards.Render`; the merge algorithm is
`internal/standards.Merge` (per-H2, project wins, project-only sections
appended). See
[`internal/standards/standards.go`](../internal/standards/standards.go)
for the parser invariants (frontmatter format, fenced-code-block-aware
section split).

## When to deviate

Add a project-level override at `~/.fleet/projects/<p>/standards.md`
when one section needs project-specific tightening (e.g., a security-
sensitive repo wants the Code review section to require two human
reviewers in addition to the bot path). Project-only sections are
appended to the merge so adding a `## Deployment` block in a deploy-
heavy project just shows up in worker prompts with no global change.

Forking the whole baseline by replacing every section is supported but
discouraged — the coordinator's worker prompts assume the three baseline
sections exist, so worker output quality is best when the override is
additive rather than wholesale.
