---
name: testing-fleet-tui
description: Safely exercise Fleet worker CLI state and dashboard in an isolated desktop terminal.
---

# Isolated Fleet CLI/TUI testing

## Devin Secrets Needed
None for local worker updates, peek, and dashboard rendering. Do not dispatch
real agents or access remote projects unless separately authorized.

## Setup
- Build a separate binary: `go build -o /tmp/fleet-test/fleet ./cmd/fleet`.
  Verify `/tmp` first and create the output directory if needed.
- Export a fresh `FLEET_HOME=$(mktemp -d /tmp/fleet-tui-test-XXXXXX)`.
- Export a unique `FLEET_TMUX_SOCKET=fleet-test-tui-$$`. Fleet uses tmux `-S`,
  not `-L`. Never use the default tmux server while testing.
- Also set `HOME="$FLEET_HOME/operator-home"` and create it. Bare fleet
  auto-initializes Claude skills under HOME even when FLEET_HOME is overridden.
  Run the isolated binary's `init` during setup to keep install noise out of
  the recording.
- Seed an empty `$FLEET_HOME/projects/alpha/workers` directory. For worker-only
  dashboard checks, no actual repository/coordinator/Claude process is needed.
  Use `workers update <slug> --project alpha --phase <phase>` for worker data;
  do not hand-edit phase fields that are the subject of the test.
- Persist these environment exports in a temporary shell rcfile and open
  Konsole with `bash --noprofile --rcfile <file>`. Maximize using
  `wmctrl -r :ACTIVE: -b add,maximized_vert,maximized_horz`.

## Runtime checks
- Use bare `fleet` to open the TUI. `fleet status` is a one-shot agent summary,
  not the interactive worker dashboard.
- Worker rows display colored dots, slug, age, and a two-letter status, not
  the full phase name. Use j/k to select a worker, Enter to inspect persisted
  JSON, Escape to close detail, q to exit.
- Fresh spec workers show green/ok; verify shows amber/rn. Refresh CLI state
  before color comparisons: after ten minutes, stale heartbeats are amber/!!
  independently of the phase and can invalidate a color test.
- `fleet peek <slug> --project alpha` exposes phase and completed history.
  Compare a byte copy of state.json after rejected updates to establish
  atomic rejection, not merely an error message.
- Keep all state/sockets/test artifacts under temporary paths. Avoid running
  skill linking/sync or project registration against the operator's home.

## Coordinator reviewer CLI smoke tests
- When fake engines are explicitly authorized, prepend a temporary shim bin
  directory to PATH. Name executable shims `codex` and `claude`; record argv
  and per-process monotonic start/end timestamps to verify overlap, not just
  wall time. Label recordings as fake-engine tests.
- Claude review replies use an outer JSON object whose `result` is a JSON
  string containing `clean` and `findings`. Codex review replies use text
  severity markers such as `[P1]`. See coordinator/tests/test_review_slot.py
  for current response contracts.
- To run generated reviewer commands against current source, use
  `fleet skills link --from <checkout>` only with the isolated HOME and
  FLEET_HOME exports active. This replaces init's frozen embedded copies.
- A full coordinator tick may require a registered checkout and an ancestor
  coordinator lease. Do not assume the mocked test_loop.py fixture alone
  satisfies those runtime guards. If a prompt-builder fallback is authorized,
  call the config loader and build_reviewer_prompt in a Python REPL, execute
  the emitted command, and explicitly leave tick/inbox publication untested.
- In Konsole, Ctrl+= increases the terminal font size for readable recordings.
