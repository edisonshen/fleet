package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// withInjectedSessionFns replaces the package-level sessionListFn /
// sessionExistsFn for the duration of the test. Restoration runs via
// t.Cleanup so a panicking test still resets.
func withInjectedSessionFns(t *testing.T,
	listFn func() ([]string, error),
	existsFn func(id string) bool,
) {
	t.Helper()
	prevList := sessionListFn
	prevExists := sessionExistsFn
	sessionListFn = listFn
	sessionExistsFn = existsFn
	t.Cleanup(func() {
		sessionListFn = prevList
		sessionExistsFn = prevExists
	})
}

func TestEnforceSessionCap_BelowCap(t *testing.T) {
	t.Setenv(state.FleetMaxSessionsEnv, "30")
	sessions := []string{"fleet-a", "fleet-b", "fleet-c"}
	withInjectedSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(id string) bool { return true },
	)
	var stderr bytes.Buffer
	if err := enforceSessionCap(&stderr, "", 0); err != nil {
		t.Fatalf("unexpected error at 3/30: %v", err)
	}
}

func TestEnforceSessionCap_AtCap(t *testing.T) {
	t.Setenv(state.FleetMaxSessionsEnv, "3")
	sessions := []string{"fleet-a", "fleet-b", "fleet-c"}
	withInjectedSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(id string) bool { return true },
	)
	var stderr bytes.Buffer
	err := enforceSessionCap(&stderr, "", 0)
	if err == nil {
		t.Fatalf("expected refusal at 3/3, got nil")
	}
	msg := err.Error()
	for _, want := range []string{
		"refusing to spawn",
		"FLEET_MAX_SESSIONS=3",
		"3 live",
		"0 orphan",
		"prune-orphan-tmux",
		"fleet rm",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("err missing %q\nfull: %s", want, msg)
		}
	}
}

func TestEnforceSessionCap_OverCap(t *testing.T) {
	// Document that the gate refuses even when count > max (e.g. a
	// race let through extra sessions). Recovery path: prune orphans.
	t.Setenv(state.FleetMaxSessionsEnv, "2")
	sessions := []string{"fleet-a", "fleet-b", "fleet-c", "fleet-d"}
	live := map[string]bool{"fleet-a": true, "fleet-b": true}
	withInjectedSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(id string) bool { return live["fleet-"+id] },
	)
	var stderr bytes.Buffer
	err := enforceSessionCap(&stderr, "", 0)
	if err == nil {
		t.Fatalf("expected refusal at 4/2, got nil")
	}
	if !strings.Contains(err.Error(), "2 live") {
		t.Errorf("expected live=2 in err: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "2 orphan") {
		t.Errorf("expected orphan=2 in err: %s", err.Error())
	}
}

func TestEnforceSessionCap_NonFleetSessionsExcluded(t *testing.T) {
	// Tmux sessions that don't start with "fleet-" must not count
	// toward the cap. Operators may have unrelated tmux sessions on
	// the same server; fleet does not own those.
	t.Setenv(state.FleetMaxSessionsEnv, "3")
	sessions := []string{
		"fleet-a", "fleet-b",
		"work", "scratch", "vim-session", "fleet-",
	}
	withInjectedSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(id string) bool { return true },
	)
	var stderr bytes.Buffer
	if err := enforceSessionCap(&stderr, "", 0); err != nil {
		t.Fatalf("unexpected error at 2 fleet / 3 cap (4 non-fleet): %v", err)
	}
}

func TestEnforceSessionCap_ForceReplacementBypass(t *testing.T) {
	// --force-replacement skips the precheck entirely. Even at-cap,
	// the spawn proceeds and the bypass reason is logged.
	t.Setenv(state.FleetMaxSessionsEnv, "1")
	sessions := []string{"fleet-a", "fleet-b", "fleet-c"}
	withInjectedSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(id string) bool { return true },
	)
	var stderr bytes.Buffer
	if err := enforceSessionCap(&stderr, SessionCapBypassForceReplacement, 0); err != nil {
		t.Fatalf("force-replacement bypass should not error: %v", err)
	}
	if !strings.Contains(stderr.String(), "force-replacement") {
		t.Errorf("expected bypass note in stderr: %s", stderr.String())
	}
}

func TestEnforceSessionCap_NilStderrTolerated(t *testing.T) {
	// Spawn paths may pass nil stderr. The cap must not panic.
	t.Setenv(state.FleetMaxSessionsEnv, "30")
	withInjectedSessionFns(t,
		func() ([]string, error) { return []string{"fleet-a"}, nil },
		func(id string) bool { return true },
	)
	if err := enforceSessionCap(nil, "", 0); err != nil {
		t.Fatalf("nil-stderr below-cap: %v", err)
	}
	// And on bypass with nil stderr (no log target).
	if err := enforceSessionCap(nil, SessionCapBypassForceReplacement, 0); err != nil {
		t.Fatalf("nil-stderr bypass: %v", err)
	}
}

func TestEnforceSessionCap_ListErrorProceedsWithoutBlocking(t *testing.T) {
	// Probe failure (tmux unreachable) — best-effort cap means we
	// log to stderr and return nil so the operator isn't locked out
	// of dispatch by a transient tmux server hiccup.
	t.Setenv(state.FleetMaxSessionsEnv, "30")
	boom := errors.New("tmux not on PATH")
	withInjectedSessionFns(t,
		func() ([]string, error) { return nil, boom },
		func(id string) bool { return true },
	)
	var stderr bytes.Buffer
	if err := enforceSessionCap(&stderr, "", 0); err != nil {
		t.Fatalf("list error should not block spawn: %v", err)
	}
	if !strings.Contains(stderr.String(), "could not enumerate") {
		t.Errorf("expected probe-fail warning, got %q", stderr.String())
	}
}

// TestEnforceSessionCap_SwapsInFlightOffsetsCount regresses codex
// iter-2 P1: when the caller is about to retire an OLD session as
// part of the spawn (handoff semantics), the cap must use the
// PROJECTED post-swap count, not the current count. At exactly
// N=max with all live sessions, a net-zero handoff would otherwise
// deadlock — the queue file fails forever because the old session
// is counted, the new spawn tips over, and nothing kills the old
// without the spawn succeeding.
func TestEnforceSessionCap_SwapsInFlightOffsetsCount(t *testing.T) {
	t.Setenv(state.FleetMaxSessionsEnv, "3")
	// Exactly at cap: 3 live fleet-* sessions.
	sessions := []string{"fleet-a", "fleet-b", "fleet-c"}
	withInjectedSessionFns(t,
		func() ([]string, error) { return sessions, nil },
		func(string) bool { return true },
	)
	// swapsInFlight=0 (dispatch): projected = 3 + 1 - 0 = 4 > 3, refuse.
	if err := enforceSessionCap(nil, "", 0); err == nil {
		t.Errorf("swaps=0 at-cap: expected refusal")
	}
	// swapsInFlight=1 (handoff): projected = 3 + 1 - 1 = 3 ≤ 3, allowed.
	if err := enforceSessionCap(nil, "", 1); err != nil {
		t.Errorf("swaps=1 at-cap (net-zero swap): unexpected refusal: %v", err)
	}
}

// TestHandoff_CapDoesNotBlockDeadSessionShortCircuit regresses
// codex iter-1 P1: the FLEET_MAX_SESSIONS cap originally ran at the
// top of runHandoff, before the dead-session short-circuit. That
// stranded operators who needed to archive a dead agent's record
// at-cap. The fix moved the cap check to right before spawn.Spawn,
// so dead-session archiving proceeds regardless of count.
//
// Test setup: at-cap (injected), no live session for the old agent
// → runHandoff should archive the record without invoking the cap.
func TestHandoff_CapDoesNotBlockDeadSessionShortCircuit(t *testing.T) {
	requireTmux(t)
	tmp := setupFleetHome(t)
	t.Setenv("FLEET_MAX_SESSIONS", "1")
	// Inject at-cap so any precheck would refuse.
	withInjectedSessionFns(t,
		func() ([]string, error) {
			return []string{"fleet-other1", "fleet-other2"}, nil
		},
		func(string) bool { return true },
	)

	// Build a record whose tmux session DOESN'T exist (dead-session
	// short-circuit path).
	rec := agent.New("deadbeef")
	rec.TaskID = "stub"
	rec.Project = "stub"
	rec.TmuxSession = "fleet-deadbeef-nonexistent"
	rec.Cwd = tmp
	rec.Command = []string{"sleep", "60"}
	if err := rec.Write(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runHandoff(&handoffOpts{
		oldID:       rec.ID,
		graceMillis: 0,
	}, &stdout, &stderr); err != nil {
		t.Fatalf("dead-session handoff should succeed (cap must not block recovery paths); err=%v", err)
	}
	// Record archived.
	livePath := filepath.Join(tmp, "agents", rec.ID+".json")
	if _, statErr := os.Stat(livePath); !os.IsNotExist(statErr) {
		t.Errorf("live record should be archived; stat=%v", statErr)
	}
}

func TestEnforceSessionCap_DefaultCap30(t *testing.T) {
	// With FLEET_MAX_SESSIONS unset, default is 30. Verify 30 sessions
	// is at-cap (refuse) and 29 is under-cap (allow).
	t.Setenv(state.FleetMaxSessionsEnv, "")
	make30 := func(n int) []string {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = "fleet-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		}
		return out
	}
	t.Run("29 allowed", func(t *testing.T) {
		s := make30(29)
		withInjectedSessionFns(t,
			func() ([]string, error) { return s, nil },
			func(string) bool { return true },
		)
		if err := enforceSessionCap(nil, "", 0); err != nil {
			t.Fatalf("29/30 should allow: %v", err)
		}
	})
	t.Run("30 refused", func(t *testing.T) {
		s := make30(30)
		withInjectedSessionFns(t,
			func() ([]string, error) { return s, nil },
			func(string) bool { return true },
		)
		if err := enforceSessionCap(nil, "", 0); err == nil {
			t.Fatalf("30/30 should refuse")
		}
	})
}
