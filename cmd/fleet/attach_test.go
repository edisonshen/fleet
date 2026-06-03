package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
)

// attach tests — Tier 1 (live) + Tier 2 (chain) happy paths. The
// post-attach-failover-59db design lets Tier 3 cover every failure
// case (cycle, broken chain, non-handoff archive, unknown token);
// those tests now live in attach_failover_test.go under names F1–F18.
// Each test below pins a Tier-1/Tier-2 hit and confirms attach is
// invoked exactly once on the resolved tail.

// fakeAttach records the session it was asked to attach to and lets
// us return nil so the cobra RunE flow can finish without exec'ing
// tmux. Replaces tmux.Attach inside runAttachFailover for tests via
// attachFnVar.
type fakeAttach struct {
	called  bool
	session string
}

func (f *fakeAttach) Attach(session string) error {
	f.called = true
	f.session = session
	return nil
}

// installFakeAttach sets up FLEET_HOME + stubs attachFnVar +
// tmuxAvailableFnVar so the Tier 1/2 happy paths don't need a real
// tmux binary. Returns the fake recorder.
func installFakeAttach(t *testing.T) *fakeAttach {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	fa := &fakeAttach{}
	prev := attachFnVar
	attachFnVar = fa.Attach
	prevAvail := tmuxAvailableFnVar
	tmuxAvailableFnVar = func() error { return nil }
	// Tier 1 / Tier 2 happy-path tests assume the resolved session is
	// alive — without this stub the post-codex-iter-1 stale-live-record
	// gate would probe the real tmux for a fake "fleet-liveeeee" session,
	// see "definitively dead", and fail over into Tier 3 (which is the
	// wrong code path for these tests). Codex review iter-1 P1.
	prevProbe := sessionProbeFnVar
	sessionProbeFnVar = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		attachFnVar = prev
		tmuxAvailableFnVar = prevAvail
		sessionProbeFnVar = prevProbe
	})
	return fa
}

func writeArchivedHandoff(t *testing.T, tmp, id, succ string) {
	t.Helper()
	r := agent.New(id)
	r.SuccessorID = succ
	r.ArchivedCause = agent.ArchivedCauseHandoff
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", id+".json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLiveAgent(t *testing.T, tmp, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := agent.New(id)
	r.TmuxSession = "fleet-" + id
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
}

// TestAttach_LiveAgent_DirectAttach — no chain walk needed; attach
// straight to the live agent's tmux session.
func TestAttach_LiveAgent_DirectAttach(t *testing.T) {
	fa := installFakeAttach(t)
	tmp := os.Getenv("FLEET_HOME")
	writeLiveAgent(t, tmp, "liveeeee")

	var stderr bytes.Buffer
	err := runAttachFailover("liveeeee", AttachOpts{Stderr: &stderr})
	if err != nil {
		t.Fatalf("runAttachFailover: %v", err)
	}
	if !fa.called {
		t.Fatal("attach not invoked")
	}
	if fa.session != "fleet-liveeeee" {
		t.Errorf("session: got %q want fleet-liveeeee", fa.session)
	}
}

// TestAttach_ChainOneHop — A handed off to B (live). attach A walks
// the chain and attaches to B; stderr shows the rotation.
func TestAttach_ChainOneHop_AttachesTailAndDiagnoses(t *testing.T) {
	fa := installFakeAttach(t)
	tmp := os.Getenv("FLEET_HOME")
	writeArchivedHandoff(t, tmp, "aaaaaaaa", "bbbbbbbb")
	writeLiveAgent(t, tmp, "bbbbbbbb")

	var stderr bytes.Buffer
	err := runAttachFailover("aaaaaaaa", AttachOpts{Stderr: &stderr})
	if err != nil {
		t.Fatalf("runAttachFailover: %v", err)
	}
	if fa.session != "fleet-bbbbbbbb" {
		t.Errorf("session: got %q want fleet-bbbbbbbb", fa.session)
	}
	got := stderr.String()
	if !strings.Contains(got, "rotated through 1 hop") {
		t.Errorf("stderr missing 'rotated through 1 hop'; got: %q", got)
	}
	if !strings.Contains(got, "aaaaaaaa") || !strings.Contains(got, "bbbbbbbb") {
		t.Errorf("stderr missing pred/succ ids; got: %q", got)
	}
}

// TestAttach_ChainThreeHops — diagnostic says "3 hops".
func TestAttach_ChainThreeHops_AttachesTailAndDiagnoses(t *testing.T) {
	fa := installFakeAttach(t)
	tmp := os.Getenv("FLEET_HOME")
	writeArchivedHandoff(t, tmp, "aaaaaaaa", "bbbbbbbb")
	writeArchivedHandoff(t, tmp, "bbbbbbbb", "cccccccc")
	writeArchivedHandoff(t, tmp, "cccccccc", "dddddddd")
	writeLiveAgent(t, tmp, "dddddddd")

	var stderr bytes.Buffer
	err := runAttachFailover("aaaaaaaa", AttachOpts{Stderr: &stderr})
	if err != nil {
		t.Fatalf("runAttachFailover: %v", err)
	}
	if fa.session != "fleet-dddddddd" {
		t.Errorf("session: got %q want fleet-dddddddd", fa.session)
	}
	if !strings.Contains(stderr.String(), "rotated through 3 hops") {
		t.Errorf("stderr missing 'rotated through 3 hops'; got: %q", stderr.String())
	}
}

// NOTE: the following pre-attach-failover-59db tests are intentionally
// superseded by the F1/F3/F4 cases in attach_failover_test.go — under
// the new contract, attach NEVER returns an error for cycle / dead-
// chain / unknown-token paths; it failovers into Tier 3 PROJECT
// RECOVERY. The new test matrix covers all of those branches with
// exact-string stderr assertions.
//
//   removed: TestAttach_DeadChain_NonHandoff  -> see TestF3
//   removed: TestAttach_CycleGuard            -> see TestF1
//   removed: TestAttach_UnknownToken          -> see TestF4 / TestF12
