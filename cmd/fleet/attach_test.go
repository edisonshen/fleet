package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/agent"
)

// attach tests for handoff-identity-cont-3f1d.
//
// Scope (dispatch task brief):
//   - chain-following resolver in attach.go: live -> archive walk
//   - "rotated through N hops" diagnostic to stderr
//   - dead-chain surface: "archived (cause=X); no live successor"

// fakeAttach records the session it was asked to attach to, and lets
// us return nil so the cobra RunE flow can finish without exec'ing
// tmux. Replaces tmux.Attach inside runAttach for tests.
type fakeAttach struct {
	called  bool
	session string
}

func (f *fakeAttach) Attach(session string) error {
	f.called = true
	f.session = session
	return nil
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

func writeArchivedNonHandoff(t *testing.T, tmp, id, cause string) {
	t.Helper()
	r := agent.New(id)
	r.ArchivedCause = cause
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", id+".json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAttach_LiveAgent_DirectAttach — no chain walk needed; attach
// straight to the live agent's tmux session.
func TestAttach_LiveAgent_DirectAttach(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeLiveAgent(t, tmp, "liveeeee")

	fa := &fakeAttach{}
	var stderr bytes.Buffer
	err := runAttach("liveeeee", &stderr, fa.Attach)
	if err != nil {
		t.Fatalf("runAttach: %v", err)
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
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeArchivedHandoff(t, tmp, "aaaaaaaa", "bbbbbbbb")
	writeLiveAgent(t, tmp, "bbbbbbbb")

	fa := &fakeAttach{}
	var stderr bytes.Buffer
	err := runAttach("aaaaaaaa", &stderr, fa.Attach)
	if err != nil {
		t.Fatalf("runAttach: %v", err)
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
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeArchivedHandoff(t, tmp, "aaaaaaaa", "bbbbbbbb")
	writeArchivedHandoff(t, tmp, "bbbbbbbb", "cccccccc")
	writeArchivedHandoff(t, tmp, "cccccccc", "dddddddd")
	writeLiveAgent(t, tmp, "dddddddd")

	fa := &fakeAttach{}
	var stderr bytes.Buffer
	err := runAttach("aaaaaaaa", &stderr, fa.Attach)
	if err != nil {
		t.Fatalf("runAttach: %v", err)
	}
	if fa.session != "fleet-dddddddd" {
		t.Errorf("session: got %q want fleet-dddddddd", fa.session)
	}
	if !strings.Contains(stderr.String(), "rotated through 3 hops") {
		t.Errorf("stderr missing 'rotated through 3 hops'; got: %q", stderr.String())
	}
}

// TestAttach_DeadChain_NonHandoff — predecessor archived cause=kill,
// no successor. Surfaces "archived (cause=kill); no live successor"
// and does NOT attach.
func TestAttach_DeadChain_NonHandoff(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeArchivedNonHandoff(t, tmp, "deadkill", "kill")

	fa := &fakeAttach{}
	var stderr bytes.Buffer
	err := runAttach("deadkill", &stderr, fa.Attach)
	if err == nil {
		t.Fatal("expected error for dead chain")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cause=kill") || !strings.Contains(msg, "no live successor") {
		t.Errorf("error must say 'cause=kill' and 'no live successor'; got %q", msg)
	}
	if fa.called {
		t.Errorf("attach should NOT be invoked on dead chain (was called with %q)", fa.session)
	}
}

// TestAttach_CycleGuard — A.succ=B, B.succ=A. Resolver returns
// cycle error; runAttach surfaces it without an infinite loop.
func TestAttach_CycleGuard(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	writeArchivedHandoff(t, tmp, "aaaaaaaa", "bbbbbbbb")
	writeArchivedHandoff(t, tmp, "bbbbbbbb", "aaaaaaaa")

	fa := &fakeAttach{}
	var stderr bytes.Buffer
	err := runAttach("aaaaaaaa", &stderr, fa.Attach)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if !errors.Is(err, agent.ErrChainCycle) {
		t.Errorf("expected ErrChainCycle, got %v", err)
	}
	if fa.called {
		t.Errorf("attach must not be invoked on cycle")
	}
}

// TestAttach_UnknownToken — no record anywhere. Reports
// "no agent record" (preserves the existing fleet attach UX —
// not in scope to add project recovery here per dispatch task).
func TestAttach_UnknownToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}

	fa := &fakeAttach{}
	var stderr bytes.Buffer
	err := runAttach("missing0", &stderr, fa.Attach)
	if err == nil {
		t.Fatal("expected error for unknown token")
	}
	if fa.called {
		t.Errorf("attach must not be invoked when id unknown")
	}
	if !strings.Contains(err.Error(), "missing0") {
		t.Errorf("error must mention the id; got %q", err.Error())
	}
}
