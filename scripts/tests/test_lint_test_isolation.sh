#!/usr/bin/env bash
# test_lint_test_isolation.sh — fixture-driven test for the
# scripts/lint-test-isolation.sh check.
#
# Covers two contracts:
#  1. (existing) tmux.Spawn / spawn.Spawn / runDispatch without an isolation
#     marker is flagged.
#  2. (new — leak-test-spawn-stub) a `&dispatchOpts{...}` literal that
#     reaches runDispatch without `commandExplicit: true` is flagged so
#     empty-command dispatch tests can't substitute the real claude wrapper
#     and fork live detached procs (DESIGN-lifecycle-leak-recurrence.md
#     PR-A, root cause #1). Already-stubbed callsites (command: + commandExplicit: true)
#     pass.
#
# Test layout: build a sandbox repo under a temp dir, drop minimal *_test.go
# fixtures, run the lint script there, assert exit code + stderr substrings.
#
# Bash 3.2 portable. Self-contained; runs via `bash scripts/tests/test_lint_test_isolation.sh`.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LINT="$SCRIPT_DIR/lint-test-isolation.sh"

if [[ ! -x "$LINT" ]] && [[ ! -f "$LINT" ]]; then
    echo "FAIL: lint script not found at $LINT" >&2
    exit 1
fi

passed=0
failed=0

# run_case <name> <expected-exit> <expected-stderr-substring> <fixture-body>
# Sets up a sandbox repo with a single *_test.go fixture, runs the lint
# script, and asserts on exit code + stderr.
run_case() {
    local name="$1"
    local want_exit="$2"
    local want_stderr="$3"
    local fixture="$4"

    local tmp
    tmp="$(mktemp -d -t fleet-lint-test-XXXXXX)"
    # Lint runs `git ls-files '*_test.go'` if inside a repo. Init a tiny
    # repo so the script's git path fires; otherwise it falls back to
    # `find`. Either path works — git is the production path.
    (
        cd "$tmp"
        git init -q 2>/dev/null || true
        git config user.email t@example.com 2>/dev/null || true
        git config user.name t 2>/dev/null || true
        mkdir -p sub
        printf '%s\n' "$fixture" > sub/example_test.go
        git add -A 2>/dev/null || true
    ) >/dev/null 2>&1

    # The lint script computes repo_root via $(dirname "$0")/.. relative
    # to its own path. We need it to operate on $tmp, so symlink the script
    # into $tmp/scripts/ and run from there. Equivalent: copy the script.
    mkdir -p "$tmp/scripts"
    cp "$LINT" "$tmp/scripts/lint-test-isolation.sh"

    local out_file
    out_file="$(mktemp -t fleet-lint-test-out-XXXXXX)"
    local got_exit=0
    bash "$tmp/scripts/lint-test-isolation.sh" >"$out_file" 2>&1 || got_exit=$?

    local ok=1
    if [[ "$got_exit" != "$want_exit" ]]; then
        ok=0
        echo "FAIL [$name]: exit code = $got_exit, want $want_exit" >&2
        echo "  fixture:" >&2
        echo "$fixture" | sed 's/^/    /' >&2
        echo "  output:" >&2
        sed 's/^/    /' "$out_file" >&2
    fi
    if [[ -n "$want_stderr" ]]; then
        if ! grep -q -F -- "$want_stderr" "$out_file"; then
            ok=0
            echo "FAIL [$name]: missing stderr substring '$want_stderr'" >&2
            echo "  output:" >&2
            sed 's/^/    /' "$out_file" >&2
        fi
    fi
    if [[ "$ok" == "1" ]]; then
        echo "PASS [$name]"
        passed=$((passed + 1))
    else
        failed=$((failed + 1))
    fi

    rm -rf "$tmp" "$out_file"
}

# --- Case A: existing contract — runDispatch without isolation marker
#     must still be flagged (regression guard for the original lint).
run_case "isolation-marker-missing" 1 "test-isolation-lint: FAIL" \
'package sub

import "testing"

func TestNoIsolation(t *testing.T) {
    _ = runDispatch(nil, nil)
}
'

# --- Case B: existing contract — runDispatch WITH isolation marker passes.
run_case "isolation-marker-present" 0 "0 violations" \
'package sub

import "testing"

func TestWithIsolation(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case C (NEW): empty-command &dispatchOpts{} reaching runDispatch
#     without commandExplicit: true must be flagged. This is the leak-
#     test-spawn-stub guard.
run_case "empty-command-dispatch-opts" 1 "dispatchOpts" \
'package sub

import "testing"

func TestEmptyCommand(t *testing.T) {
    tmuxtest.RequireTmux(t)
    opts := &dispatchOpts{taskID: "demo", project: "default"}
    _ = runDispatch(opts, nil)
}
'

# --- Case D (NEW): commandExplicit: true on the dispatchOpts passes.
run_case "stubbed-command-dispatch-opts" 0 "0 violations" \
'package sub

import "testing"

func TestStubbedCommand(t *testing.T) {
    tmuxtest.RequireTmux(t)
    opts := &dispatchOpts{
        taskID:          "demo",
        project:         "default",
        command:         []string{"sleep", "30"},
        commandExplicit: true,
    }
    _ = runDispatch(opts, nil)
}
'

# --- Case E (NEW — codex review iter-1 [P2], 2026-06-04): helper-wrapped
#     runDispatch (currently the runDispatchIgnoringSpawnErr pattern in
#     cmd/fleet/dispatch_rc_auto_marker_test.go) must ALSO be flagged
#     when the dispatchOpts is empty-command. The earlier guard only
#     matched the literal `runDispatch(` substring and missed wrappers.
run_case "empty-command-wrapper-helper-dispatch" 1 "dispatchOpts" \
'package sub

import "testing"

func TestEmptyCommandViaHelper(t *testing.T) {
    tmuxtest.RequireTmux(t)
    opts := &dispatchOpts{taskID: "demo", project: "default"}
    runDispatchIgnoringSpawnErr(t, opts)
}
'

# --- Case F (NEW): helper-wrapped dispatch WITH commandExplicit: true passes.
run_case "stubbed-command-wrapper-helper-dispatch" 0 "0 violations" \
'package sub

import "testing"

func TestStubbedCommandViaHelper(t *testing.T) {
    tmuxtest.RequireTmux(t)
    opts := &dispatchOpts{
        taskID:          "demo",
        project:         "default",
        command:         []string{"sleep", "30"},
        commandExplicit: true,
    }
    runDispatchIgnoringSpawnErr(t, opts)
}
'

echo ""
echo "test_lint_test_isolation: $passed passed, $failed failed"
if [[ "$failed" -gt 0 ]]; then
    exit 1
fi
exit 0
