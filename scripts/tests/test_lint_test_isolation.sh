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
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
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
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
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
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    opts := &dispatchOpts{
        taskID:          "demo",
        project:         "default",
        command:         []string{"sleep", "30"},
        commandExplicit: true,
    }
    runDispatchIgnoringSpawnErr(t, opts)
}
'

# --- Case G (NEW — codex review iter-3 [P2], 2026-06-04): per-literal
#     tracking — an earlier safe dispatchOpts literal must NOT mask a
#     later unsafe literal that is the one actually dispatched. The
#     earlier function-wide aggregation said this passed; the new
#     per-literal tracking correctly flags it.
run_case "mixed-safe-then-unsafe-dispatch-opts" 1 "dispatchOpts" \
'package sub

import "testing"

func TestMixedSafeThenUnsafe(t *testing.T) {
    tmuxtest.RequireTmux(t)
    safe := &dispatchOpts{
        taskID:          "demo",
        command:         []string{"sleep", "30"},
        commandExplicit: true,
    }
    _ = safe
    unsafe := &dispatchOpts{taskID: "demo2"}
    _ = runDispatch(unsafe, nil)
}
'

# --- Case H (NEW): two safe dispatchOpts literals BOTH pass.
run_case "both-safe-dispatch-opts" 0 "0 violations" \
'package sub

import "testing"

func TestBothSafe(t *testing.T) {
    tmuxtest.RequireTmux(t)
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    a := &dispatchOpts{
        taskID:          "a",
        commandExplicit: true,
    }
    b := &dispatchOpts{
        taskID:          "b",
        commandExplicit: true,
    }
    _ = runDispatch(a, nil)
    _ = runDispatch(b, nil)
}
'

# ===================================================================
# Scan-dir isolation rule (rule #3, ci-perf-pr2). The reconcile
# OrphanTmux pass walks FLEET_GC_SCAN_DIR; a test that reaches it via
# runDispatch / runStatus / gc.Reconcile without scoping that dir would
# grind real /tmp (the PR #232 hang). These cases use the THREE shapes
# the task plan calls out — tmux-only, scandir-only, both — plus the
# package-wide TestMain decoy exemption.
# ===================================================================

# --- Case I: tmux-isolated but scan-dir NOT isolated → flagged by rule #3.
#     Proves the two seams are orthogonal: a tmux marker alone is not
#     enough when the test reaches the scan dir.
run_case "scandir-tmux-only-missing-scandir" 1 "scan-dir isolation" \
'package sub

import "testing"

func TestTmuxOnlyNoScanDir(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runStatus(nil, nil, nil, "")
}
'

# --- Case I2 (codex review P2): a ONE-LINE test whose body opens AND
#     closes braces on the declaration line must still be flagged. The
#     earlier brace guard dropped it (no emit on the declaration line).
run_case "scandir-one-line-test-flagged" 1 "scan-dir isolation" \
'package sub

import "testing"

func TestOneLine(t *testing.T) { _ = runStatus(nil, nil, nil, "") }
'

# --- Case J: scan-dir isolated via t.Setenv but the test does NOT touch
#     the tmux seam → passes BOTH rules (only a scan-dir trigger present).
run_case "scandir-only-isolated" 0 "0 violations" \
'package sub

import "testing"

func TestScanDirOnly(t *testing.T) {
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    _ = runStatus(nil, nil, nil, "")
}
'

# --- Case K: needs BOTH markers — a test that reaches tmux.Spawn AND the
#     scan dir. Missing the scan-dir marker → flagged by rule #3 even
#     though the tmux marker is present.
run_case "scandir-both-needed-missing-scandir" 1 "scan-dir isolation" \
'package sub

import "testing"

func TestNeedsBothMissingScanDir(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case L: needs BOTH markers and HAS both → passes.
run_case "scandir-both-present" 0 "0 violations" \
'package sub

import "testing"

func TestNeedsBothHasBoth(t *testing.T) {
    tmuxtest.RequireTmux(t)
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    _ = runDispatch(nil, nil)
}
'

# --- Case M: scandir-exempt comment lets a deliberate real-/tmp scan pass.
run_case "scandir-exempt-comment" 0 "0 violations" \
'package sub

import "testing"

func TestScanDirExempt(t *testing.T) {
    tmuxtest.RequireTmux(t)
    // lint-test-isolation:scandir-exempt
    _ = runDispatch(nil, nil)
}
'

# --- Case M2 (codex iter-4 [P2]): a scan-dir trigger hidden behind a
#     helper must still be flagged. The test only calls seedDispatched(t);
#     the helper reaches runDispatch without scoping FLEET_GC_SCAN_DIR.
run_case "scandir-helper-wrapped-trigger-flagged" 1 "scan-dir isolation" \
'package sub

import "testing"

func seedDispatched(t *testing.T) {
    _ = runDispatch(nil, nil)
}

func TestViaHelperNoScanDir(t *testing.T) {
    tmuxtest.RequireTmux(t)
    seedDispatched(t)
}
'

# --- Case M3: helper-wrapped scan-dir trigger WITH the test scoping the
#     scan dir passes (the test isolates even though the trigger is in the
#     helper).
run_case "scandir-helper-wrapped-trigger-isolated" 0 "0 violations" \
'package sub

import "testing"

func seedDispatched2(t *testing.T) {
    _ = runDispatch(nil, nil)
}

func TestViaHelperWithScanDir(t *testing.T) {
    tmuxtest.RequireTmux(t)
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    seedDispatched2(t)
}
'

# --- Case N: package-wide TestMain decoy exempts every test in the
#     package from rule #3 (the cmd/fleet shape). The fixture splits the
#     TestMain decoy and the dispatch test across two files in the SAME
#     package dir so the package-exempt detection (file_lister-wide) fires.
run_case_two_files() {
    local name="$1" want_exit="$2" want_stderr="$3" f1="$4" f2="$5"
    local tmp; tmp="$(mktemp -d -t fleet-lint-test-XXXXXX)"
    (
        cd "$tmp"
        git init -q 2>/dev/null || true
        git config user.email t@example.com 2>/dev/null || true
        git config user.name t 2>/dev/null || true
        mkdir -p sub
        printf '%s\n' "$f1" > sub/main_test.go
        printf '%s\n' "$f2" > sub/dispatch_test.go
        git add -A 2>/dev/null || true
    ) >/dev/null 2>&1
    mkdir -p "$tmp/scripts"
    cp "$LINT" "$tmp/scripts/lint-test-isolation.sh"
    local out_file; out_file="$(mktemp -t fleet-lint-test-out-XXXXXX)"
    local got_exit=0
    bash "$tmp/scripts/lint-test-isolation.sh" >"$out_file" 2>&1 || got_exit=$?
    local ok=1
    if [[ "$got_exit" != "$want_exit" ]]; then
        ok=0; echo "FAIL [$name]: exit = $got_exit, want $want_exit" >&2
        sed 's/^/    /' "$out_file" >&2
    fi
    if [[ -n "$want_stderr" ]] && ! grep -q -F -- "$want_stderr" "$out_file"; then
        ok=0; echo "FAIL [$name]: missing stderr '$want_stderr'" >&2
        sed 's/^/    /' "$out_file" >&2
    fi
    if [[ "$ok" == "1" ]]; then echo "PASS [$name]"; passed=$((passed + 1));
    else failed=$((failed + 1)); fi
    rm -rf "$tmp" "$out_file"
}

run_case_two_files "scandir-package-testmain-decoy-exempts" 0 "0 violations" \
'package sub

import (
    "os"
    "testing"
)

func TestMain(m *testing.M) {
    _ = os.Setenv("FLEET_GC_SCAN_DIR", "/tmp/decoy")
    os.Exit(m.Run())
}
' \
'package sub

import "testing"

func TestDispatchInDecoyedPackage(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# ===================================================================
# Fake-tmux marker vs subprocess triggers (codex iter-6 [P2]). The
# in-process fake only rebinds pointers in the PARENT; a subprocess
# trigger (runFleet/runTick/runTickCap) forks a child `fleet` that
# escapes the fake and can hit the real socket. So a fake marker must
# NOT attest isolation for a subprocess-trigger test.
# ===================================================================

# --- Case O: fake marker + IN-PROCESS trigger (runDispatch) → passes.
run_case "fake-marker-inprocess-trigger-ok" 0 "0 violations" \
'package sub

import "testing"

func TestFakeInProcess(t *testing.T) {
    requireFakeTmux(t)
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    _ = runDispatch(nil, nil)
}
'

# --- Case P: fake marker + SUBPROCESS trigger (runFleet) → FLAGGED.
#     The child fleet binary escapes the parent-only fake; needs a real
#     socket marker.
run_case "fake-marker-subprocess-trigger-flagged" 1 "test-isolation-lint: FAIL" \
'package sub

import "testing"

func TestFakeSubprocess(t *testing.T) {
    requireFakeTmux(t)
    _ = runFleet(t, "status")
}
'

# --- Case Q: REAL marker + SUBPROCESS trigger → passes (real socket
#     isolation propagates to the child via FLEET_TMUX_SOCKET).
run_case "real-marker-subprocess-trigger-ok" 0 "0 violations" \
'package sub

import "testing"

func TestRealSubprocess(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runFleet(t, "status")
}
'

# --- Case R: tmuxfake.InstallFake (direct) + subprocess trigger → FLAGGED.
run_case "installfake-direct-subprocess-trigger-flagged" 1 "test-isolation-lint: FAIL" \
'package sub

import "testing"

func TestInstallFakeSubprocess(t *testing.T) {
    tmuxfake.InstallFake(t)
    _ = runTickCap(t)
}
'

# --- Case S (codex iter-7 [P2]): a subprocess trigger wrapped in a helper
#     must STILL defeat the fake marker. Test calls requireFakeTmux + a
#     helper that runs runFleet; the child escapes the fake → FLAGGED.
run_case "fake-marker-subprocess-helper-flagged" 1 "test-isolation-lint: FAIL" \
'package sub

import "testing"

func runFleetViaHelper(t *testing.T) {
    _ = runFleet(t, "status")
}

func TestFakeSubprocessHelper(t *testing.T) {
    requireFakeTmux(t)
    runFleetViaHelper(t)
}
'

# --- Case T (codex iter-7 [P2]): a TestMain that only MENTIONS
#     FLEET_GC_SCAN_DIR in a comment (no setter) must NOT exempt the
#     package — a dispatch test in it without scan-dir isolation is FLAGGED.
run_case_two_files "scandir-testmain-comment-only-not-exempt" 1 "scan-dir isolation" \
'package sub

import (
    "os"
    "testing"
)

// TestMain here does NOT set FLEET_GC_SCAN_DIR (only names it in this comment).
func TestMain(m *testing.M) {
    os.Exit(m.Run())
}
' \
'package sub

import "testing"

func TestDispatchNoRealDecoy(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case U (codex iter-7 [P2]): a COMMENTED-OUT scan-dir marker must NOT
#     attest isolation. The body calls runDispatch with the real marker
#     only inside a // comment → FLAGGED.
run_case "scandir-commented-marker-not-isolation" 1 "scan-dir isolation" \
'package sub

import "testing"

func TestCommentedScanDirMarker(t *testing.T) {
    tmuxtest.RequireTmux(t)
    // TODO: add t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    _ = runDispatch(nil, nil)
}
'

# --- Case V (codex iter-7 [P2]): a TestMain whose ONLY FLEET_GC_SCAN_DIR
#     setter is commented out must NOT exempt the package → dispatch test
#     in it is FLAGGED.
run_case_two_files "scandir-testmain-commented-setter-not-exempt" 1 "scan-dir isolation" \
'package sub

import (
    "os"
    "testing"
)

func TestMain(m *testing.M) {
    // _ = os.Setenv("FLEET_GC_SCAN_DIR", "/tmp/decoy")  // disabled
    os.Exit(m.Run())
}
' \
'package sub

import "testing"

func TestDispatchCommentedDecoy(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case W (codex iter-8 [P2]): a TestMain whose FLEET_GC_SCAN_DIR setter
#     is inside a /* ... */ BLOCK comment must NOT exempt the package.
run_case_two_files "scandir-testmain-blockcommented-setter-not-exempt" 1 "scan-dir isolation" \
'package sub

import (
    "os"
    "testing"
)

func TestMain(m *testing.M) {
    /* disabled decoy:
       os.Setenv("FLEET_GC_SCAN_DIR", "/tmp/decoy")
    */
    os.Exit(m.Run())
}
' \
'package sub

import "testing"

func TestDispatchBlockCommentedDecoy(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case X: a REAL TestMain setter (the cmd/fleet shape) still exempts.
run_case_two_files "scandir-testmain-real-setter-exempts" 0 "0 violations" \
'package sub

import (
    "os"
    "testing"
)

func TestMain(m *testing.M) {
    _ = os.Setenv("FLEET_GC_SCAN_DIR", "/tmp/real-decoy")
    os.Exit(m.Run())
}
' \
'package sub

import "testing"

func TestDispatchRealDecoy(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case Y (codex iter-9 [P2]): a TestMain with NO scan-dir setter, plus
#     an UNRELATED per-test t.Setenv in another function, must NOT exempt
#     the package. The isolated test passes; a SEPARATE unisolated dispatch
#     test in the same package is still FLAGGED.
run_case_two_files "scandir-pertest-setter-does-not-exempt-package" 1 "scan-dir isolation" \
'package sub

import (
    "os"
    "testing"
)

func TestMain(m *testing.M) {
    os.Exit(m.Run())
}

func TestHasItsOwnScanDir(t *testing.T) {
    tmuxtest.RequireTmux(t)
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    _ = runDispatch(nil, nil)
}
' \
'package sub

import "testing"

func TestSiblingUnisolated(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = runDispatch(nil, nil)
}
'

# --- Case Z (codex iter-10 [P2]): a helper that BOTH scopes the scan dir
#     AND calls runDispatch is an ISOLATING helper; a test calling it must
#     NOT be flagged.
run_case "scandir-isolating-helper-passes" 0 "0 violations" \
'package sub

import "testing"

func dispatchIsolated(t *testing.T) {
    t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir())
    _ = runDispatch(nil, nil)
}

func TestViaIsolatingHelper(t *testing.T) {
    tmuxtest.RequireTmux(t)
    dispatchIsolated(t)
}
'

# --- Case AA (codex iter-11 [P2]): a BLOCK-commented scan-dir marker must
#     NOT attest isolation (block comments stripped by the shared parser).
run_case "scandir-blockcommented-marker-not-isolation" 1 "scan-dir isolation" \
'package sub

import "testing"

func TestBlockCommentedScanDirMarker(t *testing.T) {
    tmuxtest.RequireTmux(t)
    /* t.Setenv("FLEET_GC_SCAN_DIR", t.TempDir()) */
    _ = runDispatch(nil, nil)
}
'

# --- Case BB (codex iter-11 [P2]): a multiline RAW STRING containing a `}`
#     line before a later runDispatch must NOT prematurely close the body;
#     the unisolated trigger must still be FLAGGED. The raw string is built
#     with a printf so the literal backticks embed cleanly in this fixture.
bb_bt=$(printf '\140')   # backtick
run_case "scandir-multiline-rawstring-then-trigger-flagged" 1 "scan-dir isolation" \
"package sub

import \"testing\"

func TestRawStringThenTrigger(t *testing.T) {
    tmuxtest.RequireTmux(t)
    _ = ${bb_bt}some json
}
more${bb_bt}
    _ = runDispatch(nil, nil)
}
"

echo ""
echo "test_lint_test_isolation: $passed passed, $failed failed"
if [[ "$failed" -gt 0 ]]; then
    exit 1
fi
exit 0
