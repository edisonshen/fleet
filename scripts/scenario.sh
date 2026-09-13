#!/usr/bin/env bash
# scenario.sh — stand up a throwaway Fleet sandbox and drive the BUILT
# product through it, capturing pasteable evidence.
#
# Everything runs under an isolated FLEET_HOME and an isolated tmux socket;
# ~/.fleet and the operator's default tmux server are never touched. See
# --help for the subcommands, env vars, output format and a worked example.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
SELF="scripts/scenario.sh"

usage() {
    cat <<'EOF'
usage: scripts/scenario.sh <subcommand> [args]

Stand up an isolated Fleet sandbox, run the BUILT binary (and the real
fleet-guard / coordinator skills) against it, and capture what an operator
would see. Nothing here touches ~/.fleet or the default tmux server.

SUBCOMMANDS
  up [--slug <s>]        create the sandbox and print `export` lines:
                           eval "$(scripts/scenario.sh up)"
                         - FLEET_HOME=$(mktemp -d /tmp/fleet-test-<slug>-XXXXXX)
                           with agents/ and projects/, a project
                           "$FLEET_SCENARIO_PROJECT" (default: demo) already
                           registered on a non-git repo dir
                         - FLEET_TMUX_SOCKET=/tmp/fleet-test-<slug>-XXXXXX.sock
                           (same canonical path shape the Go tests use)
                         - builds ./cmd/fleet into $FLEET_DEV_BIN when that
                           is unset (-> $FLEET_HOME/bin/fleet) or names a
                           path that does not exist yet; reuses it otherwise.
                           Fails (and removes the sandbox) on a bad binary
                         - puts $FLEET_HOME/bin first on PATH: `fleet` is the
                           dev binary, `claude` is scripts/fake-claude.sh
  seed-coord [--pct N] [--dead]
                         plant a coordinator record for the project. Live
                         (default) also spawns its fleet-<id> tmux session on
                         the sandbox socket running `claude`. --pct N stamps
                         context_pct=N; at N >= 40 the record carries
                         handoff_type=auto-yellow and the pane already shows
                         `HANDOFF REQUESTED:` -> `⏺ MILESTONE` (the soft-
                         handoff state). Also writes a Stop-hook payload at
                         N% to $FLEET_HOME/.scenario/stop.json. --dead writes
                         the record only, spawned_at 72h ago, no session.
  seed-worker <slug> --phase <p>
                         record a worker the coord has in flight: coord-
                         state.json:worker_agent_ids[<slug>] plus
                         projects/<project>/workers/<slug>/state.json.
  run -- <cmd...>        run <cmd> inside the sandbox env (FLEET_AGENT_ID set
                         to the seeded coord) and print fenced evidence:
                             ```text
                             $ <cmd>
                             <stdout+stderr>
                             exit=<code>
                             ```
                         run itself exits with <code>.
  capture [<session>]    fenced `tmux capture-pane -p` of a session on the
                         sandbox socket (default: the seeded coord's).
  down                   kill every session on the sandbox socket, remove
                         the socket and FLEET_HOME, print matching `unset`
                         lines. Leaves no /tmp/fleet-test-* debris.

ENVIRONMENT
  FLEET_HOME              sandbox state root (set by up; required after)
  FLEET_TMUX_SOCKET       sandbox tmux socket path (set by up)
  FLEET_DEV_BIN           fleet binary to use; built by up when unset/missing
  FLEET_SCENARIO_PROJECT  project tag seeded by up (default: demo)
  FLEET_FAKE_CLAUDE_MODE  ok|exit|crash-once|hang — see scripts/fake-claude.sh
  FLEET_AGENT_ID          set by `run` from the seeded coord if unset

WORKED EXAMPLE — coord at 41% holds its soft handoff while a worker is out
  eval "$(scripts/scenario.sh up)"
  scripts/scenario.sh seed-coord --pct 41
  scripts/scenario.sh seed-worker x --phase verify
  scripts/scenario.sh run -- python3 skills/fleet-guard/main.py < "$FLEET_HOME/.scenario/stop.json"
  # stderr: fleet-guard: soft handoff for coord <id> holding — 1 subagent(s) still in flight: x=<worker-id>
  # and no spawn-fresh-*.json under $FLEET_HOME/queue/
  scripts/scenario.sh down

Paste the fenced blocks from `run` / `capture` straight into the PR's
Evidence — before / Evidence — after sections.
EOF
}

die() { echo "$SELF: $*" >&2; exit 2; }

need_sandbox() {
    [[ -n "${FLEET_HOME:-}" ]] || die "FLEET_HOME is not set — run: eval \"\$($SELF up)\""
    [[ -f "$FLEET_HOME/.scenario/marker" ]] || die "FLEET_HOME=$FLEET_HOME is not a scenario sandbox (no .scenario/marker); refusing"
    [[ -n "${FLEET_TMUX_SOCKET:-}" ]] || die "FLEET_TMUX_SOCKET is not set"
    export PATH="$FLEET_HOME/bin:$PATH"
    FLEET_SCENARIO_PROJECT="${FLEET_SCENARIO_PROJECT:-demo}"
}

project_dir() { echo "$FLEET_HOME/projects/$FLEET_SCENARIO_PROJECT"; }

coord_id() {
    [[ -f "$FLEET_HOME/.scenario/coord-id" ]] || return 1
    cat "$FLEET_HOME/.scenario/coord-id"
}

fence_open() { echo '```text'; }
fence_close() { echo '```'; }

cmd_up() {
    local slug=scn
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --slug) slug="${2:?--slug needs a value}"; shift 2 ;;
            *) die "up: unknown argument $1" ;;
        esac
    done
    [[ "$slug" =~ ^[A-Za-z0-9_-]+$ ]] || die "up: --slug must match [A-Za-z0-9_-]+"
    local project="${FLEET_SCENARIO_PROJECT:-demo}"

    local home
    home=$(mktemp -d "/tmp/fleet-test-${slug}-XXXXXX")
    local sock="$home.sock"
    # A failed `up` must not leave a half-built sandbox behind.
    trap '[[ $? -eq 0 ]] || rm -rf "$home" "$sock"' EXIT
    mkdir -p "$home/agents" "$home/projects" "$home/bin" "$home/repo" "$home/.scenario"
    : > "$home/.scenario/marker"

    local bin="${FLEET_DEV_BIN:-}"
    if [[ -z "$bin" ]]; then
        bin="$home/bin/fleet"
    fi
    if [[ ! -e "$bin" ]]; then
        mkdir -p "$(dirname "$bin")"
        (cd "$REPO_ROOT" && go build -o "$bin" ./cmd/fleet) || die "up: go build ./cmd/fleet failed"
    fi
    [[ -x "$bin" ]] || die "up: FLEET_DEV_BIN=$bin is not executable"
    [[ "$bin" == "$home/bin/fleet" ]] || ln -s "$bin" "$home/bin/fleet"
    ln -s "$SCRIPT_DIR/fake-claude.sh" "$home/bin/claude"

    FLEET_HOME="$home" FLEET_TMUX_SOCKET="$sock" PATH="$home/bin:$PATH" \
        "$bin" project add --project "$project" "$home/repo" >/dev/null \
        || die "up: fleet project add failed"

    echo "export FLEET_HOME=$home"
    echo "export FLEET_TMUX_SOCKET=$sock"
    echo "export FLEET_DEV_BIN=$bin"
    echo "export FLEET_SCENARIO_PROJECT=$project"
    echo "export PATH=$home/bin:\$PATH"
}

# stop_payload <pct> — a Stop-hook payload whose transcript puts the agent at
# <pct>% of a 200k model (claude-sonnet-4-5): input_tokens = pct * 2000.
write_stop_payload() {
    local pct="$1" dir="$FLEET_HOME/.scenario"
    local tokens=$((pct * 2000))
    printf '{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":%d}}}\n' \
        "$tokens" > "$dir/transcript.jsonl"
    printf '{"hook_event_name":"Stop","session_id":"scenario","transcript_path":"%s"}\n' \
        "$dir/transcript.jsonl" > "$dir/stop.json"
}

cmd_seed_coord() {
    need_sandbox
    local pct=0 dead=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --pct) pct="${2:?--pct needs a value}"; shift 2 ;;
            --dead) dead=1; shift ;;
            *) die "seed-coord: unknown argument $1" ;;
        esac
    done
    [[ "$pct" =~ ^[0-9]+$ ]] || die "seed-coord: --pct must be an integer"
    local id
    id=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
    local session="fleet-$id" now spawned
    now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    spawned="$now"
    if [[ $dead -eq 1 ]]; then
        spawned=$(date -u -d '72 hours ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
            || date -u -v-72H +%Y-%m-%dT%H:%M:%SZ)
    fi

    if [[ $dead -eq 0 ]]; then
        local pane="$FLEET_HOME/.scenario/coord-pane.sh"
        {
            echo '#!/bin/sh'
            if [[ $pct -ge 40 ]]; then
                # What the pane shows once the 40% soft handoff was requested
                # and the coord answered with its MILESTONE turn.
                echo "printf '> HANDOFF REQUESTED: context window is over 40%% — SOFT\\n\\n⏺ MILESTONE\\n\\n'"
            fi
            echo 'exec claude'
        } > "$pane"
        FLEET_AGENT_ID="$id" tmux -S "$FLEET_TMUX_SOCKET" new-session -d -s "$session" \
            -c "$FLEET_HOME/repo" "sh $pane" \
            || die "seed-coord: tmux new-session failed"
    fi

    python3 - "$FLEET_HOME/agents/$id.json" "$id" "$FLEET_SCENARIO_PROJECT" "$session" \
        "$now" "$spawned" "$pct" "$dead" "$FLEET_HOME/repo" <<'PY'
import json, os, sys
path, aid, project, session, now, spawned, pct, dead, cwd = sys.argv[1:]
rec = {
    "schema_version": 2,
    "id": aid,
    "tmux_session": session,
    "engine": "claude-code",
    "role": "executor",
    "mode": "execute",
    "task_id": "coord-" + project,
    "project": project,
    "context_source": "",
    "last_activity_ts": now,
    "handoff_number": 1,
    "spawned_at": spawned,
    "is_coord": True,
}
if dead == "0":
    rec["cwd"] = cwd
    rec["command"] = ["claude"]
if int(pct) > 0:
    rec["context_pct"] = float(pct)
    rec["context_source"] = "hook"
    if int(pct) >= 40:
        rec["handoff_type"] = "auto-yellow"
        rec["handoff_type_at"] = now
tmp = path + ".tmp"
with open(tmp, "w", encoding="utf-8") as fh:
    json.dump(rec, fh, indent=2)
    fh.write("\n")
os.replace(tmp, path)
PY
    echo "$id" > "$FLEET_HOME/.scenario/coord-id"
    write_stop_payload "$pct"
    echo "seeded coord $id (project=$FLEET_SCENARIO_PROJECT pct=$pct dead=$dead session=$session)" >&2
    echo "export FLEET_AGENT_ID=$id"
}

cmd_seed_worker() {
    need_sandbox
    local slug="${1:-}" phase=""
    [[ -n "$slug" && "$slug" != --* ]] || die "seed-worker: usage: seed-worker <slug> --phase <p>"
    shift
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --phase) phase="${2:?--phase needs a value}"; shift 2 ;;
            *) die "seed-worker: unknown argument $1" ;;
        esac
    done
    [[ -n "$phase" ]] || die "seed-worker: --phase is required"
    local pdir wid
    pdir=$(project_dir)
    [[ -d "$pdir" ]] || die "seed-worker: project dir $pdir missing (did up run?)"
    wid=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
    mkdir -p "$pdir/workers/$slug"
    python3 - "$pdir" "$slug" "$wid" "$phase" <<'PY'
import json, os, sys
pdir, slug, wid, phase = sys.argv[1:]
cs_path = os.path.join(pdir, "coord-state.json")
try:
    with open(cs_path, encoding="utf-8") as fh:
        cs = json.load(fh)
except (OSError, ValueError):
    cs = {}
cs.setdefault("worker_agent_ids", {})[slug] = wid
for path, obj in (
    (cs_path, cs),
    (os.path.join(pdir, "workers", slug, "state.json"),
     {"phase": phase, "pr_url": "https://example.invalid/pr/7"}),
):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(obj, fh, indent=2)
        fh.write("\n")
    os.replace(tmp, path)
PY
    echo "seeded worker $slug=$wid phase=$phase" >&2
}

cmd_run() {
    need_sandbox
    [[ "${1:-}" == "--" ]] && shift
    [[ $# -gt 0 ]] || die "run: usage: run -- <cmd...>"
    if [[ -z "${FLEET_AGENT_ID:-}" ]]; then
        local cid
        if cid=$(coord_id); then
            export FLEET_AGENT_ID="$cid"
        fi
    fi
    local rc=0
    fence_open
    echo "\$ $*"
    "$@" 2>&1 || rc=$?
    echo "exit=$rc"
    fence_close
    return "$rc"
}

cmd_capture() {
    need_sandbox
    local session="${1:-}"
    if [[ -z "$session" ]]; then
        session="fleet-$(coord_id || die "capture: no session given and no coord seeded")"
    fi
    fence_open
    echo "\$ tmux -S \$FLEET_TMUX_SOCKET capture-pane -p -t $session"
    tmux -S "$FLEET_TMUX_SOCKET" capture-pane -p -t "$session" || true
    fence_close
}

# Linux best-effort: pids of `fleet` processes whose environment carries this
# sandbox's FLEET_HOME (a detached drain, a coord-run supervisor).
sandbox_fleet_pids() {
    [[ -d /proc/self ]] || return 0
    local p pid arg0
    for p in /proc/[0-9]*; do
        pid="${p#/proc/}"
        [[ "$pid" == "$$" ]] && continue
        arg0=$( { tr '\0' '\n' < "$p/cmdline" | head -n1; } 2>/dev/null ) || continue
        [[ "${arg0##*/}" == fleet ]] || continue
        if { tr '\0' '\n' < "$p/environ" | grep -qx -- "FLEET_HOME=$FLEET_HOME"; } 2>/dev/null; then
            echo "$pid"
        fi
    done
}

wait_sandbox_fleet_procs() {
    local i pids
    for i in $(seq 1 50); do
        pids=$(sandbox_fleet_pids)
        [[ -n "$pids" ]] || return 0
        sleep 0.1
    done
    echo "$SELF: down: terminating lingering fleet process(es): ${pids//$'\n'/ }" >&2
    # shellcheck disable=SC2086
    kill -TERM $pids 2>/dev/null || true
    sleep 0.5
}

cmd_down() {
    need_sandbox
    case "$FLEET_HOME" in
        /tmp/fleet-test-*) ;;
        *) die "down: FLEET_HOME=$FLEET_HOME is outside /tmp/fleet-test-*; refusing to remove" ;;
    esac
    if [[ -S "$FLEET_TMUX_SOCKET" ]]; then
        tmux -S "$FLEET_TMUX_SOCKET" kill-server 2>/dev/null || true
    fi
    rm -f "$FLEET_TMUX_SOCKET" "$FLEET_TMUX_SOCKET.lock"
    wait_sandbox_fleet_procs
    # A detached `fleet drain` kicked by the hook can still be writing
    # logs/ and .locks/ for a moment: keep removing until the tree stays
    # gone for half a second.
    local i quiet=0
    for i in $(seq 1 50); do
        rm -rf "$FLEET_HOME" 2>/dev/null || true
        sleep 0.1
        if [[ -e "$FLEET_HOME" ]]; then quiet=0; else quiet=$((quiet + 1)); fi
        [[ $quiet -ge 5 ]] && break
    done
    [[ ! -e "$FLEET_HOME" ]] || die "down: $FLEET_HOME still present"
    echo "unset FLEET_HOME FLEET_TMUX_SOCKET FLEET_SCENARIO_PROJECT FLEET_AGENT_ID"
}

main() {
    local sub="${1:-}"
    [[ $# -gt 0 ]] && shift
    case "$sub" in
        up) cmd_up "$@" ;;
        seed-coord) cmd_seed_coord "$@" ;;
        seed-worker) cmd_seed_worker "$@" ;;
        run) cmd_run "$@" ;;
        capture) cmd_capture "$@" ;;
        down) cmd_down "$@" ;;
        -h|--help|help) usage ;;
        "") usage >&2; exit 2 ;;
        *) die "unknown subcommand '$sub' (see --help)" ;;
    esac
}

main "$@"
