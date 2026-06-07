package rc

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
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
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
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

// TestResolveWorkingDir_CanonicalizesRelativeOverride (codex round-6
// P2): when the operator passes `--cwd .`, the resolved value must
// be the absolute path. Without canonicalization, rc-state.json
// stores "." but lsof reports the absolute cwd — every later
// argv/cwd verify-fails, breaking Down/Connect.
func TestResolveWorkingDir_CanonicalizesRelativeOverride(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)

	got, err := ResolveWorkingDir("demo", ".")
	if err != nil {
		t.Fatalf("ResolveWorkingDir: %v", err)
	}
	if got == "." || got == "" {
		t.Fatalf("override must be canonicalized to absolute path; got %q", got)
	}
	// Absolute paths start with / on Unix. We're not asserting an
	// exact value (depends on test runner's cwd) — just that it
	// LOOKS absolute. filepath.Abs guarantees this.
	if got[0] != '/' {
		t.Fatalf("resolved path %q should be absolute (start with /)", got)
	}
}

// TestResetAll_EnumeratesMarkerlessState (codex round-5 P2): the
// emergency reset-all path must catch projects with a markerless
// rc-state.json — that's the corruption case `fleet rc reset` is
// for. Without the glob, the pre-fix code returned success while
// silently leaving the state file behind.
func TestResetAll_EnumeratesMarkerlessState(t *testing.T) {
	root := withFleetHome(t)
	stubAgentListEmpty(t)

	// Markered project (List path).
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

	// Stub verifier + kill so Down doesn't error or signal the test
	// process. The verifier returning true means Down would call
	// killFn, which we stub to a no-op.
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
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return false })
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
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
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

// TestSweepAllProjects_ReleasesStaleVersionDaemons (T8 leak-rc-daemon-lifecycle PR-B):
// SweepAllProjects must also reap daemons whose ClaudeVersion disagrees
// with the current binary. Without this, stale daemons across version
// upgrades persist forever (the 2026-05-29 OOM root cause).
func TestSweepAllProjects_ReleasesStaleVersionDaemons(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")

	host, _ := os.Hostname()
	// "stale": marker present, state alive, recorded version older
	// than current.
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

	// "healthy": marker present, state alive, recorded version matches.
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

	// Stale: state should be cleaned up + kill fired.
	if _, err := ReadState("stale"); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("stale daemon state should be reaped by Sweep (version mismatch); err=%v", err)
	}
	if len(killed) != 1 {
		t.Fatalf("expected 1 kill for stale daemon; got %d (%v)", len(killed), killed)
	}
	// codex P1: marker MUST survive the Class 2/3 reap — the project is
	// still opted in to RC, and the coord's --respawn-only tick needs the
	// marker present to bring a fresh daemon back. Down() would remove it
	// and silently disable RC.
	if !MarkerPresent("stale") {
		t.Fatalf("stale project marker must be PRESERVED after self-heal reap (else --respawn-only returns not_enabled)")
	}
	// Healthy: untouched.
	if _, err := ReadState("healthy"); err != nil {
		t.Fatalf("healthy daemon state should be preserved by Sweep; err=%v", err)
	}
}

// TestSweepAllProjects_ReleasesDeadOwnerDaemons (T8 follow-through):
// dead-owner daemons must also be reaped by Sweep so the orphan
// reconcile path can self-heal across coord crashes.
func TestSweepAllProjects_ReleasesDeadOwnerDaemons(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")
	// Override owner-alive: pretend "dead-coord" is dead.
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
	// codex P1: dead-owner reap also keeps the marker so the next coord's
	// --respawn-only tick respawns under a fresh owner rather than finding
	// RC disabled.
	if !MarkerPresent("orphan") {
		t.Fatalf("dead-owner project marker must be PRESERVED after self-heal reap")
	}
}

// TestDefaultOwnerAlive_RecordMissingIsDead (codex P2 semantics): the
// ONLY definitive dead-owner signal is a missing agent record. A present
// record whose tmux session isn't found on the current socket is AMBIGUOUS
// (possibly a different tmux server) and must NOT be treated as dead — else
// `fleet status` run from the wrong socket reaps live RC daemons.
func TestDefaultOwnerAlive_RecordMissingIsDead(t *testing.T) {
	withFleetHome(t)

	// Empty coordID → skip check → alive.
	if !defaultOwnerAlive("") {
		t.Fatalf("empty coordID must be treated as alive (skip dead-owner check)")
	}

	// No record on disk → definitive dead.
	if defaultOwnerAlive("abcd1234") {
		t.Fatalf("missing agent record must be treated as dead-owner")
	}

	// Record present, tmux session bogus (not found on current socket) →
	// ambiguous → alive (we must not reap a live daemon owned by a coord
	// on another server).
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

// TestUp_FreshAcquire_DropsOwnerWithoutRecord (codex P2): when the caller
// passes a well-formed coord_id that has NO agent record yet (coordinator
// legacy/upgrade path), Up must persist owning_coord_id as EMPTY — not the
// ghost id. Persisting the ghost would make the next tick's dead-owner check
// reap + respawn + re-persist the same ghost, flapping the listener forever.
func TestUp_FreshAcquire_DropsOwnerWithoutRecord(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { return "2.1.156", nil }
	t.Cleanup(func() { claudeVersionFn = prevV })
	// Owner record does NOT exist for this coord-id.
	restoreExists := SetOwnerRecordExistsForTest(func(coordID string) bool { return false })
	defer restoreExists()
	restoreSpawn := SetSpawnerForTest(func(string) (int, error) { return os.Getpid(), nil })
	defer restoreSpawn()

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", CoordID: "abcd1234", SkipSpawn: true, InjectedPID: 4242})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAcquired {
		t.Fatalf("outcome=%q want acquired", out)
	}
	got, _ := ReadState("demo")
	if got.OwningCoordID != "" {
		t.Fatalf("owner with no agent record must be persisted empty; got %q", got.OwningCoordID)
	}
}

// TestUp_FreshAcquire_PersistsOwnerWithRecord: the complement — when the
// agent record DOES exist, the owner is persisted.
func TestUp_FreshAcquire_PersistsOwnerWithRecord(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { return "2.1.156", nil }
	t.Cleanup(func() { claudeVersionFn = prevV })
	restoreExists := SetOwnerRecordExistsForTest(func(coordID string) bool { return coordID == "abcd1234" })
	defer restoreExists()
	restoreSpawn := SetSpawnerForTest(func(string) (int, error) { return os.Getpid(), nil })
	defer restoreSpawn()

	if _, err := Up("demo", UpOpts{Cwd: "/tmp/demo", CoordID: "abcd1234", SkipSpawn: true, InjectedPID: 4242}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	got, _ := ReadState("demo")
	if got.OwningCoordID != "abcd1234" {
		t.Fatalf("owner with an agent record must be persisted; got %q", got.OwningCoordID)
	}
}

// TestReapDaemonKeepMarker_AbortsOnConcurrentRespawn (codex P1 race guard):
// SweepAllProjects snapshots state WITHOUT the lock, then reapDaemonKeepMarker
// locks + removes. If a coord respawned a FRESH daemon (new PID + current
// version) between snapshot and lock, the reap must abort — removing the
// fresh state would leave marker + no-state + live listener, forcing the
// next respawn-only tick into the contested path. The race guard re-reads
// under the lock and bails when PID differs or the record is no longer stale.
func TestReapDaemonKeepMarker_AbortsOnConcurrentRespawn(t *testing.T) {
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
	// Snapshot the sweeper would have taken: stale-version daemon, old PID.
	snapshot := RecordedState{
		Project: "demo", PID: 11111, HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-live",
	}
	// On-disk state has ALREADY been refreshed by a concurrent respawn:
	// new PID, current version, alive owner → no longer stale.
	fresh := RecordedState{
		Project: "demo", PID: 22222, HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.156", OwningCoordID: "coord-live",
	}
	if err := WriteState(fresh); err != nil {
		t.Fatalf("WriteState fresh: %v", err)
	}

	reapDaemonKeepMarker("demo", snapshot, "2.1.156")

	// Fresh state must survive untouched, no kill issued, marker preserved.
	got, err := ReadState("demo")
	if err != nil {
		t.Fatalf("fresh state must be preserved after aborted reap; err=%v", err)
	}
	if got.PID != 22222 {
		t.Fatalf("fresh state.pid=%d want preserved 22222 (reap must not clobber concurrent respawn)", got.PID)
	}
	if len(killed) != 0 {
		t.Fatalf("no kill must fire when state changed under lock; killed=%v", killed)
	}
	if !MarkerPresent("demo") {
		t.Fatalf("marker must remain present")
	}
}

// TestUp_AdoptHealthy_BackfillsEmptyOwner (codex P2): a daemon enabled by
// `fleet rc up <project>` (no --coord-id) records an empty owning_coord_id.
// When the coord tick later adopts it with --coord-id, the healthy-adopt
// branch must STAMP the owner so dead-owner self-heal works after a future
// coord crash. Without the backfill the owner stays empty forever.
func TestUp_AdoptHealthy_BackfillsEmptyOwner(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreKill := SetKillFnForTest(func(pid int) { t.Fatalf("healthy adopt must NOT kill") })
	defer restoreKill()

	host, _ := os.Hostname()
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	// Operator-enabled daemon: current version, EMPTY owner.
	if err := WriteState(RecordedState{
		Project: "demo", PID: os.Getpid(), HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
		ClaudeVersion: "2.1.156", OwningCoordID: "",
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	// Coord tick adopts with a coord-id.
	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", CoordID: "abcd1234"})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want already_acquired (healthy adopt)", out)
	}
	got, _ := ReadState("demo")
	if got.OwningCoordID != "abcd1234" {
		t.Fatalf("owner backfill failed: owning_coord_id=%q want abcd1234", got.OwningCoordID)
	}
	if got.PID != os.Getpid() {
		t.Fatalf("adopt must not change PID; got %d", got.PID)
	}
}

// TestUp_AdoptHealthy_DoesNotClobberExistingOwner: the backfill is
// idempotent — when the record ALREADY has an owner, a different coord's
// adopt tick must NOT overwrite it (only empty owners are stamped).
func TestUp_AdoptHealthy_DoesNotClobberExistingOwner(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreKill := SetKillFnForTest(func(pid int) { t.Fatalf("healthy adopt must NOT kill") })
	defer restoreKill()

	host, _ := os.Hostname()
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}
	if err := WriteState(RecordedState{
		Project: "demo", PID: os.Getpid(), HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
		ClaudeVersion: "2.1.156", OwningCoordID: "coord-live",
	}); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	if _, err := Up("demo", UpOpts{Cwd: "/tmp/demo", CoordID: "abcd1234"}); err != nil {
		t.Fatalf("Up: %v", err)
	}
	got, _ := ReadState("demo")
	if got.OwningCoordID != "coord-live" {
		t.Fatalf("existing owner must be preserved; owning_coord_id=%q want coord-live", got.OwningCoordID)
	}
}

// TestReapDaemonKeepMarker_SkipsWhenCwdUnverifiable (codex P2): the auto-
// sweep runs on every `fleet status` and is destructive. When the strict
// cwd verifier can't confirm the recorded working_dir (lsof missing, or the
// PID was reused by another project sharing the fleet-coord prefix), the
// reap must skip BOTH the kill and the state removal — killing would take
// down an unrelated listener; removing state would untrack a live foreign
// daemon. Expect: no kill, state preserved.
func TestReapDaemonKeepMarker_SkipsWhenCwdUnverifiable(t *testing.T) {
	withFleetHome(t)
	withStubVersionAndOwner(t, "2.1.156")
	// argv check passes (shared prefix) but strict cwd CANNOT be confirmed.
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

	reapDaemonKeepMarker("demo", rec, "2.1.156")

	if len(killed) != 0 {
		t.Fatalf("must not kill when strict cwd unverifiable; killed=%v", killed)
	}
	if _, err := ReadState("demo"); err != nil {
		t.Fatalf("state must be preserved when cwd unverifiable (could be a live foreign daemon); err=%v", err)
	}
}

// TestSweepAllProjects_ProbesVersionOnce (codex P2): a multi-project sweep
// must shell out to `claude --version` at most ONCE — not per project, and
// not again under each lock. A hung claude binary would otherwise multiply
// the stall across every project on a read-only `fleet status`.
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
	// Three projects, all stale-version (each would trigger a heal reap).
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

// TestSweepMarkerless_SkipsWhenCwdUnverifiable (codex P1): the markerless
// (Class-1) sweep path runs on every read-only `fleet status`. On hosts
// without lsof it must NOT kill a PID it can't strictly verify — a reused
// stale markerless PID could be another project's healthy listener. Expect:
// no kill, state preserved (left for a host/tick that can verify).
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
	// markerless orphan: state present + alive PID, NO marker.
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

// TestReapDaemonKeepMarker_AbortsOnCrossHostState (codex P2): the caller
// filters cross-host entries on an UNLOCKED snapshot. If state is rewritten
// for another host before the lock, the reap must abort entirely — deleting
// another host's live rc-state.json (shared/migrated FLEET_HOME) would leave
// its daemon untracked. Expect: state preserved, no kill, marker preserved.
func TestReapDaemonKeepMarker_AbortsOnCrossHostState(t *testing.T) {
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
	// Snapshot the sweeper took (looked local at the time).
	snapshot := RecordedState{
		Project: "demo", PID: 11111, HostID: "other-host", WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-x",
	}
	// On-disk state is now owned by ANOTHER host (rewritten after snapshot).
	onDisk := RecordedState{
		Project: "demo", PID: 11111, HostID: "other-host", WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, ClaudeVersion: "2.1.146", OwningCoordID: "coord-x",
	}
	if err := WriteState(onDisk); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	reapDaemonKeepMarker("demo", snapshot, "2.1.156")

	// Cross-host state must NOT be deleted, no kill issued.
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

	// Stub verifiers so the strict-cwd markerless reap proceeds (it's our
	// own PID); both argv + strict cwd must confirm before signaling.
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, expectedCwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, expectedCwd string) bool { return true })
	defer restoreStrict()
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

// withStubVersionAndOwner sets up the test seams used by self-healing
// Up: the claude --version probe is stubbed to a fixed string, and the
// owner-liveness probe is stubbed to return alive=true for any non-
// empty owner ID by default. Each test overrides as needed.
//
// leak-rc-daemon-lifecycle PR-B: self-healing Up needs both probes to
// decide whether the recorded daemon is stale (version-mismatch or
// dead-owner). Tests stub the seams so unit tests don't shell out to
// `claude --version` or check live agent records.
func withStubVersionAndOwner(t *testing.T, version string) {
	t.Helper()
	prevV := claudeVersionFn
	claudeVersionFn = func() (string, error) { return version, nil }
	prevO := ownerAliveFn
	ownerAliveFn = func(coordID string) bool {
		return coordID != "" // default: any non-empty owner is alive
	}
	prevE := ownerRecordExistsFn
	ownerRecordExistsFn = func(coordID string) bool {
		return coordID != "" // default: any non-empty owner has a record
	}
	t.Cleanup(func() {
		claudeVersionFn = prevV
		ownerAliveFn = prevO
		ownerRecordExistsFn = prevE
	})
}

// TestUp_SelfHeal_NoopForCurrentVersion (T1): recorded daemon has
// current claude_version and live owner → outcome already_acquired,
// no kill, no respawn, state unchanged.
func TestUp_SelfHeal_NoopForCurrentVersion(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()

	var killCalls int
	restoreKill := SetKillFnForTest(func(pid int) { killCalls++ })
	defer restoreKill()

	host, _ := os.Hostname()
	rec := RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.156",
		OwningCoordID: "coord-live",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want %q (current version + live owner = no-op)", out, OutcomeAlreadyAcquired)
	}
	if killCalls != 0 {
		t.Fatalf("kill must not fire for current-version daemon; got %d calls", killCalls)
	}
	got, _ := ReadState("demo")
	if got.PID != rec.PID || got.ClaudeVersion != rec.ClaudeVersion {
		t.Fatalf("state was mutated unexpectedly; got %+v want %+v", got, rec)
	}
}

// TestUp_SelfHeal_RespawnsOnStaleVersion (T2): recorded daemon's
// claude_version is older than current → killFn is called once,
// fresh daemon spawned, state updated with new PID + new version,
// outcome=OutcomeRespawnedStaleVersion.
func TestUp_SelfHeal_RespawnsOnStaleVersion(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, cwd string) bool { return true })
	defer restoreStrict()

	var killedPIDs []int
	restoreKill := SetKillFnForTest(func(pid int) { killedPIDs = append(killedPIDs, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	oldPID := os.Getpid()
	rec := RecordedState{
		Project:       "demo",
		PID:           oldPID,
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.146", // older
		OwningCoordID: "coord-live",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	const newPID = 99999
	out, err := Up("demo", UpOpts{
		Cwd:         "/tmp/demo",
		SkipSpawn:   true,
		InjectedPID: newPID,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeRespawnedStaleVersion {
		t.Fatalf("outcome=%q want %q", out, OutcomeRespawnedStaleVersion)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != oldPID {
		t.Fatalf("expected exactly one kill of oldPID=%d; got %v", oldPID, killedPIDs)
	}
	got, _ := ReadState("demo")
	if got.PID != newPID {
		t.Fatalf("state.pid=%d want newPID %d", got.PID, newPID)
	}
	if got.ClaudeVersion != "2.1.156" {
		t.Fatalf("state.claude_version=%q want 2.1.156 (current)", got.ClaudeVersion)
	}
}

// TestUp_SelfHeal_AbortsWhenCwdUnresolvable (codex P2 resolve-before-kill):
// a stale-version daemon needs respawn, but the replacement working_dir
// cannot be resolved (no --cwd, no meta repo_path, no live coord). Up MUST
// NOT kill the working listener — killing first then failing resolution
// would leave rc-state.json pointing at a dead PID with no respawn. Expect:
// error returned, killFn never called, original state preserved.
func TestUp_SelfHeal_AbortsWhenCwdUnresolvable(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t) // no live coord → no agent-record cwd
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()

	var killedPIDs []int
	restoreKill := SetKillFnForTest(func(pid int) { killedPIDs = append(killedPIDs, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	oldPID := os.Getpid()
	rec := RecordedState{
		Project:       "demo",
		PID:           oldPID,
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.146", // older → triggers self-heal
		OwningCoordID: "coord-live",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	// No Cwd override + no meta repo_path + empty agent list → cwd is
	// unresolvable, so self-heal must abort BEFORE the kill.
	_, err := Up("demo", UpOpts{SkipSpawn: true, InjectedPID: 99999})
	if err == nil {
		t.Fatalf("expected Up to error when replacement cwd is unresolvable")
	}
	if len(killedPIDs) != 0 {
		t.Fatalf("working daemon must NOT be killed when cwd unresolvable; killed=%v", killedPIDs)
	}
	got, rerr := ReadState("demo")
	if rerr != nil {
		t.Fatalf("original state must be preserved; ReadState err=%v", rerr)
	}
	if got.PID != oldPID {
		t.Fatalf("original state.pid=%d want preserved oldPID=%d", got.PID, oldPID)
	}
}

// TestUp_SelfHeal_AbortsWhenCwdUnverifiable (codex P2): a stale-version
// daemon needs respawn, cwd resolves fine, but the STRICT lsof cwd verifier
// can't confirm the PID (lsof missing / PID reuse). Up must NOT kill — the
// PID could be another project's healthy listener sharing the fleet-coord
// prefix. Expect: no kill, original state preserved, outcome already_acquired.
func TestUp_SelfHeal_AbortsWhenCwdUnverifiable(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	// strict cwd CANNOT be confirmed.
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, cwd string) bool { return false })
	defer restoreStrict()
	var killed []int
	restoreKill := SetKillFnForTest(func(pid int) { killed = append(killed, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	oldPID := os.Getpid()
	rec := RecordedState{
		Project: "demo", PID: oldPID, HostID: host, WorkingDir: "/tmp/demo",
		SessionPrefix: SessionPrefix, LastSpawnAt: time.Now().UTC(),
		ClaudeVersion: "2.1.146", OwningCoordID: "coord-live",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	out, err := Up("demo", UpOpts{Cwd: "/tmp/demo", SkipSpawn: true, InjectedPID: 99999})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAlreadyAcquired {
		t.Fatalf("outcome=%q want already_acquired (self-heal aborted)", out)
	}
	if len(killed) != 0 {
		t.Fatalf("must not kill when strict cwd unverifiable; killed=%v", killed)
	}
	got, _ := ReadState("demo")
	if got.PID != oldPID {
		t.Fatalf("original state must be preserved; got pid %d want %d", got.PID, oldPID)
	}
}

// TestUp_SelfHeal_RespawnsOnDeadOwner (T3): recorded daemon's owning
// coord is gone (agent record missing or tmux session dead) → killFn
// is called once, fresh daemon spawned, state updated with new owner,
// outcome=OutcomeRespawnedDeadOwner.
func TestUp_SelfHeal_RespawnsOnDeadOwner(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	// Override owner-alive: pretend recorded owner is dead.
	prevO := ownerAliveFn
	ownerAliveFn = func(coordID string) bool {
		return coordID != "dead-coord"
	}
	t.Cleanup(func() { ownerAliveFn = prevO })

	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, cwd string) bool { return true })
	defer restoreStrict()

	var killedPIDs []int
	restoreKill := SetKillFnForTest(func(pid int) { killedPIDs = append(killedPIDs, pid) })
	defer restoreKill()

	host, _ := os.Hostname()
	oldPID := os.Getpid()
	rec := RecordedState{
		Project:       "demo",
		PID:           oldPID,
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		ClaudeVersion: "2.1.156", // current
		OwningCoordID: "dead-coord",
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	const newPID = 88888
	const newOwner = "coord-fresh"
	out, err := Up("demo", UpOpts{
		Cwd:         "/tmp/demo",
		SkipSpawn:   true,
		InjectedPID: newPID,
		CoordID:     newOwner,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeRespawnedDeadOwner {
		t.Fatalf("outcome=%q want %q", out, OutcomeRespawnedDeadOwner)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != oldPID {
		t.Fatalf("expected exactly one kill of oldPID=%d; got %v", oldPID, killedPIDs)
	}
	got, _ := ReadState("demo")
	if got.OwningCoordID != newOwner {
		t.Fatalf("state.owning_coord_id=%q want %q", got.OwningCoordID, newOwner)
	}
}

// TestUp_SelfHeal_EmptyVersionForcesHeal (T4): legacy state.json
// (v1 backcompat) loaded with empty ClaudeVersion must be treated as
// "always stale" so one heal cycle fires to backfill the schema.
// Outcome is OutcomeRespawnedStaleVersion (legacy = stale).
func TestUp_SelfHeal_EmptyVersionForcesHeal(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")
	restoreVerify := SetVerifyPIDIsListenerForTest(func(pid int, prefix, cwd string) bool { return true })
	defer restoreVerify()
	restoreStrict := SetVerifyPIDCwdStrictForTest(func(pid int, cwd string) bool { return true })
	defer restoreStrict()
	restoreKill := SetKillFnForTest(func(pid int) {})
	defer restoreKill()

	host, _ := os.Hostname()
	rec := RecordedState{
		Project:       "demo",
		PID:           os.Getpid(),
		HostID:        host,
		WorkingDir:    "/tmp/demo",
		SessionPrefix: SessionPrefix,
		LastSpawnAt:   time.Now().UTC(),
		// ClaudeVersion + OwningCoordID intentionally empty (legacy v1).
	}
	if err := WriteState(rec); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
	if err := WriteMarker("demo"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	out, err := Up("demo", UpOpts{
		Cwd:         "/tmp/demo",
		SkipSpawn:   true,
		InjectedPID: 77777,
		CoordID:     "coord-new",
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeRespawnedStaleVersion {
		t.Fatalf("outcome=%q want %q (empty version must force heal)", out, OutcomeRespawnedStaleVersion)
	}
	got, _ := ReadState("demo")
	if got.ClaudeVersion != "2.1.156" {
		t.Fatalf("post-heal state.claude_version=%q want 2.1.156", got.ClaudeVersion)
	}
	if got.OwningCoordID != "coord-new" {
		t.Fatalf("post-heal state.owning_coord_id=%q want coord-new", got.OwningCoordID)
	}
}

// TestUp_FreshAcquire_RecordsVersionAndOwner: the fresh-acquire path
// (no prior state.json) must capture claude_version + owning_coord_id
// in the new state.json so subsequent ticks can self-heal.
func TestUp_FreshAcquire_RecordsVersionAndOwner(t *testing.T) {
	withFleetHome(t)
	stubAgentListEmpty(t)
	withStubVersionAndOwner(t, "2.1.156")

	const ownerID = "coord-xyz"
	out, err := Up("demo", UpOpts{
		Cwd:         "/tmp/demo",
		SkipSpawn:   true,
		InjectedPID: os.Getpid(),
		CoordID:     ownerID,
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	if out != OutcomeAcquired {
		t.Fatalf("outcome=%q want %q", out, OutcomeAcquired)
	}
	got, _ := ReadState("demo")
	if got.ClaudeVersion != "2.1.156" {
		t.Fatalf("ClaudeVersion=%q want 2.1.156", got.ClaudeVersion)
	}
	if got.OwningCoordID != ownerID {
		t.Fatalf("OwningCoordID=%q want %q", got.OwningCoordID, ownerID)
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
