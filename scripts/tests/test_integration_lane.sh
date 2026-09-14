#!/usr/bin/env bash
# Tests scripts/integration-lane.sh against the fixture module
# scripts/tests/fixtures/lane (own go.mod, invisible to the root `go test ./...`):
#   a_test.go              TestDefault_X    default build only
#   b_integration_test.go  TestOnlyTagged_Y //go:build integration; fails when
#                                           LANE_FIXTURE_FAIL=1
#   none/c_test.go         TestDefault_Z    package with no tagged tests
#
# Checks: tagged test runs and default does not; empty set exits 0 with a
# message and runs nothing; a failing tagged test exits non-zero with its FAIL
# line. Bash 3.2 portable.

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

# -v so per-test RUN lines are visible.
run_lane . -v
expect tagged-only "exit 0" test "$rc" -eq 0
expect tagged-only "tagged test ran" contains "$out" "=== RUN   TestOnlyTagged_Y"
expect tagged-only "tagged test passed" contains "$out" "--- PASS: TestOnlyTagged_Y"
expect tagged-only "default test did NOT run" not_contains "$out" "TestDefault_X"

run_lane ./none -v
expect empty-set "exit 0" test "$rc" -eq 0
expect empty-set "explains the empty set" contains "$out" "no integration-only tests in ./none"
expect empty-set "no test ran" not_contains "$out" "=== RUN"
expect empty-set "no go test summary" not_contains "$out" "ok "

set +e
out="$(cd "$FIXTURE" && LANE_FIXTURE_FAIL=1 bash "$LANE" . -v 2>&1)"
rc=$?
set -e
expect tagged-fail "non-zero exit" test "$rc" -ne 0
expect tagged-fail "names the failing tagged test" contains "$out" "--- FAIL: TestOnlyTagged_Y"
expect tagged-fail "default test still did NOT run" not_contains "$out" "TestDefault_X"

echo
echo "passed=$passed failed=$failed"
[[ "$failed" -eq 0 ]]
