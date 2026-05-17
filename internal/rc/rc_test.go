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
