package rc

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// ---------------------------------------------------------------------------
// Enabled — the native opt-out gate.
// ---------------------------------------------------------------------------

// TestEnabled_DefaultOnForValidProject pins the native model's core
// semantics: a valid project with no markers at all is ENABLED.
func TestEnabled_DefaultOnForValidProject(t *testing.T) {
	withFleetHome(t)
	if !Enabled("demo") {
		t.Fatalf("Enabled(demo) must default to true under the native model")
	}
}

// TestEnabled_FalseForEmptyProject: legacy / untargeted dispatches have
// no project to key an opt-out on — stay conservative and skip the flag.
func TestEnabled_FalseForEmptyProject(t *testing.T) {
	withFleetHome(t)
	if Enabled("") {
		t.Fatalf("Enabled(\"\") must be false (no project to key an opt-out on)")
	}
}

// TestEnabled_FalseForInvalidProject: names that fail project-name
// validation can't carry a marker; mirror the empty-project posture.
func TestEnabled_FalseForInvalidProject(t *testing.T) {
	withFleetHome(t)
	for _, p := range []string{"../escape", "a/b", "."} {
		if Enabled(p) {
			t.Errorf("Enabled(%q) must be false for invalid project name", p)
		}
	}
}

// TestEnabled_FalseWhenDisabledMarkerPresent: `fleet rc down` writes the
// rc-disabled marker; Enabled flips off.
func TestEnabled_FalseWhenDisabledMarkerPresent(t *testing.T) {
	withFleetHome(t)
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	if Enabled("demo") {
		t.Fatalf("Enabled(demo) must be false with rc-disabled marker present")
	}
	if err := RemoveDisabledMarker("demo"); err != nil {
		t.Fatalf("RemoveDisabledMarker: %v", err)
	}
	if !Enabled("demo") {
		t.Fatalf("Enabled(demo) must return to true after the opt-out marker is removed")
	}
}

// TestEnabled_IgnoresLegacyEnabledMarker: the legacy rc-enabled opt-in
// marker is dead weight — its presence or absence must not affect the
// native gate.
func TestEnabled_IgnoresLegacyEnabledMarker(t *testing.T) {
	withFleetHome(t)
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if !Enabled("demo") {
		t.Fatalf("legacy rc-enabled marker must not affect Enabled (still default-on)")
	}
	if err := RemoveMarker("demo"); err != nil {
		t.Fatalf("RemoveMarker: %v", err)
	}
	if !Enabled("demo") {
		t.Fatalf("legacy rc-enabled marker removal must not affect Enabled")
	}
}

// ---------------------------------------------------------------------------
// Up — enable (remove opt-out). Never spawns anything.
// ---------------------------------------------------------------------------

// TestUp_RemovesDisabledMarker: Up on an opted-out project removes the
// rc-disabled marker and reports acquired.
func TestUp_RemovesDisabledMarker(t *testing.T) {
	withFleetHome(t)
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out, err := Up("demo", UpOpts{})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAcquired {
		t.Fatalf("outcome=%q want %q", out, OutcomeAcquired)
	}
	if DisabledMarkerPresent("demo") {
		t.Fatalf("rc-disabled marker must be removed by Up")
	}
	if !Enabled("demo") {
		t.Fatalf("project must be enabled post-Up")
	}
}

// TestUp_IdempotentWhenAlreadyEnabled: Up on a default-on project is a
// no-op reporting already_acquired.
func TestUp_IdempotentWhenAlreadyEnabled(t *testing.T) {
	withFleetHome(t)
	out, err := Up("demo", UpOpts{})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want %q", out, OutcomeAlreadyAcquired)
	}
}

// TestUp_NeverSpawns pins the native model's load-bearing negative: Up
// must not exec anything and must not write rc-state.json or the legacy
// rc-enabled marker. (The v0.12 Up spawned a standalone listener here.)
func TestUp_NeverSpawns(t *testing.T) {
	withFleetHome(t)
	if _, err := Up("demo", UpOpts{}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if _, err := ReadState("demo"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("Up must not write rc-state.json (no listener exists to record); err=%v", err)
	}
	if MarkerPresent("demo") {
		t.Fatalf("Up must not write the legacy rc-enabled marker")
	}
}

// TestUp_RespawnOnly_NoopCompat pins the half-upgraded-install contract:
// pre-native skill copies shell out `fleet rc up <p> --respawn-only
// --idempotent` every 30s tick. The native Up must be a PURE no-op for
// that call shape — stable outcome, zero filesystem mutation, no spawn.
//
//	enabled (default)  → already_acquired (exit 0; old skill: SPAWN_OK)
//	opted out          → not_enabled (exit 10; old skill latches quiet)
func TestUp_RespawnOnly_NoopCompat(t *testing.T) {
	withFleetHome(t)

	out, err := Up("demo", UpOpts{RespawnOnly: true})
	if err != nil {
		t.Fatalf("Up respawn-only: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want %q (enabled project)", out, OutcomeAlreadyAcquired)
	}

	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out, err = Up("demo", UpOpts{RespawnOnly: true})
	if err != nil {
		t.Fatalf("Up respawn-only (disabled): %v", err)
	}
	if out != OutcomeNotEnabled {
		t.Fatalf("outcome=%q want %q (opted-out project)", out, OutcomeNotEnabled)
	}
	// CRITICAL: RespawnOnly must never re-enable an opted-out project.
	if !DisabledMarkerPresent("demo") {
		t.Fatalf("RespawnOnly must NOT remove the rc-disabled opt-out marker")
	}
}

// TestUp_EmptyProjectErrors mirrors the historical contract.
func TestUp_EmptyProjectErrors(t *testing.T) {
	withFleetHome(t)
	out, err := Up("", UpOpts{})
	if err == nil || out != OutcomeError {
		t.Fatalf("Up(\"\") must error; got out=%q err=%v", out, err)
	}
}

// TestUp_ContestedOnConcurrentLock: the per-project NB-flock still
// guards marker mutations; a held lock yields contested.
func TestUp_ContestedOnConcurrentLock(t *testing.T) {
	withFleetHome(t)

	path, err := LockPath("demo")
	if err != nil {
		t.Fatalf("LockPath: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := flockExNB(f); err != nil {
		t.Fatalf("flock seed: %v", err)
	}
	defer func() { _ = flockUn(f) }()

	out, _ := Up("demo", UpOpts{})
	if out != OutcomeContested {
		t.Fatalf("outcome=%q want %q (concurrent-lock should yield contested)", out, OutcomeContested)
	}
}

// ---------------------------------------------------------------------------
// Down — disable (write opt-out) + legacy listener reap.
// ---------------------------------------------------------------------------

// TestDown_WritesDisabledMarker: the core opt-out path on a pristine
// (no legacy artifacts) project.
func TestDown_WritesDisabledMarker(t *testing.T) {
	withFleetHome(t)
	out, err := Down("demo")
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if !DisabledMarkerPresent("demo") {
		t.Fatalf("Down must write the rc-disabled opt-out marker")
	}
	if Enabled("demo") {
		t.Fatalf("project must be disabled post-Down")
	}
	// Second Down is idempotent.
	out, err = Down("demo")
	if err != nil {
		t.Fatalf("second Down: %v", err)
	}
	if out != OutcomeAlreadyReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeAlreadyReleased)
	}
}

// TestDown_ReapsLegacyArtifacts: a pre-native project with the legacy
// rc-enabled marker + rc-state.json gets fully cleaned (kill via
// verified PID is covered separately) and opted out.
func TestDown_ReapsLegacyArtifacts(t *testing.T) {
	withFleetHome(t)
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()

	host, _ := os.Hostname()
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	out, err := Down("demo")
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if MarkerPresent("demo") {
		t.Fatalf("legacy rc-enabled marker should be gone after Down")
	}
	if _, err := ReadState("demo"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("legacy state should be gone after Down; got %v", err)
	}
	if !DisabledMarkerPresent("demo") {
		t.Fatalf("Down must write the rc-disabled opt-out marker")
	}
}

// TestDown_RemovesCorruptStateEvenWithoutMarker (codex round-4 P2,
// kept): `fleet rc reset` (which delegates to Down) exists to clean
// corrupt rc-state.json. Marker absent + state corrupt must still
// proceed to teardown.
func TestDown_RemovesCorruptStateEvenWithoutMarker(t *testing.T) {
	root := withFleetHome(t)
	projDir := root + "/projects/demo"
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	statePath := projDir + "/rc-state.json"
	if err := os.WriteFile(statePath, []byte("{not valid json}"), 0o644); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	out, err := Down("demo")
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q (corrupt state must be cleaned, not skipped)", out, OutcomeReleased)
	}
	if _, statErr := os.Stat(statePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("corrupt rc-state.json should have been removed; stat err=%v", statErr)
	}
}

// TestDown_SkipsKillOnPIDReuse (codex P1, kept): between the legacy
// listener exiting and Down running, the kernel can recycle the PID.
// The argv verifier refusing must skip the kill but still clean files.
func TestDown_SkipsKillOnPIDReuse(t *testing.T) {
	withFleetHome(t)

	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return false })
	defer restoreVerify()

	var killCalls int
	restoreKill := SetKillFnForTest(func(pid int) { killCalls++ })
	defer restoreKill()

	out, err := Down("demo")
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if killCalls != 0 {
		t.Fatalf("kill must be skipped on PID-reuse; got %d call(s)", killCalls)
	}
	if MarkerPresent("demo") {
		t.Fatalf("marker should be removed even when kill is skipped")
	}
	if _, sErr := ReadState("demo"); !errors.Is(sErr, ErrStateMissing) {
		t.Fatalf("state should be removed even when kill is skipped; err=%v", sErr)
	}
}

// TestDown_KillsWhenPIDVerified (codex P1 positive path, kept): when
// the verifier confirms argv matches, Down DOES signal the legacy PID.
func TestDown_KillsWhenPIDVerified(t *testing.T) {
	withFleetHome(t)

	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()

	var killCalls int
	restoreKill := SetKillFnForTest(func(pid int) { killCalls++ })
	defer restoreKill()

	out, err := Down("demo")
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if killCalls != 1 {
		t.Fatalf("kill must fire when PID is verified; got %d call(s)", killCalls)
	}
}

// ---------------------------------------------------------------------------
// Reset — back to pristine default-on.
// ---------------------------------------------------------------------------

// TestReset_PreservesOptOut (/review adversarial F4): Reset cleans
// LEGACY listener state only — the rc-disabled opt-out marker is
// operator intent and must survive bit-for-bit in both directions.
// `fleet rc down` is the push kill-switch; an emergency command must
// not silently re-arm RC on a disarmed project.
func TestReset_PreservesOptOut(t *testing.T) {
	withFleetHome(t)

	// Opted-out project stays opted out across Reset.
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out, err := Reset("demo")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if !DisabledMarkerPresent("demo") {
		t.Fatalf("Reset must PRESERVE the rc-disabled opt-out marker (operator intent)")
	}

	// Default-on project stays default-on across Reset (no marker
	// appears as a side effect).
	if _, err := Reset("plain"); err != nil {
		t.Fatalf("Reset plain: %v", err)
	}
	if DisabledMarkerPresent("plain") {
		t.Fatalf("Reset must not opt a default-on project out")
	}
	if !Enabled("plain") {
		t.Fatalf("default-on project must stay enabled after Reset")
	}
}

// TestResetAll_EnumeratesMarkerlessState (codex round-5 P2, kept):
// reset-all must catch legacy-markered projects AND markerless
// state-only projects; opt-out markers are preserved (/review
// adversarial F4).
func TestResetAll_EnumeratesMarkerlessState(t *testing.T) {
	root := withFleetHome(t)

	// Legacy-markered project (List path).
	if err := WriteMarker("markered"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project:       "markered",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/markered",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState markered: %v", err)
	}

	// Markerless state-only project (glob path).
	projDir := root + "/projects/orphan"
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "orphan",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/orphan",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState orphan: %v", err)
	}
	if MarkerPresent("orphan") {
		t.Fatalf("test precondition: orphan must have no marker")
	}

	// Opt-out-only project — must be untouched by reset-all.
	if err := WriteDisabledMarker("optedout"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	out, err := Reset("")
	if err != nil {
		t.Fatalf("Reset(\"\"): %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if _, sErr := ReadState("markered"); !errors.Is(sErr, ErrStateMissing) {
		t.Fatalf("markered state should be removed; err=%v", sErr)
	}
	if _, sErr := ReadState("orphan"); !errors.Is(sErr, ErrStateMissing) {
		t.Fatalf("orphan (markerless) state should be removed by reset-all; err=%v", sErr)
	}
	for _, p := range []string{"markered", "orphan"} {
		if DisabledMarkerPresent(p) {
			t.Errorf("reset-all must not opt %q out (no rc-disabled side effect)", p)
		}
		if MarkerPresent(p) {
			t.Errorf("reset-all must remove %q's legacy rc-enabled marker", p)
		}
	}
	// Operator opt-out survives reset-all.
	if !DisabledMarkerPresent("optedout") {
		t.Errorf("reset-all must PRESERVE the rc-disabled opt-out marker")
	}
}

// ---------------------------------------------------------------------------
// Inspect / List / ListDisabled.
// ---------------------------------------------------------------------------

// TestInspect_DefaultEnabled: native model — a pristine project
// inspects as Enabled with no listener fields.
func TestInspect_DefaultEnabled(t *testing.T) {
	withFleetHome(t)
	s, err := Inspect("demo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !s.Enabled {
		t.Fatalf("Inspect must report enabled by default (native model)")
	}
	if s.ListenerPID != 0 {
		t.Fatalf("ListenerPID=%d want 0", s.ListenerPID)
	}
}

// TestInspect_ReportsDisabledWithOptOut: rc-disabled flips Enabled off.
func TestInspect_ReportsDisabledWithOptOut(t *testing.T) {
	withFleetHome(t)
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	s, err := Inspect("demo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Enabled {
		t.Fatalf("Inspect must report disabled when the opt-out marker is present")
	}
}

// TestInspect_ReportsAliveWhenPIDAlive (kept): legacy listener fields
// surface from rc-state.json while it's still on disk awaiting sweep.
func TestInspect_ReportsAliveWhenPIDAlive(t *testing.T) {
	withFleetHome(t)

	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	s, err := Inspect("demo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !s.Enabled {
		t.Fatalf("Inspect should report enabled (default)")
	}
	if !s.Alive {
		t.Fatalf("Inspect should report Alive=true for our own PID")
	}
}

// TestList_EnumeratesLegacyMarkedProjects (kept): List() remains the
// legacy rc-enabled enumeration used by Reset.
func TestList_EnumeratesLegacyMarkedProjects(t *testing.T) {
	withFleetHome(t)
	if err := WriteMarker("alpha"); err != nil {
		t.Fatalf("WriteMarker alpha: %v", err)
	}
	if err := WriteMarker("beta"); err != nil {
		t.Fatalf("WriteMarker beta: %v", err)
	}
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len=%d want 2 (%v)", len(got), got)
	}
	want := map[string]bool{"alpha": true, "beta": true}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected project in List: %q", p)
		}
	}
}

// TestListDisabled_EnumeratesOptOuts: ListDisabled returns exactly the
// projects carrying the rc-disabled marker.
func TestListDisabled_EnumeratesOptOuts(t *testing.T) {
	withFleetHome(t)
	if err := WriteDisabledMarker("optout"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	// A legacy-markered project must NOT appear in ListDisabled.
	if err := WriteMarker("legacy"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	got, err := ListDisabled()
	if err != nil {
		t.Fatalf("ListDisabled: %v", err)
	}
	if len(got) != 1 || got[0] != "optout" {
		t.Fatalf("ListDisabled=%v want [optout]", got)
	}
}

// ---------------------------------------------------------------------------
// GateAttachFlag — the I3 chokepoint, now opt-out.
// ---------------------------------------------------------------------------

// TestGateAttachFlag_DefaultInjects: native model — no markers means
// the flag IS baked in.
func TestGateAttachFlag_DefaultInjects(t *testing.T) {
	withFleetHome(t)
	// InjectRemoteControlFlag requires the argv to be [sh -c "claude ..."]
	// where the body starts with "claude " (with trailing space).
	argv := []string{"sh", "-c", "claude --print"}
	out := GateAttachFlag("demo", argv, "fleet-coord-x")
	if equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag must inject by default (native model); got pass-through %v", out)
	}
}

// TestGateAttachFlag_RespectsOptOut: the rc-disabled marker suppresses
// the inject.
func TestGateAttachFlag_RespectsOptOut(t *testing.T) {
	withFleetHome(t)
	argv := []string{"sh", "-c", "claude --print"}
	if err := WriteDisabledMarker("demo"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	out := GateAttachFlag("demo", argv, "fleet-coord-x")
	if !equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag must pass-through when project is opted out\n got %v\nwant %v", out, argv)
	}
}

// TestGateAttachFlag_EmptyProjectPassesThrough: no project, no flag.
func TestGateAttachFlag_EmptyProjectPassesThrough(t *testing.T) {
	withFleetHome(t)
	argv := []string{"sh", "-c", "claude --print"}
	out := GateAttachFlag("", argv, "fleet-coord-x")
	if !equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag must pass-through for empty project; got %v", out)
	}
}

// TestGateAttachFlag_RespectsEnvGate (kept): FLEET_RC_BOOTSTRAP_DISABLED
// suppresses the inject even though the default is on.
func TestGateAttachFlag_RespectsEnvGate(t *testing.T) {
	withFleetHome(t)
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1")
	argv := []string{"sh", "-c", "claude --print"}
	out := GateAttachFlag("demo", argv, "fleet-coord-x")
	if !equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag should pass-through when env-gate set, even with default-on")
	}
}

// ---------------------------------------------------------------------------
// Sweep — legacy daemon decay.
// ---------------------------------------------------------------------------

// TestSweepAllProjects_ReleasesStaleVersionDaemons (kept, adapted):
// stale-version legacy daemons are reaped — state, kill, AND the legacy
// marker (nothing respawns under the native model).
func TestSweepAllProjects_ReleasesStaleVersionDaemons(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")

	host, _ := os.Hostname()
	if err := WriteMarker("stale"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "stale",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/stale",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.146", // older than 2.1.156
		OwningCoordID: "coord-live",
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	if err := WriteMarker("healthy"); err != nil {
		t.Fatalf("WriteMarker healthy: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "healthy",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/healthy",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.156",
		OwningCoordID: "coord-live",
	}); err != nil {
		t.Fatalf("WriteState healthy: %v", err)
	}

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, expectedCwd string) bool { return true })
	defer restoreStrict()
	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}

	if _, err := ReadState("stale"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("stale daemon state should be reaped by Sweep (version mismatch); err=%v", err)
	}
	if len(killed) != 1 {
		t.Fatalf("expected 1 kill for stale daemon; got %d (%v)", len(killed), killed)
	}
	// Native model: the legacy marker goes with the daemon — there is no
	// respawn tick to preserve it for.
	if MarkerPresent("stale") {
		t.Fatalf("legacy rc-enabled marker must be REMOVED with the reaped daemon (native model: nothing respawns)")
	}
	// Healthy daemon (live owner, current version): untouched — a
	// pre-native coord may still be paired through it.
	if _, err := ReadState("healthy"); err != nil {
		t.Fatalf("healthy daemon state should be preserved by Sweep; err=%v", err)
	}
}

// TestSweepAllProjects_ReleasesDeadOwnerDaemons (kept, adapted).
func TestSweepAllProjects_ReleasesDeadOwnerDaemons(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")
	prevO := ownerAliveFn
	ownerAliveFn = func(coordID string) bool { return coordID != "dead-coord" }
	t.Cleanup(func() { ownerAliveFn = prevO })

	host, _ := os.Hostname()
	if err := WriteMarker("orphan"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "orphan",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/orphan",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.156",
		OwningCoordID: "dead-coord",
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, expectedCwd string) bool { return true })
	defer restoreStrict()
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}
	if _, err := ReadState("orphan"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("dead-owner daemon state should be reaped; err=%v", err)
	}
	if MarkerPresent("orphan") {
		t.Fatalf("legacy marker must be removed with the dead-owner daemon (no respawn under native model)")
	}
}

// TestSweepAllProjects_CleansDeadPIDRecords: new native-model cleanup —
// a legacy rc-state.json whose PID is already dead is pure leak (nothing
// will ever rewrite it); the sweep removes the file + legacy marker
// without signaling anything.
func TestSweepAllProjects_CleansDeadPIDRecords(t *testing.T) {
	withFleetHome(t)

	host, _ := os.Hostname()
	if err := WriteMarker("deadproj"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "deadproj",
		PID:           1<<30 + 7, // never a live PID
		HostID:        host,
		WorkingDir:    "/tmp/deadproj",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}
	if _, err := ReadState("deadproj"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("dead-PID rc-state.json must be cleaned by sweep; err=%v", err)
	}
	if MarkerPresent("deadproj") {
		t.Fatalf("legacy marker must be cleaned with the dead-PID record")
	}
	if len(killed) != 0 {
		t.Fatalf("dead-PID cleanup must never signal anything; killed=%v", killed)
	}
}

// TestSweepAllProjects_DeadPIDCleanup_NeverTouchesOptOut: the dead-PID
// cleanup is file hygiene only — it must not add or remove the
// rc-disabled opt-out marker.
func TestSweepAllProjects_DeadPIDCleanup_NeverTouchesOptOut(t *testing.T) {
	withFleetHome(t)

	host, _ := os.Hostname()
	if err := WriteDisabledMarker("deadproj"); err != nil {
		t.Fatalf("WriteDisabledMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "deadproj",
		PID:           1<<30 + 9,
		HostID:        host,
		WorkingDir:    "/tmp/deadproj",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}
	if !DisabledMarkerPresent("deadproj") {
		t.Fatalf("sweep must preserve the operator's rc-disabled opt-out marker")
	}
}

// TestDefaultOwnerAlive_RecordMissingIsDead (codex P2 semantics, kept).
func TestDefaultOwnerAlive_RecordMissingIsDead(t *testing.T) {
	withFleetHome(t)

	if !defaultOwnerAlive("") {
		t.Fatalf("empty coordID must be treated as alive (skip dead-owner check)")
	}
	if defaultOwnerAlive("abcd1234") {
		t.Fatalf("missing agent record must be treated as dead-owner")
	}
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("state.Bootstrap: %v", err)
	}
	rec := agent.New("dead5678")
	rec.TmuxSession = "fleet-coord-nonexistent-xyz"
	if err := rec.Write(); err != nil {
		t.Fatalf("write agent record: %v", err)
	}
	if !defaultOwnerAlive("dead5678") {
		t.Fatalf("record present + session-not-found-on-this-socket must be treated as alive (ambiguous), not reaped")
	}
}

// TestReapStaleLegacyDaemon_AbortsOnConcurrentRespawn (kept, renamed):
// the locked re-read must abort when the on-disk state no longer
// matches the unlocked snapshot.
func TestReapStaleLegacyDaemon_AbortsOnConcurrentRespawn(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	snapshot := RecordedState{
		Project: "demo", PID: 11111, HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-live",
	}
	fresh := RecordedState{
		Project: "demo", PID: 22222, HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.156", OwningCoordID: "coord-live",
	}
	if err := WriteState(fresh); err != nil {
		t.Fatalf("WriteState fresh: %v", err)
	}

	reapStaleLegacyDaemon("demo", snapshot, "2.1.156")

	got, err := ReadState("demo")
	if err != nil {
		t.Fatalf("fresh state must be preserved after aborted reap; err=%v", err)
	}
	if got.PID != 22222 {
		t.Fatalf("fresh state.pid=%d want preserved 22222 (reap must not clobber concurrent rewrite)", got.PID)
	}
	if len(killed) != 0 {
		t.Fatalf("no kill must fire when state changed under lock; killed=%v", killed)
	}
	if !MarkerPresent("demo") {
		t.Fatalf("aborted reap must leave the legacy marker untouched")
	}
}

// TestReapStaleLegacyDaemon_SkipsWhenCwdUnverifiable (codex P2, kept,
// renamed): destructive sweep must skip BOTH the kill and the state
// removal when the strict cwd verifier can't confirm.
func TestReapStaleLegacyDaemon_SkipsWhenCwdUnverifiable(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, cwd string) bool { return false })
	defer restoreStrict()
	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	rec := RecordedState{
		Project: "demo", PID: os.Getpid(), HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-live",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	reapStaleLegacyDaemon("demo", rec, "2.1.156")

	if len(killed) != 0 {
		t.Fatalf("must not kill when strict cwd unverifiable; killed=%v", killed)
	}
	if _, err := ReadState("demo"); err != nil {
		t.Fatalf("state must be preserved when cwd unverifiable (could be a live foreign daemon); err=%v", err)
	}
}

// TestSweepAllProjects_ProbesVersionOnce (codex P2, kept).
func TestSweepAllProjects_ProbesVersionOnce(t *testing.T) {
	withFleetHome(t)
	var probes int
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { probes++; return "2.1.156", nil }
	t.Cleanup(func() { claudeVersionFn = prevV })
	prevO := ownerAliveFn
	ownerAliveFn = func(string) bool { return true }
	t.Cleanup(func() { ownerAliveFn = prevO })
	restoreVerify := SetVerifyPIDIsListenerForTest(func(int, string, string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(int, string) bool { return true })
	defer restoreStrict()
	restoreKill := SetKillFnForTest(func(int) {})
	defer restoreKill()

	host, _ := os.Hostname()
	for _, p := range []string{"alpha", "beta", "gamma"} {
		if err := WriteMarker(p); err != nil {
			t.Fatalf("WriteMarker %s: %v", p, err)
		}
		if err := WriteState(RecordedState{
			Project: p, PID: os.Getpid(), HostID: host, WorkingDir: "/tmp/" + p,
			SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
			ClaudeVersion: "2.1.146", OwningCoordID: "coord-live",
		}); err != nil {
			t.Fatalf("WriteState %s: %v", p, err)
		}
	}

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}
	if probes != 1 {
		t.Fatalf("claude --version probed %d times across a 3-project sweep; want exactly 1", probes)
	}
}

// TestSweepAllProjects_NoVersionProbeWithoutSweepWork (codex P2, kept):
// the probe stays lazy — empty installs, markerless entries, and
// dead-PID cleanups never shell out to `claude --version`.
func TestSweepAllProjects_NoVersionProbeWithoutSweepWork(t *testing.T) {
	withFleetHome(t)
	var probes int
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { probes++; return "2.1.156", nil }
	t.Cleanup(func() { claudeVersionFn = prevV })
	restoreVerify := SetVerifyPIDIsListenerForTest(func(int, string, string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(int, string) bool { return true })
	defer restoreStrict()
	restoreKill := SetKillFnForTest(func(int) {})
	defer restoreKill()

	// Case A: no rc-state files at all.
	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects (empty): %v", err)
	}
	if probes != 0 {
		t.Fatalf("empty sweep must not probe claude --version; got %d", probes)
	}

	// Case B: only a markerless entry (no version check needed).
	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project: "orphan", PID: os.Getpid(), HostID: host, WorkingDir: "/tmp/orphan",
		SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects (markerless): %v", err)
	}
	if probes != 0 {
		t.Fatalf("markerless-only sweep must not probe claude --version; got %d", probes)
	}

	// Case C: only a dead-PID record (pure file cleanup — no probe).
	if err := WriteState(RecordedState{
		Project: "deadproj", PID: 1<<30 + 11, HostID: host, WorkingDir: "/tmp/deadproj",
		SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState deadproj: %v", err)
	}
	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects (dead-pid): %v", err)
	}
	if probes != 0 {
		t.Fatalf("dead-PID cleanup must not probe claude --version; got %d", probes)
	}
}

// TestSweepMarkerless_SkipsWhenCwdUnverifiable (codex P1, kept).
func TestSweepMarkerless_SkipsWhenCwdUnverifiable(t *testing.T) {
	withFleetHome(t)
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, cwd string) bool { return false })
	defer restoreStrict()
	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project: "orphan", PID: os.Getpid(), HostID: host, WorkingDir: "/tmp/orphan",
		SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("markerless sweep must not kill an unverifiable PID; killed=%v", killed)
	}
	if _, err := ReadState("orphan"); err != nil {
		t.Fatalf("state must be preserved when cwd unverifiable; err=%v", err)
	}
}

// TestReapStaleLegacyDaemon_AbortsOnCrossHostState (codex P2, kept,
// renamed).
func TestReapStaleLegacyDaemon_AbortsOnCrossHostState(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	snapshot := RecordedState{
		Project: "demo", PID: 11111, HostID: "other-host", WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-x",
	}
	onDisk := RecordedState{
		Project: "demo", PID: 11111, HostID: "other-host", WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-x",
	}
	if err := WriteState(onDisk); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	reapStaleLegacyDaemon("demo", snapshot, "2.1.156")

	if _, err := ReadState("demo"); err != nil {
		t.Fatalf("cross-host state must be preserved; ReadState err=%v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("must not signal a cross-host PID; killed=%v", killed)
	}
	if !MarkerPresent("demo") {
		t.Fatalf("marker must remain present")
	}
}

// TestSweepAllProjects_ReleasesMarkerlessOrphans (codex P2, kept).
func TestSweepAllProjects_ReleasesMarkerlessOrphans(t *testing.T) {
	withFleetHome(t)

	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project:       "orphan",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/orphan",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	if err := WriteMarker("healthy"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project:       "healthy",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/healthy",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	// Stub curVer="" — the PROBE-DEGRADED path: computeHealReason
	// SKIPS the version check entirely on an empty current version, so
	// the healthy entry survives via the degraded-probe skip (NOT via
	// a version match). Stubbing a real version here would flip this
	// fixture into the reap path because its recorded ClaudeVersion is
	// empty (/review specialist note).
	withStubVersionAndOwner(t, "")

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, expectedCwd string) bool { return true })
	defer restoreStrict()
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}

	if _, err := ReadState("orphan"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("orphan state should be cleaned; err=%v", err)
	}
	if !MarkerPresent("healthy") {
		t.Fatalf("healthy marker should be preserved by sweep")
	}
	if _, err := ReadState("healthy"); err != nil {
		t.Fatalf("healthy state should be preserved by sweep; err=%v", err)
	}
}

// withStubVersionAndOwner sets up the heal-rubric test seams: the
// claude --version probe returns a fixed string and the owner-liveness
// probe treats any non-empty owner as alive. Each test overrides as
// needed.
func withStubVersionAndOwner(t *testing.T, version string) {
	t.Helper()
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { return version, nil }
	prevO := ownerAliveFn
	ownerAliveFn = func(coordID string) bool {
		return coordID != "" // default: any non-empty owner is alive
	}
	t.Cleanup(func() {
		claudeVersionFn = prevV
		ownerAliveFn = prevO
	})
}

func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
