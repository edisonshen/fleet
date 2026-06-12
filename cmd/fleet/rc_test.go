package main

import (
	"bytes"
	"encoding/json"
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
// `fleet rc up`. Native model: up never spawns anything; a default-on
// project reports already_acquired.
func TestRCCLI_UpEmitsJSONEnvelope(t *testing.T) {
	rcTestFleetHome(t)

	out := &bytes.Buffer{}
	_ = runRCUp(out, "demo", false)

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
	if resp.Outcome != rc.OutcomeAlreadyAcquired {
		t.Errorf("response outcome=%q want already_acquired (default-on)", resp.Outcome)
	}
}

// TestRCCLI_UpReenablesOptedOutProject: `fleet rc up` removes the
// rc-disabled opt-out marker and reports acquired.
func TestRCCLI_UpReenablesOptedOutProject(t *testing.T) {
	rcTestFleetHome(t)
	if err := rc.WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runRCUp(out, "demo", false); err != nil {
		t.Fatalf("runRCUp: %v\nstdout:\n%s", err, out.String())
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Outcome != rc.OutcomeAcquired {
		t.Fatalf("outcome=%q want acquired", resp.Outcome)
	}
	if !rc.Enabled("demo") {
		t.Fatalf("project must be enabled after rc up")
	}
}

// TestRCCLI_UpRespawnOnly_LegacyCompat pins the half-upgraded-install
// contract: pre-native skill copies shell out `fleet rc up <p>
// --respawn-only --idempotent` every 30s. The new binary must answer
// with a stable no-op (exit 0 / already_acquired when enabled; exit 10
// / not_enabled when opted out) and never mutate anything.
func TestRCCLI_UpRespawnOnly_LegacyCompat(t *testing.T) {
	rcTestFleetHome(t)

	out := &bytes.Buffer{}
	if err := runRCUp(out, "demo", true); err != nil {
		t.Fatalf("respawn-only on enabled project must succeed: %v", err)
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v", err)
	}
	if resp.Outcome != rc.OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want already_acquired", resp.Outcome)
	}

	// Opted-out project: not_enabled (exit 10 — the old skill latches
	// quiet on this), and the opt-out must survive.
	if err := rc.WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out.Reset()
	err := runRCUp(out, "demo", true)
	if err == nil {
		t.Fatalf("respawn-only on opted-out project must return the not_enabled sentinel")
	}
	if got := rcOutcomeFromErr(err); got != rc.OutcomeNotEnabled {
		t.Fatalf("sentinel outcome=%q want not_enabled", got)
	}
	if !rc.DisabledMarkerPresent("demo") {
		t.Fatalf("respawn-only must NOT remove the opt-out marker")
	}
}

// TestRCCLI_UpAcceptsLegacyFlags: cobra must keep parsing the retired
// flags (--cwd/--idempotent/--coord-id) so old skill copies don't get
// unknown-flag errors from a new binary.
func TestRCCLI_UpAcceptsLegacyFlags(t *testing.T) {
	rcTestFleetHome(t)
	cmd := newRCUpCmd()
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"demo", "--respawn-only", "--idempotent", "--coord-id", "abcd1234", "--cwd", t.TempDir()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("legacy flag shape must keep parsing + succeeding: %v\n%s", err, out.String())
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Outcome != rc.OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want already_acquired (pure no-op)", resp.Outcome)
	}
}

// TestRCCLI_DownEmitsJSONEnvelope pins the down-side wire shape.
// Native model: down on a clean project writes the opt-out marker and
// reports released; a second down is already_released.
func TestRCCLI_DownEmitsJSONEnvelope(t *testing.T) {
	rcTestFleetHome(t)
	restoreKill := rc.SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	out := &bytes.Buffer{}
	if err := runRCDown(out, "demo"); err != nil {
		t.Fatalf("runRCDown: %v\nstdout:\n%s", err, out.String())
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Cmd != "down" || resp.Project != "demo" {
		t.Errorf("response shape drift: %+v", resp)
	}
	if resp.Outcome != rc.OutcomeReleased {
		t.Errorf("outcome=%q want released (opt-out marker written)", resp.Outcome)
	}
	if rc.Enabled("demo") {
		t.Errorf("project must be disabled after rc down")
	}
	// Kill-switch honesty (/review adversarial F3): the response must
	// tell the operator a LIVE coord keeps RC until exit/handoff.
	if !strings.Contains(resp.Diagnostic, "fleet handoff") {
		t.Errorf("down diagnostic must surface the live-coord handoff remediation; got %q", resp.Diagnostic)
	}

	// Second down: already_released sentinel (exit 0).
	out.Reset()
	if err := runRCDown(out, "demo"); err != nil {
		t.Fatalf("second runRCDown: %v", err)
	}
	resp = rcResponse{}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v", err)
	}
	if resp.Outcome != rc.OutcomeAlreadyReleased {
		t.Errorf("outcome=%q want already_released", resp.Outcome)
	}
}

// TestRCCLI_ConnectIsDeprecatedNoop: connect no longer drives
// send-keys; it reports native_default (exit 0) with a diagnostic so
// legacy handoff docs that still say `fleet rc connect <p>` don't
// break the operator's flow.
func TestRCCLI_ConnectIsDeprecatedNoop(t *testing.T) {
	rcTestFleetHome(t)
	out := &bytes.Buffer{}
	if err := runRCConnect(out, "demo"); err != nil {
		t.Fatalf("deprecated connect must exit 0: %v\nstdout:\n%s", err, out.String())
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Outcome != rc.OutcomeNativeDefault {
		t.Fatalf("outcome=%q want native_default", resp.Outcome)
	}
	if !strings.Contains(resp.Diagnostic, "deprecated") {
		t.Errorf("diagnostic must say deprecated; got %q", resp.Diagnostic)
	}

	// Opted-out project: the diagnostic must surface the disabled
	// state + recovery command (surface, don't silo).
	if err := rc.WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out.Reset()
	if err := runRCConnect(out, "demo"); err != nil {
		t.Fatalf("connect on opted-out project must still exit 0: %v", err)
	}
	resp = rcResponse{}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v", err)
	}
	if !strings.Contains(resp.Diagnostic, "fleet rc up demo") {
		t.Errorf("opted-out diagnostic must point at `fleet rc up demo`; got %q", resp.Diagnostic)
	}
}

// TestRCCLI_ListEmitsDisabledProjects: list enumerates the EXCEPTIONS
// (opt-outs), not the default-on majority.
func TestRCCLI_ListEmitsDisabledProjects(t *testing.T) {
	rcTestFleetHome(t)
	if err := rc.WriteDisabledMarker("alpha"); err != nil {
		t.Fatalf("WriteDisabledMarker alpha: %v", err)
	}
	if err := rc.WriteDisabledMarker("beta"); err != nil {
		t.Fatalf("WriteDisabledMarker beta: %v", err)
	}
	// A legacy rc-enabled marker must NOT show up.
	if err := rc.WriteMarker("legacy"); err != nil {
		t.Fatalf("WriteMarker legacy: %v", err)
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
	if len(resp.DisabledProjects) != 2 {
		t.Errorf("disabled_projects len=%d want 2 (%v)", len(resp.DisabledProjects), resp.DisabledProjects)
	}
	for _, p := range resp.DisabledProjects {
		if p == "legacy" {
			t.Errorf("legacy rc-enabled marker must not appear in the disabled list")
		}
	}
	// /review adversarial F5: the v0.12 "projects" key (which listed
	// ENABLED projects) must stay empty so stale consumers fail loudly
	// instead of silently inverting meaning.
	if len(resp.Projects) != 0 {
		t.Errorf("legacy projects key must stay unset; got %v", resp.Projects)
	}
}

// TestRCCLI_StatusDefaultEnabled: native model — status on a pristine
// project reports acquired (enabled) with Enabled=true.
func TestRCCLI_StatusDefaultEnabled(t *testing.T) {
	rcTestFleetHome(t)
	out := &bytes.Buffer{}
	if err := runRCStatus(out, "demo", false); err != nil {
		t.Fatalf("runRCStatus: %v\nstdout:\n%s", err, out.String())
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Outcome != rc.OutcomeAcquired {
		t.Errorf("outcome=%q want acquired (default-on)", resp.Outcome)
	}
	if resp.State == nil || !resp.State.Enabled {
		t.Errorf("state.enabled must be true by default; got %+v", resp.State)
	}
}

// TestRCCLI_StatusReportsAbsentWhenOptedOut pins the opt-out
// observability path.
func TestRCCLI_StatusReportsAbsentWhenOptedOut(t *testing.T) {
	rcTestFleetHome(t)
	if err := rc.WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
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
		t.Errorf("outcome=%q want absent (opted out)", resp.Outcome)
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
		{rc.OutcomeNativeDefault, 0},
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

// TestRCCLI_StatusNoArg_AllDefaultOn_ExitsZero (codex review iter-2
// [P2] regression): on a clean install with zero rc-disabled markers,
// no-arg `fleet rc status` is the HEALTHY steady-state and must exit 0
// (success outcome, empty projects list) — not absent/exit 11, which
// broke shell health checks.
func TestRCCLI_StatusNoArg_AllDefaultOn_ExitsZero(t *testing.T) {
	rcTestFleetHome(t)
	out := &bytes.Buffer{}
	if err := runRCStatus(out, "", false); err != nil {
		t.Fatalf("no-arg status on all-default-on host must succeed (exit 0); got %v", err)
	}
	var resp rcResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v\nstdout:\n%s", err, out.String())
	}
	if resp.Outcome != rc.OutcomeAcquired {
		t.Errorf("outcome=%q want acquired (healthy default-on)", resp.Outcome)
	}
	if len(resp.DisabledProjects) != 0 {
		t.Errorf("disabled_projects=%v want empty (no opt-outs)", resp.DisabledProjects)
	}

	// With an opt-out present, the list carries it (still exit 0).
	if err := rc.WriteDisabledMarker("optout"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out.Reset()
	if err := runRCStatus(out, "", false); err != nil {
		t.Fatalf("no-arg status with opt-outs must still succeed: %v", err)
	}
	resp = rcResponse{}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON envelope: %v", err)
	}
	if len(resp.DisabledProjects) != 1 || resp.DisabledProjects[0] != "optout" {
		t.Errorf("disabled_projects=%v want [optout]", resp.DisabledProjects)
	}
}
