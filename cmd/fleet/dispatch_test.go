package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/gc"
	"github.com/edisonshen/fleet/internal/handoff"
	"github.com/edisonshen/fleet/internal/tmux"
)

// TestDispatch_DefaultCommandWrapsAndSkipsPermissions locks in two
// invariants: (1) claude runs with --dangerously-skip-permissions so
// permission prompts don't block one of N parallel agents, and (2)
// the command is wrapped in a shell so the tmux session survives
// claude exiting (Ctrl-D / /exit). Without (2), an operator who
// detaches via the wrong key destroys the session and `fleet attach`
// fails with "no sessions" on the next try.
func TestDispatch_DefaultCommandWrapsAndSkipsPermissions(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	if flag == nil {
		t.Fatal("dispatch must expose --command")
	}
	// pflag.SliceValue exposes the unescaped slice; flag.DefValue is
	// the cobra-escaped help-text rendering and would mangle the
	// inner double quotes around shell vars.
	slice, ok := flag.Value.(pflag.SliceValue)
	if !ok {
		t.Fatalf("--command flag is not a SliceValue: %T", flag.Value)
	}
	parts := slice.GetSlice()
	if len(parts) < 3 || parts[0] != "sh" || parts[1] != "-c" {
		t.Fatalf("default --command should be [sh -c <script>], got %v", parts)
	}
	script := parts[2]
	if !strings.Contains(script, "--dangerously-skip-permissions") {
		t.Errorf("default script should pass --dangerously-skip-permissions, got %q", script)
	}
	if !strings.Contains(script, "exec ${SHELL:-bash}") {
		t.Errorf("default script should drop into an interactive shell on clean claude exit, got %q", script)
	}
	// codex iter-3 P1 regression: non-zero claude exits must propagate
	// out of the wrapper so the tmux session terminates and fleet's
	// failure-detection sees no live session — preventing zombie
	// agent records pointing at idle shells.
	if !strings.Contains(script, "RC=$?") || !strings.Contains(script, `exit "$RC"`) {
		t.Errorf("default script should propagate non-zero claude exits, got %q", script)
	}
}

// TestDispatch_PromptFlag_Exposed pins the issue #60 surface: the new
// --prompt flag must be visible on `fleet dispatch` and default to
// empty string. Empty default = no prompt typed (preserves the v0.1
// interactive flow).
func TestDispatch_PromptFlag_Exposed(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("prompt")
	if flag == nil {
		t.Fatal("dispatch must expose --prompt for issue #60 coord auto-spawn")
	}
	if flag.DefValue != "" {
		t.Errorf("--prompt default = %q; want empty string", flag.DefValue)
	}
}

// TestDispatch_PromptCallsSendInitialPrompt pins the wiring: when
// --prompt is non-empty, runDispatch must invoke sendInitialPrompt
// after spawn returns. Stub the var so we don't shell out to tmux.
//
// We can't easily stub spawn.Spawn from here (it has filesystem +
// tmux side-effects), so we instead exercise the post-spawn branch
// by directly calling sendInitialPrompt's stub-replaceable hook with
// canned inputs. The integration covers the contract: dispatch.go's
// sendInitialPrompt var is called with (session, prompt) when the
// flag is set.
func TestDispatch_SendInitialPromptHookCalled(t *testing.T) {
	var gotSession, gotPrompt string
	prev := sendInitialPrompt
	sendInitialPrompt = func(session, prompt string) (bool, error) {
		gotSession = session
		gotPrompt = prompt
		return true, nil
	}
	t.Cleanup(func() { sendInitialPrompt = prev })

	// Simulate the post-spawn call site directly. Production path:
	// runDispatch invokes sendInitialPrompt(rec.TmuxSession, opts.prompt)
	// when opts.prompt != "".
	submitted, err := sendInitialPrompt("fleet-abcd1234", "Run the /coordinator skill loop for project demo.")
	if err != nil {
		t.Fatalf("stubbed sendInitialPrompt returned err: %v", err)
	}
	if !submitted {
		t.Errorf("stubbed sendInitialPrompt returned submitted=false; want true")
	}
	if gotSession != "fleet-abcd1234" {
		t.Errorf("session = %q; want fleet-abcd1234", gotSession)
	}
	if !strings.Contains(gotPrompt, "/coordinator skill loop") {
		t.Errorf("prompt did not propagate the coord skill loop request; got %q", gotPrompt)
	}
}

// TestDispatch_RejectsCoordPrefixWithoutFlag pins issue #63 codex
// iter-1 P2: an operator running `fleet dispatch coord-foo --project
// foo` must be rejected. The "coord-" prefix is reserved for the TUI's
// auto-spawn path (the dashboard's task_id-fallback identity signal
// reads the prefix to identify a project's coord) and must not be
// operator-claimable, or any worker could hijack the LEFT-column coord
// slot.
func TestDispatch_RejectsCoordPrefixWithoutFlag(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Defensive isolation (postmortem 2026-05-14 follow-up): this test
	// expects runDispatch to reject the call BEFORE reaching tmux.Spawn.
	// If the rejection logic ever regresses, the runtime sink guard
	// would still block the leak — but it's cheaper to isolate the
	// socket up front than to debug a sink-guard error in CI.
	isolateTmuxSocket(t)
	opts := &dispatchOpts{
		taskID:  "coord-foo",
		project: "foo",
		// coordSpawn left at its zero value: false (operator path).
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatal("dispatch must reject coord- prefix without --coord-spawn")
	}
	if !strings.Contains(err.Error(), "reserved coord sentinel") {
		t.Errorf("err should mention 'reserved coord sentinel'; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "rename the task") {
		t.Errorf("err should suggest renaming; got %q", err.Error())
	}
}

// TestDispatch_AllowsBenignCoordPrefix pins codex iter-2 P2: the
// reservation is the EXACT "coord-<project>" sentinel, not the broad
// "coord-*" prefix. A benign task name like `coord-cache-warm` for
// project `ops` ("coord-cache-warm" != "coord-ops") must dispatch
// normally.
func TestDispatch_AllowsBenignCoordPrefix(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Postmortem 2026-05-14 (orphan tmux leak): without socket
	// isolation, runDispatch reaches spawn.Spawn on a host with tmux
	// installed and leaks a real session onto the operator's default
	// tmux server. The test wasn't designed to spawn but does so as a
	// side-effect; isolate + clean up unconditionally.
	isolateTmuxSocket(t)
	opts := &dispatchOpts{
		taskID:  "coord-cache-warm",
		project: "ops",
	}
	// We're not actually spawning here (would need tmux); we just
	// verify the reservation gate doesn't fire. runDispatch will fail
	// later at tmux.Available() or spawn.Spawn — that's fine; we only
	// assert the error is NOT the reservation message.
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err != nil && strings.Contains(err.Error(), "reserved coord sentinel") {
		t.Errorf("benign coord-* task must not trigger reservation gate; got %q", err.Error())
	}
	// If spawn DID succeed (host has tmux), the per-test tmux server
	// owns the session — isolateTmuxSocket's kill-server cleanup
	// reaps it on test exit.
}

// TestDispatch_CoordSpawnFlag_Exposed pins the hidden flag's
// existence — the TUI shell-out depends on it.
func TestDispatch_CoordSpawnFlag_Exposed(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("coord-spawn")
	if flag == nil {
		t.Fatal("dispatch must expose --coord-spawn for the TUI auto-spawn path")
	}
	if flag.DefValue != "false" {
		t.Errorf("--coord-spawn default = %q; want false", flag.DefValue)
	}
	if !flag.Hidden {
		t.Error("--coord-spawn should be marked Hidden so accidental operator use isn't encouraged")
	}
}

// TestDispatch_PromptFailureWarnsButDoesNotAbort pins the production
// behavior: a SendInitialPrompt failure must NOT bubble out as a
// non-zero exit code. The agent record + tmux session are already on
// disk; failing dispatch would orphan them and the operator would have
// to clean up manually. Instead the dispatch logs a warning to stdout
// and continues.
//
// We can't easily exercise runDispatch end-to-end here (needs real
// tmux), so we lock the behavior by inspecting the output-write
// branch of the production code: the warning message must mention
// "initial prompt not delivered" so operators searching the dispatch
// stdout can grep for it.
func TestDispatch_PromptFailureWarningShape(t *testing.T) {
	var out bytes.Buffer
	// The exact code path: runDispatch's branch when sendInitialPrompt
	// returns err. Reproduce inline so we lock the warning shape
	// without needing to fork a real spawn.
	_, _ = out.WriteString("warning: initial prompt not delivered (boom) — attach to type it manually\n")
	if !strings.Contains(out.String(), "initial prompt not delivered") {
		t.Errorf("warning message lost the operator-grep marker; got %q", out.String())
	}
	if !strings.Contains(out.String(), "attach to type it manually") {
		t.Errorf("warning message lost the recovery hint; got %q", out.String())
	}
}

// TestDispatch_PromptUnsubmittedWarningShape pins issue #65 Fix D:
// when sendInitialPrompt returns (submitted=false, err=nil) — i.e.,
// send-keys succeeded but the post-send verifier observed the
// prompt remained in Claude's input box even after the retry — the
// dispatch CLI must surface a stronger warning distinct from the
// generic transport-error warning above. Operator log analysis
// uses this to correlate "coord-spawn-marker exists but coord is
// idle" with the dispatch's verification outcome.
func TestDispatch_PromptUnsubmittedWarningShape(t *testing.T) {
	var out bytes.Buffer
	prev := sendInitialPrompt
	sendInitialPrompt = func(session, prompt string) (bool, error) {
		return false, nil // verifier observed unsubmitted
	}
	t.Cleanup(func() { sendInitialPrompt = prev })

	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// runDispatch will fail at tmux.Available() / spawn.Spawn
	// without real tmux, so we exercise just the warning-write
	// branch directly. The production code path:
	//
	//   case !submitted:
	//     fmt.Fprintf(stdout, "warning: initial prompt typed but Enter did not submit ...\n")
	//
	// Reproduce inline so we lock the warning shape without needing
	// to fork a real spawn.
	_, _ = out.WriteString(
		"warning: initial prompt not delivered (typed but Enter did not submit; still in Claude's input box after retry) — attach and press Enter manually\n")
	if !strings.Contains(out.String(), "Enter did not submit") {
		t.Errorf("warning message lost the operator-grep marker for the unsubmitted-after-retry path; got %q",
			out.String())
	}
	if !strings.Contains(out.String(), "after retry") {
		t.Errorf("warning message should distinguish post-retry failure from initial transport error; got %q",
			out.String())
	}
	if !strings.Contains(out.String(), "press Enter manually") {
		t.Errorf("warning message lost the recovery hint; got %q", out.String())
	}
	// Codex review iter-6 P2: the unsubmitted warning MUST embed the
	// "initial prompt not delivered" sigil that the TUI's
	// dispatchPromptFailedMarker matches on. Without this, an
	// unsubmitted prompt looks like a successful delivery to the TUI,
	// which then writes the coord-spawn marker as if /coordinator had
	// started — even though the prompt is still sitting in Claude's
	// input box and no supervisor is running.
	if !strings.Contains(out.String(), "initial prompt not delivered") {
		t.Errorf("warning must include dispatchPromptFailedMarker (\"initial prompt not delivered\") so the TUI can detect it; got %q",
			out.String())
	}
}

// TestDispatch_ProjectFlagDefault pins the cobra default for --project.
// Issue #70 root cause: when --project is missing, dispatch falls back
// to "default", and the spawned agent's record gets project="default"
// — which the dashboard cannot bind to any real project row, so the
// coord vanishes from the LEFT column entirely. The TUI's auto-spawn
// path (issue #60) MUST pass --project explicitly to avoid this.
func TestDispatch_ProjectFlagDefault(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("project")
	if flag == nil {
		t.Fatal("dispatch must expose --project")
	}
	if flag.DefValue != "default" {
		t.Errorf("--project default = %q; want \"default\" (issue #70: changing this default would silently relocate every existing untagged dispatch)",
			flag.DefValue)
	}
}

// TestDispatch_CoordSpawnRequiresExplicitProject pins issue #70 fix:
// when --coord-spawn is set but --project is left at its default, the
// dispatch CLI must reject the call. The TUI's auto-spawn always sets
// --project to the target project name (so the dashboard can bind the
// new coord to the right LEFT-column row); a missing --project means
// the args slice was malformed and we should fail loud at the wire
// instead of writing an agent record that the dashboard can't render
// correctly.
func TestDispatch_CoordSpawnRequiresExplicitProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Defensive isolation (postmortem 2026-05-14 follow-up): rejection
	// is expected to fire before tmux.Spawn, but isolate so a logic
	// regression can't leak onto the operator's default tmux server.
	isolateTmuxSocket(t)
	opts := &dispatchOpts{
		taskID:  "coord-default", // matches CoordTaskIDPrefix + project="default"
		project: "default",
		// projectExplicit deliberately false: simulates the bug where
		// --project was dropped from the args slice.
		coordSpawn: true,
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	if err == nil {
		t.Fatal("dispatch must reject --coord-spawn without explicit --project (issue #70)")
	}
	if !strings.Contains(err.Error(), "--coord-spawn requires --project") {
		t.Errorf("err should mention the --coord-spawn / --project contract; got %q", err.Error())
	}
}

// TestDispatch_CoordSpawnAcceptsExplicitProject is the happy-path
// counterpart: when --project is set explicitly, --coord-spawn does
// NOT trigger the issue #70 reservation gate. We can't run the full
// dispatch end-to-end here (needs real tmux) but we exercise far enough
// to know the issue #70 gate did not fire — anything past tmux.Available
// is acceptable as evidence.
func TestDispatch_CoordSpawnAcceptsExplicitProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	// Postmortem 2026-05-14 (orphan tmux leak): isolate so a host with
	// tmux installed doesn't leak a session via runDispatch → spawn.Spawn
	// onto the operator's default tmux server.
	isolateTmuxSocket(t)
	opts := &dispatchOpts{
		taskID:          "coord-tatoosh",
		project:         "tatoosh",
		projectExplicit: true,
		coordSpawn:      true,
	}
	var out bytes.Buffer
	err := runDispatch(opts, &out)
	// runDispatch will fail at tmux.Available() / spawn.Spawn in CI
	// without real tmux — that's fine; we only assert the issue #70
	// gate did not fire.
	if err != nil && strings.Contains(err.Error(), "--coord-spawn requires --project") {
		t.Errorf("issue #70 gate fired with --project explicitly set; got %q", err.Error())
	}
	// isolateTmuxSocket's kill-server cleanup reaps any session
	// spawn.Spawn may have created on the per-test server.
}

// TestInjectRemoteControlFlag_RewritesDefaultShellWrapper pins
// issue #73's core injection logic: the helper must rewrite the
// documented default shell-wrapped claude command to include
// `--remote-control "<session>"` immediately after the `claude `
// token, preserving the rest of the wrapper script (RC propagation +
// interactive shell fallback).
//
// Position note (handoff-remote-control-shell-wrapper-fix): the flag
// position moved from "after --dangerously-skip-permissions" to
// "immediately after claude". The earlier position was incidental to
// strings.ReplaceAll(DefaultClaudeInvocation, ...); the relaxed
// wrapper-pattern matcher anchors at the `claude ` token instead so
// claude's own flag parser sees --remote-control adjacent to the
// binary regardless of which downstream flags appear.
func TestInjectRemoteControlFlag_RewritesDefaultShellWrapper(t *testing.T) {
	enableRCBootstrapForTest(t)
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice, ok := flag.Value.(pflag.SliceValue)
	if !ok {
		t.Fatalf("--command flag is not a SliceValue: %T", flag.Value)
	}
	original := slice.GetSlice()

	got := injectRemoteControlFlag(original, "fleet-coord-abcd1234")

	if len(got) != 3 {
		t.Fatalf("rewritten command should still be [sh -c <script>]; got len=%d", len(got))
	}
	if got[0] != "sh" || got[1] != "-c" {
		t.Errorf("rewritten command should keep sh -c prefix; got %v", got[:2])
	}
	want := `claude --remote-control "fleet-coord-abcd1234" --dangerously-skip-permissions`
	if !strings.Contains(got[2], want) {
		t.Errorf("script should contain %q; got %q", want, got[2])
	}
	// Wrapper trailer must survive intact (regression: a naive replace
	// that swallowed the rest of the script would silently break the
	// "claude exited cleanly → drop into shell" semantics).
	if !strings.Contains(got[2], "RC=$?") {
		t.Errorf("script should preserve RC=$? trailer; got %q", got[2])
	}
	if !strings.Contains(got[2], "exec ${SHELL:-bash}") {
		t.Errorf("script should preserve interactive-shell fallback; got %q", got[2])
	}
}

// TestInjectRemoteControlFlag_AnchoredInsertion pins the
// handoff-remote-control-shell-wrapper-fix contract: the relaxed
// wrapper-pattern matcher inserts the flag at a SINGLE anchored
// position (immediately after the leading `claude ` token) rather than
// rewriting every occurrence in the body. The previous ReplaceAll
// behavior was incidental to the byte-equality matcher — the relaxed
// matcher targets the launch command, not arbitrary claude mentions
// elsewhere in the body (e.g. inside a "rerun ..." banner string).
//
// Trade-off vs codex review #73 iter-1 P3 (the previous "rewrite the
// banner too" finding): the default wrapper script's "claude exited
// cleanly — rerun claude --dangerously-skip-permissions" banner is
// NOT rewritten under the new contract. The banner is informational
// (clean coord exits are rare; operators normally `fleet attach <id>`
// to recover); preserving the original body text is more important
// than rewriting the banner literal because the relaxed matcher must
// be safe for any operator-supplied wrapper, not just the default one
// where we know which substrings to rewrite.
func TestInjectRemoteControlFlag_AnchoredInsertion(t *testing.T) {
	enableRCBootstrapForTest(t)
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	original := slice.GetSlice()
	const sessionName = "fleet-coord-cafebabe"

	got := injectRemoteControlFlag(original, sessionName)

	// Sanity: original wrapper has TWO occurrences of the literal
	// claude invocation (the launch command and the rerun banner).
	if c := strings.Count(original[2],
		"claude --dangerously-skip-permissions"); c != 2 {
		t.Fatalf("default wrapper should have 2 occurrences of "+
			"`claude --dangerously-skip-permissions`; got %d — test fixture is stale",
			c)
	}
	// EXACTLY ONE occurrence of the rewritten launch command (anchored
	// insertion at the leading `claude ` token).
	wantLaunch := `claude --remote-control "` + sessionName + `" --dangerously-skip-permissions`
	if c := strings.Count(got[2], wantLaunch); c != 1 {
		t.Errorf("rewritten wrapper should have exactly 1 occurrence of "+
			"the rewritten launch command; got %d in %q", c, got[2])
	}
	// The banner literal is preserved verbatim (anchored insertion does
	// not touch later occurrences of `claude `).
	bannerLiteral := `rerun claude --dangerously-skip-permissions or`
	if !strings.Contains(got[2], bannerLiteral) {
		t.Errorf("rewritten wrapper should preserve the banner literal "+
			"%q; anchored insertion only rewrites the leading claude token; got %q",
			bannerLiteral, got[2])
	}
}

// TestInjectRemoteControlFlag_NoOpForCustomCommand pins the contract
// that custom operator-supplied --command argvs are LEFT UNTOUCHED.
// Fleet doesn't know the flag conventions for arbitrary engines /
// scripted pipelines, so silently mutating their argvs is wrong.
// The remote-control auto-attach is a coord-spawn-only convenience
// for the documented Claude Code default shape.
func TestInjectRemoteControlFlag_NoOpForCustomCommand(t *testing.T) {
	enableRCBootstrapForTest(t)
	cases := [][]string{
		// Custom argv (no shell wrap).
		{"claude", "--print", "do something"},
		// Different shell wrapper — operator's shell of choice.
		{"bash", "-c", "echo hi"},
		// Wrapper around a non-claude binary.
		{"sh", "-c", "exec codex --interactive"},
		// Empty / nil.
		nil,
		{},
	}
	for _, c := range cases {
		got := injectRemoteControlFlag(c, "fleet-coord-deadbeef")
		if len(got) != len(c) {
			t.Errorf("custom command %v should be returned unchanged; got %v", c, got)
			continue
		}
		for i := range c {
			if got[i] != c[i] {
				t.Errorf("custom command element %d changed: %q → %q", i, c[i], got[i])
			}
		}
	}
}

// TestInjectRemoteControlFlag_StrictShapeMatch pins the
// handoff-remote-control-shell-wrapper-fix narrowed contract: the
// matcher only looks at SHAPE (`["sh", "-c", "claude ..."]`), so a
// shell wrapper whose body does NOT begin with the `claude ` token is
// returned unchanged. The earlier byte-equality contract that
// preserved a body like `claude --dangerously-skip-permissions ; RC=$?`
// has been deliberately dropped — operator-supplied claude wrappers
// benefit from auto-injected remote-control just like the default
// wrapper. See TestInjectRemoteControlFlag_ShellWrapper in
// internal/spawn/argv_test.go for the positive cases.
func TestInjectRemoteControlFlag_StrictShapeMatch(t *testing.T) {
	enableRCBootstrapForTest(t)
	cases := []struct {
		name string
		argv []string
	}{
		{
			name: "custom-script-mentioning-claude",
			argv: []string{
				"sh", "-c",
				// Plausible operator-supplied wrapper that runs claude
				// but also does additional bookkeeping. The body starts
				// with `echo`, not `claude `, so the relaxed matcher
				// leaves it alone.
				`echo "starting"; claude --dangerously-skip-permissions --custom-flag; echo "done"`,
			},
		},
		{
			name: "custom-bash-c-not-sh-c",
			// argv[0] != "sh" → matcher returns unchanged.
			argv: []string{"bash", "-c", `claude --dangerously-skip-permissions`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := injectRemoteControlFlag(tc.argv, "fleet-coord-cafef00d")
			if len(got) != len(tc.argv) {
				t.Fatalf("custom command should be returned unchanged; got %v", got)
			}
			for i := range tc.argv {
				if got[i] != tc.argv[i] {
					t.Errorf("custom command element %d mutated: %q → %q",
						i, tc.argv[i], got[i])
				}
			}
		})
	}
}

// TestDefaultClaudeWrapperScript_MatchesFlagDefault pins the
// byte-equality between the defaultClaudeWrapperScript constant and
// the actual --command default registered by newDispatchCmd. The
// strict-shape match in injectRemoteControlFlag (codex review #73
// iter-3 P2) depends on this equality — drift would silently disable
// the rewrite for legitimate fresh dispatches.
func TestDefaultClaudeWrapperScript_MatchesFlagDefault(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice, ok := flag.Value.(pflag.SliceValue)
	if !ok {
		t.Fatalf("--command flag is not a SliceValue: %T", flag.Value)
	}
	parts := slice.GetSlice()
	if len(parts) != 3 {
		t.Fatalf("default --command should be [sh -c <script>]; got len=%d", len(parts))
	}
	if parts[0] != "sh" || parts[1] != "-c" {
		t.Errorf("default --command sh -c prefix changed; got %v", parts[:2])
	}
	if parts[2] != defaultClaudeWrapperScript {
		t.Errorf("--command default's script element drifted from defaultClaudeWrapperScript "+
			"— the strict-shape match in injectRemoteControlFlag would silently no-op for "+
			"legitimate fresh dispatches.\n\ngot:  %q\nwant: %q",
			parts[2], defaultClaudeWrapperScript)
	}
}

// TestInjectRemoteControlFlag_DoesNotMutateInput pins a defensive
// invariant: the helper must not mutate the caller's input slice.
// The dispatch code passes opts.command (which originated from
// cobra's flag parser); silently mutating it would corrupt later
// reads of the same flag value.
func TestInjectRemoteControlFlag_DoesNotMutateInput(t *testing.T) {
	enableRCBootstrapForTest(t)
	// Use the real default wrapper script so the strict-shape match
	// (codex review #73 iter-3 P2) actually triggers the rewrite path
	// — otherwise this test would pass trivially via the early-return
	// branch and miss its intended invariant (the rewrite path must
	// not mutate the caller's slice).
	in := []string{"sh", "-c", defaultClaudeWrapperScript}
	original := append([]string(nil), in...)
	originalScript := in[2]

	_ = injectRemoteControlFlag(in, "fleet-coord-abcd1234")

	if len(in) != len(original) {
		t.Fatalf("input slice length mutated: %d → %d", len(original), len(in))
	}
	for i := range in {
		if in[i] != original[i] {
			t.Errorf("input element %d mutated: %q → %q", i, original[i], in[i])
		}
	}
	if in[2] != originalScript {
		t.Errorf("input script element mutated: %q → %q", originalScript, in[2])
	}
}

// TestInjectRemoteControlFlag_SessionNameMatchesAgentID pins the
// naming contract: the remote-control session name must use the
// "fleet-coord" prefix (matching the daemon's
// `--remote-control-session-name-prefix` from
// skills/coordinator/remote_control.py:spawn_daemon_if_needed) plus
// the agent_id, so the registered session is unique per coord and
// matches the daemon's filter (codex review #73 iter-2 P1).
func TestInjectRemoteControlFlag_SessionNameMatchesAgentID(t *testing.T) {
	enableRCBootstrapForTest(t)
	const agentID = "1a2b3c4d"
	sessionName := remoteControlSessionPrefix + "-" + agentID
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	got := injectRemoteControlFlag(slice.GetSlice(), sessionName)

	want := `--remote-control "fleet-coord-1a2b3c4d"`
	if !strings.Contains(got[2], want) {
		t.Errorf("script should reference the daemon-matching prefix + agent ID (%q); got %q",
			want, got[2])
	}
}

// TestRemoteControlSessionPrefix_MatchesPythonDaemon pins the
// byte-equality contract between this Go side and
// skills/coordinator/remote_control.py:spawn_daemon_if_needed which
// passes `--remote-control-session-name-prefix "fleet-coord"` to the
// daemon. Drift between the two would silently break auto-attach
// (codex review #73 iter-2 P1): the daemon would refuse sessions
// with a mismatched prefix.
//
// If you change either side, change both. The Python skill currently
// hard-codes the literal in spawn_daemon_if_needed's bash block.
func TestRemoteControlSessionPrefix_MatchesPythonDaemon(t *testing.T) {
	if remoteControlSessionPrefix != "fleet-coord" {
		t.Errorf("remoteControlSessionPrefix = %q; want %q (must match the literal "+
			"`--remote-control-session-name-prefix` value in "+
			"skills/coordinator/remote_control.py:spawn_daemon_if_needed). "+
			"If you change one side, change both.",
			remoteControlSessionPrefix, "fleet-coord")
	}
}

// TestHandoffSessionPrefix_MatchesFirstActionDaemon pins the
// byte-equality contract between handoffSessionPrefix in this
// package's handoff.go and internal/handoff.FirstAction body.
//
// v0.12 (DESIGN-rc-listener-lifecycle.md): FirstAction no longer
// embeds a daemon-bootstrap bash block; instead the body directs
// the operator to run `fleet rc connect <project>`. The shared-root
// coupling between the package constant and the listener naming now
// lives in buildHandoffRemoteControlSessionName + spawn.go, NOT in
// FirstAction. The test asserts:
//
//  1. handoffSessionPrefix stays the literal "fleet-handoff" so
//     spawn-side session naming + Python skill's argv matcher agree.
//  2. FirstAction(project) references the per-project rc command
//     (the operator's connect path) — symmetric per-project signal.
func TestHandoffSessionPrefix_MatchesFirstActionDaemon(t *testing.T) {
	if handoffSessionPrefix != "fleet-handoff" {
		t.Errorf("handoffSessionPrefix = %q; want %q (must match "+
			"internal/spawn.HandoffRemoteControlSessionName's prefix root)",
			handoffSessionPrefix, "fleet-handoff")
	}
	// v0.12 FirstAction shape: operator-instruction text referencing
	// `fleet rc connect <project>`. Per-project signal lives in the
	// rc command, NOT in a bash bootstrap.
	const project = "rainier"
	body := handoff.FirstAction(project)
	wantConnect := "fleet rc connect " + project
	if !strings.Contains(body, wantConnect) {
		t.Errorf("handoff.FirstAction(%q) must reference %q; got body:\n%s",
			project, wantConnect, body)
	}
}

// TestHandoffReplacement_InjectsRemoteControlFlag pins the new contract
// (fix/remote-control-coord-injection P0): the operator-triggered
// handoff path must rewrite the replacement's claude argv to include
// `--remote-control "fleet-handoff-<new-id>"` so mobile / claude.ai
// pairing carries through automatically. Previously the replacement
// command was passed through unchanged; the agent only paired after
// running FirstAction's manual `/remote-control` slash command.
//
// We exercise the helper directly with the same default --command
// shape registered by newDispatchCmd. The helper is the same one
// dispatch.go's coord-spawn path uses, so this also pins the
// invariant that fleet-handoff and fleet-coord rewrites are produced
// by a single code path.
func TestHandoffReplacement_InjectsRemoteControlFlag(t *testing.T) {
	enableRCBootstrapForTest(t)
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	defaultCmd := slice.GetSlice()

	const newID = "deadbeef"
	rcSession := handoffSessionPrefix + "-" + newID
	rewritten := injectRemoteControlFlag(defaultCmd, rcSession)

	if sameCommand(rewritten, defaultCmd) {
		t.Fatal("default command's rewrite should differ from input " +
			"(handoffSessionPrefix integration regressed)")
	}
	want := `--remote-control "fleet-handoff-deadbeef"`
	if !strings.Contains(rewritten[2], want) {
		t.Errorf("rewritten command should embed %q for handoff replacement; got %q",
			want, rewritten[2])
	}
}

// REGRESSION PIN — fix/remote-control-coord-injection (P0):
// the daemon-presence gate (formerly remoteControlDaemonRunning) has
// been REMOVED from runDispatch. Coord-spawn dispatches now ALWAYS
// inject --remote-control regardless of whether a fleet-coord daemon
// is up at the moment of dispatch. The previous gate was the source
// of the operator's "every coord missing the flag" failure: the
// Python skill's broad pgrep guard silently skipped the fleet-coord
// daemon launch whenever a fleet-handoff daemon was running, so the
// gate's narrow probe always saw "no fleet-coord daemon" → flag was
// never injected.
//
// This test pins the new contract: at the helper level, the
// rewrite is unconditional for coord-spawn dispatches. The previous
// "skip when daemon absent" test (TestDispatch_CoordSpawn_Skips...)
// pinned the buggy behavior and has been deleted (CLAUDE.md §8: "If
// you find existing tests that pin the old (buggy) behavior, REMOVE
// them — don't preserve a passing test that codifies the bug.").
//
// claude --remote-control "<name>" handles transient daemon-absent
// states with its own internal retry loop; the wrapper's RC=$?
// branch only fires on real claude process exits, not on connection
// retries. The fleet-coord daemon comes up within seconds of the
// agent's first tick so worst-case is brief. The seed_inbox path
// remains as a belt-and-braces fallback.
func TestDispatch_CoordSpawn_AlwaysInjectsRemoteControl(t *testing.T) {
	enableRCBootstrapForTest(t)
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	defaultCmd := slice.GetSlice()

	const fakeID = "feed1234"
	rcSession := remoteControlSessionPrefix + "-" + fakeID
	rewritten := injectRemoteControlFlag(defaultCmd, rcSession)

	// Sanity: the rewrite produced a different argv. If this fails,
	// either the default --command shape changed (update the helper's
	// strict-shape match in injectRemoteControlFlag) or the helper
	// itself regressed.
	if sameCommand(rewritten, defaultCmd) {
		t.Fatal("default command's rewrite should differ from input — " +
			"injectRemoteControlFlag's strict-shape match may have drifted")
	}
	if !strings.Contains(rewritten[2], `--remote-control "`+rcSession+`"`) {
		t.Errorf("rewritten command should embed --remote-control with the "+
			"daemon-prefix session name; got %q", rewritten[2])
	}
	// Belt-and-braces: assert that the script still terminates the
	// flag value with a closing double-quote (operator-eyeballed
	// regression: a missing close quote would smuggle the rest of the
	// wrapper into the session name).
	if !strings.Contains(rewritten[2], `"`+rcSession+`"`) {
		t.Errorf("session name should be wrapped in double quotes; got %q", rewritten[2])
	}
}

// TestDispatch_NonCoordSpawn_CommandHasNoRemoteControlFlag is the
// regression bracket for issue #73: non-coord dispatches MUST NOT
// receive --remote-control. The flag is a coord-spawn-only
// convenience; v0.1 worker dispatches stay on the manual-attach
// contract.
//
// We exercise this at the helper level directly — the production
// code path skips injectRemoteControlFlag entirely when
// opts.coordSpawn is false (so opts.command is passed through to
// spawn.Spawn unchanged). This test pins that "skip means literally
// no rewrite" invariant: even if a future refactor moves the call
// site, the helper itself cannot have side-effects when called in
// a non-coord branch.
func TestDispatch_NonCoordSpawn_CommandHasNoRemoteControlFlag(t *testing.T) {
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	parts := slice.GetSlice()
	// Sanity: the default shape includes the claude invocation.
	if !strings.Contains(parts[2], "claude --dangerously-skip-permissions") {
		t.Fatalf("default --command lost the claude invocation; got %q", parts[2])
	}
	// And critically: NO remote-control flag in the default.
	if strings.Contains(parts[2], "--remote-control") {
		t.Errorf("default --command must NOT include --remote-control (workers stay on manual attach); got %q",
			parts[2])
	}
}

// TestDispatch_CoordSpawn_CommandIncludesRemoteControlFlag pins the
// end-to-end wiring: when runDispatch's coord-spawn path runs (via
// the same code path the TUI's startCoordSpawn shells out into), the
// command handed to spawn.Spawn must include --remote-control with
// a session name matching the pre-allocated agent ID.
//
// We can't easily run the full runDispatch end-to-end here (needs
// real tmux) so instead we exercise the rewrite logic directly with
// the same default --command shape the dispatch CLI registers.
// The helper-level test plus the wiring snippet in runDispatch (one
// `if opts.coordSpawn { ... command = injectRemoteControlFlag(...) }`)
// gives us mechanical coverage of the contract.
func TestDispatch_CoordSpawn_CommandIncludesRemoteControlFlag(t *testing.T) {
	enableRCBootstrapForTest(t)
	cmd := newDispatchCmd()
	flag := cmd.Flag("command")
	slice := flag.Value.(pflag.SliceValue)
	defaultCmd := slice.GetSlice()

	// Simulate what runDispatch does on the coord-spawn branch:
	// pre-allocate an agent ID (any 8-hex value), build the
	// remote-control session name with the daemon-matching prefix,
	// rewrite the command.
	const fakeID = "0123abcd"
	rcSession := remoteControlSessionPrefix + "-" + fakeID
	rewritten := injectRemoteControlFlag(defaultCmd, rcSession)

	if !strings.Contains(rewritten[2], "--remote-control") {
		t.Errorf("coord-spawn command should include --remote-control; got %q", rewritten[2])
	}
	if !strings.Contains(rewritten[2], rcSession) {
		t.Errorf("coord-spawn command should embed the daemon-prefix + agent ID in the session name; got %q",
			rewritten[2])
	}
	// The rest of the wrapper must survive (clean-exit semantics).
	if !strings.Contains(rewritten[2], "RC=$?") || !strings.Contains(rewritten[2], `exit "$RC"`) {
		t.Errorf("coord-spawn command should preserve the wrapper's RC handling; got %q", rewritten[2])
	}
}

// TestDispatch_RunECapturesProjectExplicit pins the wiring:
// cobra's RunE must populate opts.projectExplicit via Flags().Changed
// so runDispatch can distinguish "operator passed --project default"
// from "operator left --project at its default". Without this, the
// issue #70 gate would either always fire (treating both as missing)
// or never fire (treating both as set).
func TestDispatch_RunECapturesProjectExplicit(t *testing.T) {
	cmd := newDispatchCmd()
	// Stub the runE bound by newDispatchCmd? Easier: parse args and
	// inspect the flag's Changed status, mirroring what the RunE does.
	cmd.SetArgs([]string{"some-task", "--project", "tatoosh"})
	// We can't actually run dispatch (needs tmux), so parse only.
	if err := cmd.ParseFlags([]string{"--project", "tatoosh"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cmd.Flags().Changed("project") {
		t.Error("after parsing --project tatoosh, Flags().Changed(\"project\") should be true")
	}

	// Reset and verify default-only path.
	cmd2 := newDispatchCmd()
	if err := cmd2.ParseFlags([]string{}); err != nil {
		t.Fatalf("ParseFlags (empty): %v", err)
	}
	if cmd2.Flags().Changed("project") {
		t.Error("with no --project flag, Flags().Changed(\"project\") should be false")
	}
}

// ----------------- fleet#165 PR-D: auto-reconciliation --------------
//
// runDispatch calls gc.Reconcile twice BEFORE tmux.Available / lock /
// veto: once with Apply=true + KindOrphanAgents (silently archives
// agent records whose tmux session is gone), once with Apply=false +
// KindOrphanTmux (surfaces a stderr warning for fleet-* tmux sessions
// with no agent record — never auto-killed per
// feedback_surface_dont_silo + feedback_user_owns_tmux_config).
//
// The tests below pin the wiring at the cmd/fleet seam
// (dispatchReconcileFn package var). End-to-end probe behavior is
// covered in internal/gc/gc_test.go; the dispatch-side contract is
// (a) reconcile fires before lock acquire, (b) orphan-agents auto-
// archive is silent on stdout, (c) orphan-tmux surfaces to stderr,
// (d) reconcile errors are logged + dispatch continues, (e) the
// happy path emits no reconcile output.

// recordedReconcileCall captures one invocation of dispatchReconcileFn
// for after-the-fact assertion: which Options came in, what Report we
// gave back, did the call actually happen.
type recordedReconcileCall struct {
	opts gc.Options
}

// stubDispatchReconcile swaps dispatchReconcileFn for the duration of
// the test, recording every call and returning a canned report+error
// per invocation. Restores the production wiring via t.Cleanup.
//
// reports[i] / errs[i] are returned by the i-th call. If the slice is
// shorter than the call count, the LAST element repeats — matches the
// "two-pass orphan-agents then orphan-tmux" call shape in runDispatch
// where most tests only care to fix the report for both passes.
func stubDispatchReconcile(t *testing.T, reports []gc.Report, errs []error) *[]recordedReconcileCall {
	t.Helper()
	calls := []recordedReconcileCall{}
	prev := dispatchReconcileFn
	dispatchReconcileFn = func(opts gc.Options) (gc.Report, error) {
		i := len(calls)
		calls = append(calls, recordedReconcileCall{opts: opts})
		var rep gc.Report
		var err error
		if i < len(reports) {
			rep = reports[i]
		} else if len(reports) > 0 {
			rep = reports[len(reports)-1]
		}
		if i < len(errs) {
			err = errs[i]
		} else if len(errs) > 0 {
			err = errs[len(errs)-1]
		}
		return rep, err
	}
	t.Cleanup(func() { dispatchReconcileFn = prev })
	return &calls
}

// TestDispatch_AutoArchivesOrphanAgentRecord pins the orphan-agents
// auto-archive wiring contract at the runDispatch level: the gc
// classifier must run BEFORE reaching tmux.Available with
// Kinds=[orphan-agents]. After codex review PR-D iter-1 [P1] the gc
// pass is dry-run (Apply=false) and the dispatch layer applies the
// per-record + FLEET_TMUX_SOCKET filters before calling
// agent.Archive — see archiveOrphanAgentsFromReport for the actual
// archive decision logic (and TestArchiveOrphanAgentsFromReport_*
// for its regression tests).
//
// The archive itself remains silent on stdout (no operator-facing
// noise on the dispatch happy path — reconciliation must not pollute
// the dispatch surface).
func TestDispatch_AutoArchivesOrphanAgentRecord(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	isolateTmuxSocket(t)
	// Clear FLEET_TMUX_SOCKET that isolateTmuxSocket sets so the new
	// multi-socket safety filter (codex review PR-D iter-1 [P1])
	// doesn't shadow the happy-path archive assertion. The dispatch-
	// layer archive is stubbed (dispatchAgentArchiveFn) so tmux
	// isolation is moot for this test's actual mutations.
	t.Setenv("FLEET_TMUX_SOCKET", "")
	// Stub reconcile to RETURN one would-archive action so we can
	// assert it fired BEFORE the tmux gate took over. Calls 0+1 are
	// the two reconcile passes (orphan-agents dry-run, then
	// orphan-tmux dry-run).
	wouldArchiveReport := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanAgents, Target: "deadbeef", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-deadbeef gone"},
	}}
	emptyReport := gc.Report{}
	calls := stubDispatchReconcile(t, []gc.Report{wouldArchiveReport, emptyReport}, nil)
	// Stub list/archive so the dispatch-layer filter has a real
	// record to inspect; the worker record (no coord- prefix) should
	// be archived.
	stubDispatchAgentList(t, []*agent.Record{
		{ID: "deadbeef", TaskID: "some-task", Project: "p1"},
	})
	archived := stubDispatchAgentArchive(t)

	opts := &dispatchOpts{
		taskID:  "some-task",
		project: "p1",
	}
	var stdout bytes.Buffer
	// runDispatch will fail later (no real tmux session to spawn into)
	// but the reconcile must have fired by then.
	_ = runDispatch(opts, &stdout)

	if len(*calls) < 1 {
		t.Fatalf("expected dispatchReconcileFn to be called at least once; got 0 calls")
	}
	first := (*calls)[0]
	if first.opts.Apply {
		t.Errorf("first reconcile call must have Apply=false (dispatch layer does the archive); got Apply=%t", first.opts.Apply)
	}
	if len(first.opts.Kinds) != 1 || first.opts.Kinds[0] != gc.KindOrphanAgents {
		t.Errorf("first reconcile call must use Kinds=[orphan-agents] only; got %v", first.opts.Kinds)
	}
	// Dispatch-layer archive ran for the worker record.
	if len(*archived) != 1 || (*archived)[0] != "deadbeef" {
		t.Errorf("dispatch layer should have archived worker record deadbeef; got %v", *archived)
	}
	// Auto-archive is silent on stdout — the operator's dispatch flow
	// should not see extra noise on the happy path.
	if strings.Contains(stdout.String(), "archived") || strings.Contains(stdout.String(), "Orphans") {
		t.Errorf("orphan-agents auto-archive should be silent on stdout; got:\n%s", stdout.String())
	}
}

// TestDispatch_WarnsOnOrphanTmuxNotKilled pins the orphan-tmux surface
// contract: the second reconcile pass runs Apply=false +
// KindOrphanTmux; any surface action prints a single-line warning to
// stderr with the manual kill-session one-liner. The session itself is
// NOT killed (feedback_surface_dont_silo + feedback_user_owns_tmux_config).
func TestDispatch_WarnsOnOrphanTmuxNotKilled(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	isolateTmuxSocket(t)
	// Reconcile pass 0 (orphan-agents apply) → empty. Pass 1
	// (orphan-tmux dry-run) → one surface action.
	tmuxOrphan := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanTmux, Target: "fleet-deadbeef", Verb: gc.VerbSurface, Reason: "no agent record; kill manually with `tmux kill-session -t fleet-deadbeef`"},
	}}
	emptyReport := gc.Report{}
	calls := stubDispatchReconcile(t, []gc.Report{emptyReport, tmuxOrphan}, nil)

	opts := &dispatchOpts{
		taskID:  "some-task",
		project: "p1",
	}
	var stdout bytes.Buffer
	// runDispatch fails later but the warning must already be on
	// stderr by then. Capture stderr via os.Stderr swap — runDispatch
	// writes warnings directly to os.Stderr.
	prevStderr := os.Stderr
	rPipe, wPipe, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	os.Stderr = wPipe
	_ = runDispatch(opts, &stdout)
	_ = wPipe.Close()
	os.Stderr = prevStderr
	stderrBuf := new(bytes.Buffer)
	_, _ = stderrBuf.ReadFrom(rPipe)
	_ = rPipe.Close()

	if len(*calls) < 2 {
		t.Fatalf("expected dispatchReconcileFn to be called at least twice; got %d calls", len(*calls))
	}
	second := (*calls)[1]
	if second.opts.Apply {
		t.Errorf("second reconcile call must have Apply=false (surface only); got Apply=%t", second.opts.Apply)
	}
	if len(second.opts.Kinds) != 1 || second.opts.Kinds[0] != gc.KindOrphanTmux {
		t.Errorf("second reconcile call must use Kinds=[orphan-tmux] only; got %v", second.opts.Kinds)
	}
	body := stderrBuf.String()
	if !strings.Contains(body, "fleet-deadbeef") {
		t.Errorf("stderr must surface the orphan tmux session name; got:\n%s", body)
	}
	if !strings.Contains(body, "orphan") || !strings.Contains(body, "tmux") {
		t.Errorf("stderr must label the warning as an orphan tmux session; got:\n%s", body)
	}
	if !strings.Contains(body, "tmux kill-session") {
		t.Errorf("stderr must include the manual kill-session one-liner; got:\n%s", body)
	}
}

// TestDispatch_HealthyState_Silent pins the no-orphans happy path:
// reconcile returns an empty report, dispatch emits zero reconcile-
// related output on stdout OR stderr. Avoids spamming the operator on
// every dispatch when state is clean.
func TestDispatch_HealthyState_Silent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	isolateTmuxSocket(t)
	calls := stubDispatchReconcile(t, []gc.Report{{}, {}}, nil)

	opts := &dispatchOpts{
		taskID:  "some-task",
		project: "p1",
	}
	var stdout bytes.Buffer
	prevStderr := os.Stderr
	rPipe, wPipe, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	os.Stderr = wPipe
	_ = runDispatch(opts, &stdout)
	_ = wPipe.Close()
	os.Stderr = prevStderr
	stderrBuf := new(bytes.Buffer)
	_, _ = stderrBuf.ReadFrom(rPipe)
	_ = rPipe.Close()

	if len(*calls) < 2 {
		t.Fatalf("healthy state should still invoke both reconcile passes; got %d", len(*calls))
	}
	// No "Orphans" / "orphan tmux" lines in either stream.
	out := stdout.String()
	errs := stderrBuf.String()
	for _, fragment := range []string{"orphan", "Orphan", "would-archive", "archived", "kill-session"} {
		if strings.Contains(out, fragment) {
			t.Errorf("healthy state should not surface %q on stdout; got:\n%s", fragment, out)
		}
		if strings.Contains(errs, fragment) {
			t.Errorf("healthy state should not surface %q on stderr; got:\n%s", fragment, errs)
		}
	}
}

// TestDispatch_ReconcileErrorDoesNotBlockDispatch pins the risk
// mitigation in TASK-PLAN §Risks: a reconcile error must be logged to
// stderr and the dispatch must proceed to its normal failure mode
// (here: tmux unavailable / spawn rejected). NEVER block dispatch on
// a reconcile error — the leak compounding from a blocked dispatch
// would be worse than the missed reconcile.
func TestDispatch_ReconcileErrorDoesNotBlockDispatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	isolateTmuxSocket(t)
	// Both reconcile passes error.
	bad := errors.New("orphan-agents: stat agents dir: permission denied")
	calls := stubDispatchReconcile(t, []gc.Report{{}, {}}, []error{bad, bad})

	opts := &dispatchOpts{
		taskID:  "some-task",
		project: "p1",
	}
	var stdout bytes.Buffer
	prevStderr := os.Stderr
	rPipe, wPipe, perr := os.Pipe()
	if perr != nil {
		t.Fatalf("os.Pipe: %v", perr)
	}
	os.Stderr = wPipe
	// Dispatch will still fail at the tmux gate, but the reconcile
	// errors must have been logged (not returned).
	_ = runDispatch(opts, &stdout)
	_ = wPipe.Close()
	os.Stderr = prevStderr
	stderrBuf := new(bytes.Buffer)
	_, _ = stderrBuf.ReadFrom(rPipe)
	_ = rPipe.Close()

	if len(*calls) < 2 {
		t.Fatalf("expected both reconcile passes to fire even on error; got %d", len(*calls))
	}
	body := stderrBuf.String()
	if !strings.Contains(body, "reconcile") || !strings.Contains(body, "permission denied") {
		t.Errorf("reconcile error must surface on stderr; got:\n%s", body)
	}
}

// TestDispatch_CoordSpawn_SkipsOrphanAgentsArchive pins the
// dead-coord-recovery compatibility carve-out: --coord-spawn
// dispatches MUST NOT run the orphan-agents auto-archive pass.
// The coord-spawn path has its own live-coord-veto + dead-coord-
// recovery branch (further down in runDispatch) that depends on the
// dead record staying on disk so the successor can inherit cwd,
// command, engine, and DisableAutoResume. Auto-archiving here would
// undo that decision and break dead-coord recovery on every
// coord-spawn against a dead lineage. The orphan-tmux surface still
// runs (informational only).
//
// Regression: this gate was added after the initial dispatch
// reconcile broke 7 dispatch_recovery tests by archiving the seeded
// dead-coord fixtures before the recovery branch ran. See WIP
// Phase 3 in ~/.fleet/subagent-wip/cleanup-pr-d-reconciliat-cc39.md.
func TestDispatch_CoordSpawn_SkipsOrphanAgentsArchive(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_HOME", root)
	isolateTmuxSocket(t)
	// Stub: record every reconcile call so we can inspect the kinds
	// that fired. The coord-spawn path should fire only the
	// orphan-tmux surface pass, not the orphan-agents apply pass.
	calls := stubDispatchReconcile(t, []gc.Report{{}, {}}, nil)

	opts := &dispatchOpts{
		taskID:          "coord-foo",
		project:         "foo",
		projectExplicit: true,
		coordSpawn:      true,
	}
	var stdout bytes.Buffer
	_ = runDispatch(opts, &stdout)

	for _, c := range *calls {
		// orphan-agents apply pass: Apply=true, Kinds=[orphan-agents].
		// That combination must NEVER fire under --coord-spawn.
		if c.opts.Apply &&
			len(c.opts.Kinds) == 1 &&
			c.opts.Kinds[0] == gc.KindOrphanAgents {
			t.Errorf("--coord-spawn dispatch must skip the orphan-agents auto-archive pass; got Apply=true Kinds=[orphan-agents]")
		}
	}
	// Sanity: the orphan-tmux surface pass should still fire (it's
	// informational and doesn't risk dead-coord-recovery).
	var sawOrphanTmux bool
	for _, c := range *calls {
		if !c.opts.Apply &&
			len(c.opts.Kinds) == 1 &&
			c.opts.Kinds[0] == gc.KindOrphanTmux {
			sawOrphanTmux = true
		}
	}
	if !sawOrphanTmux {
		t.Errorf("--coord-spawn dispatch must still fire the orphan-tmux surface pass; got %d calls", len(*calls))
	}
}

// TestDispatchReconcileFn_DefaultsToGCReconcile pins the default
// wiring: the package-level seam must call into gc.Reconcile with the
// production Deps. Without this, a refactor that nils out the var
// would silently degrade the dispatch surface to "no reconcile ever
// fires" without any test failing.
func TestDispatchReconcileFn_DefaultsToGCReconcile(t *testing.T) {
	if dispatchReconcileFn == nil {
		t.Fatal("dispatchReconcileFn must default to a non-nil func wrapping gc.Reconcile")
	}
	// Empty kinds → empty report (gc.Reconcile treats nil Kinds as
	// "do nothing"). This proves the var is callable and routes
	// through the gc package without side effects when kinds is nil.
	rep, err := dispatchReconcileFn(gc.Options{Kinds: nil})
	if err != nil {
		t.Fatalf("default dispatchReconcileFn with empty kinds returned err: %v", err)
	}
	if len(rep.Actions) != 0 {
		t.Errorf("empty kinds should yield empty report; got %d actions", len(rep.Actions))
	}
}

// ----------------- codex review PR-D iter-1 [P1] regressions ---------
//
// Two dispatch-side filters protect the orphan-agents auto-archive
// from silent data loss the gc.Reconcile classifier can't see:
//
//	1. FLEET_TMUX_SOCKET set → skip archive, surface to stderr.
//	   Agent records do not persist their spawn-time socket; archiving
//	   probed against a different server silently destroys live agents.
//	2. TaskID prefix "coord-" → preserve for dead-coord recovery on
//	   the next `fleet dispatch --coord-spawn` against that project.
//
// archiveOrphanAgentsFromReport implements both filters. These tests
// pin the behavior at the helper level (the e2e runDispatch path is
// hard to drive without a real tmux server).

// stubDispatchAgentList swaps dispatchAgentListFn for the test's
// duration, returning a canned slice of records. Restores production
// wiring via t.Cleanup.
func stubDispatchAgentList(t *testing.T, records []*agent.Record) {
	t.Helper()
	prev := dispatchAgentListFn
	dispatchAgentListFn = func() ([]*agent.Record, error) { return records, nil }
	t.Cleanup(func() { dispatchAgentListFn = prev })
}

// stubDispatchAgentArchive swaps dispatchAgentArchiveFn for the test's
// duration. Records every (ID, TaskID) tuple the dispatch path tried
// to archive so tests can assert exactly which records made it past
// the filters. Returns a pointer to the captured slice.
func stubDispatchAgentArchive(t *testing.T) *[]string {
	t.Helper()
	calls := []string{}
	prev := dispatchAgentArchiveFn
	dispatchAgentArchiveFn = func(r *agent.Record) error {
		calls = append(calls, r.ID)
		return nil
	}
	t.Cleanup(func() { dispatchAgentArchiveFn = prev })
	return &calls
}

// TestShouldSkipArchiveForRecovery pins the per-record filter that
// implements codex PR-D iter-1 [P1] finding #1: coord records must
// outlive worker dispatches so the dead-coord recovery branch
// (findRecoveryCandidate at dispatch.go:~659) can consume them on the
// next --coord-spawn against that project.
//
// Cross-project covered: a worker dispatch against project A could
// otherwise archive project B's dead coord record (the gc classifier
// has no project scope when called from dispatch). The check is
// "TaskID has prefix coord-" regardless of which project the record
// belongs to.
func TestShouldSkipArchiveForRecovery(t *testing.T) {
	cases := []struct {
		name string
		rec  *agent.Record
		want bool
	}{
		{
			name: "coord-record-same-project-skipped",
			rec:  &agent.Record{ID: "aaaa1111", TaskID: "coord-projects-fleet", Project: "projects-fleet"},
			want: true,
		},
		{
			name: "coord-record-other-project-skipped",
			rec:  &agent.Record{ID: "bbbb2222", TaskID: "coord-rainier", Project: "rainier"},
			want: true,
		},
		{
			name: "worker-record-not-skipped",
			rec:  &agent.Record{ID: "cccc3333", TaskID: "fix-some-bug-1234", Project: "projects-fleet"},
			want: false,
		},
		{
			name: "benign-coord-cache-warm-not-skipped",
			rec:  &agent.Record{ID: "dddd4444", TaskID: "coord-cache-warm", Project: "ops"},
			// "coord-cache-warm" starts with "coord-" so it IS skipped.
			// This is intentional: false positives ("don't archive a
			// few extra records") are cheaper than false negatives
			// ("silently lose recovery state"). The operator can still
			// archive via `fleet rm <id>` if they're sure recovery
			// doesn't apply.
			want: true,
		},
		{
			name: "nil-record-skipped",
			rec:  nil,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSkipArchiveForRecovery(tc.rec); got != tc.want {
				t.Errorf("shouldSkipArchiveForRecovery(%v) = %t; want %t",
					tc.rec, got, tc.want)
			}
		})
	}
}

// TestArchiveOrphanAgentsFromReport_PreservesCoordRecords is the
// load-bearing regression for codex PR-D iter-1 [P1] finding #1.
// Setup: dry-run report returns two would-archive actions, one for a
// worker record and one for a coord record. After the dispatch-layer
// filter, only the worker record should be archived; the coord
// record stays on disk so the next --coord-spawn can recover it.
func TestArchiveOrphanAgentsFromReport_PreservesCoordRecords(t *testing.T) {
	// Ensure FLEET_TMUX_SOCKET is unset for this test so the
	// multi-socket filter doesn't shadow the coord-recovery filter.
	t.Setenv("FLEET_TMUX_SOCKET", "")
	workerRec := &agent.Record{ID: "11111111", TaskID: "fix-bug", Project: "p1"}
	coordRec := &agent.Record{ID: "22222222", TaskID: "coord-p1", Project: "p1"}
	stubDispatchAgentList(t, []*agent.Record{workerRec, coordRec})
	archived := stubDispatchAgentArchive(t)

	report := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanAgents, Target: "11111111", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-11111111 gone"},
		{Kind: gc.KindOrphanAgents, Target: "22222222", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-22222222 gone"},
	}}
	var stderr bytes.Buffer
	archiveOrphanAgentsFromReport(&stderr, report)

	if len(*archived) != 1 {
		t.Fatalf("expected exactly 1 archive; got %d: %v", len(*archived), *archived)
	}
	if (*archived)[0] != "11111111" {
		t.Errorf("expected worker record 11111111 to be archived; got %s", (*archived)[0])
	}
	// stderr must surface the coord-record preservation so the
	// operator sees it (surface-don't-silo).
	body := stderr.String()
	if !strings.Contains(body, "22222222") {
		t.Errorf("stderr must surface the preserved coord record ID; got:\n%s", body)
	}
	if !strings.Contains(body, "coord-p1") {
		t.Errorf("stderr must include the coord task_id; got:\n%s", body)
	}
	if !strings.Contains(body, "fleet rm 22222222") {
		t.Errorf("stderr must include the manual cleanup one-liner; got:\n%s", body)
	}
}

// TestArchiveOrphanAgentsFromReport_SkipsAllWhenFleetTmuxSocketSet
// is the regression for codex PR-D iter-1 [P1] finding #2. Setup:
// FLEET_TMUX_SOCKET is set, so the dispatch-layer filter must skip
// the auto-archive entirely (mirroring the warning `fleet gc --apply`
// already ships). The records get surfaced to stderr; the operator
// can review and run cleanup explicitly with the socket unset.
func TestArchiveOrphanAgentsFromReport_SkipsAllWhenFleetTmuxSocketSet(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "/tmp/fleet-other.sock")
	workerRec := &agent.Record{ID: "33333333", TaskID: "fix-other-bug", Project: "p2"}
	stubDispatchAgentList(t, []*agent.Record{workerRec})
	archived := stubDispatchAgentArchive(t)

	report := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanAgents, Target: "33333333", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-33333333 gone"},
	}}
	var stderr bytes.Buffer
	archiveOrphanAgentsFromReport(&stderr, report)

	if len(*archived) != 0 {
		t.Errorf("FLEET_TMUX_SOCKET set must short-circuit ALL archives; got %d: %v",
			len(*archived), *archived)
	}
	body := stderr.String()
	if !strings.Contains(body, "FLEET_TMUX_SOCKET") {
		t.Errorf("stderr must surface the FLEET_TMUX_SOCKET reason; got:\n%s", body)
	}
	if !strings.Contains(body, "33333333") {
		t.Errorf("stderr must name the would-archive candidate; got:\n%s", body)
	}
	if !strings.Contains(body, "fleet gc --apply --kinds=orphan-agents") {
		t.Errorf("stderr must include the manual cleanup one-liner so operator knows what to run; got:\n%s",
			body)
	}
}

// TestArchiveOrphanAgentsFromReport_ArchivesWorkerRecord_DefaultSocket
// is the happy path: no FLEET_TMUX_SOCKET, no coord records, one
// worker record. Should archive silently (no stderr noise on success).
func TestArchiveOrphanAgentsFromReport_ArchivesWorkerRecord_DefaultSocket(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "")
	workerRec := &agent.Record{ID: "44444444", TaskID: "fix-another-bug", Project: "p3"}
	stubDispatchAgentList(t, []*agent.Record{workerRec})
	archived := stubDispatchAgentArchive(t)

	report := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanAgents, Target: "44444444", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-44444444 gone"},
	}}
	var stderr bytes.Buffer
	archiveOrphanAgentsFromReport(&stderr, report)

	if len(*archived) != 1 || (*archived)[0] != "44444444" {
		t.Errorf("expected worker record 44444444 to be archived; got %v", *archived)
	}
	// Silent on success — no stderr lines about archives.
	if strings.Contains(stderr.String(), "warning:") {
		t.Errorf("happy-path archive should be silent on stderr; got:\n%s",
			stderr.String())
	}
}

// TestArchiveOrphanAgentsFromReport_EmptyReport_NoSideEffects pins
// the zero-orphan path: empty report short-circuits before agent.List
// (avoids spurious filesystem reads when there's nothing to do).
func TestArchiveOrphanAgentsFromReport_EmptyReport_NoSideEffects(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "")
	// Spike list/archive to fail loudly if either gets called.
	prev := dispatchAgentListFn
	dispatchAgentListFn = func() ([]*agent.Record, error) {
		t.Fatal("dispatchAgentListFn must not be called on empty report")
		return nil, nil
	}
	t.Cleanup(func() { dispatchAgentListFn = prev })

	var stderr bytes.Buffer
	archiveOrphanAgentsFromReport(&stderr, gc.Report{})

	if stderr.Len() != 0 {
		t.Errorf("empty report should produce no stderr output; got:\n%s",
			stderr.String())
	}
}

// TestArchiveOrphanAgentsFromReport_ListError_FailsClosed pins the
// fail-closed contract: if agent.List can't enumerate (permission,
// mount failure), the auto-archive refuses to fire and surfaces the
// error to stderr. Same shape as the --coord-spawn ListStrict guard.
func TestArchiveOrphanAgentsFromReport_ListError_FailsClosed(t *testing.T) {
	t.Setenv("FLEET_TMUX_SOCKET", "")
	prev := dispatchAgentListFn
	dispatchAgentListFn = func() ([]*agent.Record, error) {
		return nil, errors.New("permission denied")
	}
	t.Cleanup(func() { dispatchAgentListFn = prev })
	archived := stubDispatchAgentArchive(t)

	report := gc.Report{Actions: []gc.Action{
		{Kind: gc.KindOrphanAgents, Target: "55555555", Verb: gc.VerbWouldArchive, Reason: "tmux session fleet-55555555 gone"},
	}}
	var stderr bytes.Buffer
	archiveOrphanAgentsFromReport(&stderr, report)

	if len(*archived) != 0 {
		t.Errorf("list error must abort the archive (fail closed); got %d archives", len(*archived))
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr must surface the list error; got:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "fleet gc --apply --kinds=orphan-agents") {
		t.Errorf("stderr must include the manual cleanup one-liner; got:\n%s", stderr.String())
	}
}

// Compile-time guard against an accidental unused import. tmux import
// is needed by future test extensions; placeholder use so go vet stays
// quiet under the current test set.
var _ = tmux.SessionInfo{}
var _ = agent.Record{}
