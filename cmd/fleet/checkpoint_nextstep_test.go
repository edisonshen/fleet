package main

// checkpoint_nextstep_test.go — `fleet checkpoint next-step` RMW behavior:
// session_next_steps append/dedupe/cap, coord_id stamp, newline-sanitize,
// empty-text rejection, --slug, sibling-key preservation, and lock
// serialization. See docs/TASK-PLAN-handoff-next-steps-open-21c2.md (T1–T6).

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/edisonshen/fleet/internal/state"
)

// sessionNextStepsOf pulls session_next_steps out of a coord-state map.
func sessionNextStepsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["session_next_steps"].([]any)
	if !ok {
		t.Fatalf("session_next_steps missing or wrong type: %#v", m["session_next_steps"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// T1 — fresh state: append records {text, coord_id, ts}.
func TestCheckpointNextStep_AppendsEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	t.Setenv("FLEET_AGENT_ID", "cafe0123")
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	seedCoordStateFile(t, home, "myproj", `{"tick_count":4}`)

	if err := runCheckpoint(t, "next-step", "--project", "myproj", "revive codex-engine-mvp"); err != nil {
		t.Fatalf("next-step: %v", err)
	}
	m := readCoordState(t, home, "myproj")
	steps := sessionNextStepsOf(t, m)
	if len(steps) != 1 || steps[0]["text"] != "revive codex-engine-mvp" {
		t.Fatalf("session_next_steps: got %#v", steps)
	}
	if steps[0]["coord_id"] != "cafe0123" {
		t.Errorf("entry not coord-stamped: %#v", steps[0])
	}
	if ts, _ := steps[0]["ts"].(string); ts == "" {
		t.Errorf("entry missing ts: %#v", steps[0])
	}
	if tc, _ := m["tick_count"].(float64); tc != 4 {
		t.Errorf("sibling tick_count clobbered: %v", m["tick_count"])
	}
}

// T2 — cap: 12 appends keep only the newest checkpointNextStepsMax (10).
func TestCheckpointNextStep_CapsToNewest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	const n = checkpointNextStepsMax + 2 // 12 > cap 10
	for i := 0; i < n; i++ {
		if err := runCheckpoint(t, "next-step", "--project", "myproj",
			fmt.Sprintf("step-%02d", i)); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	steps := sessionNextStepsOf(t, readCoordState(t, home, "myproj"))
	if len(steps) != checkpointNextStepsMax {
		t.Fatalf("expected cap %d, got %d", checkpointNextStepsMax, len(steps))
	}
	// Oldest two dropped; newest at tail.
	if steps[0]["text"] != "step-02" {
		t.Errorf("oldest not trimmed: head=%v want step-02", steps[0]["text"])
	}
	if steps[len(steps)-1]["text"] != fmt.Sprintf("step-%02d", n-1) {
		t.Errorf("newest not at tail: %v", steps[len(steps)-1]["text"])
	}
}

// T3 — sanitize: embedded CR/LF flattened to spaces (no forged `## header`).
func TestCheckpointNextStep_FlattensNewlines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	inject := "do the thing\n## INJECTED\r- forged: x"
	if err := runCheckpoint(t, "next-step", "--project", "myproj", inject); err != nil {
		t.Fatalf("next-step: %v", err)
	}
	steps := sessionNextStepsOf(t, readCoordState(t, home, "myproj"))
	got, _ := steps[0]["text"].(string)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("stored text retains CR/LF (injection vector): %q", got)
	}
	if got != "do the thing ## INJECTED - forged: x" {
		t.Errorf("flatten drift: got %q", got)
	}
}

// Dedupe by exact text (last wins → tail).
func TestCheckpointNextStep_DedupeExactText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, txt := range []string{"aaa", "bbb", "aaa"} {
		if err := runCheckpoint(t, "next-step", "--project", "myproj", txt); err != nil {
			t.Fatalf("next-step %q: %v", txt, err)
		}
	}
	steps := sessionNextStepsOf(t, readCoordState(t, home, "myproj"))
	if len(steps) != 2 {
		t.Fatalf("expected 2 deduped entries, got %d: %#v", len(steps), steps)
	}
	// "aaa" moved to tail (last wins); "bbb" first.
	if steps[0]["text"] != "bbb" || steps[1]["text"] != "aaa" {
		t.Errorf("dedupe order wrong: %#v", steps)
	}
}

// T4 — --slug stored verbatim; absent → no slug key.
func TestCheckpointNextStep_Slug(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := runCheckpoint(t, "next-step", "--project", "myproj", "--slug", "foo-1234", "finish foo"); err != nil {
		t.Fatalf("with slug: %v", err)
	}
	if err := runCheckpoint(t, "next-step", "--project", "myproj", "no slug here"); err != nil {
		t.Fatalf("no slug: %v", err)
	}
	steps := sessionNextStepsOf(t, readCoordState(t, home, "myproj"))
	if len(steps) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(steps))
	}
	if steps[0]["slug"] != "foo-1234" {
		t.Errorf("slug not stored: %#v", steps[0])
	}
	if _, ok := steps[1]["slug"]; ok {
		t.Errorf("absent --slug must not set slug key: %#v", steps[1])
	}
}

// Empty / whitespace-only text is rejected (no write).
func TestCheckpointNextStep_RejectsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := runCheckpoint(t, "next-step", "--project", "myproj", "   "); err == nil {
		t.Fatalf("expected error on blank next-step text")
	}
}

// T5 — RMW preserves sibling keys (session_docs untouched by a next-step write).
func TestCheckpointNextStep_PreservesSiblings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	seedCoordStateFile(t, home, "myproj",
		`{"session_docs":[{"path":"docs/D.md","role":"authored","ts":"t"}],"recent_decisions":["why"]}`)
	if err := runCheckpoint(t, "next-step", "--project", "myproj", "next thing"); err != nil {
		t.Fatalf("next-step: %v", err)
	}
	m := readCoordState(t, home, "myproj")
	if docs, ok := m["session_docs"].([]any); !ok || len(docs) != 1 {
		t.Errorf("session_docs clobbered: %#v", m["session_docs"])
	}
	if dec, ok := m["recent_decisions"].([]any); !ok || len(dec) != 1 {
		t.Errorf("recent_decisions clobbered: %#v", m["recent_decisions"])
	}
}

// T6 — concurrent next-step writers serialize on coordinator.lock: every
// distinct entry survives (no lost update).
func TestCheckpointNextStep_ConcurrentSerialize(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLEET_HOME", home)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	seedCoordStateFile(t, home, "myproj", `{"worker_agent_ids":{"keep":"me01"}}`)

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = runCheckpoint(t, "next-step", "--project", "myproj",
				fmt.Sprintf("step-%d", i))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	m := readCoordState(t, home, "myproj")
	steps := sessionNextStepsOf(t, m)
	seen := map[string]bool{}
	for _, s := range steps {
		seen[s["text"].(string)] = true
	}
	for i := 0; i < writers; i++ { // writers=8 < cap 10 so all survive
		if !seen[fmt.Sprintf("step-%d", i)] {
			t.Errorf("lost step-%d under concurrency: %#v", i, steps)
		}
	}
	if _, ok := m["worker_agent_ids"].(map[string]any); !ok {
		t.Errorf("sibling worker_agent_ids lost: %#v", m["worker_agent_ids"])
	}
}
