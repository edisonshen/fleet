//go:build linux || darwin

package main

// Integration coverage for DESIGN-coord-lease-false-fence-prevention piece 1
// (task lease-fence-no-rival-rea-5a90, plan test 8): a coordinator whose
// supervisor's lease renewal STALLED (expired `active` record, no rival)
// must keep serving — `fleet lease-check` re-acquires the lease in place at
// the SAME epoch and exits 0, and a real loop.tick() proceeds without any
// lease-fenced skip. Before this fix the same setup exited 3 and the tick
// self-killed the coord's tmux session, stranding the project coordless
// (rainier, 2x in 9h on 2026-07-03/04).

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/state"
)

// epochJSON mirrors coordlock's on-disk epoch record for test-side
// inspection/mutation. Owner/Candidate stay raw so a rewrite preserves the
// exact identity bytes; the int64 fields avoid a float64 round-trip that
// would corrupt ~1e18 monotonic stamps.
type epochJSON struct {
	Epoch         int64           `json:"epoch"`
	State         string          `json:"state"`
	Owner         json.RawMessage `json:"owner"`
	Candidate     json.RawMessage `json:"candidate,omitempty"`
	Host          string          `json:"host"`
	BootID        string          `json:"boot_id"`
	RenewedAtMono int64           `json:"renewed_at_mono"`
	RenewedAtWall int64           `json:"renewed_at_wall"`
}

func epochPathFor(t *testing.T, fleetHome, project string) string {
	t.Helper()
	return filepath.Join(fleetHome, "projects", project, ".locks", "coordinator.epoch")
}

func readEpochJSON(t *testing.T, path string) epochJSON {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	var rec epochJSON
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("unmarshal epoch: %v", err)
	}
	return rec
}

// staleifyEpoch rewinds renewed_at_mono by 60s (2x the 30s TTL) so the
// record reads as a stalled renewal, and returns the stale stamp for
// later "was it refreshed" comparisons.
func staleifyEpoch(t *testing.T, path string) int64 {
	t.Helper()
	rec := readEpochJSON(t, path)
	rec.RenewedAtMono -= 60 * int64(1e9)
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal epoch: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write stale epoch: %v", err)
	}
	return rec.RenewedAtMono
}

// Plan test 8a: `fleet lease-check` against our OWN expired active lease
// (no rival) exits 0 and re-acquires in place — same epoch, refreshed
// renewed_at, state active, owner unchanged.
func TestLeaseCheck_StalledRenewalReacquiresSameEpoch(t *testing.T) {
	bin := buildFleetBinary(t)
	const project = "stall-reacquire"
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	// The TEST process is the "supervisor": it owns the lease, and it is an
	// ancestor of the lease-check subprocess below.
	lease, acquired, err := coordlock.AcquireLease(project, "stall-sup")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("pre-acquire lease failed (acquired=%v err=%v)", acquired, err)
	}
	t.Cleanup(lease.Release)

	epochPath := epochPathFor(t, fleetHome, project)
	before := readEpochJSON(t, epochPath)
	staleMono := staleifyEpoch(t, epochPath)

	// Read-only default first (codex iter-2 [P1]): WITHOUT --reacquire the
	// check must fence (exit 3) and leave the stale record untouched — only
	// the tick's opt-in path may renew.
	roCmd := exec.Command(bin, "lease-check", "--project", project,
		"--pid", strconv.Itoa(os.Getpid()))
	roCmd.Env = append(os.Environ(),
		"FLEET_HOME="+fleetHome,
		"FLEET_LEASE_FAILOVER=1",
	)
	roOut, roErr := roCmd.CombinedOutput()
	var roExit *exec.ExitError
	if !errors.As(roErr, &roExit) || roExit.ExitCode() != 3 {
		t.Fatalf("read-only lease-check on a stalled lease must exit 3, got err=%v\noutput:\n%s", roErr, roOut)
	}
	if got := readEpochJSON(t, epochPath); got.RenewedAtMono != staleMono {
		t.Fatalf("read-only lease-check must not renew, renewed_at=%d want stale %d",
			got.RenewedAtMono, staleMono)
	}

	// --reacquire + --pid must be REJECTED (codex iter-4 [P2]: --pid would
	// otherwise be a write primitive for arbitrary local callers).
	rejCmd := exec.Command(bin, "lease-check", "--project", project,
		"--pid", strconv.Itoa(os.Getpid()), "--reacquire")
	rejCmd.Env = append(os.Environ(),
		"FLEET_HOME="+fleetHome,
		"FLEET_LEASE_FAILOVER=1",
	)
	rejOut, rejErr := rejCmd.CombinedOutput()
	var rejExit *exec.ExitError
	if !errors.As(rejErr, &rejExit) || rejExit.ExitCode() != 1 {
		t.Fatalf("--reacquire with --pid must be rejected (exit 1), got err=%v\noutput:\n%s", rejErr, rejOut)
	}

	// The supervisor's renewal has "stalled" (no heartbeat started). A
	// lease-check --reacquire (default ppid flow: this fleet subprocess's
	// parent IS the test process, the lease owner) must RE-ACQUIRE, not
	// fence (exit 3 was the pre-fix behavior).
	cmd := exec.Command(bin, "lease-check", "--project", project, "--reacquire")
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+fleetHome,
		"FLEET_LEASE_FAILOVER=1",
	)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			t.Fatalf("lease-check must exit 0 (re-acquire in place), got exit %d\noutput:\n%s",
				exitErr.ExitCode(), out)
		}
		t.Fatalf("lease-check: %v\noutput:\n%s", runErr, out)
	}

	after := readEpochJSON(t, epochPath)
	if after.Epoch != before.Epoch {
		t.Errorf("re-acquire must keep the SAME epoch: got %d want %d", after.Epoch, before.Epoch)
	}
	if after.State != "active" {
		t.Errorf("state = %q, want active", after.State)
	}
	if after.RenewedAtMono <= staleMono {
		t.Errorf("renewed_at_mono must be refreshed past the stale stamp: got %d stale %d",
			after.RenewedAtMono, staleMono)
	}
	if string(after.Owner) != string(before.Owner) {
		t.Errorf("owner identity must be byte-identical: got %s want %s", after.Owner, before.Owner)
	}
}

// Plan test 8b: a REAL loop.tick() against a stalled-renewal lease keeps
// serving — no lease-fenced skip, no self-exit — and the tick's own
// lease-check leaves a refreshed same-epoch active record behind.
func TestCoordIntegration_StalledLeaseTickReacquiresAndProceeds(t *testing.T) {
	env := setupCoordIntegration(t, "stall-tick")
	env.plantCoord(t)
	initGitRepo(t, env.repoCwd)
	env.bindRepo(t)

	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	lease, acquired, err := coordlock.AcquireLease(env.project, "stall-tick-sup")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("pre-acquire lease failed (acquired=%v err=%v)", acquired, err)
	}
	t.Cleanup(lease.Release)

	epochPath := epochPathFor(t, env.fleetHome, env.project)
	before := readEpochJSON(t, epochPath)
	staleMono := staleifyEpoch(t, epochPath)

	out := env.runTick(t)

	if strings.Contains(out, "lease-fenced") {
		t.Fatalf("a stalled-renewal tick with no rival must NOT fence: %s", out)
	}
	var res struct {
		Skipped bool   `json:"skipped"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse tick result: %v\nout=%s", err, out)
	}
	if res.Skipped {
		t.Fatalf("tick must proceed after in-place re-acquire, got skipped reason=%q", res.Reason)
	}

	after := readEpochJSON(t, epochPath)
	if after.Epoch != before.Epoch {
		t.Errorf("re-acquire must keep the SAME epoch: got %d want %d", after.Epoch, before.Epoch)
	}
	if after.State != "active" {
		t.Errorf("state = %q, want active", after.State)
	}
	if after.RenewedAtMono <= staleMono {
		t.Errorf("renewed_at_mono must be refreshed: got %d stale %d", after.RenewedAtMono, staleMono)
	}
}

// forgeLiveRival rewrites the epoch record as an in-progress takeover:
// state=fencing with a LIVE candidate. The candidate identity is copied
// byte-for-byte from Owner (this test process), so pid+pid_start are
// genuinely alive — ownExpiredRival must read it as a rival.
func forgeLiveRival(t *testing.T, path string) {
	t.Helper()
	rec := readEpochJSON(t, path)
	rec.State = "fencing"
	rec.Candidate = rec.Owner
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal epoch: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write rival epoch: %v", err)
	}
}

// Reviewer iter-2 (testing specialist), split-brain half of the change:
// a fencing record with a LIVE candidate must STILL make `fleet
// lease-check` exit 3 (own-expired-rival-fenced) through the real binary
// — the no-rival re-acquire must not have opened a bare-proceed hole.
func TestLeaseCheck_LiveRivalStillFencesExit3(t *testing.T) {
	bin := buildFleetBinary(t)
	const project = "rival-fence"
	fleetHome := t.TempDir()
	t.Setenv("FLEET_HOME", fleetHome)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	lease, acquired, err := coordlock.AcquireLease(project, "rival-sup")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("pre-acquire lease failed (acquired=%v err=%v)", acquired, err)
	}
	t.Cleanup(lease.Release)

	epochPath := epochPathFor(t, fleetHome, project)
	forgeLiveRival(t, epochPath)

	cmd := exec.Command(bin, "lease-check", "--project", project,
		"--pid", strconv.Itoa(os.Getpid()))
	cmd.Env = append(os.Environ(),
		"FLEET_HOME="+fleetHome,
		"FLEET_LEASE_FAILOVER=1",
	)
	out, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		t.Fatalf("lease-check with a live rival must exit non-zero, got err=%v\noutput:\n%s", runErr, out)
	}
	if exitErr.ExitCode() != 3 {
		t.Fatalf("lease-check with a live rival must exit 3, got %d\noutput:\n%s",
			exitErr.ExitCode(), out)
	}
	if !strings.Contains(string(out), "own-expired-rival-fenced") {
		t.Errorf("refusal must carry the own-expired-rival-fenced tag, got:\n%s", out)
	}
	if after := readEpochJSON(t, epochPath); after.State != "fencing" {
		t.Errorf("a fence verdict must not write; state = %q, want fencing", after.State)
	}
}

// Reviewer iter-2 (testing specialist): the same live-rival record through
// a REAL loop.tick() — the tick skips with reason "lease-fenced" (no
// mutation) and does NOT request session teardown (kill route deleted).
func TestCoordIntegration_LiveRivalTickSkipsAndStaysAlive(t *testing.T) {
	env := setupCoordIntegration(t, "rival-tick")
	env.plantCoord(t)
	initGitRepo(t, env.repoCwd)
	env.bindRepo(t)

	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	lease, acquired, err := coordlock.AcquireLease(env.project, "rival-tick-sup")
	if err != nil || !acquired || lease == nil {
		t.Fatalf("pre-acquire lease failed (acquired=%v err=%v)", acquired, err)
	}
	t.Cleanup(lease.Release)

	epochPath := epochPathFor(t, env.fleetHome, env.project)
	forgeLiveRival(t, epochPath)

	out := env.runTick(t)

	var res struct {
		Skipped bool   `json:"skipped"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse tick result: %v\nout=%s", err, out)
	}
	if !res.Skipped {
		t.Fatalf("a live-rival tick must skip, got %s", out)
	}
	if res.Reason != "lease-fenced" {
		t.Fatalf("reason = %q, want lease-fenced (skip, stay alive)", res.Reason)
	}
	if after := readEpochJSON(t, epochPath); after.State != "fencing" {
		t.Errorf("the fenced tick must not write; state = %q, want fencing", after.State)
	}
}
