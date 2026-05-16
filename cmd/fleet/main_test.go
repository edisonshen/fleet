package main

import (
	"os"
	"testing"
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
	os.Exit(m.Run())
}
