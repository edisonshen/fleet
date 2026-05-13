// Package enginecfg resolves the spawn-time invocation for a named
// engine (currently "claude-code" or "codex"). v0.9 ships hard-coded
// defaults; a future ~/.fleet/config.yaml override is anticipated by
// the schema docs (docs/STATE.md / docs/DESIGN.md "engines map") but
// not wired here yet — operators override per-spawn via
// `fleet dispatch --command ...` until the file path lands.
//
// The package intentionally avoids a Go `Engine` interface. Per memory
// `project_v11_engine_adapter.md`: three lines of `if engine == "..."`
// beats a premature abstraction. The two engines differ only in the
// binary they launch; the wrapper shape (sh -c with survive-clean-exit
// semantics) is shared.
//
// Default invocations:
//   - claude-code: `claude --dangerously-skip-permissions` (matches
//     spawn.DefaultClaudeInvocation; v0 default).
//   - codex:       `codex` (no flag — codex has no equivalent of
//     --dangerously-skip-permissions and the operator picks engine at
//     coord-session start, not per-task).
package enginecfg

import (
	"fmt"
)

// EngineClaudeCode is the canonical engine name for Claude Code. v0
// always wrote this string to agents/<id>.json:engine; v0.9+ writes
// EngineCodex when `fleet -codex` is used.
const EngineClaudeCode = "claude-code"

// EngineCodex is the engine name for the OpenAI Codex CLI.
const EngineCodex = "codex"

// DefaultEngine is the engine selected when no CLI flag is passed.
// Codex is opt-in per operator request — the existing fleet population
// runs claude-code, so changing the default would surprise everyone.
const DefaultEngine = EngineClaudeCode

// Invocation is the bare-binary-plus-flags string that the dispatch
// shell wrapper inlines as the first command, plus the short binary
// name used in the wrapper's "exited"/"rerun" status banners.
//
// Kept as a string (not []string) because the wrapper template is a
// single shell line — splitting would require re-joining at template
// expansion time. The argv that tmux executes IS shell-tokenised:
//
//	[]string{"sh", "-c", "<invocation>; RC=$?; if ..."}
//
// so embedded quotes inside Invocation would need shell-escaping. We
// keep Invocation simple (binary name + flat flags, no quoted args) for
// the two known engines. Custom engines that need quoted args set
// `fleet dispatch --command` directly and bypass this package.
type Invocation struct {
	// Name is the engine identifier persisted on agent.Record.Engine.
	Name string
	// Cmd is the binary-plus-flags string inlined into the survive-
	// clean-exit shell wrapper.
	Cmd string
	// Binary is the bare binary name (no flags) used in the wrapper's
	// banner text. claude-code uses "claude" (NOT "claude
	// --dangerously-skip-permissions") so the banner matches the
	// historical wrapper byte-for-byte; codex uses "codex".
	Binary string
}

// engineDefaults is the registry of built-in engine invocations. v0.9
// hardcodes two entries; the future config.yaml override layer will
// merge over this map.
//
// The values are intentionally small — no array, no aliases — because
// the wrapper template only consumes the Cmd string.
var engineDefaults = map[string]Invocation{
	EngineClaudeCode: {
		Name: EngineClaudeCode,
		// Cmd matches spawn.DefaultClaudeInvocation; the literal lives
		// there because the dispatch flag default + the remote-control
		// rewriter both reference it. A unit test pins the equality.
		// Binary intentionally drops the flag so the wrapper banner
		// reads "[fleet] claude exited code …" (matches the legacy
		// DefaultClaudeWrapperScript byte-for-byte).
		Cmd:    "claude --dangerously-skip-permissions",
		Binary: "claude",
	},
	EngineCodex: {
		Name:   EngineCodex,
		Cmd:    "codex",
		Binary: "codex",
	},
}

// Resolve returns the Invocation registered for name. Unknown names
// produce an error; the dispatch CLI surfaces it to the operator
// (typo at `fleet --engine xyz`).
//
// Accepts the empty string as a synonym for DefaultEngine so callers
// that haven't been migrated to the flag-default branch still resolve
// to claude-code (forward-compat with legacy agent records).
func Resolve(name string) (Invocation, error) {
	if name == "" {
		name = DefaultEngine
	}
	inv, ok := engineDefaults[name]
	if !ok {
		return Invocation{}, fmt.Errorf(
			"unknown engine %q (known: claude-code, codex)", name)
	}
	return inv, nil
}

// WrapperScript returns the survive-clean-exit shell-wrapper body for
// the given invocation. Mirrors the template historically inlined in
// spawn.DefaultClaudeWrapperScript:
//
//	<cmd>; RC=$?; if [ "$RC" -ne 0 ]; then echo;
//	echo "[fleet] <binary> exited code $RC — session terminating";
//	exit "$RC"; fi; echo;
//	echo "[fleet] <binary> exited cleanly — rerun <cmd> or Ctrl-b then & to kill ...";
//	exec ${SHELL:-bash} -i
//
// inv.Binary is used for the banner text; inv.Cmd is what's actually
// executed and reproduced in the "rerun" hint. claude-code's
// invocation matches the historical spawn.DefaultClaudeWrapperScript
// byte-for-byte (pinned by a unit test).
func WrapperScript(inv Invocation) string {
	return inv.Cmd +
		`; RC=$?; if [ "$RC" -ne 0 ]; then echo; echo "[fleet] ` + inv.Binary +
		` exited code $RC — session terminating"; exit "$RC"; fi; echo; echo "[fleet] ` +
		inv.Binary + ` exited cleanly — rerun ` + inv.Cmd +
		` or Ctrl-b then & to kill this session"; exec ${SHELL:-bash} -i`
}

// BuildWrapperCommand returns the argv tmux executes for the given
// engine. The shape is `sh -c "<wrapper-body>"` — the dispatch caller
// (spawn.Options.Command) gets this verbatim.
//
// Unknown engine name returns an error.
func BuildWrapperCommand(name string) ([]string, error) {
	inv, err := Resolve(name)
	if err != nil {
		return nil, err
	}
	return []string{"sh", "-c", WrapperScript(inv)}, nil
}

// Known returns true iff name is a registered engine.
func Known(name string) bool {
	_, ok := engineDefaults[name]
	return ok
}
