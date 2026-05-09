"""Register a Claude Agent-tool subagent_id for a worker slug (issue #94).

Phase A (#84) made workers Agent-tool subagents of the coord agent.
Phase B (#93) plumbed the Fleet 8-hex agent_id through coord-state.json
and the handoff doc. Phase C (this module) captures the orthogonal
**Claude Agent-tool subagent_id** — the opaque token the host Claude
session generates per Agent call — so the fleet TUI can cross-reference
its worker rows with Claude's chat-side "N local agents" indicator.

The Python skill cannot read the Agent tool's return value (it's a
Claude tool, not a Python API). The coord agent (Claude session) sees
the subagent_id in the Agent call's response, then runs:

    python3 -m coordinator.register_subagent \\
        --project <p> <slug> <subagent_id>

This script does ONE thing: take coordinator.lock, RMW
~/.fleet/projects/<p>/coord-state.json, persist the slug → subagent_id
mapping under `worker_subagent_ids`, release. The TUI dashboard reads
that field on every render tick (mtime-driven fsnotify) and surfaces it
as a `· <8-char>` chip on the worker row.

Locking story:
- coordinator.lock is the one cross-process lock for coord-state.json
  mutations. The coord skill takes it LOCK_EX | LOCK_NB inside `tick()`
  and holds it for the full tick. The supervisor's stuck-check writes
  coord-state.json under that same lock.
- register_subagent runs OUTSIDE tick (between Stop hooks, during the
  coord agent's assistant turn). To avoid clobbering a concurrent tick
  write, it takes coordinator.lock LOCK_EX | LOCK_NB with a brief retry
  loop. On persistent contention (rare — tick is short for empty work),
  the script exits non-zero; the coord agent's protocol logs the failure
  and continues (the chip just stays empty until the next dispatch).
- A failure to register is non-fatal. The worker still spawns; only the
  TUI cross-reference chip is missing.

CLI usage::

    python3 -m coordinator.register_subagent <slug> <subagent_id>
    python3 -m coordinator.register_subagent --project <p> <slug> <id>
    python3 -m coordinator.register_subagent --clear <slug>

Project defaults to `$FLEET_PROJECT` env var (set by `fleet dispatch`
in the coord's tmux session) if --project is not given.
"""
from __future__ import annotations

import argparse
import errno
import fcntl
import json
import os
import sys
import tempfile
import time
from pathlib import Path

# Local sibling import (same pattern as handoff_resume.py).
import supervisor as supervisor_mod


# Lock retry budget. Tick latency is ~ms when no work is queued; the
# supervisor stuck-check holds coord-state.json briefly inside its
# pass. 10 attempts at 100ms = 1s total — enough to cover any normal
# tick, fail-fast on a stuck coord.
_LOCK_RETRY_ATTEMPTS = 10
_LOCK_RETRY_INTERVAL_S = 0.1


class RegisterError(Exception):
    """Public failure mode. Raised on lock contention, missing project
    dir, or invalid input. CLI converts to exit-1 + stderr message."""


def main(argv: list[str] | None = None) -> int:
    """Script entry. Returns the exit code."""
    args = _parse_args(argv)
    project = args.project or os.environ.get("FLEET_PROJECT", "")
    if not project:
        print(
            "register_subagent: --project required "
            "(or set FLEET_PROJECT env var)",
            file=sys.stderr,
        )
        return 2
    home = Path(os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet"))
    try:
        if args.clear:
            cleared = clear(project, args.slug, home=home)
            if cleared:
                print(f"register_subagent: cleared {project}:{args.slug}")
            else:
                print(
                    f"register_subagent: {project}:{args.slug} not in map "
                    "(no-op)",
                )
            return 0
        register(
            project, args.slug, args.subagent_id, home=home,
        )
    except RegisterError as exc:
        print(f"register_subagent: {exc}", file=sys.stderr)
        return 1
    print(
        f"register_subagent: {project}:{args.slug} -> {args.subagent_id}",
    )
    return 0


def _parse_args(argv: list[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        prog="register_subagent",
        description=(
            "Persist a Claude Agent-tool subagent_id for a worker slug "
            "into coord-state.json:worker_subagent_ids."
        ),
    )
    parser.add_argument("--project", default="", help="project name (default: $FLEET_PROJECT)")
    parser.add_argument(
        "--clear",
        action="store_true",
        help="remove the slug's subagent_id mapping (no <subagent_id> required)",
    )
    parser.add_argument("slug", help="worker slug (matches tasks.md)")
    parser.add_argument(
        "subagent_id",
        nargs="?",
        default="",
        help="Claude Agent-tool subagent_id; required unless --clear",
    )
    args = parser.parse_args(argv)
    if not args.clear and not args.subagent_id:
        parser.error("subagent_id required unless --clear")
    return args


def register(
    project: str,
    slug: str,
    subagent_id: str,
    *,
    home: Path | None = None,
) -> None:
    """RMW coord-state.json: persist `worker_subagent_ids[slug] = id`.

    Raises RegisterError on:
      - empty slug or subagent_id
      - subagent_id failing supervisor._is_subagent_id (whitespace,
        non-printable, or length > 128)
      - lock contention exceeding the retry budget
      - project dir missing (no coord ever ran here)
    """
    if not slug or not slug.strip():
        raise RegisterError("slug must be non-empty")
    if not supervisor_mod._is_subagent_id(subagent_id):
        raise RegisterError(
            "subagent_id rejected (empty / whitespace / non-printable / >128 chars)",
        )
    home = home or Path(os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet"))
    project_dir = home / "projects" / project
    if not project_dir.is_dir():
        raise RegisterError(
            f"project dir missing: {project_dir} "
            "(no coord has run for this project yet)",
        )

    state_path = project_dir / "coord-state.json"
    locks_dir = project_dir / ".locks"
    locks_dir.mkdir(parents=True, exist_ok=True)
    lock_path = locks_dir / "coordinator.lock"

    with _take_coord_lock(lock_path):
        cs = _read_coord_state(state_path)
        supervisor_mod.remember_subagent_id(cs, slug, subagent_id)
        _write_coord_state(state_path, cs)


def clear(
    project: str,
    slug: str,
    *,
    home: Path | None = None,
) -> bool:
    """RMW coord-state.json: drop `worker_subagent_ids[slug]`. Returns
    True when an entry was removed, False when slug wasn't in the map.

    Idempotent — calling clear on an unmapped slug is a no-op + False.
    """
    if not slug or not slug.strip():
        raise RegisterError("slug must be non-empty")
    home = home or Path(os.environ.get("FLEET_HOME") or os.path.expanduser("~/.fleet"))
    project_dir = home / "projects" / project
    if not project_dir.is_dir():
        # Idempotent — clearing in a missing project is a no-op.
        return False

    state_path = project_dir / "coord-state.json"
    locks_dir = project_dir / ".locks"
    locks_dir.mkdir(parents=True, exist_ok=True)
    lock_path = locks_dir / "coordinator.lock"

    with _take_coord_lock(lock_path):
        cs = _read_coord_state(state_path)
        before = supervisor_mod.load_subagent_id_map(cs)
        had = slug in before
        supervisor_mod.forget_subagent_id(cs, slug)
        if had:
            _write_coord_state(state_path, cs)
        return had


# ---------- locking + io helpers ----------


class _LockHandle:
    """Tiny RAII handle for the coord lock. Closing the fd releases the
    flock via the kernel; we explicitly LOCK_UN first so the release is
    visible to any subsequent strace-level audit. Errors during release
    are swallowed — the kernel will reclaim on close anyway."""

    def __init__(self, fd: int) -> None:
        self.fd = fd

    def __enter__(self) -> "_LockHandle":
        return self

    def __exit__(self, *_exc) -> None:
        try:
            fcntl.flock(self.fd, fcntl.LOCK_UN)
        except OSError:
            pass
        try:
            os.close(self.fd)
        except OSError:
            pass


def _take_coord_lock(path: Path) -> _LockHandle:
    """Acquire coordinator.lock LOCK_EX | LOCK_NB with retry. Raises
    RegisterError after _LOCK_RETRY_ATTEMPTS failures so the caller can
    surface the contention to the operator.

    Why LOCK_NB + retry instead of LOCK_EX blocking: a stuck supervisor
    pass could otherwise hold the lock for the full poll_interval (30s
    default). Failing fast keeps the coord agent's turn responsive; the
    next dispatch's register call has another chance.

    NB: this is the SAME lock the coord skill takes inside `tick()`. The
    coord process drops the lock between ticks (Stop-hook driven), so
    the contention window is bounded by tick latency.
    """
    last_err: OSError | None = None
    for attempt in range(_LOCK_RETRY_ATTEMPTS):
        try:
            fd = os.open(str(path), os.O_RDWR | os.O_CREAT, 0o644)
        except OSError as exc:
            raise RegisterError(f"open coord lock: {exc}") from exc
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return _LockHandle(fd)
        except OSError as exc:
            try:
                os.close(fd)
            except OSError:
                pass
            if exc.errno not in (errno.EAGAIN, errno.EWOULDBLOCK):
                raise RegisterError(f"flock coord lock: {exc}") from exc
            last_err = exc
            time.sleep(_LOCK_RETRY_INTERVAL_S)
    raise RegisterError(
        f"coord lock busy after {_LOCK_RETRY_ATTEMPTS} attempts "
        f"(last err: {last_err}); coord may be mid-tick — retry from coord agent",
    )


def _read_coord_state(path: Path) -> dict:
    """Load coord-state.json. Empty dict on first run / read error.

    Mirrors supervisor._read_coord_state — kept local so the helper
    doesn't depend on supervisor's private file I/O surface."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
        if isinstance(data, dict):
            return data
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        pass
    return {}


def _write_coord_state(path: Path, state: dict) -> None:
    """Atomic publish (tmp + rename + fsync). Mirrors loop._save_coord_state.
    Same atomic discipline as the supervisor's writer so a crash mid-
    register leaves the prior state intact, never a partial write."""
    parent = path.parent
    parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=path.name + ".tmp.", dir=str(parent))
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(state, fh, indent=2, sort_keys=True)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except Exception:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


if __name__ == "__main__":
    sys.exit(main())
