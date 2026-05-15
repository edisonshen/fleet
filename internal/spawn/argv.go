package spawn

import "strings"

// DefaultClaudeInvocation is the literal claude argv inside the shell
// wrapper that --command defaults to. Historical: kept exported because
// other call sites grep for it. The relaxed matcher in
// InjectRemoteControlFlag no longer requires byte-equality on the full
// wrapper, so this constant is now informational rather than load-bearing.
const DefaultClaudeInvocation = "claude --dangerously-skip-permissions"

// DefaultClaudeWrapperScript is the EXACT literal of the default
// --command's third element (the shell script body). The dispatch
// surface registers this string as the cobra flag default and the
// `TestDefaultClaudeWrapperScript_MatchesFlagDefault` test pins drift.
//
// Lifted from cmd/fleet so internal/handoffop (the auto-handoff drain)
// could rewrite replacement-spawn argvs without an import cycle through
// cmd/fleet.
//
// Note: as of the wrapper-pattern relaxation
// (handoff-remote-control-shell-wrapper-fix), InjectRemoteControlFlag
// no longer requires byte-equality with this constant. Any
// `["sh", "-c", "claude ..."]` shape is rewritten. The constant is
// still consumed by the dispatch flag default and by tests that pin the
// default --command's exact bytes.
const DefaultClaudeWrapperScript = `claude --dangerously-skip-permissions; RC=$?; if [ "$RC" -ne 0 ]; then echo; echo "[fleet] claude exited code $RC — session terminating"; exit "$RC"; fi; echo; echo "[fleet] claude exited cleanly — rerun claude --dangerously-skip-permissions or Ctrl-b then & to kill this session"; exec ${SHELL:-bash} -i`

// claudeToken is the binary-name-plus-space prefix the relaxed wrapper
// matcher requires. The trailing space is load-bearing: it prevents
// false positives like `claudefoo` (a different binary that
// incidentally starts with the letters c-l-a-u-d-e). See
// TestInjectRemoteControlFlag_NonClaudeShellWrapper/claude-prefix-but-different-binary.
const claudeToken = "claude "

// InjectRemoteControlFlag rewrites a shell-wrapped claude command to
// include `--remote-control "<sessionName>"` so the spawned Claude
// Code session auto-attaches to the remote-control daemon at startup.
//
// Wrapper-pattern matcher (handoff-remote-control-shell-wrapper-fix):
// matches ANY `["sh", "-c", "<body>"]` whose body begins (after
// optional leading whitespace) with the `claude ` token. Returns the
// slice unchanged otherwise.
//
//	["sh", "-c", "claude --dangerously-skip-permissions; ..."]
//	  ↓ match: argv[0]=="sh" && argv[1]=="-c" && body starts with "claude "
//	["sh", "-c", "claude --remote-control \"<name>\" --dangerously-skip-permissions; ..."]
//
// The flag is positioned IMMEDIATELY after the `claude` token so claude's
// own flag parser sees it adjacent to the binary (not buried after
// trailing flags that may consume it as a positional value).
//
// Why a relaxed matcher? The previous strict byte-equality on
// DefaultClaudeWrapperScript silently disabled injection whenever the
// persisted Command body drifted from the literal default — older
// fleet release wrapper text, manual operator edits, or future wrapper
// variants would all skip remote-control injection. The forensic case:
// coord ca7eb43e lost remote control after a manual handoff because
// the persisted command body diverged from the literal default by
// enough characters to break byte-equality. Matching on the
// `["sh", "-c", "claude ..."]` SHAPE (not the wrapper text) catches
// every variant that genuinely launches claude.
//
// Negative contract (returned unchanged):
//   - argv[0] != "sh" (e.g. `bash -c`, direct argv).
//   - argv[1] != "-c".
//   - len(argv) != 3.
//   - body doesn't start with `claude ` after optional whitespace
//     (e.g. `python3 ...`, `echo ...; claude ...`, `claudefoo ...`).
//   - empty/nil argv.
//
// Source: Claude Code remote-control CLI flag, documented at
// https://code.claude.com/docs/en/remote-control.md (issue #73).
//
// Used by THREE call sites — dispatch (coord-spawn), operator-triggered
// handoff replacement, and internal/handoffop (auto-handoff drain) —
// each picking its own session-name prefix (`fleet-coord` /
// `fleet-handoff`).
func InjectRemoteControlFlag(command []string, sessionName string) []string {
	if len(command) != 3 || command[0] != "sh" || command[1] != "-c" {
		return command
	}
	body := command[2]
	// Skip leading whitespace so an indented body still matches. We
	// keep the original prefix in the rewritten body so the rest of
	// the script is byte-preserved (whitespace included).
	idx := 0
	for idx < len(body) && (body[idx] == ' ' || body[idx] == '\t') {
		idx++
	}
	rest := body[idx:]
	if !strings.HasPrefix(rest, claudeToken) {
		return command
	}
	// Inject `--remote-control "<sessionName>" ` immediately after the
	// `claude ` token. We slice rather than call strings.Replace so the
	// rewrite is anchored at this single position — a body that
	// mentions `claude ` later (e.g. inside a "rerun claude ..." banner)
	// is preserved verbatim. This differs from the prior implementation
	// which used ReplaceAll specifically because the legacy wrapper had
	// the literal twice; with the relaxed matcher we no longer assume
	// the wrapper shape, so anchored insertion is the correct tool.
	//
	// sessionName is fleet-controlled (e.g. "fleet-coord-<hex-id>",
	// "fleet-handoff-<hex-id>"); double-quoting is defensive. Embedded
	// double-quotes / shell metas in sessionName are not expected, but
	// shellEscape would be cleaner if that contract ever loosens.
	prefix := body[:idx+len(claudeToken)] // includes leading WS + "claude "
	suffix := body[idx+len(claudeToken):] // everything after "claude "
	rewritten := prefix + `--remote-control "` + sessionName + `" ` + suffix
	out := make([]string, len(command))
	copy(out, command)
	out[2] = rewritten
	return out
}

// CoordRemoteControlSessionName returns the canonical
// --remote-control session name for a coord-spawn dispatch. Format:
// `fleet-coord-<id>-<project>` (suffix-extension over the legacy
// `fleet-coord-<id>` shape).
//
// Why suffix-extension and not prefix-replacement: the
// pidresolver's disambiguator (internal/spawn/pidresolver.go's
// pidResolveDisambiguator) builds `needle = "fleet-coord-" + agentID`
// and does substring containment against argv. The new shape remains
// a strict superstring of the legacy needle, so the resolver works
// for both legacy in-flight coords and new ones without code change.
//
// Same reasoning applies to the Python skill's argv matcher in
// skills/fleet-guard/health.py:_resolve_self_pid which builds
// `needle = f"fleet-coord-{agent_id}"`.
//
// Empty project falls back to the legacy `fleet-coord-<id>` shape
// so legacy records (created before agent.Record carried project)
// and recovery paths that haven't populated project yet still get a
// well-formed name. Production dispatch always passes a non-empty
// project (state.ValidateProjectName rejects empty at the dispatch
// boundary).
func CoordRemoteControlSessionName(agentID, project string) string {
	const prefix = "fleet-coord"
	if project == "" {
		return prefix + "-" + agentID
	}
	return prefix + "-" + agentID + "-" + project
}

// HandoffRemoteControlSessionName returns the canonical
// --remote-control session name for a handoff replacement spawn.
// Format: `fleet-handoff-<id>-<project>`. See
// CoordRemoteControlSessionName for the suffix-extension rationale.
func HandoffRemoteControlSessionName(agentID, project string) string {
	const prefix = "fleet-handoff"
	if project == "" {
		return prefix + "-" + agentID
	}
	return prefix + "-" + agentID + "-" + project
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
