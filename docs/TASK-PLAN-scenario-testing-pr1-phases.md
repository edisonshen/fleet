# TASK PLAN — scenario-testing PR-1: phases + verification fields (no gate)

- **Task:** `scenario-testing-pr1-phases` · **Priority:** P1 · **PR-base:** `main`
- **Design:** `docs/DESIGN-scenario-first-testing.md` §"Phase enum", §"State fields + flags"
- **Depends-on:** none. **Blocks:** PR-2 (prompts may not emit the new phases until this is
  merged **and the operator has rebuilt the binary** — `fleet skills status` clean).
- **Status:** DRAFT — pending dual plan-review + operator promote.

---

## TL;DR

Teach the binary the three new worker phases and the three verification fields so the
PR-2 prompts have something to write to. **Additive only:** old `tdd-*` phases stay
accepted, no validation rule changes, no behaviour change for any existing worker.

```
BEFORE                                         AFTER
--phase tdd-red|tdd-green|tdd-refactor         --phase spec-repro|spec-encode|verify   (tdd-* still accepted)
(no verification fields)                       --scenarios-total M --scenarios-verified N --gates-status passed|failed
                                               state.json: scenarios_total, scenarios_verified, gates_status
```

## Deliverables

| # | Deliverable | Surface |
|---|-------------|---------|
| D1 | `PhaseSpecRepro`, `PhaseSpecEncode`, `PhaseVerify` constants; `validPhase` accepts them; `Phase` doc comment reworded (`reproduce → encode → verify → review → push`) | `internal/workers/workers.go` |
| D2 | `State.ScenariosTotal`, `State.ScenariosVerified` (`int`), `State.GatesStatus` (`string`, `omitempty`); `validGatesStatus ∈ {"", passed, failed}`; `verified ≤ total`, both `≥ 0`, enforced in `writeStateLocked` | `internal/workers/workers.go` |
| D3 | CLI: `--scenarios-total`, `--scenarios-verified`, `--gates-status` flags on `fleet workers update`; `--phase` help + usage text list the new phases; `phase=starting` reset block clears the three fields | `cmd/fleet/workers.go` |
| D4 | Lifecycle bucket: `spec-repro`, `spec-encode`, `verify` → `Active` (beside `tdd-*`) | `internal/lifecycle/lifecycle.go` |
| D5 | `_WORKER_AUTHORED_PHASES` gains the three names | `skills/coordinator/loop.py` |
| D6 | `fleet workers show` / dashboard render the new phase strings unchanged (they are plain strings — verify, no code expected) | `internal/tui`, `cmd/fleet` |

## Scenario contract

| S | Requirement | Stand-up | Trigger | Observable outcome | Level | Harness |
|---|-------------|----------|---------|--------------------|-------|---------|
| S1 | WHEN a worker writes each new phase THE CLI SHALL accept it and persist it | built binary, sandbox `FLEET_HOME`, seeded project + worker record | `fleet workers update w --phase spec-repro` then `spec-encode`, `verify` | exit 0; `state.json.phase` equals the value written; `fleet workers show w` prints it | e2e | `FLEET_HOME` sandbox, built binary (`cmd/fleet` test harness already builds it) |
| S2 | WHEN a worker writes the legacy `tdd-*` phases THE CLI SHALL still accept them | S1 | `--phase tdd-red` | exit 0, persisted | e2e | same |
| S3 | WHEN a worker records verification fields THE CLI SHALL persist them and `show` SHALL print them | S1 | `--scenarios-total 3 --scenarios-verified 3 --gates-status passed` | `state.json` carries the three; `show` line `gates: passed (3/3 scenarios)` | e2e | same |
| S4 | WHEN fields are malformed THE CLI SHALL reject before writing | S1 | `--gates-status maybe` · `--scenarios-verified 4 --scenarios-total 3` · `--scenarios-total -1` | non-zero exit, message names the flag; `state.json` unchanged (mtime + content) | e2e | same |
| S5 | WHEN a task is re-dispatched (`phase=starting`) THE reset SHALL clear the three fields | S3 | `--phase starting` | `scenarios_total=0`, `scenarios_verified=0`, `gates_status=""` in `state.json`; review fields also cleared (existing behaviour) | integ | Go `internal/workers` + `cmd/fleet` reset test |
| S6 | WHEN a record predates the fields THE reader SHALL load it with zero values and rewrite without adding noise | old `state.json` fixture without the keys | `ReadState` → `WriteState` | round-trip succeeds; keys absent from output (omitempty) | unit (pure) | — |
| S7 | WHEN the coord sees a worker in a new phase THE lifecycle SHALL classify it Active and the loop SHALL treat it as worker-authored | `lifecycle.Classify` / `loop._WORKER_AUTHORED_PHASES` | classify each of the three | `Active`; membership true | unit (pure) | — |

S1–S4 are rows of **one** Go table test in `cmd/fleet/workers_test.go` that runs the built
binary against a sandbox `FLEET_HOME` (extend the existing `fleet workers update` CLI
tests' builder). S5 extends the existing reset test. S6/S7 are rows in existing tables.

## Tests removed / KEEP

- **Tests removed:** none.
- **KEEP:** every test that uses `tdd-red`/`tdd-green`/`tdd-refactor` as a sample phase
  (≈90 fixtures across `skills/coordinator/tests`, `cmd/fleet`, `internal/tui`,
  `internal/lifecycle`) — those strings stay valid by design. The phase-constant validity
  test gains three rows, loses none.

## Acceptance

1. `go build ./... && go test -race -count=1 -timeout=5m ./...`, `gofmt -l .` empty,
   `golangci-lint run ./...` clean, `python3 -m pytest skills/ scripts/ -q` green.
2. S1–S7 pass; `verification.md` for this task shows S1–S4 run against the built binary.
3. `fleet workers update --help` lists the six worker phases and three new flags.
4. No existing `state.json` shape changes unless the new flags are used.

## Non-goals

- Any gate on the new fields (PR-3).
- Emitting the new phases from any prompt (PR-2).
- Removing `tdd-*`.
