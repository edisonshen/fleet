#!/usr/bin/env bash
# test_scenario_sh.sh — drives scripts/scenario.sh the way an operator would
# and asserts the Scenario contract rows S1-S4 (plus the S3 worked example)
# against the BUILT fleet binary and a real tmux server.
#
# Hermetic: HOME and TMUX_TMPDIR point at throwaway dirs so a bug that
# reaches ~/.fleet or the default tmux server shows up as a file in a dir we
# own, not as damage to the developer's machine. Runs via
# `bash scripts/tests/test_scenario_sh.sh`.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"
SLUG="tsh$$"

for tool in tmux python3 go; do
    command -v "$tool" >/dev/null 2>&1 || { echo "SKIP: $tool not installed" >&2; exit 0; }
done

passed=0
failed=0
fail() { echo "FAIL [$CASE]: $*" >&2; failed=$((failed + 1)); }
ok() { echo "PASS [$CASE]"; passed=$((passed + 1)); }
assert() { # assert <cond-description> <test-args...>
    local what="$1"; shift
    if ! test "$@"; then fail "$what"; return 1; fi
}

# Everything the harness could leak into lands under $WORK. Pin the Go
# caches first: they default from HOME and must not be re-downloaded into
# the throwaway one.
GOPATH_REAL="$(go env GOPATH)"; GOMODCACHE_REAL="$(go env GOMODCACHE)"; GOCACHE_REAL="$(go env GOCACHE)"
export GOPATH="$GOPATH_REAL" GOMODCACHE="$GOMODCACHE_REAL" GOCACHE="$GOCACHE_REAL"
WORK="$(mktemp -d -t fleet-scn-test-XXXXXX)"
export HOME="$WORK/home"
export TMUX_TMPDIR="$WORK/tmux"
mkdir -p "$HOME" "$TMUX_TMPDIR"
unset FLEET_HOME FLEET_TMUX_SOCKET FLEET_DEV_BIN FLEET_SCENARIO_PROJECT FLEET_AGENT_ID FLEET_FAKE_CLAUDE_MODE
cleanup() {
    if [[ -n "${FLEET_HOME:-}" && -d "$FLEET_HOME" ]]; then
        scripts/scenario.sh down >/dev/null 2>&1 || true
    fi
    chmod -R u+w "$WORK" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
CASE="S1 up: builds when FLEET_DEV_BIN unset, isolates HOME + socket"
out="$(scripts/scenario.sh up --slug "$SLUG" 2>"$WORK/up.err")"
eval "$out"
if assert "FLEET_HOME under /tmp/fleet-test-$SLUG-*" -n "${FLEET_HOME:-}" \
    && [[ "$FLEET_HOME" == /tmp/fleet-test-$SLUG-* ]] \
    && assert "agents/ created" -d "$FLEET_HOME/agents" \
    && assert "projects/ created" -d "$FLEET_HOME/projects" \
    && assert "project registered" -d "$FLEET_HOME/projects/demo" \
    && [[ "$FLEET_TMUX_SOCKET" == /tmp/fleet-test-$SLUG-*.sock ]] \
    && assert "FLEET_DEV_BIN built into sandbox" -x "$FLEET_DEV_BIN" \
    && [[ "$FLEET_DEV_BIN" == "$FLEET_HOME/bin/fleet" ]] \
    && assert "fake claude first on PATH" "$(command -v claude)" = "$FLEET_HOME/bin/claude" \
    && assert "export lines only on stdout" "$(grep -vc '^export ' <<<"$out")" = 0 \
    && assert "~/.fleet untouched" ! -e "$HOME/.fleet"; then
    ok
else
    [[ "$FLEET_HOME" == /tmp/fleet-test-$SLUG-* ]] || fail "FLEET_HOME=$FLEET_HOME"
    [[ "$FLEET_TMUX_SOCKET" == /tmp/fleet-test-$SLUG-*.sock ]] || fail "FLEET_TMUX_SOCKET=$FLEET_TMUX_SOCKET"
    [[ "$FLEET_DEV_BIN" == "$FLEET_HOME/bin/fleet" ]] || fail "FLEET_DEV_BIN=$FLEET_DEV_BIN"
fi
# Keep the built binary for later `up` calls so they don't rebuild.
cp "$FLEET_DEV_BIN" "$WORK/fleet"
KEPT_BIN="$WORK/fleet"

# ---------------------------------------------------------------------------
CASE="S2 run: fenced output with command + exit code, propagates failure"
rc=0
run_out="$(scripts/scenario.sh run -- sh -c 'echo hello from sandbox; echo oops >&2; exit 3' 2>&1)" || rc=$?
if [[ "$rc" == 3 ]] \
    && [[ "$(sed -n 1p <<<"$run_out")" == '```text' ]] \
    && [[ "$(sed -n 2p <<<"$run_out")" == '$ sh -c echo hello from sandbox; echo oops >&2; exit 3' ]] \
    && grep -q '^hello from sandbox$' <<<"$run_out" \
    && grep -q '^oops$' <<<"$run_out" \
    && grep -q '^exit=3$' <<<"$run_out" \
    && [[ "$(tail -n1 <<<"$run_out")" == '```' ]]; then
    ok
else
    fail "rc=$rc output:"; sed 's/^/    /' <<<"$run_out" >&2
fi

CASE="S2 run: command sees the sandbox env"
env_out="$(scripts/scenario.sh run -- sh -c 'echo "$FLEET_HOME"; command -v fleet' 2>&1)"
if grep -qx "$FLEET_HOME" <<<"$env_out" && grep -qx "$FLEET_HOME/bin/fleet" <<<"$env_out"; then
    ok
else
    fail "env not propagated:"; sed 's/^/    /' <<<"$env_out" >&2
fi

# ---------------------------------------------------------------------------
CASE="S3 seed-coord --pct 41 + seed-worker: real Stop hook holds, no spawn-fresh"
eval "$(scripts/scenario.sh seed-coord --pct 41 2>/dev/null)"
scripts/scenario.sh seed-worker x --phase verify 2>/dev/null
pane="$(scripts/scenario.sh capture)"
hook_rc=0
hook_out="$(scripts/scenario.sh run -- python3 skills/fleet-guard/main.py < "$FLEET_HOME/.scenario/stop.json" 2>&1)" || hook_rc=$?
wid="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["worker_agent_ids"]["x"])' \
    "$FLEET_HOME/projects/demo/coord-state.json")"
want="fleet-guard: soft handoff for coord $FLEET_AGENT_ID holding — 1 subagent(s) still in flight: x=$wid"
if [[ "$hook_rc" == 0 ]] \
    && grep -q 'HANDOFF REQUESTED' <<<"$pane" && grep -q 'MILESTONE' <<<"$pane" \
    && grep -qF -- "$want" <<<"$hook_out" \
    && grep -q '^exit=0$' <<<"$hook_out" \
    && [[ -z "$(ls "$FLEET_HOME"/queue/spawn-fresh-*.json 2>/dev/null)" ]] \
    && assert "agent record on the sandbox" -f "$FLEET_HOME/agents/$FLEET_AGENT_ID.json" \
    && assert "no session on the default tmux server" ! -e "$TMUX_TMPDIR/tmux-$(id -u)/default" \
    && assert "~/.fleet untouched by the hook" ! -e "$HOME/.fleet"; then
    ok
else
    fail "want line: $want"; echo "  pane:" >&2; sed 's/^/    /' <<<"$pane" >&2
    echo "  hook:" >&2; sed 's/^/    /' <<<"$hook_out" >&2
    ls "$FLEET_HOME/queue" >&2 || true
fi

CASE="S3 control: same hook with no worker in flight performs the handoff"
rm -f "$FLEET_HOME/projects/demo/coord-state.json"
rm -rf "$FLEET_HOME/projects/demo/workers"
ctl_out="$(scripts/scenario.sh run -- python3 skills/fleet-guard/main.py < "$FLEET_HOME/.scenario/stop.json" 2>&1)" || true
if [[ -n "$(ls "$FLEET_HOME"/queue/spawn-fresh-"$FLEET_AGENT_ID".json 2>/dev/null)" ]] \
    && ! grep -q 'holding' <<<"$ctl_out"; then
    ok
else
    fail "expected spawn-fresh-$FLEET_AGENT_ID.json:"; sed 's/^/    /' <<<"$ctl_out" >&2
    ls "$FLEET_HOME/queue" >&2 || true
fi

# ---------------------------------------------------------------------------
CASE="capture: a missing session is a failure (non-zero exit), fence still closed"
rc=0; cap_out="$(scripts/scenario.sh capture fleet-nosuch 2>&1)" || rc=$?
if assert "exit!=0" "$rc" -ne 0 \
    && [[ "$(tail -n1 <<<"$cap_out")" == '```' ]]; then
    ok
else
    fail "rc=$rc"; sed 's/^/    /' <<<"$cap_out" >&2
fi

# ---------------------------------------------------------------------------
CASE="S4 down: refuses a FLEET_TMUX_SOCKET that is not this sandbox's"
other="/tmp/fleet-test-$SLUG-other.sock"
tmux -S "$other" new-session -d -s bystander sleep 60
rc=0
FLEET_TMUX_SOCKET="$other" scripts/scenario.sh down >/dev/null 2>"$WORK/down-sock.err" || rc=$?
if assert "exit=2" "$rc" -eq 2 \
    && assert "sandbox home kept" -d "$FLEET_HOME" \
    && assert "bystander server untouched" "$(tmux -S "$other" ls 2>/dev/null | grep -c '^bystander:')" = 1 \
    && grep -q 'does not belong to FLEET_HOME' "$WORK/down-sock.err"; then
    ok
else
    fail "rc=$rc"; sed 's/^/    /' "$WORK/down-sock.err" >&2
fi
tmux -S "$other" kill-server 2>/dev/null || true
rm -f "$other"

# ---------------------------------------------------------------------------
CASE="S4 down: removes home + socket, kills sandbox tmux, no debris"
sock="$FLEET_TMUX_SOCKET"; home="$FLEET_HOME"
tmux -S "$sock" has-session -t "fleet-$FLEET_AGENT_ID" 2>/dev/null || fail "precondition: coord session missing"
down_out="$(scripts/scenario.sh down)"
if assert "FLEET_HOME removed" ! -e "$home" \
    && assert "socket removed" ! -e "$sock" \
    && ! tmux -S "$sock" ls >/dev/null 2>&1 \
    && assert "no /tmp/fleet-test-$SLUG-* debris" -z "$(ls -d /tmp/fleet-test-"$SLUG"-* 2>/dev/null)" \
    && grep -q '^unset FLEET_HOME FLEET_TMUX_SOCKET FLEET_DEV_BIN ' <<<"$down_out"; then
    ok
else
    fail "down left: $(ls -d /tmp/fleet-test-"$SLUG"-* 2>/dev/null); down_out=$down_out"
fi
eval "$down_out"

CASE="up: a stale FLEET_DEV_BIN inside a torn-down sandbox is ignored, not rebuilt there"
export FLEET_DEV_BIN="$home/bin/fleet"   # what a shell that skipped `eval "$(down)"` still holds
eval "$(scripts/scenario.sh up --slug "$SLUG" 2>"$WORK/up-stale.err")"
if assert "old home not recreated" ! -e "$home" \
    && [[ "$FLEET_DEV_BIN" == "$FLEET_HOME/bin/fleet" ]] \
    && grep -q 'ignoring stale FLEET_DEV_BIN' "$WORK/up-stale.err"; then
    ok
else
    fail "FLEET_DEV_BIN=$FLEET_DEV_BIN old=$home"; sed 's/^/    /' "$WORK/up-stale.err" >&2
fi
eval "$(scripts/scenario.sh down)"

CASE="S4 down: refuses a FLEET_HOME that is not a sandbox"
rc=0
FLEET_HOME="$HOME/.fleet-real" FLEET_TMUX_SOCKET=/tmp/nope scripts/scenario.sh down >/dev/null 2>"$WORK/down.err" || rc=$?
mkdir -p "$HOME/.fleet-real/.scenario"; : > "$HOME/.fleet-real/.scenario/marker"
rc2=0
FLEET_HOME="$HOME/.fleet-real" FLEET_TMUX_SOCKET="$HOME/.fleet-real.sock" scripts/scenario.sh down >/dev/null 2>>"$WORK/down.err" || rc2=$?
if [[ "$rc" == 2 && "$rc2" == 2 ]] && [[ -d "$HOME/.fleet-real" ]] \
    && grep -q 'not a scenario sandbox' "$WORK/down.err" \
    && grep -q 'outside /tmp/fleet-test-' "$WORK/down.err"; then
    ok
else
    fail "rc=$rc rc2=$rc2"; sed 's/^/    /' "$WORK/down.err" >&2
fi

# ---------------------------------------------------------------------------
CASE="up reuses FLEET_DEV_BIN; fake claude honours FLEET_FAKE_CLAUDE_MODE=hang"
export FLEET_DEV_BIN="$KEPT_BIN"
eval "$(scripts/scenario.sh up --slug "$SLUG" 2>/dev/null)"
if [[ "$FLEET_DEV_BIN" == "$KEPT_BIN" ]] && [[ "$(readlink "$FLEET_HOME/bin/fleet")" == "$KEPT_BIN" ]]; then
    FLEET_FAKE_CLAUDE_MODE=hang scripts/scenario.sh seed-coord --pct 12 >/dev/null 2>&1
    sleep 1
    pane="$(scripts/scenario.sh capture)"
    if grep -q 'fake claude: hanging pid' <<<"$pane" && ! grep -q 'fake claude: ready' <<<"$pane"; then
        ok
    else
        fail "pane:"; sed 's/^/    /' <<<"$pane" >&2
    fi
else
    fail "FLEET_DEV_BIN=$FLEET_DEV_BIN link=$(readlink "$FLEET_HOME/bin/fleet" || true)"
fi
scripts/scenario.sh down >/dev/null
unset FLEET_HOME FLEET_TMUX_SOCKET FLEET_AGENT_ID

# ---------------------------------------------------------------------------
CASE="up: FLEET_DEV_BIN naming a missing path is built there (not an error)"
export FLEET_DEV_BIN="$WORK/build-here/fleet"
eval "$(scripts/scenario.sh up --slug "$SLUG" 2>"$WORK/up-missing.err")"
if assert "binary built at the requested path" -x "$WORK/build-here/fleet" \
    && [[ "$(readlink "$FLEET_HOME/bin/fleet")" == "$WORK/build-here/fleet" ]]; then
    ok
else
    fail "FLEET_DEV_BIN=$FLEET_DEV_BIN"; sed 's/^/    /' "$WORK/up-missing.err" >&2
fi
scripts/scenario.sh down >/dev/null
unset FLEET_HOME FLEET_TMUX_SOCKET FLEET_AGENT_ID

CASE="up: a relative FLEET_DEV_BIN is linked by absolute path"
mkdir -p "$WORK/rel"; cp "$KEPT_BIN" "$WORK/rel/fleet"
eval "$(cd "$WORK" && FLEET_DEV_BIN=./rel/fleet "$REPO_ROOT/scripts/scenario.sh" up --slug "$SLUG" 2>/dev/null)"
if [[ "$FLEET_DEV_BIN" == "$WORK/rel/fleet" ]] \
    && [[ "$(readlink "$FLEET_HOME/bin/fleet")" == "$WORK/rel/fleet" ]] \
    && assert "fleet resolves through the sandbox PATH" -x "$FLEET_HOME/bin/fleet"; then
    ok
else
    fail "FLEET_DEV_BIN=$FLEET_DEV_BIN link=$(readlink "$FLEET_HOME/bin/fleet" || true)"
fi
scripts/scenario.sh down >/dev/null
unset FLEET_HOME FLEET_TMUX_SOCKET FLEET_AGENT_ID FLEET_DEV_BIN

CASE="up: FLEET_SCENARIO_PROJECT with path characters is refused, no sandbox left"
rc=0; out="$(FLEET_SCENARIO_PROJECT='../evil' scripts/scenario.sh up --slug "$SLUG" 2>"$WORK/up-proj.err")" || rc=$?
if assert "exit=2" "$rc" -eq 2 \
    && assert "no export lines" -z "$out" \
    && grep -q 'FLEET_SCENARIO_PROJECT=../evil must match' "$WORK/up-proj.err" \
    && assert "no /tmp/fleet-test-$SLUG-* left" -z "$(ls -d /tmp/fleet-test-"$SLUG"-* 2>/dev/null)"; then
    ok
else
    fail "rc=$rc"; sed 's/^/    /' "$WORK/up-proj.err" >&2
fi

CASE="fake-claude crash-once: default marker dir is created under a clean HOME"
mkdir -p "$WORK/cleanhome"
rc1=0; out1="$(env -u FLEET_HOME HOME="$WORK/cleanhome" FLEET_FAKE_CLAUDE_MODE=crash-once scripts/fake-claude.sh </dev/null 2>&1)" || rc1=$?
out2="$(env -u FLEET_HOME HOME="$WORK/cleanhome" FLEET_FAKE_CLAUDE_MODE=crash-once scripts/fake-claude.sh </dev/null 2>&1)" || true
if assert "first start exits 1" "$rc1" -eq 1 \
    && grep -q 'fake claude: crashing once' <<<"$out1" \
    && assert "marker written" -e "$WORK/cleanhome/.fleet/fake-claude.crashed" \
    && grep -q 'fake claude: ready' <<<"$out2"; then
    ok
else
    fail "rc1=$rc1"; sed 's/^/    1: /' <<<"$out1" >&2; sed 's/^/    2: /' <<<"$out2" >&2
fi

CASE="up: a failing up (non-executable FLEET_DEV_BIN) exits 2 and leaves no sandbox"
: > "$WORK/not-a-binary"
export FLEET_DEV_BIN="$WORK/not-a-binary"
rc=0; out="$(scripts/scenario.sh up --slug "$SLUG" 2>"$WORK/up-bad.err")" || rc=$?
if assert "exit=2" "$rc" -eq 2 \
    && assert "no export lines" -z "$out" \
    && grep -q 'is not executable' "$WORK/up-bad.err" \
    && assert "no /tmp/fleet-test-$SLUG-* left by the failed up" -z "$(ls -d /tmp/fleet-test-"$SLUG"-* 2>/dev/null)"; then
    ok
else
    fail "rc=$rc"; sed 's/^/    /' "$WORK/up-bad.err" >&2; ls -d /tmp/fleet-test-"$SLUG"-* 2>/dev/null >&2 || true
fi
unset FLEET_DEV_BIN

CASE="isolation: default tmux server and ~/.fleet never touched"
sleep 1  # let any detached drain from the hook cases finish
if assert "no default tmux socket dir" ! -e "$TMUX_TMPDIR/tmux-$(id -u)" \
    && assert "~/.fleet absent" ! -e "$HOME/.fleet" \
    && assert "no /tmp/fleet-test-$SLUG-* debris after all downs" -z "$(ls -d /tmp/fleet-test-"$SLUG"-* 2>/dev/null)"; then
    ok
fi

echo
echo "test_scenario_sh: $passed passed, $failed failed"
[[ "$failed" == 0 ]]
