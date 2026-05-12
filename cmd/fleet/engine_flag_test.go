package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/enginecfg"
)

// TestDispatch_EngineFlag_Exposed pins the --engine flag on the
// dispatch subcommand so a future refactor doesn't drop it. The TUI
// (internal/tui/keys.go:startCoordSpawn) and the operator both rely
// on this flag.
func TestDispatch_EngineFlag_Exposed(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("engine")
	if flag == nil {
		t.Fatal("dispatch must expose --engine for the multi-engine wiring")
	}
	if flag.DefValue != "" {
		t.Errorf("--engine default = %q; want empty (inherits FLEET_ENGINE)",
			flag.DefValue)
	}
}

// TestDispatch_UnknownEngineRejected pins fail-fast behavior: a typo at
// `fleet dispatch --engine nopeengine` errors out before we touch tmux
// or write any record, so the operator gets an actionable error rather
// than a half-spawned agent with engine="nopeengine".
func TestDispatch_UnknownEngineRejected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	opts := &dispatchOpts{
		taskID:  "t1",
		project: "p1",
		engine:  "nopeengine",
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatal("dispatch must reject unknown engine")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("err should mention 'unknown'; got %q", err.Error())
	}
}

// TestDispatch_EngineFromEnv pins the env-fallback path: when
// --engine is empty on the CLI but FLEET_ENGINE is set in the
// environment (as the root cmd's PersistentPreRunE sets it), the
// dispatch path picks up the env value. Verified by checking that
// runDispatch does NOT error with "unknown engine" when the env
// holds a valid name — we can't easily inspect the agent record
// without tmux, so this test asserts the gate doesn't reject.
func TestDispatch_EngineFromEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	t.Setenv("FLEET_ENGINE", "codex")
	opts := &dispatchOpts{
		taskID:  "t1",
		project: "p1",
		// opts.engine left empty → reads env.
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err != nil && strings.Contains(err.Error(), "unknown engine") {
		t.Errorf("env FLEET_ENGINE=codex must be accepted; got %q",
			err.Error())
	}
	// runDispatch will fail at tmux.Available() or spawn.Spawn in CI
	// without a real tmux server — that's fine; we only assert the
	// engine-validation branch didn't fire.
}

func TestResolveEngineFlags_DefaultWhenAllBlank(t *testing.T) {
	got, err := resolveEngineFlags("", false, false, "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != enginecfg.DefaultEngine {
		t.Errorf("got %q, want %q (default)", got, enginecfg.DefaultEngine)
	}
}

func TestResolveEngineFlags_CodexShorthand(t *testing.T) {
	got, err := resolveEngineFlags("", true, false, "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != enginecfg.EngineCodex {
		t.Errorf("got %q, want %q", got, enginecfg.EngineCodex)
	}
}

func TestResolveEngineFlags_ClaudeShorthand(t *testing.T) {
	got, err := resolveEngineFlags("", false, true, "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != enginecfg.EngineClaudeCode {
		t.Errorf("got %q, want %q", got, enginecfg.EngineClaudeCode)
	}
}

func TestResolveEngineFlags_LongFormEngine(t *testing.T) {
	got, err := resolveEngineFlags("codex", false, false, "")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != enginecfg.EngineCodex {
		t.Errorf("got %q, want %q", got, enginecfg.EngineCodex)
	}
}

func TestResolveEngineFlags_EnvFallback(t *testing.T) {
	got, err := resolveEngineFlags("", false, false, "codex")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != enginecfg.EngineCodex {
		t.Errorf("got %q, want %q (env fallback)",
			got, enginecfg.EngineCodex)
	}
}

func TestResolveEngineFlags_ShorthandWinsOverEnv(t *testing.T) {
	// FLEET_ENGINE=codex but `-claude` explicitly passed → claude wins.
	got, err := resolveEngineFlags("", false, true, "codex")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != enginecfg.EngineClaudeCode {
		t.Errorf("got %q, want %q (shorthand wins over env)",
			got, enginecfg.EngineClaudeCode)
	}
}

func TestResolveEngineFlags_BothShorthandsConflict(t *testing.T) {
	_, err := resolveEngineFlags("", true, true, "")
	if err == nil {
		t.Fatal("expected error for -codex AND -claude")
	}
	if !strings.Contains(err.Error(), "conflicting flags") {
		t.Errorf("err = %v, want 'conflicting flags' phrase", err)
	}
}

func TestResolveEngineFlags_LongFormVsConflictingShorthand(t *testing.T) {
	_, err := resolveEngineFlags("codex", false, true, "")
	if err == nil {
		t.Fatal("expected error for --engine codex vs -claude")
	}
	if !strings.Contains(err.Error(), "conflicting flags") {
		t.Errorf("err = %v, want 'conflicting flags' phrase", err)
	}
}

func TestResolveEngineFlags_UnknownEngineRejected(t *testing.T) {
	_, err := resolveEngineFlags("nopecorp", false, false, "")
	if err == nil {
		t.Fatal("expected error for unknown engine")
	}
	if !strings.Contains(err.Error(), "unknown engine") {
		t.Errorf("err = %v, want 'unknown engine' phrase", err)
	}
}

func TestRewriteEngineShorthand(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{"-codex"}, []string{"--codex"}},
		{[]string{"-claude"}, []string{"--claude"}},
		{[]string{"-codex", "dispatch", "t1"}, []string{"--codex", "dispatch", "t1"}},
		{[]string{"dispatch", "t1", "-codex"}, []string{"dispatch", "t1", "--codex"}},
		// already long form — unchanged.
		{[]string{"--codex", "dispatch"}, []string{"--codex", "dispatch"}},
		// unrelated short flag — unchanged.
		{[]string{"-c", "foo"}, []string{"-c", "foo"}},
		// looks-similar but isn't — `--codex-other` shouldn't be touched.
		{[]string{"--codex-other"}, []string{"--codex-other"}},
		// empty argv.
		{[]string{}, []string{}},
	}
	for _, tc := range tests {
		got := rewriteEngineShorthand(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("rewriteEngineShorthand(%v) = %v, want %v",
				tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("rewriteEngineShorthand(%v)[%d] = %q, want %q",
					tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
