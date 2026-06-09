//go:build linux || darwin

package main

// doctor_status_test.go — the stuck-handoff surface wired into `fleet status`
// (DESIGN-handoff-drain-storm-leak PR6). A coordinator handoff that was
// requested but never completed (a pending spawn-fresh queue file with no
// live leader) must be SURFACED with the canonical plain-English line +
// the `fleet doctor` next step — never silent (surface-don't-silo).

import (
	"bytes"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/queue"
)

func TestStatus_StuckHandoff_Surfaced(t *testing.T) {
	// Inject deps: one pending coord handoff for "stuckproj" with NO live
	// leader -> stuck. A second project "liveproj" has a pending handoff but a
	// LIVE leader -> in-flight, NOT surfaced.
	d := doctorTestDeps()
	d.ListPendingQueue = func() ([]string, error) {
		return []string{"/q/spawn-fresh-a.json", "/q/spawn-fresh-b.json"}, nil
	}
	d.ReadQueue = func(p string) (queue.SpawnFresh, error) {
		if strings.Contains(p, "-a.json") {
			return queue.SpawnFresh{OldAgentID: "a", Project: "stuckproj", TaskID: CoordTaskIDPrefix + "stuckproj"}, nil
		}
		return queue.SpawnFresh{OldAgentID: "b", Project: "liveproj", TaskID: CoordTaskIDPrefix + "liveproj"}, nil
	}
	// The scan uses the READ-ONLY Diagnose-based leader probe (codex PR6
	// iter-15): liveproj has a HEALTHY leader (LeaseHealthOK) -> in-flight,
	// stuckproj has none -> stuck.
	d.Diagnose = func(project string) coordlock.LeaseDiagnosis {
		if project == "liveproj" {
			return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthOK, HasRecord: true}
		}
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthNone}
	}

	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	s := out.String()

	if !strings.Contains(s, "the handoff to a fresh coordinator didn't complete") {
		t.Errorf("missing canonical stuck-handoff line:\n%s", s)
	}
	if !strings.Contains(s, "fleet doctor") {
		t.Errorf("missing the `fleet doctor` next step:\n%s", s)
	}
	if !strings.Contains(s, "stuckproj") {
		t.Errorf("stuck project not named:\n%s", s)
	}
	if strings.Contains(s, "liveproj") {
		t.Errorf("in-flight handoff (live leader) was wrongly surfaced:\n%s", s)
	}
	// No jargon in the status surface.
	for _, w := range []string{"tmux", "flock", "epoch", "STONITH", "lease", "fence", "wedged"} {
		if strings.Contains(strings.ToLower(s), strings.ToLower(w)) {
			t.Errorf("status stuck-handoff line leaked jargon %q:\n%s", w, s)
		}
	}
}

func TestStatus_NoStuckHandoff_Silent(t *testing.T) {
	d := doctorTestDeps() // ListPendingQueue returns nil -> nothing pending
	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	if out.Len() != 0 {
		t.Errorf("healthy state should be silent, got:\n%s", out.String())
	}
}

// codex PR6 iter-5 [P2]: with FLEET_LEASE_FAILOVER=0 (legacy mode) the
// lease-aware stuck-handoff signal doesn't apply — LeaderPresent is always
// false, so a benign pending legacy handoff must NOT be advertised as stuck.
func TestStatus_FailoverDisabled_NoStuckHandoff(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "0")
	d := doctorTestDeps()
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-a.json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: "a", Project: "legacyproj", TaskID: CoordTaskIDPrefix + "legacyproj"}, nil
	}
	d.LeaderPresent = func(string) bool { return false } // legacy: no lease

	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	if out.Len() != 0 {
		t.Errorf("legacy mode (failover off) must not surface stuck handoffs, got:\n%s", out.String())
	}
}

// codex PR6 iter-8 [P2]: `fleet status --json` must still surface a stuck
// handoff (on stderr, keeping stdout valid JSON) — the JSON branch returns
// before the human-path scan, so the JSON callers would otherwise get `[]`
// and no `fleet doctor` signal.
func TestStatus_StuckHandoff_OnJSONPath(t *testing.T) {
	stubEnsureFresh(t)
	t.Setenv("FLEET_HOME", t.TempDir())
	t.Setenv("FLEET_LEASE_FAILOVER", "1")

	d := doctorTestDeps()
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-x.json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: "x", Project: "jsonstuck", TaskID: CoordTaskIDPrefix + "jsonstuck"}, nil
	}
	d.LeaderPresent = func(string) bool { return false }
	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var stdout, stderr bytes.Buffer
	if err := runStatus(&statusOpts{jsonOut: true}, &stdout, &stderr, "dev"); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	if !strings.Contains(stderr.String(), "the handoff to a fresh coordinator didn't complete") {
		t.Errorf("--json path must surface the stuck handoff on stderr; stderr:\n%s", stderr.String())
	}
	// stdout must remain valid JSON (the stuck line went to stderr).
	if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "[") {
		t.Errorf("--json stdout should be a JSON array, got: %s", stdout.String())
	}
}

// codex PR6 iter-15 [P2]: the read-only status stuck-handoff scan must use the
// Diagnose-based leader probe (which never creates lock files), NOT the
// flock-creating production LeaderPresent. Assert LeaderPresent is never
// called from the scan.
func TestStatus_StuckHandoff_DoesNotCallLeaderPresent(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	d := doctorTestDeps()
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-a.json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: "a", Project: "p", TaskID: CoordTaskIDPrefix + "p"}, nil
	}
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthNone}
	}
	d.LeaderPresent = func(string) bool {
		t.Errorf("read-only stuck-handoff scan called the flock-creating LeaderPresent")
		return false
	}
	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	if !strings.Contains(out.String(), "the handoff to a fresh coordinator didn't complete") {
		t.Errorf("scan should still surface the stuck handoff via Diagnose:\n%s", out.String())
	}
}

// codex PR6 iter-16 [P2]: a coordinator mid-startup (LeaseHealthBooting: flock
// taken, epoch not written yet) is a leader-is-coming state — a pending coord
// handoff during that window must NOT be surfaced as stuck.
func TestStatus_BootingHolder_NotStuck(t *testing.T) {
	t.Setenv("FLEET_LEASE_FAILOVER", "1")
	d := doctorTestDeps()
	d.ListPendingQueue = func() ([]string, error) { return []string{"/q/spawn-fresh-a.json"}, nil }
	d.ReadQueue = func(string) (queue.SpawnFresh, error) {
		return queue.SpawnFresh{OldAgentID: "a", Project: "bootproj", TaskID: CoordTaskIDPrefix + "bootproj"}, nil
	}
	d.Diagnose = func(string) coordlock.LeaseDiagnosis {
		return coordlock.LeaseDiagnosis{Health: coordlock.LeaseHealthBooting}
	}
	orig := stuckHandoffStatusFn
	stuckHandoffStatusFn = func() doctorDeps { return d }
	t.Cleanup(func() { stuckHandoffStatusFn = orig })

	var out, errOut bytes.Buffer
	emitStuckHandoffSection(&out, &errOut)
	if out.Len() != 0 {
		t.Errorf("a booting coordinator must not be surfaced as a stuck handoff, got:\n%s", out.String())
	}
}
