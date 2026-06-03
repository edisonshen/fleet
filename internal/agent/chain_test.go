// Chain tests — v0.13 schema v2 + chain-following resolver.
//
// Scope (per dispatch task brief handoff-identity-cont-3f1d):
//  1. Schema v2 — agent record adds SuccessorID, PredecessorID,
//     ArchivedCause. v1 records load cleanly.
//  2. LoadArchive — reads agents/archive/<id>.json (handoff-chain
//     starting point when live record is gone).
//  3. ResolveChain — walks live -> archive transitively when
//     archived_cause="handoff" and successor_id is set. Cycle guard.
//     Dead-chain surface ("archived (cause=X); no live successor")
//     and live-tail-hit return.
package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSchemaVersion_BumpedToV2 pins the schema bump itself — if a future
// PR forgets to bump (or bumps further), the test catches it. The bump
// signals to readers that records may now carry chain fields.
func TestSchemaVersion_BumpedToV2(t *testing.T) {
	if SchemaVersion != 2 {
		t.Errorf("SchemaVersion: got %d want 2 (chain fields added)", SchemaVersion)
	}
}

// TestNew_DefaultsForChainFields pins that fresh records have empty
// chain fields (no predecessor on a brand-new dispatch, no successor
// until archive).
func TestNew_DefaultsForChainFields(t *testing.T) {
	r := New("a1b2c3d4")
	if r.PredecessorID != "" {
		t.Errorf("PredecessorID: got %q want empty (fresh record)", r.PredecessorID)
	}
	if r.SuccessorID != "" {
		t.Errorf("SuccessorID: got %q want empty (fresh record)", r.SuccessorID)
	}
	if r.ArchivedCause != "" {
		t.Errorf("ArchivedCause: got %q want empty (live record)", r.ArchivedCause)
	}
}

// TestWriteLoad_RoundTripsChainFields pins JSON round-trip for the
// chain fields so reader/writer parity holds.
func TestWriteLoad_RoundTripsChainFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New("aaaaaaaa")
	r.PredecessorID = "predprdp"
	if err := r.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load("aaaaaaaa")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PredecessorID != "predprdp" {
		t.Errorf("PredecessorID round-trip: got %q want %q", got.PredecessorID, "predprdp")
	}
}

// TestLoad_V1RecordWithNoChainFields pins backcompat: a v1 record
// (no schema_version, no chain fields) loads cleanly with empty
// chain fields. Older fleet installs upgrade in place without
// rewriting every archive.
func TestLoad_V1RecordWithNoChainFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Hand-craft v1 shape — no schema_version, no successor/predecessor.
	v1 := `{
  "id": "v1agentx",
  "engine": "claude-code",
  "role": "executor",
  "task_id": "t",
  "project": "p"
}`
	if err := os.WriteFile(filepath.Join(tmp, "agents", "v1agentx.json"),
		[]byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load("v1agentx")
	if err != nil {
		t.Fatalf("Load v1 record: %v", err)
	}
	if got.PredecessorID != "" {
		t.Errorf("PredecessorID: got %q want empty (v1 had no field)", got.PredecessorID)
	}
	if got.SuccessorID != "" {
		t.Errorf("SuccessorID: got %q want empty (v1)", got.SuccessorID)
	}
	if got.ArchivedCause != "" {
		t.Errorf("ArchivedCause: got %q want empty (v1)", got.ArchivedCause)
	}
}

// TestLoadArchive_ReadsArchivedRecord pins that LoadArchive walks
// the agents/archive/ dir and returns the parsed record. Resolver
// needs this to start chain walks from an archived predecessor id.
func TestLoadArchive_ReadsArchivedRecord(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New("archivd1")
	r.SuccessorID = "succssr1"
	r.ArchivedCause = ArchivedCauseHandoff
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", "archivd1.json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadArchive("archivd1")
	if err != nil {
		t.Fatalf("LoadArchive: %v", err)
	}
	if got.ID != "archivd1" {
		t.Errorf("ID: got %q want archivd1", got.ID)
	}
	if got.SuccessorID != "succssr1" {
		t.Errorf("SuccessorID: got %q want succssr1", got.SuccessorID)
	}
	if got.ArchivedCause != ArchivedCauseHandoff {
		t.Errorf("ArchivedCause: got %q want %q", got.ArchivedCause, ArchivedCauseHandoff)
	}
}

// TestLoadArchive_NotFound returns ErrNotFound (state.ErrNotFound)
// for missing archives, so the resolver can distinguish "no archive"
// from "read error".
func TestLoadArchive_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadArchive("missing0")
	if err == nil {
		t.Fatal("LoadArchive: expected error for missing archive")
	}
	if !isNotFoundErr(err) {
		t.Errorf("expected NotFound-shaped error, got: %v", err)
	}
}

// isNotFoundErr is a test helper that flexibly recognizes the "not
// found" error class returned by Load/LoadArchive (currently
// state.ErrNotFound wrapped; switching error sentinel must keep the
// is-check valid in callers).
func isNotFoundErr(err error) bool {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if cur.Error() != "" && (containsAny(cur.Error(), "not found", "no such file")) {
			return true
		}
	}
	return false
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if len(s) >= len(n) {
			for i := 0; i+len(n) <= len(s); i++ {
				if s[i:i+len(n)] == n {
					return true
				}
			}
		}
	}
	return false
}

// TestResolveChain_LiveHit returns the live record directly with
// hops=0 when the id is currently live.
func TestResolveChain_LiveHit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New("liveeeee")
	if err := r.Write(); err != nil {
		t.Fatal(err)
	}
	got, hops, err := ResolveChain("liveeeee")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if got.ID != "liveeeee" {
		t.Errorf("ID: got %q want liveeeee", got.ID)
	}
	if hops != 0 {
		t.Errorf("hops: got %d want 0 (live hit, no chain walk)", hops)
	}
}

// TestResolveChain_OneHopLiveTail — A archived (cause=handoff,
// successor=B); B live. ResolveChain("A") returns B with hops=1.
func TestResolveChain_OneHopLiveTail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Predecessor archived with handoff -> succ=B.
	a := New("aaaaaaaa")
	a.SuccessorID = "bbbbbbbb"
	a.ArchivedCause = ArchivedCauseHandoff
	data, _ := json.MarshalIndent(a, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", "aaaaaaaa.json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Successor live.
	b := New("bbbbbbbb")
	b.PredecessorID = "aaaaaaaa"
	if err := b.Write(); err != nil {
		t.Fatal(err)
	}

	got, hops, err := ResolveChain("aaaaaaaa")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if got.ID != "bbbbbbbb" {
		t.Errorf("tail ID: got %q want bbbbbbbb", got.ID)
	}
	if hops != 1 {
		t.Errorf("hops: got %d want 1", hops)
	}
}

// TestResolveChain_ThreeHopsLiveTail — A -> B -> C -> D, all handoff,
// D live. ResolveChain("A") returns D with hops=3.
func TestResolveChain_ThreeHopsLiveTail(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{"aaaaaaaa": "bbbbbbbb", "bbbbbbbb": "cccccccc", "cccccccc": "dddddddd"} {
		r := New(src)
		r.SuccessorID = dst
		r.ArchivedCause = ArchivedCauseHandoff
		data, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", src+".json"),
			data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d := New("dddddddd")
	d.PredecessorID = "cccccccc"
	if err := d.Write(); err != nil {
		t.Fatal(err)
	}

	got, hops, err := ResolveChain("aaaaaaaa")
	if err != nil {
		t.Fatalf("ResolveChain: %v", err)
	}
	if got.ID != "dddddddd" {
		t.Errorf("tail: got %q want dddddddd", got.ID)
	}
	if hops != 3 {
		t.Errorf("hops: got %d want 3", hops)
	}
}

// TestResolveChain_CycleGuard — A.succ=B, B.succ=A. Resolver detects
// cycle and returns ErrChainCycle.
func TestResolveChain_CycleGuard(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	for src, dst := range map[string]string{"aaaaaaaa": "bbbbbbbb", "bbbbbbbb": "aaaaaaaa"} {
		r := New(src)
		r.SuccessorID = dst
		r.ArchivedCause = ArchivedCauseHandoff
		data, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", src+".json"),
			data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := ResolveChain("aaaaaaaa")
	if !errors.Is(err, ErrChainCycle) {
		t.Errorf("expected ErrChainCycle, got %v", err)
	}
}

// TestResolveChain_DeadChain_NoSuccessor — A archived with cause=kill
// (no successor). Surfaces ErrNoLiveSuccessor with cause embedded.
func TestResolveChain_DeadChain_NoSuccessor(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New("deadkill")
	r.ArchivedCause = "kill"
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", "deadkill.json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveChain("deadkill")
	if !errors.Is(err, ErrNoLiveSuccessor) {
		t.Fatalf("expected ErrNoLiveSuccessor, got %v", err)
	}
	// Error message must include the cause so the operator can act.
	if !containsAny(err.Error(), "cause=kill") {
		t.Errorf("error must include cause string; got %q", err.Error())
	}
}

// TestResolveChain_DeadChain_HandoffSuccessorMissing — A archived
// cause=handoff, successor=B, but B has no live record AND no
// archive record. Surfaces ErrNoLiveSuccessor.
func TestResolveChain_DeadChain_HandoffSuccessorMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := New("aaaaaaaa")
	r.SuccessorID = "missing0"
	r.ArchivedCause = ArchivedCauseHandoff
	data, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "agents", "archive", "aaaaaaaa.json"),
		data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveChain("aaaaaaaa")
	if !errors.Is(err, ErrNoLiveSuccessor) {
		t.Errorf("expected ErrNoLiveSuccessor for broken chain, got %v", err)
	}
}

// TestResolveChain_TokenUnknown — id not found in live or archive.
// Returns ErrNotFound-class (so callers can distinguish "agent
// never existed" from "chain dead").
func TestResolveChain_TokenUnknown(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FLEET_HOME", tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "agents", "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveChain("nosuchid")
	if err == nil {
		t.Fatal("expected error for unknown token")
	}
	if !isNotFoundErr(err) {
		t.Errorf("expected NotFound-shaped error, got %v", err)
	}
}
