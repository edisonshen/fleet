package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/dispatch"
)

// updateGolden refreshes the JSON fixtures under
// cmd/fleet/testdata/claims/. Toggle with `go test -update ./cmd/fleet/...`
// when the response schema (claimsResponse) changes intentionally.
//
// Pattern lifted from stdlib's go test conventions; gives the operator
// an obvious "I changed the schema, refresh fixtures" knob without
// regenerating files manually.
var updateGolden = flag.Bool("update", false, "update golden testdata fixtures")

// testHome sets FLEET_HOME to a fresh tempdir for the duration of t,
// and resets every subdir the claims CLI touches. Returns the absolute
// path so callers can scope-check on-disk artifacts.
func testHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	return dir
}

// runAcquire wraps runClaimsAcquirePrompt with byte buffers so tests
// don't need to fork a subprocess. Captures stdout for envelope
// inspection and the returned error for exit-code derivation.
func runAcquire(t *testing.T, dispatchID, owner, hostID string, content string) (claimsResponse, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runClaimsAcquirePrompt(&stdout, &stderr,
		strings.NewReader(content),
		dispatchID, owner, hostID, "", "worker", false)
	return parseEnvelope(t, stdout.Bytes()), err
}

func runRelease(t *testing.T, dispatchID, kind, hostID string) (claimsResponse, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runClaimsRelease(&stdout, &stderr, dispatchID, kind, hostID, false)
	return parseEnvelope(t, stdout.Bytes()), err
}

func runInspect(t *testing.T, dispatchID, kind string) (claimsResponse, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runClaimsInspect(&stdout, &stderr, dispatchID, kind)
	return parseEnvelope(t, stdout.Bytes()), err
}

func parseEnvelope(t *testing.T, data []byte) claimsResponse {
	t.Helper()
	if len(data) == 0 {
		return claimsResponse{}
	}
	var r claimsResponse
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parse envelope %q: %v", data, err)
	}
	return r
}

// fixturePath returns testdata/claims/<name>.json.
func fixturePath(name string) string {
	return filepath.Join("testdata", "claims", name+".json")
}

// loadFixture reads + parses a fixture envelope.
func loadFixture(t *testing.T, name string) claimsResponse {
	t.Helper()
	path := fixturePath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var r claimsResponse
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return r
}

// writeFixture writes a normalized fixture envelope. Called when
// -update is set so the operator can regenerate fixtures in-place
// after a schema change.
func writeFixture(t *testing.T, name string, got claimsResponse) {
	t.Helper()
	got = stripHostPath(got)
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture %s: %v", name, err)
	}
	data = append(data, '\n')
	path := fixturePath(name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

// stripHostPath normalizes the Path field to a placeholder so the
// fixture comparison is FLEET_HOME-independent. Inbox paths embed
// the per-test t.TempDir() path which changes every run.
func stripHostPath(r claimsResponse) claimsResponse {
	if r.Path != "" {
		r.Path = "<FLEET_HOME>/inbox/" + filepath.Base(r.Path)
	}
	return r
}

// assertGolden compares the captured envelope to the on-disk fixture.
// When -update is set, refresh the fixture from `got`.
func assertGolden(t *testing.T, name string, got claimsResponse) {
	t.Helper()
	if *updateGolden {
		writeFixture(t, name, got)
		return
	}
	want := loadFixture(t, name)
	gotNorm := stripHostPath(got)
	if gotNorm != want {
		gotJ, _ := json.MarshalIndent(gotNorm, "", "  ")
		wantJ, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("envelope mismatch for %s\nwant:\n%s\ngot:\n%s",
			name, wantJ, gotJ)
	}
}

// assertExitOutcome asserts the error returned by a runClaims* helper
// maps to the expected exit code via claimsExitCode.
func assertExitOutcome(t *testing.T, err error, wantExit int) {
	t.Helper()
	if wantExit == 0 {
		if err != nil {
			// 0-exit outcomes return a sentinel only on outcome-error
			// paths (acquired/already_acquired/released/already_released
			// all return nil from emitOutcome). Any non-nil here is a
			// regression.
			t.Errorf("want exit 0 (nil err), got %v", err)
		}
		return
	}
	if err == nil {
		t.Errorf("want exit %d, got nil err", wantExit)
		return
	}
	outcome := claimsOutcomeFromErr(err)
	got := claimsExitCode(outcome)
	if got != wantExit {
		t.Errorf("outcome=%q exit=%d, want exit %d", outcome, got, wantExit)
	}
}

// TestClaimsAcquirePrompt_Acquired_Golden — happy path golden fixture.
func TestClaimsAcquirePrompt_Acquired_Golden(t *testing.T) {
	testHome(t)
	resp, err := runAcquire(t, "a690424b", "test-owner", "test-host", "hello prompt")
	assertExitOutcome(t, err, 0)
	if resp.Outcome != dispatch.OutcomeAcquired {
		t.Errorf("Outcome = %q; want acquired", resp.Outcome)
	}
	assertGolden(t, "acquire-prompt-acquired", resp)
}

// TestClaimsAcquirePrompt_AlreadyAcquired_Golden — idempotent retry.
func TestClaimsAcquirePrompt_AlreadyAcquired_Golden(t *testing.T) {
	testHome(t)
	if _, err := runAcquire(t, "a690424b", "test-owner", "test-host", "first body"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	resp, err := runAcquire(t, "a690424b", "test-owner", "test-host", "second body")
	assertExitOutcome(t, err, 0)
	if resp.Outcome != dispatch.OutcomeAlreadyAcquired {
		t.Errorf("Outcome = %q; want already_acquired", resp.Outcome)
	}
	assertGolden(t, "acquire-prompt-already-acquired", resp)
}

// TestClaimsRelease_Released_Golden — successful release.
func TestClaimsRelease_Released_Golden(t *testing.T) {
	testHome(t)
	if _, err := runAcquire(t, "a690424b", "owner", "test-host", "body"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	resp, err := runRelease(t, "a690424b", dispatch.KindCoordPromptInbox, "test-host")
	assertExitOutcome(t, err, 0)
	if resp.Outcome != dispatch.OutcomeReleased {
		t.Errorf("Outcome = %q; want released", resp.Outcome)
	}
	assertGolden(t, "release-released", resp)
}

// TestClaimsRelease_AlreadyReleased_Golden — second release.
func TestClaimsRelease_AlreadyReleased_Golden(t *testing.T) {
	testHome(t)
	if _, err := runAcquire(t, "a690424b", "owner", "test-host", "body"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := runRelease(t, "a690424b", dispatch.KindCoordPromptInbox, "test-host"); err != nil {
		t.Fatalf("first release: %v", err)
	}
	resp, err := runRelease(t, "a690424b", dispatch.KindCoordPromptInbox, "test-host")
	assertExitOutcome(t, err, 0)
	if resp.Outcome != dispatch.OutcomeAlreadyReleased {
		t.Errorf("Outcome = %q; want already_released", resp.Outcome)
	}
	assertGolden(t, "release-already-released", resp)
}

// TestClaimsRelease_NotOwned_Golden — cross-host release attempt.
func TestClaimsRelease_NotOwned_Golden(t *testing.T) {
	testHome(t)
	if _, err := runAcquire(t, "a690424b", "owner", "owner-host", "body"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	resp, err := runRelease(t, "a690424b", dispatch.KindCoordPromptInbox, "intruder-host")
	assertExitOutcome(t, err, 10)
	if resp.Outcome != dispatch.OutcomeNotOwned {
		t.Errorf("Outcome = %q; want not_owned", resp.Outcome)
	}
	assertGolden(t, "release-not-owned", resp)
}

// TestClaimsInspect_Absent_Golden — inspect with no journal.
func TestClaimsInspect_Absent_Golden(t *testing.T) {
	testHome(t)
	resp, err := runInspect(t, "deadbeef", dispatch.KindCoordPromptInbox)
	assertExitOutcome(t, err, 11)
	if resp.Outcome != dispatch.OutcomeAbsent {
		t.Errorf("Outcome = %q; want absent", resp.Outcome)
	}
	assertGolden(t, "inspect-absent", resp)
}

// TestClaimsInspect_Acquired_Golden — inspect a live claim.
func TestClaimsInspect_Acquired_Golden(t *testing.T) {
	testHome(t)
	if _, err := runAcquire(t, "a690424b", "owner", "test-host", "body"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	resp, err := runInspect(t, "a690424b", dispatch.KindCoordPromptInbox)
	assertExitOutcome(t, err, 0)
	if resp.Outcome != dispatch.OutcomeAcquired {
		t.Errorf("Outcome = %q; want acquired", resp.Outcome)
	}
	assertGolden(t, "inspect-acquired", resp)
}

// TestClaimsAcquirePrompt_Error_BadID_Golden — invalid 8-hex input.
func TestClaimsAcquirePrompt_Error_BadID_Golden(t *testing.T) {
	testHome(t)
	resp, err := runAcquire(t, "not-hex", "owner", "test-host", "body")
	assertExitOutcome(t, err, 1)
	if resp.Outcome != dispatch.OutcomeError {
		t.Errorf("Outcome = %q; want error", resp.Outcome)
	}
	// Don't pin the error message in the fixture (will drift with
	// regex tweaks). Pin the outcome + dispatch_id + kind only.
	resp.Error = "<MESSAGE-ELIDED>"
	assertGolden(t, "acquire-prompt-error-bad-id", resp)
}

// TestClaimsExitCodeMap asserts every documented outcome maps to its
// stable exit code. Catches regressions where someone reshuffles the
// switch in claimsExitCode.
func TestClaimsExitCodeMap(t *testing.T) {
	cases := map[string]int{
		dispatch.OutcomeAcquired:        0,
		dispatch.OutcomeAlreadyAcquired: 0,
		dispatch.OutcomeReleased:        0,
		dispatch.OutcomeAlreadyReleased: 0,
		dispatch.OutcomeNotOwned:        10,
		dispatch.OutcomeAbsent:          11,
		dispatch.OutcomeContested:       12,
		dispatch.OutcomeError:           1,
		"some-future-outcome":           1, // default
	}
	for outcome, want := range cases {
		got := claimsExitCode(outcome)
		if got != want {
			t.Errorf("claimsExitCode(%q) = %d, want %d", outcome, got, want)
		}
	}
}

// TestClaimsAcquirePrompt_RoundTrip — full cycle without golden file:
// acquire writes file → inspect sees Present → release unlinks file →
// inspect sees Absent. Single test exercises every code path; the
// golden fixtures lock the wire format separately.
func TestClaimsAcquirePrompt_RoundTrip(t *testing.T) {
	home := testHome(t)
	if _, err := runAcquire(t, "a690424b", "owner", "test-host", "prompt"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	inbox := filepath.Join(home, "inbox", "a690424b.md")
	if _, err := os.Stat(inbox); err != nil {
		t.Fatalf("inbox missing post-acquire: %v", err)
	}
	if _, err := runRelease(t, "a690424b", dispatch.KindCoordPromptInbox, "test-host"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(inbox); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inbox lingered post-release: %v", err)
	}
	resp, err := runInspect(t, "a690424b", dispatch.KindCoordPromptInbox)
	if !errors.Is(err, errClaimsError{outcome: dispatch.OutcomeAbsent}) {
		t.Errorf("post-release inspect err = %v; want errClaimsError{absent}", err)
	}
	if resp.Outcome != dispatch.OutcomeAbsent {
		t.Errorf("post-release inspect Outcome = %q; want absent", resp.Outcome)
	}
}

// Silence unused-import warnings on platforms where io is only used
// transitively via bytes.Buffer.
var _ io.Reader = bytes.NewReader(nil)
