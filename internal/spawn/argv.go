package spawn

import "strings"

// DefaultClaudeInvocation is the literal claude argv inside the shell
// wrapper that --command defaults to. We rewrite this exact substring
// when injecting --remote-control so an operator-supplied --command
// (scripted pipeline, alt engine) is left untouched. See
// InjectRemoteControlFlag.
const DefaultClaudeInvocation = "claude --dangerously-skip-permissions"

// DefaultClaudeWrapperScript is the EXACT literal of the default
// --command's third element (the shell script body). InjectRemoteControlFlag
// matches on this byte-equal string before rewriting, so a custom
// `--command sh -c '<arbitrary script that mentions claude>'` is NOT
// silently mutated (codex review #73 iter-3 P2). Must stay byte-equal
// with cmd/fleet/dispatch.go's --command default; a regression test
// pins the equality.
//
// Lifted from cmd/fleet during the fix/remote-control-coord-injection
// work so internal/handoffop (the auto-handoff drain) can rewrite
// replacement-spawn argvs without an import cycle through cmd/fleet.
const DefaultClaudeWrapperScript = `claude --dangerously-skip-permissions; RC=$?; if [ "$RC" -ne 0 ]; then echo; echo "[fleet] claude exited code $RC — session terminating"; exit "$RC"; fi; echo; echo "[fleet] claude exited cleanly — rerun claude --dangerously-skip-permissions or Ctrl-b then & to kill this session"; exec ${SHELL:-bash} -i`

// InjectRemoteControlFlag rewrites a shell-wrapped claude command to
// include `--remote-control "<sessionName>"` so the spawned Claude
// Code session auto-attaches to the remote-control daemon at startup.
// Returns the slice unchanged if the command does NOT match the
// documented default shape (custom --command argvs are out of scope —
// fleet doesn't know their flag conventions).
//
// Default shape from cmd/fleet/dispatch.go's --command default:
//
//	["sh", "-c", "claude --dangerously-skip-permissions; RC=$?; ..."]
//
// We replace the literal "claude --dangerously-skip-permissions" with
// "claude --dangerously-skip-permissions --remote-control \"<name>\""
// inside the shell-script element. The trailing arguments (`; RC=$?;
// ...`) are preserved so the wrapper's clean-exit semantics still
// apply.
//
// Source: Claude Code remote-control CLI flag, documented at
// https://code.claude.com/docs/en/remote-control.md (issue #73 research
// finding).
//
// Used by THREE call sites — dispatch (coord-spawn), operator-triggered
// handoff replacement, and internal/handoffop (auto-handoff drain) —
// each picking its own session-name prefix (`fleet-coord` / `fleet-handoff`).
func InjectRemoteControlFlag(command []string, sessionName string) []string {
	// Strict shape match: ONLY rewrite Fleet's documented default
	// `--command` (the literal shell-wrapped claude invocation
	// registered by newDispatchCmd). Custom operator-supplied
	// commands — even shell-wrapped ones that incidentally mention
	// `claude --dangerously-skip-permissions` — are returned
	// untouched. A loose `Contains` match risked rewriting arbitrary
	// shell text inside a custom launcher (codex review #73 iter-3 P2).
	if len(command) != 3 || command[0] != "sh" || command[1] != "-c" {
		return command
	}
	script := command[2]
	if script != DefaultClaudeWrapperScript {
		return command
	}
	// Inject the flag IMMEDIATELY after the matched substring so the
	// rest of the script (`; RC=$?; ...`) keeps its position. Quoting
	// the session name with double quotes mirrors how the documented
	// CLI usage is written; sessionName is hex (agent ID) plus the
	// "fleet-" prefix so quoting is mostly defensive (no spaces, no
	// shell metas) but cheap to keep.
	//
	// ReplaceAll (not Replace n=1) is deliberate: the default wrapper
	// contains the literal `claude --dangerously-skip-permissions`
	// TWICE — once as the launch command, once inside the
	// "[fleet] claude exited cleanly — rerun ..." banner that prints
	// when claude exits 0. Rewriting only the first occurrence would
	// leave the banner suggesting a rerun command without remote-
	// control, so an operator who follows the banner restarts WITHOUT
	// auto-attach (codex review #73 iter-1 P3). Replacing both keeps
	// the banner accurate.
	replaced := strings.ReplaceAll(
		script,
		DefaultClaudeInvocation,
		DefaultClaudeInvocation+` --remote-control "`+sessionName+`"`,
	)
	out := make([]string, len(command))
	copy(out, command)
	out[2] = replaced
	return out
}

// SameCommand returns true iff the two argvs are identical
// element-for-element. Used by the dispatch + handoff coord/handoff
// spawn paths to detect when InjectRemoteControlFlag returned an
// unchanged slice (operator-overridden custom --command), so we don't
// pollute Options.ExecCommand with a no-op duplicate of Command.
func SameCommand(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
