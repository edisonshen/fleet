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
	"strings"
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
	r := agent.New(id)
	r.Project = project
	r.TaskID = projectlookup.CoordTaskID(project)
	r.TmuxSession = "fleet-" + id
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

	writeD9eRecord(t, "realholder", project)
	attachCalls := installD9eStubs(t, map[string]bool{"fleet-realholder": true})

	err := resolveAndAttachCoord("tok", project, AttachOpts{ResolveMaxAttempts: 1, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("resolveAndAttachCoord: unexpected error %v, want a successful D9e record-fallback attach", err)
	}
	if *attachCalls != 1 {
		t.Fatalf("attachFnVar called %d times; want exactly 1 (attach into the real flock holder's record)", *attachCalls)
	}
}
