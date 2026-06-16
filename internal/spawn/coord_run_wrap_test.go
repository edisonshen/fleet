// Tests for the lease-failover coord-run wrap applied inside spawn.Spawn
// (DESIGN-handoff-drain-storm-leak PR2). The wrap lives in spawn — not at
// the dispatch call site — so fresh dispatch and dead-coord recovery get the
// lease supervisor, while documented legacy fallbacks can opt out explicitly.
// These exercise the pure decision + builder so they stay deterministic
// without a real tmux server:
//
//	W1  coordRunWrap shape: head = [fleet coord-run --agent <id>
//	    --project <p> --standby --standby-timeout <T> --], tail = the engine argv.
//	W2  flag OFF -> leaseFailoverEnabled() false -> no wrap.
//	W3  alreadyCoordRunWrapped guards against double/nested wrapping.
package spawn

import (
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestCoordRunWrap_Shape(t *testing.T) {
	// PR-A: these are production argv-shape assertions on the EXACT timeout
	// string. The CI integration step runs this lane with FLEET_STANDBY_TIMEOUT
	// exported globally; pin it empty so the test asserts the real caller/default
	// value, not the ambient test seam (the design's "must keep asserting" rule).
	t.Setenv("FLEET_STANDBY_TIMEOUT", "")
	const (
		fleetBin = "/usr/local/bin/fleet"
		agentID  = "deadbeef"
		project  = "projects-fleet"
	)
	engine := []string{"sh", "-c", "claude --dangerously-skip-permissions"}
	got := coordRunWrap(fleetBin, agentID, project, 2*time.Minute, engine)

	wantHead := []string{
		fleetBin, "coord-run", "--agent", agentID, "--project", project,
		"--standby", "--standby-timeout", "2m0s", "--",
	}
	if len(got) < len(wantHead) {
		t.Fatalf("wrapped argv too short: %v", got)
	}
	if !reflect.DeepEqual(got[:len(wantHead)], wantHead) {
		t.Errorf("argv head = %v, want %v", got[:len(wantHead)], wantHead)
	}
	if tail := got[len(wantHead):]; !reflect.DeepEqual(tail, engine) {
		t.Errorf("argv tail = %v, want %v", tail, engine)
	}
}

func TestCoordRunWrap_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	engine := []string{"sh", "-c", "claude"}
	orig := append([]string(nil), engine...)
	_ = coordRunWrap("/bin/fleet", "id1", "p1", time.Minute, engine)
	if !reflect.DeepEqual(engine, orig) {
		t.Errorf("input mutated: got %v want %v", engine, orig)
	}
}

func TestCoordRunWrap_DefaultTimeout(t *testing.T) {
	// PR-A: assert the true DefaultStandbyTimeout regardless of the lane's
	// ambient FLEET_STANDBY_TIMEOUT (see TestCoordRunWrap_Shape).
	t.Setenv("FLEET_STANDBY_TIMEOUT", "")
	const (
		fleetBin = "/usr/local/bin/fleet"
		agentID  = "sb-deadbeef"
		project  = "projects-fleet"
	)
	engine := []string{"sh", "-c", "claude"}
	got := coordRunWrap(fleetBin, agentID, project, 0, engine)

	want := []string{
		fleetBin, "coord-run", "--agent", agentID, "--project", project,
		"--standby", "--standby-timeout", DefaultStandbyTimeout.String(), "--",
	}
	want = append(want, engine...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("standby wrap = %v, want %v", got, want)
	}
}

// W2: PR4 flipped the default to ON. The gate is now ON unless
// FLEET_LEASE_FAILOVER is EXPLICITLY a disable token (0/false/off/no);
// unset/empty/anything-else -> ON. Mirrors coordlock.parseFailover.
//
// On non-linux/darwin (spawn_lease_other.go) the lease primitive is
// unsupported, so leaseFailoverEnabled is ALWAYS false regardless of the
// env (codex PR4 [P2]) — guard the expectations by GOOS so the test passes
// on every build target.
func TestLeaseFailoverEnabled_Gate(t *testing.T) {
	// NOT t.Parallel: uses t.Setenv, which panics under t.Parallel
	// (Go forbids parallel + per-test env mutation).
	leaseCapable := runtime.GOOS == "linux" || runtime.GOOS == "darwin"
	cases := map[string]bool{
		"": true, "1": true, "yes": true, "on": true, "anything": true, " 1 ": true,
		"0": false, "false": false, "off": false, "no": false, "FALSE": false, " 0 ": false,
	}
	for v, want := range cases {
		if !leaseCapable {
			want = false // unsupported platform: always off
		}
		t.Setenv("FLEET_LEASE_FAILOVER", v)
		if got := leaseFailoverEnabled(); got != want {
			t.Errorf("FLEET_LEASE_FAILOVER=%q -> %v, want %v (GOOS=%s)", v, got, want, runtime.GOOS)
		}
	}
}

func TestShouldLeaseWrap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                         string
		failoverOn, isCoord, disable bool
		want                         bool
	}{
		{"coord, flag on", true, true, false, true},
		{"coord, explicit legacy fallback", true, true, true, false},
		{"flag off", false, true, false, false},
		{"worker (not coord)", true, false, false, false},
		{"worker and flag off", false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldLeaseWrap(c.failoverOn, c.isCoord, c.disable); got != c.want {
				t.Errorf("shouldLeaseWrap(%v,%v,%v) = %v, want %v",
					c.failoverOn, c.isCoord, c.disable, got, c.want)
			}
		})
	}
}

// W3: the idempotency guard recognizes an already-wrapped argv so a
// re-spawn never nests coord-run inside coord-run.
func TestAlreadyCoordRunWrapped(t *testing.T) {
	t.Parallel()
	yes := [][]string{
		{"/usr/local/bin/fleet", "coord-run", "--agent", "x", "--", "sh"},
		{"/tmp/go-build/exe/fleet.test", "coord-run", "--", "true"},
	}
	no := [][]string{
		{"sh", "-c", "claude"},
		{"/usr/local/bin/fleet", "dispatch", "task"},
		{"/usr/local/bin/claude"},
		{},
		{"fleet"},
	}
	for _, a := range yes {
		if !alreadyCoordRunWrapped(a) {
			t.Errorf("alreadyCoordRunWrapped(%v) = false, want true", a)
		}
	}
	for _, a := range no {
		if alreadyCoordRunWrapped(a) {
			t.Errorf("alreadyCoordRunWrapped(%v) = true, want false", a)
		}
	}
}

func TestAugmentCoordRunWrap_AddsStandbyAndTimeout(t *testing.T) {
	// PR-A: assert the exact passed-in timeout regardless of the lane's ambient
	// FLEET_STANDBY_TIMEOUT (see TestCoordRunWrap_Shape).
	t.Setenv("FLEET_STANDBY_TIMEOUT", "")
	engine := []string{"sh", "-c", "claude"}
	got := augmentCoordRunWrap(
		[]string{"/usr/local/bin/fleet", "coord-run", "--agent", "a1", "--project", "p1", "--", "sh", "-c", "claude"},
		90*time.Second,
	)
	want := []string{
		"/usr/local/bin/fleet", "coord-run", "--agent", "a1", "--project", "p1",
		"--standby", "--standby-timeout", "1m30s", "--",
	}
	want = append(want, engine...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("augmented argv = %v, want %v", got, want)
	}
}

func TestAugmentCoordRunWrap_ReplacesWrongTimeout(t *testing.T) {
	// PR-A: assert the exact replacement timeout regardless of the lane's
	// ambient FLEET_STANDBY_TIMEOUT (see TestCoordRunWrap_Shape).
	t.Setenv("FLEET_STANDBY_TIMEOUT", "")
	got := augmentCoordRunWrap([]string{
		"/usr/local/bin/fleet", "coord-run", "--agent", "a1", "--project", "p1",
		"--standby", "--standby-timeout=5s", "--", "sh", "-c", "claude",
	}, 2*time.Minute)
	want := []string{
		"/usr/local/bin/fleet", "coord-run", "--agent", "a1", "--project", "p1",
		"--standby", "--standby-timeout", "2m0s", "--", "sh", "-c", "claude",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("augmented argv = %v, want %v", got, want)
	}
}

func TestShouldStandDownLeaseWrappedSpawn(t *testing.T) {
	t.Parallel()
	leader := func(string) bool { return true }

	if shouldStandDownLeaseWrappedSpawn("rainier", 0, leader,
		func(string) (int, bool) { return 1234, true }) {
		t.Fatal("missing supervisor pid must not stand down")
	}
	if shouldStandDownLeaseWrappedSpawn("rainier", 1234, nil,
		func(string) (int, bool) { return 5678, true }) {
		t.Fatal("missing leader check must not stand down")
	}
	if shouldStandDownLeaseWrappedSpawn("rainier", 1234, func(string) bool { return false },
		func(string) (int, bool) { return 5678, true }) {
		t.Fatal("no healthy leader must not stand down")
	}
	if shouldStandDownLeaseWrappedSpawn("rainier", 1234, leader,
		func(string) (int, bool) { return 1234, true }) {
		t.Fatal("this supervisor is the owner; must not stand down")
	}
	if !shouldStandDownLeaseWrappedSpawn("rainier", 1234, leader,
		func(string) (int, bool) { return 5678, true }) {
		t.Fatal("healthy different owner must stand down instead of exposing idle standby")
	}
}
