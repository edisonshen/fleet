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
# This lint scans every *_test.go for tmux.Spawn (or the spawn.Spawn
# wrapper that calls it) and verifies the same file references one of
# the canonical isolation markers (requireTmux / isolatedSocket /
# isolateTmuxSocket / a literal FLEET_TMUX_SOCKET t.Setenv). A file
# that calls tmux.Spawn but never mentions the isolation surface fails
# the lint — we'd rather block CI than re-leak production.
#
# Run locally: bash scripts/lint-test-isolation.sh
# Exit 0 = clean, exit 1 = at least one offender.
#
# Known limitations (acknowledged in docstring rather than worked around):
#   - We grep for substring presence. A file that LITERALLY has the
#     string "requireTmux" in a comment but never calls it would pass.
#     Acceptable: the lint is a tripwire, not a proof system. The Go
#     regression test (TestRequireTmux_SetsFleetTmuxSocket) provides
#     the runtime check.
#   - We don't follow cross-file helper resolution. A test in pkg A
#     that delegates to a helper in pkg B which sets the socket would
#     fail this lint. Today Fleet has no such helper — every
#     requireTmux is co-located with its callers. If that changes,
#     update this script and its docstring rather than carve a special
#     case (the alternative is silent regression).

set -euo pipefail

# Repo root: this script lives in scripts/ so the repo root is one
# directory up. Resolve via $0 to support being invoked from anywhere.
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

# Patterns the lint accepts as "this file isolates the socket somehow".
# Keep this list narrow: every entry is an explicit, reviewable opt-in.
isolation_markers=(
  "requireTmux"
  "isolatedSocket"
  "isolateTmuxSocket"
  "FLEET_TMUX_SOCKET"
)

# Build the marker grep pattern up front so the per-file check is fast.
marker_pattern="$(IFS='|'; echo "${isolation_markers[*]}")"

# Find every *_test.go that calls tmux.Spawn (direct) or spawn.Spawn
# (which wraps tmux.Spawn under the hood). The wrapper case matters:
# a test that calls spawn.Spawn without isolation leaks the same way
# as one that calls tmux.Spawn directly.
violations=()
spawn_pattern='tmux\.Spawn\(|spawn\.Spawn\('

# Iterate test files. Use git ls-files so we don't trip on vendored or
# untracked artifacts; fall back to find when not inside a git repo.
# Read with a while loop instead of mapfile so macOS's bash 3.2 (which
# ships without mapfile) works without bash 4+ pre-reqs.
test_files=()
file_lister() {
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files '*_test.go'
  else
    find . -name '*_test.go' -not -path './vendor/*'
  fi
}
while IFS= read -r line; do
  test_files+=("$line")
done < <(file_lister)

for f in "${test_files[@]}"; do
  # Skip if no spawn call at all. The -E flag is portable; -P (PCRE)
  # is GNU grep only.
  if ! grep -E -q "$spawn_pattern" "$f"; then
    continue
  fi
  # Spawn call present; require an isolation marker in the same file.
  if ! grep -E -q "$marker_pattern" "$f"; then
    violations+=("$f")
  fi
done

if (( ${#violations[@]} > 0 )); then
  echo "test-isolation-lint: FAIL" >&2
  echo "" >&2
  echo "The following test files call tmux.Spawn or spawn.Spawn but do NOT" >&2
  echo "isolate FLEET_TMUX_SOCKET. Production leak risk — see postmortem" >&2
  echo "docs/postmortems/2026-05-14-orphan-tmux-leak.md." >&2
  echo "" >&2
  printf '  %s\n' "${violations[@]}" >&2
  echo "" >&2
  echo "Fix: add 'requireTmux(t)' (or 'isolateTmuxSocket(t)' for tests" >&2
  echo "that don't need the handoff-tuning env vars) at the top of every" >&2
  echo "test that calls tmux.Spawn or spawn.Spawn." >&2
  exit 1
fi

echo "test-isolation-lint: ${#test_files[@]} *_test.go files scanned, 0 violations"
