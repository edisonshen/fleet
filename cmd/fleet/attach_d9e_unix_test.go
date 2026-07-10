//go:build linux || darwin

package main

// attach_d9e_unix_test.go — real-flock coverage for resolveAndAttachCoord's
// D9e bounded-Wait record-fallback (cmd/fleet/attach.go). Requires a genuine
// coordlock.AcquireLease, which has no cross-platform stub, hence the build
// tag (mirrors the atomic_coord_swap_unix_test.go split, review iter-7).
//
// D9e had ZERO existing test coverage before this review (every prior test
// exercises it only through a fully-mocked coordreconcile.Resolve, against a
// FLEET_HOME where no real flock is ever created — so the "flock genuinely
// busy" branch never actually fires in the existing suite). This proves both
// directions of the codex-flagged [P2] gate directly against the real
// coordlock primitives:
//
//   - flock genuinely FREE + a matching alive-session record exists ⇒ the
//     record-fallback must NOT fire (would attach into an unrelated/orphan
//     session); the caller gets the "not attachable" diagnostic instead.
//   - flock genuinely BUSY (a real held lease) + a matching alive-session
//     record for THAT holder exists ⇒ the record-fallback DOES fire and
//     attaches into it (the D9e recovery path this whole branch exists for).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/projectlookup"
	"github.com/edisonshen/fleet/internal/state"
)

// installD9eStubs wires the minimal seam set resolveAndAttachCoord needs,
// with attachResolveFnVar ALWAYS returning a bounded Wait (simulating an
// identity that Resolve can never read) so every call exhausts
// ResolveMaxAttempts and reaches the D9e fallback deterministically. Returns
// a pointer to the attach-call counter the caller asserts against.
func installD9eStubs(t *testing.T, aliveSessions map[string]bool) *int {
	t.Helper()
	attachCalls := 0

	prevAttach := attachFnVar
	attachFnVar = func(session string) error {
		attachCalls++
		return nil
	}
	prevTmuxAvailable := tmuxAvailableFnVar
	tmuxAvailableFnVar = func() error { return nil }
	prevNewID := attachNewAgentIDFnVar
	attachNewAgentIDFnVar = func() string { return "d9etoken" }
	prevResolve := attachResolveFnVar
	attachResolveFnVar = func(project, agentID string) (coordreconcile.Verdict, error) {
		return coordreconcile.Verdict{Decision: coordreconcile.Wait, Reason: "test: identity unreadable"}, nil
	}
	restorePL := projectlookup.SetTestStubs(
		func(session string) bool { return aliveSessions[session] },
		func(session string) (bool, error) { return aliveSessions[session], nil },
		func() ([]string, error) {
			var out []string
			for s := range aliveSessions {
				out = append(out, s)
			}
			return out, nil
		},
	)
	t.Cleanup(func() {
		attachFnVar = prevAttach
		tmuxAvailableFnVar = prevTmuxAvailable
		attachNewAgentIDFnVar = prevNewID
		attachResolveFnVar = prevResolve
		restorePL()
	})
	return &attachCalls
}

func writeD9eRecord(t *testing.T, id, project string) {
	t.Helper()
	writeD9eRecordWithPID(t, id, project, 0)
}

// writeD9eRecordWithPID writes a coord-tagged record for id/project, also
// stamping SupervisorPID so FindLiveCoordPreferPID's strict PID match (codex
// confirm round [P2] #3) can recognize it as the confirmed flock holder's
// record. pid<=0 leaves SupervisorPID unset (the "unrelated orphan record"
// shape most D9e tests want).
func writeD9eRecordWithPID(t *testing.T, id, project string, pid int) {
	t.Helper()
	r := agent.New(id)
	r.Project = project
	r.TaskID = projectlookup.CoordTaskID(project)
	r.TmuxSession = "fleet-" + id
	if pid > 0 {
		r.SupervisorPID = pid
	}
	if err := r.Write(); err != nil {
		t.Fatalf("write agent record %s: %v", id, err)
	}
}

// TestResolveAndAttachCoord_D9e_FreeFlock_SkipsRecordFallback is the codex
// confirm-round [P2] regression guard: a bounded Wait with a GENUINELY FREE
// flock (no live holder at all — e.g. a handoff in flight) must NOT fall
// back to an arbitrary project-matching agent record. Before the fix,
// FindLiveCoordPreferPID(records, project, 0) would happily return the first
// alive-session match regardless of whether any real flock holder existed.
func TestResolveAndAttachCoord_D9e_FreeFlock_SkipsRecordFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const project = "d9e-free"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// An unrelated but project-matching, alive-session record — the thing a
	// naive first-match fallback would wrongly attach into. No real flock is
	// ever acquired for this project in this test.
	writeD9eRecord(t, "orphanrec", project)
	attachCalls := installD9eStubs(t, map[string]bool{"fleet-orphanrec": true})

	err := resolveAndAttachCoord("tok", project, AttachOpts{ResolveMaxAttempts: 1, Stderr: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("resolveAndAttachCoord: err=nil, want the bounded-wait-exhausted diagnostic")
	}
	if *attachCalls != 0 {
		t.Fatalf("attachFnVar called %d times; want 0 — must NOT attach into an unrelated record when the flock is genuinely free", *attachCalls)
	}
	if got := err.Error(); !strings.Contains(got, "no live coord record is derivable") {
		t.Fatalf("error = %q, want the \"no live coord record is derivable\" diagnostic", got)
	}
}

// TestResolveAndAttachCoord_D9e_BusyTornBody_SkipsRecordFallback covers the
// SECOND codex confirm-round [P2]: the flock can be genuinely BUSY (a real
// live LOCK_EX holder) while its body is torn beyond even a readable PID —
// coordlock.CurrentActiveOwnerPID then returns ok=false too (not just
// LiveOwner-busy=true), so there is STILL no PID to cross-check a candidate
// record against. Must behave identically to the genuinely-free case: skip
// the fallback, surface the honest diagnostic.
func TestResolveAndAttachCoord_D9e_BusyTornBody_SkipsRecordFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const project = "d9e-torn"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// Hold a REAL flock lock with a torn (non-JSON) body — busy, but no PID
	// is readable from it at all.
	pdir, err := state.ProjectDir(project)
	if err != nil {
		t.Fatalf("ProjectDir: %v", err)
	}
	lockDir := filepath.Join(pdir, ".locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	flockPath := filepath.Join(lockDir, "coordinator.flock")
	f, err := os.OpenFile(flockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open flock: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock LOCK_EX: %v", err)
	}
	if _, err := f.WriteString("not valid json, no pid field here"); err != nil {
		t.Fatalf("write torn body: %v", err)
	}
	// Held for the test's duration; released by t.Cleanup's f.Close().

	// An unrelated but project-matching, alive-session record — the thing a
	// naive fallback (or a fallback keyed on LiveOwner-busy alone) would
	// wrongly attach into.
	writeD9eRecord(t, "orphanrec2", project)
	attachCalls := installD9eStubs(t, map[string]bool{"fleet-orphanrec2": true})

	gotErr := resolveAndAttachCoord("tok", project, AttachOpts{ResolveMaxAttempts: 1, Stderr: &bytes.Buffer{}})
	if gotErr == nil {
		t.Fatal("resolveAndAttachCoord: err=nil, want the bounded-wait-exhausted diagnostic")
	}
	if *attachCalls != 0 {
		t.Fatalf("attachFnVar called %d times; want 0 — must NOT attach into an unrelated record when the flock body is torn beyond a readable PID", *attachCalls)
	}
	if got := gotErr.Error(); !strings.Contains(got, "no live coord record is derivable") {
		t.Fatalf("error = %q, want the \"no live coord record is derivable\" diagnostic", got)
	}
}

// TestResolveAndAttachCoord_D9e_BusyFlock_UsesRecordFallback is the positive
// case: a REAL held flock (genuinely busy, identity unreadable via the
// mocked Resolve) with a matching alive-session record for that SAME holder
// must still recover through the D9e agent-record fallback — proving the
// new gate doesn't regress the mechanism it's meant to protect.
func TestResolveAndAttachCoord_D9e_BusyFlock_UsesRecordFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const project = "d9e-busy"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// A REAL held flock — this process is the live (if identity-unreadable,
	// per the mocked Resolve) holder.
	lease, acquired, lerr := coordlock.AcquireLease(project, "realholder")
	if lerr != nil || !acquired || lease == nil {
		t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, lerr)
	}
	t.Cleanup(lease.Release)

	// AcquireLease stamps THIS test process's real pid into the flock body
	// (no subprocess involved) — the record must carry that SAME pid as its
	// SupervisorPID to be recognized as the confirmed holder's record under
	// the strict PID match (codex confirm round [P2] #3).
	writeD9eRecordWithPID(t, "realholder", project, os.Getpid())
	attachCalls := installD9eStubs(t, map[string]bool{"fleet-realholder": true})

	err := resolveAndAttachCoord("tok", project, AttachOpts{ResolveMaxAttempts: 1, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("resolveAndAttachCoord: unexpected error %v, want a successful D9e record-fallback attach", err)
	}
	if *attachCalls != 1 {
		t.Fatalf("attachFnVar called %d times; want exactly 1 (attach into the real flock holder's record)", *attachCalls)
	}
}

// TestResolveAndAttachCoord_D9e_KnownPID_NoMatchingRecord_SkipsRecordFallback
// is the THIRD codex confirm-round [P2]: a CONFIRMED holder PID (the flock
// is genuinely busy, identity readable) with NO on-disk record claiming that
// SupervisorPID must still refuse to guess — even when an unrelated, alive,
// project-matching orphan record exists. FindLiveCoordPreferPID must return
// ok=false rather than degrading into a first-match guess.
func TestResolveAndAttachCoord_D9e_KnownPID_NoMatchingRecord_SkipsRecordFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const project = "d9e-nomatch"
	if _, err := state.EnsureProjectInitialized(project); err != nil {
		t.Fatalf("EnsureProjectInitialized: %v", err)
	}

	// A REAL held flock naming a specific holder identity ("realholder2") —
	// CurrentActiveOwnerPID will report THIS process's pid.
	lease, acquired, lerr := coordlock.AcquireLease(project, "realholder2")
	if lerr != nil || !acquired || lease == nil {
		t.Fatalf("AcquireLease: acquired=%v err=%v", acquired, lerr)
	}
	t.Cleanup(lease.Release)

	// Deliberately do NOT write a record for "realholder2" — the true holder
	// has no matching on-disk record. Only an UNRELATED orphan record exists
	// for the project, which a first-match guess would wrongly pick.
	writeD9eRecord(t, "unrelatedorphan", project)
	attachCalls := installD9eStubs(t, map[string]bool{"fleet-unrelatedorphan": true})

	err := resolveAndAttachCoord("tok", project, AttachOpts{ResolveMaxAttempts: 1, Stderr: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("resolveAndAttachCoord: err=nil, want the bounded-wait-exhausted diagnostic")
	}
	if *attachCalls != 0 {
		t.Fatalf("attachFnVar called %d times; want 0 — a confirmed PID with no matching record must never fall back to an unrelated orphan", *attachCalls)
	}
	if got := err.Error(); !strings.Contains(got, "no live coord record is derivable") {
		t.Fatalf("error = %q, want the \"no live coord record is derivable\" diagnostic", got)
	}
}
