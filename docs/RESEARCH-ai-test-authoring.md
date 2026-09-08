# RESEARCH — how to make agents write good tests

- **Status:** RESEARCH (input to `docs/DESIGN-spec-driven-testing.md`)
- **Scope:** external evidence + fleet gap analysis. No fleet behavior change in this doc.
- **Date:** 2026-08-29
- **Question asked:** how do we get fleet workers to write tests that are
  spec-driven, multi-level (unit / integration / regression / benchmark),
  consolidated, and traceable to the design and task-plan docs?

## The short version

Agents are already good at *volume* and *boundary cases*. They are bad at
three things, and all three are fixable with process, not with a better model:

1. **Tautology.** A test written after the implementation by the same agent
   asserts what the code does, not what the spec says. Green suite, broken
   feature, zero signal.
2. **Non-hermeticity.** Real clock, real filesystem, unseeded randomness,
   real network → flaky suites that train the team to ignore red.
3. **No traceability / no shape discipline.** Nothing links a test to a
   requirement, so coverage is unmeasurable against intent, and the suite
   grows as N near-identical single-scenario functions.

The counter-measures with the best evidence behind them: **write the test
before the code and from the spec** (not from the diff), **split the roles**
(test author ≠ implementer), **prove the test can fail** (confirm-RED or a
mutation probe), **enforce hermeticity with a linter, not a request**, and
**force the agent to learn repo conventions before writing** anything.

## Evidence

### Agent-written tests: broad coverage you can't trust

The largest study to date compares 204,673 test files from the AIDev dataset
(24,941 human-authored, 179,732 agent-generated) with AST-level static
analysis — "Beyond Test Presence: Assessing the Quality and Robustness of
Agent-Generated Tests in Open-Source Projects"
(<https://arxiv.org/html/2607.12068v1>):

| Dimension | Human | Agent | Reading |
|---|---|---|---|
| Edge-case variety score | 0.32 | 0.62 | agents ~2x better at boundaries |
| Tests null inputs | 8.3% | 13.4% | agents better |
| Tests zero inputs | 11.2% | 27.7% | agents better |
| Strong assertions | 88.1% | 85.4% | humans slightly better |
| "Unknown" assertion patterns | 1.46% | **11.58%** | agents much worse |
| Non-determinism | 3.1% | 5.2% | agents worse |
| File-I/O usage | 3.5% | 4.4% | agents worse |
| Flakiness-candidate rate | 0.30 | 0.41 | agents worse |

Two numbers matter for fleet. **11.58% "unknown" assertion patterns**: nearly
one in eight agent assertions uses a non-standard or misspelled assertion
method — some of which assert *nothing* and pass unconditionally. And a
**0.41 flakiness-candidate rate** driven by file I/O and non-deterministic
logic — what the paper calls missing "environmental awareness". The authors'
conclusion: treat agents as high-volume test generators requiring oversight,
not as autonomous test authors.

Companion studies on the same dataset: agents authored 16.4% of all
test-adding commits, and their test methods are longer with a higher assertion
density but lower cyclomatic complexity (linear, table-shaped logic), with
coverage gains comparable to human tests
(<https://arxiv.org/pdf/2603.13724v1>). So the shape agents naturally produce
is *close* to the table-driven shape we want — it just needs to be demanded
explicitly.

Field data on the surrounding blast radius (as collated in
<https://www.codewithseb.com/blog/test-driven-agentic-development-guide>):
incidents per PR up ~24%, change-failure rate up ~30%, PRs ~18% larger, logic
errors 1.75x more common in AI-generated code. The bottleneck moved from
producing code to verifying it.

### The ordering constraint (test-first) matters *more* with agents

Same source, and the single most load-bearing claim in the literature: if the
model that wrote the implementation then writes the test, the test encodes the
implementation's misunderstanding. Prose intent is interpretable
("handle invalid input gracefully"); an assertion is not
(`expect(() => parse('')).toThrow(ValidationError)`). In an agentic loop the
test is the only artifact carrying operator intent in machine-checkable form,
so it must exist first and be **reviewed as a specification**.

Practical corollaries the post recommends and we should steal:

- Rules in project memory (`CLAUDE.md` / standards), not in each prompt.
  Specifically: *never* modify an existing assertion to make a test pass —
  ask instead. Without that rule, a stuck agent eventually "fixes" red by
  editing the oracle.
- Hooks over discipline: a `PostToolUse` hook running the related tests after
  every edit gives feedback without being asked.
- **Role split with filesystem permissions**: one agent writes tests and
  cannot edit `src/`; another implements and cannot edit `tests/`. This kills
  the tautology structurally rather than by convention.
- Hermeticity audit as a checklist item *and* a lint rule: injected clock,
  seeded randomness, temp-dir fixtures, shuffled test order, and an
  assertion-exists rule (`vitest/expect-expect`, pytest strict markers) — the
  cheapest possible catch for the 11.58% pathology.

### Spec-driven test generation

CoreStory's spec-driven-test-generation playbook
(<https://docs.corestory.ai/playbooks/spec-driven-test-generation>) frames the
distinction fleet needs verbatim: **code-mirroring tests assert *how*,
specification-driven tests assert *what***.

```python
# code-mirroring — breaks on any refactor, verifies nothing
def test_order_calls_minimum_check_validator():
    with mock.patch("OrderValidator.check_minimum") as m:
        m.return_value = False
        submit_order(create_order(total=4.99))
        m.assert_called_once_with(4.99)

# specification-driven — survives any behavior-preserving refactor
def test_order_rejected_when_below_minimum_amount():
    result = submit_order(create_order(total=4.99))
    assert result.status == "rejected"
    assert "minimum order amount" in result.error
```

Their workflow order is: behavioral extraction (acceptance criteria, state
transitions, invariants, authorization matrices) → convention discovery →
gap analysis (already covered / partially covered / uncovered) → generation →
validation that each test verifies the intended spec rather than an
implementation detail. The gap-analysis and validation steps are the parts
fleet currently has no analogue for.

AWS Kiro's spec workflow (<https://kiro.dev/docs/specs/feature-specs/>) is the
closest thing to fleet's PLAN-DOC → TASK-PLAN-DOC gates that exists as a
product. Its three artifacts are `requirements.md` / `design.md` /
`tasks.md`, and requirements are written in **EARS** notation:

```
WHEN <condition/event> THE SYSTEM SHALL <expected behavior>
```

Kiro's stated reason for EARS is exactly our ask #3: clarity, **testability**
("each requirement can be directly translated into test cases"),
**traceability** (a requirement can be tracked through implementation), and
completeness. It also has an explicit "analyze requirements for
inconsistencies, ambiguities, conflicts and gaps" step before design.

SpecSafe (<https://github.com/Agentic-Engineering-Agency/specsafe>) shows the
staged enforcement shape: `spec slice → tests → implementation → verify → QA`,
where the test stage **generates test files from spec scenarios with every
test skipped**, and the code stage un-skips one test at a time. The skip-list
*is* the pre-defined test-case set — a mechanically checkable contract
between planner and implementer.

### Making the agent prove the test can fail

Microsoft's polyglot unit-test agent
(<https://devblogs.microsoft.com/dotnet/polyglot-unit-testing-agent/>,
open source in `dotnet/skills`) is the best-measured "testing skill" published.
Its structure is a four-phase loop:

1. **Learn the repo first** — language, framework, where tests live, how they
   are named, and *how the repo's own test command discovers tests* (this
   catches the classic "new test project passes locally but never runs in CI").
2. **Size the work** — direct / single-pass-plan / iterative, chosen by scope.
3. **Write, following local conventions**, never modifying production code, and
   never writing unit tests that hit URLs, ports, or wall-clock timing.
4. **Check the tests are useful** — consider small code changes that *should*
   break each test (a lightweight mutation probe), look for weak or missing
   assertions, confirm every requested scenario has a matching test, run the
   full suite, and confirm the repo's normal test command finds the new tests.

Measured against stock Copilot on 152 real tasks with the same model: 92.1%
vs 78.9% task completion, 63% fewer failures — and the entire gain came from
**vague prompts** (88.8% vs 66.3%) and **diff-scoped requests** (15/15 vs
0/15). On detailed prompts the two tied (96.8% each). It generated 2.3%
*fewer* tests at the same coverage.

That result is the core argument for a fleet testing skill: a workflow lifts a
mid-tier model to near-top-tier results, and it pays off precisely where the
instruction is under-specified — which is every worker prompt that says
"add tests".

Mutation testing as a **gate** rather than a metric is the other recurring
idea (<https://dontcodethisathome.com/proving-a-generated-test-can-fail-mutation-testing-as-a-sufficiency-gate-for-an-ai-coding-agent>,
<https://visdom-maturity-matrix.virtuslab.com/guides/development/mutation-testing-agent-validation>).
The pipeline described there certifies a **confirm-RED baseline before the
implementer runs**, then re-checks with a mutation gate at the end; a suite
scoring low on mutants is "mostly decorative" however many assertions it has.
Full mutation runs are expensive; a targeted one-mutant probe per contract
item is not.

### How Devin frames the same problem

Devin's public guidance
(<https://docs.devin.ai/product-guides/creating-playbooks>,
<https://docs.devin.ai/use-cases/gallery/test-coverage-playbook>) puts testing
conventions in two places: **Knowledge** for standing style/convention rules,
and **Playbooks** for repeatable procedures. A playbook has
`Procedure` (imperative, one step per line, MECE, covering setup → task →
delivery), `Specifications` (postconditions — what must be true when done),
`Advice` (corrections to the agent's priors), `Forbidden Actions`, and
`Required from User`.

Their published test-coverage playbook is worth reading as a shape, because
almost every line is domain-specific and mechanically checkable: read the
module and enumerate exported functions; study existing tests for patterns;
file naming; AAA structure; *which* mock helper to use; integer cents, never
floats; the specific idempotency case that must be covered; the exact command
to verify coverage. Forbidden: real gateways, float currency, modifying the
source to ease testing, skipping retry/idempotency edge cases.

**The transferable lesson for fleet:** the unit of quality is not "a testing
philosophy" — it is a per-project, per-domain, *imperative* procedure with
postconditions and forbidden actions, plus repo-convention discovery. Fleet's
`standards.md` is the Knowledge analogue and already good; fleet has no
Playbook analogue for testing, and no per-project test-convention artifact.

## Where fleet stands today

What fleet already has (and is ahead on):

- `templates/standards.md` `## Testing` — TDD-required, stdlib-only,
  regression test per bug fix, test SHAPE rules (one table test per driver,
  one row per case), test-one-CONTRACT rule, ~1.5x test-LOC budget, and
  "every test must name the bug it catches". This is a strong baseline; much
  of the external advice is already in it.
- `## Implementation` — delete orphaned/subsumed tests, with the task plan
  listing "Tests removed" and "KEEP".
- Standards are merged per-H2 (global + per-project override) and **inlined
  into every worker prompt** by the coordinator skill — a real distribution
  channel, no per-prompt repetition needed.
- Mandatory PLAN-DOC and TASK-PLAN-DOC gates before promotion, with a
  "Test plan = one line per test (scenario / input / expected)" doc rule and
  a dual-reviewer pass whose lenses include testability.
- A worker → reviewer → finisher split (the reviewer never wrote the code).
- Real test-hygiene machinery in CI: `scripts/lint-test-isolation.sh`
  (tmux-socket isolation enforcement), an integration-only build-tag lane,
  a `/tmp` leak sentinel + assert, per-step timeouts and a wall-clock budget
  (`docs/DESIGN-ci-3min-test-suite.md`).

The gaps, mapped to the four asks:

| # | Ask | Gap today |
|---|---|---|
| 1 | Be comparable to Devin | No test-authoring *procedure* (learn-repo → plan → write → self-check), no per-project test-convention artifact, no forbidden-actions list, no postconditions the worker must paste. |
| 2 | Task plan pre-defines test cases | "Test plan" is prose, one line per test, with no IDs, no levels, no oracle column, and nothing mechanically links a shipped test to a planned case. |
| 3 | Spec-driven, covers design + task plan | Design docs have no requirement IDs, so "tests cover the design" is a reviewer's opinion, not a matrix. No gap analysis, no NO-TEST-with-reason escape hatch. |
| 4 | Compressed / consolidated / right level | Shape rules exist in prose but nothing gates them; no rule for *when* an integration test is required vs. forbidden; no benchmark policy at all; no confirm-RED, no mutation probe, no hermeticity lint beyond tmux sockets; nothing stops an implementer from editing the oracle. |

## What the evidence says fleet should adopt

Ranked by expected value per unit of work:

1. **Requirement IDs in DESIGN docs + a Test Contract table in TASK-PLAN
   docs** (Kiro/EARS + CoreStory gap analysis). Turns "tests cover the design"
   into a matrix diff, and pre-defines the test set before any code exists —
   asks #2 and #3, and it is almost free because the doc gates already exist.
2. **Confirm-RED before implementation** (TDD ordering + the mutation-gate
   pipelines). The cheapest structural kill for tautological tests; fleet
   already has worktrees to run a test at the parent commit.
3. **Oracle immutability rule** ("never edit an assertion to go green; ask") —
   one line in standards, prevents the worst agent failure mode.
4. **Hermeticity lint extension** (clock / randomness / network / real FS /
   assertion-exists). Directly targets the measured 0.41 flakiness rate and
   the 11.58% no-op-assertion rate, and fleet already owns the lint script
   that these checks belong in.
5. **A test-authoring skill with a learn-repo phase and a self-check phase**
   (dotnet/skills shape, Devin-playbook rigor). Biggest measured lift on
   vague prompts, which is what "add tests" always is.
6. **Level-selection policy with an explicit integration-required trigger
   list, plus consolidation and budget gates** — ask #4, and it is the piece
   that keeps the suite from becoming the thing that slows fleet down.

The design that turns this list into fleet-shaped changes is
`docs/DESIGN-spec-driven-testing.md`.

## Sources

- Beyond Test Presence: Quality and Robustness of Agent-Generated Tests (204,673 files, AIDev) — <https://arxiv.org/html/2607.12068v1>
- Testing with AI Agents: Test Generation Frequency, Quality, Coverage — <https://arxiv.org/pdf/2603.13724v1>
- Do Autonomous Agents Contribute Test Code? Tests in Agentic PRs — <https://arxiv.org/pdf/2601.03556>
- Test-Driven Agentic Development — <https://www.codewithseb.com/blog/test-driven-agentic-development-guide>
- From generated code to trusted code with a unit-test agent (.NET blog, `dotnet/skills`) — <https://devblogs.microsoft.com/dotnet/polyglot-unit-testing-agent/>
- Spec-Driven Test Generation playbook — <https://docs.corestory.ai/playbooks/spec-driven-test-generation>
- Kiro Feature Specs + EARS notation — <https://kiro.dev/docs/specs/feature-specs/>
- SpecSafe (spec slice → tests → code, skipped-test contract) — <https://github.com/Agentic-Engineering-Agency/specsafe>
- Mutation testing as a sufficiency gate for a coding agent — <https://dontcodethisathome.com/proving-a-generated-test-can-fail-mutation-testing-as-a-sufficiency-gate-for-an-ai-coding-agent>
- Mutation-testing agent validation (maturity matrix, L4) — <https://visdom-maturity-matrix.virtuslab.com/guides/development/mutation-testing-agent-validation>
- Devin: Creating Playbooks — <https://docs.devin.ai/product-guides/creating-playbooks>
- Devin: test-coverage playbook example — <https://docs.devin.ai/use-cases/gallery/test-coverage-playbook>
