package rc

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
)

// stubAgentListEmpty wires cwd's agent.List seam to return no records
// so ResolveWorkingDir falls through (or, with the override, doesn't
// consult it).
func stubAgentListEmpty(t *testing.T) {
	t.Helper()
	prev := agentList
	agentList = func() ([]*agent.Record, error) { return nil, nil }
	t.Cleanup(func() { agentList = prev })
}

func TestUp_FreshAcquireWritesMarkerAndState(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true, InjectedPID: os.Getpid()})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAcquired {
		t.Fatalf("outcome=%q want %q", out, OutcomeAcquired)
	}
	if !MarkerPresent("demo") {
		t.Fatalf("marker should be present post-Up")
	}
	rec, err := ReadState("demo")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if rec.PID != os.Getpid() {
		t.Fatalf("PID=%d want %d", rec.PID, os.Getpid())
	}
	if rec.WorkingDir != "/tmp/demo" {
		t.Fatalf("WorkingDir=%q want %q", rec.WorkingDir, "/tmp/demo")
	}
	if rec.LastSpawnAt.IsZero() {
		t.Fatalf("LastSpawnAt should be set")
	}
}

func TestUp_IdempotentReAcquireReturnsAlreadyAcquired(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	// Stub argv verifier so the test PID (os.Getpid()) is treated as
	// a real listener — production checks `ps -p <pid> -o args=` for
	// the session_prefix, which the test process obviously fails.
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix string) bool { return true })
	defer restoreVerify()

	host, _ := os.Hostname()
	// Pre-write state.json so the idempotent branch fires.
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want %q", out, OutcomeAlreadyAcquired)
	}
	// Marker should now be present even though we didn't pre-write it.
	if !MarkerPresent("demo") {
		t.Fatalf("idempotent Up should re-publish marker if absent")
	}
}

// TestDown_RemovesCorruptStateEvenWithoutMarker (codex round-4 P2):
// `fleet rc reset` (which delegates to Down) exists to clean corrupt
// rc-state.json. The pre-fix code returned already_released when the
// marker was absent, skipping RemoveState, leaving the operator stuck
// with a state file that `fleet rc status` keeps choking on. Now the
// path distinguishes ErrStateMissing from parse errors and proceeds
// to teardown even with marker absent + state corrupt.
func TestDown_RemovesCorruptStateEvenWithoutMarker(t *testing.T) {
	root := withFleetHome(t)
	// Marker absent. Write a malformed rc-state.json that ReadState
	// will refuse to parse.
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

// TestUp_AdoptVerifyFailRespawns (codex round-3 P2): if the recorded
// PID is alive but argv does not match session_prefix (kernel PID
// reuse, external kill), Up must NOT return already_acquired with
// the stale PID. It falls through to a fresh spawn so RC actually
// works. Surface-don't-silo: a stderr diagnostic is emitted too,
// but we don't assert that — just the outcome.
func TestUp_AdoptVerifyFailRespawns(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	host, _ := os.Hostname()
	if err := WriteState(RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}

	// Stub argv verifier to refuse — pretend the PID was recycled
	// for an unrelated process.
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix string) bool { return false })
	defer restoreVerify()

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true, InjectedPID: os.Getpid()})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAcquired {
		t.Fatalf("outcome=%q want %q (fresh spawn, not already_acquired)", out, OutcomeAcquired)
	}
}

// TestUp_DuplicateSpawnRefusal pins the codex round 2 invariant: when
// marker is present + state.json absent + a fleet-coord listener
// appears alive in workingDir, Up refuses to spawn or adopt. Operator
// must `fleet rc reset`. THIS IS T2 (plan-eng-review critical test).
func TestUp_DuplicateSpawnRefusal(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	// Seed: marker present, no state.json, listener "alive".
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	restore := SetDetectListenerForTest(func(workingDir string) (bool, error) {
		return true, nil
	})
	defer restore()

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true})
	if err == nil {
		t.Fatalf("Up should error on duplicate-spawn refusal; got out=%q err=nil", out)
	}
	if out != OutcomeContested {
		t.Fatalf("outcome=%q want %q", out, OutcomeContested)
	}
	// State.json must NOT have been written — the conservative refusal
	// path leaves the operator a clean slate to `reset` from.
	if _, sErr := ReadState("demo"); !errors.Is(sErr, ErrStateMissing) {
		t.Fatalf("state.json should not exist after refusal; got err=%v", sErr)
	}
}

func TestUp_ContestedOnConcurrentLock(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	// Manually acquire the per-project lock so withLock returns
	// contested. Open the lock file ourselves to simulate a second
	// invocation holding it.
	path, err := LockPath("demo")
	if err != nil {
		t.Fatalf("LockPath: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	// LOCK_EX|LOCK_NB on the same FD — succeeds. Second flock from
	// withLock will fail with EWOULDBLOCK.
	if err := flockExNB(f); err != nil {
		t.Fatalf("flock seed: %v", err)
	}
	defer func() { _ = flockUn(f) }()

	out, _ := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true})
	if out != OutcomeContested {
		t.Fatalf("outcome=%q want %q (concurrent-lock should yield contested)", out, OutcomeContested)
	}
}

func TestDown_RemovesMarkerAndStateIdempotently(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	// Stub kill so Down doesn't actually signal anything (the seed
	// uses os.Getpid() as the recorded PID — signaling that would
	// kill the test binary).
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	// First Up to seed.
	if _, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true, InjectedPID: os.Getpid()}); err != nil {
		t.Fatalf("seed Up: %v", err)
	}
	out, err := Down("demo")
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if out != OutcomeReleased {
		t.Fatalf("outcome=%q want %q", out, OutcomeReleased)
	}
	if MarkerPresent("demo") {
		t.Fatalf("marker should be gone after Down")
	}
	if _, err := ReadState("demo"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("state should be gone after Down; got %v", err)
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

func TestInspect_ReportsDisabledWithoutMarker(t *testing.T) {
	withFleetHome(t)
	s, err := Inspect("demo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if s.Enabled {
		t.Fatalf("Inspect should report disabled when marker absent")
	}
	if s.ListenerPID != 0 {
		t.Fatalf("ListenerPID=%d want 0", s.ListenerPID)
	}
}

func TestInspect_ReportsAliveWhenPIDAlive(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	if _, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true, InjectedPID: os.Getpid()}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	s, err := Inspect("demo")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !s.Enabled {
		t.Fatalf("Inspect should report enabled")
	}
	if !s.Alive {
		t.Fatalf("Inspect should report Alive=true for our own PID")
	}
}

func TestList_EnumeratesMarkedProjects(t *testing.T) {
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

func TestGateAttachFlag_RespectsMarker(t *testing.T) {
	withFleetHome(t)
	// InjectRemoteControlFlag requires the argv to be [sh -c "claude ..."]
	// where the body starts with "claude " (with trailing space).
	argv := []string{"sh", "-c", "claude --print"}
	out := GateAttachFlag("demo", argv, "fleet-coord-x")
	if !equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag should pass-through when marker absent\n got %v\nwant %v", out, argv)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	out = GateAttachFlag("demo", argv, "fleet-coord-x")
	if equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag should inject when marker present; got pass-through %v", out)
	}
}

func TestGateAttachFlag_RespectsEnvGate(t *testing.T) {
	withFleetHome(t)
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	t.Setenv("FLEET_RC_BOOTSTRAP_DISABLED", "1")
	argv := []string{"sh", "-c", "claude --print"}
	out := GateAttachFlag("demo", argv, "fleet-coord-x")
	if !equalArgv(argv, out) {
		t.Fatalf("GateAttachFlag should pass-through when env-gate set, even with marker")
	}
}

// TestUp_RespawnOnlyRefusesToCreateMarker (codex P1): the Python
// coord tick uses `fleet rc up <p> --respawn-only --idempotent` so
// the implicit-respawn path NEVER auto-enables RC on a project the
// operator hasn't opted in to. Marker absent + RespawnOnly=true MUST
// return OutcomeNotEnabled with no marker write and no state write.
func TestUp_RespawnOnlyRefusesToCreateMarker(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	if MarkerPresent("demo") {
		t.Fatalf("precondition: marker should be absent")
	}

	out, err := Up("demo", UpOpts{
		Cwd:         "/tmp/demo",
		SkipSpawn:   true,
		InjectedPID: os.Getpid(),
		RespawnOnly: true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeNotEnabled {
		t.Fatalf("outcome=%q want %q", out, OutcomeNotEnabled)
	}
	if MarkerPresent("demo") {
		t.Fatalf("RespawnOnly must NOT create marker for un-enabled project")
	}
	if _, sErr := ReadState("demo"); !errors.Is(sErr, ErrStateMissing) {
		t.Fatalf("RespawnOnly must NOT write rc-state.json; ReadState err=%v", sErr)
	}
}

// TestUp_RespawnOnlyWithMarkerProceeds (codex P1 follow-through):
// when the marker IS present (operator opted in), --respawn-only
// behaves like a normal Up — adopts an alive listener, respawns a
// dead one. This test covers the adopt path.
func TestUp_RespawnOnlyWithMarkerProceeds(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix string) bool { return true })
	defer restoreVerify()

	// Operator has opted in.
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

	out, err := Up("demo", UpOpts{
		Cwd:         "/tmp/demo",
		SkipSpawn:   true,
		InjectedPID: os.Getpid(),
		RespawnOnly: true,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want %q", out, OutcomeAlreadyAcquired)
	}
}

// TestDown_SkipsKillOnPIDReuse (codex P1): if the recorded PID is
// alive but its argv no longer matches our session_prefix (kernel
// recycled the PID for an unrelated process), Down must NOT signal
// it. Marker/state cleanup still proceeds.
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

	// Stub the verifier to refuse — pretend the PID is recycled.
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix string) bool { return false })
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

// TestDown_KillsWhenPIDVerified (codex P1, positive path): when
// the verifier confirms argv matches, Down DOES signal the PID.
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

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix string) bool { return true })
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

// TestSweepAllProjects_ReleasesMarkerlessOrphans (codex P2): the
// sweeper's whole point is to release entries where rc-state.json
// claims a live PID but the marker is gone (operator rm'd it
// manually). List() filters those out, so the sweeper MUST iterate
// rc-state.json directly. Pre-list-based sweep would never see
// these orphans.
func TestSweepAllProjects_ReleasesMarkerlessOrphans(t *testing.T) {
	withFleetHome(t)

	// "orphan": state.json present + alive PID + marker absent.
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

	// "healthy": state.json present + alive PID + marker present.
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

	// Stub verifier so Down doesn't refuse to kill (it's our own
	// PID).
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix string) bool { return true })
	defer restoreVerify()
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	if err := SweepAllProjects(); err != nil {
		t.Fatalf("SweepAllProjects: %v", err)
	}

	// Orphan: state should be cleaned up (Down ran).
	if _, err := ReadState("orphan"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("orphan state should be cleaned; err=%v", err)
	}
	// Healthy: untouched.
	if !MarkerPresent("healthy") {
		t.Fatalf("healthy marker should be preserved by sweep")
	}
	if _, err := ReadState("healthy"); err != nil {
		t.Fatalf("healthy state should be preserved by sweep; err=%v", err)
	}
}

// equalArgv is a tiny helper to dodge a slices import.
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
