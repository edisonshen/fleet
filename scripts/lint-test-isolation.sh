#!/usr/bin/env bash
# lint-test-isolation.sh — enforce tmux socket isolation in Go tests.
#
# Postmortem 2026-05-14 (orphan tmux leak): tests that call tmux.Spawn
# without first setting FLEET_TMUX_SOCKET hit the operator's default
# tmux server. Because tmux sessions outlive the test process, every
# such test leaks one tmux session per run — production observed 2,800+
# stale /tmp/fleet-test-*.sock files plus 100+ orphan fleet-* sessions
# after a morning of subagent test runs.
#
# Follow-up 2026-05-15 (AtomicCoordSwap leak): file-scope linting wasn't
# enough. Tests that call runDispatch/runHandoff/Resume reach
# spawn.Spawn → tmux.Spawn transitively. A test file could have ONE
# function with requireTmux + ten functions that call runDispatch with
# no isolation, and the file-level grep would mark it clean. The new
# load-bearing safety boundary is the runtime sink guard at
# internal/tmux/tmux.go's Spawn function (refuses default socket under
# `go test`). This lint is the static tripwire that catches violations
# at PR review instead of inside a failing test.
#
# Algorithm: per-test-function scan. For each `func TestXxx(t *testing.T)`
# block in every *_test.go file, check whether its body contains any
# transitive entry point (tmux.Spawn, spawn.Spawn, runDispatch, runHandoff,
# runHandoffDrain, Resume). If yes, the same body must also contain an
# isolation marker (requireTmux, isolateTmuxSocket, tmuxtest.RequireTmux,
# or the literal FLEET_TMUX_SOCKET env-var name).
#
# Function-body delimitation: a test function starts at `func TestXxx(`
# at column 0 and ends at the matching `}` at column 0. Helper functions
# defined inside the file (lowercase names, table-driven sub-cases) are
# NOT separately validated — Go's call-graph would need a parser. The
# convention is that any test helper that wraps tmux.Spawn either calls
# tmuxtest.RequireTmux directly (so the lint sees it in the helper file)
# or is named after one of the isolation markers (so the test function
# that invokes the helper trips the marker match).
#
# Bash 3.2 portable (macOS default). Awk does the function-body parsing
# so we don't need bash 4+ associative arrays or readarray/mapfile.
#
# Run locally: bash scripts/lint-test-isolation.sh
# Exit 0 = clean, exit 1 = at least one offender.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# Trigger substrings: any test function calling one of these reaches
# tmux.Spawn either directly or via the spawn → coord-spawn / handoff
# call graph. Add to this list when new transitive entry points appear.
# (Substring matching via awk index() is portable across BSD/GNU awk —
# regex with literal '(' chars trips BSD awk's ERE on macOS.)
#
# Subprocess entry points (runFleet, runTickCap) shell out to a child
# `fleet` binary that calls tmux.Spawn from inside the child process.
# `testing.Testing()` is false in the child, so the runtime sink guard
# doesn't fire — the lint is the only safety net for these. Codex
# review iter-3 [P2] (2026-05-15) flagged this gap.
triggers="tmux.Spawn( spawn.Spawn( runDispatch( runHandoff( runHandoffDrain( Resume( runFleet( runTickCap( runTick("

# Isolation marker substrings. Keep narrow — every entry is an explicit
# opt-in. The canonical marker is tmuxtest.RequireTmux (see
# internal/testutil/tmuxtest). The two-pass scan below adds any
# non-test helper function that itself calls one of these markers
# (e.g., setupCoordIntegration → requireTmux → tmuxtest.RequireTmux).
#
# Codex review iter-3 [P2] (2026-05-15): the bare substring
# `FLEET_TMUX_SOCKET` matched any read of the env var (e.g., a helper
# that forwards it to a subprocess) and falsely attested isolation.
# Narrow to the two patterns that ACTUALLY set the variable:
# `t.Setenv("FLEET_TMUX_SOCKET"` (the helper path) and
# `os.Setenv("FLEET_TMUX_SOCKET"` (the rare direct path). Reads /
# inspections of the var still pass the trigger-bearing helper as a
# trigger, not a marker.
markers='tmuxtest.RequireTmux requireTmux( isolateTmuxSocket( t.Setenv("FLEET_TMUX_SOCKET os.Setenv("FLEET_TMUX_SOCKET'

# Enumerate test files. Use git when inside a repo so vendored/untracked
# artifacts are skipped; fall back to find otherwise.
file_lister() {
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files '*_test.go'
  else
    find . -name '*_test.go' -not -path './vendor/*'
  fi
}

# Skip the canonical helper's own test file. tmuxtest_test.go exercises
# tmuxtest.RequireTmux without calling it transitively through any of the
# trigger entry points; it's the helper's unit test, not a regression
# test that leaks sessions.
skip_file() {
  case "$1" in
    internal/testutil/tmuxtest/*) return 0;;
  esac
  return 1
}

# Per-function scan in awk. Emits one line per offending function:
#   <file>:<startline>:<funcname>
# Algorithm:
#   - Track `func TestXxx(...)` line as start.
#   - Brace counter increments on every `{` outside strings/comments. We
#     simplify by counting raw braces — Go code with literal braces in
#     strings is rare in test bodies and would produce a false positive
#     that prompts a quick lint re-tune; never a silent bypass.
#   - When the brace counter returns to 0, the function body has ended.
#   - Inside the body, accumulate a marker bitmask: trigger=1, marker=2.
#   - At end, if trigger seen AND marker NOT seen, print the offender.
scan_file() {
  local f="$1"
  awk -v file="$f" -v trigs="$triggers" -v marks="$markers" '
    function reset() {
      in_func = 0
      func_name = ""
      start_line = 0
      saw_trig = 0
      saw_iso = 0
    }
    function any_substr(haystack, needles_str,    n, arr, i) {
      n = split(needles_str, arr, " ")
      for (i = 1; i <= n; i++) {
        if (arr[i] == "") continue
        if (index(haystack, arr[i]) > 0) return 1
      }
      return 0
    }
    BEGIN {
      reset()
    }
    # Function start: "func TestXxx(" at column 0. We only lint exported
    # Test functions — helpers (lowercase names) are checked indirectly:
    # if a Test function calls a helper that ends up at tmux.Spawn but
    # never mentions an isolation marker, the Test will be flagged.
    /^func Test[A-Za-z0-9_]*\(/ {
      reset()
      in_func = 1
      start_line = NR
      n = $0
      sub(/^func[ \t]+/, "", n)
      sub(/\(.*/, "", n)
      func_name = n
      # Skip trigger/marker scan for the function-declaration line:
      # a test named TestXxx_SuggestsResume would otherwise match the
      # "Resume(" substring on its own declaration.
      next
    }
    in_func {
      if (any_substr($0, trigs)) saw_trig = 1
      if (any_substr($0, marks)) saw_iso = 1
      # End of function: closing brace at column 0.
      if ($0 ~ /^}/) {
        if (saw_trig && !saw_iso) {
          printf "%s:%d:%s\n", file, start_line, func_name
        }
        reset()
      }
    }
  ' "$f"
}

# Pass 1: discover helper functions (any non-Test function in *_test.go
# OR a Method declaration) whose body calls one of the seed markers OR
# triggers. Helper names get appended to the appropriate list:
#
#   - Marker-bearing helpers (call requireTmux / tmuxtest.RequireTmux /
#     FLEET_TMUX_SOCKET / etc.) get added to `markers`, so a test
#     calling e.g. setupCoordIntegration(t) inherits the isolation
#     attestation.
#
#   - Trigger-bearing helpers (call tmux.Spawn / spawn.Spawn / etc.,
#     and do NOT themselves contain a marker) get added to `triggers`,
#     so a test calling seedAgent(t) / plantCoord(t) is required to
#     ALSO call an isolation marker. Codex review iter-2 [P3]
#     (2026-05-15): without this, a test that only calls a helper-
#     wrapped Spawn passes the lint and the runtime guard becomes
#     the sole safety net.
#
# Both passes use the same awk scanner, parameterized on which list to
# match against. Method receivers like "(env *integrationEnv) plantCoord(...)"
# are stripped — emit the bare method name so the lookup substring is
# ".plantCoord(" or just "plantCoord(".
discover_helpers() {
  local file="$1"
  local needles="$2"
  local exclude="$3" # don't emit a helper whose name appears here (avoids self-loop)
  awk -v needles="$needles" -v exclude="$exclude" '
    function any_substr(haystack, needles_str,    n, arr, i) {
      n = split(needles_str, arr, " ")
      for (i = 1; i <= n; i++) {
        if (arr[i] == "") continue
        if (index(haystack, arr[i]) > 0) return 1
      }
      return 0
    }
    function reset() {
      in_func = 0
      func_name = ""
      saw = 0
    }
    BEGIN { reset() }
    /^func / {
      reset()
      in_func = 1
      n = $0
      sub(/^func[ \t]+/, "", n)
      if (substr(n, 1, 1) == "(") {
        idx = index(n, ") ")
        if (idx > 0) n = substr(n, idx + 2)
      }
      sub(/\(.*/, "", n)
      func_name = n
      # Skip Test funcs — they are checked, not used as helpers.
      if (substr(func_name, 1, 4) == "Test") in_func = 0
      next
    }
    in_func {
      if (any_substr($0, needles)) saw = 1
      if ($0 ~ /^}/) {
        if (saw && func_name != "") {
          # Suppress helpers whose name is in the exclude list — this
          # prevents trigger-bearing helpers that ALSO call markers from
          # being emitted as triggers (e.g. setupCoordIntegration calls
          # requireTmux and seeds tmux; we want it as a marker, not a
          # trigger).
          emit = 1
          if (exclude != "") {
            en = split(exclude, ea, " ")
            for (i = 1; i <= en; i++) {
              if (ea[i] == "") continue
              if (index(ea[i], func_name "(") > 0) { emit = 0; break }
            }
          }
          if (emit) printf "%s(\n", func_name
        }
        reset()
      }
    }
  ' "$file"
}

# Multi-pass helper discovery. We need to fixed-point both:
#   1. Helpers that call any marker → become markers themselves.
#   2. Helpers that call any trigger AND no marker → become triggers
#      themselves (so a test calling only a Spawn-wrapping helper
#      without an isolation marker gets flagged).
#
# Pass M (marker chain): a helper that calls a marker is an isolating
# delegate. setupCoordIntegration → requireTmux → tmuxtest.RequireTmux
# is the classic chain.
#
# Pass T (trigger chain): a helper that calls tmux.Spawn / spawn.Spawn /
# runDispatch / etc. (without itself isolating) becomes a trigger. A
# test calling seedAgent(t) without requireTmux(t) is then flagged.
# Codex review iter-2 [P3] (2026-05-15): the "Spawn-wrapping helpers
# bypass the lint" hole closes here.
#
# Iteration cap = 3 passes per chain. Deeper helper graphs would need
# a real call-graph parser; today's tree has at most 2 levels.
discover_markers_in_file() { discover_helpers "$1" "$markers" ""; }
discover_triggers_in_file() {
  # Exclude helpers that ALSO match the marker list — those are
  # isolation delegates, not trigger sinks. The exclude check uses
  # the current markers list (which includes discovered marker
  # helpers from earlier passes).
  discover_helpers "$1" "$triggers" "$markers"
}

# --- Marker chain ---
for pass in 1 2 3; do
  new_markers=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      case " $markers " in
        *" $h "*) ;; # already a marker
        *) new_markers="$new_markers $h";;
      esac
    done < <(discover_markers_in_file "$f")
  done < <(file_lister)
  if [ -z "$new_markers" ]; then break; fi
  markers="$markers $new_markers"
done

# --- Trigger chain ---
# Run AFTER the marker chain so the exclude-list (markers) is final;
# a Spawn-wrapping helper that ALSO calls an isolation marker (e.g.,
# a helper that pre-spawns under an already-isolated socket) won't be
# misclassified as a trigger sink.
for pass in 1 2 3; do
  new_triggers=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      case " $triggers " in
        *" $h "*) ;; # already a trigger
        *) new_triggers="$new_triggers $h";;
      esac
    done < <(discover_triggers_in_file "$f")
  done < <(file_lister)
  if [ -z "$new_triggers" ]; then break; fi
  triggers="$triggers $new_triggers"
done

violations=()
files_scanned=0
while IFS= read -r f; do
  if skip_file "$f"; then continue; fi
  files_scanned=$((files_scanned + 1))
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    violations+=("$v")
  done < <(scan_file "$f")
done < <(file_lister)

if (( ${#violations[@]} > 0 )); then
  echo "test-isolation-lint: FAIL" >&2
  echo "" >&2
  echo "The following test functions reach tmux.Spawn (directly or via" >&2
  echo "spawn.Spawn / runDispatch / runHandoff / Resume) but do NOT" >&2
  echo "isolate FLEET_TMUX_SOCKET. Production leak risk — see" >&2
  echo "docs/postmortems/2026-05-14-orphan-tmux-leak.md." >&2
  echo "" >&2
  printf '  %s\n' "${violations[@]}" >&2
  echo "" >&2
  echo "Fix: call 'tmuxtest.RequireTmux(t)' (the canonical helper at" >&2
  echo "internal/testutil/tmuxtest) — or a local wrapper that delegates" >&2
  echo "to it — inside the offending test function." >&2
  echo "" >&2
  echo "The runtime sink guard at internal/tmux/tmux.go's Spawn would" >&2
  echo "fail these tests at runtime; this lint catches them at review." >&2
  exit 1
fi

echo "test-isolation-lint: ${files_scanned} *_test.go files scanned, 0 violations"
