"""Reader/drainer half of the watcher->coord message channel (Slice 1/4).

The Go writer (internal/watchchan) appends one file per EVENT message under
~/.fleet/projects/<project>/watch-msgs/ and overwrites one file per WORKER
heartbeat (hb-<worker>.json). This module is the coord-side reader: it lists
pending EVENTS in time order, reads/refuses-newer a message, and provides
mark_done (the apply-before-delete commit point). Heartbeats are read
separately and are NEVER drained or deleted as events.

Mirrors the internal/queue list+delete idiom and pr_watch.py's
refuse-newer-schema discipline. No coordinator.lock is taken here — this is
pre-serialization plumbing.

    EVENTS    <unix_nanos>-<watcher_id>-<seq>.json   list_pending / mark_done
    HEARTBEAT hb-<worker_id>.json                    read_heartbeat (latest)

Drain contract (apply-before-delete):

    for path in list_pending(project_dir):
        msg = read_message(path)     # refuses a newer schema v
        apply(msg)                   # <-- COMMIT POINT is AFTER this
        mark_done(path)              # a crash before here replays the msg

The channel does NOT dedup; replay-idempotency is the caller's job
(bookkeeping flips are idempotent; dispatch suppression uses pr-watch keys).
"""
from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Optional

# Wire version shared with internal/watchchan.SchemaVersion. We refuse a
# message whose `v` is newer than this rather than mis-parse it.
SCHEMA_VERSION = 1

DIR_NAME = "watch-msgs"
HEARTBEAT_PREFIX = "hb-"


class SchemaTooNewError(Exception):
    """A message declares a schema `v` newer than this reader understands."""


def watch_msgs_dir(project_dir) -> Path:
    """Return ~/.fleet/projects/<project>/watch-msgs/ for a project dir."""
    return Path(project_dir) / DIR_NAME


def _is_event_file(name: str) -> bool:
    """True iff name is a drainable EVENT message — a .json file that is
    neither a heartbeat (hb-*) nor a WriteAtomic .tmp.<pid> sidecar."""
    if name.startswith(HEARTBEAT_PREFIX):
        return False
    # WriteAtomic sidecars are "<name>.tmp.<pid>" — they do not end in .json.
    return name.endswith(".json")


def list_pending(project_dir) -> list[Path]:
    """Return absolute paths of pending EVENT messages, sorted by filename
    (== time order, since the filename is <unix_nanos>-...). Excludes hb-*
    heartbeats AND .tmp sidecars; a half-written file is never returned. A
    missing directory yields an empty list (not an error)."""
    d = watch_msgs_dir(project_dir)
    try:
        names = os.listdir(d)
    except FileNotFoundError:
        return []
    events = [n for n in names if _is_event_file(n)]
    events.sort()
    return [d / n for n in events]


def read_message(path) -> dict:
    """Parse one message file. Refuses a `v` newer than SCHEMA_VERSION
    (SchemaTooNewError) rather than mis-parsing — mirrors
    queue.ReadSpawnFresh / pr_watch SCHEMA_VERSION discipline."""
    with open(path, "r", encoding="utf-8") as fh:
        msg = json.load(fh)
    if not isinstance(msg, dict):
        raise ValueError(f"watchchan: {path} is not a JSON object")
    v = msg.get("v")
    if isinstance(v, int) and v > SCHEMA_VERSION:
        raise SchemaTooNewError(
            f"watchchan: {path} schema v={v} newer than supported {SCHEMA_VERSION} (upgrade fleet)"
        )
    return msg


def _fsync_dir(directory) -> None:
    """Best-effort fsync of a directory so a just-completed unlink is durable
    across a crash. On POSIX an unlink is NOT crash-durable until its parent
    directory is fsynced; without this a "drained" event file can reappear
    after a reboot and be processed twice. Mirrors the Go writer's fsyncDir.
    Errors are swallowed: the unlink already took observable effect, and some
    platforms/filesystems refuse a directory fsync."""
    try:
        fd = os.open(str(directory), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)


def mark_done(path) -> None:
    """Delete a drained EVENT file — the apply-before-delete COMMIT POINT.
    The caller applies the mutation first, THEN calls mark_done; a crash in
    between replays the message on recovery. Missing file is a no-op
    (idempotent).

    The unlink is made durable (parent-dir fsync) so this is a REAL commit
    point: without it, applying a worker_delta/pr_event, calling mark_done,
    then crashing could resurrect the file on reboot and double-apply the
    action. A missing file skips the fsync (nothing was committed here)."""
    path = Path(path)
    try:
        os.remove(path)
    except FileNotFoundError:
        return
    _fsync_dir(path.parent)


# --- heartbeats: latest-state, read separately, never drained as events ---


def heartbeat_path(project_dir, worker_id: str) -> Path:
    """Return the hb-<worker>.json path for a worker."""
    return watch_msgs_dir(project_dir) / f"{HEARTBEAT_PREFIX}{worker_id}.json"


def read_heartbeat(project_dir, worker_id: str) -> Optional[dict]:
    """Return the latest heartbeat dict for a worker, or None if none/
    unreadable. Heartbeats are latest-state files — read, never mark_done.

    Refuses a `v` newer than SCHEMA_VERSION (SchemaTooNewError), same as
    read_message: in a mixed-version rollout an older coord must surface the
    upgrade requirement, NOT silently consume an incompatible heartbeat and
    skew the liveness/stuck-worker decisions built on it. Raising (rather than
    returning None) is deliberate — a None would read as 'worker dead' and
    could trip false stuck-worker recovery."""
    p = heartbeat_path(project_dir, worker_id)
    try:
        with open(p, "r", encoding="utf-8") as fh:
            hb = json.load(fh)
    except (FileNotFoundError, json.JSONDecodeError):
        return None
    if isinstance(hb, dict):
        v = hb.get("v")
        if isinstance(v, int) and v > SCHEMA_VERSION:
            raise SchemaTooNewError(
                f"watchchan: {p} heartbeat schema v={v} newer than supported "
                f"{SCHEMA_VERSION} (upgrade fleet)"
            )
    return hb


def list_heartbeats(project_dir) -> list[Path]:
    """Return absolute paths of every hb-<worker>.json heartbeat file."""
    d = watch_msgs_dir(project_dir)
    try:
        names = os.listdir(d)
    except FileNotFoundError:
        return []
    return [
        d / n
        for n in names
        if n.startswith(HEARTBEAT_PREFIX) and n.endswith(".json")
    ]
