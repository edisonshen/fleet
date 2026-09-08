package enginecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edisonshen/fleet/internal/spawn"
)

func TestResolveDefaults(t *testing.T) {
	tests := []struct {
		in      string
		wantBin string
		wantCmd string
	}{
		{"", "claude", "claude --dangerously-skip-permissions"},
		{EngineClaudeCode, "claude", "claude --dangerously-skip-permissions"},
		{EngineCodex, "codex", "codex --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust"},
	}
	for _, tc := range tests {
		inv, err := Resolve(tc.in)
		if err != nil {
			t.Fatalf("Resolve(%q) returned err: %v", tc.in, err)
		}
		if inv.Binary != tc.wantBin {
			t.Errorf("Resolve(%q).Binary = %q want %q",
				tc.in, inv.Binary, tc.wantBin)
		}
		if inv.Cmd != tc.wantCmd {
			t.Errorf("Resolve(%q).Cmd = %q want %q",
				tc.in, inv.Cmd, tc.wantCmd)
		}
	}
}

func TestResolveUnknownEngine(t *testing.T) {
	_, err := Resolve("does-not-exist")
	if err == nil {
		t.Fatal("Resolve(\"does-not-exist\") returned nil err")
	}
	if !strings.Contains(err.Error(), "unknown engine") {
		t.Errorf("err = %v, want it to mention 'unknown engine'", err)
	}
}

// TestClaudeWrapperByteEqualsLegacy pins the rewritten claude-code
// wrapper byte-for-byte against the historical spawn.
// DefaultClaudeWrapperScript constant. Tests across the repo that pin
// dispatch's --command default to that constant (e.g.,
// cmd/fleet/dispatch_test.go:TestDefaultClaudeWrapperScript_MatchesFlagDefault)
// continue to pass because the dispatch path's claude-code branch
// produces the identical byte sequence as before.
func TestClaudeWrapperByteEqualsLegacy(t *testing.T) {
	argv, err := BuildWrapperCommand(EngineClaudeCode)
	if err != nil {
		t.Fatalf("BuildWrapperCommand(claude-code) err = %v", err)
	}
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv shape = %v, want [sh -c <body>]", argv)
	}
	if argv[2] != spawn.DefaultClaudeWrapperScript {
		t.Fatalf("claude-code wrapper body drifted from "+
			"spawn.DefaultClaudeWrapperScript:\n  got:  %q\n  want: %q",
			argv[2], spawn.DefaultClaudeWrapperScript)
	}
}

func TestCodexWrapperShape(t *testing.T) {
	argv, err := BuildWrapperCommand(EngineCodex)
	if err != nil {
		t.Fatalf("BuildWrapperCommand(codex) err = %v", err)
	}
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("argv shape = %v, want [sh -c <body>]", argv)
	}
	body := argv[2]
	// Must start with the bare invocation so the survive-clean-exit
	// shell wrapper executes codex first.
	if !strings.HasPrefix(body, "codex --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust;") {
		t.Errorf("codex wrapper body should start with the unattended codex invocation: %q", body)
	}
	// Banner uses the bare binary name.
	if !strings.Contains(body, `[fleet] codex exited code`) {
		t.Errorf("codex wrapper missing exit banner: %q", body)
	}
	if !strings.Contains(body, `[fleet] codex exited cleanly`) {
		t.Errorf("codex wrapper missing clean-exit banner: %q", body)
	}
	// Rerun hint reproduces the actual invocation.
	if !strings.Contains(body, `rerun codex --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust or Ctrl-b`) {
		t.Errorf("codex wrapper missing rerun hint: %q", body)
	}
	// Survive-clean-exit branch present.
	if !strings.Contains(body, `exec ${SHELL:-bash} -i`) {
		t.Errorf("codex wrapper missing exec ${SHELL} fallback: %q", body)
	}
}

func TestKnown(t *testing.T) {
	for _, name := range []string{EngineClaudeCode, EngineCodex} {
		if !Known(name) {
			t.Errorf("Known(%q) = false, want true", name)
		}
	}
	if Known("nope") {
		t.Errorf("Known(\"nope\") = true, want false")
	}
}

func TestHelper(t *testing.T) {
	tests := map[string]string{
		"":               EngineCodex,
		EngineClaudeCode: EngineCodex,
		EngineCodex:      EngineClaudeCode,
		"nope":           "",
	}
	for in, want := range tests {
		if got := Helper(in); got != want {
			t.Errorf("Helper(%q) = %q want %q", in, got, want)
		}
	}
}

func TestInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if Installed(EngineCodex) || Installed(EngineClaudeCode) {
		t.Fatal("nothing on PATH, yet Installed reported true")
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !Installed(EngineCodex) {
		t.Error("Installed(codex) = false with codex on PATH")
	}
	if Installed(EngineClaudeCode) {
		t.Error("Installed(claude-code) = true with only codex on PATH")
	}
	if Installed("nope") {
		t.Error("Installed(unknown) must be false")
	}
}

func TestSkillHomeAndHooksFile(t *testing.T) {
	home := "/h"
	cases := []struct {
		engine, wantSkill, wantHooks string
	}{
		{"", "/h/.claude", "/h/.claude/settings.json"},
		{EngineClaudeCode, "/h/.claude", "/h/.claude/settings.json"},
		{EngineCodex, "/h/.agents", "/h/.codex/hooks.json"},
	}
	for _, tc := range cases {
		if got := SkillHome(home, tc.engine); got != filepath.FromSlash(tc.wantSkill) {
			t.Errorf("SkillHome(%q) = %q want %q", tc.engine, got, tc.wantSkill)
		}
		if got := HooksFile(home, tc.engine); got != filepath.FromSlash(tc.wantHooks) {
			t.Errorf("HooksFile(%q) = %q want %q", tc.engine, got, tc.wantHooks)
		}
	}
}
