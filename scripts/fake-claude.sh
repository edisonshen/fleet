#!/bin/sh
# fake-claude.sh — stand-in for the `claude` binary in scenarios.
#
# Same observable contract as the Go shim internal/testutil/coorde2e
# .FakeClaude writes (the parity test in that package pins the two
# together). Put the directory holding a `claude` symlink/copy of this
# file first on PATH — `scripts/scenario.sh up` does — and Fleet's
# dispatch wrapper execs it instead of a real Claude Code.
#
# Mode is read from FLEET_FAKE_CLAUDE_MODE (default ok):
#
#   ok          print `fake claude: ready pid <pid>` + a `> ` prompt, turn
#               terminal echo off, then acknowledge every stdin line as
#               `fake claude: got prompt (<n> chars)`; exit 1 when stdin
#               closes.
#   exit        print `fake claude: startup failure pid <pid>`, exit 1 —
#               "claude died at startup".
#   crash-once  first start behaves like `exit` (printing `fake claude:
#               crashing once pid <pid>`) and records the crash in
#               FLEET_FAKE_CLAUDE_STATE; every later start behaves like
#               `ok`. Models a transient launch failure that a retry
#               recovers from.
#   hang        print `fake claude: hanging pid <pid>` and block forever
#               (exec tail -f /dev/null, so the pid stays killable) without
#               ever printing the `> ` prompt or reading stdin — "claude
#               launched but never became ready".
#
# FLEET_FAKE_CLAUDE_STATE is the crash marker path for crash-once
# (default: $FLEET_HOME/fake-claude.crashed, or ~/.fleet/… when
# FLEET_HOME is unset). Delete it to re-arm the crash.
#
# Arguments (`--dangerously-skip-permissions`, prompts, …) are ignored.

mode="${FLEET_FAKE_CLAUDE_MODE:-ok}"
state="${FLEET_FAKE_CLAUDE_STATE:-${FLEET_HOME:-$HOME/.fleet}/fake-claude.crashed}"

case "$mode" in
  ok) ;;
  exit)
    echo "fake claude: startup failure pid $$"
    exit 1
    ;;
  crash-once)
    if [ ! -e "$state" ]; then
      : > "$state"
      echo "fake claude: crashing once pid $$"
      exit 1
    fi
    ;;
  hang)
    echo "fake claude: hanging pid $$"
    exec tail -f /dev/null
    ;;
  *)
    echo "fake claude: unknown FLEET_FAKE_CLAUDE_MODE=$mode" >&2
    exit 2
    ;;
esac

echo "fake claude: ready pid $$"
echo "> "
stty -echo 2>/dev/null
while IFS= read -r line; do
  echo "fake claude: got prompt (${#line} chars)"
done
exit 1
