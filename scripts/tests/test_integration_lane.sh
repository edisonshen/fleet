#!/usr/bin/env bash
# test_integration_lane.sh — fixture-driven test for scripts/integration-lane.sh.
#
# Fixture module: scripts/tests/fixtures/lane (its own go.mod, so the root
# `go test ./...` never sees it):
#   a_test.go              TestDefault_X   (default build)
#   b_integration_test.go  TestOnlyTagged_Y (//go:build integration; fails when
#                                            LANE_FIXTURE_FAIL=1)
#   none/c_test.go         TestDefault_Z   (package with no tagged tests)
#
# Contracts (scenario-testing PR-3 S7–S9):
#   S7  the lane runs the tagged test and NOT the default one
#   S8  a package with no integration-only tests exits 0 with a message and
#       runs nothing
#   S9  a failing tagged test propagates a non-zero exit and its FAIL line
#
# Bash 3.2 portable. Runs via `bash scripts/tests/test_integration_lane.sh`.

set -euo pipefail

SCRIPTS_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LANE="$SCRIPTS_DIR/integration-lane.sh"
FIXTURE="$SCRIPTS_DIR/tests/fixtures/lane"

if [[ ! -f "$LANE" ]]; then
    echo "FAIL: lane script not found at $LANE" >&2
    exit 1
fi
if [[ ! -f "$FIXTURE/go.mod" ]]; then
    echo "FAIL: fixture module not found at $FIXTURE" >&2
    exit 1
fi

passed=0
failed=0

# run_lane <pkg> [extra flags...]; sets $out and $rc, never aborts the suite.
out=""
rc=0
run_lane() {
    set +e
    out="$(cd "$FIXTURE" && bash "$LANE" "$@" 2>&1)"
    rc=$?
    set -e
}

# expect <name> <condition-description> <bash test expression...>
expect() {
    local name="$1"; shift
    local desc="$1"; shift
    if "$@"; then
        passed=$((passed + 1))
        echo "PASS: $name — $desc"
    else
        failed=$((failed + 1))
        echo "FAIL: $name — $desc" >&2
        echo "----- output -----" >&2
        echo "$out" >&2
        echo "----- exit=$rc -----" >&2
    fi
}

contains() { [[ "$1" == *"$2"* ]]; }
not_contains() { [[ "$1" != *"$2"* ]]; }

# S7: tagged runs, default does not. -v so per-test RUN lines are visible.
run_lane . -v
expect S7 "exit 0" test "$rc" -eq 0
expect S7 "tagged test ran" contains "$out" "=== RUN   TestOnlyTagged_Y"
expect S7 "tagged test passed" contains "$out" "--- PASS: TestOnlyTagged_Y"
expect S7 "default test did NOT run" not_contains "$out" "TestDefault_X"

# S8: no integration-only tests -> exit 0, message, nothing run.
run_lane ./none -v
expect S8 "exit 0" test "$rc" -eq 0
expect S8 "explains the empty set" contains "$out" "no integration-only tests in ./none"
expect S8 "no test ran" not_contains "$out" "=== RUN"
expect S8 "no go test summary" not_contains "$out" "ok "

# S9: failing tagged test -> non-zero exit, FAIL line surfaces.
set +e
out="$(cd "$FIXTURE" && LANE_FIXTURE_FAIL=1 bash "$LANE" . -v 2>&1)"
rc=$?
set -e
expect S9 "non-zero exit" test "$rc" -ne 0
expect S9 "names the failing tagged test" contains "$out" "--- FAIL: TestOnlyTagged_Y"
expect S9 "default test still did NOT run" not_contains "$out" "TestDefault_X"

echo
echo "passed=$passed failed=$failed"
[[ "$failed" -eq 0 ]]
