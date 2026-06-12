"""RETIRED: remote-control bootstrap for coordinator agents.

Native model (rc-default-native-startup, operator directive
2026-05-29): every coord is spawned with Claude Code's native
`--remote-control "fleet-coord-<id>-<project>"` flag baked into its
own claude argv by `fleet dispatch --coord-spawn` (and the handoff /
drain replacement paths). Remote Control is live the moment the coord
process starts — there is nothing for the coord tick to bootstrap.

What this module used to do (v0.12/v0.13), all retired:

  - spawn_daemon_if_needed() shelled out to `fleet rc up <p>
    --respawn-only --idempotent` every 30s tick to (re)spawn a
    standalone `claude remote-control` listener daemon. RETIRED: no
    standalone listener exists under the native model. The Go side
    keeps `fleet rc up --respawn-only` as a stable zero-exit no-op so
    HALF-UPGRADED installs (this old skill copy + new fleet binary)
    stay storm-free.
  - seed_inbox() wrote a one-shot ~/.fleet/inbox/<id>.md message
    nudging the session toward /remote-control. RETIRED: the session
    is already RC-attached at startup.
  - bootstrap_remote_control() orchestrated both behind a per-coord
    marker file. RETIRED: kept below as a no-op shim so older callers
    / test fixtures that monkeypatch it keep working.

Push-storm note (the reason the old design was opt-in): the v0.12
5,620-mobile-push incident came from a zombie reviewer subagent
re-running the daemon bootstrap. Under the native model there is no
bootstrap to re-run and no daemon to push through; the flag is baked
ONLY into coord spawn argv at the Go call sites (workers / Agent-tool
subagents never get it).

Opt-out: `fleet rc down <project>` writes
~/.fleet/projects/<project>/rc-disabled, which suppresses the flag on
the next coord spawn. `fleet rc up <project>` re-enables.
"""
from __future__ import annotations

# Status constants — stable strings kept for log-greppers and older
# callers. The only value the retired shims ever return is
# STATUS_NATIVE.
STATUS_NATIVE = "native_default"
# The FULL legacy constant surface is retained as inert strings so a
# half-upgraded install (pre-native loop.py copy + this module) never
# AttributeErrors mid-tick — the old loop dereferences STATUS_FAILED_*
# and BOOTSTRAP_LOG after calling bootstrap_remote_control (/review
# adversarial F7).
STATUS_OK = "ok"
STATUS_SKIPPED_MARKER = "skipped_marker"
STATUS_SKIPPED_INVALID = "skipped_invalid"
STATUS_SKIPPED_NO_ARGS = "skipped_no_args"
STATUS_SKIPPED_INBOX_BUSY = "skipped_inbox_busy"
STATUS_NOT_ENABLED = "not_enabled"
STATUS_FAILED_SEED = "failed_seed"
STATUS_FAILED_MARKER = "failed_marker"
SPAWN_OK = "ok"
SPAWN_NOT_ENABLED = "not_enabled"
SPAWN_TRANSIENT_ERROR = "transient_error"
BOOTSTRAP_LOG = "/tmp/fleet-bootstrap.log"


def bootstrap_remote_control(
    project: str,
    coord_id: str,
    *,
    fleet_home=None,
) -> str:
    """No-op shim. RC is native at coord startup; nothing to bootstrap.

    Always returns STATUS_NATIVE without touching the filesystem,
    spawning processes, or seeding the inbox.
    """
    return STATUS_NATIVE


def spawn_daemon_status(project: str = "", coord_id: str = "") -> str:
    """No-op shim. The standalone listener daemon is retired; nothing
    spawns. Always SPAWN_OK."""
    return SPAWN_OK


def spawn_daemon_if_needed(project: str = "", coord_id: str = "") -> bool:
    """No-op shim. The standalone listener daemon is retired; nothing
    spawns. Always True ("nothing to do")."""
    return True


__all__ = [
    "bootstrap_remote_control",
    "spawn_daemon_status",
    "spawn_daemon_if_needed",
    "STATUS_NATIVE",
    "STATUS_OK",
    "STATUS_SKIPPED_MARKER",
    "STATUS_SKIPPED_INVALID",
    "STATUS_SKIPPED_NO_ARGS",
    "STATUS_SKIPPED_INBOX_BUSY",
    "STATUS_NOT_ENABLED",
    "STATUS_FAILED_SEED",
    "STATUS_FAILED_MARKER",
    "SPAWN_OK",
    "SPAWN_NOT_ENABLED",
    "SPAWN_TRANSIENT_ERROR",
    "BOOTSTRAP_LOG",
]
