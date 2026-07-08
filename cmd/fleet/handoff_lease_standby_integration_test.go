//go:build integration

// PR-A fork-bomb fence: these handoff tests drive a coord handoff that reaches a
// REAL lease-wrapped spawn.Spawn (a `coord-run --standby` tmux pane), so they
// live in the integration lane and are excluded from the default `go test ./...`
// gate. Each sets FLEET_STANDBY_TIMEOUT=3s so an orphaned run self-reaps the
// standby in seconds instead of looping 10m and fork-bombing the box.
// See docs/DESIGN-spawn-test-fork-bomb-root-fix.md.
package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/handoffdelivery"
	"github.com/edisonshen/fleet/internal/queue"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// Codex P1 regression: a coord handoff against a LEGACY/bare coord (no lease
// record, so CurrentOwner never resolves an owner)
// must NOT loop the owner-poll until timeout and fail. deliverHandoffResumePrompt
// detects ErrNoOwnerObserved and falls back to a direct send into the live
// replacement's session — the pre-PR2 delivery path.
func TestDeliverHandoffResumePrompt_NoLeaseOwner_FallsBackToDirectSend(t *testing.T) {
	requireTmux(t)
	setupFleetHome(t)
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")

	const project = "rainier"
	// Live replacement with an idle shell so the typed prompt stays visible in
	// the pane for capture (the verified send types + submits into this pane).
	rep, err := agentSpawnForTest(t, t.TempDir(),
		[]string{"sh", "-c", "sleep 30"},
		project, "coord-"+project)
	if err != nil {
		t.Fatalf("seed replacement: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(rep.TmuxSession) })

	docPath := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(docPath, []byte("stub doc"), 0o644); err != nil {
		t.Fatal(err)
	}

	origDeps := handoffDeliveryDepsFn
	origTimeout := handoffDeliveryTimeout
	origPoll := handoffDeliveryPoll
	t.Cleanup(func() {
		handoffDeliveryDepsFn = origDeps
		handoffDeliveryTimeout = origTimeout
		handoffDeliveryPoll = origPoll
	})
	handoffDeliveryTimeout = 200 * time.Millisecond
	handoffDeliveryPoll = time.Millisecond
	// CurrentOwner never resolves an owner -> the legacy/bare-coord case.
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(string) (coordlock.Owner, bool) {
			return coordlock.Owner{}, false
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}

	out := &bytes.Buffer{}
	delivered, err := deliverHandoffResumePrompt(project, true, false, rep, docPath, out, out)
	if err != nil {
		t.Fatalf("expected direct-send fallback, got error: %v\n%s", err, out.String())
	}
	if delivered == nil || delivered.ID != rep.ID {
		t.Fatalf("fallback delivered = %+v, want replacement %s", delivered, rep.ID)
	}

	// The verified send already confirmed submission (delivered != nil above);
	// the pane carries the prompt text (tmux wraps long lines, so strip \n).
	want := "Read your handoff doc at " + docPath
	deadline := time.Now().Add(2 * time.Second)
	var lastOut []byte
	for time.Now().Before(deadline) {
		captured, cerr := tmux.CapturePane(rep.TmuxSession)
		if cerr == nil {
			lastOut = captured
			if strings.Contains(strings.ReplaceAll(string(captured), "\n", ""), want) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("fallback did not deliver prompt; want %q in:\n%s", want, string(lastOut))
}

func TestHandoff_GracefulRoute_SwapsViaBarrier(t *testing.T) {
	requireTmux(t)
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	root := setupFleetHome(t)

	const project = "rainier"
	cwd := seedRecoveryRepo(t, root, project)
	old := seedGracefulHandoffOld(t, project, cwd)
	forbidHandoffDeliveryOutsideBarrier(t)

	winner := agent.New(agent.NewID())
	winner.TaskID = "coord-" + project
	winner.Project = project
	winner.Cwd = cwd
	winner.Command = []string{"sleep", "60"}
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	if err := winner.Write(); err != nil {
		t.Fatalf("write winner: %v", err)
	}

	origElig := handoffGracefulEligibleFn
	origSwap := handoffGracefulSwapFn
	t.Cleanup(func() {
		handoffGracefulEligibleFn = origElig
		handoffGracefulSwapFn = origSwap
	})
	handoffGracefulEligibleFn = func(rec *agent.Record, autoResume bool) bool {
		if rec == nil || rec.ID != old.ID {
			t.Errorf("eligibility consulted for %+v, want old coord %s", rec, old.ID)
		}
		return true
	}
	swapCalls := 0
	var swapNewID string
	handoffGracefulSwapFn = func(o, n *agent.Record, docPath string, grace time.Duration,
		deliver func() (*agent.Record, error), stderr io.Writer) (*agent.Record, error) {
		swapCalls++
		if o.ID != old.ID {
			t.Errorf("graceful swap got old=%s, want %s", o.ID, old.ID)
		}
		if !n.LeaseWrapped {
			t.Error("graceful swap successor is not lease-wrapped — its lease would lapse")
		}
		if deliver == nil {
			t.Error("graceful swap got a nil deliver closure")
		}
		swapNewID = n.ID
		// Simulate the barrier completion: OLD retired, doc on the winner.
		if err := o.ArchiveWithHandoff(n.ID); err != nil {
			t.Fatalf("simulated retire: %v", err)
		}
		_ = tmux.Kill(o.TmuxSession)
		return winner, nil
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err != nil {
		t.Fatalf("graceful-route handoff failed: %v\n%s", err, out.String())
	}
	if swapCalls != 1 {
		t.Fatalf("graceful swap ran %d times, want 1", swapCalls)
	}
	arc, aerr := agent.LoadArchive(old.ID)
	if aerr != nil {
		t.Fatalf("LoadArchive(%s): %v", old.ID, aerr)
	}
	if arc.SuccessorID != winner.ID {
		t.Fatalf("archive successor = %q, want lock winner %q", arc.SuccessorID, winner.ID)
	}
	// The superseded freshly-spawned replacement is dropped post-queue-delete.
	if _, lerr := agent.Load(swapNewID); lerr == nil {
		t.Fatalf("superseded replacement %s should be dropped", swapNewID)
	}
	if !strings.Contains(out.String(), "handed off → "+winner.ID) {
		t.Fatalf("output did not report the winner %q:\n%s", winner.ID, out.String())
	}
	qp, qerr := state.QueuePath(queue.SpawnFreshName(old.ID))
	if qerr != nil {
		t.Fatalf("QueuePath: %v", qerr)
	}
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Fatalf("queue file should be consumed, statErr=%v", statErr)
	}
}

func TestHandoff_GracefulRouteError_PreservesQueueAndOld(t *testing.T) {
	requireTmux(t)
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	root := setupFleetHome(t)

	const project = "rainier"
	cwd := seedRecoveryRepo(t, root, project)
	old := seedGracefulHandoffOld(t, project, cwd)
	forbidHandoffDeliveryOutsideBarrier(t)

	origElig := handoffGracefulEligibleFn
	origSwap := handoffGracefulSwapFn
	t.Cleanup(func() {
		handoffGracefulEligibleFn = origElig
		handoffGracefulSwapFn = origSwap
	})
	handoffGracefulEligibleFn = func(*agent.Record, bool) bool { return true }
	wantErr := errors.New("winner never converged")
	handoffGracefulSwapFn = func(_, _ *agent.Record, _ string, _ time.Duration,
		_ func() (*agent.Record, error), _ io.Writer) (*agent.Record, error) {
		return nil, wantErr
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err == nil || !strings.Contains(err.Error(), "winner never converged") {
		t.Fatalf("graceful route error not surfaced: %v", err)
	}
	// OLD stays live and the queue stays pending for the retry.
	if _, lerr := agent.Load(old.ID); lerr != nil {
		t.Fatalf("old coord record must survive a graceful-route failure: %v", lerr)
	}
	qp, qerr := state.QueuePath(queue.SpawnFreshName(old.ID))
	if qerr != nil {
		t.Fatalf("QueuePath: %v", qerr)
	}
	if _, statErr := os.Stat(qp); statErr != nil {
		t.Fatalf("queue file must be preserved on a graceful-route failure: %v", statErr)
	}
}

func TestHandoff_LeaseFailoverCoordHandoffRefusesBeforeRetiringOld(t *testing.T) {
	requireTmux(t)
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	root := setupFleetHome(t)

	const project = "rainier"
	cwd := seedRecoveryRepo(t, root, project)
	old := agent.New(agent.NewID())
	old.TaskID = "coord-" + project
	old.Project = project
	old.Cwd = cwd
	old.Command = []string{"sleep", "60"}
	old.TmuxSession = tmux.SessionName(old.ID)
	old.SpawnedAt = time.Now().UTC()
	old.LastActivityTS = old.SpawnedAt
	old.SupervisorPID = 4242
	origOwner := handoffLeaseActiveOwnerPIDFn
	origLeader := handoffLeaseLeaderPresentFn
	origDeliveryDeps := handoffDeliveryDepsFn
	origDeliveryTimeout := handoffDeliveryTimeout
	origDeliveryPoll := handoffDeliveryPoll
	handoffLeaseActiveOwnerPIDFn = func(p string) (int, bool) {
		if p != project {
			t.Fatalf("active owner checked for project %q, want %q", p, project)
		}
		return old.SupervisorPID, true
	}
	handoffLeaseLeaderPresentFn = func(p string) bool {
		if p != project {
			t.Fatalf("leader checked for project %q, want %q", p, project)
		}
		return true
	}
	t.Cleanup(func() {
		handoffLeaseActiveOwnerPIDFn = origOwner
		handoffLeaseLeaderPresentFn = origLeader
		handoffDeliveryDepsFn = origDeliveryDeps
		handoffDeliveryTimeout = origDeliveryTimeout
		handoffDeliveryPoll = origDeliveryPoll
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(p string) (coordlock.Owner, bool) {
			if p != project {
				t.Fatalf("current owner checked for project %q, want %q", p, project)
			}
			recs, err := agent.List()
			if err != nil {
				return coordlock.Owner{}, false
			}
			for _, rec := range recs {
				if rec.Project == project && rec.ID != old.ID {
					return coordlock.Owner{AgentID: rec.ID, PID: rec.SupervisorPID, PidStart: rec.SupervisorPidStart}, true
				}
			}
			return coordlock.Owner{}, false
		}
		deps.WaitReady = func(string) error { return nil }
		deps.SessionAlive = func(string) (bool, error) { return true, nil }
		deps.SendVerified = func(session, prompt string) (bool, error) {
			if !strings.Contains(prompt, "Read your handoff doc at ") {
				t.Fatalf("resume prompt has wrong shape: %q", prompt)
			}
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	if err := tmux.Spawn(old.TmuxSession, old.Cwd, old.Command, []string{"FLEET_AGENT_ID=" + old.ID}); err != nil {
		t.Fatalf("spawn old coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })
	if err := old.Write(); err != nil {
		t.Fatalf("write old coord: %v", err)
	}
	// old.TaskID == coord-<project> already makes it the project's coord
	// (spawn.IsCoordSpawn); the coord-spawn marker is gone (D3).
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err != nil {
		t.Fatalf("handoff should deliver via lock owner under PR2: %v\n%s", err, out.String())
	}
	qp, qerr := state.QueuePath(queue.SpawnFreshName(old.ID))
	if qerr != nil {
		t.Fatalf("QueuePath: %v", qerr)
	}
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Fatalf("queue file should be consumed after verified delivery, statErr=%v", statErr)
	}
}

func TestHandoff_LeaseFailoverCoordHandoffFinalizesDeliveredLockOwner(t *testing.T) {
	requireTmux(t)
	t.Setenv("FLEET_STANDBY_TIMEOUT", "3s")
	root := setupFleetHome(t)

	const project = "rainier"
	cwd := seedRecoveryRepo(t, root, project)
	old := agent.New(agent.NewID())
	old.TaskID = "coord-" + project
	old.Project = project
	old.Cwd = cwd
	old.Command = []string{"sleep", "60"}
	old.TmuxSession = tmux.SessionName(old.ID)
	old.SpawnedAt = time.Now().UTC()
	old.LastActivityTS = old.SpawnedAt
	old.SupervisorPID = 4242

	winner := agent.New(agent.NewID())
	winner.TaskID = "coord-" + project
	winner.Project = project
	winner.Cwd = cwd
	winner.Command = []string{"sleep", "60"}
	winner.TmuxSession = tmux.SessionName(winner.ID)
	winner.SpawnedAt = time.Now().UTC()
	winner.LastActivityTS = winner.SpawnedAt
	winner.SupervisorPID = 9001
	winner.SupervisorPidStart = 99

	origOwner := handoffLeaseActiveOwnerPIDFn
	origLeader := handoffLeaseLeaderPresentFn
	origDeliveryDeps := handoffDeliveryDepsFn
	origDeliveryTimeout := handoffDeliveryTimeout
	origDeliveryPoll := handoffDeliveryPoll
	handoffLeaseActiveOwnerPIDFn = func(p string) (int, bool) {
		if p != project {
			t.Fatalf("active owner checked for project %q, want %q", p, project)
		}
		return old.SupervisorPID, true
	}
	handoffLeaseLeaderPresentFn = func(p string) bool {
		if p != project {
			t.Fatalf("leader checked for project %q, want %q", p, project)
		}
		return true
	}
	t.Cleanup(func() {
		handoffLeaseActiveOwnerPIDFn = origOwner
		handoffLeaseLeaderPresentFn = origLeader
		handoffDeliveryDepsFn = origDeliveryDeps
		handoffDeliveryTimeout = origDeliveryTimeout
		handoffDeliveryPoll = origDeliveryPoll
	})
	handoffDeliveryTimeout = time.Second
	handoffDeliveryPoll = time.Millisecond
	var sentSession string
	handoffDeliveryDepsFn = func() handoffdelivery.Deps {
		deps := handoffdelivery.DefaultDeps()
		deps.CurrentOwner = func(p string) (coordlock.Owner, bool) {
			if p != project {
				t.Fatalf("current owner checked for project %q, want %q", p, project)
			}
			return coordlock.Owner{AgentID: winner.ID, PID: winner.SupervisorPID, PidStart: winner.SupervisorPidStart}, true
		}
		deps.WaitReady = func(session string) error {
			if session != winner.TmuxSession {
				t.Fatalf("wait-ready session = %q, want delivered owner session %q", session, winner.TmuxSession)
			}
			return nil
		}
		deps.SessionAlive = func(session string) (bool, error) {
			if session != winner.TmuxSession {
				t.Fatalf("session-alive probe = %q, want delivered owner session %q", session, winner.TmuxSession)
			}
			return true, nil
		}
		deps.SendVerified = func(session, prompt string) (bool, error) {
			sentSession = session
			if session != winner.TmuxSession {
				t.Fatalf("resume prompt session = %q, want delivered owner session %q", session, winner.TmuxSession)
			}
			if !strings.Contains(prompt, "Read your handoff doc at ") {
				t.Fatalf("resume prompt has wrong shape: %q", prompt)
			}
			return true, nil
		}
		deps.Sleep = func(time.Duration) {}
		return deps
	}
	if err := tmux.Spawn(old.TmuxSession, old.Cwd, old.Command, []string{"FLEET_AGENT_ID=" + old.ID}); err != nil {
		t.Fatalf("spawn old coord: %v", err)
	}
	t.Cleanup(func() { _ = tmux.Kill(old.TmuxSession) })
	if err := old.Write(); err != nil {
		t.Fatalf("write old coord: %v", err)
	}
	if err := winner.Write(); err != nil {
		t.Fatalf("write lock-winning standby: %v", err)
	}
	// old.TaskID == coord-<project> already marks it as the coord
	// (spawn.IsCoordSpawn); the coord-spawn marker is gone (D3).
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	out := &bytes.Buffer{}
	err := runHandoff(&handoffOpts{
		oldID:       old.ID,
		command:     []string{"sleep", "60"},
		graceMillis: 0,
	}, out, out)
	if err != nil {
		t.Fatalf("handoff should finalize delivered lock owner: %v\n%s", err, out.String())
	}
	if sentSession != winner.TmuxSession {
		t.Fatalf("resume prompt sent to %q, want %q", sentSession, winner.TmuxSession)
	}
	arc, err := agent.LoadArchive(old.ID)
	if err != nil {
		t.Fatalf("LoadArchive(%s): %v", old.ID, err)
	}
	if arc.SuccessorID != winner.ID {
		t.Fatalf("archive successor = %q, want delivered lock owner %q", arc.SuccessorID, winner.ID)
	}
	live, err := agent.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, rec := range live {
		if rec.Project == project && rec.ID != winner.ID {
			t.Fatalf("superseded replacement %s should have been removed; live project record: %+v", rec.ID, rec)
		}
	}
	if !strings.Contains(out.String(), "handed off → "+winner.ID) {
		t.Fatalf("output did not report delivered lock owner %q:\n%s", winner.ID, out.String())
	}
	qp, qerr := state.QueuePath(queue.SpawnFreshName(old.ID))
	if qerr != nil {
		t.Fatalf("QueuePath: %v", qerr)
	}
	if _, statErr := os.Stat(qp); !os.IsNotExist(statErr) {
		t.Fatalf("queue file should be consumed after verified delivery, statErr=%v", statErr)
	}
}
