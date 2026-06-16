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
# Use "|" as the inter-needle separator so individual needles can
# legally contain spaces (e.g., "= Spawn(" for package-local
# helper calls like `err := Spawn(...)` minus the `:`).
triggers="tmux.Spawn(|spawn.Spawn(|runDispatch(|runHandoff(|runHandoffDrain(|runDrain(|Resume(|runFleet(|runTickCap(|runTick(|= Spawn(|:= Spawn("

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
#
# Codex review iter-17 [P2] (2026-05-15): removed `t.Setenv` /
# `os.Setenv` markers because they allow tests to set
# FLEET_TMUX_SOCKET to non-canonical values (e.g.,
# `/tmp/tmux-501/default` for guard-regression tests) and still attest
# isolation. Tests that need raw env mutation around the guard use
# the explicit `lint-test-isolation:exempt` marker; everyone else
# must call the canonical helpers.
markers='tmuxtest.RequireTmux|tmuxtest.IsolateSocket|requireTmux(|isolateTmuxSocket('

# ci-perf-pr2 (DESIGN-ci-3min-test-suite): the in-process fake tmux backend
# (internal/testutil/tmuxfake) is a valid isolation mechanism for
# IN-PROCESS triggers — InstallFake swaps the package tmux backend so
# tmux.Spawn records in-process and NEVER reaches the operator's default
# socket. A test that installs the fake (tmuxfake.InstallFake, or the
# requireFakeTmux wrapper) satisfies the safety boundary FOR IN-PROCESS
# code paths.
#
# CRUCIAL LIMIT (codex iter-6 [P2]): the fake only rebinds function
# pointers in the PARENT test process. A SUBPROCESS trigger — runFleet /
# runTick / runTickCap shells out to a child `fleet` binary where
# testing.Testing() is false and the fake was never installed — so the
# child can still reach the real/default tmux socket. The fake therefore
# does NOT attest isolation for a test that hits a subprocess trigger;
# those MUST use real-socket isolation (tmuxtest.RequireTmux /
# FLEET_TMUX_SOCKET). fake_markers below are honored ONLY when the test
# has no subprocess trigger.
#
# NOTE: unlike markers / triggers / subproc_triggers / scandir_markers,
# fake_markers gets NO helper-chain discovery pass — requireFakeTmux is the
# only wrapper today and is listed literally. If a second wrapper appears
# (e.g. setupFakeSpawn that calls tmuxfake.InstallFake), add it here OR wire a
# fake-marker discovery loop. The failure mode of forgetting is SAFE (a
# false-positive flag, never a missed leak), which is why auto-discovery is
# deferred per "no premature abstractions".
fake_markers='tmuxfake.InstallFake(|requireFakeTmux('

# Subprocess entry points: these fork a child `fleet` that calls tmux from
# its own process image (the fake is parent-only). A fake marker cannot
# isolate these — only a real socket marker can.
subproc_triggers="runFleet(|runTickCap(|runTick("

# Enumerate test files. Use git when inside a repo so vendored/untracked
# artifacts are skipped; fall back to find otherwise.
file_lister() {
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files '*_test.go'
  else
    find . -name '*_test.go' -not -path './vendor/*'
  fi
}

# Skip the isolation helpers' own test files. tmuxtest_test.go exercises
# tmuxtest.RequireTmux, and tmuxfake's tests drive the fake's Spawn/Kill
# methods directly (`f.Spawn(...)`) — the fake IS the isolation, so its
# in-process calls never reach a real socket and can't leak a session.
# These are the helpers' unit tests, not regression tests that leak.
skip_file() {
  case "$1" in
    internal/testutil/tmuxtest/*) return 0;;
    internal/testutil/tmuxfake/*) return 0;;
  esac
  return 1
}

# Per-function scan in awk. Emits one line per offending function:
#   <file>:<startline>:<funcname>
#
# Algorithm (codex review iter-7 [P2], 2026-05-15): proper Go-aware
# brace counting. A naive "first column-0 }" detector misclassifies a
# raw-string-literal that spans multiple lines and contains a line
# starting with `}` (or any string with such content). Real Go code in
# this repo has that shape — `cmd/fleet/handoff_test.go` builds a
# legacy JSON via a multi-line `` `…` `` raw string whose first line
# after the open-brace puts `}` at column 0.
#
# Tracker state per line:
#   - in_raw  = inside Go raw-string (backtick to backtick), braces ignored
#   - in_str  = inside Go interpreted string (double-quote), braces ignored
#   - brace   = net depth of {} characters outside strings/comments
#
# Function body ends when brace returns to 0 having gone above 0 — i.e.,
# we've seen the opening { for the function and matched it.
# _scan_bodies is the shared, Go-aware per-test-function scanner. It walks
# each `func TestXxx` body with full raw-string / interpreted-string / //
# and /* */ comment awareness and correct brace matching, then flags a
# function whose body has a TRIGGER but no isolation. Parameterized on the
# trigger/marker/fake-marker/subproc/exempt lists so BOTH the tmux rule
# (scan_file) and the scan-dir rule (scan_scandir) reuse ONE correct parser
# (codex iter-11 [P2]: the scan-dir rule's earlier crude per-line counter
# mishandled block comments + multiline raw strings).
#   $1=file $2=triggers $3=markers $4=fake_markers $5=subproc_triggers
#   $6=exempt_marker
_scan_bodies() {
  awk -v file="$1" -v trigs="$2" -v marks="$3" \
      -v fake_marks="$4" -v subproc_trigs="$5" \
      -v exempt_marker="$6" '
    function reset() {
      in_func = 0
      func_name = ""
      start_line = 0
      saw_trig = 0
      saw_iso = 0
      saw_fake_iso = 0   # fake-backend marker present (parent-only)
      saw_subproc = 0    # subprocess trigger present (child escapes fake)
      brace = 0
      in_raw = 0
      in_str = 0
      in_blkcmt = 0  # codex iter-18 [P2]: prevent block-comment state
                     # bleeding from a prior function into the next one.
    }
    # is_isolated folds the fake-marker rule: a real-socket marker always
    # isolates; a fake marker isolates ONLY when the test has no subprocess
    # trigger (the child binary escapes the parent-only fake — codex
    # iter-6 [P2]).
    function is_isolated() {
      if (saw_iso) return 1
      if (saw_fake_iso && !saw_subproc) return 1
      return 0
    }
    function any_substr(haystack, needles_str,    n, arr, i) {
      # "|" separator so a needle can contain spaces (e.g., "= Spawn(").
      n = split(needles_str, arr, "|")
      for (i = 1; i <= n; i++) {
        if (arr[i] == "") continue
        if (index(haystack, arr[i]) > 0) return 1
      }
      return 0
    }
    # process_line counts code-level braces AND returns the
    # COMMENT-STRIPPED version of the line via the global var
    # `code_line`. String literal CONTENTS are preserved (they may
    # contain marker arguments like `t.Setenv("FLEET_TMUX_SOCKET", ...)`
    # that the substring check needs).
    #
    # Tracks four states:
    #   - in_raw      = inside Go raw-string (backtick to backtick)
    #   - in_str      = inside Go interpreted string (double-quote)
    #   - in_blkcmt   = inside /* ... */ block comment (multi-line)
    #
    # Codex review iter-16 [P2] (2026-05-15): comments inside helpers
    # like `// call requireTmux(t)` previously falsely attested
    # isolation; iter-16 [P3] flagged the same direction for triggers.
    function process_line(line,    i, ch, nch, prev, ln) {
      code_line = ""
      prev = ""
      ln = length(line)
      for (i = 1; i <= ln; i++) {
        ch = substr(line, i, 1)
        if (in_blkcmt) {
          if (ch == "*" && i < ln && substr(line, i+1, 1) == "/") {
            in_blkcmt = 0
            i++
          }
          prev = ch
          continue
        }
        if (in_raw) {
          # Raw-string contents pass through as code_line. Codex iter-7
          # confirmed brace-balance matters here; substring matches on
          # raw string literals are intentional (matching "tmux.Spawn("
          # inside a sanity test would still detect a trigger).
          code_line = code_line ch
          if (ch == "`") in_raw = 0
          prev = ch
          continue
        }
        if (in_str) {
          code_line = code_line ch
          if (ch == "\\" && prev != "\\") { prev = ch; continue }
          if (ch == "\"" && prev != "\\") in_str = 0
          prev = ch
          continue
        }
        if (ch == "/" && i < ln) {
          nch = substr(line, i+1, 1)
          if (nch == "/") {
            # Rest of line is a // comment; stop processing.
            return
          }
          if (nch == "*") {
            in_blkcmt = 1
            i++
            prev = ch
            continue
          }
        }
        code_line = code_line ch
        if (ch == "`") in_raw = 1
        else if (ch == "\"") in_str = 1
        else if (ch == "{") brace++
        else if (ch == "}") brace--
        prev = ch
      }
    }
    BEGIN { reset() }
    # Function start: "func TestXxx(" at column 0. We only lint
    # exported Test functions — helpers (lowercase names) become
    # additional triggers/markers via discover_helpers and are
    # checked indirectly through their callers.
    /^func Test[A-Za-z0-9_]*\(/ {
      reset()
      in_func = 1
      start_line = NR
      n = $0
      sub(/^func[ \t]+/, "", n)
      sub(/\(.*/, "", n)
      func_name = n
      # Process the line to obtain code_line (comments + strings
      # stripped). Then strip the func-declaration prefix and scan only
      # the body portion for triggers/markers — critical for one-liner
      # tests like `func TestX(t *testing.T) { runDispatch(...) }`
      # (codex iter-14 [P2]).
      process_line($0)
      body = code_line
      sub(/^[^{]*\{/, "", body)
      if (body != "") {
        if (any_substr(body, trigs)) saw_trig = 1
        if (any_substr(body, marks)) saw_iso = 1
        if (any_substr(body, fake_marks)) saw_fake_iso = 1
        if (any_substr(body, subproc_trigs)) saw_subproc = 1
      }
      # The exempt marker `lint-test-isolation:exempt` is intentionally
      # placed in comments; check the raw line for it separately.
      if (index($0, exempt_marker) > 0) saw_iso = 1
      if (brace == 0) {
        if (saw_trig && !is_isolated()) {
          printf "%s:%d:%s\n", file, start_line, func_name
        }
        reset()
      }
      next
    }
    in_func {
      # Scan code_line (comments/strings stripped) so a comment that
      # mentions a trigger or marker keyword does not falsely match.
      # Codex review iter-16 [P3] (2026-05-15).
      process_line($0)
      if (code_line != "") {
        if (any_substr(code_line, trigs)) saw_trig = 1
        if (any_substr(code_line, marks)) saw_iso = 1
        if (any_substr(code_line, fake_marks)) saw_fake_iso = 1
        if (any_substr(code_line, subproc_trigs)) saw_subproc = 1
      }
      # The exempt marker `lint-test-isolation:exempt` is intentionally
      # placed in comments; check the raw line for it separately.
      if (index($0, exempt_marker) > 0) saw_iso = 1
      # Function body ends when brace returns to 0. (The opening { put
      # us at 1 on the declaration line; the matching close brings us
      # back to 0.)
      if (brace == 0) {
        if (saw_trig && !is_isolated()) {
          printf "%s:%d:%s\n", file, start_line, func_name
        }
        reset()
      }
    }
  ' "$1"
}

# scan_file = the tmux socket-isolation rule (#1/#2), using the global
# trigger / marker / fake-marker / subproc lists + the tmux exempt marker.
scan_file() {
  _scan_bodies "$1" "$triggers" "$markers" "$fake_markers" \
    "$subproc_triggers" "lint-test-isolation:exempt"
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
  awk -v needles="$needles" -v exclude="$exclude" -v exempt_marker="lint-test-isolation:exempt" '
    function any_substr(haystack, needles_str,    n, arr, i) {
      # "|" separator so a needle can contain spaces (e.g., "= Spawn(").
      n = split(needles_str, arr, "|")
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
      brace = 0
      in_raw = 0
      in_str = 0
      in_blkcmt = 0
    }
    # process_line — comment-stripped line extraction + brace counting.
    # String contents pass through (markers may live inside string args
    # like `t.Setenv("FLEET_TMUX_SOCKET", ...)`). See scan_file for the
    # full rationale. Codex iter-16 [P2] (2026-05-15).
    function process_line(line,    i, ch, nch, prev, ln) {
      code_line = ""
      prev = ""
      ln = length(line)
      for (i = 1; i <= ln; i++) {
        ch = substr(line, i, 1)
        if (in_blkcmt) {
          if (ch == "*" && i < ln && substr(line, i+1, 1) == "/") {
            in_blkcmt = 0
            i++
          }
          prev = ch
          continue
        }
        if (in_raw) {
          code_line = code_line ch
          if (ch == "`") in_raw = 0
          prev = ch
          continue
        }
        if (in_str) {
          code_line = code_line ch
          if (ch == "\\" && prev != "\\") { prev = ch; continue }
          if (ch == "\"" && prev != "\\") in_str = 0
          prev = ch
          continue
        }
        if (ch == "/" && i < ln) {
          nch = substr(line, i+1, 1)
          if (nch == "/") return
          if (nch == "*") { in_blkcmt = 1; i++; prev = ch; continue }
        }
        code_line = code_line ch
        if (ch == "`") in_raw = 1
        else if (ch == "\"") in_str = 1
        else if (ch == "{") brace++
        else if (ch == "}") brace--
        prev = ch
      }
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
      if (substr(func_name, 1, 4) == "Test") { in_func = 0; next }
      # Scan the code-only BODY portion of this declaration line.
      # Required for one-line helper wrappers like
      # `func withTmux(t *testing.T) { tmuxtest.RequireTmux(t) }`
      # (codex iter-14 [P2]) + comment/string stripping (iter-16 [P2]).
      process_line($0)
      body = code_line
      sub(/^[^{]*\{/, "", body)
      if (body != "" && any_substr(body, needles)) saw = 1
      if (brace == 0) {
        if (saw && func_name != "") {
          emit = 1
          if (exclude != "") {
            en = split(exclude, ea, "|")
            target = func_name "("
            for (i = 1; i <= en; i++) {
              if (ea[i] == "") continue
              if (ea[i] == target) { emit = 0; break }
            }
          }
          if (emit) printf "%s(\n", func_name
        }
        reset()
      }
      next
    }
    in_func {
      # Scan code_line (comments + strings stripped) — codex iter-16 [P2].
      process_line($0)
      if (code_line != "" && any_substr(code_line, needles)) saw = 1
      if (brace == 0) {
        if (saw && func_name != "") {
          emit = 1
          if (exclude != "") {
            # "|"-separated exclude list (same separator as the
            # needles list). Test for exact match of "<funcName>(".
            en = split(exclude, ea, "|")
            target = func_name "("
            for (i = 1; i <= en; i++) {
              if (ea[i] == "") continue
              if (ea[i] == target) { emit = 0; break }
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

# Lists are "|"-separated so individual needles can legally contain
# spaces. Membership-checks use "|<needle>|" anchors (a bracketing
# delimiter pair) for unambiguous substring testing.
list_contains() {
  # $1 = pipe-separated list; $2 = needle.
  case "|$1|" in
    *"|$2|"*) return 0;;
  esac
  return 1
}

# --- Marker chain ---
for pass in 1 2 3; do
  new_markers=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      if list_contains "$markers" "$h"; then continue; fi
      if list_contains "$new_markers" "$h"; then continue; fi
      new_markers="$new_markers|$h"
    done < <(discover_markers_in_file "$f")
  done < <(file_lister)
  if [ -z "$new_markers" ]; then break; fi
  markers="$markers$new_markers"
done

# --- Trigger chain ---
# Run AFTER the marker chain so the exclude-list (markers) is final;
# a Spawn-wrapping helper that ALSO calls an isolation marker won't be
# misclassified as a trigger sink.
for pass in 1 2 3; do
  new_triggers=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      if list_contains "$triggers" "$h"; then continue; fi
      if list_contains "$new_triggers" "$h"; then continue; fi
      new_triggers="$new_triggers|$h"
    done < <(discover_triggers_in_file "$f")
  done < <(file_lister)
  if [ -z "$new_triggers" ]; then break; fi
  triggers="$triggers$new_triggers"
done

# --- Subprocess-trigger chain (codex iter-7 [P2]) ---
# A helper that wraps a subprocess entry point (runFleet / runTick /
# runTickCap) is itself a subprocess trigger: a test calling it forks a
# child `fleet` that escapes the parent-only fake. Track these separately
# from the in-process trigger chain so the fake marker is NOT accepted as
# isolation for a test that reaches a subprocess trigger THROUGH a helper.
# Exclude markers + fake_markers so an isolating helper isn't a sink.
for pass in 1 2 3; do
  new_subproc=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      if list_contains "$subproc_triggers" "$h"; then continue; fi
      if list_contains "$new_subproc" "$h"; then continue; fi
      new_subproc="$new_subproc|$h"
    done < <(discover_helpers "$f" "$subproc_triggers" "$markers|$fake_markers")
  done < <(file_lister)
  if [ -z "$new_subproc" ]; then break; fi
  subproc_triggers="$subproc_triggers$new_subproc"
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

# ----- Second guard: empty-command dispatchOpts → real claude spawn -----
#
# Postmortem 2026-05-29 (OOM) / docs/DESIGN-lifecycle-leak-recurrence.md
# PR-A root cause #1: any test that builds a `&dispatchOpts{...}` literal
# and calls runDispatch on it MUST set `commandExplicit: true` so the
# wrapper-swap block (cmd/fleet/dispatch.go:416-424) does NOT substitute
# the real `claude --dangerously-skip-permissions` argv. With explicit
# isolation but empty commandExplicit, runDispatch reaches spawn.Spawn,
# resolves the engine wrapper, and forks a real detached tmux+claude on
# any dev host with both tmux and claude on PATH. The 2026-05-29
# investigation found procs up to 7 days old.
#
# Per-test-function scan: for each `func TestXxx`, if the body contains
# `&dispatchOpts{` AND `runDispatch(`, the body MUST also contain
# `commandExplicit: true` OR the explicit exemption marker
# `lint-test-isolation:command-exempt`. The latter is for tests like
# TestDispatch_NoLocalEngineFlagShadow that don't actually drive a
# dispatch — only inspect cobra wiring.
#
# Scanner re-uses the comment-stripping awk used above; just a different
# trigger / marker set. The dispatchOpts literal is always built inline
# at the test callsite, but the runDispatch CALL can be wrapped in a
# helper (e.g. runDispatchIgnoringSpawnErr in
# cmd/fleet/dispatch_rc_auto_marker_test.go). Codex review iter-1 [P2]
# (2026-06-04): the previous "saw_run matches only `runDispatch(`"
# leaked when a test built &dispatchOpts{} and called a helper wrapper
# that itself reached runDispatch. We now also accept the documented
# wrapper(s). Add to this list whenever a new helper wraps runDispatch;
# the safer alternative (full helper-chain discovery like the first
# guard) is intentionally deferred until a second wrapper appears, per
# CLAUDE.md "no premature abstractions."
scan_empty_command_dispatch() {
  local f="$1"
  awk -v file="$f" \
      -v exempt_marker="lint-test-isolation:command-exempt" '
    # Per-literal tracking (codex review iter-3 [P2], 2026-06-04):
    # the earlier function-wide aggregation of `saw_explicit` could mask
    # a later empty-command literal when an earlier safe literal in the
    # same test set the flag. Track each dispatchOpts{ literal
    # independently — every one that appears in a runDispatch-bearing
    # function MUST be marked commandExplicit:true (or carry the
    # function-scope exempt marker). Any literal lacking the mark
    # flags the enclosing function as a violation.
    #
    # State machine:
    #   - func_brace          = brace depth relative to function start
    #     (declaration line opens at depth 1 once { is seen)
    #   - in_opts             = 1 inside a dispatchOpts{...} literal
    #   - opts_open_depth     = func_brace depth at the moment the
    #                           literal opened; literal closes when
    #                           func_brace returns to opts_open_depth
    #   - opts_explicit       = 1 if the CURRENT literal set
    #                           commandExplicit:true
    #   - unsafe_literal      = function-scope sticky bit: 1 once any
    #                           literal closed without explicit; reset
    #                           only at function end
    function reset() {
      in_func = 0
      func_name = ""
      start_line = 0
      saw_run = 0
      saw_exempt = 0
      func_brace = 0
      in_raw = 0
      in_str = 0
      in_blkcmt = 0
      in_opts = 0
      opts_open_depth = 0
      opts_explicit = 0
      unsafe_literal = 0
    }
    # process_line walks the line char-by-char to:
    #   - strip // line comments + /* */ block comments + Go string
    #     contents (treated as opaque w.r.t. brace counting)
    #   - track func_brace (whole-function depth)
    #   - detect `dispatchOpts{` openings inline so opts_open_depth is
    #     captured at the EXACT brace event, not summed across the line
    #   - detect `commandExplicit: true` / `commandExplicit:      true`
    #     inside the current literal so opts_explicit attaches to the
    #     right literal even when multiple are present in one function
    #   - detect literal close when func_brace returns to
    #     opts_open_depth; closing unsafely sets unsafe_literal.
    #
    # code_line is still emitted so the caller can grep for
    # runDispatch( / runDispatchIgnoringSpawnErr( / the exempt marker
    # at function scope.
    function process_line(line,    i, ch, nch, prev, ln, look) {
      code_line = ""
      prev = ""
      ln = length(line)
      for (i = 1; i <= ln; i++) {
        ch = substr(line, i, 1)
        if (in_blkcmt) {
          if (ch == "*" && i < ln && substr(line, i+1, 1) == "/") {
            in_blkcmt = 0
            i++
          }
          prev = ch
          continue
        }
        if (in_raw) {
          code_line = code_line ch
          if (ch == "`") in_raw = 0
          prev = ch
          continue
        }
        if (in_str) {
          code_line = code_line ch
          if (ch == "\\" && prev != "\\") { prev = ch; continue }
          if (ch == "\"" && prev != "\\") in_str = 0
          prev = ch
          continue
        }
        if (ch == "/" && i < ln) {
          nch = substr(line, i+1, 1)
          if (nch == "/") return
          if (nch == "*") { in_blkcmt = 1; i++; prev = ch; continue }
        }
        # Inline `dispatchOpts{` detection: at the position of the `{`
        # check the preceding 13 chars to match the literal.
        if (ch == "{" && i >= 13) {
          look = substr(line, i - 12, 13)
          if (look == "dispatchOpts{") {
            # Open a new literal. If we are nested inside another,
            # finalize that one as unsafe (a dispatchOpts containing
            # another dispatchOpts is implausible in practice; safe to
            # mark the outer one unsafe and reset).
            if (in_opts && !opts_explicit) unsafe_literal = 1
            in_opts = 1
            opts_open_depth = func_brace
            opts_explicit = 0
          }
        }
        # Inline `commandExplicit: true` / `commandExplicit:      true`
        # detection inside the current literal. Match by looking at the
        # tail substring ending at the current char.
        if (in_opts && ch == "e" && i >= 21) {
          look = substr(line, i - 20, 21)
          if (look == "commandExplicit: true") opts_explicit = 1
          look = substr(line, i - 25, 26)
          if (i >= 26 && look == "commandExplicit:      true") opts_explicit = 1
        }
        code_line = code_line ch
        if (ch == "`") in_raw = 1
        else if (ch == "\"") in_str = 1
        else if (ch == "{") {
          func_brace++
        }
        else if (ch == "}") {
          # Close the current dispatchOpts literal if this brace
          # returns func_brace to opts_open_depth.
          if (in_opts && func_brace == opts_open_depth + 1) {
            if (!opts_explicit) unsafe_literal = 1
            in_opts = 0
            opts_explicit = 0
            opts_open_depth = 0
          }
          func_brace--
        }
        prev = ch
      }
    }
    function consume_body(body) {
      # Function-scope: runDispatch call detection.
      if (index(body, "runDispatch(") > 0) saw_run = 1
      if (index(body, "runDispatchIgnoringSpawnErr(") > 0) saw_run = 1
    }
    function check_emit() {
      # Violation if: a runDispatch call exists AND any dispatchOpts
      # literal closed without commandExplicit:true AND no function-
      # scope exempt marker.
      if (saw_run && unsafe_literal && !saw_exempt) {
        printf "%s:%d:%s\n", file, start_line, func_name
      }
    }
    BEGIN { reset() }
    /^func Test[A-Za-z0-9_]*\(/ {
      reset()
      in_func = 1
      start_line = NR
      n = $0
      sub(/^func[ \t]+/, "", n)
      sub(/\(.*/, "", n)
      func_name = n
      process_line($0)
      consume_body(code_line)
      if (index($0, exempt_marker) > 0) saw_exempt = 1
      if (func_brace == 0) {
        check_emit()
        reset()
      }
      next
    }
    in_func {
      process_line($0)
      consume_body(code_line)
      if (index($0, exempt_marker) > 0) saw_exempt = 1
      if (func_brace == 0) {
        check_emit()
        reset()
      }
    }
  ' "$f"
}

cmd_violations=()
while IFS= read -r f; do
  if skip_file "$f"; then continue; fi
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    cmd_violations+=("$v")
  done < <(scan_empty_command_dispatch "$f")
done < <(file_lister)

if (( ${#cmd_violations[@]} > 0 )); then
  echo "test-isolation-lint: FAIL (empty-command dispatchOpts)" >&2
  echo "" >&2
  echo "The following test functions build a &dispatchOpts{...} literal" >&2
  echo "and call runDispatch (or runDispatchIgnoringSpawnErr) on it" >&2
  echo "WITHOUT setting commandExplicit: true." >&2
  echo "With commandExplicit unset (or false) AND opts.command empty," >&2
  echo "runDispatch substitutes the engine wrapper (cmd/fleet/dispatch.go" >&2
  echo ":416-424) and forks a real detached tmux+claude on any host with" >&2
  echo "tmux + claude on PATH. The 2026-05-29 OOM investigation found" >&2
  echo "such procs up to 7 days old." >&2
  echo "" >&2
  printf '  %s\n' "${cmd_violations[@]}" >&2
  echo "" >&2
  echo "Fix: set both" >&2
  echo "      command:         []string{\"sleep\", \"30\"}," >&2
  echo "      commandExplicit: true," >&2
  echo "on the dispatchOpts literal so the engine-wrapper swap is" >&2
  echo "bypassed. For tests that intentionally exercise the CLI default-" >&2
  echo "command path (NOT a real spawn), add the trailing comment" >&2
  echo "'// lint-test-isolation:command-exempt' inside the test body." >&2
  echo "" >&2
  echo "See docs/DESIGN-lifecycle-leak-recurrence.md PR-A (root cause #1)." >&2
  exit 1
fi

# ----- Third guard: scan-dir isolation (FLEET_GC_SCAN_DIR) -----
#
# ci-perf-pr2 (DESIGN-ci-3min-test-suite). Postmortem 2026-06-13 (PR #232
# 24-min CI hang, fleet#165): runDispatch / runStatus call
# runDispatchReconcile, and reconcile's OrphanTmux pass walks the socket
# SCAN DIR (gc.gcScanDir → FLEET_GC_SCAN_DIR, default `/tmp`) and execs
# `tmux -S <sock> ls` on every fleet-test-*.sock it finds. On a host with
# hundreds of leaked sockets that is N tmux subprocesses PER test — the
# exact grind that hung CI. Rule #1/#2 above guard the tmux SOCKET seam;
# this rule guards the orthogonal SCAN-DIR seam (a test can isolate its
# tmux socket yet still scan the host's real /tmp).
#
# Triggers (reach the scan-dir walk, directly or transitively):
#   runDispatch( · runDispatchReconcile( · gc.Reconcile( · runStatus(
#
# Isolation is satisfied EITHER per-function OR per-package:
#   per-function markers (the test scopes its own scan dir):
#     t.Setenv("FLEET_GC_SCAN_DIR"  ·  DefaultDepsWithScanDir(
#     ·  stubDispatchReconcile(  ·  isolateScanDir(
#   per-function exempt comment:
#     lint-test-isolation:scandir-exempt
#   per-PACKAGE decoy: the package's TestMain sets FLEET_GC_SCAN_DIR to a
#     decoy dir for the whole binary (cmd/fleet does this), which isolates
#     every test in the package at the process boundary. A file whose
#     package carries such a TestMain is exempt wholesale — re-annotating
#     every dispatch test would be noise.
#
# A test may legitimately need BOTH a tmux marker (rule #1/#2) AND a
# scan-dir marker (this rule); they guard different seams.
scandir_triggers="runDispatch(|runDispatchReconcile(|gc.Reconcile(|runStatus("
scandir_markers='t.Setenv("FLEET_GC_SCAN_DIR"|DefaultDepsWithScanDir(|stubDispatchReconcile(|isolateScanDir(|lint-test-isolation:scandir-exempt'

# Packages exempted wholesale because their TestMain installs a process-
# wide FLEET_GC_SCAN_DIR decoy. Detection (codex iter-7 [P2]): the file
# must contain `func TestMain(` AND an ACTUAL setter call —
# `Setenv("FLEET_GC_SCAN_DIR"` (os.Setenv or t.Setenv; TestMain uses
# os.Setenv). A bare mention in a comment / dead code no longer qualifies,
# so a package can't claim the exemption without really scoping the scan.
# has_real_scandir_setter: true iff the file's TestMain FUNCTION BODY
# contains an ACTUAL (non-commented) Setenv("FLEET_GC_SCAN_DIR" call. The
# setter MUST live inside TestMain (codex iter-9 [P2]) — a package with a
# TestMain plus an unrelated per-test t.Setenv elsewhere is NOT package-
# exempt, because that per-test setter only isolates ITS test. Both `//`
# and `/* */` comments are stripped so dead-code setters don't count
# (codex iter-7/8 [P2]).
has_real_scandir_setter() {
  awk '
    function strip(line,    i,n,c,out) {
      out=""; i=1; n=length(line)
      while (i<=n) {
        if (in_blk) {
          c=substr(line,i,1)
          if (c=="*" && i<n && substr(line,i+1,1)=="/") { in_blk=0; i+=2; continue }
          i++; continue
        }
        c=substr(line,i,1)
        if (c=="/" && i<n && substr(line,i+1,1)=="/") break
        if (c=="/" && i<n && substr(line,i+1,1)=="*") { in_blk=1; i+=2; continue }
        out=out c; i++
      }
      return out
    }
    BEGIN { in_main=0; depth=0; in_blk=0; found=0 }
    {
      code=strip($0)
      if (!in_main) {
        if (code ~ /func[ \t]+TestMain[ \t]*\(/) {
          in_main=1
          # count braces on the declaration line itself
        } else {
          next
        }
      }
      if (in_main) {
        if (index(code, "Setenv(\"FLEET_GC_SCAN_DIR\"") > 0) found=1
        tmp=code; gsub(/[^{]/,"",tmp); depth+=length(tmp)
        tmp=code; gsub(/[^}]/,"",tmp); depth-=length(tmp)
        # TestMain body closes when depth returns to 0 after the opening {.
        if (depth<=0 && code ~ /}/) in_main=0
      }
    }
    END { exit(found ? 0 : 1) }
  ' "$1"
}

scandir_exempt_pkgs="|"
while IFS= read -r f; do
  if skip_file "$f"; then continue; fi
  if has_real_scandir_setter "$f"; then
    pkgdir="${f%/*}"
    case "$scandir_exempt_pkgs" in
      *"|$pkgdir|"*) ;;
      *) scandir_exempt_pkgs="$scandir_exempt_pkgs$pkgdir|";;
    esac
  fi
done < <(file_lister)

# Helper-chain discovery for scan-dir triggers (codex iter-4 [P2]). Like
# rule #1's trigger chain, a non-test helper that itself reaches a scan-dir
# trigger (runDispatch / runStatus / gc.Reconcile / runDispatchReconcile)
# WITHOUT isolating the scan dir becomes a trigger too — so a test that only
# calls that helper (e.g. seedDispatched(t)) is still required to scope
# FLEET_GC_SCAN_DIR. Without this, a trigger hidden behind a helper slips
# past and the /tmp-scan hang this rule guards can recur.
#
# Two chains, run MARKER-first (mirrors rule #1):
#
#  (a) scan-dir MARKER-helper chain (codex iter-10 [P2]): a helper whose
#      BODY scopes the scan dir (t.Setenv FLEET_GC_SCAN_DIR /
#      DefaultDepsWithScanDir / ...) becomes a scan-dir MARKER itself, so a
#      test calling that isolating helper is NOT falsely flagged. Without
#      this, a helper that BOTH scopes the dir AND calls runDispatch would
#      be misclassified as a trigger (discover_helpers' exclude is name-
#      based, not body-based).
#
#  (b) scan-dir TRIGGER-helper chain: a helper reaching a scan-dir trigger
#      WITHOUT isolating becomes a trigger. Run AFTER (a) so the exclude
#      list (scandir_markers) already contains the isolating helpers.
#
# Iteration cap 3, matching the tmux chains (helper graphs are shallow).
for pass in 1 2 3; do
  new_scandir_markers=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      if list_contains "$scandir_markers" "$h"; then continue; fi
      if list_contains "$new_scandir_markers" "$h"; then continue; fi
      new_scandir_markers="$new_scandir_markers|$h"
    done < <(discover_helpers "$f" "$scandir_markers" "")
  done < <(file_lister)
  if [ -z "$new_scandir_markers" ]; then break; fi
  scandir_markers="$scandir_markers$new_scandir_markers"
done

for pass in 1 2 3; do
  new_scandir_triggers=""
  while IFS= read -r f; do
    if skip_file "$f"; then continue; fi
    while IFS= read -r h; do
      [ -n "$h" ] || continue
      if list_contains "$scandir_triggers" "$h"; then continue; fi
      if list_contains "$new_scandir_triggers" "$h"; then continue; fi
      new_scandir_triggers="$new_scandir_triggers|$h"
    done < <(discover_helpers "$f" "$scandir_triggers" "$scandir_markers")
  done < <(file_lister)
  if [ -z "$new_scandir_triggers" ]; then break; fi
  scandir_triggers="$scandir_triggers$new_scandir_triggers"
done

# scan_scandir = the scan-dir isolation rule (#3). Reuses the SAME robust
# Go-aware parser as scan_file via _scan_bodies (codex iter-11 [P2]): the
# earlier hand-rolled per-line counter mishandled /* */ block comments and
# multiline raw strings. Scan-dir has no fake-marker / subprocess concepts,
# so those lists are empty; the exempt marker is the scandir-exempt comment.
scan_scandir() {
  _scan_bodies "$1" "$scandir_triggers" "$scandir_markers" "" "" \
    "lint-test-isolation:scandir-exempt"
}

scandir_violations=()
while IFS= read -r f; do
  if skip_file "$f"; then continue; fi
  pkgdir="${f%/*}"
  case "$scandir_exempt_pkgs" in
    *"|$pkgdir|"*) continue;;  # package-wide TestMain decoy
  esac
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    scandir_violations+=("$v")
  done < <(scan_scandir "$f")
done < <(file_lister)

if (( ${#scandir_violations[@]} > 0 )); then
  echo "test-isolation-lint: FAIL (scan-dir isolation)" >&2
  echo "" >&2
  echo "The following test functions reach the socket scan dir (via" >&2
  echo "runDispatch / runDispatchReconcile / gc.Reconcile / runStatus)" >&2
  echo "but do NOT isolate FLEET_GC_SCAN_DIR. The reconcile OrphanTmux" >&2
  echo "pass walks that dir (default /tmp) and execs 'tmux -S <sock> ls'" >&2
  echo "on every fleet-test-*.sock — the PR #232 24-min CI hang." >&2
  echo "" >&2
  printf '  %s\n' "${scandir_violations[@]}" >&2
  echo "" >&2
  echo "Fix (any one):" >&2
  echo "  - t.Setenv(\"FLEET_GC_SCAN_DIR\", t.TempDir())  // scope the scan" >&2
  echo "  - use gc.DefaultDepsWithScanDir(dir) / stubDispatchReconcile(...)" >&2
  echo "  - give the package a TestMain that sets a FLEET_GC_SCAN_DIR decoy" >&2
  echo "  - if the test genuinely must scan real /tmp, add the comment" >&2
  echo "    // lint-test-isolation:scandir-exempt" >&2
  echo "" >&2
  echo "See docs/DESIGN-ci-3min-test-suite.md + the PR #232 postmortem." >&2
  exit 1
fi

echo "test-isolation-lint: ${files_scanned} *_test.go files scanned, 0 violations"
