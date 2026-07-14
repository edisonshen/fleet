# DESIGN — reviewer engine/model + effort selection (minimal)

- **Status:** DUAL-REVIEW CLEAN (rev13 — both reviewers SOUND, no P0/P1) — **pending operator re-approval**
- **Scope:** give **users who don't have codex a real two-reviewer gate** — today they
  get only one. Pin the reviewer stage to the **right models at high effort** and always
  run **two** reviewers (two Claude models when there's no codex; codex + Claude when
  there is). Nothing else changes.
- **Primary use case:** the **no-codex user.** Codex users already get a second engine;
  this task is what makes the review gate hold for everyone else.
- **Priority:** P1. **Depends-on:** none. **PR-base:** `main`.
- **Supersedes:** `feedback_code_review_model_effort` (codex-medium + Sonnet) and the
  Approach-A single-Claude-reviewer selection in `build_reviewer_prompt`.
- **Explicitly cut (NOT in v1):** consensus/exchange/agree loop, human-approval-before-PR,
  a new `review-consensus` phase, a headless process-runner, finding-ID cross-round
  continuity, **backward-compat / migration of in-flight records** (operator: clean
  rename — delete the old flow + its tests, don't carry compat shims).

---

## The problem, in plain English

The reviewer stage is the quality gate before merge. **The gap this task closes: a user
without codex gets only ONE reviewer.** Today's reviewer runs `/review` plus a
best-effort `codex review`; with no codex the codex half is skipped and the change ships
reviewed by a single model — one set of blind spots. Worse, the gate fields are named
after engines (`review_claude_status`, `review_codex_status`), so there is literally
nowhere to record a *second Claude* reviewer: a no-codex user *cannot* be given two.

A secondary weakness, for everyone: **no model or effort is chosen** — the review runs
on whatever the session defaults to, when we'd want a strong model at high reasoning.

Constraint that makes the fix clean: **every Fleet user has Claude**; codex is an
optional upgrade. So: always two reviewers, pinned to the best available models at high
effort — a **second Claude model** fills the other slot when there's no codex, codex
fills it when there is.

## What we want

Two reviewers — **alpha** and **beta** — each pinned to a model + `high` effort. They
run inside today's review loop (review → fix P0/P1 → re-review → until both clean);
**no consensus, no human-approval interposition** — just the existing flow with pinned
models and a slot-named gate.

| Tier | Condition | **alpha** (diverse) | **beta** (Claude anchor) |
|------|-----------|---------------------|--------------------------|
| **1** | git tree **and** has codex | codex (default model), high | Claude Opus 4.8, high |
| **2** | no codex, **or** non-git tree | Claude Sonnet 5, high | Claude Opus 4.8, high |
| **3** | preferred version unavailable | highest Sonnet avail, high | highest Opus avail, high |

Per-slot fallback along each family list (Opus 4.8→4.7→…; Sonnet 5→…). Codex present
but its model unpinnable → alpha degrades to Claude Sonnet (tier 2), not an older GPT.
`SingleClaudeOnly` fires **only when the resolution needs two distinct Claude models**
(tier 2 / non-git) and only one is reachable (near-impossible) — then keep today's
behavior (single Claude reviewer) rather than block. It does **not** apply in tier 1,
where beta is the sole Claude slot and one Claude model is sufficient. It is a **distinct,
gate-recognized single-reviewer state**, *not* a skip: a Claude slot never "skips" (only a
codex slot can, and only for `rate-limited`/`unavailable`).

**Non-git projects (delivers the purpose for non-git no-codex users).** Codex `review`
needs git, so on a non-git tree it cannot be a reviewer at all — today's behavior is
`claude=passed, codex=skipped:no-git`, i.e. **one** reviewer, which fails this task's
whole point for a no-codex non-git user. Under the new model, **a non-git tree always
resolves two Claude slots** (alpha=Sonnet 5, beta=Opus 4.8, both `high`), reviewing the
**working-tree changes** — no codex slot, no skip, two real reviewers. This is
codex-independent: codex can't review non-git, so we simply don't assign it there. The
`skipped:no-git` path is therefore **gone** (it only ever applied to a codex slot that is
now never assigned on non-git). `/review` genuinely requires a git base (its skill runs
`git branch --show-current`), so on a non-git tree the Claude slots degrade to a raw
strong-model structured review of the working tree (same graceful degrade as the
no-gstack path) — still two models at high effort.

**Scoping the non-git review input.** With no `git diff` to enumerate "what the worker
changed," the degrade reviewer needs a defined input, or it reviews the wrong surface.
**v1 rule (no new state):** the non-git reviewer reviews the **whole project tree against
the task's acceptance criteria**, with a stated **file-count budget** so a large tree
can't blow the round. A tighter **touched-file manifest** (worker records the files it
edited; reviewer scopes to them) is a **v1.x follow-up** — it needs a new `State` field +
`fleet workers update` flag + test, scope this cut deliberately avoids.

## How the two reviewers actually run

Today the reviewer subagent runs `/review` in itself and **shells out** to
`codex review`. This keeps that shape — the reviewer subagent orchestrates and applies
fixes, and each round runs the two **pinned** reviewers via a small helper, waits, and
collects their P0/P1 findings:

```
reviewer subagent (orchestrator + fixer), each round, per slot:
  python3 skills/coordinator/review_slot.py --engine <E> --model <M> --effort high [--base <ref>]
     ├─ engine=codex : run `codex review --base <ref> --config model_reasoning_effort='"high"'`,
     │                   parse codex's [P0]/[P1] markers   (--base omitted on non-git)
     └─ engine=claude: run `claude -p --model <M> --effort high
                             --output-format json --json-schema <S> "/review the diff"`,
                          unwrap envelope → .result → validate {clean, findings[{severity,…}]}
     → retry ≤2 on parse/validation failure; on exhaustion exit 3 (BLOCKED)
     → exit 0 = no P0/P1  ·  exit 1 = P0/P1 findings(JSON on stdout)  ·  exit 3 = blocked
  orchestrator contract (tested): exit 0 → record slot passed · exit 1 → apply P0/P1 fixes,
                re-run · exit 3 → write phase=blocked(+reason), never review-done.
                Loop until BOTH slots exit 0 → record review_alpha/beta_status (+engine/model).
```

- Both invocations are **synchronous subprocess calls** — exactly like today's
  `codex review` shell-out. No detached sessions, no heartbeat, no new process-runner.
- `--effort high` is a real `claude` CLI flag; `--config model_reasoning_effort` is real
  on codex (both verified on `claude` 2.1.209). The Agent-*tool* has no effort knob,
  which is why the pinned reviewers are CLI shell-outs, not Agent subagents.
- **Parse contract has a code owner (`review_slot.py`), not prompt prose.** The prior
  design left envelope-unwrap + bounded-retry to LLM instructions, which isn't a testable,
  deterministic interface. `review_slot.py` owns it:
  1. **Envelope unwrap.** `claude -p --output-format json` emits a session *envelope* —
     `{ type, subtype, result, session_id, … }` — with the model's final answer in
     `.result`, NOT the finding shape directly. The helper unmarshals the envelope, reads
     `.result`, then validates the inner `{clean, findings[]}` against the schema.
     `--json-schema` is **required** (without it `.result` is a free-form `/review`
     markdown checklist and validation fails every round).
  2. **Bounded retry in code.** A parse/validation failure retries the shell-out at most
     **N=2** times inside the helper; on exhaustion the helper exits `3` (BLOCKED) and the
     orchestrator writes `phase=blocked` with a `blocked_reason` (`PhaseBlocked` already
     exists and requires a reason — workers.go). Termination is guaranteed by the helper,
     not trusted to the LLM.
  3. **`clean` means no P0/P1, not "findings empty".** The inner schema carries
     `findings[].severity`; the helper exits `0` when there are no `P0`/`P1` findings
     (any `P2`/`P3` are logged, not blocking). Without this the *outer* review loop would
     never converge if a reviewer keeps emitting an unfixed P2 — exit 0 is the outer
     loop's only bound.
  - **Skill-composition risk (flagged):** `/review` is a Skill, not natively a
    JSON-protocol CLI. If a `/review` run can't be wrapped by `--json-schema`, the slot
    degrades to a raw strong-model structured review (model runs the review logic and
    emits the schema directly, no skill checklist) — same graceful degrade as the
    no-gstack path.
- Fixes apply **all** P0/P1 either reviewer reports (today's behavior); no cross-reviewer
  consensus filtering in v1. The LLM still owns the *fix* judgment between rounds; the
  per-slot run+parse+retry is deterministic code.

## The gate (slot-named, single rule — no legacy branch)

Clean-rename the persisted review gate **fields** from engine-named to slot-named:
`review_claude_status`/`review_codex_status` → `review_alpha_status`/`review_beta_status`
(+ `*_skip_reason`, `*_rounds`, and per-slot `*_engine`/`*_model`). **The old fields are
deleted, not retained** (operator: delete the old flow). There is **one** gate rule — no
legacy branch, no `schema_version`, no field-presence branching:

```
validateReviewGate(s, gitMode):
  if s.Phase != gate: return nil          # unchanged: fires only at terminal write
                                          #   (gate = push for git, done for non-git)
  if alpha empty OR beta empty: reject    # fail-closed (review still required)
  for slot in {alpha, beta}:              # a passed/skipped slot must carry its identity
      require slot.engine ∈ {codex, claude} AND slot.model != ""

  # beta is ALWAYS the Claude anchor: always Claude, always a real pass, never skips.
  # Pinning it guarantees ≥1 real review no matter what alpha does — the gate's whole point.
  require beta.engine == claude AND beta.status == passed

  # Branch on the PERSISTED alpha.status — the gate validates a record, not the resolver's
  # in-memory flag, so it needs no external SingleClaudeOnly input.
  if alpha.status == single-claude-degraded:   # near-impossible two-Claude degrade
      require alpha.engine == claude
  else:                                    # alpha is the only variable slot: pass, or a legal codex skip
      require alpha.status == passed
           OR (alpha.status == skipped AND alpha.engine == codex AND gitMode
               AND alpha.reason ∈ {rate-limited, unavailable})
```

The skip is part of the predicate, not a side comment: a tier-1 codex **alpha** that
rate-limits records `alpha=skipped` and still passes because `beta` (Claude) passed. Only
`alpha` may skip; `beta` cannot — so "both slots skipped" can never satisfy the gate, and
a codex masquerading as the beta anchor is rejected.

- **Slot identity required on a passing slot (P1-c).** The rule checks status *and* that
  each non-empty slot carries `engine ∈ {codex,claude}` + a non-empty model. Status-only
  would let a malformed `alpha=passed,beta=passed` with empty engine/model satisfy the
  gate — the same "don't trust upstream" stance the skip rule takes.
- **Skip legality by engine AND git-mode:** a slot may be `skipped` (reason
  `rate-limited`/`unavailable`) only when `*_engine == codex` **and `gitMode == true`**. A
  Claude slot with `skipped` is a hard invalid; a codex-engine skip on a non-git tree is
  a hard invalid — the gate does not trust the resolver's "never codex on non-git"
  invariant. `no-git` is **removed** from `allowedCodexSkipReasons` entirely.
- **The CLI is a *second* gate — key its skip-legality on engine value, not flag name.**
  `cmd/fleet/workers.go` validates `fleet workers update` flags *before* the record
  reaches `validateReviewGate`. Today that check keys on the flag *name* (the claude flag
  rejects `skipped`, the codex flag allows it). After the slot rename a slot is
  engine-agnostic, so the CLI skip-legality must read the new `--review-{alpha,beta}-engine`
  **value** (skip legal only when that slot's engine is codex). A literal flag-rename that
  kept the old flag-name logic would **wedge a tier-1 codex user whose codex rate-limits**
  — it could no longer record `--review-alpha-status skipped`. The CLI status allow-list
  must also accept `single-claude-degraded`. Share **one** Go definition of the *non-empty
  status set* (see the contract table's shared-def note) — the CLI and `validReviewStatus`
  keep their own empty-string handling, which differs deliberately.
- **`SingleClaudeOnly` degrade:** when the resolver forms only one distinct Claude
  reviewer, the record carries `alpha_status = single-claude-degraded` (a distinct status
  value, *not* `skipped`; add it to the `validReviewStatus` allow-list) +
  `beta_status = passed`; the gate accepts that exact shape. The **alpha** slot always
  carries the degrade.

**In-flight records at the upgrade boundary (fail-closed, operator-recovered — not
auto-healing).** A clean rename means an old on-disk record (old field names) no longer
unmarshals into a gate-satisfying shape, so the gate **fail-closes** — it never ships
unreviewed code, which is the property that matters. But be precise about what happens
next: a record already at `phase=review-done` gets a **finisher** dispatch (not a worker
re-dispatch through `phase=starting`), so the reset that clears review fields never runs
and nothing repopulates the slots. The finisher's terminal write hits the new gate, reads
empty alpha/beta, and is rejected. **Resolution:** the finisher, on gate-reject, writes
`phase=blocked(+reason)` (surface for the operator, never loop) — the operator drains
in-flight workers before an upgrade, or re-dispatches the blocked one to re-review under
the new gate. This is the operator's accepted cost for deleting the compat flow; it does
**not** silently auto-recover. No migration, no grandfather, no `schema_version`.

**Phases are NOT renamed.** The progress phases `PhaseReviewClaude = "review-claude"`
and `PhaseReviewCodex = "review-codex"` (workers.go) stay verbatim — `lifecycle.go` and
the dashboard key off those strings, and renaming them breaks the UI/lifecycle mapping.
Only the *gate fields* change name; the phase constants do not. Flow is otherwise
unchanged: gate satisfied → `review-done` → finisher → PR (existing merge policy).

**Phase ↔ slot mapping is cosmetic, not semantic.** The two phase strings are progress
labels for the two review stages, decoupled from which engine fills a slot. The reviewer
emits `review-claude` then `review-codex` as its two stages regardless of whether alpha
is codex (tier 1) or a second Claude (tier 2 / non-git). This is a telemetry label, not a
correctness signal (the gate reads the fields, never the phase); renaming it is out of
scope.

## Implementation detail (for engineers)

**Resolver — Python (`skills/coordinator/reviewcfg.py`), pure + pytest-tested.** The
resolver's *only* consumer is the reviewer-prompt builder, which is Python
(`dispatch.py`). Putting the resolver in Python makes it a plain import — no Go↔Python
bridge, no `fleet review-slots` command, no duplicated constants. Model IDs + ordered
fallback lists live here.

```python
# pure: availability is an input (the gc_test.go:412 lesson), no exec/env inside
def resolve_slots(has_codex: bool, is_git: bool, unavailable: set[str]) -> Resolution
# Resolution = {alpha: Slot, beta: Slot, single_claude_only: bool}
# Slot = {engine, model, effort="high"}
```

`is_git=False` forces both slots to Claude (tier 2) regardless of `has_codex`. The
**caller wiring is a named deliverable, not incidental:** `_dispatch_review_handoffs`
(`loop.py:~7607`) today threads only `is_git`; it must also compute `has_codex` (an
`exec.LookPath`-style probe cached on `coord-state.json` for the session) and
`unavailable`, call `resolve_slots(...)`, and pass the resolution into
`build_reviewer_prompt`. `is_git` comes from the existing `dispatch_mod.project_is_git()`
(reads `meta.json`) — **not** a fresh `git rev-parse`. A pinned model a slot proves
unavailable is added to `unavailable` and re-resolved once; family exhausted → Claude
Opus/Sonnet floor.

**Codex model string** — v1 leaves codex's **default model** (effort still high); the
GPT-5.5 string is a one-line constant to set once confirmed.

**Reviewer prompt (`skills/coordinator/dispatch.py`)** — `build_reviewer_prompt` imports
`reviewcfg.resolve_slots`, threads the resolved `{alpha, beta}` into the prompt, and
replaces the single `/review` + `codex review` block with the two `review_slot.py` calls
above; keep the fix-and-loop-until-both-clean logic. **Thread the review base:** on a git
tree the prompt passes `--base <ref>` (today's `--base origin/main`, dispatch.py:627) into
each `review_slot.py` call; on non-git it omits `--base` (whole-tree review).

**Blast radius (clean rename — old flow deleted, verified against code):**

- `internal/workers/workers.go` — **rename** the `State` gate fields
  `ReviewClaude*`/`ReviewCodex*` → `ReviewAlpha*`/`ReviewBeta*` (+ `*_engine`/`*_model`);
  rewrite `validateReviewGate` to the single rule above (incl. the slot-identity check);
  add `single-claude-degraded` to `validReviewStatus`; **remove `no-git`** from
  `allowedCodexSkipReasons`.
- `cmd/fleet/workers.go` — **rename** the `--review-*-status`/`-rounds`/`-skip-reason`
  flags (`~:359`, validation `~:464`) to slot names; add `--review-{alpha,beta}-engine`/
  `-model`; delete the old flags (no aliases). **Rework the CLI validation gate
  (`~:518-556`):** move skip-legality from the flag name to the `--review-*-engine` value,
  and accept `single-claude-degraded` in the status allow-list (shared Go def). The
  re-dispatch **reset block** (`~:581-587`) must clear the renamed status/rounds/skip
  fields **and** the new `*_engine`/`*_model` on `phase=starting`.
- `skills/coordinator/dispatch.py` — TWO consumers:
  - The **reviewer** prompt: rewrite the **entire `build_reviewer_prompt`, both git and
    non-git branches** — the renamed `--review-*-status` flags appear across terminal
    writes (git `~:641`, non-git `~:704-706`), mid-loop `iterating` nudges (git
    `~:623`/`:637`, non-git `~:694`), and the reviewer-contract docstring (`~:473-485`).
  - The **finisher**'s field reads — git finisher jq (`~:838`), non-git finisher jq
    (`~:913`), PR-body text (`~:846-847`, `:826`, `:898`, `:914`).
- `skills/coordinator/reviewcfg.py` (new) + `skills/coordinator/review_slot.py` (new) +
  their pytest suites.
- `skills/coordinator/SKILL.md` — the "Non-git Projects" / reviewer prose still documents
  `/review + codex` and `skipped:no-git`; update to the two-slot model.
- **Tests — delete vs keep vs rewrite (be precise, not "delete all"):**
  - **DELETE** the engine-field-specific gate/skip cases (`workers_test.go` gate/skip
    block, `cmd/fleet/workers_test.go` flag tests incl. the now-inverted `no-git accepted`
    case) — subsumed by the rename.
  - **KEEP** the phase-constant validity test (asserts `review-claude`/`review-codex` are
    valid phases) — those strings survive the rename untouched.
  - **REWRITE** the invariant tests that survive but reference old fields: gate does *not*
    fire on `review-done`, git `done` needs only a PR URL, non-git `done` is review-gated,
    non-git `push` is rejected — re-express against the slot fields.
  - The PR lists exactly which cases were deleted vs rewritten.
- **No change:** `lifecycle.go`/`dashboard.go` read only the phase constants
  `review-claude`/`review-codex` (and `review-pending`/`review-done`), never the gate
  fields.

## Dependency: `/review` is a gstack skill, not fleet-embedded

The Claude reviewer shell-out runs `claude -p "/review …"`. `/review` is a **gstack**
skill installed in the user's `~/.claude/skills/`, not bundled with the fleet binary. A
no-codex user who *also* lacks gstack gets a plain strong-model review (still two models
at high effort, without the house-style `/review` checklist). **This does not regress vs
today** — today's reviewer has the identical `/review` dependency. The long-term fix is
the pending **first-party `fleet-review` skill** (own design doc); until then the
shell-out degrades gracefully to a raw model review.

## Cross-language string contract (one source of truth per side)

The Python resolver/prompt and the Go gate/CLI share a small string contract. Keep each
side's constants in one place; a rename on one side must break a test on the other.

| Field | Values |
|-------|--------|
| engine | `codex`, `claude` |
| status | `pending`, `iterating`, `passed`, `skipped`, `blocked`, `single-claude-degraded` |
| codex skip reason | `rate-limited`, `unavailable` (no `no-git`) |
| helper exit | `0` no P0/P1 · `1` P0/P1 findings · `3` blocked |

**Shared Go def = the non-empty status *set* (enum membership), not empty-handling.** The
two current validators differ *deliberately* on the empty string: `validReviewStatus`
(workers.go) treats `""` as **valid** (it runs on every write, including early phases
where review fields are legitimately empty); the CLI's `workersReviewStatusValid`
(cmd/fleet/workers.go) treats `""` as **invalid** (the CLI already gates on the flag being
set). Share only the non-empty value set — **including `skipped`** — and let each caller
keep its own empty-string rule, or a naive merge re-wedges the codex rate-limit path
(gate/CLI would reject a legitimate `skipped`).

## Test plan (consolidated, TDD-driven)

**TDD:** for each deliverable, write the table (red) first, then implement to green. Keep
to **five** table-driven suites — do not add per-case functions; a new scenario is a new
**row**, not a new test. Budget ~1.5× prod LOC; reviewers flag added boilerplate as P2.

**S1 — `reviewcfg` resolver matrix** (pytest, one param table). Rows: codex+git → codex/Opus;
codex+non-git → Sonnet/Opus (no codex); no-codex → Sonnet/Opus; Opus unavailable → next
Opus; codex model unpinnable → Sonnet; one-Claude-only → `single_claude_only`. Plus a
purity assert (run 2× identical; module imports no `subprocess`/`os.environ`).

**S2 — `validateReviewGate` matrix** (Go, one gitMode-parameterized table). Rows (each =
state → accept/reject): both passed → accept; one pending → reject; empty slots → reject
(fail-closed); passed slot with empty engine/model → reject; **invalid non-empty engine
(`gpt`) → reject**; codex-engine `skipped`+git+`rate-limited` → accept; Claude-engine
`skipped` → reject; codex-engine `skipped` on non-git → reject; skip reason ∈
{`no-git`,``,garbage} → reject; `single-claude-degraded` (claude) alpha + passed beta → accept;
`skipped` alpha in the SingleClaudeOnly slot → reject; **both slots codex+skipped+git+rate-limited
→ reject** (beta must be a Claude pass — ≥1 real review); **beta engine=codex → reject**
(anchor must be Claude); **single-claude-degraded with engine=codex → reject**. **Plus the
four rewritten surviving invariants** (each its own row): gate does not fire off the
terminal phase; git `done` needs only a PR URL; non-git `done` is review-gated; non-git
`push` is rejected (`ErrPhasePushNonGit`).

**S3 — CLI flag validation** (Go, one table). Rows: `--review-alpha-status
single-claude-degraded` → accepted; codex-engine slot `skipped` → accepted; claude-engine
slot `skipped` → rejected; **skip-reason set without `status=skipped` → rejected** (the
surviving CLI invariant); reset on `phase=starting` clears status/rounds/skip **and**
engine/model. (Proves skip-legality keys on engine value, not flag name — the tier-1
codex rate-limit path.)

**S4 — `review_slot.py`** (pytest, stubbed shell-out via a **PATH shim**, not a prod
flag). Rows: claude envelope → reads `.result` → inner shape → exit 0; unparseable ×3 →
≤2 retries → exit 3 (termination); codex `[P0]/[P1]` markers → findings, exit 1; findings
with only P2/P3 → exit 0 (clean = no P0/P1); **git slot command includes `--base <ref>`,
non-git omits it**.

**S5 — reviewer prompt / dispatch** (pytest). Rows: `build_reviewer_prompt` threads the
resolved slots + emits `--effort high` / `model_reasoning_effort=high` **+ `--base` on
git / no base on non-git** for both; `_dispatch_review_handoffs` computes+passes
`has_codex`/`is_git`/`unavailable` (codex+git vs no-codex/non-git produce different
prompts); orchestrator contract — exit 0 records passed, exit 1 fixes, exit 3 →
`phase=blocked`; **finisher on gate-reject (empty slots at the terminal write) emits
`phase=blocked`, not a loop**; finisher reads slot fields (PR body names both reviewers,
jq not null).

Old engine-field gate/skip/flag tests are **deleted** (§blast radius); the phase-constant
validity test is **kept**; surviving invariants are **rewritten** into S2/S3.

## Non-goals (v1)

- **Backward-compat / migration of in-flight records** — clean rename; the old fields,
  gate branch, CLI flags, and their tests are **deleted**, not preserved.
- **Consensus / exchange / agreed-set** between the reviewers — future design.
- **Human-approval-before-PR** — the existing merge policy is unchanged.
- **A new worker phase, headless process-runner, TUI/lifecycle changes** — none needed.
- **Touched-file manifest for non-git scoping** — v1.x follow-up.
- Operator-configurable model lists / effort (future `~/.fleet/config.yaml`).
- Changing worker/finisher engine selection.
