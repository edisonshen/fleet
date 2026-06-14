package main

import (
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/testutil"
)

// TestMain is the package-shared test entrypoint for cmd/fleet.
//
// Sets FLEET_RC_BOOTSTRAP_DISABLED=1 BEFORE any test runs so that the
// cmd/fleet wrapper `injectRemoteControlFlag` (and through it the
// coord-spawn + handoff replacement paths) returns the input command
// unchanged. Without this gate, tests that exercise runDispatch /
// runHandoff with the default claude wrapper would spawn a real
// `claude --remote-control "fleet-coord-..."` inside the isolated
// tmux session, attach to the operator's mobile / web Claude Code
// service, and emit a push notification on every test run. The
// reviewer for PR2 (rc-listener-bootstrap-sk-3e98 root cause) saw
// dozens of mobile pings per /codex iteration across 29+ runs.
//
// Production code (no `go test`) is unaffected: the env var is unset
// by default, so the wrapper rewrites the body to inject
// `--remote-control "<session>"` exactly as it always has. The pair of
// regression tests in rc_bootstrap_env_test.go pins both branches.
//
// One TestMain serves the entire `cmd/fleet` package (it is
// `package main`, single test binary across all *_test.go files in
// this directory). The brief enumerated five test files explicitly
// (handoff_test, dispatch_test, dispatch_recovery_test, autoinit_test,
// drain_test); since Go forbids multiple TestMain in one package, a
// single shared TestMain here covers all of them.
//
// fleet#165 PR-B: also reaps stale /tmp/fleet-test-*.sock debris at
// suite start AND end. Belt-and-suspenders for the case where a test
// panics before its tmuxtest.RequireTmux cleanup fires. See
// internal/testutil/sweeper.go for the wrapper contract.
func TestMain(m *testing.M) {
	// Setenv (not t.Setenv) — there is no *testing.T available in
	// TestMain; we set the env globally for the whole test binary.
	// The two regression tests in rc_bootstrap_env_test.go use
	// t.Setenv to toggle the value per-test, which restores
	// automatically via t.Cleanup.
	//
	// os.Setenv only returns a non-nil error on platforms with
	// strict env limits. We still check the return rather than
	// discard it to satisfy errcheck — fail loud at process start
	// rather than silently regress and leak listener spawns.
	if err := os.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1"); err != nil {
		panic("TestMain: os.Setenv FLEET_RC_BOOTSTRAP_DISABLED failed: " + err.Error())
	}

	// ci-perf-pr1 (P0 CI-hang fix): point the GC socket scan at an EMPTY decoy
	// dir for the whole cmd/fleet package so no test walks the host's real
	// /tmp. The reconcile pass in runDispatch/runStatus scans
	// FLEET_GC_SCAN_DIR (via gc.gcScanDir) and runs `tmux -S <sock> ls` on
	// every fleet-test-*.sock it finds; on a host with hundreds of leaked
	// sockets (fleet#165) that becomes N tmux subprocesses PER test and hangs
	// the suite (24-min kill on PR #232). The decoy is empty: tmuxtest keeps
	// its real fixture socket in /tmp (macOS ~104-byte socket-path limit) and
	// reaps it in its own cleanup, so nothing is bound into the decoy and no
	// per-test server-reap is needed here. Tests that need to assert against a
	// seeded socket override this with their own per-test FLEET_GC_SCAN_DIR.
	// Prefix `fleet-test-` (codex iter-5 [P2]) so that if this dir ever leaks
	// (a SIGKILL skipping the explicit RemoveAll before os.Exit), CI's
	// top-level `/tmp/fleet-test-*` leak gate makes it VISIBLE rather than
	// letting empty decoy dirs accumulate silently on the runner.
	scanDecoy, err := os.MkdirTemp("", "fleet-test-gc-scan-decoy-")
	if err != nil {
		panic("TestMain: os.MkdirTemp for FLEET_GC_SCAN_DIR decoy failed: " + err.Error())
	}
	if err := os.Setenv("FLEET_GC_SCAN_DIR", scanDecoy); err != nil {
		panic("TestMain: os.Setenv FLEET_GC_SCAN_DIR failed: " + err.Error())
	}

	// Cap the 10s pid-resolve poll to 1s for the package. The cmd/fleet
	// dispatch tests spawn a stub that never publishes a real claude pid, so
	// each resolve burns the full default timeout — a ~10s-per-test cluster.
	// 1s keeps the fail-path fast without masking the default-resolve path,
	// which maintenance_test.go re-clears via t.Setenv where it matters.
	if err := os.Setenv("FLEET_PID_RESOLVE_S", "1"); err != nil {
		panic("TestMain: os.Setenv FLEET_PID_RESOLVE_S failed: " + err.Error())
	}
	// ci-perf-pr1 (P0), codex iter-2 [P1]: sweep the DECOY, not real /tmp.
	// testutil.Sweep(time.Hour) hardcodes SweepDir("/tmp") and probes EVERY
	// /tmp/fleet-test-*.sock with `tmux -S <sock> list-sessions` — on a host
	// with hundreds of leaked sockets that is the exact N-tmux-subprocess
	// grind this PR removes, and it runs in TestMain BEFORE any isolated test.
	// Pointing it at the empty decoy makes the sweep a no-op (nothing to
	// probe) so TestMain itself can't hang. Reaping real /tmp leaks is the
	// socket-leak P0's job (`fleet gc`), out of scope here and not the test
	// harness's to do (fleet owns its own resources, not the operator's /tmp).
	_ = testutil.SweepDir(scanDecoy, time.Hour) // start-of-run; best-effort
	code := m.Run()
	// End-of-run teardown: guarded sweep (freshness window +
	// socketLive probe). The force-mode testutil.SweepAll is
	// deliberately NOT used here: `go test ./...` runs sibling test
	// binaries in parallel (default -p=GOMAXPROCS), all sharing the
	// /tmp/fleet-test-*.sock namespace via tmuxtest.isolatedSocketPath.
	// SweepAll on teardown would force-kill sockets owned by sibling
	// packages whose tests are still running, causing a P0 CI
	// regression (cmd/fleet finishes last in practice but the trade-
	// off is identical across all packages). SweepAll/SweepAllDir
	// remain in sweeper.go for PR-D's operator-invoked
	// `fleet gc --force-test-sockets` path. The lint guard in
	// scripts/lint-test-isolation.sh closes the original root cause
	// (empty-command dispatch tests forking real claude) at the
	// source; bypassed t.Cleanup orphans (rare panic path) get reaped
	// by the operator-invoked `fleet gc` (the socket-leak P0's lane).
	//
	// ci-perf-pr1: both sweeps now target the empty decoy, not /tmp (see the
	// start-of-run note) — they no longer reap real /tmp debris, but cmd/fleet
	// tests no longer CREATE any (tmuxtest reaps its own sockets via
	// t.Cleanup), so there is nothing for this sweep to reap. Trading the
	// /tmp grind (a hang vector) for a no-op decoy sweep is the whole point.
	_ = testutil.SweepDir(scanDecoy, time.Hour) // end-of-run; best-effort (decoy, see start-of-run note)
	// os.Exit skips defers, so reap the decoy scan dir explicitly (fleet owns
	// the lifecycle of everything it creates — feedback_fleet_owns_its_resources).
	_ = os.RemoveAll(scanDecoy)
	os.Exit(code)
}
