package spawn

import (
	"strings"
	"testing"
)

// TestInjectRemoteControlFlag_ShellWrapper pins the relaxed
// wrapper-pattern matcher (handoff-remote-control-shell-wrapper-fix):
// ANY `["sh", "-c", "<body>"]` whose body begins with `claude ` (after
// optional leading whitespace) must be rewritten to inject
// `--remote-control "<session>"` immediately after the `claude ` token,
// preserving the rest of the body byte-for-byte.
//
// Previously the helper required byte-equality with the literal
// DefaultClaudeWrapperScript constant — so the slightest drift in the
// persisted Command body (older fleet release wrapper text, manual
// edit, future wrapper variant) silently disabled remote-control
// injection on handoff replacements. The forensic case: coord ca7eb43e
// lost remote control after a manual handoff because the persisted
// command body diverged from the literal default by a single character.
func TestInjectRemoteControlFlag_ShellWrapper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		// wantSubstring is the literal string the rewritten body[2] must
		// contain. The rest of the body must be preserved unchanged
		// outside the inserted flag.
		wantSubstring string
		// preserved is a slice of substrings from the original body that
		// must survive in the rewritten body verbatim.
		preserved []string
	}{
		{
			name: "default-wrapper-still-rewritten",
			in: []string{"sh", "-c",
				`claude --dangerously-skip-permissions; RC=$?; exit $RC`},
			wantSubstring: `claude --remote-control "fleet-coord-abcd1234" --dangerously-skip-permissions`,
			preserved:     []string{"RC=$?", "exit $RC"},
		},
		{
			name: "wrapper-with-extra-flags",
			in: []string{"sh", "-c",
				`claude --print --dangerously-skip-permissions --custom-flag; echo done`},
			wantSubstring: `claude --remote-control "fleet-coord-abcd1234" --print`,
			preserved:     []string{"--custom-flag", "echo done"},
		},
		{
			name: "wrapper-with-leading-whitespace",
			in: []string{"sh", "-c",
				`  claude --dangerously-skip-permissions`},
			wantSubstring: `claude --remote-control "fleet-coord-abcd1234" --dangerously-skip-permissions`,
		},
		{
			name: "wrapper-with-leading-tab",
			in: []string{"sh", "-c",
				"\tclaude --dangerously-skip-permissions"},
			wantSubstring: `claude --remote-control "fleet-coord-abcd1234" --dangerously-skip-permissions`,
		},
		{
			name: "fleet-default-wrapper-script-still-matches",
			in:   []string{"sh", "-c", DefaultClaudeWrapperScript},
			// DefaultClaudeWrapperScript starts with the legacy literal
			// "claude --dangerously-skip-permissions". The relaxed matcher
			// must still rewrite it (regression: don't break the dispatch
			// path that already works).
			wantSubstring: `claude --remote-control "fleet-coord-abcd1234" --dangerously-skip-permissions`,
			preserved:     []string{"RC=$?", "exec ${SHELL:-bash}"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InjectRemoteControlFlag(tc.in, "fleet-coord-abcd1234")
			if len(got) != 3 {
				t.Fatalf("rewritten command should keep [sh -c <body>] shape; got len=%d", len(got))
			}
			if got[0] != "sh" || got[1] != "-c" {
				t.Errorf("rewritten command should keep sh -c prefix; got %v", got[:2])
			}
			if !strings.Contains(got[2], tc.wantSubstring) {
				t.Errorf("rewritten body should contain %q; got %q",
					tc.wantSubstring, got[2])
			}
			for _, p := range tc.preserved {
				if !strings.Contains(got[2], p) {
					t.Errorf("rewritten body should preserve substring %q; got %q",
						p, got[2])
				}
			}
		})
	}
}

// TestInjectRemoteControlFlag_NonClaudeShellWrapper pins the negative
// contract: shell wrappers whose body does NOT begin with `claude `
// (after optional whitespace) must be returned unchanged. Operator may
// have wrapped a wholly different binary inside `sh -c`; silently
// injecting --remote-control would corrupt that argv.
func TestInjectRemoteControlFlag_NonClaudeShellWrapper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
	}{
		{
			name: "python-script",
			argv: []string{"sh", "-c", `python3 myscript.py`},
		},
		{
			name: "echo-then-claude",
			argv: []string{"sh", "-c", `echo "starting"; claude --dangerously-skip-permissions`},
		},
		{
			name: "exec-codex",
			argv: []string{"sh", "-c", `exec codex --interactive`},
		},
		{
			name: "claude-prefix-but-different-binary",
			// `claudefoo` starts with `claude` but is NOT the `claude `
			// binary — the trailing space in the matcher prevents this
			// false positive.
			argv: []string{"sh", "-c", `claudefoo --dangerously-skip-permissions`},
		},
		{
			name: "bash-c-not-sh-c",
			// argv[0] is `bash` not `sh`; brief: only `sh -c` matches.
			argv: []string{"bash", "-c", `claude --dangerously-skip-permissions`},
		},
		{
			name: "wrong-flag-position",
			argv: []string{"sh", "-X", `claude --dangerously-skip-permissions`},
		},
		{
			name: "two-element-argv",
			argv: []string{"sh", "-c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InjectRemoteControlFlag(tc.argv, "fleet-coord-deadbeef")
			if len(got) != len(tc.argv) {
				t.Fatalf("non-matching command should be returned unchanged (len differs); got %v", got)
			}
			for i := range tc.argv {
				if got[i] != tc.argv[i] {
					t.Errorf("non-matching element %d mutated: %q → %q",
						i, tc.argv[i], got[i])
				}
			}
		})
	}
}

// TestInjectRemoteControlFlag_DirectClaudeArgv documents that the
// helper's contract scope is the `sh -c` shell wrapper. A direct argv
// like `["claude", "--flag", ...]` is NOT in scope and is returned
// unchanged. (Coord-spawn always uses the shell wrapper; if a future
// engine adapter drops the wrapper, that adapter must own its own
// flag-injection — the helper stays narrow.)
func TestInjectRemoteControlFlag_DirectClaudeArgv(t *testing.T) {
	t.Parallel()
	in := []string{"claude", "--print", "do something"}
	got := InjectRemoteControlFlag(in, "fleet-coord-deadbeef")
	if len(got) != len(in) {
		t.Fatalf("direct claude argv should be returned unchanged; got %v", got)
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("direct claude argv element %d mutated: %q → %q",
				i, in[i], got[i])
		}
	}
}

// TestInjectRemoteControlFlag_PositionAfterClaudeToken pins the
// rewrite POSITION: the flag goes IMMEDIATELY after the `claude ` token,
// not after the trailing flags. This matters because some operator
// wrappers may chain claude with other claude-arg-like tokens; the
// relaxed matcher must still place --remote-control adjacent to the
// claude binary so claude's own flag parser sees it.
func TestInjectRemoteControlFlag_PositionAfterClaudeToken(t *testing.T) {
	t.Parallel()
	in := []string{"sh", "-c", `claude --print --resume`}
	got := InjectRemoteControlFlag(in, "fleet-coord-position")
	if len(got) != 3 {
		t.Fatalf("expected [sh -c <body>]; got %v", got)
	}
	// The rewritten body must place --remote-control before --print, so
	// claude reads it as its own flag (not as a positional after --print).
	wantPrefix := `claude --remote-control "fleet-coord-position" --print --resume`
	body := strings.TrimSpace(got[2])
	if body != wantPrefix {
		t.Errorf("flag position wrong:\n got: %q\nwant: %q", body, wantPrefix)
	}
}

// TestInjectRemoteControlFlag_PreservesInputSlice pins the defensive
// invariant that the helper does not mutate the caller's slice (the
// dispatch path passes opts.command which originated from cobra's flag
// parser; mutation would corrupt later reads).
func TestInjectRemoteControlFlag_PreservesInputSlice(t *testing.T) {
	t.Parallel()
	in := []string{"sh", "-c", `claude --dangerously-skip-permissions`}
	original := make([]string, len(in))
	copy(original, in)
	originalBody := in[2]

	_ = InjectRemoteControlFlag(in, "fleet-coord-abcd1234")

	if len(in) != len(original) {
		t.Fatalf("input slice length mutated: %d → %d", len(original), len(in))
	}
	for i := range in {
		if in[i] != original[i] {
			t.Errorf("input element %d mutated: %q → %q", i, original[i], in[i])
		}
	}
	if in[2] != originalBody {
		t.Errorf("input body mutated: %q → %q", originalBody, in[2])
	}
}
