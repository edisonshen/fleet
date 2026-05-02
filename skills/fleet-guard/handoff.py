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
import shutil
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

# Two distinct "pending" semantics:
#
# _PENDING_TYPES — any of the three auto-* values, meaning the skill is
# tracking a handoff state for this agent. Used by the public-ish
# is_handoff_pending() and the Yellow MILESTONE branch (auto-yellow is the
# only state where MILESTONE matters).
#
# _COMMITTED_TYPES — auto-red or precompact only. These are states where a
# handoff doc + queue file have ALREADY been written and are waiting on
# drain. Red and PreCompact branches bail when the record is committed
# (re-firing would orphan a doc), but they MUST still escalate when only
# auto-yellow is set: Yellow's injection went out without a doc, so Red is
# the safety net the agent reaches by ignoring/missing MILESTONE. SKILL.md
# documents this as the explicit Yellow→Red guarantee.
_PENDING_TYPES = frozenset({TYPE_AUTO_YELLOW, TYPE_AUTO_RED, TYPE_PRECOMPACT})
_COMMITTED_TYPES = frozenset({TYPE_AUTO_RED, TYPE_PRECOMPACT})

# Canonical placeholder string for unfilled body sections. MUST match
# internal/handoff.Placeholder byte-for-byte — alternate sentinels would
# break 4a's chain reader. The em dash is U+2014.
PLACEHOLDER = "_(operator-triggered handoff — fill in before resuming)_"

# Modes where auto-handoff is disabled — see SKILL.md Handoff thresholds.
THINKING_MODES = frozenset({"plan", "review", "fix"})

# Stuck-pending watchdog: re-inject HANDOFF REQUESTED if Yellow has
# lingered for this long without a MILESTONE. Without re-injection the
# agent stays wedged at auto-yellow until Red fires at 70% or operator
# presses [h] — observed in dogfood as "agent at 53% for >1 day, no
# handoff action, 'hello' didn't wake it." The most common root cause
# is a lost prior injection (pre-v0.1.1 stdout-only Stop-hook output);
# crashed tmux pane and operator-edited records also land here.
_YELLOW_RESEND_THRESHOLD_SEC = 30 * 60

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


def _is_handoff_committed(record: dict[str, Any]) -> bool:
    """Doc + queue file have already been written for this agent (auto-red
    or precompact). Re-running _do_handoff would orphan the existing doc.
    Distinct from is_handoff_pending: auto-yellow is pending (injection
    out, no doc) but NOT committed (Yellow→Red escalation must still
    fire if the agent never reaches MILESTONE)."""
    return record.get("handoff_type") in _COMMITTED_TYPES


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

    Capture extends into scrollback (10000 lines) so a long-running
    Yellow cycle — the agent has produced many turns of output between
    the HANDOFF REQUESTED injection and finally emitting MILESTONE —
    still finds both markers. Without scrollback the visible 50-line
    pane window outruns busy agents and find_milestone returns False
    forever, leaving the handoff stuck in auto-yellow until 70% / Red.
    """
    out = _capture_pane(session, lines=10000)
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


# Common openers that signal the agent is asking the operator a question
# even when the line doesn't end in "?". Lowercased; matched as the line
# prefix after stripping whitespace + leading bullets/quotes.
_QUESTION_OPENERS = (
    "do you", "did you", "should i", "should we", "would you",
    "shall i", "shall we", "can i", "can you", "could you",
    "may i", "want me to", "would you like", "are you",
    "is this", "is that", "ok to", "okay to", "ready to",
    "proceed?", "continue?", "approve",
)

# Inline interactive prompts we treat as questions: claude code's own
# y/n confirmation widgets, multi-choice, etc.
_QUESTION_INLINE = (
    "[y/n]", "(y/n)", "[Y/n]", "(Y/n)", "[y/N]", "(y/N)",
    "❯ 1.", "❯ 1)",
)


def detect_question(pane_text: str) -> bool:
    """True if the agent's last visible turn ended on a question for the
    operator. Heuristic — false positives ("Maybe X?" in a status line)
    and false negatives ("Let me know if you'd like X.") are both
    expected. The TUI uses this only to split "asking" vs "idle" labels;
    nothing load-bearing depends on accuracy.

    Strategy: scan the last few non-empty lines (the agent's reply usually
    ends with the question, sometimes followed by short whitespace or
    decorations). Match if any of:
      - line ends with "?"
      - line, lowercased, starts with a known question opener
      - line contains a y/n widget marker
    Empty / pane-capture-failed text returns False (default to idle —
    safer to under-claim than to wake the operator on a false alarm)."""
    if not pane_text:
        return False
    # Walk lines from end backward; stop after inspecting up to 8
    # non-empty lines. Bounded because a long pane can include earlier
    # questions the operator already answered, and we only care about
    # what the final turn ended on.
    inspected = 0
    for raw in reversed(pane_text.splitlines()):
        line = raw.strip()
        if not line:
            continue
        inspected += 1
        if inspected > 8:
            break
        if line.endswith("?"):
            return True
        low = line.lstrip("> -*•").lstrip().lower()
        for opener in _QUESTION_OPENERS:
            if low.startswith(opener):
                return True
        for marker in _QUESTION_INLINE:
            if marker in line:
                return True
    return False


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
        committed = _is_handoff_committed(record)
        yellow_pending = record.get("handoff_type") == TYPE_AUTO_YELLOW

        if state == "red":
            # Bail only if the doc + queue already exist (drain owns it
            # now). auto-yellow is NOT committed: Yellow injected
            # HANDOFF REQUESTED but the agent ignored/missed MILESTONE
            # and is now over 70%. Red is the documented safety net —
            # write the emergency auto-red handoff and overwrite the
            # auto-yellow mark.
            if committed:
                return None
            health.update_record(
                agent_id,
                handoff_type=TYPE_AUTO_RED,
                handoff_type_at=health.now_rfc3339(),
            )
            if not _do_handoff(record, session, TYPE_AUTO_RED, pct):
                _clear_pending(agent_id)
            return None

        if state == "yellow":
            if committed:
                # Doc + queue exist for an earlier Red/PreCompact fire;
                # drain will pick up. Yellow noop.
                return None
            if yellow_pending:
                if find_milestone(session):
                    if not _do_handoff(record, session,
                                       TYPE_AUTO_YELLOW, pct):
                        _clear_pending(agent_id)
                    return None
                # Stuck-pending watchdog: prior HANDOFF REQUESTED
                # may have been lost. Re-stamp + re-inject so the
                # agent gets a fresh nudge; without this it stays
                # wedged until Red or [h]. Treat missing timestamp
                # as "re-inject now" so legacy records (set under a
                # pre-watchdog skill) recover on the first Stop
                # after the operator upgrades.
                if _yellow_stuck_too_long(record):
                    health.update_record(
                        agent_id,
                        handoff_type_at=health.now_rfc3339(),
                    )
                    return inject_handoff_requested()
                return None
            health.update_record(
                agent_id,
                handoff_type=TYPE_AUTO_YELLOW,
                handoff_type_at=health.now_rfc3339(),
            )
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
        # Same semantics as Red: bail only if a doc + queue already exist.
        # auto-yellow (injection out, no doc) escalates to precompact —
        # context is about to be lost; we'd rather overwrite the yellow
        # mark with a real handoff than respect the pending check.
        if _is_handoff_committed(record):
            return
        pct, _ = health.read_context_pct(payload)
        health.update_record(
            agent_id,
            handoff_type=TYPE_PRECOMPACT,
            handoff_type_at=health.now_rfc3339(),
        )
        if not _do_handoff(record, session, TYPE_PRECOMPACT, pct):
            _clear_pending(agent_id)
    except Exception as exc:
        print(f"fleet-guard handoff.emergency_trigger: {exc}",
              file=sys.stderr)


def _do_handoff(record: dict[str, Any], session: str,
                handoff_type: str, pct: float | None) -> bool:
    """Capture the pane, render + write the doc, write the queue file.

    Returns True on success (both doc and queue durable on disk),
    False on any I/O failure. Caller is responsible for clearing the
    pre-marked handoff_type when this returns False — without that
    rollback, the agent stays pending forever and is_handoff_pending
    short-circuits every subsequent fire (no retry, no successor,
    wedged until manual repair).

    Pure best-effort otherwise: any failure aborts the chain so we
    don't leave a queue file pointing at a doc that didn't render.
    """
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
        return False
    if not write_queue(
        old_id=agent_id,
        new_id=new_id,
        doc_path=doc_path,
        project=project,
        task_id=task_id,
        ts=ts,
    ):
        return False
    # NOTE: drain is NOT kicked here. main._on_stop() still has tail
    # writes (capture_recent + final health.update_record) after
    # maybe_trigger() returns; if drain ran in parallel and archived
    # the agent record between update_record's read and write, the
    # os.replace would resurrect it (codex iter-5 P1). Caller invokes
    # `kick_drain_if_pending(agent_id)` after all hook writes finish.
    return True


def kick_drain_if_pending(agent_id: str) -> None:
    """Public entry point: kick `fleet drain` iff this agent's queue file
    exists. Called by main._on_stop / _on_precompact AFTER all hook
    writes complete, so drain's archive can't race the hook's
    read-modify-write of the agent record (codex iter-5 P1).

    Idempotent: noops when no queue file is pending. Safe to call on
    every Stop fire — the filesystem stat is the only cost.
    """
    queue_path = health.fleet_home() / "queue" / f"spawn-fresh-{agent_id}.json"
    if not queue_path.exists():
        return
    _kick_drain()


def _kick_drain() -> None:
    """Fire a detached `fleet drain` so the handoff completes end-to-end
    without depending on a running TUI watcher.

    Detached: start_new_session=True puts drain in its own process group
    so it survives if claude (the parent of fleet-guard) exits between
    queue write and drain finish. DEVNULL on all three streams keeps
    drain's output out of claude's altscreen tty (claude renders that
    pane; stray writes corrupt the rendering).

    Resolution order:
    1. `FLEET_BIN` env (stamped by spawn — internal/spawn/spawn.go via
       os.Executable()). Mirrors the TUI's keys.go fleetBinary trick so
       side-loaded installs, where `which fleet` resolves to nothing
       or a different binary, still self-drain. `go run` is partial:
       works while the operator's parent fleet is alive, breaks once
       the parent exits and the go tool deletes the temp build.
    2. `shutil.which("fleet")` — covers older fleet binaries that spawn
       agents without FLEET_BIN, brew/`go install` setups regardless
       of FLEET_BIN, and any edge case where the stamped path went
       stale (e.g. go run temp build evaporated; in checkout-only
       setups where `fleet` is also not on PATH, fall through).
    3. Noop. Queue file stays on disk and any later drain run consumes
       it. The skill must never raise.
    """
    fleet_bin = os.environ.get("FLEET_BIN")
    if not fleet_bin or not os.access(fleet_bin, os.X_OK):
        fleet_bin = shutil.which("fleet")
    if not fleet_bin:
        return
    try:
        subprocess.Popen(
            [fleet_bin, "drain"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
    except Exception:
        # Producer's job is done (queue file is on disk). Drain is a
        # convenience trigger; failure here just means we wait for the
        # next consumer.
        pass


def _clear_pending(agent_id: str) -> None:
    """Roll back a pre-marked handoff_type when _do_handoff failed
    on the producer side (write_doc or write_queue I/O failure).
    Without this rollback, the next Stop fire sees pending=True and
    Red short-circuits forever. Best-effort — if this update_record
    also fails, the agent stays wedged, but at that point disk I/O
    is broken across the board and the wedge is the operator's
    least concern."""
    health.update_record(agent_id, handoff_type=None, handoff_type_at=None)


def _yellow_stuck_too_long(record: dict[str, Any]) -> bool:
    """True if Yellow has lingered without MILESTONE long enough that
    the watchdog should re-inject HANDOFF REQUESTED. Reads
    `handoff_type_at` as RFC 3339 (matching health.now_rfc3339).

    Missing/unparseable timestamp returns True — that path covers
    legacy records stuck under a pre-watchdog skill. Treating "no
    timestamp" as "very stale" migrates them in one Stop after the
    operator upgrades; the alternative (set the timestamp now and
    wait) leaves operators staring at a still-stuck agent for
    another full threshold window after they explicitly upgraded
    to fix this very bug."""
    raw = record.get("handoff_type_at")
    if not isinstance(raw, str) or not raw:
        return True
    try:
        # health.now_rfc3339 emits trailing "Z"; Python's fromisoformat
        # accepts "Z" only on 3.11+. Normalize to "+00:00" for safety.
        ts = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return True
    if ts.tzinfo is None:
        ts = ts.replace(tzinfo=timezone.utc)
    elapsed = (datetime.now(timezone.utc) - ts).total_seconds()
    return elapsed >= _YELLOW_RESEND_THRESHOLD_SEC


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
        # v2 added DisableAutoResume for the per-handoff auto-resume
        # override (codex review iter-12 P2). fleet-guard never sets
        # an override — auto-handoff inherits from the outgoing record
        # — but writing the current schema version means the consumer
        # can distinguish v2 (auto-resume aware) from v1 (legacy, no
        # auto-resume; consumer skips the prompt for safety).
        "schema_version": 2,
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
