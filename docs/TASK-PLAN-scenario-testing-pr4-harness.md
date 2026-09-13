# TASK PLAN — scenario-testing PR-4: scenario harness (`scenario.sh`, pytest sandbox, coorde2e)

- **Task:** `scenario-testing-pr4-harness` · **Priority:** P2 · **PR-base:** `main`
- **Design:** `docs/DESIGN-scenario-first-testing.md` §Lever 5
- **Depends-on:** PR-2 (docs/TESTING.md exists to update). Independent of PR-3.
- **Status:** DRAFT — pending dual plan-review + operator promote.

---

## TL;DR

Make step 2a a one-liner so workers do not fall back to unit tests when the e2e path
feels expensive.

```
before (PR-2):  worker hand-writes ~15 lines of sandbox setup per scenario, Go-only harness
after:          scripts/scenario.sh up | seed-coord | seed-worker | run -- <cmd> | capture | down
                pytest fixture fleet_sandbox → (fleet_bin, fleet_home, tick) for coordinator scenarios
                coorde2e: CapturePane, WriteHookEvent, SetContextPct for hook-driven scenarios
```

## Deliverables

| # | Deliverable | Surface |
|---|-------------|---------|
| D1 | `scripts/scenario.sh` — subcommands: `up` (build to `$FLEET_DEV_BIN` if unset, `mktemp -d` FLEET_HOME, `FLEET_TMUX_SOCKET=fleet-test-<name>-$$`, fake claude on PATH, prints `export` lines), `seed-coord [--pct N] [--dead]`, `seed-worker <slug> --phase <p>`, `run -- <cmd…>` (runs under the sandbox env, prints exit code + stdout/stderr fenced for `verification.md`), `capture [<session>]` (`tmux -L … capture-pane -p`, fenced), `down` (kill sessions on the socket, `rm -rf` FLEET_HOME, remove socket). Fake-claude modes via `FLEET_FAKE_CLAUDE_MODE=ok\|exit\|crash-once\|hang` | `scripts/scenario.sh` |
| D2 | Fake claude as a standalone script `scripts/fake-claude.sh` sharing the mode contract with `coorde2e.FakeClaude` (Go test keeps its own copy; a parity test asserts both honour the same mode strings) | `scripts/fake-claude.sh`, `internal/testutil/coorde2e` |
| D3 | `coorde2e` additions: `CapturePane(t, session) string`, `WriteHookEvent(t, project, pct int, event string)` (writes what fleet-guard's Stop hook would), `SeedCoord(t, project, opts)` (live/dead, pct) | `internal/testutil/coorde2e/coorde2e.go` |
| D4 | pytest fixture `fleet_sandbox` in `skills/coordinator/tests/conftest.py`: builds (or reuses `$FLEET_DEV_BIN`) the binary once per session, yields `(bin, home, run(cmd), tick())` where `tick()` calls `loop.tick` against `home`; auto-`down` | `skills/coordinator/tests/conftest.py` |
| D5 | `docs/TESTING.md` rewritten around `scenario.sh` (the 10-line example becomes 4); `## Evidence` format documented as "paste `run`/`capture` output as-is" | `docs/TESTING.md` |
| D6 | Worker prompt 2a names `scripts/scenario.sh` first, `coorde2e` second | `skills/coordinator/dispatch.py` |
| D7 | `scripts/lint-test-isolation.sh` recognises `scenario.sh up` as satisfying the FLEET_TMUX_SOCKET isolation rule for shell-driven tests under `scripts/tests/` | `scripts/lint-test-isolation.sh` |

## Scenario contract

| S | Requirement | Stand-up | Trigger | Observable outcome | Level | Harness |
|---|-------------|----------|---------|--------------------|-------|---------|
| S1 | WHEN `up` runs THE sandbox SHALL be isolated: nothing touches `~/.fleet` or the default tmux server | record `ls ~/.fleet` and `tmux ls` (default socket) before | `eval "$(scenario.sh up)"; scenario.sh seed-coord; scenario.sh run -- fleet status` | `~/.fleet` listing identical; default `tmux ls` identical; `$FLEET_HOME` contains `agents/`, `projects/` | e2e | bash test `scripts/tests/test_scenario_sh.sh` |
| S2 | WHEN `run` executes a fleet command THE output SHALL be fenced with exit code, suitable to paste | S1 | `run -- fleet workers show nope` | stdout begins ```` ```text\n$ fleet workers show nope\nexit=1 ```` and ends with a fence | e2e | same |
| S3 | WHEN `seed-coord --pct 41` and a worker is in flight THE hook trigger SHALL be reproducible in one line (the #302 scenario) | S1 + `seed-worker x --phase verify` | `run -- python3 skills/fleet-guard/main.py` with a `Stop` hook payload on stdin (`hook_event_name=Stop`, pct 41) | `capture` / `run` output shows `hold: worker x in flight`; no `spawn-fresh-*.json` under `$FLEET_HOME` | e2e | same — this row doubles as the doc's worked example |
| S4 | WHEN `down` runs THE sandbox SHALL leave no debris | S3 | `down` | `$FLEET_HOME` gone; `tmux -L $FLEET_TMUX_SOCKET ls` fails "no server"; `/tmp/fleet-test-*` count unchanged from before `up` (the CI leak sentinel stays green) | e2e | same |
| S5 | WHEN a pytest uses `fleet_sandbox` THE real binary and a real tick SHALL run against the sandbox and clean up | conftest fixture | a smoke test: seed a project + promoted task, `tick()` | `tasks.md` in the sandbox shows the task dispatched (or `workers/<slug>/state.json` exists); after the test `home` is removed | e2e | pytest `test_fleet_sandbox_smoke.py` |
| S6 | WHEN both fake claudes get the same mode THE behaviour SHALL match | `FLEET_FAKE_CLAUDE_MODE=exit` for Go `FakeClaude` and `scripts/fake-claude.sh` | run each once | same exit code and same first stdout line, per mode (table over `ok`, `exit`, `crash-once`) | integ | Go parity test in `coorde2e` (extends existing `parity_test.go` pattern from `tmuxfake`) |
| S7 | WHEN `CapturePane`/`WriteHookEvent`/`SeedCoord` are used by an existing e2e test THE test SHALL shrink without changing its assertions | `cmd/fleet/dead_coord_recovery_e2e_integration_test.go` | refactor one test to the helpers | integration lane still passes; diff shows only setup lines removed | integ | integration lane |

## Tests removed / KEEP

- **Tests removed:** inline sandbox setup blocks in the one refactored e2e test (S7) — replaced by helper calls, assertions untouched.
- **KEEP:** all of `coorde2e`'s existing callers; `tmuxfake`/`tmuxtest` unchanged.

## Acceptance

1. Full gates green including the integration lanes and `scripts/tests/*.sh`.
2. S1–S7 pass; the PR body's `## Evidence — after` includes S3's fenced output verbatim
   (it becomes the worked example in `docs/TESTING.md`).
3. `docs/TESTING.md` ≤80 lines, example uses `scenario.sh`.
4. Post-test leak check in CI (`/tmp/fleet-test-*` sentinel) unchanged and green.

## Non-goals

- Screenshot/asciinema capture.
- Replacing `coorde2e`'s Go fake claude with the shell one (both stay; parity-tested).
- Windows/macOS-specific tmux socket handling beyond what `tmuxtest` already does.
