package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/rc"
)

// rcTestFleetHome scopes FLEET_HOME to a t.TempDir and clears the
// env-gate so the rc helpers exercise their production paths.
func rcTestFleetHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_HOME", dir)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
	return dir
}

// TestRCCLI_UpEmitsJSONEnvelope pins the JSON wire shape for
// `fleet rc up` so external callers (skills/coordinator/remote_control.py)
// can parse the response deterministically. Uses the SkipSpawn test
// seam so no real listener launches.
func TestRCCLI_UpEmitsJSONEnvelope(t *testing.T) {
	rcTestFleetHome(t)

	// Inject a fake spawner that returns a synthetic PID without
	// actually exec'ing claude.
	restoreSpawn := rc.SetSpawnerForTest(func(workingDir string) (int, error) {
		return os.Getpid(), nil
	})
	defer restoreSpawn()

	// Stub agent.List to return nothing so cwd falls through to the
	// `--cwd` override (the test passes it explicitly via Up).
	// Up's CLI wrapper calls rc.Up which calls ResolveWorkingDir.
	// We use the explicit --cwd flag to short-circuit.

	out := &bytes.Buffer{}
	if err := runRCUp(out, "demo", t.TempDir(), false, false, ""); err != nil {
		// runRCUp returns errRC sentinels for non-zero exit outcomes
		// (not_owned/contested). acquired/already_acquired return nil.
		// If we got an error, the JSON envelope is already on stdout.
		_ = err
	}

	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Cmd != "up" {
		t.Errorf("response cmd=%q want up", resp.Cmd)
	}
	if resp.Project != "demo" {
		t.Errorf("response project=%q want demo", resp.Project)
	}
	if resp.Outcome != rc.OutcomeAcquired && resp.Outcome != rc.OutcomeAlreadyAcquired {
		t.Errorf("response outcome=%q want acquired/already_acquired", resp.Outcome)
	}
}

// TestRCCLI_UpRejectsInvalidCoordID (codex P2): a malformed --coord-id
// must be rejected BEFORE rc.Up persists it as owning_coord_id; otherwise
// defaultOwnerAlive treats the (missing) record as dead-owner and reaps +
// respawns the daemon repeatedly with the same poison owner. Expect a
// non-nil error and an error JSON envelope, with no listener spawned.
func TestRCCLI_UpRejectsInvalidCoordID(t *testing.T) {
	rcTestFleetHome(t)
	spawned := false
	restoreSpawn := rc.SetSpawnerForTest(func(workingDir string) (int, error) {
		spawned = true
		return os.Getpid(), nil
	})
	defer restoreSpawn()

	out := &bytes.Buffer{}
	err := runRCUp(out, "demo", t.TempDir(), false, false, "NOT-HEX!!")
	if err == nil {
		t.Fatalf("expected error for invalid --coord-id")
	}
	if spawned {
		t.Fatalf("listener must NOT spawn when --coord-id is invalid")
	}
	var resp rcResponse
	if jerr := json.Unmarshal(out.Bytes(), &resp); jerr != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", jerr, out.String())
	}
	if resp.Outcome != rc.OutcomeError {
		t.Fatalf("outcome=%q want error", resp.Outcome)
	}
	if !strings.Contains(resp.Error, "invalid --coord-id") {
		t.Fatalf("error %q must mention invalid --coord-id", resp.Error)
	}
}

// TestRCCLI_UpAcceptsValidCoordID confirms the validation gate doesn't
// reject a legitimate 8-hex agent ID — it must reach rc.Up and acquire.
func TestRCCLI_UpAcceptsValidCoordID(t *testing.T) {
	rcTestFleetHome(t)
	restoreSpawn := rc.SetSpawnerForTest(func(workingDir string) (int, error) {
		return os.Getpid(), nil
	})
	defer restoreSpawn()

	out := &bytes.Buffer{}
	_ = runRCUp(out, "demo", t.TempDir(), false, false, "abcd1234")
	var resp rcResponse
	if jerr := json.Unmarshal(out.Bytes(), &resp); jerr != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", jerr, out.String())
	}
	if resp.Outcome != rc.OutcomeAcquired && resp.Outcome != rc.OutcomeAlreadyAcquired {
		t.Fatalf("valid coord-id outcome=%q want acquired/already_acquired", resp.Outcome)
	}
}

// TestRCCLI_DownEmitsJSONEnvelope pins the down-side wire shape.
func TestRCCLI_DownEmitsJSONEnvelope(t *testing.T) {
	rcTestFleetHome(t)
	restoreKill := rc.SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	out := &bytes.Buffer{}
	if err := runRCDown(out, "demo"); err != nil {
		// already_released → errRC sentinel via emitRC (non-zero
		// outcome paths). Either way the JSON is on stdout.
		_ = err
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Cmd != "down" || resp.Project != "demo" {
		t.Errorf("response shape drift: %+v", resp)
	}
	// Without prior up, down on a clean project is already_released.
	if resp.Outcome != rc.OutcomeAlreadyReleased {
		t.Errorf("outcome=%q want already_released", resp.Outcome)
	}
}

// TestRCCLI_ListEmitsProjectsArray pins the list-output shape.
func TestRCCLI_ListEmitsProjectsArray(t *testing.T) {
	rcTestFleetHome(t)
	if err := rc.WriteMarker("alpha"); err != nil {
		t.Fatalf("WriteMarker alpha: %v", err)
	}
	if err := rc.WriteMarker("beta"); err != nil {
		t.Fatalf("WriteMarker beta: %v", err)
	}
	out := &bytes.Buffer{}
	if err := runRCList(out); err != nil {
		_ = err
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Cmd != "list" {
		t.Errorf("cmd=%q want list", resp.Cmd)
	}
	if len(resp.Projects) != 2 {
		t.Errorf("projects len=%d want 2 (%v)", len(resp.Projects), resp.Projects)
	}
}

// TestRCCLI_StatusReportsAbsentWhenNoMarker pins the
// not-enabled / absent semantics.
func TestRCCLI_StatusReportsAbsentWhenNoMarker(t *testing.T) {
	rcTestFleetHome(t)
	out := &bytes.Buffer{}
	if err := runRCStatus(out, "demo", false); err != nil {
		// absent → errRC sentinel via emitRC. JSON still on stdout.
		_ = err
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Outcome != rc.OutcomeAbsent {
		t.Errorf("outcome=%q want absent (marker missing)", resp.Outcome)
	}
}

// TestRCCLI_ExitCodeMappingStable pins the public exit-code contract.
// Skill consumers depend on these numeric exits.
func TestRCCLI_ExitCodeMappingStable(t *testing.T) {
	cases := []struct {
		outcome string
		want    int
	}{
		{rc.OutcomeAcquired, 0},
		{rc.OutcomeAlreadyAcquired, 0},
		{rc.OutcomeReleased, 0},
		{rc.OutcomeAlreadyReleased, 0},
		{rc.OutcomeConnected, 0},
		{rc.OutcomeNotEnabled, 10},
		{rc.OutcomeNotOwned, 10},
		{rc.OutcomeAbsent, 11},
		{rc.OutcomeContested, 12},
		{rc.OutcomeError, 1},
		{"unknown_outcome", 1},
	}
	for _, c := range cases {
		got := rcExitCode(c.outcome)
		if got != c.want {
			t.Errorf("rcExitCode(%q)=%d want %d", c.outcome, got, c.want)
		}
	}
}
