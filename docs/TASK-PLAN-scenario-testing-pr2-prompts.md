# TASK PLAN — scenario-testing PR-2: Scenario Contract + reproduce/encode/verify prompts

- **Task:** `scenario-testing-pr2-prompts` · **Priority:** P1 · **PR-base:** `main`
- **Design:** `docs/DESIGN-scenario-first-testing.md` §Lever 1–3, §"Verification doc",
  §"Finisher PR body", §"Reviewer lens", §"Standards ## Testing"
- **Depends-on:** PR-1 merged **and** the operator's binary rebuilt (`fleet skills status`
  clean). The prompts emit `--phase spec-repro|spec-encode|verify` and the three
  verification flags; an old binary rejects them and wedges the worker at its first
  phase write.
- **Status:** DRAFT — pending dual plan-review + operator promote.

---

## TL;DR

This is the PR that changes what fleet workers test. Python + markdown only.

```
worker prompt        2a Write the failing test           →  2a REPRODUCE  build fleet, stand up each S in a sandbox, capture the wrong outcome
                     2b Minimal impl, test passes        →  2b ENCODE     test at S's level asserting the observed outcome; must fail now
                     2c Refactor                         →  2c IMPLEMENT + VERIFY  fix, rerun by hand + test, full ci.yml gates, evidence, fields
task plan (step 5)   "one line per test"                 →  Scenario Contract table (S | requirement | stand-up | trigger | observable | level | harness)
reviewer prompt      alpha/beta slots                    →  + contract lens (every S has a test at its level asserting the observable); re-run gates after fixes
finisher prompt      "- [ ] CI green / - [ ] verify"     →  cat verification.md into ## Verification; blocked if missing
standards ## Testing "TDD required …"                    →  scenario-first text (design, verbatim)
```

## Deliverables

| # | Deliverable | Surface |
|---|-------------|---------|
| D1 | `## Testing` replaced with the design's verbatim text | `templates/standards.md` |
| D2 | Step 5: Scenario Contract table + rules (observable outcome, level default e2e, unit needs reason, grouping decided here, `none — <reason>` escape); writing-standard item 5 reworded; step 6 rules gain the worker/reviewer/finisher evidence duties | `skills/coordinator/SKILL.md` |
| D3 | §5/§6 mirror of D2 for the operator-facing doc | `docs/COORDINATOR-WORKFLOW.md` |
| D4 | `build_worker_prompt`: steps 2a–2c rewritten (git + non-git variants), `verification.md` path + section list, sandbox env, `docs/TESTING.md` pointer, gate list identical to `ci.yml`, baseline rule, "red gate → no review-pending", final `fleet workers update … --scenarios-total M --scenarios-verified N --gates-status passed` before `--phase review-pending` | `skills/coordinator/dispatch.py` |
| D5 | `build_reviewer_prompt`: contract lens in step 2 (git) and in `--task-context` (non-git); "after any fix: re-run touched scenario tests + full gates, append `## Review re-verification`" before the terminal write | `skills/coordinator/dispatch.py` |
| D6 | `build_finisher_prompt`: step 3 reads `verification.md`; PR body `## Test plan` placeholder → `## Verification` with the file's sections verbatim; missing file / empty `## Gates` or `## Evidence — after` → `phase=blocked`; non-git finisher writes the same block into the done note | `skills/coordinator/dispatch.py` |
| D7 | `docs/TESTING.md` (≤80 lines): build to `$FLEET_DEV_BIN`; `FLEET_HOME=$(mktemp -d)`; `FLEET_TMUX_SOCKET=fleet-test-<slug>-$$`; `coorde2e.FakeClaude` modes; seed via `coorde2e.SeedProject` / `SeedInFlightWorker`; trigger; `tmux -L "$FLEET_TMUX_SOCKET" capture-pane -p`; teardown; where `//go:build integration` tests live and how the lane picks them up (until PR-3: add to the `-run` list) | `docs/TESTING.md` |
| D8 | Rendered `.html` for D2's parent docs if the renderer is configured (per SKILL step 2/5) | `docs/` |

### D4 — worker prompt text (the load-bearing part)

```
  fleet workers update <slug> --project <p> --phase spec-repro
2a. REPRODUCE. `go build -o "$FLEET_DEV_BIN" ./cmd/fleet` (FLEET_DEV_BIN=$(mktemp -d)/fleet).
    For every e2e/integ row in the task plan's Scenario contract: stand it up in a
    sandbox — FLEET_HOME=$(mktemp -d) FLEET_TMUX_SOCKET=fleet-test-<slug>-$$ — with the
    fake claude (internal/testutil/coorde2e; see docs/TESTING.md). Run the trigger.
    Capture the wrong outcome VERBATIM (tmux capture-pane -p, file contents, exit code)
    into ~/.fleet/projects/<p>/workers/<slug>/verification.md under "## Evidence — before".
    Cannot reproduce → `fleet workers update <slug> --phase blocked --reason "spec
    mismatch: S<n> <what you saw instead>"` and exit. Do not guess. git commit.

  fleet workers update <slug> --project <p> --phase spec-encode
2b. ENCODE. One test per row (grouped as the plan says) at the row's level, asserting
    the SAME observable you captured:
      e2e   → //go:build integration in cmd/fleet or internal/tui, driving $FLEET_DEV_BIN
              + real tmux via coorde2e; add its name to that package's -run list in
              .github/workflows/ci.yml (until the lane discovers tests itself, an
              unlisted test never runs)
      integ → package test at a real boundary (real FS, real flock, tmuxfake)
      unit  → only where the contract says unit
    Run it: it MUST fail for the reason in "Evidence — before". A test that passes before
    the fix tests nothing. git commit.

  fleet workers update <slug> --project <p> --phase verify
2c. IMPLEMENT + VERIFY. Fix. Re-run every scenario by hand AND via its test; record
    "## Evidence — after" (same commands, verbatim output, PASS). Then the FULL gates,
    exactly as ci.yml: go build ./... · gofmt -l . · golangci-lint run ./... ·
    go test -race -count=1 -timeout=5m ./... · FLEET_STANDBY_TIMEOUT=3s go test
    -tags=integration -count=1 -timeout=5m -run '^(<your e2e tests>)$' ./<pkg> ·
    python3 -m pytest skills/ scripts/ -q · bash scripts/lint-test-isolation.sh.
    Record each under "## Gates" (command + result line). Any red you believe is
    pre-existing: re-run that exact command on origin/main in `git worktree add
    /tmp/fleet-base-<slug> origin/main`; record both under "## Baseline". Red gate that
    is yours → fix it; do NOT flip review-pending. git commit. Then:
      fleet workers update <slug> --project <p> --scenarios-total M --scenarios-verified N \
        --gates-status passed
    (M = rows in the contract, N = rows with PASS under "Evidence — after"; M=0 only when
    the contract says `none`.)

  fleet workers update <slug> --project <p> --phase review-pending
3.  (unchanged)
```

Non-git variant: same text; "git commit" lines dropped; e2e means the project's own run
command named in the contract row.

## Scenario contract

| S | Requirement | Stand-up | Trigger | Observable outcome | Level | Harness |
|---|-------------|----------|---------|--------------------|-------|---------|
| S1 | WHEN the coord dispatches a worker THE prompt SHALL carry the 2a/2b/2c text, the verification.md path, the full gate list, and the three flags — and SHALL NOT contain `tdd-` or `Write the failing test` | `parse.Task` with spec/acceptance; `standards_md` = new template | `build_worker_prompt(task, …)` git and non-git | prompt contains each of: `--phase spec-repro`, `--phase spec-encode`, `--phase verify`, `verification.md`, `Evidence — before`, `go test -race -count=1 -timeout=5m ./...`, `--gates-status passed`, `docs/TESTING.md`; contains none of `tdd-`, `Write the failing test`; non-git variant has no `git commit` | integ | pytest `test_dispatch.py` (prompt is the boundary the worker sees) |
| S2 | WHEN a worker is dispatched by the real coord loop THE prompt written to disk SHALL be the S1 prompt | sandbox `FLEET_HOME`, seeded project + promoted task, `loop.tick` with the spawn stubbed to capture argv/prompt (existing `test_loop` harness) | one tick | captured prompt satisfies the S1 assertions | integ | existing `test_loop` dispatch harness |
| S3 | WHEN the reviewer is dispatched THE prompt SHALL carry the contract lens and the re-verify-after-fix rule | task with a Scenario contract in its plan text | `build_reviewer_prompt` git + non-git | contains `Scenario contract`, `observable outcome`, `re-run`, `## Review re-verification`; non-git `--task-context` includes the contract | integ | pytest |
| S4 | WHEN the finisher is dispatched THE prompt SHALL read verification.md into the PR body and SHALL NOT contain the placeholder | — | `build_finisher_prompt` git + non-git | contains `verification.md`, `## Verification`, `no verification evidence`; contains neither `- [ ] CI green` nor `- [ ] verify locally` | integ | pytest |
| S5 | WHEN a finisher runs with an empty `## Gates` THE workflow SHALL block, not open a PR | sandbox `FLEET_HOME`, `state.json` at review-done, `verification.md` with empty `## Gates`, PATH-shim `gh` + `git` that record calls | drive the finisher prompt's step 3 shell snippet (the prompt embeds a deterministic `awk`/`sed` check; test executes that snippet) | `gh pr create` never called; `fleet workers update --phase blocked --reason "finisher: no verification evidence…"` called | e2e | bash snippet extracted from the prompt, shims on PATH |
| S6 | WHEN the standards template is merged THE `## Testing` section SHALL be the design text and `fleet standards show --merged` SHALL print it | built binary, sandbox `FLEET_HOME` after `fleet init` | `fleet standards show --merged` | output contains `Scenario-first, not test-first`; does not contain `TDD required` | e2e | built binary |
| S7 | WHEN the SKILL is read THE step-5 section SHALL contain the Scenario Contract header row and rules | `skills/coordinator/SKILL.md` | read | contains `## Scenario contract`, `Level defaults to`, `none — <reason>`; writing-standard item 5 no longer says `one line per test` | unit | `test_skill_md.py` |
| S8 | Dogfood: the first real task dispatched after merge produces a PR whose body has `## Evidence — after` and `## Gates` with commands | live coord on `main` after rebuild | operator promotes one small P2 bug with a one-row contract | PR body sections present; `state.json.gates_status=passed` | e2e (manual, recorded in this task's verification.md `## Baseline` as "post-merge dogfood") | live |

S1/S3/S4 are rows of one parametrized pytest (`build_*_prompt` × git/non-git); S2 extends
the existing dispatch-capture test; S5 is one bash-driven pytest; S6 one Go CLI test row.

## Tests removed / KEEP

- **Tests removed:** `test_dispatch.py` assertions pinning `2a. Write the failing test` /
  `- [ ] verify locally` if any exist at implementation time (a grep today finds only the
  phase list at `test_dispatch.py:50`, which is a KEEP).
- **KEEP:** every reviewer/finisher prompt test not about the test plan text (review
  slot commands, `--base`, worktree `cd`, gate reject → blocked).

## Acceptance

1. Full gates green (build, gofmt, golangci-lint, `go test -race`, pytest `skills/ scripts/`).
2. S1–S7 pass; S8 recorded after merge.
3. `grep -rn 'tdd-red\|Write the failing test\|verify locally' skills/coordinator/dispatch.py templates/standards.md skills/coordinator/SKILL.md docs/COORDINATOR-WORKFLOW.md` → no matches.
4. `docs/TESTING.md` ≤80 lines and every command in it was executed once by the worker
   (recorded under `## Evidence — after`).

## Non-goals

- Enforcing the fields (PR-3). Until then `--gates-status` is prompt-mandated only.
- `scripts/scenario.sh` / pytest fixture (PR-4); `docs/TESTING.md` points at `coorde2e`
  directly for now.
- Changing review slot resolution or the alpha/beta gate.
