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

// TestDropReplacementRecord_KillSucceedsRecordRemoved is the happy
// path: tmux.Kill returns nil (session already gone or just-killed),
// the record file is removed, the function returns nil.
func TestDropReplacementRecord_KillSucceedsRecordRemoved(t *testing.T) {
	setupFleetHome(t)
	rec := agent.New("test1234")
	rec.TmuxSession = "fleet-test1234"
	if err := rec.Write(); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	path, err := state.AgentPath(rec.ID)
	if err != nil {
		t.Fatalf("state.AgentPath: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("seed: agent record not present at %s: %v", path, err)
	}

	var killed string
	origKill := tmuxKillFn
	origAlive := tmuxSessionAliveFn
	tmuxKillFn = func(s string) error { killed = s; return nil }
	tmuxSessionAliveFn = func(s string) (bool, error) { return false, nil }
	t.Cleanup(func() { tmuxKillFn = origKill; tmuxSessionAliveFn = origAlive })

	var stderr bytes.Buffer
	if err := DropReplacementRecord(rec.TmuxSession, rec.ID, &stderr); err != nil {
		t.Fatalf("DropReplacementRecord: %v", err)
	}
	if killed != rec.TmuxSession {
		t.Errorf("expected Kill called with %q; got %q", rec.TmuxSession, killed)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record file still present after DropReplacementRecord: stat err=%v", err)
	}
}

// TestDropReplacementRecord_KillFailsSessionStillAlive_PreservesRecord
// is the REGRESSION test for the orphan-tmux-session leak. The fleet-
// guard auto-handoff path used to call `tmux.HasSession` (which returns
// false on probe failure), then os.Remove(state.AgentPath(rec.ID)) —
// orphaning the tmux session. DropReplacementRecord must:
//
//   - call tmux.Kill, which fails (mocked transport error),
//   - re-probe via SessionAlive, which reports STILL ALIVE,
//   - return an error,
//   - leave the record file intact on disk (so the operator's `fleet
//     status` shows the orphan and they can clean it up manually
//     instead of fleet silently leaking it).
//
// Before the fix, the record was unconditionally removed; this test
// fails on the pre-fix code.
func TestDropReplacementRecord_KillFailsSessionStillAlive_PreservesRecord(t *testing.T) {
	setupFleetHome(t)
	rec := agent.New("leakcase")
	rec.TmuxSession = "fleet-leakcase"
	if err := rec.Write(); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	path, err := state.AgentPath(rec.ID)
	if err != nil {
		t.Fatalf("state.AgentPath: %v", err)
	}

	killErr := errors.New("simulated tmux kill failure (transport error)")
	origKill := tmuxKillFn
	origAlive := tmuxSessionAliveFn
	tmuxKillFn = func(string) error { return killErr }
	tmuxSessionAliveFn = func(string) (bool, error) { return true, nil } // STILL ALIVE
	t.Cleanup(func() { tmuxKillFn = origKill; tmuxSessionAliveFn = origAlive })

	var stderr bytes.Buffer
	err = DropReplacementRecord(rec.TmuxSession, rec.ID, &stderr)
	if err == nil {
		t.Fatalf("DropReplacementRecord: expected error when session is still alive after kill failure; got nil")
	}
	// Error must mention "still alive" so the operator can grep it.
	if !strings.Contains(err.Error(), "still alive") {
		t.Errorf("error message should mention 'still alive'; got: %v", err)
	}
	// Critical: the agent record MUST still be on disk. This is the
	// regression bar.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("agent record %s removed despite live tmux session; statErr=%v (this is the LEAK)", path, statErr)
	}
}

// TestDropReplacementRecord_KillFailsProbeAmbiguous_PreservesRecord
// covers the other half of the leak surface: the post-kill probe is
// also failing (e.g., tmux binary genuinely gone, socket disconnected
// mid-cleanup). We cannot prove the session is dead, so we MUST refuse
// to remove the record — same operator-visible-orphan invariant.
func TestDropReplacementRecord_KillFailsProbeAmbiguous_PreservesRecord(t *testing.T) {
	setupFleetHome(t)
	rec := agent.New("probefail")
	rec.TmuxSession = "fleet-probefail"
	if err := rec.Write(); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	path, err := state.AgentPath(rec.ID)
	if err != nil {
		t.Fatalf("state.AgentPath: %v", err)
	}

	killErr := errors.New("simulated tmux kill error")
	probeErr := errors.New("simulated post-probe failure")
	origKill := tmuxKillFn
	origAlive := tmuxSessionAliveFn
	tmuxKillFn = func(string) error { return killErr }
	tmuxSessionAliveFn = func(string) (bool, error) { return false, probeErr }
	t.Cleanup(func() { tmuxKillFn = origKill; tmuxSessionAliveFn = origAlive })

	var stderr bytes.Buffer
	err = DropReplacementRecord(rec.TmuxSession, rec.ID, &stderr)
	if err == nil {
		t.Fatalf("DropReplacementRecord: expected error on ambiguous probe; got nil")
	}
	if !strings.Contains(err.Error(), "post-kill probe also failed") {
		t.Errorf("error should mention post-kill probe failure; got: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("agent record removed despite ambiguous probe; statErr=%v", statErr)
	}
}

// TestDropReplacementRecord_KillFailsButSessionGone_RecordRemoved
// covers the race: tmux.Kill returned an error, but the session
// vanished concurrently (operator manual kill, OS shutdown). The
// post-probe confirms dead, so it's safe to remove the record. A
// note goes to stderr so the caller's logs flag the unusual path.
func TestDropReplacementRecord_KillFailsButSessionGone_RecordRemoved(t *testing.T) {
	setupFleetHome(t)
	rec := agent.New("racecase")
	rec.TmuxSession = "fleet-racecase"
	if err := rec.Write(); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	path, err := state.AgentPath(rec.ID)
	if err != nil {
		t.Fatalf("state.AgentPath: %v", err)
	}

	killErr := errors.New("simulated kill error")
	origKill := tmuxKillFn
	origAlive := tmuxSessionAliveFn
	tmuxKillFn = func(string) error { return killErr }
	tmuxSessionAliveFn = func(string) (bool, error) { return false, nil } // dead
	t.Cleanup(func() { tmuxKillFn = origKill; tmuxSessionAliveFn = origAlive })

	var stderr bytes.Buffer
	if err := DropReplacementRecord(rec.TmuxSession, rec.ID, &stderr); err != nil {
		t.Fatalf("DropReplacementRecord: expected nil (session is genuinely dead); got %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("record should be removed when post-probe confirms dead; statErr=%v", statErr)
	}
	if !strings.Contains(stderr.String(), "session is gone") {
		t.Errorf("stderr should note the kill-error-but-session-gone race; got: %s", stderr.String())
	}
}

// TestDropReplacementRecord_EmptySession_SkipsKill is for the legacy
// path where the record was written but tmux.Spawn never ran — no
// session to kill, just remove the record.
func TestDropReplacementRecord_EmptySession_SkipsKill(t *testing.T) {
	setupFleetHome(t)
	rec := agent.New("nosession")
	rec.TmuxSession = ""
	if err := rec.Write(); err != nil {
		t.Fatalf("rec.Write: %v", err)
	}
	path, err := state.AgentPath(rec.ID)
	if err != nil {
		t.Fatalf("state.AgentPath: %v", err)
	}

	called := false
	origKill := tmuxKillFn
	tmuxKillFn = func(string) error { called = true; return nil }
	t.Cleanup(func() { tmuxKillFn = origKill })

	if err := DropReplacementRecord("", rec.ID, nil); err != nil {
		t.Fatalf("DropReplacementRecord: %v", err)
	}
	if called {
		t.Errorf("Kill should not be invoked for empty session")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("record should be removed; statErr=%v", statErr)
	}
}

// TestDropReplacementRecord_RecordAlreadyMissing_NoError is a
// defensive case: a concurrent cleanup removed the record between when
// the caller decided "drop this replacement" and when DropReplacementRecord
// ran. Surface the absence as a no-op success — re-reporting the
// missing file would force the caller into a needless rollback branch.
func TestDropReplacementRecord_RecordAlreadyMissing_NoError(t *testing.T) {
	setupFleetHome(t)
	// Don't seed any record.
	origKill := tmuxKillFn
	origAlive := tmuxSessionAliveFn
	tmuxKillFn = func(string) error { return nil }
	tmuxSessionAliveFn = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() { tmuxKillFn = origKill; tmuxSessionAliveFn = origAlive })

	if err := DropReplacementRecord("fleet-ghost", "ghost", nil); err != nil {
		t.Errorf("DropReplacementRecord: missing record should be a no-op; got %v", err)
	}
}

// TestDropReplacementRecord_EmptyRecID_ReturnsError pins the bad-input
// guard. Callers shouldn't pass an empty recID, but if they do we'd
// rather fail loud than os.Remove(~/.fleet/agents/.json) by accident.
func TestDropReplacementRecord_EmptyRecID_ReturnsError(t *testing.T) {
	setupFleetHome(t)
	if err := DropReplacementRecord("fleet-x", "", nil); err == nil {
		t.Errorf("expected error on empty recID; got nil")
	}
}
