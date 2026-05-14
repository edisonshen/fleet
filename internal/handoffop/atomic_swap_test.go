package handoffop

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/state"
)

// seedSwapRecords builds two stub records on disk (OLD live, NEW live)
// and returns them. Tests then stub tmux behavior to drive the swap.
func seedSwapRecords(t *testing.T) (oldRec, newRec *agent.Record) {
	t.Helper()
	setupFleetHome(t)
	if _, err := state.EnsureProjectInitialized("rainier"); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}
	oldRec = agent.New("oldcoord")
	oldRec.TmuxSession = "fleet-oldcoord"
	oldRec.Project = "rainier"
	oldRec.TaskID = "swap-test"
	if err := oldRec.Write(); err != nil {
		t.Fatalf("OLD rec.Write: %v", err)
	}
	newRec = agent.New("newcoord")
	newRec.TmuxSession = "fleet-newcoord"
	newRec.Project = "rainier"
	newRec.TaskID = "swap-test"
	if err := newRec.Write(); err != nil {
		t.Fatalf("NEW rec.Write: %v", err)
	}
	return oldRec, newRec
}

// stubSwapTmux replaces the package-level tmux helpers for the
// duration of the test. Binds both AtomicSwap's seam and
// DropReplacementRecord's seam so rollback paths see consistent
// behavior.
func stubSwapTmux(t *testing.T,
	kill func(string) error,
	alive func(string) (bool, error),
) {
	t.Helper()
	origKill, origAlive := tmuxKillForSwap, tmuxSessionAliveForSwap
	origRepKill, origRepAlive := tmuxKillFn, tmuxSessionAliveFn
	tmuxKillForSwap = kill
	tmuxSessionAliveForSwap = alive
	tmuxKillFn = kill
	tmuxSessionAliveFn = alive
	t.Cleanup(func() {
		tmuxKillForSwap = origKill
		tmuxSessionAliveForSwap = origAlive
		tmuxKillFn = origRepKill
		tmuxSessionAliveFn = origRepAlive
	})
}

// TestAtomicSwap_HappyPath: NEW alive, OLD kill ok → archive OLD,
// marker swapped, result reports both.
func TestAtomicSwap_HappyPath(t *testing.T) {
	oldRec, newRec := seedSwapRecords(t)
	if err := state.WriteCoordSpawnMarker(oldRec.Project, oldRec.ID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	killCalled := ""
	stubSwapTmux(t,
		func(s string) error { killCalled = s; return nil },
		func(string) (bool, error) { return true, nil },
	)

	var stderr bytes.Buffer
	res, err := AtomicSwap(AtomicSwapOpts{
		OldRec: oldRec, NewRec: newRec, Stderr: &stderr, SwapMarker: true,
	})
	if err != nil {
		t.Fatalf("AtomicSwap: %v", err)
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; want true on happy path")
	}
	if !res.MarkerSwapped {
		t.Errorf("MarkerSwapped = false; want true (marker pointed at OLD)")
	}
	if killCalled != oldRec.TmuxSession {
		t.Errorf("Kill called with %q; want %q", killCalled, oldRec.TmuxSession)
	}
	livePath, _ := state.AgentPath(oldRec.ID)
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OLD live record still present at %s", livePath)
	}
	archPath, _ := state.AgentArchivePath(oldRec.ID)
	if _, err := os.Stat(archPath); err != nil {
		t.Errorf("OLD archive missing at %s: %v", archPath, err)
	}
	if want := state.ReadCoordSpawnMarker(oldRec.Project); want != newRec.ID {
		t.Errorf("marker = %q; want %q", want, newRec.ID)
	}
}

// TestAtomicSwap_RollbackOnOldKillFailureStillAlive: the headline
// regression test. OLD kill fails AND post-probe says still alive →
// MUST NOT archive OLD; MUST roll back NEW.
func TestAtomicSwap_RollbackOnOldKillFailureStillAlive(t *testing.T) {
	oldRec, newRec := seedSwapRecords(t)

	stubSwapTmux(t,
		func(s string) error {
			if s == oldRec.TmuxSession {
				return errors.New("simulated tmux kill failure on OLD")
			}
			return nil
		},
		func(s string) (bool, error) {
			// NEW probe: alive; OLD post-kill probe: still alive.
			if s == oldRec.TmuxSession {
				return true, nil
			}
			return true, nil
		},
	)

	var stderr bytes.Buffer
	_, err := AtomicSwap(AtomicSwapOpts{
		OldRec: oldRec, NewRec: newRec, Stderr: &stderr, SwapMarker: true,
	})
	if err == nil {
		t.Fatalf("expected error when OLD kill fails + still alive")
	}
	if !strings.Contains(err.Error(), "still alive") {
		t.Errorf("error should mention 'still alive'; got: %v", err)
	}
	livePath, _ := state.AgentPath(oldRec.ID)
	if _, statErr := os.Stat(livePath); statErr != nil {
		t.Errorf("OLD live record gone (%v) — invariant violation", statErr)
	}
	archPath, _ := state.AgentArchivePath(oldRec.ID)
	if _, statErr := os.Stat(archPath); statErr == nil {
		t.Errorf("OLD archive present — invariant violation")
	}
	newLivePath, _ := state.AgentPath(newRec.ID)
	if _, statErr := os.Stat(newLivePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("NEW live record still present (%v); rollback failed", statErr)
	}
}

// TestAtomicSwap_RefusesWhenNewProbeAmbiguous.
func TestAtomicSwap_RefusesWhenNewProbeAmbiguous(t *testing.T) {
	oldRec, newRec := seedSwapRecords(t)

	stubSwapTmux(t,
		func(string) error {
			t.Errorf("Kill must not be called when NEW probe is ambiguous")
			return nil
		},
		func(s string) (bool, error) {
			if s == newRec.TmuxSession {
				return false, errors.New("simulated transport probe failure on NEW")
			}
			t.Errorf("OLD probe should not run when NEW probe is ambiguous")
			return false, nil
		},
	)
	var stderr bytes.Buffer
	_, err := AtomicSwap(AtomicSwapOpts{
		OldRec: oldRec, NewRec: newRec, Stderr: &stderr,
	})
	if err == nil {
		t.Fatalf("expected error on ambiguous NEW probe")
	}
	if !strings.Contains(err.Error(), "probe NEW") {
		t.Errorf("error should mention probe NEW; got %v", err)
	}
	livePath, _ := state.AgentPath(oldRec.ID)
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("OLD live record removed despite ambiguous NEW probe: %v", err)
	}
	newPath, _ := state.AgentPath(newRec.ID)
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("NEW live record removed despite ambiguous NEW probe: %v", err)
	}
}

// TestAtomicSwap_NewSessionDeadRollsBack.
func TestAtomicSwap_NewSessionDeadRollsBack(t *testing.T) {
	oldRec, newRec := seedSwapRecords(t)

	stubSwapTmux(t,
		func(s string) error {
			if s == oldRec.TmuxSession {
				t.Errorf("Kill OLD must not be called when NEW is dead")
			}
			return nil
		},
		func(string) (bool, error) {
			return false, nil
		},
	)
	var stderr bytes.Buffer
	_, err := AtomicSwap(AtomicSwapOpts{
		OldRec: oldRec, NewRec: newRec, Stderr: &stderr,
	})
	if err == nil {
		t.Fatalf("expected error on dead NEW")
	}
	newPath, _ := state.AgentPath(newRec.ID)
	if _, statErr := os.Stat(newPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("NEW live record still present after dead-NEW rollback")
	}
	livePath, _ := state.AgentPath(oldRec.ID)
	if _, statErr := os.Stat(livePath); statErr != nil {
		t.Errorf("OLD live record removed despite dead-NEW path: %v", statErr)
	}
}

// TestAtomicSwap_KillRaceOldGoneAfterKillError.
func TestAtomicSwap_KillRaceOldGoneAfterKillError(t *testing.T) {
	oldRec, newRec := seedSwapRecords(t)
	killErr := errors.New("simulated tmux kill race")
	stubSwapTmux(t,
		func(s string) error {
			if s == oldRec.TmuxSession {
				return killErr
			}
			return nil
		},
		func(s string) (bool, error) {
			if s == newRec.TmuxSession {
				return true, nil
			}
			return false, nil
		},
	)
	var stderr bytes.Buffer
	res, err := AtomicSwap(AtomicSwapOpts{
		OldRec: oldRec, NewRec: newRec, Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("expected success on kill-race-but-confirmed-dead; got %v", err)
	}
	if !res.OldArchived {
		t.Errorf("OldArchived = false; want true on kill-race path")
	}
	if !strings.Contains(stderr.String(), "session is gone") {
		t.Errorf("stderr should note race; got: %s", stderr.String())
	}
}

// TestAtomicSwap_InvalidInputs.
func TestAtomicSwap_InvalidInputs(t *testing.T) {
	setupFleetHome(t)
	cases := []struct {
		name string
		opts AtomicSwapOpts
	}{
		{"nil OldRec", AtomicSwapOpts{NewRec: agent.New("x")}},
		{"nil NewRec", AtomicSwapOpts{OldRec: agent.New("x")}},
		{"empty OldRec ID", AtomicSwapOpts{
			OldRec: &agent.Record{TmuxSession: "x"}, NewRec: agent.New("y"),
		}},
		{"empty OldRec TmuxSession", AtomicSwapOpts{
			OldRec: agent.New("x"), NewRec: agent.New("y"),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AtomicSwap(tc.opts)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidSwap) {
				t.Errorf("expected ErrInvalidSwap; got %v", err)
			}
		})
	}
}

// TestAtomicSwap_SkipsMarkerWhenSwapMarkerFalse.
func TestAtomicSwap_SkipsMarkerWhenSwapMarkerFalse(t *testing.T) {
	oldRec, newRec := seedSwapRecords(t)
	if err := state.WriteCoordSpawnMarker(oldRec.Project, newRec.ID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	stubSwapTmux(t,
		func(string) error { return nil },
		func(string) (bool, error) { return true, nil },
	)
	var stderr bytes.Buffer
	res, err := AtomicSwap(AtomicSwapOpts{
		OldRec: oldRec, NewRec: newRec, Stderr: &stderr, SwapMarker: false,
	})
	if err != nil {
		t.Fatalf("AtomicSwap: %v", err)
	}
	if res.MarkerSwapped {
		t.Errorf("MarkerSwapped = true with SwapMarker=false")
	}
	if want := state.ReadCoordSpawnMarker(oldRec.Project); want != newRec.ID {
		t.Errorf("marker = %q; want %q (unchanged)", want, newRec.ID)
	}
}
