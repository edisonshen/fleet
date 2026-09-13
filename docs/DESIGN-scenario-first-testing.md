# DESIGN — scenario-first testing (replace the TDD ladder)

- **Status:** DRAFT — pending operator approval, then dual plan-review per SKILL step 5.
- **Scope:** change *how fleet-dispatched workers test*: from "failing unit test first"
  to "reproduce the requirement's real scenario on the built product, encode it at that
  boundary, verify, ship the evidence". Touches the worker/reviewer/finisher prompts,
  `templates/standards.md`, the coordinator SKILL, the worker phase enum, and the CI
  integration lanes. Does **not** change dispatch, lease, handoff, or TUI behaviour.
- **Priority:** P1. **Depends-on:** none. **PR-base:** `main`.
- **Task plans:** `docs/TASK-PLAN-scenario-testing-pr1-phases.md`,
  `…-pr2-prompts.md`, `…-pr3-gate.md`, `…-pr4-harness.md`.
- **Supersedes:** `standards.md ## Testing` line "TDD required: a failing test on disk
  before the implementation"; the `2a/2b/2c` TDD steps in `build_worker_prompt`; the
  `- [ ] CI green / - [ ] verify locally` PR-body placeholder in `build_finisher_prompt`.
- **Explicitly cut (NOT in v1):** deleting the `tdd-*` phase strings (kept as accepted
  legacy values), any change to `review_slot.py` or the alpha/beta gate, a new subagent
  stage (no `test-author` / `confirm-red` stage), screenshots/video capture, per-project
  opt-out of the scenario contract.

---

## The problem, in plain English

Fleet-built PRs and Devin-built PRs in this repo add about the same *amount* of test
code. They differ in what the tests **prove**. Over the last 60 PRs (#229–#303):

- 5 of 11 Devin code PRs rebuilt the `fleet` binary, stood up the failing situation in an
  isolated `FLEET_HOME` + tmux socket with a fake `claude`, ran the real command or
  keypress, and pasted what happened (pane text, file contents, exit codes) into the PR.
  0 of 37 fleet-built PRs did.
- Devin's tests for user-visible behaviour live at the boundary where the operator would
  see it: `cmd/fleet` and `internal/tui` `//go:build integration` tests driving the built
  binary and real tmux (#292, #298, #294), or a pytest running a real coordinator tick
  against real ledgers (#302). Fleet-built PRs test the same features through
  function-level Go tests with stub seams.
- 9 fleet-built PRs merged with the PR body's test plan still reading
  `- [ ] CI green / - [ ] verify locally` — both boxes unticked. Devin PRs list the test
  names, the exact commands, and prove that any red they left is also red on `main`.

The gap is **shape and evidence**, not effort. Workers spend the effort; the prompts point
it at the wrong target.

## How it works today

```
coord (step 5)          worker                          reviewer            finisher
-------------           ------                          --------            --------
TASK-PLAN test plan:    2a  write the FAILING test      alpha/beta slots    push
 "one line per test     2b  minimal impl → green        fix P0/P1           PR body:
  scenario/input/       2c  refactor                    (never builds or      "- [ ] CI green
  expected"             phase=review-pending             runs fleet)           - [ ] verify locally"
                        (never builds or runs fleet)
```

Three things push a worker toward small-function tests:

1. **"Write the failing test" comes before the worker has seen the product misbehave.**
   With no running `fleet` in front of it, the cheapest failing test is a unit test on the
   function it is about to write. The test then pins the implementation, not the
   requirement.
2. **The task plan names tests, not scenarios.** "scenario / input / expected" never says
   *at which boundary* the test runs or *how the state is stood up*, so the worker picks
   the cheapest level.
3. **Nobody in worker → reviewer → finisher ever runs `fleet`.** The reviewer only shells
   out to `review_slot.py`; the finisher is mechanical. The first time the product runs
   against the change is GitHub CI — and only the default `go test ./...` lane, because the
   integration lanes are `-run '^(TestA|TestB…)$'` allowlists a new test is not on.

The e2e harness Devin left behind (`internal/testutil/coorde2e`: `FakeClaude`,
`SeedProject`, `SeedInFlightWorker`, `Dispatch`, `KillCoordCorpse`, `AgeDeadCoord`,
`WaitLiveCoord`; plus `tmuxfake`, `tmuxtest`) exists, but no prompt, standard, or doc
tells a worker it is there.

## What goes wrong

```
requirement: "WHEN coord ≥40% AND a worker is in flight, THE COORD SHALL NOT swap"

  TDD worker                                   what the operator actually needed
  ----------                                   ---------------------------------
  TestShouldSwap_WorkerInFlight_ReturnsFalse   fleet-guard Stop hook fires at 41% with
    shouldSwap(pct=41, inflight=1) == false      worker x running → NO spawn-fresh-*.json,
                                                 stderr says "hold: worker x in flight",
  passes; shipped.                               and the NEXT tick after x returns DOES swap.
                                               (the bug in #302 was in the caller: the hook
                                                never passed inflight through)
```

A test that never crosses the boundary cannot see the class of bug most of fleet's
production incidents were: wiring, lifecycle, timing, lost keystrokes, stale files.

## The fix: reproduce → encode → verify

Make the **requirement's scenario** the unit of testing, and make the worker *see* the
product fail before it writes anything.

```
coord (step 5)                 worker                                  reviewer         finisher
--------------                 ------                                  --------         --------
TASK-PLAN Scenario Contract    2a REPRODUCE  build fleet; stand up      alpha/beta +     copy the
 S | requirement | stand-up |     each e2e S in a sandbox; run the     contract lens:   worker's
 trigger | observable |          trigger; capture the wrong outcome   every S has a    verification
 level (e2e default) |           → "## Evidence — before"              test at its      block into
 harness                      2b ENCODE      test at S's level that    level, asserting the PR body;
                                 asserts the observed outcome;         the observable;  block if
 reviewed at plan-review         must fail for the "before" reason     re-run after fix empty
                              2c IMPLEMENT + VERIFY  fix; rerun by
                                 hand + test; full ci.yml gates;
                                 "## Evidence — after", "## Gates",
                                 "## Baseline"; gates red → no
                                 review-pending
```

### Lever 1 — the task plan carries a Scenario Contract

Step 5's "Test plan = one line per test" becomes a table the coord fills before promote:

```
## Scenario contract
| S  | Requirement (WHEN … SHALL …)                       | Stand-up                        | Trigger              | Observable outcome                                   | Level | Harness  |
| S1 | WHEN coord ≥40% AND worker x in flight SHALL hold | seeded coord + worker x running | Stop hook at pct=41  | no spawn-fresh-*.json; stderr "hold: worker x …"     | e2e   | coorde2e |
| S2 | WHEN x returns SHALL swap on next tick            | S1, then x → done               | next tick            | spawn-fresh-*.json written; doc lists x's PR         | e2e   | coorde2e |
| S3 | resolve_slots picks alpha/beta (pure)             | —                               | resolve_slots(cfg)   | slot table                                           | unit  | —        |
```

Rules the existing dual plan-review checks:

- Every acceptance requirement has ≥1 row. Every row names an **observable** outcome —
  file on disk, pane text, exit code, PR state, TUI flash — never "function returns X".
- **Level defaults to `e2e`** (built binary / real tmux / real tick). `integ` = package test
  at a real boundary (real FS, real flock, `tmuxfake`). `unit` needs a one-word reason
  (`pure`). A downgrade the reviewer disagrees with is a P1 on the plan.
- Rows sharing a stand-up are one table test; the plan decides grouping, not the worker.
- The contract is the worker's **complete** test assignment. `Tests removed / KEEP` stays.
- Docs-only or pure-refactor tasks write `Scenario contract: none — <reason>`; the
  reviewer confirms the reason.

### Lever 2 — the worker reproduces first, encodes second, verifies last

`build_worker_prompt` steps 2a–2c are replaced (full text in TASK-PLAN pr2):

- **2a REPRODUCE** (`--phase spec-repro`): `go build -o "$FLEET_DEV_BIN" ./cmd/fleet`;
  for each `e2e`/`integ` row, stand it up in a sandbox (`FLEET_HOME=$(mktemp -d)`,
  `FLEET_TMUX_SOCKET=fleet-test-<slug>-$$`, `coorde2e.FakeClaude` or
  `scripts/scenario.sh`), run the trigger, capture the wrong outcome **verbatim** into
  `~/.fleet/projects/<p>/workers/<slug>/verification.md` under `## Evidence — before`.
  Cannot reproduce → `--phase blocked --reason "spec mismatch: S<n> …"`. Never guess.
- **2b ENCODE** (`--phase spec-encode`): one test per row (table-grouped as the plan
  says) at the row's level, asserting the **same** observable the worker just saw. `e2e`
  tests go behind `//go:build integration` in `cmd/fleet` / `internal/tui` (or a
  default-lane test if hermetic and <1s). Run it: it must fail for the reason in
  "Evidence — before". A test that passes before the fix tests nothing.
- **2c IMPLEMENT + VERIFY** (`--phase verify`): fix; re-run each scenario by hand *and*
  via the test; run the **full** gates exactly as `ci.yml` (build, gofmt, golangci-lint,
  `go test -race -count=1 -timeout=5m ./...`, the touched integration lane(s) with
  `-tags=integration`, `pytest skills/ scripts/`, `scripts/lint-test-isolation.sh`). Any
  red the worker believes is pre-existing is re-run on `origin/main` in a worktree and
  recorded under `## Baseline`. Write `## Evidence — after` and `## Gates`. Record
  `fleet workers update … --scenarios-verified N --scenarios-total M --gates-status passed`.
  Red gate → the worker does not flip `review-pending`.

Phases: `tdd-red/green/refactor` are unordered labels validated by `validPhase`. v1 adds
`spec-repro`, `spec-encode`, `verify` and keeps the `tdd-*` strings **accepted** so
in-flight workers, `lifecycle.go`'s `tdd-* → Active` mapping, and the ~90 test fixtures
that use them keep working. The prompt stops emitting `tdd-*`.

### Lever 3 — reviewer and finisher consume the evidence

- **Reviewer** gets a contract lens in both slots' task context: *for each S — is there a
  test at the stated level; does its assertion match the observable outcome (not an
  internal call); does `## Evidence — before` show it would have failed?* Missing or
  level-downgraded S = P1. After any fix commit the reviewer re-runs the touched scenario
  tests and gates before recording terminal status (today it only re-runs the slots).
- **Finisher** replaces the placeholder test plan with `verification.md`'s
  `## Scenario contract`, `## Evidence — before`, `## Evidence — after`, `## Gates`,
  `## Baseline` copied verbatim. Missing file or empty `## Gates` / `## Evidence — after`
  → `--phase blocked --reason "finisher: no verification evidence"` (same shape as the
  existing review-gate reject at `phase=push`).

### Lever 4 — structural gate (fail-closed, like the review gate)

`writeStateLocked` gains one rule at `phase=review-pending` (and re-checked at `push`):

```
validateVerifyGate(s):
  if s.Phase ∉ {review-pending, push}: return nil
  require s.GatesStatus == passed                              # ErrPhaseRequiresGates
  require s.ScenariosTotal >= 0 AND s.ScenariosVerified == s.ScenariosTotal   # ErrPhaseRequiresScenarios
```

`ScenariosTotal == 0` is legal (docs-only task, contract `none`) but `GatesStatus` must
still be `passed`. The worker's `--gates-status` flag is the *claim*; the recorded
`## Gates` block in `verification.md` is the *evidence* the finisher publishes and the
operator can audit. Reset on `phase=starting` (re-dispatch) clears all three fields, like
the review fields.

### Lever 5 — make the e2e path cheap, or workers regress to unit tests

- `docs/TESTING.md` (≤80 lines): "stand up a fleet scenario in 10 lines" — build to
  `$FLEET_DEV_BIN`, sandbox env, fake-claude modes, seed a coord/worker record, run the
  trigger, `tmux -L "$FLEET_TMUX_SOCKET" capture-pane -p`, tear down. Linked from the
  worker prompt and standards.
- `scripts/scenario.sh {up|seed|run|capture|down}` wrapping exactly that, so 2a is one
  command per row and the "before/after" evidence has one format.
- pytest `fleet_sandbox` fixture in `skills/coordinator/tests/conftest.py`: real `fleet`
  binary + real `loop.tick` against a sandbox `FLEET_HOME` (today's coordinator tests are
  almost all pure-unit).
- CI integration lanes select tests by **discovery, not allowlist**:
  `scripts/integration-lane.sh <pkg>` computes the integration-only set as
  `comm -13 <(go test -list . ./pkg) <(go test -tags=integration -list . ./pkg)` — the
  exact derivation the `ci.yml` comment says was done by hand — and runs it. A new
  `//go:build integration` test can no longer silently never run. The per-package `-run`
  scoping (which keeps the already-run default suite out of the lane) is preserved.

## What "done" looks like

The next fleet-built PR for a user-visible change carries: a Scenario Contract in its task
plan; ≥1 `//go:build integration` (or real-tick pytest) test per e2e row; a PR body with
verbatim before/after evidence, the exact gate commands and counts, and a baseline note for
any pre-existing red; and `state.json` showing `gates_status=passed`,
`scenarios_verified == scenarios_total`. Reviewers can tell from the PR alone what the
operator will see.

---

## Implementation detail (for engineers)

### PR split and order (order matters — skill symlink vs binary)

The coordinator skill is symlinked into `~/.claude/skills`, so a merged prompt change is
live on the next dispatch, while a Go change needs a rebuild (CLAUDE.md, #182). A prompt
that emits `--phase spec-repro` against a binary that does not accept it wedges every
worker on its first phase write. Therefore:

| PR | Unit | Surface | Depends |
|----|------|---------|---------|
| PR-1 `scenario-phases` | new phases + `scenarios_*`/`gates_status` fields + flags, **no gate** | Go: `internal/workers`, `cmd/fleet`, `internal/lifecycle`; Py: `loop.py` phase set | none; rebuild + `fleet skills status` before PR-2 dispatches |
| PR-2 `scenario-prompts` | Scenario Contract, worker/reviewer/finisher prompts, standards, SKILL, workflow doc, `docs/TESTING.md` | Py/docs: `dispatch.py`, `templates/standards.md`, `skills/coordinator/SKILL.md`, `docs/COORDINATOR-WORKFLOW.md` | PR-1 merged **and binary rebuilt** |
| PR-3 `scenario-gate` | `validateVerifyGate` fail-closed at `review-pending`/`push`; CI lanes by discovery | Go: `internal/workers`, `cmd/fleet`; `.github/workflows/ci.yml`, `scripts/integration-lane.sh` | PR-2 (prompts must write the fields before the gate enforces them) |
| PR-4 `scenario-harness` | `scripts/scenario.sh`, pytest `fleet_sandbox`, coorde2e additions | `scripts/`, `internal/testutil/coorde2e`, `skills/coordinator/tests/conftest.py` | PR-2 (doc references); independent of PR-3 |

PR-2 is the one that changes the shape of every subsequent fleet PR. PR-3 makes it
un-skippable. PR-4 keeps it cheap. PR-1 exists only so PR-2 can name the phases.

### Phase enum (`internal/workers/workers.go`)

```go
PhaseSpecRepro  Phase = "spec-repro"   // 2a reproduce on the built product
PhaseSpecEncode Phase = "spec-encode"  // 2b encode the observed scenario as a test
PhaseVerify     Phase = "verify"       // 2c implement + full gates + evidence
// PhaseTDDRed / PhaseTDDGreen / PhaseTDDRefactor: retained, accepted, no longer emitted.
```

`validPhase` accepts all; `isTerminalPhase` unchanged. `lifecycle.go`: `spec-*`,
`verify` → `Active` (same bucket as `tdd-*`). `cmd/fleet/workers.go`: help text + `--phase`
flag list gain the three names; comment `// Phase is the worker's current step in its TDD →
review → push pipe` → `reproduce → encode → verify → review → push`. `loop.py
_WORKER_AUTHORED_PHASES` gains the three names.

### State fields + flags (PR-1 accepts, PR-3 enforces)

```go
ScenariosTotal    int    `json:"scenarios_total,omitempty"`
ScenariosVerified int    `json:"scenarios_verified,omitempty"`
GatesStatus       string `json:"gates_status,omitempty"`   // "" | passed | failed
```

`fleet workers update <slug> --scenarios-total M --scenarios-verified N --gates-status
passed|failed`. CLI rejects `gates_status ∉ {"", passed, failed}`, negative counts, and
`verified > total`. The `phase=starting` reset block clears all three (same block that
clears review fields). `ReadState` of an old record without the fields is unchanged
(omitempty, zero values).

### Verification doc (worker-owned, finisher-read)

Path: `~/.fleet/projects/<project>/workers/<slug>/verification.md` — beside `state.json`,
so the finisher's existing `workers_dir` resolution finds it and re-dispatch cleanup
(`workers.Archive`) archives it with the state. Not the `subagent-wip` phase log (that is
a free-form narrative the coord reads on BLOCKED). Sections, in order, all required
(empty allowed only where noted):

```
## Scenario contract        # copied from the task plan
## Evidence — before        # per S: command(s) run, verbatim captured output
## Evidence — after         # per S: same commands, verbatim output, PASS/FAIL
## Gates                    # each ci.yml gate: exact command, result line (e.g. "ok  … 312 tests")
## Baseline                 # empty or: per pre-existing red, the same command on origin/main + result
## Unit tests               # rows at level unit, test names
```

### Finisher PR body

`## Test plan` placeholder → the five sections above, verbatim, under `## Verification`.
The finisher does not summarise or reformat; it `cat`s. Missing file, or `## Gates` /
`## Evidence — after` empty → `phase=blocked` with reason `finisher: no verification
evidence (<path>)`.

### Reviewer lens

Appended to the `--task-context` (non-git) and to the prompt's step 2 (git): the Scenario
Contract table plus the lens text. Findings about missing/downgraded scenarios are
ordinary P1s in the slot loop — no new gate field. After a fix commit the reviewer runs
the touched scenario tests + full gates before the terminal write and appends a
`## Review re-verification` section to `verification.md`.

### Standards `## Testing` (replacement text, verbatim)

```
## Testing

- Scenario-first, not test-first. Reproduce the requirement's real scenario on the built
  product before writing any test; the test encodes what you observed.
- Test at the boundary where the operator would see it: built binary, real tmux
  (isolated FLEET_TMUX_SOCKET), real files under an isolated FLEET_HOME, real tick.
  Unit tests only for pure logic the scenario cannot reach; never a mock of a boundary
  we can run for real. (Harness: internal/testutil/coorde2e, tmuxfake, tmuxtest;
  docs/TESTING.md.)
- Assert the observable outcome (file content, pane text, exit code, PR state, TUI
  flash) — never that an internal function was called.
- One table test per shared stand-up; one row per scenario. New case = new row.
- Every bug fix ships the scenario that reproduces it; the PR shows it failing before
  and passing after. Pre-existing red is proven on main, not asserted.
- Delete tests the new scenario subsumes; the task plan lists Tests removed / KEEP.
- Test shape over volume: one scenario test through the real arc beats N function tests
  re-pinning it through mocks. Test LOC that dwarfs the arc it covers is a signal to
  consolidate.
- stdlib `testing` / pytest only. Every test names the scenario it pins (S<n>) or the
  bug it catches.
```

Removed: "TDD required", "failing test on disk before implementation", the 1.5× LOC
budget as a rule (kept as a signal).

### CI lanes by discovery (`scripts/integration-lane.sh`)

```sh
#!/usr/bin/env bash
# usage: integration-lane.sh <pkg> [extra go test flags]
# Runs exactly the tests present in the integration build and absent from the
# default build — the -run derivation ci.yml previously hand-maintained.
set -euo pipefail
pkg=$1; shift
only=$(comm -13 <(go test -list . "$pkg" | grep '^Test' | sort) \
                <(go test -tags=integration -list . "$pkg" | grep '^Test' | sort) \
       | paste -sd'|' -)
[ -n "$only" ] || { echo "no integration-only tests in $pkg"; exit 0; }
exec go test -tags=integration -count=1 -timeout=5m -run "^(${only})\$" "$@" "$pkg"
```

`ci.yml` replaces the three hardcoded `-run` lists with one call per package
(`FLEET_STANDBY_TIMEOUT=3s` preserved). Cost: two `-list` compiles per package, cached by
the Go build cache after the first. Tested by `scripts/tests/test_integration_lane.sh`
against a fixture module with one default and one tagged test.

### Non-git projects

Same phases, same `verification.md`, same fields. "e2e" for a non-git project means the
project's own run command (documented in the project's standards); the coord's contract
row names it. Finisher (non-git) writes the verification block into the task's done note
instead of a PR body.

### Rollout

1. PR-1 merges → operator rebuilds (`go install ./cmd/fleet`) → `fleet skills status`.
2. PR-2 merges → live on next dispatch via symlink. First dogfood task: a small real bug
   with one e2e row, to shake out `docs/TESTING.md`.
3. PR-3 merges → operator rebuilds; from here a worker with red gates cannot hand off.
4. PR-4 merges; `docs/TESTING.md` updated to prefer `scripts/scenario.sh`.

Regression signal: `gates_status=passed` present on every `state.json` reaching
`review-pending`; PR bodies contain `## Evidence — after`; zero PRs merged with
`- [ ] verify locally`.

## Non-goals

- Removing `tdd-*` strings or migrating in-flight records.
- Changing review slots, models, or the alpha/beta gate.
- Screenshots / asciinema capture (pane text is the v1 evidence).
- A separate test-author subagent or a mandatory RED-before-code step.
- Per-project opt-out; the contract `none — <reason>` row is the escape hatch, reviewed.
