#!/usr/bin/env bash
# ci-fleet-test-leak.sh — bound a CI job's /tmp/fleet-test-* footprint.
#
#   sentinel   touch /tmp/fleet-test-ci-sentinel before any test runs
#   reap       SIGKILL tmux servers bound to sockets created after the
#              sentinel, wait for them to exit, then rm the files
#   assert     exit 1 if any /tmp/fleet-test-* newer than the sentinel
#              remains
#
# Every kill is scoped to a literal socket path this run created, so a
# sibling job's tmux server on a shared runner is never touched. SIGKILL
# (not TERM) because a tmux server re-creates its socket file while
# shutting down gracefully on Linux. The `^tmux` anchor keeps pgrep from
# matching the `timeout` wrapper whose own argv carries the pattern.
set -uo pipefail

sentinel=/tmp/fleet-test-ci-sentinel

if command -v timeout >/dev/null 2>&1; then
  to() { timeout "$@"; }
else
  to() { shift; "$@"; }
fi

leaked_files() {
  to 10 find /tmp -maxdepth 1 -name 'fleet-test-*' \
    -newer "$sentinel" ! -name "$(basename "$sentinel")" 2>/dev/null || true
}

# `<sock>.sock.lock` -> `<sock>.sock`; empty when the name is a bare
# prefix (fnmatch lets `*` match nothing) so the kill stays scoped.
sock_path() {
  s="${1%.lock}"
  case "$s" in
    /tmp/fleet-test-?*) printf '%s' "$s" ;;
    *) printf '' ;;
  esac
}

kill_bound() {
  for art in $1; do
    sock=$(sock_path "$art")
    [ -n "$sock" ] || continue
    to 5 pkill -KILL -f "^tmux.*${sock}" 2>/dev/null || true
  done
}

case "${1:-}" in
  sentinel)
    touch "$sentinel"
    ;;
  reap)
    leaked=$(leaked_files)
    if [ -z "$leaked" ]; then
      echo "no /tmp/fleet-test-* cleanup needed"
      exit 0
    fi
    echo "reaping /tmp/fleet-test-* artifacts from this CI run:"
    echo "$leaked"
    kill_bound "$leaked"
    for _ in $(seq 1 100); do
      still=""
      for art in $(leaked_files); do
        sock=$(sock_path "$art")
        [ -n "$sock" ] || continue
        if to 5 pgrep -f "^tmux.*${sock}" >/dev/null 2>&1; then
          still=1
          to 5 pkill -KILL -f "^tmux.*${sock}" 2>/dev/null || true
        fi
      done
      [ -z "$still" ] && break
      sleep 0.1
    done
    sleep 1
    for _ in $(seq 1 10); do
      remaining=$(leaked_files)
      [ -z "$remaining" ] && break
      for art in $remaining; do
        rm -rf "$art" || true
      done
      sleep 0.2
    done
    ;;
  assert)
    leaked=$(leaked_files)
    if [ -n "$leaked" ]; then
      echo "::error::leaked /tmp/fleet-test-* files from this CI run:"
      echo "$leaked"
      # shellcheck disable=SC2086
      ls -la $leaked 2>/dev/null || true
      exit 1
    fi
    echo "no /tmp/fleet-test-* leak detected"
    ;;
  *)
    echo "usage: $0 sentinel|reap|assert" >&2
    exit 2
    ;;
esac
