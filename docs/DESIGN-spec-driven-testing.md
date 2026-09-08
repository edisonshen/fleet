# DESIGN — spec-driven testing for fleet workers

- **Status:** PROPOSAL — awaiting operator approval (G2)
- **Scope:** how fleet plans, dispatches, gates, and consolidates tests. Doc
  gates + standards + one new skill + two lint/CLI gates. No coordinator
  lifecycle change.
- **Priority:** P1 (quality of every future PR fleet produces)
- **Depends-on:** `docs/RESEARCH-ai-test-authoring.md` (evidence),
  `docs/DESIGN-coordinator-plan-docs.md` (the doc gates this extends)
- **PR-base:** `main`

## Problem

Fleet workers write tests today because `standards.md` tells them to. What
they write is decided *by the worker, after it has written the code*. Three
things follow from that, and all three are visible in fleet's own history:

1. **The test can only be as right as the implementation.** The same agent
   that misread the requirement writes the assertion, so the assertion agrees
   with the misreading. Green suite, wrong feature.
2. **Nothing connects a test to the design.** "Do the tests cover the design
   doc?" is answered by a reviewer's judgment on every task, from scratch,
   with no artifact to diff. Coverage against *intent* is unmeasurable.
3. **The suite grows faster than the product.** With no level policy and no
   consolidation gate, "add tests" means "add more test functions" — a matrix
   of near-identical single-scenario tests, integration tests written for
   single functions, and nothing at all for performance claims.

The external evidence (see the research doc) is blunt about where this ends:
agent-written tests cover boundaries about twice as well as human ones, but
carry a 0.41 flakiness-candidate rate (vs 0.30) and **11.58% "unknown"
assertion patterns** — nearly one in eight assertions is non-standard,
misspelled, or asserts nothing at all.

## How it works today

```
DISCUSS ─▶ PLAN-DOC ─▶ SPLIT ─▶ TASK LIST ─▶ TASK-PLAN-DOC ─▶ IMPLEMENT ─▶ PR-TRACK ─▶ DONE
             │                                    │                │
             │ "Test Plan" section:               │ "Tests" bullet │ worker writes code
             │ prose, one line per test           │ prose          │ AND tests, then
             └────────────── no IDs ──────────────┘                └─ reviewer reads both
```

The plan docs describe tests in prose. The worker writes code and tests
together in one context. The reviewer checks both — by reading. The only
mechanical test gates in the repo are hygiene gates
(`scripts/lint-test-isolation.sh`, the `/tmp` leak sentinel, per-step
timeouts): nothing checks *what* was tested.

## What goes wrong

```
   design doc says          worker writes           reviewer sees
   "reject below $5"   ─▶   code that rejects  ─▶   test passes, code
                            below $5.00 only        matches test  ─▶ MERGED
                            (float compare)                            │
                            + a test asserting                         ▼
                            exactly that                        bug ships, no
                                                                test names it
```

- **Tautology:** the oracle is derived from the code, not the spec.
- **Oracle drift:** a worker stuck on red eventually edits the assertion to go
  green. Nothing forbids it.
- **Unprovable regressions:** "all bug fixes carry a regression test that
  fails on the parent commit" is a rule with no check. Nobody ever runs it at
  the parent commit.
- **Level confusion:** integration tests written against one function; PRs
  with no cross-boundary change carrying integration tests anyway; perf claims
  with no benchmark.
- **Suite bloat:** N functions where one table with N rows belongs, and no
  deletion of the tests the change subsumed.

## The fix

Four moves. Each one turns an opinion into an artifact or a check.

**1. Requirements get IDs; tests get IDs; the plan maps one to the other.**

```
DESIGN-<topic>.md                    TASK-PLAN-<slug>.md
┌───────────────────────────┐        ┌────────────────────────────────────────┐
│ ## Requirements           │        │ ## Test Contract                       │
│ R1 WHEN <cond> THE        │◀──────▶│ T1  R1  unit    <scenario> <oracle>    │
│    SYSTEM SHALL <behavior>│        │ T2  R1  unit    <row 2 of same table>  │
│ R2 ...                    │◀──────▶│ T3  R2,R3 integ <end-to-end arc>       │
│ R3 ...                    │        │ R4  NO-TEST — <reason>                 │
└───────────────────────────┘        └────────────────────────────────────────┘
        every R appears in ≥1 T, or is explicitly NO-TEST with a reason
```

The Test Contract is written by the coordinator **before promotion**, reviewed
in the existing dual-review pass, and is the worker's *complete* test
assignment. New tests the worker discovers it needs are allowed — but they
land as amendment rows in the doc, not as silent additions.

**2. The test exists, and is proven able to fail, before the implementation.**

```
task promoted
   │
   ├─▶ test-author subagent   writes ONLY test files from the Test Contract
   │                          runs them  ──▶ must be RED for every T
   │                          `fleet tests confirm-red` records the evidence
   │
   └─▶ worker (implementer)   writes production code until GREEN
                              MAY NOT edit test files or assertions;
                              a needed oracle change = amendment + re-RED
```

This is the structural kill for tautology: a different agent, with a
different context, wrote the oracle — and the oracle was observed failing.

The RED commit is also the **audit boundary** for every commit that follows:

```
  <red_sha>            impl commit         impl commit        HEAD
  tests only  ─────────▶ prod code ─────────▶ prod code ────────▶
  (all T RED)            (no test edits)      (no test edits)

  git diff <red_sha>..HEAD -- '*_test.go' 'skills/**/tests/'  MUST be empty
  non-empty ⟹ a matching amendment row in the Test Contract, else P0
```

A mid-flight test change is legal only through the amendment path: the worker
reports `ORACLE_DISPUTE <T> <argument>` (existing oracle is wrong) or
`NEW_CASE <what>` (a case the contract missed), the coord amends the contract,
and the change lands as its own `tests: amend T<n>` commit that is proven RED
**before** the commit that makes it green. Invariant: every oracle in the
branch has a commit where it was observed failing.

**3. Level policy: one contract per test, the cheapest level that can hold it.**

```
level        answers                                  required when
─────────    ──────────────────────────────────────   ──────────────────────────────
unit/table   pure logic, boundaries, error paths      always (the default home)
contract     one module's public promise (parse,      a public surface changes
             resolve, write-format)
integration  ONE end-to-end operator-visible arc      change crosses ≥2 packages OR a
             across REAL boundaries                   process/tmux/FS/PR boundary OR an
                                                      on-disk format OR a lifecycle
                                                      transition — otherwise FORBIDDEN
regression   the exact bug, named                     every bug fix (with confirm-RED)
benchmark    a perf claim                              the PR claims a perf property
property     an invariant over generated input        an invariant is stated in R
```

Integration tests assert an *arc*, so one integration test legitimately
satisfies several R IDs — the Test Contract records that as `T3 → R2,R3`. If
the trigger list does not fire, the plan must say
`Integration: NOT REQUIRED — <reason>` and the reviewer rejects an integration
test added anyway.

**4. Consolidation and hygiene become gates, not preferences.**

- One table per driver: the Test Contract's rows *are* the table rows, so N
  contract items on one driver arrive as one function with N rows by
  construction.
- Budget: test LOC ≤ ~1.5x the production LOC it covers (already in
  standards) plus a **new function-count check**: a PR adding more test
  functions than Test Contract drivers needs a justification line.
- Deletions: "Tests removed (reason)" and "KEEP (retained behavior)" already
  required by standards — the Test Contract makes them a column, so
  over/under-deletion is visible pre-implementation.
- Hermeticity + assertion-exists lint, extending the script fleet already has.

## Decisions

### Q1 — Where does the pre-defined test set live: task plan doc, or `tasks.md`?

**Doc first, field later.** Phase 1 puts the Test Contract in
`docs/TASK-PLAN-<slug>.md` and links it into worker-visible task text exactly
as the existing TASK-PLAN-DOC step already requires — zero schema change,
ships immediately. Phase 2 adds an optional `### Tests` section to the
`tasks.md` task schema (`internal/tasks`) so `fleet tasks show` and the worker
prompt can carry the contract inline, and so a machine can check it.

### Q2 — EARS notation for requirements, or freeform?

**EARS-lite, IDs mandatory.** `R<n>: WHEN <trigger> THE SYSTEM SHALL
<observable behavior>` where it fits; freeform prose allowed for invariants
("`SHALL always` ..."). The ID is the load-bearing part, not the grammar. Rule:
one testable behavior per R — if an R needs "and", split it.

### Q3 — Separate test-author subagent, or the worker writes tests first?

**Separate subagent (operator decision, 2026-08-29), and a coord-authored
Test Contract is mandatory in both cases.** The role split is the strongest
anti-tautology measure available: a different agent, with a different context,
writes the oracle. It costs one extra dispatch and one extra context per task.
Same-worker test-first-then-code remains acceptable for P2/P3 and doc-only
tasks, where a wrong-but-green test is cheap; the RED-commit audit boundary
above applies identically in that mode, so the enforcement surface does not
fork. Fleet's IMPLEMENT flow already has three roles; this adds a fourth stage
in front, reusing the same dispatch machinery:

```
test-author ─▶ worker ─▶ reviewer(alpha/beta) ─▶ finisher
 (RED proof)    (GREEN)   (P0/P1 + contract      (push + PR)
                           traceability)
```

### Q4 — How hard is the oracle-immutability rule?

**Hard, with a named escape hatch.** A worker may not modify an existing
assertion or delete a test to go green. If the oracle is wrong, the worker
stops, reports `ORACLE_DISPUTE` with the T ID and the argument, and the coord
amends the Test Contract (its allowed write surface) and re-runs the RED
proof. The reviewer diffs test files against the test-author commit; any
unexplained oracle change is an automatic P0.

### Q5 — Mutation testing: full run, or targeted probe?

**Targeted probe only.** Full mutation runs do not fit the CI budget
(`docs/DESIGN-ci-3min-test-suite.md` bought a <3-minute suite; we are not
spending it). Instead: `confirm-RED` at the parent commit is the probe for
regressions, and for new behavior the test-author's RED run *is* the mutation
evidence (the code path does not exist yet). A manual
`fleet tests probe --test T3 --mutate <file>:<line>` stays a reviewer tool,
not a gate.

### Q6 — Do we gate traceability in CI, or only in review?

**Reviewer-only first (operator default), CI after it is quiet — the finisher
does not block on it.** Phase 1: the reviewer prompt gets a
mechanical checklist (every T present, every R covered or NO-TEST, no
un-amended oracle change, level policy respected). Phase 3: a
`scripts/lint-test-traceability.sh` step fails a PR whose linked task plan has
a Test Contract with T IDs missing from the diff. Gating a doc-derived
contract in CI is only safe once the contract format has been exercised on
real tasks.

### Q7 — Are benchmarks per-PR?

**No.** Benchmarks are required only when a PR *claims* a performance property
(latency, allocations, suite wall-clock, tick cost). Then: a Go `testing.B`
benchmark in the touched package, a before/after `benchstat` paste in the PR
body from the same machine, and the claim stated as a number with a threshold.
Benchmarks never run in the default CI lane; they go where the integration
lane already lives — a scoped, tagged step.

## Implementation detail (for engineers)

### Artifact: `## Requirements` in DESIGN docs

```markdown
## Requirements

| ID | Requirement | Level (planned) | Notes |
|----|-------------|-----------------|-------|
| R1 | WHEN a task plan has no Test Contract THE SYSTEM SHALL refuse promotion | contract | exit 2 |
| R2 | WHEN the implementer edits a test file THE SYSTEM SHALL fail review with P0 | integration | reviewer diff |
| R3 | The resolver SHALL always return a beta slot | unit/table | invariant |
```

Rules: IDs are stable once the doc is approved (append-only; never renumber).
An R that turns out untestable is struck through with the reason, never
deleted. `NO-TEST` requires one of: pure-plumbing, covered-by-R<m>,
externally-owned, or operator-waived.

### Artifact: `## Test Contract` in TASK-PLAN docs

```markdown
## Test Contract

| T | Covers | Level | Scenario / row | Oracle (what must be true) | Suite / driver | Names the bug |
|----|--------|-------|----------------|----------------------------|----------------|----------------|
| T1 | R1 | unit | plan doc missing Test Contract | `promote` exits 2, stderr names the doc | `TestPromoteGate` (table row) | promotion without a test set |
| T2 | R1 | unit | contract present but zero T rows | same | `TestPromoteGate` (row 2) | empty contract accepted |
| T3 | R2,R3 | integration | full task: promote → test-author → worker edits a test | reviewer returns P0 with T ID | `TestOracleImmutability_Integration` | implementer silently rewrites oracle |

**Tests removed:** `TestPromoteAllowsAnyDoc` — subsumed by T1/T2 (asserted the
old no-gate behavior).
**KEEP:** `TestPromoteRequiresRenderedHTML` — unrelated retained behavior.
**Integration:** REQUIRED — crosses `cmd/fleet` + `skills/coordinator`.
**Benchmark:** NOT REQUIRED — no perf claim.
```

Every T row is one table row in one test function wherever the driver is
shared; the `Suite / driver` column is what makes consolidation visible before
any code exists. Test names carry their T ID (`.../T1_...` or a row named
`T1_plan_doc_missing`) so traceability is greppable:

```sh
git diff --unified=0 main... | grep -oE 'T[0-9]+' | sort -u   # shipped T IDs
```

### Stage: test-author subagent

Return contract (sentinel grammar, consistent with the existing worker
sentinels): `TESTS_RED_PROVEN <slug> <n_tests> <commit>` or
`TESTS_BLOCKED <slug> <reason>`. Constraints in the prompt:

- MAY create/modify only test files and test-only fixtures/testdata.
- MUST implement every T row and nothing else; MUST NOT add a test with no T.
- MUST run the suite and observe RED for each T, pasting the failure lines.
- MUST NOT touch production code — if a test needs a seam (injected clock, an
  interface), it reports `SEAM_NEEDED <T> <what>` and the coord amends the
  plan (a seam is a design decision, not a test detail).
- Hermetic by construction: injected clock, seeded randomness, `t.TempDir()` /
  `tmp_path`, `FLEET_HOME` + `FLEET_TMUX_SOCKET` isolation, no network, no
  real `/tmp` scan, no `time.Sleep` for synchronization.

For a bug fix, RED is proven **at the parent commit** — `fleet tests
confirm-red` creates a worktree at the parent, applies only the test files,
runs the scoped test, and requires a failure. That is the mechanical form of
the existing standards rule.

The RED commit SHA is recorded in the worker's `state.json`
(`tests_red_sha`) and echoed in the PR body, so the reviewer's audit diff and
a later CI check read the same boundary rather than re-deriving it.

### Rendered docs

`.md` is the spec and the only committed artifact (operator decision,
2026-08-29). `docs/*.html` stays gitignored and local — the PLAN-DOC /
TASK-PLAN-DOC render+open steps remain a local reviewer convenience, not a
committed output.

### Gate: reviewer checklist additions (mechanical, in order)

1. Every T ID in the contract appears in the diff; every R is covered or
   explicitly NO-TEST. → missing = P0.
2. `git diff <tests_red_sha>..HEAD -- '*_test.go' 'skills/**/tests/'` is empty,
   or every hunk maps to an amendment row that was itself proven RED in a
   `tests: amend T<n>` commit. → unexplained change = P0.
3. Level policy: integration test present iff the trigger list fires; each
   integration test asserts one operator-visible arc, not one function. → P1.
4. Shape: T rows sharing a driver arrived as rows, not functions; every test
   names the bug it catches; "Tests removed"/"KEEP" honored exactly. → P1.
5. Hygiene: no real clock/sleep-sync/unseeded randomness/network/real-FS path;
   every test has a real assertion. → P0 (this is the 11.58% pathology).
6. Perf claims carry a benchmark + benchstat paste. → P1.

### Gate: `scripts/lint-test-hygiene.sh` (new, sibling of the isolation lint)

Static, AST-free, ripgrep-shaped checks over `*_test.go` and
`skills/**/tests/test_*.py`, following the existing script's allow-marker
convention (`// test-hygiene: allow <rule> — <reason>`):

| Rule | Go pattern | Python pattern |
|---|---|---|
| no sleep-sync | `time.Sleep(` outside a marked helper | `time.sleep(` |
| no wall clock | `time.Now()` in an assertion path | `datetime.now(` |
| unseeded rand | `rand.` without `rand.New(rand.NewSource(` | `random.` without `seed(` |
| real FS | absolute `/tmp/` literal (use `t.TempDir()`) | `open("/tmp` |
| network | `net.Dial`, `http.Get`, `httptest` against a real host | `requests.`, `socket.` |
| assertion exists | test func body with no `t.Fatal`/`t.Error`/`want` compare | test with no `assert` |
| env isolation | `os.Setenv` (use `t.Setenv`) | `os.environ[` without `monkeypatch` |

Also: `go test -shuffle=on` in the default lane (order-dependency detector,
one flag, no new runtime) and pytest strict markers.

### Gate: `fleet tests` subcommands (Phase 2/3)

```sh
fleet tests contract <slug>            # print the Test Contract parsed from the plan doc
fleet tests check <slug> [--base main] # T IDs in contract vs. T IDs in the diff → exit 2 on gap
fleet tests confirm-red <slug> --base <sha>   # worktree at base + test files → require FAIL
fleet tests probe --test <T> --mutate <file>:<line>   # reviewer-only mutation probe
```

`confirm-red` reuses `internal/dispatch`'s worktree machinery; `check` reads
the plan doc path from the task's Spec/Notes link written by the existing
TASK-PLAN-DOC step.

### Artifact: per-project test conventions

`~/.fleet/projects/<p>/test-conventions.md`, generated once by the
test-architect skill's learn-repo phase and refreshed when it goes stale:
framework and version, test file locations and naming, the harness/builder
helpers to reuse, the exact run commands (full + scoped), how the repo
*discovers* tests (so a new test file that CI never runs is caught), fixture
and isolation conventions, and the suite's wall-clock budget. Merged into
worker prompts next to the standards block. This is the piece that makes a
fleet worker behave like a Devin playbook run rather than a generic agent.

### New skill: `skills/test-architect/`

One skill, two entry points, embedded and installed like the existing two:

- **`contract` mode (coordinator-side):** read the design doc → extract/assign
  R IDs → gap-analyze against existing tests (what is already covered) →
  choose levels via the policy → emit the Test Contract table + removals/KEEP
  + integration/benchmark verdicts. Runs inside the coord's plan-doc write
  surface, so it needs no new mutation exception.
- **`author` mode (subagent-side):** learn repo conventions (or read the cached
  `test-conventions.md`) → write only the T rows → prove RED → self-check
  (assertions present and strong, every T covered, hermetic, the repo's own
  test command discovers the new tests) → return the sentinel.

Both modes get an explicit **Forbidden actions** list (Devin-playbook shape):
no production-code edits in author mode, no oracle rewrites, no
`assert` on stringified internals, no mocking a boundary the integration level
exists to exercise, no `sleep` for synchronization, no test without a T ID, no
new test framework or assertion library.

### Standards rewrite

`templates/standards.md` `## Testing` gains, without dropping anything it
already says: the level ladder + integration trigger list, "the Test Contract
is the assignment; no test without a T ID", oracle immutability +
`ORACLE_DISPUTE`, the hermeticity list, "every test must be able to fail —
prove it", and the benchmark-only-on-perf-claim rule. Distribution is free:
the coordinator already inlines merged standards into every worker prompt, and
per-project overrides already merge per-H2.

## Task split

| # | PR | Surface | Test Contract lives in |
|---|----|---------|------------------------|
| 1 | Doc gates: `## Requirements` + `## Test Contract` templates, coord SKILL steps 2/5 + reviewer checklist, `docs/COORDINATOR-WORKFLOW.md` | `skills/coordinator/SKILL.md`, docs | `test_skill_md.py` pins |
| 2 | Standards `## Testing` rewrite + level ladder/triggers + oracle rule | `templates/standards.md`, `docs/STANDARDS-BASELINE.md` | standards render/merge tests |
| 3 | `skills/test-architect/` (contract + author modes, forbidden actions) + embed + `fleet init` install | `skills/test-architect/`, `embed.go`, `cmd/fleet/init.go` | new `skills/test-architect/tests/` |
| 4 | `scripts/lint-test-hygiene.sh` + CI step + `go test -shuffle=on` + fixture regression suite | `scripts/`, `.github/workflows/ci.yml` | `scripts/tests/` shell fixtures |
| 5 | `tasks.md` optional `### Tests` section (parse/write/round-trip) | `internal/tasks` | `internal/tasks` table tests |
| 6 | `fleet tests contract\|check\|confirm-red` | `cmd/fleet/tests.go`, `internal/dispatch` worktrees | `cmd/fleet` table tests + one integration case |
| 7 | test-author stage in the IMPLEMENT flow (dispatch + sentinels + phase) | `skills/coordinator/dispatch.py`, `loop.py`, `internal/workers` | coordinator pytest + workers table tests |
| 8 | `test-conventions.md` generation + worker-prompt injection | `skills/test-architect/`, `skills/coordinator/dispatch.py` | coordinator pytest |

Order: 1 → 2 land immediately (docs/prompts only, reversible). 3 → 4 next
(new skill, new lint; both additive). 5 → 6 → 7 are the schema/CLI/lifecycle
changes and each needs its own dual review. 8 last.

Dogfood rule: PR #1 and #2 are the first tasks that must themselves ship a
Test Contract. If writing one for a docs PR is painful, the format is wrong —
fix the format before PR #3.

## Test plan (for this design's own PRs)

- `python3 -m pytest skills/coordinator/tests/test_skill_md.py -q` — skill text
  pins for the new steps (PR 1).
- `go test ./internal/standards -run Merge` — Testing-section override merge
  still per-H2 (PR 2).
- `python3 -m pytest skills/test-architect/tests -q` — contract emitter on a
  fixture design doc; author-mode forbidden-action pins (PR 3).
- `bash scripts/tests/test_lint_test_hygiene.sh` — one fixture per rule, both
  polarities, plus the allow-marker path (PR 4).
- `go test ./internal/tasks -run Tests` — `### Tests` round-trip, fence-aware,
  absent-section back-compat (PR 5).
- `go test ./cmd/fleet -run TestsCheck` — contract-vs-diff gap → exit 2; one
  integration case for `confirm-red` in a temp worktree (PR 6).
- `python3 -m pytest skills/coordinator/tests -q` + `go test ./internal/workers`
  — test-author phase transitions and sentinel parsing (PR 7).
- Full: `go build ./...`, `gofmt -l .`, `golangci-lint run ./...`,
  `go test -race -shuffle=on ./...`, `python3 -m pytest skills/ scripts/ -q`.

## Assumptions

- The coordinator remains the only writer of plan docs; the Test Contract is
  inside its existing write surface, so no new mutation exception is needed.
- Requirement IDs are append-only after approval. Renumbering would silently
  invalidate shipped test names.
- One extra dispatch per P0/P1 task (the test-author) is an acceptable cost;
  parallelism defaults to 3, and the stage is short-lived.
- The <3-minute CI budget is a hard constraint. Nothing in this design adds a
  default-lane cost beyond `-shuffle=on` and one ripgrep-shaped lint.
- `confirm-red` can rely on `internal/dispatch` worktrees being available in
  the worker's environment; if not, the stage degrades to "RED observed at
  HEAD before implementation" and says so in the PR body.
- Operators can opt a project out per-H2 via
  `~/.fleet/projects/<p>/standards.md` — this design does not add a new
  configuration surface.

## Open threads

1. Should `fleet tests check` gate the **finisher** (block PR creation) instead
   of only the reviewer? Cheaper to enforce, but it moves a doc-derived check
   into the push path.
2. Property-based testing: worth a `property` level in the ladder now, or wait
   until an R actually states an invariant that needs generated input?
3. Do we want a flakiness ledger (`~/.fleet/projects/<p>/flaky.md`) fed by
   reviewer/CI reruns, so repeat offenders get deleted rather than retried?
