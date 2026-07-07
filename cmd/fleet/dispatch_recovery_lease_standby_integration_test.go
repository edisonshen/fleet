//go:build integration

// PR-A fork-bomb fence: this dispatch recovery test drives runDispatch on a
// coord (coordSpawn=true) under FLEET_LEASE_FAILOVER=1, which lease-wraps the
// replacement into a REAL `coord-run --standby` tmux pane. It lives in the
// integration lane (excluded from the default `go test ./...` gate) and sets
// FLEET_STANDBY_TIMEOUT=3s so an orphaned run self-reaps the standby in seconds.
// See docs/DESIGN-spawn-test-fork-bomb-root-fix.md.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestRunDispatch_DeadCoordRecovery_AdvertisesLockWinner pins codex iter-26 P1:
// when the dead-coord recovery path delivers the resume prompt via
// DeliverToCurrentOwner and a RACING standby (different agent-id than the one
// this dispatch just spawned) won the lease first, dispatch must advertise the
// WINNER's id in the "attach with" line — not the losing spawned standby's id.
//
//	spawn standby S  ──┐
//	                   ├─ both poll the lock; racer W wins, gets doc + marker
//	racing standby W ──┘
//	dispatch must print "attach with: fleet attach W"  (NOT S)
//
// Otherwise the TUI parses S from stdout, re-stamps the coord-spawn marker onto
// the losing standby S, overwrites the marker DeliverToCurrentOwner promoted to
// W, and attaches the operator to the wrong/dead session.
func TestRunDispatch_DeadCoordRecovery_AdvertisesLockWinner(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_LEASE_FAILOVER", "1")   // delivery branch only fires with failover on
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s") // PR-A: self-bound the standby this dispatch spawns

	// Stub the lock-owner delivery so we deterministically model "a racing
	// standby (winnerID) won the lease" without seeding a live foreign lease +
	// PID + tmux session. The returned record is the WINNER, distinct from the
	// standby runDispatch spawns below.
	const winnerID = "racewin1"
	winnerRec := agent.New(winnerID)
	winnerRec.TaskID = "coord-myproj"
	winnerRec.Project = "myproj"
	winnerRec.TmuxSession = "fleet-" + winnerID
	prevDeliver := deliverToCurrentOwner
	var deliverCalled bool
	deliverToCurrentOwner = func(opts handoffdelivery.Options) (*agent.Record, error) {
		deliverCalled = true
		if opts.Project != "myproj" {
			t.Errorf("delivery project = %q, want myproj", opts.Project)
		}
		return winnerRec, nil
	}
	t.Cleanup(func() { deliverToCurrentOwner = prevDeliver })

	deadRec := agent.New("deadc0d3")
	deadRec.TaskID = "coord-myproj"
	deadRec.Project = "myproj"
	deadRec.PID = 99999 // not alive → triggers recovery synth doc
	deadRec.TmuxSession = "fleet-deadc0d3"
	deadRec.Engine = "claude-code"
	if err := deadRec.Write(); err != nil {
		t.Fatalf("seed dead record: %v", err)
	}
	root := os.Getenv("FLEET_HOME")
	seedRecoveryRepo(t, root, "myproj")
	pdir := filepath.Join(root, "projects", "myproj")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	cs := map[string]any{"worker_agent_ids": map[string]string{}}
	csData, _ := json.Marshal(cs)
	csPath := filepath.Join(pdir, "coord-state.json")
	if err := os.WriteFile(csPath, csData, 0o644); err != nil {
		t.Fatalf("write coord-state: %v", err)
	}
	stale := time.Now().Add(-2 * coordFreshnessWindow)
	if err := os.Chtimes(csPath, stale, stale); err != nil {
		t.Fatalf("chtimes coord-state: %v", err)
	}

	opts := &dispatchOpts{
		taskID:          "coord-myproj",
		project:         "myproj",
		projectExplicit: true,
		coordSpawn:      true,
		command:         []string{"sleep", "60"},
		commandExplicit: true,
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	// Reap whatever standby this dispatch spawned (the LOSER of the race).
	live, _ := agent.List()
	for _, r := range live {
		if r.TaskID == "coord-myproj" && r.Project == "myproj" &&
			r.ID != "deadc0d3" && r.ID != winnerID {
			t.Cleanup(func() { _ = tmux.Kill(r.TmuxSession) })
		}
	}

	if !deliverCalled {
		t.Fatalf("expected dead-coord recovery to route delivery through DeliverToCurrentOwner; it did not\n%s", out.String())
	}
	got := out.String()
	wantAttach := "attach with: fleet attach " + winnerID
	if !strings.Contains(got, wantAttach) {
		t.Errorf("dispatch must advertise the lock WINNER; want %q in output, got:\n%s", wantAttach, got)
	}
	// The spawned standby (loser) id must NOT be the advertised attach target.
	if strings.Contains(got, "attach with: fleet attach deadc0d3") {
		t.Errorf("must not advertise the dead coord id as attach target:\n%s", got)
	}
	// A differing-owner note tells the operator the real coord landed elsewhere.
	if !strings.Contains(got, "won the coord lease") {
		t.Errorf("expected a note that another standby won the lease; got:\n%s", got)
	}
	// The authoritative machine-readable line the TUI parses (LAST
	// "agent <id> spawned") must name the WINNER, so the TUI promotes the
	// coord marker + attaches to the live coordinator, not the losing standby
	// (codex iter-27 P1). Assert the winner's line appears AFTER the note.
	winnerSpawn := "agent " + winnerID + " spawned"
	if !strings.Contains(got, winnerSpawn) {
		t.Errorf("expected authoritative %q line for the TUI to parse; got:\n%s", winnerSpawn, got)
	}
	if idx := strings.LastIndex(got, "agent "); !strings.HasPrefix(got[idx:], winnerSpawn) {
		t.Errorf("the LAST 'agent ... spawned' line must name the winner %s (TUI takes last match); got tail:\n%s",
			winnerID, got[idx:])
	}
}
