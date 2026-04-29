"""Handoff orchestration — the meat of fleet-guard.

State machine on every Stop hook fire (delegated to maybe_trigger):

      ┌──────────────┐
  ── new fire ──►│ read record  │── missing ─►  noop (not under fleet)
      └──────┬───────┘
             │
             ▼
      ┌──────────────┐
      │ mode plan/   │── yes ─► noop (operator's deliberate space)
      │ review/fix?  │
      └──────┬───────┘
             │ no
             ▼
      ┌──────────────┐
      │ context_pct  │
      └──────┬───────┘
       green │  yellow              red
        ▼    ▼                       ▼
       noop  ┌─── pending? ────┐    write doc + queue
             │ no             │ yes immediately (auto-red)
             ▼                ▼
        inject + mark    capture pane;
        handoff_type=    ^MILESTONE$ found?
        auto-yellow         │ no       │ yes
                            ▼          ▼
                          noop     write doc + queue
                                   (type from record)

PreCompact hook fires emergency_trigger directly — same write path as Red,
no threshold check.

Frontmatter byte-shape MUST match internal/handoff.Render exactly: 8 keys
in fixed order, all string values double-quoted with Go strconv.Quote-
compatible escapes. The chain reader and resume probe parse this with
line-prefix matching, so reordering or formatting drift breaks 4a's
reader silently.
"""
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import health
import ids

HANDOFF_REQUESTED = "HANDOFF REQUESTED"
MILESTONE = "MILESTONE"

# Type values mirroring internal/handoff/handoff.go:30-36. The skill never
# writes "manual" (that's Week 4a's operator path).
TYPE_AUTO_YELLOW = "auto-yellow"
TYPE_AUTO_RED = "auto-red"
TYPE_PRECOMPACT = "precompact"

# is_handoff_pending must distinguish "this agent was spawned from a handoff"
# (handoff_type stamped to "manual" by internal/spawn/spawn.go on every
# successor record) from "a handoff is pending right now". Without this
# distinction, every successor agent starts life with pending=True, which
# silently kills auto-handoff after the first chain step: Yellow skips the
# HANDOFF REQUESTED injection and jumps straight to MILESTONE grep, and Red
# short-circuits entirely because the pre-mark race-protection bails out.
# Only the three auto-* values mark "pending" — "manual" and anything else
# is a spawn-origin label, not a pending state.
_PENDING_TYPES = frozenset({TYPE_AUTO_YELLOW, TYPE_AUTO_RED, TYPE_PRECOMPACT})

# Canonical placeholder string for unfilled body sections. MUST match
# internal/handoff.Placeholder byte-for-byte — alternate sentinels would
# break 4a's chain reader. The em dash is U+2014.
PLACEHOLDER = "_(operator-triggered handoff — fill in before resuming)_"

# Modes where auto-handoff is disabled — see SKILL.md Handoff thresholds.
THINKING_MODES = frozenset({"plan", "review", "fix"})

# ANSI SGR escape codes that show up in tmux capture-pane output. Stripping
# these is the minimum useful polish; bracketed-paste sequences and cursor
# moves will land in iteration 1.
_ANSI_SGR_RE = re.compile(r"\x1b\[[0-9;]*[mK]")


def inject_handoff_requested() -> str:
    """The exact text injected into the agent's next turn at Yellow.

    The agent is expected to wrap up, summarize via MILESTONE on its own
    line, and stop. The skill picks up the MILESTONE on the following fire
    and writes the handoff doc.
    """
    return (
        f"{HANDOFF_REQUESTED}: context window is over 50%. "
        f"Wrap up the current sub-task at the next safe boundary, "
        f"then on its own line write a single token:\n\n"
        f"{MILESTONE}\n\n"
        f"This signals fleet-guard to write your handoff doc and spawn "
        f"a fresh agent that inherits the work. Do not write MILESTONE "
        f"mid-paragraph; it must be the only thing on its line."
    )


def is_handoff_pending(record: dict[str, Any]) -> bool:
    """A handoff is pending when the record's handoff_type is one of the
    auto-* values. The "manual" stamp written by internal/spawn/spawn.go
    on every successor record is NOT pending — it's a spawn-origin label
    that future skill fires must overwrite when an actual auto-handoff
    is requested."""
    return record.get("handoff_type") in _PENDING_TYPES


def find_milestone(session: str) -> bool:
    """Capture the agent's tmux pane and search for a MILESTONE line that
    appeared AFTER the most recent HANDOFF REQUESTED injection. Returns
    False on tmux failure — the skill is non-blocking, and a missed
    MILESTONE just means the next fire tries again.

    Bounded to "after HANDOFF REQUESTED" because the pane scrollback can
    contain prior MILESTONE lines (the agent's own narration of the
    handoff token, an earlier discussion that quoted the marker, even
    a previous Yellow cycle's wrap-up). Counting any historical
    MILESTONE would fire the handoff the moment Yellow first injects —
    cutting off active work mid-subtask.

    Match is exact (`^MILESTONE$` after stripping whitespace), so the
    word "MILESTONES" or "MILESTONE: foo" never falsely triggers.
    """
    out = _capture_pane(session)
    if not out:
        return False
    lines = out.splitlines()
    # Find the LAST occurrence of HANDOFF REQUESTED. Multiple matches are
    # plausible — the injection itself, the agent quoting it, etc. The
    # most recent one is what bounds the new MILESTONE search.
    last_request = -1
    for i, line in enumerate(lines):
        if HANDOFF_REQUESTED in line:
            last_request = i
    if last_request == -1:
        # No HANDOFF REQUESTED visible — either it scrolled off or
        # never fired. Conservative: don't trigger. The pending check
        # in maybe_trigger would have called this only when the record
        # already had handoff_type=auto-yellow set, so the injection
        # DID fire on a prior turn; if it scrolled off, the agent
        # eventually crosses 70% and emergency triggers via Red.
        return False
    for line in lines[last_request + 1:]:
        if line.strip() == MILESTONE:
            return True
    return False


def capture_recent(session: str, lines: int = 200) -> str:
    """Last N lines of the tmux pane with ANSI SGR codes stripped.
    Returns "" on tmux failure."""
    out = _capture_pane(session, lines=lines)
    if not out:
        return ""
    return _ANSI_SGR_RE.sub("", out)


def _capture_pane(session: str, lines: int | None = None) -> str:
    """Wrap `tmux capture-pane -t <session> -p`. With lines, request only
    the last N via `-S -<lines>`. Returns stdout on success, "" on any
    failure. The 5s timeout protects against a hung tmux server.
    """
    cmd = ["tmux", "capture-pane", "-t", session, "-p"]
    if lines is not None:
        cmd.extend(["-S", f"-{lines}"])
    try:
        result = subprocess.run(
            cmd, capture_output=True, text=True, timeout=5, check=False,
        )
    except Exception:
        return ""
    if result.returncode != 0:
        return ""
    return result.stdout or ""


def maybe_trigger(payload: dict[str, Any], *, agent_id: str,
                  session: str) -> str | None:
    """Run the Stop-hook handoff state machine for one fire.

    Returns the injection text to print to stdout (a HANDOFF REQUESTED
    nudge), or None for the silent-noop / threshold-not-crossed / wrote-
    doc-and-queue paths. Never raises — wraps every branch in try/except.
    """
    try:
        record = health.read_record(agent_id)
        if record is None:
            return None
        if record.get("mode") in THINKING_MODES:
            return None

        pct, _model = health.read_context_pct(payload)
        state = health.threshold(pct)
        pending = is_handoff_pending(record)

        if state == "red":
            # Pending check protects against re-fire racing the drain:
            # if the previous fire already wrote doc + queue, skip
            # entirely so we don't leave an orphan doc on disk (a
            # second render would have a different timestamp+rnd
            # filename, and the queue would be re-clobbered to point
            # at it — orphaning the first doc).
            if pending:
                return None
            # Pre-mark handoff_type so any re-fire that lands before
            # this _do_handoff completes sees pending=True and bails
            # at the check above.
            health.update_record(agent_id, handoff_type=TYPE_AUTO_RED)
            # Re-load so _do_handoff sees the freshly-set type (its
            # captured `record` dict is now stale on that field, but
            # _do_handoff doesn't use record.handoff_type — it takes
            # the type as an argument).
            _do_handoff(record, session, TYPE_AUTO_RED, pct)
            return None

        if state == "yellow":
            if pending:
                if find_milestone(session):
                    handoff_type = (record.get("handoff_type")
                                    or TYPE_AUTO_YELLOW)
                    _do_handoff(record, session, handoff_type, pct)
                return None
            health.update_record(agent_id, handoff_type=TYPE_AUTO_YELLOW)
            return inject_handoff_requested()

        return None
    except Exception as exc:
        print(f"fleet-guard handoff.maybe_trigger: {exc}", file=sys.stderr)
        return None


def emergency_trigger(payload: dict[str, Any], *, agent_id: str,
                      session: str) -> None:
    """PreCompact path: write doc + queue regardless of threshold.

    Best-effort and silent on failure. The compaction is already in
    motion; we save what we can. Same pre-mark-pending pattern as
    maybe_trigger's Red branch — protects against a hypothetical
    PreCompact re-fire (Claude Code shouldn't double-fire, but the
    skill's "never block" contract demands we don't trust upstream).
    """
    try:
        record = health.read_record(agent_id)
        if record is None:
            return
        if is_handoff_pending(record):
            return
        pct, _ = health.read_context_pct(payload)
        health.update_record(agent_id, handoff_type=TYPE_PRECOMPACT)
        _do_handoff(record, session, TYPE_PRECOMPACT, pct)
    except Exception as exc:
        print(f"fleet-guard handoff.emergency_trigger: {exc}",
              file=sys.stderr)


def _do_handoff(record: dict[str, Any], session: str,
                handoff_type: str, pct: float | None) -> None:
    """Capture the pane, render + write the doc, write the queue file,
    update the record's handoff_type. Pure best-effort: any I/O failure
    aborts the chain (we don't want a queue file pointing at a doc that
    failed to render)."""
    agent_id = record["id"]
    task_id = record.get("task_id", "")
    project = record.get("project", "")
    number = int(record.get("handoff_number", 1) or 1)
    prev_raw = record.get("last_handoff_path")
    prev_path: str | None = prev_raw if isinstance(prev_raw, str) and prev_raw else None

    recent = capture_recent(session)
    new_id = ids.new_id()
    ts = datetime.now(timezone.utc)

    doc_path = write_doc(
        agent_id=agent_id,
        task_id=task_id,
        project=project,
        handoff_type=handoff_type,
        number=number,
        prev_path=prev_path,
        context_pct=pct,
        ts=ts,
        recent_activity=recent,
    )
    if not doc_path:
        return
    if not write_queue(
        old_id=agent_id,
        new_id=new_id,
        doc_path=doc_path,
        project=project,
        task_id=task_id,
        ts=ts,
    ):
        return
    # handoff_type is set by maybe_trigger BEFORE this call (so a
    # re-fire racing _do_handoff sees pending=True and skips). The
    # post-write idempotent re-set used to live here; removing it
    # because the pre-mark is the load-bearing one and a redundant
    # write at the end widens the race window for nothing.


# -- doc rendering -----------------------------------------------------------

def write_doc(*, agent_id: str, task_id: str, project: str,
              handoff_type: str, number: int, prev_path: str | None,
              context_pct: float | None, ts: datetime,
              recent_activity: str) -> str:
    """Render and atomically write the handoff doc. Returns the absolute
    path on success, "" on failure. Body has the captured tmux pane in
    `## Completed` (per plan D3 / SKILL.md) and Placeholder in the other
    four sections — the operator or successor agent fills those before
    resuming."""
    path = _handoff_path(agent_id, ts)
    body = _render_doc(
        agent_id=agent_id,
        task_id=task_id,
        project=project,
        handoff_type=handoff_type,
        number=number,
        prev_path=prev_path,
        context_pct=context_pct,
        ts=ts,
        recent_activity=recent_activity,
    )
    if not _atomic_write_bytes(path, body):
        return ""
    return str(path)


def _handoff_path(agent_id: str, ts: datetime) -> Path:
    """Reproduce internal/state.HandoffPath: <id>-<UTC YYYYMMDD-HHMMSS>-<4hex>.md"""
    stamp = ts.astimezone(timezone.utc).strftime("%Y%m%d-%H%M%S")
    rnd = os.urandom(2).hex()
    return health.fleet_home() / "handoffs" / f"{agent_id}-{stamp}-{rnd}.md"


def _render_doc(*, agent_id: str, task_id: str, project: str,
                handoff_type: str, number: int, prev_path: str | None,
                context_pct: float | None, ts: datetime,
                recent_activity: str) -> bytes:
    """Render the handoff doc bytes. Frontmatter ordering and quoting must
    match internal/handoff.Render byte-for-byte."""
    out: list[str] = ["---\n"]
    out.append(f"agent_id: {_go_quote(agent_id)}\n")
    out.append(f"task_id: {_go_quote(task_id)}\n")
    out.append(f"project: {_go_quote(project)}\n")
    if context_pct is None:
        out.append("context_pct_at_handoff: null\n")
    else:
        out.append(
            f"context_pct_at_handoff: {_format_float_go(context_pct)}\n",
        )
    if prev_path is None:
        out.append("previous_handoff: null\n")
    else:
        out.append(f"previous_handoff: {_go_quote(prev_path)}\n")
    out.append(f"handoff_number: {number}\n")
    out.append(f"timestamp: {_go_quote(ts.astimezone(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'))}\n")
    out.append(f"handoff_type: {_go_quote(handoff_type)}\n")
    out.append("---\n\n")

    completed = recent_activity.strip() if recent_activity.strip() else PLACEHOLDER
    out.append(f"## Completed\n{completed}\n\n")
    out.append(f"## Key Decisions\n{PLACEHOLDER}\n\n")
    out.append(f"## Files Modified\n{PLACEHOLDER}\n\n")
    out.append(f"## Open Questions\n{PLACEHOLDER}\n\n")
    out.append(f"## Next Steps (prioritized)\n{PLACEHOLDER}\n")
    return "".join(out).encode("utf-8")


def _go_quote(s: str) -> str:
    """Reproduce Go's strconv.Quote (used by fmt %q for strings) for the
    ASCII subset the skill writes. Wraps in double quotes; escapes \\ " and
    common control chars as Go does. Non-ASCII unicode passes through (Go's
    %q without + flag), so the em dash in PLACEHOLDER round-trips intact.

    Coverage is intentional, not exhaustive — agent_id is hex, task_id is
    a slug, project is a name, paths are filesystem paths. The escape set
    below covers all characters these fields contain in practice plus the
    danger characters (quote, backslash, newline) that would otherwise
    break the line-prefix-matching parsers in cmd/fleet/handoff.go.
    """
    out = ['"']
    for ch in s:
        cp = ord(ch)
        if ch == '"':
            out.append('\\"')
        elif ch == '\\':
            out.append('\\\\')
        elif ch == '\n':
            out.append('\\n')
        elif ch == '\r':
            out.append('\\r')
        elif ch == '\t':
            out.append('\\t')
        elif cp < 0x20 or cp == 0x7f:
            out.append(f'\\x{cp:02x}')
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _format_float_go(v: float) -> str:
    """Match Go's strconv.FormatFloat(v, 'f', -1, 64): whole numbers omit
    the trailing '.0' (so 25.0 → '25'); fractional values use shortest
    round-trip representation."""
    if v == int(v):
        return str(int(v))
    s = repr(v)
    if s.endswith(".0"):
        return s[:-2]
    return s


# -- queue writing -----------------------------------------------------------

def write_queue(*, old_id: str, new_id: str, doc_path: str,
                project: str, task_id: str, ts: datetime) -> bool:
    """Write ~/.fleet/queue/spawn-fresh-<old_id>.json. Returns True on
    success.

    Both new_agent_id AND new_session are pre-populated so the recovery
    probe in cmd/fleet/handoff.go (iter-13 fix) can detect crashed-mid-
    spawn replacements on retry. Omitting new_session makes the
    `tmux has-session` check no-op and produces orphaned auto-handoffs.
    """
    payload = {
        "schema_version": 1,
        "old_agent_id": old_id,
        "handoff_doc": doc_path,
        "project": project,
        "task_id": task_id,
        "new_agent_id": new_id,
        "new_session": f"fleet-{new_id}",
        "enqueued_at": ts.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }
    path = health.fleet_home() / "queue" / f"spawn-fresh-{old_id}.json"
    return _atomic_write_bytes(
        path,
        (json.dumps(payload, indent=2) + "\n").encode("utf-8"),
    )


# -- atomic file write -------------------------------------------------------

def _atomic_write_bytes(path: Path, data: bytes) -> bool:
    """Same pattern as health._atomic_write_json: tempfile in the same
    directory, fsync, os.replace. Returns False on any I/O failure."""
    tmp_path: str | None = None
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            mode="wb",
            dir=path.parent,
            prefix=f".{path.name}.",
            suffix=".tmp",
            delete=False,
        ) as tf:
            tf.write(data)
            tf.flush()
            os.fsync(tf.fileno())
            tmp_path = tf.name
        os.replace(tmp_path, path)
        return True
    except Exception:
        if tmp_path:
            try:
                os.unlink(tmp_path)
            except Exception:
                pass
        return False
