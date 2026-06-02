package main

import (
	"bytes"
	"strings"
	"testing"
)

// claims_durability_test.go — CLI boundary tests for the dispatch-
// durability (#184) subcommands. These exercise the JSON envelope +
// outcome strings the Python coord parses (loop.py / register_subagent /
// handoff_resume), the stable contract.

// assertExit0 fails the test if err maps to a non-zero exit code. The
// #184 success-shaped outcomes (ok/acked/reserved/capped/reset) return
// the errClaimsError sentinel from emitOutcome but resolve to exit 0 via
// claimsExitCode — mirror main.go's interpretation rather than treating
// the sentinel as a hard failure.
func assertExit0(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if oc := claimsOutcomeFromErr(err); oc != "" {
		if code := claimsExitCode(oc); code != 0 {
			t.Fatalf("outcome %q maps to exit %d, want 0", oc, code)
		}
		return
	}
	t.Fatalf("unexpected error: %v", err)
}

func runMarkLaunch(t *testing.T, id, gen string) (claimsResponse, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runClaimsMarkLaunchAttempted(&stdout, id, gen)
	return parseEnvelope(t, stdout.Bytes()), err
}

func runMarkAcked(t *testing.T, id string) (claimsResponse, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runClaimsMarkAcked(&stdout, id)
	return parseEnvelope(t, stdout.Bytes()), err
}

func runReserveReplay(t *testing.T, id string, cap int) (claimsResponse, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runClaimsReserveReplay(&stdout, id, cap)
	return parseEnvelope(t, stdout.Bytes()), err
}

func runResetForRelaunch(t *testing.T, id, body string) (claimsResponse, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runClaimsResetForRelaunch(&stdout, strings.NewReader(body), id)
	return parseEnvelope(t, stdout.Bytes()), err
}

// TestClaimsMarkLaunchAttempted_CLITriState exercises the full
// acquire→launch lifecycle through the CLI surface.
func TestClaimsMarkLaunchAttempted_CLITriState(t *testing.T) {
	testHome(t)
	id := "ab000001"
	if _, err := runAcquire(t, id, "project/fleet/slug/x", "h", "prompt"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Wrong gen → predicate_fail, exit 20.
	resp, err := runMarkLaunch(t, id, "7")
	assertExitOutcome(t, err, 20)
	if resp.Outcome != "predicate_fail" {
		t.Fatalf("outcome = %q, want predicate_fail", resp.Outcome)
	}

	// Right gen (0) → ok, exit 0.
	resp, err = runMarkLaunch(t, id, "0")
	assertExit0(t, err)
	if resp.Outcome != "ok" {
		t.Fatalf("outcome = %q, want ok", resp.Outcome)
	}

	// Second attempt → predicate_fail (no double-launch).
	resp, _ = runMarkLaunch(t, id, "0")
	if resp.Outcome != "predicate_fail" {
		t.Fatalf("2nd outcome = %q, want predicate_fail", resp.Outcome)
	}
}

// TestClaimsMarkAcked_CLI pins ack flips launch_attempted → acked.
func TestClaimsMarkAcked_CLI(t *testing.T) {
	testHome(t)
	id := "ac000001"
	if _, err := runAcquire(t, id, "o", "h", "p"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := runMarkLaunch(t, id, "0"); err != nil {
		assertExit0(t, err)
	}
	resp, err := runMarkAcked(t, id)
	assertExit0(t, err)
	if resp.Outcome != "acked" {
		t.Fatalf("outcome = %q, want acked", resp.Outcome)
	}
}

// TestClaimsReserveReplay_CLI pins the reserve → cap → blocked path and
// the generation field in the envelope.
func TestClaimsReserveReplay_CLI(t *testing.T) {
	testHome(t)
	id := "ad000001"
	if _, err := runAcquire(t, id, "o", "h", "p"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	resp, err := runReserveReplay(t, id, 2)
	assertExit0(t, err)
	if resp.Outcome != "reserved" {
		t.Fatalf("outcome = %q, want reserved", resp.Outcome)
	}
	if resp.Generation == nil || *resp.Generation != 0 {
		t.Fatalf("generation = %v, want 0", resp.Generation)
	}
	// 2nd reserve hits cap=2 → reserved again (attempts now 1 < 2), then
	// 3rd is capped.
	if r, _ := runReserveReplay(t, id, 2); r.Outcome != "reserved" {
		t.Fatalf("2nd reserve = %q, want reserved", r.Outcome)
	}
	r, _ := runReserveReplay(t, id, 2)
	if r.Outcome != "capped" {
		t.Fatalf("3rd reserve = %q, want capped", r.Outcome)
	}
}

// TestClaimsResetForRelaunch_CLI pins reset rewrites inbox + bumps gen.
func TestClaimsResetForRelaunch_CLI(t *testing.T) {
	testHome(t)
	id := "ae000001"
	if _, err := runAcquire(t, id, "o", "h", "original"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := runMarkLaunch(t, id, "0"); err != nil {
		assertExit0(t, err)
	}
	if _, err := runMarkAcked(t, id); err != nil {
		assertExit0(t, err)
	}
	resp, err := runResetForRelaunch(t, id, "RESUME BODY")
	assertExit0(t, err)
	if resp.Outcome != "reset" {
		t.Fatalf("outcome = %q, want reset", resp.Outcome)
	}
	if resp.Generation == nil || *resp.Generation != 1 {
		t.Fatalf("generation = %v, want 1", resp.Generation)
	}
	// After reset the entry is ExecPending again at gen 1 → a launch with
	// the new gen succeeds.
	if r, _ := runMarkLaunch(t, id, "1"); r.Outcome != "ok" {
		t.Fatalf("post-reset launch (gen 1) = %q, want ok", r.Outcome)
	}
}

// TestClaimsResetForRelaunch_Absent pins absent-journal handling.
func TestClaimsResetForRelaunch_Absent(t *testing.T) {
	testHome(t)
	resp, _ := runResetForRelaunch(t, "deadbeef", "body")
	if resp.Outcome != "absent" {
		t.Fatalf("outcome = %q, want absent", resp.Outcome)
	}
}
