//go:build integration && (linux || darwin)

// PR-A fork-bomb fence: this whole file launches REAL `fleet coord-run`
// supervisor SUBPROCESSES (build a binary, exec it), so it lives in the
// integration lane and is excluded from the default `go test ./...` gate.
// Each subprocess sets FLEET_STANDBY_TIMEOUT=3s so any lease-wrapped standby it
// (or a child it spawns) starts self-reaps in seconds rather than looping 10m.
// See docs/DESIGN-spawn-test-fork-bomb-root-fix.md.
//
// W11 — cross-boundary integration: a REAL `fleet coord-run` supervisor
// holds + releases a REAL coordinator.flock for its child's lifetime.
// This crosses the dispatch -> coord-run -> coordlock -> kernel-flock
// boundary that unit tests (which stub the lease) cannot. Per the task
// plan W11 is mandatory because the whole point of PR2 is the LIFETIME
// hold, which only a real process + real flock exercises.
//
// Flow (no time.Sleep ASSERTIONS — we poll for a condition with a bound):
//
//  1. isolated FLEET_HOME; pre-write the coord agent record.
//  2. launch `fleet coord-run --agent <id> --project <p> -- sleep 600`
//     with FLEET_LEASE_FAILOVER=1.
//  3. poll until a from-test AcquireLease returns acquired=false (the
//     supervisor holds the flock) AND the epoch names the supervisor as
//     the active owner with a fresh renewed_at (heartbeat established).
//  4. kill the supervisor; poll until a from-test AcquireLease SUCCEEDS
//     (kernel + the supervisor's defer Release freed the flock).
//  5. t.Cleanup reaps the temp home + the process.
package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

func TestCoordRun_Lease_Integration_RealFlockHeldThenReleased(t *testing.T) {
	bin := buildFleetBinary(t) // skips if no `go` toolchain

	const (
		agentID = "lw11intg"
		project = "lw11-integration"
	)
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	// Pre-write the coord record so the supervisor can stamp its identity.
	rec := agent.New(agentID)
	rec.Project = project
	rec.TaskID = CoordTaskIDPrefix + project
	if err := rec.Write(); err != nil {
		t.Fatalf("write record: %v", err)
	}

	// Launch the real supervisor. `sleep 600` is the long-lived child; we
	// kill the supervisor explicitly. SIGTERM the process group on cleanup
	// so the child can't outlive the test.
	cmd := exec.Command(bin, "coord-run", "--agent", agentID, "--project", project, "--", "sleep", "600")
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+fleetHome,
		"FLEET_LEASE_FAILOVER=1",
		"FLEET_STANDBY_TIMEOUT=3s", // PR-A: bound any standby to seconds, not 10m
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start coord-run: %v", err)
	}
	supPid := cmd.Process.Pid
	killed := false
	killSupervisor := func() {
		if killed {
			return
		}
		killed = true
		// Kill the whole process group (supervisor + sleep child).
		_ = syscall.Kill(-supPid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(killSupervisor)

	lockDir := filepath.Join(fleetHome, "projects", project, ".locks")
	epochPath := filepath.Join(lockDir, "coordinator.epoch")
	flockPath := filepath.Join(lockDir, "coordinator.flock")

	// Step 3: poll until the supervisor holds the lease (a from-test
	// nonblocking flock probe reports busy) AND the epoch names it active.
	// Do not call coordlock.AcquireLease as the probe here: on Linux that
	// participates in takeover/epoch logic and can perturb the exact state
	// this test is trying to observe.
	heldDeadline := time.Now().Add(15 * time.Second)
	held := false
	for time.Now().Before(heldDeadline) {
		busy, err := flockBusy(flockPath)
		if err == nil && busy {
			// Busy: a live holder owns the flock. Confirm it is OUR supervisor
			// via the epoch's active owner pid.
			if ownerPid, ok := coordlock.CurrentActiveOwnerPID(project); ok && ownerPid == supPid {
				held = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !held {
		t.Fatalf("supervisor pid %d did not hold the lease within 15s", supPid)
	}

	// W8-lite: the epoch record is active, names the supervisor, and has a
	// non-zero monotonic renewed_at (the heartbeat path stamped it). We do
	// not assert exact advancement timing (heartbeat is ~10s) — presence
	// of a fresh active owner record is the deterministic signal.
	var epoch struct {
		State         string            `json:"state"`
		Owner         struct{ Pid int } `json:"owner"`
		RenewedAtMono int64             `json:"renewed_at_mono"`
	}
	data, err := os.ReadFile(epochPath)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if err := json.Unmarshal(data, &epoch); err != nil {
		t.Fatalf("unmarshal epoch: %v", err)
	}
	if epoch.State != "active" {
		t.Errorf("epoch state = %q, want active", epoch.State)
	}
	if epoch.Owner.Pid != supPid {
		t.Errorf("epoch owner pid = %d, want supervisor %d", epoch.Owner.Pid, supPid)
	}
	if epoch.RenewedAtMono == 0 {
		t.Errorf("epoch renewed_at_mono = 0, want a monotonic stamp")
	}

	// W10 (end-to-end): the supervisor stamped its identity into the agent
	// record — supervisor_pid == the live process, non-empty pid_start +
	// exe_path. This is the identity KillCoordIfIdentityMatches targets.
	stamped, lerr := agent.Load(agentID)
	if lerr != nil {
		t.Fatalf("load record: %v", lerr)
	}
	if stamped.SupervisorPID != supPid {
		t.Errorf("record supervisor_pid = %d, want %d", stamped.SupervisorPID, supPid)
	}
	if stamped.SupervisorPidStart == 0 {
		t.Errorf("record supervisor_pid_start = 0, want a stamped start time")
	}
	if stamped.SupervisorExePath == "" {
		t.Errorf("record supervisor_exe_path empty, want the fleet binary path")
	}

	// W11 release: kill the supervisor; the kernel (+ its defer Release)
	// frees the flock. Poll until a from-test AcquireLease SUCCEEDS.
	killSupervisor()
	relDeadline := time.Now().Add(15 * time.Second)
	released := false
	for time.Now().Before(relDeadline) {
		lease, acquired, err := coordlock.AcquireLease(project, "probe-after-kill")
		if err == nil && acquired && lease != nil {
			lease.Release()
			released = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !released {
		t.Fatalf("flock was not released within 15s after killing supervisor pid %d", supPid)
	}
}

func flockBusy(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	return false, err
}

// T9 end-to-end + do-not-resurrect precondition (codex PR2 iter-3 [P2]):
// a real `fleet coord-run` that loses the lease race STANDS DOWN — it
// prints "a coord is already running for <project>", does NOT start the
// child, exits 0, and its coord.Cleanup ARCHIVES the pre-launch record.
// That archive is exactly what makes spawn.Spawn's merge see ErrNotFound
// and skip the resurrecting final write.
func TestCoordRun_Lease_Integration_StandDownArchivesRecord(t *testing.T) {
	bin := buildFleetBinary(t)

	const (
		agentID = "lsd1down"
		project = "lsd-standdown"
	)
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	// Pre-hold the lease from the test so the spawned coord-run must lose.
	leader, acquired, err := coordlock.AcquireLease(project, "test-leader")
	if err != nil || !acquired || leader == nil {
		t.Fatalf("test could not pre-acquire the lease (acquired=%v err=%v)", acquired, err)
	}
	t.Cleanup(leader.Release)

	// Pre-write the stand-down coord's record (as spawn would pre-launch).
	rec := agent.New(agentID)
	rec.Project = project
	rec.TaskID = CoordTaskIDPrefix + project
	if err := rec.Write(); err != nil {
		t.Fatalf("write record: %v", err)
	}

	// A sentinel the child would touch IF it ran — it must NOT, because
	// stand-down never starts the child.
	sentinel := filepath.Join(t.TempDir(), "child-ran")
	cmd := exec.Command(bin, "coord-run", "--agent", agentID, "--project", project,
		"--", "sh", "-c", "touch "+sentinel)
	cmd.Env = append(os.Environ(), "FLEET_HOME="+fleetHome, "FLEET_LEASE_FAILOVER=1",
		"FLEET_STANDBY_TIMEOUT=3s") // PR-A: bound any standby to seconds, not 10m
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("coord-run stand-down should exit 0, got %v\noutput:\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "a coord is already running for "+project) {
		t.Errorf("expected stand-down message, got:\n%s", out)
	}
	if _, serr := os.Stat(sentinel); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("child MUST NOT run on stand-down; sentinel %s exists", sentinel)
	}
	// The record must be ARCHIVED (not live) — coord.Cleanup ran.
	if _, lerr := agent.Load(agentID); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("stand-down coord record must be archived; Load err=%v", lerr)
	}
}

// coordLeaderCheck disambiguates stand-down from failure for spawn (codex
// PR2 iter-6 [P2]): false when the flag is off OR no active leader; true
// when a healthy leader holds the lease.
func TestCoordLeaderCheck(t *testing.T) {
	const project = "clc-test"
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	// Flag OFF -> always false (no lease in play).
	t.Setenv("FLEET_LEASE_FAILOVER", "")
	if coordLeaderCheck(project) {
		t.Error("coordLeaderCheck must be false when failover is off")
	}

	// Flag ON, no leader -> false.
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	if coordLeaderCheck(project) {
		t.Error("coordLeaderCheck must be false when no leader holds the lease")
	}

	// Flag ON, a live leader -> true.
	lease, acquired, err := coordlock.AcquireLease(project, "clc-leader")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("pre-acquire lease failed (acquired=%v err=%v)", acquired, err)
	}
	t.Cleanup(lease.Release)
	if !coordLeaderCheck(project) {
		t.Error("coordLeaderCheck must be true when a healthy leader holds the lease")
	}
}
