# DESIGN — CI test suite: kill the hang, get under 3 minutes

- **Status:** APPROVED (operator, 2026-06-13) — PR-1 first, then combined PR-2
- **Scope:** test-suite reliability + speed. No production behavior change.
- **Priority:** PR-1 = P0 (CI hang blocks every PR), PR-2 = P1
- **Depends-on:** none. PR-2 stacks on PR-1's branch; both target `main`.

## Decision matrix

| Decision | Default |
|---|---|
| Approve PR-1 (kill the hang, cap runtime) | **YES** |
| Approve PR-2 (get under 3 min, lock it in) | **YES** |
| Enforce a <3 min wall-clock budget gate (measured, pinned runner) | **YES** |
| Use an in-process **fake tmux** (vs keeping real tmux) | **YES** |

## Executive summary

CI runs one job — build + test. It both **hangs** (24 min on PR #232, then the
runner killed it) and is **slow** (~9 min green). Root cause is one disease:
unit tests do real OS work — they shell out to `tmux`, `sleep` on a real clock,
and scan the host's real `/tmp`. **PR-1** removes the hang and bounds every run
at 5 minutes. **PR-2** removes the real-`tmux` tax and gets the suite under 3
minutes, with a structural guard so it stays there.

## Why now

CI is the gate on **every** PR. A single hung test consumes up to 24 minutes of
runner time and blocks every engineer waiting to merge — #232 is blocked on it
right now. The slowness taxes every push (~9 min/run). This is a direct hit to
developer productivity and release velocity, and the hang is **intermittent and
recurring** (it reproduced on a re-run), so it will keep biting until fixed.

## Problem

A test calls a real `fleet` code path (`dispatch` or `status`). That code runs a
"reconcile" (GC of orphaned resources). Reconcile lists leftover test sockets by
scanning `/tmp` and running a `tmux` probe on **every** socket. There are **two**
such hardcoded scans:

```
   fleet dispatch ──▶ reconcile (orphan-tmux) ──▶ scanSocketsDir("/tmp")  ◀── live_test_sockets.go:157
   fleet status   ──▶ reconcile (ALL kinds)   ──▶ scanSocketsDir("/tmp")  ◀── gc.go:948 (KindSockets)
                                                     │
                                                     ▼  for EACH /tmp/fleet-test-*.sock
                                                 tmux -S <sock> ls         ◀── one subprocess each
```

When `/tmp` fills with leftover `fleet-test-*.sock` files (a known leak,
fleet#165 — 157 found on the host; the suite also leaks into its own `/tmp`
mid-run), the scan becomes **N tmux subprocesses per test**:

```
   /tmp has 157 leaked sockets
        ▼
   scanSocketsDir("/tmp") ──▶ 157 × (tmux -S … ls) ──▶ minutes of grind ──▶ 24-min hang
```

Slowness, measured (green run, per package):

| Package | Time | Why |
|---|---|---|
| `cmd/fleet` | **495.84s** | 523 tests, 0 parallel, real tmux, the `/tmp` scan, a 10s pid-resolve cluster |
| `internal/handoffop` | 145.52s | 10 tests each doing 2–3 real `tmux` spawns |
| `internal/spawn` | 59.83s | 80 tests × ~1.5s real tmux spawn |
| everything else | ~1–5s each | fine |

CI wall-clock is almost entirely `cmd/fleet`.

## Proposal

Two PRs.

**PR-1 (P0) — stop the hang, cap the runtime.** Make the GC scan dir
configurable (default `/tmp`; tests point it at an empty decoy), cap the
10-second pid-resolve, and add a hard `-timeout=5m`. The hang becomes impossible
and every run is bounded. PR-1's win is qualitative — it does not remove the
real `tmux` spawns, so it does not yet reach <3 min, but it unblocks #232 today.

**PR-2 (P1) — get under 3 minutes and lock it in.** Replace real `tmux` with an
in-process fake so no unit test spawns a subprocess; delete abandoned/low-value
tests under an objective rubric; add a lint rule so the PR-1 isolation can't
regress; enable `t.Parallel()` where it's safe. A structural CI gate (0 real
`tmux` execs, no host `/tmp` probe, lint green) plus a measured wall-clock budget
on a pinned runner keep the suite fast.

**Performance model.** `go test ./...` runs packages in parallel, so wall-clock ≈
**max(package) + overhead**, not the sum. The lever that matters is the slowest
package (`cmd/fleet`), which both PRs target. The <3 min target is enforced by
**measurement**, not a hardcoded estimate.

## Tradeoffs

| Tradeoff | Why accepted |
|---|---|
| Fake tmux → less real-environment realism | Massive, deterministic speed gain; a parity contract + real-tmux smoke tests keep the fake honest |
| Fake adds a maintenance surface | Removes the subprocess dependency from ~all unit tests; net complexity drops |
| Function-table seams in `internal/tmux` | Enables deterministic, parallel-safe tests without an interface refactor across ~140 call sites |
| Deleting tests | Objective rubric (coverage gate + no-delete classes) bounds the risk; fewer, higher-signal tests |
| A <3 min budget that can flake across runners | Hard gates are structural; wall-clock is a tracked budget on a pinned runner, not a flaky per-run assertion |

## Alternatives considered

- **Keep real tmux, just shard CI into parallel jobs.** Rejected as the primary
  fix: it hides the cost rather than removing it, multiplies runner minutes, and
  leaves the hang (a sharded job still scans real `/tmp`). Sharding the one 38s
  real-binary integration test into a side job is a possible *later* margin play
  (out of scope).
- **A Go `interface` over `tmux`.** Rejected: `internal/tmux` is free functions
  with ~140 call sites; an interface forces threading a value through all of
  them. The function-table seam achieves the same with no call-site churn.
- **mockery / generated mocks.** Rejected (operator-confirmed): mockery needs an
  interface (which we're avoiding) and produces expectation-style mocks that are
  clumsy for the *stateful* behavior tmux needs (`HasSession` reflecting prior
  `Spawn`/`Kill`). A ~150-line hand-written stateful fake + parity contract is a
  better, zero-dependency fit. (Revisit later if broad DI need appears.)

## Rollout

1. Land **PR-1** first. Its own branch carries the scan isolation + `-timeout`,
   so PR-1's own CI passes the very thing that hangs everyone else — it is
   self-unblocking.
2. Rebase **#232** onto PR-1 → its CI goes green → merge.
3. Land **PR-2** (stacked on PR-1's branch, base `main`).
4. Establish the wall-clock baseline on the pinned runner; the budget gate
   tracks regressions from there.

## Risks

| Risk | Mitigation |
|---|---|
| Fake tmux diverges from real behavior | Parity contract runs fake AND real through the same scenarios; fake fails fast on unsupported calls; real-tmux smoke tests retained |
| Test cull drops real coverage | Hard coverage gate (no statement-coverage drop) + per-test branch mapping + hard no-delete classes (error/recovery/cleanup paths) |
| `t.Parallel()` introduces data races | Scope to tests with zero env/cwd/package-global mutation; prove `-race` clean; broad parallelism deferred |
| `-timeout=5m` wedges instead of failing fast | Confirmed: Go's timeout aborts at process level and kills wedged `tmux` execs; `timeout-minutes: 6` on the job as backstop |

## Appendix — Implementation detail (for engineers)

### PR-1 exact changes

- **`internal/gc` — one env-aware scan-dir source, wired into BOTH callsites.**
  Add `gcScanDir()` → `FLEET_GC_SCAN_DIR` or `/tmp` when unset. `DefaultDeps()`
  reads it **per call** (confirmed per-reconcile, not memoized:
  `dispatch.go:68`, `status.go:42`) and wires it into `ListSockets` (`gc.go:948`)
  **and** `listLiveTestSocketsOnDisk` (`live_test_sockets.go:157`, take a `dir`
  arg). A `Deps.ScanDir` *field* alone is inert — the closures don't read the
  struct. Add `DefaultDepsWithScanDir(dir)` for `internal/gc` unit tests
  (env-free, parallel-friendly).
- **`cmd/fleet/main_test.go` `TestMain`** — set `FLEET_GC_SCAN_DIR` to one empty
  temp dir for the whole package (covers `status_test.go` and every
  default-wrapper test that bypasses per-test helpers) AND `FLEET_PID_RESOLVE_S=1`,
  beside the existing `FLEET_RC_BOOTSTRAP_DISABLED` set (check err + panic).
  `maintenance_test.go` already clears `FLEET_PID_RESOLVE_S` for the
  default-resolve test, so the global doesn't mask it. The scan dir is an empty
  decoy — no tmux server is bound into it (`tmuxtest` keeps the real socket in
  `/tmp` due to the macOS ~104-byte socket-path limit, and reaps it in its own
  cleanup), so no per-test server-reap is added.
- **`.github/workflows/ci.yml`** — Test step → `go test -race -count=1
  -timeout=5m ./...`; add `timeout-minutes: 6` under `build-test-lint:`.

### PR-2 exact changes

- **`internal/tmux` — function-table seam (NOT an interface).** Wrap **all 11**
  shelling funcs (`Available, HasSession, SessionAlive, Spawn, Attach, SendKeys,
  CapturePane, SetStatusHint, Kill, ListSessions, ListSessionsWithCreated`; only
  pure `SessionName` is exempt) behind package-level vars; a test helper swaps
  them to a memory-backed fake. Wrapping only `Spawn`+4 leaves `SessionAlive`
  (26×), `Available` (17×), `Attach` (13×), … still shelling out. The real
  backend keeps the socket-sink guard (`tmux.go:242-250`); a `PATH` `tmux`-shim
  hard-fails any routed test that execs real tmux (fake call-recording can't see
  a bypass).
- **Parity contract** — all 11 ops run fake vs real (under `requireTmux`); the
  fake fails fast on unsupported calls; the fake ships its own unit tests.
- **Route tests** (`internal/handoffop`, `internal/spawn`, `cmd/fleet` handoff/
  dispatch). "0 real tmux" asserted per-package via fake recording + the shim.
- **`scripts/lint-test-isolation.sh`** — third rule: a `TestXxx` calling
  `runDispatch(`/`runDispatchReconcile(`/`gc.Reconcile(`/`runStatus(` must also
  contain a marker that routes the scan (`stubDispatchReconcile(`,
  `t.Setenv("FLEET_GC_SCAN_DIR"`, `DefaultDepsWithScanDir(`, `isolateScanDir(`,
  or a `scandir-exempt` comment). `TMPDIR` is NOT accepted. Three fixtures
  (tmux-only / scandir-only / both).
- **Test cull rubric** — coverage gate (no statement-coverage drop per package),
  per-test branch mapping, hard no-delete classes (error/recovery/destructive/
  cleanup), delete-eligible = pure duplicates/removed-feature/tautologies. Suite
  is ~2072 funcs (`cmd/fleet` 523, `handoffop` 81, `spawn` 80).
- **`t.Parallel()`** — only tests with zero env/cwd AND zero package-global
  (fake-seam) mutation; the dominant blocker is `FLEET_HOME` (~125 `t.Setenv`),
  not tmux. Broad parallelism (threading `FLEET_HOME`) is out of scope. Commit
  order: fake+tests+parity → lint fixtures → cull (coverage evidence) →
  `t.Parallel()` last, with before/after `-race` in the PR body.

### Test plan

PR-1:
| Scenario | Input | Expected |
|---|---|---|
| OrphanTmux path | `runDispatch` test, `FLEET_GC_SCAN_DIR`=temp + real socket + `PATH` fake-tmux recorder | ≥1 probe; all under scan dir, none under `/tmp` |
| KindSockets path | gc test via `DefaultDepsWithScanDir(dir)` + an **aged** socket (mtime > MaxAge → reaches `SocketLive`) | ReadDir + probe target `dir`, not `/tmp` |
| Production unchanged | `DefaultDeps()`, `FLEET_GC_SCAN_DIR` unset | both closures use `/tmp` |
| No stray callsite | `grep 'scanSocketsDir("/tmp")'` | **zero** matches; `/tmp` only inside `gcScanDir()` |
| pid cap | any `cmd/fleet` dispatch test | per-test wall < 2s |
| Timeout fail-fast | wedged tmux probe | `go test` aborts at 5m |

PR-2:
| Scenario | Input | Expected |
|---|---|---|
| Fake tmux | handoffop Resume via fake | 0 real `tmux` execs (fake recording + `PATH` shim) |
| Fake parity | all 11 ops vs real under `requireTmux` | identical semantics; fail-fast on unsupported |
| Lint catches gap | `runDispatch` test w/o scan-dir marker | `lint-test-isolation.sh` non-zero |
| Cull safety | each deleted test | coverage no-drop + named kept test for its branch |
| Parallel race-clean | `t.Parallel()` tests under `-race` | green, no races |
| Wall budget | `go test -race -timeout=5m ./...` on pinned runner | < 3 min vs baseline |
