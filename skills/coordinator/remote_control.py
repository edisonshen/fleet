"""Remote-control auto-injection for fresh coordinator agents.

Issue #56: every coordinator agent — fresh or replacement-from-handoff —
must connect itself to `claude remote-control` so the operator's mobile /
claude.ai pairing carries through. Handoffs already cover the resumption
path via internal/handoff.FirstAction. This module covers the FRESH path:
the very first tick after a coord agent boots, before it's ever rendered
a handoff doc.

Mechanism (two parts, both gated by one per-coord marker file so we only
do it once per coord lifetime):

  1. spawn_daemon_if_needed() — start `claude remote-control` in the
     background, idempotent across the whole machine via `pgrep -f`.
     Same exact bash form that internal/handoff.FirstAction emits, so
     fresh-coord and handoff-replacement converge on the same daemon.
     Log: /tmp/claude-rc-coord.log.

  2. seed_inbox() — write a one-shot inbox message at
     ~/.fleet/inbox/<coord_id>.md telling the agent to run the slash
     command `/remote-control`. fleet-guard's inbox-relay
     (skills/fleet-guard/inbox.py + main._on_stop) delivers it on the
     next Stop hook fire as `[OPERATOR] <body>` and archives the file.
     The agent sees the [OPERATOR] line, recognizes the slash command,
     and runs /remote-control to attach this session to the daemon.

Both gated by the marker file
~/.fleet/projects/<project>/.remote-control-bootstrap-<coord_id>; once
written, subsequent ticks short-circuit. Idempotent + fail-soft: any
I/O failure logs to stderr and proceeds. The bootstrap NEVER blocks the
coord tick — same discipline as fleet-guard.

ASCII diagram of the fresh-coord flow:

    coord agent boots → first loop.tick() → bootstrap_remote_control(p, id)
                                              │
                                              ▼
                          marker exists? ── yes ─► return (no-op)
                                              │ no
                                              ▼
                       spawn_daemon_if_needed()
                          (pgrep guards re-launch)
                                              │
                                              ▼
                       seed_inbox(coord_id, project)
                          writes ~/.fleet/inbox/<id>.md
                                              │
                                              ▼
                       write_marker(project, coord_id)
                          ~/.fleet/projects/<p>/.remote-control-bootstrap-<id>
                                              │
                                              ▼
                       next Stop hook fires → fleet-guard inbox.deliver()
                          injects [OPERATOR] Run `/remote-control` ...
                                              │
                                              ▼
                       agent runs /remote-control slash command
                          → session attaches to daemon
"""
from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from pathlib import Path


# Path-to-log for the background daemon. Mirrors handoff.FirstAction's
# /tmp/claude-rc-handoff.log naming so log inspection by the operator
# stays predictable; fresh-coord vs handoff is encoded in the suffix.
_DAEMON_LOG = "/tmp/claude-rc-coord.log"

# Inbox body delivered to the coord agent on the next Stop hook. The
# fleet-guard relay wraps this with `[OPERATOR] ` prefix automatically
# (see skills/fleet-guard/inbox.py:deliver). Body must include the
# literal slash command `/remote-control` so the agent recognizes and
# runs it.
_INBOX_BODY = (
    "Run the slash command `/remote-control` (in the chat, not bash) "
    "to attach this coordinator session to its remote-control daemon. "
    "This lets the operator pair the session via mobile / claude.ai."
)


def bootstrap_remote_control(
    project: str,
    coord_id: str,
    *,
    fleet_home: Path | None = None,
) -> bool:
    """One-shot bootstrap for fresh coordinator agents.

    Returns True iff this call did the bootstrap (daemon-spawn attempt +
    inbox-seed + marker write). Returns False on any of:
      - missing project / coord_id (silently noop — caller's signal that
        we're not under fleet supervision yet),
      - marker file already present (we already bootstrapped),
      - I/O failure writing the marker (logged; treated as "tried but
        couldn't persist", so the next tick retries).

    Failures in spawn_daemon_if_needed / seed_inbox do NOT abort the
    bootstrap: each is independently fail-soft. The marker is written
    iff BOTH the inbox seed succeeded AND we attempted the daemon spawn
    — we don't want to mark "done" if the agent never got the inbox
    message; conversely, a failed pgrep / spawn is acceptable (operator
    may have started the daemon manually, or it's not on PATH yet).
    """
    if not project or not coord_id:
        return False
    home = _resolve_home(fleet_home)
    marker = _marker_path(home, project, coord_id)
    if marker.exists():
        return False
    # Order: daemon first, inbox second. The inbox message tells the
    # agent to run /remote-control which connects to a daemon — if we
    # seed the inbox before launching the daemon, there's a small
    # window where /remote-control fires with no daemon listening.
    # In practice the agent's Stop fire is many seconds after this
    # call so the order doesn't matter for correctness, but the
    # narrative order matches the agent's observed sequence.
    spawn_daemon_if_needed()
    seed_ok = seed_inbox(coord_id, fleet_home=home)
    if not seed_ok:
        # Don't write the marker — we want to retry the inbox seed on
        # the next tick. Daemon was already attempted; pgrep guards
        # the re-launch.
        return False
    if not _write_marker(marker):
        return False
    return True


def spawn_daemon_if_needed() -> bool:
    """Spawn `claude remote-control` in the background if not already
    running. Idempotent across the whole machine: pgrep guards re-launch.

    Returns True if the spawn shell command ran without raising
    (regardless of whether pgrep matched — the shell handles both
    branches). Returns False if subprocess.run itself failed (no shell,
    no claude binary, etc.). Logs to /tmp/claude-rc-coord.log.

    Mirrors the byte-shape of internal/handoff.FirstAction's bash block
    (with the suffix swapped: `fleet-coord` prefix, coord log path) so
    the daemon process surface stays consistent.
    """
    # Same form as handoff.FIRST_ACTION's bash block in
    # skills/fleet-guard/handoff.py. nohup + & + disowned subshell
    # detaches from the coord tick's process group; the daemon survives
    # tick exit. pgrep -f matches the full command line.
    cmd = (
        'pgrep -f "claude remote-control" >/dev/null 2>&1 || '
        'nohup claude remote-control '
        '--remote-control-session-name-prefix "fleet-coord" '
        f'> {_DAEMON_LOG} 2>&1 &'
    )
    try:
        # start_new_session detaches the spawned shell from our process
        # group; subprocess.Popen with a string + shell=True is the
        # canonical way to fork-and-forget a daemon. We don't wait —
        # the `&` inside the shell already backgrounds the claude
        # process, but DEVNULL-ing stdout/stderr keeps any pgrep noise
        # from leaking onto the coord's stdout (which the skill emits
        # JSON on).
        subprocess.Popen(
            ["bash", "-c", cmd],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        return True
    except FileNotFoundError:
        # bash missing on the host — extremely unlikely, but fail-soft.
        # Operator will see no remote-control attachment; the inbox
        # seed still goes out.
        print(
            "coord remote-control: bash not found; daemon not spawned",
            file=sys.stderr,
        )
        return False
    except Exception as exc:
        print(
            f"coord remote-control: daemon spawn failed: {exc}",
            file=sys.stderr,
        )
        return False


def seed_inbox(coord_id: str, *, fleet_home: Path | None = None) -> bool:
    """Write the one-shot `/remote-control` instruction to
    ~/.fleet/inbox/<coord_id>.md.

    Atomic via tmp + rename + fsync — same pattern as
    dispatch.write_worker_inbox + fleet-guard's _atomic_write_bytes.
    Returns True on success, False on any I/O failure.

    fleet-guard's _on_stop reads this file on the next Stop fire,
    delivers it as `[OPERATOR] <body>`, archives it. After that the
    file is consumed; the marker file gates re-seeding so the message
    stays one-shot per coord.

    Skip-if-exists posture: if the operator already queued a message at
    the same path via `fleet message <coord_id>`, do NOT clobber it.
    Return False so the caller withholds the marker and the next tick
    retries (by which time fleet-guard's Stop hook will have delivered
    + archived the operator's message and the path is free). This is
    rare — the operator would have to message the coord between
    dispatch and the very first tick — but the cost of clobbering
    operator context is much higher than the cost of one extra tick of
    /remote-control delay.
    """
    home = _resolve_home(fleet_home)
    inbox_dir = home / "inbox"
    target = inbox_dir / f"{coord_id}.md"
    try:
        inbox_dir.mkdir(parents=True, exist_ok=True)
    except OSError as exc:
        print(
            f"coord remote-control: inbox mkdir failed: {exc}",
            file=sys.stderr,
        )
        return False
    if target.exists():
        # Operator (or fleet-guard's prior delivery + a re-seed race we
        # haven't gated) already wrote to this inbox. Don't clobber.
        # Return False so bootstrap_remote_control withholds the marker
        # and we retry next tick — by then the operator's message has
        # been delivered + archived and the path is free for our seed.
        return False
    body = _INBOX_BODY
    if not body.endswith("\n"):
        body = body + "\n"
    tmp_path: str | None = None
    try:
        fd, tmp = tempfile.mkstemp(
            prefix=f".{coord_id}.rc-bootstrap.",
            suffix=".tmp",
            dir=str(inbox_dir),
        )
        tmp_path = tmp
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.write(body)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_path, target)
        return True
    except OSError as exc:
        print(
            f"coord remote-control: inbox seed failed: {exc}",
            file=sys.stderr,
        )
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except OSError:
                pass
        return False


# ---------- internals ----------


def _resolve_home(fleet_home: Path | None) -> Path:
    if fleet_home is not None:
        return fleet_home
    env = os.environ.get("FLEET_HOME")
    if env:
        return Path(env)
    return Path(os.path.expanduser("~/.fleet"))


def _marker_path(home: Path, project: str, coord_id: str) -> Path:
    """Per-coord, per-project marker file. Hidden (`.` prefix) so it
    doesn't show up in `ls`. Suffix carries coord_id so two consecutive
    coordinator IDs (handoff replacement) each get their own bootstrap
    pass — handoff path will overwrite, but if a coord crashes before
    handoff and a fresh dispatch lands, the new coord_id misses the
    old marker and re-bootstraps correctly."""
    return home / "projects" / project / f".remote-control-bootstrap-{coord_id}"


def _write_marker(marker: Path) -> bool:
    """Atomic create of the per-coord marker. Returns True on success.

    A pre-existing marker is treated as a no-op success (someone else
    raced us; their bootstrap is the one of record). Any other I/O
    failure returns False so the next tick retries.
    """
    try:
        marker.parent.mkdir(parents=True, exist_ok=True)
        # tmp + rename for atomic create. exclusive open would leak
        # file descriptors on Windows-style filesystems; rename is the
        # cross-platform idiom matching the rest of the skill.
        tmp_path: str | None = None
        try:
            fd, tmp = tempfile.mkstemp(
                prefix=f".{marker.name}.",
                suffix=".tmp",
                dir=str(marker.parent),
            )
            tmp_path = tmp
            with os.fdopen(fd, "w", encoding="utf-8") as fh:
                fh.write("")
                fh.flush()
                os.fsync(fh.fileno())
            os.replace(tmp_path, marker)
            return True
        except OSError as exc:
            print(
                f"coord remote-control: marker write failed: {exc}",
                file=sys.stderr,
            )
            if tmp_path:
                try:
                    os.unlink(tmp_path)
                except OSError:
                    pass
            return False
    except OSError as exc:
        print(
            f"coord remote-control: marker mkdir failed: {exc}",
            file=sys.stderr,
        )
        return False


# Re-export for tests / discoverability.
__all__ = [
    "bootstrap_remote_control",
    "spawn_daemon_if_needed",
    "seed_inbox",
]
