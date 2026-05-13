package main

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/enginecfg"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestDispatch_EngineFlag_Exposed pins that `fleet dispatch --engine ...`
// is a valid invocation — the flag must be visible on the assembled
// dispatch subcommand WHEN reached through the root cmd (cobra walks
// parent persistent flags). The TUI (internal/tui/keys.go:startCoordSpawn)
// and the operator both rely on this surface.
//
// Why root assembly (not bare newDispatchCmd): the --engine flag is
// registered ONLY on the root command's PersistentFlags(). Registering
// it locally on dispatch would shadow the root version (codex review
// iter-1 P1) and silently bypass root's resolveEngineFlags conflict
// detection (--engine vs -codex/-claude). The pin therefore goes
// through the assembled root.
func TestDispatch_EngineFlag_Exposed(t *testing.T) {
	root := newRootCmd()
	var dispatchCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "dispatch" {
			dispatchCmd = c
			break
		}
	}
	if dispatchCmd == nil {
		t.Fatal("root must expose the dispatch subcommand")
	}
	flag := dispatchCmd.Flag("engine")
	if flag == nil {
		t.Fatal("dispatch must inherit --engine from root for the multi-engine wiring")
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

// TestDispatch_NoLocalEngineFlagShadow regresses codex review iter-1
// [P1]: a `--engine` flag registered locally on the dispatch
// subcommand shadowed root's persistent flag and silently bypassed
// resolveEngineFlags' conflict detection (--engine vs -codex/-claude).
// After the fix the dispatch subcommand exposes --engine ONLY via
// inheritance from root, so a value passed on dispatch is funneled
// through root's PersistentPreRunE → FLEET_ENGINE → runDispatch.
//
// We exercise the assembled root cmd with a real argv and assert:
//  1. Conflicting --engine codex --claude returns a "conflicting flags"
//     error from resolveEngineFlags (would silently no-op before).
//  2. The local dispatch flag set contains NO `engine` flag entry —
//     only the inherited persistent one. Cobra's PersistentFlags() on
//     the assembled subcommand surfaces the inherited flag, while
//     LocalFlags() does NOT.
func TestDispatch_NoLocalEngineFlagShadow(t *testing.T) {
	root := newRootCmd()
	var dispatchCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "dispatch" {
			dispatchCmd = c
			break
		}
	}
	if dispatchCmd == nil {
		t.Fatal("root must expose dispatch")
	}
	// Local flag set must NOT have --engine; only inheritance from root.
	local := dispatchCmd.LocalFlags().Lookup("engine")
	if local != nil {
		t.Errorf(
			"dispatch must NOT register a local --engine flag (would shadow root); got DefValue=%q",
			local.DefValue,
		)
	}
	inherited := dispatchCmd.InheritedFlags().Lookup("engine")
	if inherited == nil {
		t.Error("dispatch must inherit --engine from root's PersistentFlags")
	}
}

// TestRootEngineConflict_PassesThroughDispatch regresses codex review
// iter-1 [P1]: `fleet --engine codex --claude dispatch t` must error
// at root's PersistentPreRunE (resolveEngineFlags conflict). Before
// the fix the local --engine flag captured codex and the root saw
// "" for rootEngine, so resolveEngineFlags("", false, true, "")
// returned claude-code without complaint — the disagreement landed
// silently in env vs record.
func TestRootEngineConflict_PassesThroughDispatch(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--engine", "codex", "--claude", "dispatch", "t1", "--project", "p1"})
	// Suppress output to keep test logs clean; we only care about err.
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected conflict error for --engine codex --claude")
	}
	if !strings.Contains(err.Error(), "conflicting flags") {
		t.Errorf("err = %v, want 'conflicting flags' phrase", err)
	}
}

// TestDispatch_FleetEngineEnvStamped regresses the second half of
// codex review iter-1 [P1]: runDispatch must re-stamp FLEET_ENGINE
// with the resolved engine BEFORE spawn so the spawned tmux process
// inherits a consistent value matching the agent record. Previous
// code wrote opts.engine into the record but left FLEET_ENGINE stale,
// so the spawned coord could read the wrong engine from env.
func TestDispatch_FleetEngineEnvStamped(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Start with env unset; opts.engine carries the intent.
	t.Setenv("FLEET_ENGINE", "")
	opts := &dispatchOpts{
		taskID:  "t1",
		project: "p1",
		engine:  "codex",
	}
	var out bytes.Buffer
	// runDispatch will fail later (no tmux in CI), but only AFTER it
	// re-stamps FLEET_ENGINE. We re-read the env after the call.
	_ = runDispatch(opts, &out)
	if got := os.Getenv("FLEET_ENGINE"); got != "codex" {
		t.Errorf("FLEET_ENGINE = %q after dispatch; want %q (re-stamp regression)",
			got, "codex")
	}
}

// TestDispatch_ProgrammaticCommandNotOverriddenByEngine regresses a CI
// bug where the engine-wrapper override in runDispatch stomped a
// programmatically-injected `opts.command`. The original guard checked
// only `opts.commandExplicit` — a flag set ONLY inside RunE via cobra
// Changed(). Tests (and any other in-process caller) that bypass cobra
// to call runDispatch directly with a populated `command` had it
// silently replaced with the engine wrapper argv (`exec claude` in the
// claude wrapper, `exec codex` in the codex wrapper). On CI the
// resolved wrapper exec'd a binary not on PATH and the tmux session
// crashed at startup, manifesting as "expected 1 live agent, got 0"
// in 9+ handoff/drain/rm tests. The fix: also honor `len(opts.command)
// > 0` as caller intent — both signals mean "the caller picked a
// command, don't stomp it."
//
// This test asserts the persisted agent.Record.Command equals the
// programmatic argv even when FLEET_ENGINE=codex would otherwise
// route through BuildWrapperCommand("codex").
func TestDispatch_ProgrammaticCommandNotOverriddenByEngine(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping integration test")
	}
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	if _, err := state.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	// Isolate the tmux socket so concurrent test runs don't collide.
	var b [3]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-test-"+hex.EncodeToString(b[:])+".sock")
	t.Setenv("FLEET_ENGINE", "codex")

	opts := &dispatchOpts{
		taskID:  "t1",
		project: "p1",
		// commandExplicit deliberately left false (bypasses cobra,
		// matches the seedAgent test helper pattern).
		command: []string{"sleep", "60"},
	}
	var out bytes.Buffer
	if err := runDispatch(opts, &out); err != nil {
		t.Fatalf("runDispatch: %v\n%s", err, out.String())
	}
	live, err := agent.List()
	if err != nil {
		t.Fatalf("agent.List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 live agent, got %d", len(live))
	}
	got := live[0].Command
	want := []string{"sleep", "60"}
	if len(got) != len(want) {
		t.Fatalf("record.Command = %v, want %v (engine wrapper stomped programmatic command)",
			got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("record.Command[%d] = %q, want %q (engine wrapper stomped programmatic command: full=%v)",
				i, got[i], want[i], got)
		}
	}
	// Clean up the live tmux session so the test doesn't leak.
	_ = tmux.Kill(live[0].TmuxSession)
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
		// codex review iter-1 P2: `-codex` is data for --command, must
		// pass through unchanged so a custom wrapper sees the same
		// argument the operator typed. Multiple --command pairs each
		// guard their own immediate next-token.
		{
			in:   []string{"dispatch", "t", "--command", "sh", "--command", "-c", "--command", "-codex"},
			want: []string{"dispatch", "t", "--command", "sh", "--command", "-c", "--command", "-codex"},
		},
		// codex review iter-1 P2: POSIX `--` end-of-options marker
		// halts the rewrite for everything after it.
		{
			in:   []string{"dispatch", "t", "--", "-codex", "-claude"},
			want: []string{"dispatch", "t", "--", "-codex", "-claude"},
		},
		// Fleet-level -codex before `--` still rewrites; child data
		// after `--` does not.
		{
			in:   []string{"-codex", "dispatch", "t", "--", "-claude"},
			want: []string{"--codex", "dispatch", "t", "--", "-claude"},
		},
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
