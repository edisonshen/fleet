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

# first_action(project) returns the body of the "First Action (auto)"
# section for the given project. It instructs the resuming agent to
# spawn `claude remote-control` in the background so the operator's
# mobile / claude.ai pairing carries through the fleet-guard handoff.
# Idempotent (pgrep guards re-launch when the daemon is already up).
#
# Per-project (rc-session-name-include): the daemon prefix carries
# the project name (`fleet-handoff-<project>`) so per-project handoff
# daemons coexist on the host and the operator can distinguish
# per-project sessions on phone / claude.ai. The pgrep guard is
# narrowed on the project-scoped prefix with a word-boundary
# terminator `( |$)` so a hypothetical longer prefix like
# `fleet-handoff-sparkX` doesn't false-positive — mirrors the
# coord-side pattern in skills/coordinator/remote_control.py.
#
# Empty project falls back to the legacy generic
# `--remote-control-session-name-prefix "fleet-handoff"` shape so
# rendered docs without a project (legacy records / tests) still
# produce a well-formed bash block.
#
# Body order is load-bearing:
#   1. Bash block: bootstrap the `claude remote-control` daemon (pgrep
#      guard makes it idempotent).
#   2. `/remote-control` slash command (issue #56): attach this fresh
#      chat session to the daemon so the operator's mobile pairing
#      reconnects. Must run BEFORE /coordinator so any supervisor
#      startup output streams through the operator's mobile.
#   3. `/coordinator` slash command (handoff-coord-spawn-prompt-fix):
#      resume the per-project supervisor loop so the new agent
#      acquires ~/.fleet/projects/<p>/.locks/coordinator.lock with its
#      own 8-hex ID. Without this paragraph, replacement coord
#      sessions never run /coordinator — the predecessor's lock body
#      persists, the TUI dashboard's coord display shows the OLD ID,
#      and the task queue stops draining silently. Universal
#      (non-coord lineages also get the line) because /coordinator is
#      idempotent: NB-flock skips when held, and a non-coord cwd has
#      no project to supervise → exit clean.
#
# Slash commands run in chat (not bash), so they're separate
# paragraphs after the bash block, not piped continuations of it.
#
# Issue #31, #56. MUST stay byte-identical with
# internal/handoff.FirstAction (Go side) — auto-handoffs (this file) and
# operator-triggered handoffs (Go) emit the same doc shape, and
# renderers are tested for byte-equality.


def _escape_project_for_pgrep(s: str) -> str:
    """Escape regex metacharacters in the project-scoped daemon prefix
    when used as a pgrep -f pattern. ValidateProjectName allows
    `[a-z0-9._-]`; among those only `.` is a regex metacharacter
    (matches any char). Hyphens are special only inside character
    classes; underscores are always literal.

    Mirror of internal/handoff.escapeProjectForPgrep on the Go side —
    keeping the two renderers byte-equal is a load-bearing invariant
    (test_handoff.TestRenderByteGolden).
    """
    return s.replace(".", "\\.")


def first_action(project: str) -> str:
    """Return the v0.12 operator-instruction body for the "First Action
    (auto)" handoff section.

    v0.12 (DESIGN-rc-listener-lifecycle.md §"Handoff doc rewrite"):
    the embedded bash bootstrap is GONE. The body directs the operator
    to run `fleet rc connect <project>` to re-attach mobile/web
    pairing — a small UX regression (operator types one command) for a
    large architectural win (no bash exec from a markdown file → no
    5,620-mobile-push regressions from a stuck reviewer loop).

    Empty project falls back to `<project>` placeholder text so legacy
    records / tests produce well-formed output.

    MUST stay byte-identical with internal/handoff.FirstAction (Go).
    Both renderers are tested for byte-equality
    (TestRender_SkillByteGolden + EXPECTED_GOLDEN here in Python).
    """
    display = project if project else "<project>"
    return (
        "To re-attach mobile/web pairing for this coord, run in your terminal:\n"
        "\n"
        f"    fleet rc connect {display}\n"
        "\n"
        "(Or `/remote-control` from within Claude Code.) The pairing resumes\n"
        "from where the previous coord left off, provided RC was previously\n"
        f"enabled via `fleet rc up {display}`.\n"
        "\n"
        "If RC was not previously enabled, run:\n"
        "\n"
        f"    fleet rc up {display}\n"
        "\n"
        f"first, then `fleet rc connect {display}`.\n"
        "\n"
        "Then run the slash command `/coordinator` (in the chat, not bash) to resume the per-project supervisor tick loop. The /coordinator skill is idempotent — running it on a coord session that already holds the NB-flock is a no-op (the flock skips when held), and on a non-coord lineage it exits cleanly with no project to supervise.\n"
        "\n"
        "Then continue with the sections below."
    )

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

    # (a) FENCE (correctness — DESIGN-handoff-drain-storm-leak PR4 item 10a).
    # Under FLEET_LEASE_FAILOVER, a COORD that was FENCED (a successor took
    # over its lease) must NOT write a handoff doc / enqueue — that would be
    # a zombie producer write the new leader's state would have to reconcile.
    # Prove parent-lease ownership exactly as the supervisor tick does
    # (via `fleet lease-check`); a fenced producer is REFUSED, not merely
    # backed off. Returning False here makes the caller clear the pending
    # mark and the (dead) coord stops trying.
    #
    # The fence applies to COORD producers ONLY (codex PR4 [P1]/[P2]). A
    # worker pane is NOT a descendant of the project's `coord-run` supervisor,
    # so `fleet lease-check --project <p>` would return exit 3 (the coord lease
    # owner is not the worker's ancestor) while a perfectly healthy coord lease
    # exists — refusing a legitimate WORKER handoff and dropping its context.
    # Workers don't hold the coord lease and can't be fenced by it. The coord
    # identity is the EXACT task_id "coord-<project>" (mirrors
    # internal/tui.coordTaskID); a prefix match would misclassify a worker
    # whose slug merely starts with "coord-" (codex PR4 [P2]).
    is_coord = bool(project) and task_id == _COORD_TASK_ID_PREFIX + project
    if is_coord and _producer_fenced(project):
        print(
            f"fleet-guard: handoff producer for coord agent {agent_id} "
            f"(project {project!r}) is FENCED — a successor coord holds the "
            f"lease; refusing to write a zombie handoff doc/queue",
            file=sys.stderr,
        )
        return False

    # (b) BACK-OFF (storm suppression — PR5-deferred half, the lease-coupled
    # producer back-off). Even when validly the leader, do NOT re-fire a
    # handoff while one is already in flight: a queue file already enqueued
    # for THIS agent means a successor is live / being spawned. Writing doc
    # #2..#16 on top of it is exactly the storm the incident showed (16 docs,
    # zero successors). Back off silently — the in-flight handoff owns it.
    if _handoff_already_in_flight(agent_id):
        return True  # treat as success: the handoff is already happening

    recent = capture_recent(session)
    # Issue #93 Phase B2: when the outgoing agent is a coord, harvest
    # its in-flight worker subagents so the successor can re-dispatch
    # them. Coord identity convention is task_id="coord-<project>"
    # (mirrors internal/tui.coordTaskID). Best-effort: any read failure
    # falls through to an empty list; the section still renders with
    # the canonical placeholder body and the successor parser sees
    # zero entries.
    active_subagents = _collect_active_subagents(task_id, project)
    # v0.8.3: capture open worker PRs via `gh pr list`. The successor
    # coord re-spawns one shepherd per URL listed. Best-effort: gh
    # missing / network failure / non-zero exit all produce [] and the
    # `_(no open PRs)_` placeholder renders.
    open_prs = _collect_open_prs()

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
        active_subagents=active_subagents,
        open_prs=open_prs,
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


_KICK_BACKOFF_S = 30


def kick_drain_if_pending(agent_id: str) -> None:
    """Public entry point: kick `fleet drain` iff this agent's queue file
    exists AND we haven't kicked it within the last _KICK_BACKOFF_S
    seconds. Called by main._on_stop / _on_precompact AFTER all hook
    writes complete (codex iter-5 P1: avoid the archive/update_record
    race).

    Throttle rationale: some drain failures are permanent and
    intentionally leave the queue file in place — DisableAutoResume
    agents are rejected by spawnAndRetire (interactive `fleet handoff`
    is the supported path), and legacy v1 records without cwd can hit
    similar paths. Without throttling, every Stop forks a doomed
    `fleet drain` — log spam, wasted cycles, no signal to the
    operator since stdio goes to DEVNULL (codex iter-7 P2). The
    sentinel file's mtime carries cross-process state without
    needing the agent record schema.

    Idempotent + cheap when no queue file pending — safe to call on
    every Stop fire. Stale sentinel from a long-completed drain is
    cleaned up here too.
    """
    queue_path = health.fleet_home() / "queue" / f"spawn-fresh-{agent_id}.json"
    sentinel = queue_path.with_name(queue_path.name + ".kicked")
    if not queue_path.exists():
        # Drain consumed the queue; clean up the sentinel so a future
        # handoff re-fires immediately instead of being throttled by
        # an unrelated past kick.
        try:
            sentinel.unlink()
        except FileNotFoundError:
            pass
        except OSError:
            pass
        return
    try:
        last_kick_age = (
            datetime.now(timezone.utc).timestamp()
            - sentinel.stat().st_mtime
        )
        if last_kick_age < _KICK_BACKOFF_S:
            return
    except FileNotFoundError:
        pass  # never kicked; proceed
    if not _kick_drain():
        # Popen didn't actually launch (no fleet binary, Popen
        # exception). Don't stamp the sentinel: it gates the
        # throttle, and refusing to advance it lets the next Stop
        # retry. The cost is: log spam if the failure is permanent
        # (DisableAutoResume + no fleet binary at the same time —
        # rare). With a working fleet binary, the throttle works as
        # intended once a launch succeeds.
        return
    try:
        sentinel.touch()
    except OSError:
        # Best-effort: if we can't stamp the sentinel, the worst
        # outcome is we re-kick on the next Stop. No correctness
        # impact.
        pass


def _kick_drain() -> bool:
    """Fire a detached `fleet drain` so the handoff completes end-to-end
    without depending on a running TUI watcher. Returns True iff
    Popen actually launched a drain process; False on missing binary
    or Popen failure. Caller uses this to decide whether to stamp the
    in-flight sentinel (codex iter-9 P2 — don't suppress inbox on a
    drain that never started).

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
    3. Noop, return False. Queue file stays on disk and any later
       drain run consumes it. The skill must never raise.
    """
    fleet_bin = os.environ.get("FLEET_BIN")
    if not fleet_bin or not os.access(fleet_bin, os.X_OK):
        fleet_bin = shutil.which("fleet")
    if not fleet_bin:
        return False
    try:
        subprocess.Popen(
            [fleet_bin, "drain"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        return True
    except Exception:
        # Producer's job is done (queue file is on disk). Drain is a
        # convenience trigger; failure here just means we wait for
        # the next consumer.
        return False


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

# Issue #93 Phase B2: Active Subagents section pinned by
# internal/handoff.ActiveSubagentsNonePlaceholder. Both sides MUST
# stay byte-identical — the Go byte-golden test (TestRender_SkillByteGolden)
# and the Python golden (test_render_byte_golden) cross-verify this.
ACTIVE_SUBAGENTS_NONE_PLACEHOLDER = "_(none)_"

# v0.8.3: Open PRs snapshot section. Pinned by
# internal/handoff.OpenPRsNonePlaceholder. Empty list / None renders the
# placeholder; non-empty renders one bullet per PR.
OPEN_PRS_NONE_PLACEHOLDER = "_(no open PRs)_"


def write_doc(*, agent_id: str, task_id: str, project: str,
              handoff_type: str, number: int, prev_path: str | None,
              context_pct: float | None, ts: datetime,
              recent_activity: str,
              active_subagents: list[dict] | None = None,
              open_prs: list[dict] | None = None) -> str:
    """Render and atomically write the handoff doc. Returns the absolute
    path on success, "" on failure. Body has the captured tmux pane in
    `## Completed` (per plan D3 / SKILL.md) and Placeholder in the other
    four sections — the operator or successor agent fills those before
    resuming.

    active_subagents (issue #93 Phase B2): list of in-flight worker
    subagents the outgoing coord had spawned via the Agent tool. Each
    entry is a dict with keys task, branch, phase, status, pr_url,
    agent_id, subagent_id (any may be empty string). Defaults to None
    which renders the `_(none)_` placeholder.

    open_prs (v0.8.3): list of open worker PRs at handoff time. Each
    entry is a dict with keys number, title, head, url. Defaults to
    None which renders the `_(no open PRs)_` placeholder. The successor
    coord re-spawns one shepherd until-loop per URL listed."""
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
        active_subagents=active_subagents,
        open_prs=open_prs,
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
                recent_activity: str,
                active_subagents: list[dict] | None = None,
                open_prs: list[dict] | None = None) -> bytes:
    """Render the handoff doc bytes. Frontmatter ordering and quoting must
    match internal/handoff.Render byte-for-byte.

    active_subagents: optional list of {task, branch, phase, status,
    pr_url, agent_id, subagent_id} dicts; missing keys default to "".
    None or empty list renders the `_(none)_` placeholder body. See
    internal/handoff/handoff.go renderActiveSubagents for the matching
    Go-side shape.

    open_prs (v0.8.3): optional list of {number, title, head, url}
    dicts capturing open worker PRs at handoff time. None / empty list
    renders the `_(no open PRs)_` placeholder. The successor coord
    re-spawns one shepherd until-loop per URL."""
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
    out.append(f"## First Action (auto)\n{first_action(project)}\n\n")
    out.append(f"## Completed\n{completed}\n\n")
    out.append(f"## Key Decisions\n{PLACEHOLDER}\n\n")
    out.append(f"## Files Modified\n{PLACEHOLDER}\n\n")
    out.append(f"## Open Questions\n{PLACEHOLDER}\n\n")
    out.append(f"## Next Steps (prioritized)\n{PLACEHOLDER}\n\n")
    out.append(
        f"## Active Subagents\n{_render_active_subagents(active_subagents)}\n\n",
    )
    out.append(
        f"## Open PRs\n{_render_open_prs(open_prs)}\n",
    )
    return "".join(out).encode("utf-8")


def _render_open_prs(prs: list[dict] | None) -> str:
    """Mirror of internal/handoff.renderOpenPRs. Empty list / None
    renders the canonical `_(no open PRs)_` placeholder; non-empty
    renders one `- #<num> <title> — <head> — <url>` bullet per entry,
    input order preserved.

    Embedded newlines / carriage returns in titles are replaced with
    spaces so each entry stays one physical line — the bullet-per-PR
    contract must hold even when the upstream `gh pr list` returns a
    title with embedded whitespace control codes.
    """
    if not prs:
        return OPEN_PRS_NONE_PLACEHOLDER
    lines: list[str] = []
    for p in prs:
        try:
            num = int(p.get("number", 0) or 0)
        except (TypeError, ValueError):
            num = 0
        title = str(p.get("title", "") or "").replace("\n", " ").replace("\r", " ")
        head = str(p.get("head", "") or p.get("headRefName", "") or "")
        url = str(p.get("url", "") or "")
        lines.append(f"- #{num} {title} — {head} — {url}")
    return "\n".join(lines)


def _collect_open_prs() -> list[dict]:
    """Run `gh pr list --state open --search "head:worker/" --json
    number,title,headRefName,url` and return the parsed list. Best-
    effort: any gh failure, missing binary, json parse error, or
    timeout returns []. The handoff doc still writes; the
    `_(no open PRs)_` placeholder body renders.

    Search filter `head:worker/` matches any branch whose name starts
    with `worker/` — the convention for fleet worker branches. Other
    in-flight branches (release-prep, hotfix, etc.) are intentionally
    excluded; the successor coord only re-spawns shepherds for
    worker-track PRs.
    """
    gh = shutil.which("gh")
    if not gh:
        return []
    try:
        result = subprocess.run(
            [
                gh, "pr", "list",
                "--state", "open",
                "--search", "head:worker/",
                "--json", "number,title,headRefName,url",
            ],
            capture_output=True, text=True, timeout=10, check=False,
        )
    except Exception:
        return []
    if result.returncode != 0 or not result.stdout.strip():
        return []
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError:
        return []
    if not isinstance(data, list):
        return []
    out: list[dict] = []
    for item in data:
        if not isinstance(item, dict):
            continue
        out.append({
            "number": item.get("number", 0),
            "title": item.get("title", ""),
            "head": item.get("headRefName", ""),
            "url": item.get("url", ""),
        })
    return out


def _render_active_subagents(subs: list[dict] | None) -> str:
    """Mirror of internal/handoff.renderActiveSubagents. Empty list
    renders the canonical placeholder; non-empty renders one
    `- task=<q> branch=<q> phase=<q> status=<q> pr_url=<q> agent_id=<q>
    subagent_id=<q>` line per entry (v0.8.3+ 7-field shape),
    deterministic order (input order preserved).

    Each value is Go-quoted via _go_quote so the Go-side parser's
    strconv.Unquote round-trips unchanged. Missing keys default to
    empty string — the field set is uniform across rows so the parser
    can rely on positional layout if needed.

    status + pr_url are sourced from tasks.md per slug
    (_collect_active_subagents reads them); legacy callers passing
    dicts without these keys get the empty-string default, which the
    successor parser treats as "no PR yet, re-dispatch Agent" — the
    pre-enrichment behavior.
    """
    if not subs:
        return ACTIVE_SUBAGENTS_NONE_PLACEHOLDER
    lines: list[str] = []
    for s in subs:
        task = str(s.get("task", "") or "")
        branch = str(s.get("branch", "") or "")
        phase = str(s.get("phase", "") or "")
        status = str(s.get("status", "") or "")
        pr_url = str(s.get("pr_url", "") or "")
        agent_id = str(s.get("agent_id", "") or "")
        subagent_id = str(s.get("subagent_id", "") or "")
        lines.append(
            "- task={t} branch={b} phase={p} status={st} pr_url={u} "
            "agent_id={a} subagent_id={s}".format(
                t=_go_quote(task), b=_go_quote(branch),
                p=_go_quote(phase), st=_go_quote(status),
                u=_go_quote(pr_url), a=_go_quote(agent_id),
                s=_go_quote(subagent_id),
            )
        )
    return "\n".join(lines)


# Coord identity convention: task_id="coord-<project>" mirrors
# internal/tui.coordTaskID. fleet-guard never ran on a coord pre-v0.2 so
# this constant is new — adding it here keeps the lookup contained
# (no additional cross-module imports).
_COORD_TASK_ID_PREFIX = "coord-"


def _collect_active_subagents(task_id: str, project: str) -> list[dict]:
    """Read the coord's coord-state.json and return one active-subagent
    entry per in-flight worker. Returns [] when the agent isn't a coord
    or any read step fails (best-effort: handoff doc still writes).

    Worker identity comes from coord-state.json's `worker_agent_ids`
    map (slug → 8-hex Fleet agent_id; written by the coord skill on
    every dispatch). subagent_id stays empty when the coord skill
    hasn't captured it (Phase A's DISPATCH-block flow doesn't feed
    captured Claude IDs back to the skill); in that case the
    successor re-dispatches via the AgentID's inbox file regardless.

    Phase (last-known) comes from each worker's
    workers/<slug>/state.json. Missing/unparseable state.json yields
    phase="" — the entry is still emitted because the agent_id
    + slug are enough to drive a re-dispatch.

    status + pr_url are sourced from tasks.md per slug (v0.8.3+: the
    successor coord branches its re-dispatch decision on these so we
    skip Agent re-dispatch when the worker already has an open PR in
    review). Read failures default both to ""; the successor falls
    back to "always re-dispatch Agent" — the pre-enrichment behavior.
    """
    if not task_id or not task_id.startswith(_COORD_TASK_ID_PREFIX):
        return []
    if not project:
        return []
    try:
        coord_state_path = (
            health.fleet_home() / "projects" / project / "coord-state.json"
        )
        with open(coord_state_path, "r", encoding="utf-8") as fh:
            coord_state = json.load(fh)
    except (FileNotFoundError, NotADirectoryError, PermissionError, json.JSONDecodeError):
        return []
    if not isinstance(coord_state, dict):
        return []
    worker_map = coord_state.get("worker_agent_ids", {})
    if not isinstance(worker_map, dict):
        return []
    # Future-proof: a Phase B follow-up may add a parallel slug →
    # claude-subagent-id map. Read it if present so an upgrade adds
    # data without changing this function's signature.
    subagent_map = coord_state.get("worker_subagent_ids", {})
    if not isinstance(subagent_map, dict):
        subagent_map = {}
    # Read tasks.md once per handoff write, not once per slug — 50-task
    # projects would re-scan the same file 50 times otherwise. The
    # helper returns {slug: (status, pr_url)} with empty defaults for
    # any unread slug.
    task_meta = _read_tasks_meta(project)
    out: list[dict] = []
    # Sort by slug for deterministic doc shape — the byte-golden test
    # cares about ordering, and operators reading the doc benefit from
    # alphabetic stability across handoffs.
    for slug in sorted(worker_map.keys()):
        agent_id = worker_map.get(slug, "")
        if not isinstance(slug, str) or not isinstance(agent_id, str):
            continue
        if not slug or not agent_id:
            continue
        phase = _read_worker_phase(project, slug)
        sub_id = subagent_map.get(slug, "")
        if not isinstance(sub_id, str):
            sub_id = ""
        status, pr_url = task_meta.get(slug, ("", ""))
        out.append({
            "task": slug,
            "branch": f"worker/{slug}",
            "phase": phase,
            "status": status,
            "pr_url": pr_url,
            "agent_id": agent_id,
            "subagent_id": sub_id,
        })
    return out


def _read_tasks_meta(project: str) -> dict[str, tuple[str, str]]:
    """Read ~/.fleet/projects/<project>/tasks.md and return
    {slug: (status, pr_url)} for every parseable task block.

    Minimal inline reader (no import of the coord skill's parse.py
    because that lives in a sibling skill directory and fleet-guard
    runs with sys.path scoped to its own folder). We only need two
    bullet lines per task block — status + pr_url — and the bullet
    grammar is simple enough that a hand-rolled scan beats pulling
    in a yaml/regex dependency.

    Tolerant: any I/O, parse, or schema failure returns {}; the
    handoff doc renders with empty status/pr_url and the successor
    falls back to pre-enrichment "always re-dispatch" semantics.
    """
    if not project:
        return {}
    try:
        tasks_path = (
            health.fleet_home() / "projects" / project / "tasks.md"
        )
        with open(tasks_path, "r", encoding="utf-8") as fh:
            text = fh.read()
    except (FileNotFoundError, NotADirectoryError, PermissionError, OSError):
        return {}

    out: dict[str, tuple[str, str]] = {}
    current_slug: str | None = None
    status = ""
    pr_url = ""
    # Bullet section ends on the first blank line OR the next H2/H3.
    # Past that, we're in body sections that may contain `- ` markdown
    # bullets unrelated to task fields. Track in_bullets to avoid
    # mis-reading a Spec/Acceptance/Notes bullet as a task field.
    in_bullets = False
    for raw in text.replace("\r\n", "\n").split("\n"):
        if raw.startswith("## task: "):
            # Flush previous block.
            if current_slug:
                out[current_slug] = (status, pr_url)
            current_slug = raw[len("## task: "):].strip() or None
            status = ""
            pr_url = ""
            in_bullets = False
            continue
        if raw.startswith("## "):
            # Non-task H2 (e.g. footer). Flush + stop scanning.
            if current_slug:
                out[current_slug] = (status, pr_url)
                current_slug = None
            break
        if current_slug is None:
            continue
        stripped = raw.strip()
        if stripped == "":
            # First blank line after bullets closes the bullet section.
            if in_bullets:
                in_bullets = False
            continue
        if raw.startswith("### "):
            # H3 (Spec/Acceptance/Notes) — no more task bullets.
            in_bullets = False
            continue
        if raw.startswith("- ") and not in_bullets:
            # First bullet seen for this block opens the bullet section.
            in_bullets = True
        if not in_bullets or not raw.startswith("- "):
            continue
        # Parse "- key: value" — split on first colon only.
        body = raw[2:]
        colon = body.find(":")
        if colon < 0:
            continue
        key = body[:colon].strip()
        val = body[colon + 1:].strip()
        if val == "null":
            val = ""
        if key == "status":
            status = val
        elif key == "pr_url":
            pr_url = val
    # EOF flush.
    if current_slug:
        out[current_slug] = (status, pr_url)
    return out


def _read_worker_phase(project: str, slug: str) -> str:
    """Read workers/<slug>/state.json and return its `phase` field.
    Returns "" on any error; the active-subagents entry still renders
    with empty phase, and the successor coord's re-dispatch logic
    relies on WIP-file existence + tasks.md status — phase is triage
    context only."""
    try:
        path = (
            health.fleet_home() / "projects" / project /
            "workers" / slug / "state.json"
        )
        with open(path, "r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (FileNotFoundError, NotADirectoryError, PermissionError, json.JSONDecodeError):
        return ""
    if not isinstance(data, dict):
        return ""
    phase = data.get("phase", "")
    return phase if isinstance(phase, str) else ""


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


# -- lease fence + storm back-off (PR4) --------------------------------------

# `fleet lease-check`'s dedicated "not the lease owner" exit code (mirrors
# cmd/fleet/lease_check.go::leaseCheckNotOwnerExit + loop.py).
_LEASE_CHECK_NOT_OWNER_EXIT = 3
_LEASE_CHECK_TIMEOUT_S = 5.0


def _lease_failover_enabled() -> bool:
    """Mirror coordlock.FailoverEnabled's tri-state gate (PR4 default ON):
    enabled unless FLEET_LEASE_FAILOVER is EXPLICITLY 0/false/off/no
    (case-insensitive)."""
    return (os.environ.get("FLEET_LEASE_FAILOVER", "") or "").strip().lower() \
        not in ("0", "false", "off", "no")


def _lease_check_unknown_command(stderr_text: str) -> bool:
    """True if a `fleet lease-check` exit-1 was an UNKNOWN-COMMAND error (the
    installed binary is too old to have the subcommand) vs a genuine internal
    error. Mirrors loop.py._lease_check_unknown_command byte-for-byte."""
    low = stderr_text.lower()
    return "unknown command" in low or "unknown subcommand" in low


def _producer_fenced(project: str) -> bool:
    """True iff this COORD producer is FENCED / cannot prove ownership and so
    must NOT write a handoff doc/queue.

    Routes through `fleet lease-check --project <p>` (the Go epoch re-read +
    ancestor proof). Fences on:
      - exit 3 (definitive "not owner"); AND
      - exit 1 that is a genuine INTERNAL error (e.g. corrupt coordinator.epoch)
        — "cannot prove ownership" -> fail CLOSED (codex PR4 [P1]).
    Fails OPEN (returns False) only when there is no lease machinery to fence
    against: failover off, the binary absent (FileNotFoundError), a too-old
    binary lacking the subcommand (exit-1 unknown-command), or a timeout. A
    transient/legacy environment must not wedge a healthy coord."""
    if not _lease_failover_enabled() or not project:
        return False
    env = dict(os.environ)
    env["FLEET_HOME"] = str(health.fleet_home())
    fleet_bin = os.environ.get("FLEET_BIN", "fleet")
    try:
        proc = subprocess.run(
            [fleet_bin, "lease-check", "--project", project],
            capture_output=True, text=True,
            timeout=_LEASE_CHECK_TIMEOUT_S, check=False, env=env,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return False  # fail-open: don't wedge a legacy/slow environment
    if proc.returncode == 0:
        return False
    if proc.returncode == _LEASE_CHECK_NOT_OWNER_EXIT:
        return True
    # exit 1: too-old binary (unknown command) -> fail open; else internal
    # error -> cannot prove ownership -> FENCE.
    detail = (proc.stderr or proc.stdout or "").strip()
    if _lease_check_unknown_command(detail):
        return False
    print(
        f"fleet-guard: lease-check for {project!r} could not prove ownership "
        f"(exit {proc.returncode}): {detail}; treating as FENCED",
        file=sys.stderr,
    )
    return True


def _handoff_already_in_flight(agent_id: str) -> bool:
    """True iff a handoff for agent_id is already enqueued (a successor is
    live / being spawned). The durable signal is the consumer queue file
    ~/.fleet/queue/spawn-fresh-<id>.json, which `write_queue` drops and the
    consumer deletes when the handoff completes. Its presence means do NOT
    write doc #2..#16 — that is the storm. Best-effort: any stat error
    returns False (write the doc rather than silently skip a real handoff)."""
    try:
        qpath = health.fleet_home() / "queue" / f"spawn-fresh-{agent_id}.json"
        return qpath.exists()
    except OSError:
        return False


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
