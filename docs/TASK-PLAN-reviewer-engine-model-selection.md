# TASK PLAN — reviewer engine/model selection (two-reviewer gate)

- **Task:** `reviewer-engine-model-se-f756` · **Priority:** P1 · **PR-base:** `main`
- **Design:** `docs/DESIGN-reviewer-engine-model-selection.md` (rev13 — pending operator re-approval)
- **Depends-on:** none · **Scope:** one PR, one business unit
- **Status:** DUAL-REVIEW CLEAN (rev6 — both reviewers SOUND, no P0/P1) — **pending operator promote gate**

---

## TL;DR

Make the review gate run **two** reviewers instead of one for no-codex users. A pure
**Python** resolver picks `{alpha, beta}` slots (codex+Opus / Sonnet+Opus / two-Claude on
non-git); a small **Python** helper (`review_slot.py`) owns each slot's shell-out + parse
+ bounded retry; the Go gate is **clean-renamed** to slot-named fields (old fields, gate
branch, flags, and tests **deleted** — no compat). No new phase, no consensus, no
lifecycle/TUI change.

```
                       BEFORE                                 AFTER
 reviewer subagent: /review (1 Claude)             review_slot.py alpha ─┐
                    + codex review (skipped          review_slot.py beta ─┴─ both exit 0
                      if no codex)                       → gate
 gate fields:       review_claude_status            review_alpha_status  (old DELETED)
                    review_codex_status              review_beta_status (+_engine/_model)
 resolver:          (none — hardcoded)              skills/coordinator/reviewcfg.py (Python, pure)
 no-codex / non-git: 1 reviewer                     2 reviewers  ✅
```

**Operator directive folded:** clean rename, **delete** the old flow + its tests (no
legacy branch, no `schema_version`, no flag aliases, no grandfather/straddle). In-flight
records at an upgrade boundary fail-closed and re-review via re-dispatch.

**Dual-review P1s folded:** resolver moved to Python (kills the Go↔Python bridge gap);
`review_slot.py` gives the parse/bounded-retry contract a real, testable code owner.

## Deliverables (each maps to a test suite)

| # | Deliverable | Surface | Suite |
|---|-------------|---------|-------|
| D1 | Pure resolver (Python) | `skills/coordinator/reviewcfg.py` (new) | S1 |
| D2 | Gate: single rule + slot-identity + git-aware skip | `internal/workers/workers.go` | S2 |
| D3 | Clean rename + CLI validation rework | `internal/workers/workers.go`, `cmd/fleet/workers.go` | S2, S3 |
| D4 | Per-slot run+parse+retry helper (exit 0=no P0/P1) | `skills/coordinator/review_slot.py` (new) | S4 |
| D5 | Prompt import resolver + caller wiring + orchestrator contract | `skills/coordinator/dispatch.py`, `loop.py` | S5 |
| D6 | Finisher jq + PR-body read slot fields | `skills/coordinator/dispatch.py` | S5 |
| D7 | SKILL.md prose → two-slot model | `skills/coordinator/SKILL.md` | doc review |

## D1 — Resolver (`skills/coordinator/reviewcfg.py`, pure)

```python
def resolve_slots(has_codex: bool, is_git: bool, unavailable: set[str]) -> Resolution
# Resolution = {alpha: Slot, beta: Slot, single_claude_only: bool}
# Slot = {engine: "codex"|"claude", model: str, effort: "high"}
```

Rules (design §"What we want"): tier 1 `is_git and has_codex` → alpha=codex(default
model), beta=Opus 4.8. Tier 2 `not has_codex or not is_git` → alpha=Sonnet 5, beta=Opus
4.8. Tier 3 a pinned model in `unavailable` → next in that family's fallback list.
`is_git=False` forces tier 2 regardless of `has_codex`. `single_claude_only=True` **only
in the two-Claude tiers** (tier 2 / non-git) when just one distinct Claude model is
reachable — never in tier 1, where beta is the sole Claude slot. **Pure:** no subprocess,
no env reads — `has_codex`/`is_git` are inputs. Model constants + fallback lists live in
this module.

**Why Python (not Go):** the resolver's sole consumer is `build_reviewer_prompt`
(Python). A Python module is a plain import — no `fleet review-slots` CLI, no Go↔Python
duplication. Caller (`loop.py`) computes inputs: `has_codex` from a cached probe on
`coord-state.json`; `is_git` from the existing `dispatch_mod.project_is_git()` (reads
`meta.json`) — **not** a fresh `git rev-parse`.

## D2/D3 — Gate + clean rename (`internal/workers/workers.go`, `cmd/fleet/workers.go`)

Rename gate fields (old **deleted**, no retention):

```
review_claude_status/_rounds        → review_alpha_status/_rounds/_skip_reason/_engine/_model
review_codex_status/_rounds/_skip.. → review_beta_status /_rounds/_skip_reason/_engine/_model
```

`validateReviewGate(s, gitMode)` — ONE rule, no legacy branch, no schema_version:

```
if s.Phase != gate: return nil            # gate = push (git) | done (non-git); unchanged
if alpha empty OR beta empty: reject       # fail-closed
for slot in {alpha,beta}: require engine∈{codex,claude} AND model!=""
require beta.engine==claude AND beta.status==passed   # anchor: always Claude, always a real pass, never skips → guarantees ≥1 review
if alpha.status==single-claude-degraded: require alpha.engine==claude   # branch on persisted status, no external flag
else: require alpha.status==passed
           OR (alpha.status==skipped AND alpha.engine==codex AND gitMode AND reason∈{rate-limited,unavailable})
```

- Add `single-claude-degraded` to `validReviewStatus`; **share ONE Go def of the
  non-empty status *set*** (incl. `skipped`) with the CLI check. The two validators keep
  their own **empty-string** rule (`validReviewStatus` `""`=valid for early-phase writes;
  CLI `""`=invalid) — merge only the enum membership, or a codex `skipped` gets rejected.
- **Remove `no-git`** from `allowedCodexSkipReasons` (no codex slot on non-git).
- Re-dispatch reset (`cmd/fleet/workers.go:581-587`, `phase=starting`) clears the renamed
  status/rounds/skip **and** the new `*_engine`/`*_model` (else re-worker inherits stale
  `passed`).
- **CLI validation rework (`cmd/fleet/workers.go:518-556`) — the load-bearing part:** the
  CLI is a *second* gate that today keys skip-legality on the **flag name** (claude-flag
  rejects `skipped`, codex-flag allows). After the slot rename, move that decision to the
  `--review-{alpha,beta}-engine` **value** (skip legal only when the slot's engine is
  codex), and accept `single-claude-degraded` in the CLI status allow-list. Without this a
  **tier-1 codex user whose codex rate-limits cannot record `--review-alpha-status
  skipped` → reviewer wedges.** Add `--review-{alpha,beta}-engine`/`-model` flags; rename
  `:359`/`:464`; delete old flags (no aliases).
- **Finisher-on-gate-reject → `phase=blocked`.** A pre-upgrade `review-done` record whose
  old fields no longer satisfy the gate must have its finisher write `phase=blocked(+reason)`
  (surface for operator drain/re-dispatch), never loop.
- **Tests — delete vs keep vs rewrite:** DELETE the engine-field gate/skip/flag cases
  (incl. the now-inverted `no-git accepted`); **KEEP** the phase-constant validity test
  (`review-claude`/`review-codex` survive); **REWRITE** surviving invariants (gate off
  terminal phase; git `done` needs only PR URL; non-git `done` review-gated; non-git
  `push` rejected). PR lists deleted vs rewritten.

## D4 — `review_slot.py` (per-slot run + parse + bounded retry, Python)

```
python3 review_slot.py --engine <E> --model <M> --effort high [--base <ref>]
  engine=codex : run codex review --base <ref> --config model_reasoning_effort='"high"'; parse [P0]/[P1]
  engine=claude: run claude -p --model <M> --effort high --output-format json
                     --json-schema <S> "/review the diff";
                 unwrap envelope → .result → validate {clean, findings[{severity,...}]}
  --base threaded from build_reviewer_prompt on git (today's origin/main); omitted on non-git
  retry ≤2 on parse/validation failure
  exit: 0 = NO P0/P1 (P2/P3 logged, not blocking) · 1 = P0/P1 findings(JSON stdout) · 3 = blocked
```

Owns the two-layer parse (envelope `.result` → inner schema) and the N=2 bound **in
code** — termination is guaranteed by the helper, not LLM prose. **`exit 0` = no P0/P1**
(not "findings empty"): this bounds the *outer* review loop — otherwise a persistently
unfixed P2 spins forever. The reviewer prompt calls it per slot; a slot exiting `3` →
orchestrator writes `phase=blocked(+reason)`. `--json-schema` required. Tested with a
**PATH-shim stub** (no test-only flag on the prod CLI) so S4 is deterministic.

## D5/D6 — Prompt + caller wiring + finisher (`skills/coordinator/dispatch.py`, `loop.py`)

- **Caller wiring (P1-a):** `_dispatch_review_handoffs` (`loop.py:~7607`) today threads
  only `is_git`; it must compute `has_codex` (cached probe) + `unavailable`, call
  `reviewcfg.resolve_slots`, and pass the resolution into `build_reviewer_prompt`.
- **Prompt:** rewrite the **whole `build_reviewer_prompt`, both git and non-git branches**
  — thread `{alpha, beta}`, replace the `/review`+`codex review` block with the two
  `review_slot.py` calls, keep the loop; **orchestrator contract:** exit 0 records passed,
  exit 1 fixes+re-runs, exit 3 → `phase=blocked`. Non-git: no codex slot; two Claude slots
  review whole tree vs acceptance (file-count budget). Renamed sites: git terminal `:641`,
  non-git terminal `:704-706`, nudges `:623`/`:637`/`:694`, docstring `:473-485`.
- **Finisher:** jq (git `:838`, non-git `:913`) + PR-body (`:826`/`:846-847`/`:898`/`:914`)
  read the slot fields.

## Test plan (5 consolidated suites, TDD-driven)

**TDD:** per deliverable, write the suite's table (red) first, then implement to green.
**Five** table-driven suites — a new scenario is a **row**, never a new function. Shared
builders: pytest `slot(...)`, Go `stateWithReview(...)`. Budget ~1.5× prod LOC; added
per-case boilerplate is a P2. See DESIGN §"Cross-language string contract" for the shared
constants both sides test against.

| Suite | Where | Rows (each a table entry) |
|-------|-------|---------------------------|
| **S1** resolver matrix | pytest `reviewcfg` | codex+git→codex/Opus · codex+non-git→Sonnet/Opus · no-codex→Sonnet/Opus · Opus-unavail→next Opus · codex-model-unpinnable→Sonnet · one-Claude→`single_claude_only` · purity (2×-identical + no `subprocess`/`os.environ`) |
| **S2** gate matrix | Go `internal/workers` (gitMode-param) | both-passed→accept · one-pending→reject · empty→reject · passed-but-empty-engine/model→reject · invalid-engine `gpt`→reject · codex+git+`rate-limited` skip→accept · claude skip→reject · codex skip non-git→reject · reason∈{`no-git`,``,garbage}→reject · `single-claude-degraded`(claude)+passed→accept · skipped-in-single-slot→reject · **both-slots codex+skipped+rate-limited→reject** · beta engine=codex→reject · single-claude-degraded+codex→reject · **4 rewritten invariants:** gate-off-terminal-phase · git-`done`-needs-PR-URL · non-git-`done`-review-gated · non-git-`push`-rejected (`ErrPhasePushNonGit`) |
| **S3** CLI validation | Go `cmd/fleet` | `--review-alpha-status single-claude-degraded`→accepted · codex-engine slot skip→accepted · claude-engine slot skip→rejected · **skip-reason set w/o `status=skipped`→rejected** · reset clears status/rounds/skip **and** engine/model |
| **S4** `review_slot.py` | pytest (PATH-shim stub) | claude envelope→`.result`→inner→exit 0 · unparseable×3→≤2 retries→exit 3 · codex `[P0]/[P1]`→findings, exit 1 · only-P2/P3→exit 0 · **git slot cmd includes `--base <ref>`, non-git omits** |
| **S5** prompt / dispatch | pytest | prompt threads slots + `--effort high` **+ `--base` on git / none on non-git** · `_dispatch_review_handoffs` computes+passes `has_codex`/`is_git`/`unavailable` (codex+git vs no-codex/non-git differ) · orchestrator exit 0/1/3 contract · **finisher on gate-reject (empty slots)→`phase=blocked`, not loop** · finisher reads slot fields (PR body names both, jq not null) |

## Acceptance criteria

1. `go build ./... && go test -race -count=1 ./...` green; `golangci-lint run ./...` clean.
2. `python3 -m pytest skills/coordinator/tests/ -q` green.
3. A no-codex run and a non-git run each record **two** passing slots and reach `review-done`.
4. A **tier-1 codex rate-limit** records `alpha=skipped` and finishes (proves S3 engine-keyed skip).
5. Old `review_claude/codex_*` fields, gate branch, and old CLI flags are gone; PR lists
   deleted vs rewritten tests (phase-constant test kept).
6. A slot unparseable after 2 retries drives `phase=blocked`, not an infinite loop (S4);
   a persistently-unfixed P2 does not spin the outer loop (exit 0 = no P0/P1).

## Non-goals

- **Backward-compat / migration** — clean rename; old flow + tests deleted (operator).
- **Touched-file manifest** for non-git scoping — v1.x follow-up; v1 uses whole-tree+budget.
- Consensus/exchange, human-approval-before-PR, new phase, process-runner, TUI/lifecycle
  changes, operator-configurable model lists.
