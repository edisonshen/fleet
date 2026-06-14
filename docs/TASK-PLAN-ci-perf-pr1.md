# TASK-PLAN — PR-1: kill the CI test hang + cap runtime

- **Task:** `ci-perf-pr1` (P0) · **Owner:** unassigned worker · **Base:** `main` · **Branch:** `worker/ci-perf-pr1`
- **Design:** `docs/DESIGN-ci-3min-test-suite.md` · **Depends-on:** none

## Goal

Make the test suite **unable to hang** and **bounded at 5 minutes**, and unblock
PR #232.

## Success criteria

- No unit test probes the host's real `/tmp` (both GC scan callsites isolated).
- The `cmd/fleet` 10-second pid-resolve cluster drops to ~1s/test.
- CI carries `-timeout=5m` + `timeout-minutes: 6`.
- Production behavior unchanged (scan still defaults to `/tmp`).
- Full suite green under `-race`.

## Work breakdown

| # | Task | Priority |
|---|------|----------|
| 1 | Env-aware GC **scan-dir seam**, wired into **both** `/tmp` callsites | **Critical** |
| 2 | `cmd/fleet` `TestMain` sets the decoy scan dir **+** `FLEET_PID_RESOLVE_S=1` | **Critical** |
| 3 | CI `-timeout=5m` + `timeout-minutes: 6` | High |

## Dependencies

None. PR-2 depends on this (consumes the scan-dir API + the reusable `PATH`
tmux-shim helper).

## Exit criteria (per task)

1. **Scan-dir seam** — done when: both `runDispatch` (OrphanTmux) and the
   `DefaultDepsWithScanDir`+aged-socket (KindSockets) tests show all probes
   target the injected dir and **none** `/tmp`; `grep 'scanSocketsDir("/tmp")'`
   returns zero matches; `DefaultDeps()` with the env unset still uses `/tmp`.
2. **TestMain** — done when: every `cmd/fleet` test (incl. `status_test.go`)
   scans the decoy dir; per-test dispatch wall < 2s.
3. **CI** — done when: a wedged probe makes `go test` abort at 5 min, not 24.

## Risks

| Risk | Mitigation |
|---|---|
| Seam changes production `/tmp` behavior | Env defaults to `/tmp`; covered by a production-default test |
| One callsite missed (the hang persists) | Acceptance tests **both** callsites; grep guard for stray `"/tmp"` |
| pid-cap masks the real 10s-resolve path | `maintenance_test.go` already clears the env for that test |

## Not doing

- Fake tmux, `t.Parallel()`, the lint rule, the test cull — all PR-2.
- Fixing the underlying socket leak (`p0-cleanup-fleet-owns-it-0295`).
- Build-tagging the real-binary integration test (separate P2).

## Gate

Standard verify (`go build ./...`, `go test -race -count=1 -timeout=5m ./...`,
`gofmt -l .`, `golangci-lint run ./...`, `python3 -m pytest skills/ scripts/ -q`),
then multi-round `/codex review` + `/review` until no P0/P1 in two consecutive
runs each. PR body: "PR-1/2 of DESIGN-ci-3min-test-suite; base main; PR-2 stacks
next."

---

## Implementation notes (for the engineer)

### Scan-dir seam — wire BOTH callsites
There are TWO hardcoded `/tmp` scans: `live_test_sockets.go:157` (OrphanTmux,
`fleet dispatch`) and `gc.go:948` `ListSockets` (KindSockets, `fleet status` via
`AllKinds`). Add `gcScanDir()` → `FLEET_GC_SCAN_DIR` or `/tmp` when unset.
`DefaultDeps()` reads it **per call** (it is per-reconcile, not memoized —
`dispatch.go:68`, `status.go:42`) and wires it into BOTH closures
(`listLiveTestSocketsOnDisk` takes a `dir` arg). A `Deps.ScanDir` *field* alone
is inert — the closures don't read the struct. Add `DefaultDepsWithScanDir(dir)`
for `internal/gc` unit tests (env-free → parallel-friendly).

### TestMain decoy + pid cap
In `cmd/fleet/main_test.go` `TestMain`, set `FLEET_GC_SCAN_DIR` to one empty temp
dir for the whole package (covers `status_test.go` and every default-wrapper
test that doesn't use a per-test helper) and `FLEET_PID_RESOLVE_S=1`, beside the
existing `FLEET_RC_BOOTSTRAP_DISABLED` (check err + panic). The scan dir is an
empty **decoy** — no tmux server is bound into it (`tmuxtest` keeps the real
socket in `/tmp` due to the macOS ~104-byte socket-path limit and reaps it in
its own cleanup), so no per-test server-reap is added. `maintenance_test.go`
already clears `FLEET_PID_RESOLVE_S` for the default-resolve test.

### Acceptance test mechanics
A "200 fake `.sock` regular files, assert <2s" test passes trivially even if the
seam is broken (`firstFleetSession` rejects non-sockets via `os.Lstat`+`ModeSocket`;
KindSockets filters `age<MaxAge` before any tmux call). So:
- **OrphanTmux:** `runDispatch` + a **real** seeded socket (fresh is fine — the
  age filter only gates KindSockets) + a reusable `PATH` fake-`tmux` recorder
  (kept reusable so PR-2 can reuse it); assert ≥1 probe, all paths under the
  scan dir, none under `/tmp`.
- **KindSockets:** `DefaultDepsWithScanDir(dir)` + an **aged** real socket
  (mtime > MaxAge, so it passes the `age<MaxAge` skip and reaches `SocketLive`);
  assert ReadDir + probe target `dir`, not `/tmp`. A fresh socket via `runStatus`
  would pass via the OrphanTmux path even if `gc.go:948` stayed hardcoded — so
  it must be aged / directly injected.

### Files
`internal/gc/live_test_sockets.go` (take `dir` arg), `internal/gc/gc.go`
(`gcScanDir()` + both closures + `DefaultDepsWithScanDir`), `cmd/fleet/main_test.go`,
`.github/workflows/ci.yml`, plus new tests in `internal/gc/*_test.go` and a
`cmd/fleet` probe-recording test covering both paths.
