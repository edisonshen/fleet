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
     background, idempotent at the **fleet-coord-prefix** level via
     `pgrep -f`. This is *intentionally* narrower than the handoff
     daemon's pgrep guard. See below for the bug history.
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
I/O failure logs to stderr AND to BOOTSTRAP_LOG and proceeds. The
bootstrap NEVER blocks the coord tick — same discipline as fleet-guard.

ASCII diagram of the fresh-coord flow:

    coord agent boots → first loop.tick() → bootstrap_remote_control(p, id)
                                              │
                                              ▼
                          marker exists? ── yes ─► return STATUS_SKIPPED_MARKER
                                              │ no
                                              ▼
                       spawn_daemon_if_needed()
                          (pgrep narrowly guards re-launch of
                           the fleet-coord-prefix daemon ONLY)
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
                          injects [OPERATOR] INFO: claude remote-control ...
                                              │
                                              ▼
                       agent runs /remote-control slash command
                          → session attaches to daemon


Status enum (returned by bootstrap_remote_control). Stable strings —
operators / log scrapers may grep for these:

  - STATUS_OK                 daemon spawn fired, inbox seeded, marker
                              written. Quiet success; no log line.
  - STATUS_SKIPPED_MARKER     marker already present; this is the
                              steady-state after the first successful
                              bootstrap. Quiet; no log line.
  - STATUS_SKIPPED_NO_ARGS    empty project / coord_id (caller's signal
                              that we're not under fleet supervision
                              yet, e.g. tests with FLEET_AGENT_ID
                              unset). Quiet; no log line.
  - STATUS_SKIPPED_INVALID    project or coord_id failed the canonical
                              shape check (defense in depth). Logged
                              to stderr AND BOOTSTRAP_LOG.
  - STATUS_SKIPPED_INBOX_BUSY operator queued an inbox message via
                              `fleet message <coord_id>` between
                              dispatch and this tick. Not a failure;
                              fleet-guard's Stop hook archives the
                              message on next fire and the path frees
                              up for our seed. Quiet; self-healing.
  - STATUS_FAILED_SEED        seed_inbox returned False from a real
                              I/O failure (mkdir/mkstemp/disk full).
                              Marker NOT written; next tick retries.
                              Logged.
  - STATUS_FAILED_MARKER      seed succeeded but marker write failed.
                              Next tick retries (seed is idempotent).
                              Logged.

The previous return type was bool ("True iff this call did the
bootstrap"). The new enum form preserves the truth-table:
  old True  → STATUS_OK
  old False → any of the SKIPPED_*/FAILED_* values.

PRIOR BUG (this module's regression pin, fixed alongside the dispatch
flag injection in fix/remote-control-coord-injection): the previous
pgrep pattern was the broad `claude remote-control` match, which ALSO
matched a `fleet-handoff`-prefixed daemon (started by
internal/handoff.FirstAction's bash bootstrap). When the operator had
a handoff daemon running, the coord skill would silently skip its own
daemon spawn — so mobile claude.ai pairing for fleet-coord-* sessions
never worked. The new pattern anchors on `^claude` AND the
`--remote-control-session-name-prefix fleet-coord` literal so
fleet-handoff daemons no longer mask the coord launch. This pattern is
intentionally kept symmetric with cmd/fleet/dispatch.go's
remoteControlDaemonRunning regex to ease audits.
"""
from __future__ import annotations

import datetime as _dt
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path


# Path-to-log for the background daemon. Mirrors handoff.FirstAction's
# /tmp/claude-rc-handoff.log naming so log inspection by the operator
# stays predictable; fresh-coord vs handoff is encoded in the suffix.
_DAEMON_LOG = "/tmp/claude-rc-coord.log"

# Bootstrap status log — failures + non-default skips append a single
# JSON-ish line here. Quiet success is silent. Operator-facing breadcrumb:
# when mobile pairing breaks, `tail /tmp/fleet-bootstrap.log` shows
# whether bootstrap_remote_control ever ran for the missing coord and
# where it failed. Override via FLEET_BOOTSTRAP_LOG for tests / non-
# default homes (the env-var read happens at import time, but tests
# manipulate it via monkeypatch on the module attribute below).
BOOTSTRAP_LOG = os.environ.get(
    "FLEET_BOOTSTRAP_LOG",
    "/tmp/fleet-bootstrap.log",
)

# Status enum constants. Return values from bootstrap_remote_control.
# Stable strings — callers may grep logs.
STATUS_OK = "ok"
STATUS_SKIPPED_MARKER = "skipped_marker"
STATUS_SKIPPED_INVALID = "skipped_invalid"
STATUS_SKIPPED_NO_ARGS = "skipped_no_args"
# Inbox path was occupied (operator pre-queued a message via
# `fleet message <coord_id>` between dispatch and first tick). Don't
# clobber — retry next tick after fleet-guard's Stop hook delivers +
# archives the operator's message. NOT a failure: the bootstrap is
# self-healing across ticks; surface as skipped, not failed.
STATUS_SKIPPED_INBOX_BUSY = "skipped_inbox_busy"
STATUS_FAILED_SEED = "failed_seed"
STATUS_FAILED_MARKER = "failed_marker"

# Strict 8-lowercase-hex shape for coord_id, matching the format
# emitted by skills/fleet-guard/ids.py:new_id (`secrets.token_hex(4)`)
# and the validator in skills/coordinator/dispatch.py:_AGENT_ID_FULL_RE.
# Any caller passing a coord_id outside this shape is a bug; we refuse
# to use it as a path component (defense in depth: a coord_id of
# "../foo" would otherwise traverse out of ~/.fleet/inbox/).
_COORD_ID_RE = re.compile(r"^[0-9a-f]{8}$")

# Inbox body delivered to the coord agent on the next Stop hook. The
# fleet-guard relay wraps this with `[OPERATOR] ` prefix automatically
# (see skills/fleet-guard/inbox.py:deliver).
#
# IMPORTANT: this body must NOT be phrased as an imperative directed at
# the agent. Earlier wording ("Run the slash command `/remote-control`
# ...") caused Claude to interpret the message as a Skill invocation
# and error out with "remote-control is a UI command, not a skill"
# (issue #69). The fix: frame the message as a status notification
# describing what the daemon is doing, with the slash-command phrasing
# anchored to the operator. Constraints:
#   1. NO leading imperative ("Run", "Execute", "Invoke", "Please run").
#   2. Must contain the literal `/remote-control` so operators searching
#      the inbox can find it.
#   3. Must mention "daemon" so the agent gets real situational context.
#   4. Must frame the slash command as something the OPERATOR types,
#      not the agent.
_INBOX_BODY = (
    "INFO: claude remote-control daemon started for this coordinator "
    "session. To attach via mobile or claude.ai, the operator can type "
    "/remote-control in this chat."
)


def bootstrap_remote_control(
    project: str,
    coord_id: str,
    *,
    fleet_home: Path | None = None,
) -> str:
    """One-shot bootstrap for fresh coordinator agents.

    Returns a status string (see module docstring). Quiet success is
    STATUS_OK; the SKIPPED_*/FAILED_* paths each write a structured
    line to BOOTSTRAP_LOG so an operator post-mortem can grep for
    "why didn't bootstrap fire for coord <id>?".

    Failures in spawn_daemon_if_needed / seed_inbox do NOT abort the
    bootstrap: each is independently fail-soft. The marker is written
    iff BOTH the inbox seed succeeded AND we attempted the daemon spawn
    — we don't want to mark "done" if the agent never got the inbox
    message; conversely, a failed pgrep / spawn is acceptable (operator
    may have started the daemon manually, or it's not on PATH yet).
    """
    if not project or not coord_id:
        # Caller's signal that we're not under fleet supervision yet
        # (e.g. tests with FLEET_AGENT_ID unset). Don't log: the
        # no-args path fires every tick under those conditions and
        # would spam the log file.
        return STATUS_SKIPPED_NO_ARGS
    # Defense in depth: refuse to use a coord_id that doesn't match the
    # canonical 8-hex shape. In production the value comes from
    # FLEET_AGENT_ID (always 8-hex via secrets.token_hex(4)), so this
    # never trips; a future caller passing user-controlled input would
    # otherwise land path traversal via the marker / inbox filenames.
    if not _COORD_ID_RE.match(coord_id):
        print(
            f"coord remote-control: refusing invalid coord_id {coord_id!r}",
            file=sys.stderr,
        )
        _bootstrap_log(
            STATUS_SKIPPED_INVALID,
            project=project,
            coord_id=coord_id,
            detail="invalid coord_id (not 8-lowercase-hex)",
        )
        return STATUS_SKIPPED_INVALID
    # Project also gets a strict guard: it's used as a filesystem path
    # component, and a project of "../" would put the marker outside
    # ~/.fleet/projects/. The Go side already validates project names
    # at dispatch time, so a non-conforming value here is a bug — log
    # and refuse rather than silently traverse.
    if "/" in project or "\\" in project or project in (".", ".."):
        print(
            f"coord remote-control: refusing invalid project {project!r}",
            file=sys.stderr,
        )
        _bootstrap_log(
            STATUS_SKIPPED_INVALID,
            project=project,
            coord_id=coord_id,
            detail="invalid project (path traversal / separator)",
        )
        return STATUS_SKIPPED_INVALID
    home = _resolve_home(fleet_home)
    marker = _marker_path(home, project, coord_id)
    if marker.exists():
        return STATUS_SKIPPED_MARKER
    # Pre-check the inbox path: if an operator queued a message via
    # `fleet message <coord_id>` between dispatch and this tick, the
    # path is occupied. seed_inbox handles this internally by
    # returning False (don't clobber operator content), but bootstrap
    # needs to distinguish "operator queued a message" (transient,
    # self-healing — fleet-guard archives the file on next Stop hook)
    # from "real I/O failure" (persistent — needs operator attention).
    # Without this pre-check, every operator-queued-message lands as
    # STATUS_FAILED_SEED + a noisy tick error, which the operator
    # would see as a regression even though it's normal operation.
    inbox_target = home / "inbox" / f"{coord_id}.md"
    if inbox_target.exists():
        return STATUS_SKIPPED_INBOX_BUSY
    # Order: daemon first, inbox second. The inbox message tells the
    # agent to run /remote-control which connects to a daemon — if we
    # seed the inbox before launching the daemon, there's a small
    # window where /remote-control fires with no daemon listening.
    # In practice the agent's Stop fire is many seconds after this
    # call so the order doesn't matter for correctness, but the
    # narrative order matches the agent's observed sequence.
    spawn_daemon_if_needed(project)
    seed_ok = seed_inbox(coord_id, fleet_home=home)
    if not seed_ok:
        # Pre-check above already eliminated the "inbox path exists"
        # case, so reaching here means a real I/O failure
        # (mkdir/mkstemp/disk-full/permissions). Don't write the
        # marker — we want to retry the inbox seed on the next tick.
        # Daemon was already attempted; pgrep guards the re-launch.
        _bootstrap_log(
            STATUS_FAILED_SEED,
            project=project,
            coord_id=coord_id,
            detail="seed_inbox returned False (see stderr for cause)",
        )
        return STATUS_FAILED_SEED
    if not _write_marker(marker):
        _bootstrap_log(
            STATUS_FAILED_MARKER,
            project=project,
            coord_id=coord_id,
            detail="marker write failed (see stderr for cause)",
        )
        return STATUS_FAILED_MARKER
    return STATUS_OK


def spawn_daemon_if_needed(project: str = "") -> bool:
    """v0.12 (DESIGN-rc-listener-lifecycle.md §"Attach-surface gates"
    S1): shell out to `fleet rc up <project> --idempotent` so the Go
    controller (internal/rc) is the SINGLE owner of spawn / marker /
    state. The parallel pgrep+nohup path is gone — keeping it would
    re-introduce the "two writers, two opinions" hazard the v0.12
    architecture closes.

    Behaviour:
      - Marker absent for project → Go `fleet rc up` no-ops on the
        marker (rc.Enabled returns false; fleet rc up creates the
        marker AND spawns the listener). Returns True.
      - Marker present, listener alive → Go returns
        outcome=already_acquired exit 0. Returns True.
      - Operator hasn't opted in (no marker, no project arg) →
        skip entirely; returns True (no spawn, no-op).

    PR #157 env-gate (`FLEET_RC_BOOTSTRAP_DISABLED`) is kept as
    defense-in-depth through v0.12; it short-circuits BEFORE the
    fleet-rc-up shell-out. The CI invariant test
    (rc_invariant_test.go) proves the marker-gate is sufficient on
    its own; v0.13 retires the env-gate after field-proof.

    project is the per-project name (matches Go-side
    state.ValidateProjectName). Empty project means "we're not in a
    project-supervised coord" (legacy / unsupervised lineage) — the
    function returns True (no-op) without invoking fleet.
    """
    # Primary gate: marker absence. Without an explicit project the
    # function cannot make a per-project decision, so we no-op.
    if not project:
        return True
    # FLEET_RC_BOOTSTRAP_DISABLED env-gate (defense-in-depth from PR
    # #157, retired in v0.13). Test runs set this so pytest never
    # forks a real `claude remote-control` listener.
    if os.environ.get("FLEET_RC_BOOTSTRAP_DISABLED", ""):
        return True
    # Shell out to the Go controller. --idempotent maps to Up's
    # already_acquired semantics: exit 0 when the listener is already
    # alive AND the marker is present. The Go side handles
    # working_dir resolution (meta.json + live coord fallback) so the
    # Python side doesn't need to thread it through.
    try:
        result = subprocess.run(
            ["fleet", "rc", "up", project, "--idempotent"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=10,
            check=False,
        )
        # Exit 0 = acquired or already_acquired (both success). Any
        # other exit (10/11/12/1) is logged but not raised — the
        # caller treats this as fail-soft per S1 contract.
        if result.returncode != 0:
            print(
                f"coord remote-control: fleet rc up {project!r} exit "
                f"{result.returncode}; listener may not be alive",
                file=sys.stderr,
            )
            return False
        return True
    except FileNotFoundError:
        # `fleet` binary not on PATH — extremely unlikely (the coord
        # skill is invoked from a fleet-managed shell), but fail-soft.
        print(
            "coord remote-control: fleet binary not found; listener not spawned",
            file=sys.stderr,
        )
        return False
    except subprocess.TimeoutExpired:
        print(
            f"coord remote-control: fleet rc up {project!r} timed out (>10s)",
            file=sys.stderr,
        )
        return False
    except Exception as exc:
        print(
            f"coord remote-control: fleet rc up failed: {exc}",
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
    # Same coord_id guard as bootstrap_remote_control: this function is
    # also called directly from tests, and a non-conforming coord_id
    # would land path traversal via f"{coord_id}.md".
    if not coord_id or not _COORD_ID_RE.match(coord_id):
        print(
            f"coord remote-control: refusing invalid coord_id {coord_id!r}",
            file=sys.stderr,
        )
        return False
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


def _bootstrap_log(
    status: str,
    *,
    project: str,
    coord_id: str,
    detail: str = "",
) -> None:
    """Append one structured line to BOOTSTRAP_LOG. Best-effort: any
    log-write failure is itself swallowed — we don't want logging to
    crash the tick (that's the whole point of the fail-soft posture).

    Format (one line):
      <ISO-8601-UTC> [fleet-bootstrap] status=<status> project=<p> coord=<id> detail="<msg>"

    Stable enough to grep, free-form enough that future fields slot in
    without breaking parsers. The detail string is sanitized so a stray
    newline doesn't break tail-based log readers.
    """
    try:
        ts = _dt.datetime.now(_dt.timezone.utc).isoformat(timespec="seconds")
        safe_detail = detail.replace("\n", " ").replace("\r", " ")
        line = (
            f"{ts} [fleet-bootstrap] status={status} "
            f"project={project} coord={coord_id} "
            f'detail="{safe_detail}"\n'
        )
        # Append-only. mode "a" creates the file on first write; we
        # don't fsync — operator-visible diagnostics, not durable state.
        with open(BOOTSTRAP_LOG, "a", encoding="utf-8") as fh:
            fh.write(line)
    except OSError:
        # Log path unwritable (e.g., /tmp full, permissions); fall back
        # to stderr so the operator at least sees the status when they
        # tail the coord's tmux pane.
        try:
            print(
                f"[fleet-bootstrap] status={status} project={project} "
                f"coord={coord_id} detail={detail!r}",
                file=sys.stderr,
            )
        except Exception:
            # Truly nothing we can do.
            pass


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
    "BOOTSTRAP_LOG",
    "STATUS_OK",
    "STATUS_SKIPPED_MARKER",
    "STATUS_SKIPPED_INVALID",
    "STATUS_SKIPPED_NO_ARGS",
    "STATUS_SKIPPED_INBOX_BUSY",
    "STATUS_FAILED_SEED",
    "STATUS_FAILED_MARKER",
]
