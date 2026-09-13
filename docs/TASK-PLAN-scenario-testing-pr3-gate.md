# TASK PLAN — scenario-testing PR-3: fail-closed verify gate + CI lanes by discovery

- **Task:** `scenario-testing-pr3-gate` · **Priority:** P1 · **PR-base:** `main`
- **Design:** `docs/DESIGN-scenario-first-testing.md` §Lever 4, §Lever 5 (CI lanes),
  §"CI lanes by discovery"
- **Depends-on:** PR-2 merged and dogfooded once (S8) — the gate rejects any worker that
  does not record the fields, so the prompts must already write them. Operator rebuilds
  after merge.
- **Status:** DRAFT — pending dual plan-review + operator promote.

---

## TL;DR

Two enforcement pieces, both "you cannot forget it":

```
1. validateVerifyGate  — phase=review-pending (and push) is REJECTED unless
                         gates_status == passed AND scenarios_verified == scenarios_total
                         (mirrors validateReviewGate: fail-closed, error names the field)

2. integration lanes   — ci.yml stops hand-listing -run '^(TestA|TestB…)$' per package;
                         scripts/integration-lane.sh <pkg> derives the integration-only set
                         with `comm -13 <(go test -list) <(go test -tags=integration -list)`
                         and runs exactly that. A new //go:build integration test can no
                         longer silently never run.
```

## Deliverables

| # | Deliverable | Surface |
|---|-------------|---------|
| D1 | `validateVerifyGate(s *State) error` called from `writeStateLocked` for `Phase ∈ {review-pending, push}`; errors `ErrPhaseRequiresGates` (`"phase=%s requires gates_status=passed (got %q)"`), `ErrPhaseRequiresScenarios` (`"… scenarios_verified=%d != scenarios_total=%d"`) | `internal/workers/workers.go` |
| D2 | CLI surfaces the gate error verbatim on `fleet workers update --phase review-pending` (same path the review-gate reject already takes at `push`) | `cmd/fleet/workers.go` |
| D3 | Worker prompt: one line after 2c — "if the review-pending write is rejected by the verify gate, your gates or counts are wrong; fix and re-record — never `--phase blocked` to bypass" | `skills/coordinator/dispatch.py` |
| D4 | `scripts/integration-lane.sh <pkg> [go test flags]` per the design; `set -euo pipefail`; exit 0 with a message when the package has no integration-only tests | `scripts/integration-lane.sh` |
| D5 | `ci.yml`: the three lane steps call `scripts/integration-lane.sh ./internal/spawn`, `./cmd/fleet`, `./internal/tui` with `FLEET_STANDBY_TIMEOUT=3s`; the "keep these -run lists in sync" comment is rewritten to describe discovery; a fourth step `integration-lane.sh ./internal/handoffop` is **not** added (zero integration-only tests; the script would exit 0 anyway — document it) | `.github/workflows/ci.yml` |
| D6 | `scripts/tests/test_integration_lane.sh` (wired into CI beside `test_lint_test_isolation.sh`): fixture Go module under `scripts/tests/fixtures/lane/` with `a_test.go` (default, `TestDefault_X`) and `b_integration_test.go` (`//go:build integration`, `TestOnlyTagged_Y`) | `scripts/tests/` |
| D7 | PR-2's `docs/TESTING.md` + worker prompt 2b: drop "add its name to the -run list"; say "the lane discovers `//go:build integration` tests automatically" | `docs/TESTING.md`, `dispatch.py` |

## Scenario contract

| S | Requirement | Stand-up | Trigger | Observable outcome | Level | Harness |
|---|-------------|----------|---------|--------------------|-------|---------|
| S1 | WHEN a worker flips review-pending without recording gates THE CLI SHALL reject and leave state.json untouched | built binary, sandbox `FLEET_HOME`, worker at `verify`, no fields | `fleet workers update w --phase review-pending` | non-zero exit; stderr contains `requires gates_status=passed`; `state.json.phase` still `verify` | e2e | built binary, sandbox |
| S2 | WHEN gates passed but counts disagree THE CLI SHALL reject naming both counts | S1 + `--gates-status passed --scenarios-total 3 --scenarios-verified 2` | `--phase review-pending` | stderr `scenarios_verified=2 != scenarios_total=3`; phase unchanged | e2e | same |
| S3 | WHEN gates passed and counts agree (including 0/0) THE CLI SHALL accept | S1 + `passed 3/3` · S1 + `passed 0/0` | `--phase review-pending` | exit 0; phase `review-pending` | e2e | same (rows) |
| S4 | WHEN `gates_status=failed` THE CLI SHALL reject at review-pending AND at push | S3 then `--gates-status failed` | `--phase review-pending`; separately a review-done record → `--phase push` | both rejected with `requires gates_status=passed` | e2e | same |
| S5 | WHEN the review gate already rejects at push THE verify gate SHALL not mask it (both errors reachable) | review-done record, verify fields ok, review fields empty | `--phase push` | `ErrPhaseRequiresReview` (review gate runs first, unchanged ordering documented) | integ | Go `internal/workers` table |
| S6 | WHEN the gate fires THE finisher prompt path is unaffected (finisher never writes review-pending) | `build_finisher_prompt` | read | no `review-pending` write in the prompt | unit | pytest |
| S7 | WHEN a package has one default + one tagged test THE lane script SHALL run exactly the tagged one | fixture module `scripts/tests/fixtures/lane/` | `scripts/integration-lane.sh ./scripts/tests/fixtures/lane -v` | output has `=== RUN   TestOnlyTagged_Y`, does not have `TestDefault_X`; exit 0 | e2e | bash test `scripts/tests/test_integration_lane.sh` |
| S8 | WHEN a package has no integration-only tests THE script SHALL exit 0 with a message and run nothing | `./internal/handoffop` (real package) | script | stdout `no integration-only tests`; no `=== RUN`; exit 0 | e2e | same |
| S9 | WHEN a tagged test fails THE script SHALL exit non-zero | fixture's `TestOnlyTagged_Y` fails when `LANE_FIXTURE_FAIL=1` | `LANE_FIXTURE_FAIL=1 scripts/integration-lane.sh ./scripts/tests/fixtures/lane` | exit ≠ 0; `--- FAIL: TestOnlyTagged_Y` | e2e | same |
| S10 | The discovered set on `main` equals today's hand-maintained lists for the three packages (no test silently dropped by the switch) | current `main` | run the `comm` derivation for each package | set equality with the three `-run` lists in `ci.yml` at the parent commit; recorded verbatim in `## Evidence — before` (pre-checked at `037350c`: 2 / 3 / 17 names, all three sets equal) | e2e (one-off, evidence only) | shell |

S1–S4 are rows of one `cmd/fleet` table test on the built binary; S5 rows in the existing
gate table; S7–S9 rows in one bash test.

## Tests removed / KEEP

- **Tests removed:** none.
- **KEEP:** every existing gate test (`validateReviewGate` rows, `ErrPhasePushNonGit`,
  `phase=done requires pr_url`); they run before the new rule and are untouched. Existing
  fixtures that write `phase=review-pending` directly via `WriteState` in Go tests **must
  be surveyed**: any that do so without the fields now fail — those are updated to set
  `GatesStatus="passed"` (a fixture change, not a behaviour change; list them in the PR).

## Acceptance

1. Full gates green; the three CI lanes run via the script and their step logs show the
   same test names as before the switch (S10 evidence in the PR body).
2. S1–S9 pass.
3. A worker (dogfood: this task's own worker) that records fields correctly reaches
   `review-pending` unhindered; one deliberately wrong write is shown rejected in
   `## Evidence — after`.

## Non-goals

- Validating `verification.md` contents in Go (the finisher's prompt check stays the
  content gate; the Go gate checks the claim fields).
- Renaming tests to a naming convention.
- Touching the review slots.
