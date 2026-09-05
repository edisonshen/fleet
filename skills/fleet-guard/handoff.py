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

"write doc + queue" is `fleet handoff-write` (cmd/fleet/handoff_write.go):
the skill captures the tmux pane and hands identity + type + pane to Go,
which renders the doc from durable coord state and enqueues the successor
exactly as `fleet handoff <id>` does. No doc bytes are produced here.
"""
from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from typing import Any

import health

HANDOFF_REQUESTED = "HANDOFF REQUESTED"
MILESTONE = "MILESTONE"

# A MILESTONE-only assistant turn does NOT render as a bare "MILESTONE"
# line in the tmux pane: Claude Code prefixes every assistant turn with a
# bullet glyph (`⏺ `), so the pane shows `⏺ MILESTONE`. The agent may also
# indent the token. `.strip() == MILESTONE` demanded an exact bare match
# and therefore NEVER fired on a real pane — the auto-handoff's central
# trigger was dead (the 2026-06 coord that climbed to 52% in auto-yellow
# with handoff_type never committing). Tolerate a leading run of
# whitespace + the agent turn glyph, but keep the trailing `MILESTONE$`
# anchored so `MILESTONES` and `MILESTONE: foo` still do NOT match (no
# false trigger). The `⏺` is U+23FA (Claude Code's turn glyph).
#
# List-bullet markers (`-`, `*`, `•`) are DELIBERATELY EXCLUDED (codex
# review iter-5 [P2]): an agent narrating its plan in response to the
# request ("- MILESTONE once the tests pass") is NOT the terminal
# signal; the injected instruction demands a single token on its own
# line, which renders as `⏺ MILESTONE` (or whitespace-indented), never
# as a list item.
#
# `>` is DELIBERATELY EXCLUDED from the prefix class. The injected
# HANDOFF REQUESTED prompt (inject_handoff_requested) literally contains a
# standalone `MILESTONE` instruction line, and an injected user message
# renders in the pane with a `>` quote prefix on every line (`> MILESTONE`).
# Matching `>`-prefixed lines would make find_milestone fire on the skill's
# OWN instruction echo the instant Yellow injects — cutting off active work
# before the agent ever responds. The agent's genuine signal carries the
# `⏺` assistant glyph (or no prefix), never `>`.
_MILESTONE_LINE_RE = re.compile(r"^[\s⏺]*MILESTONE\s*$")

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

# Modes where auto-handoff is disabled — see SKILL.md Handoff thresholds.
THINKING_MODES = frozenset({"plan", "review", "fix"})

# Stuck-pending watchdog: re-inject HANDOFF REQUESTED if Yellow has
# lingered for this long without a MILESTONE. Without re-injection the
# agent stays wedged at auto-yellow until Red fires at 50% or operator
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
        f"{HANDOFF_REQUESTED}: context window is over 40%. "
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
    appeared AFTER the Yellow cycle's HANDOFF REQUESTED injection. Returns
    False on tmux failure — the skill is non-blocking, and a missed
    MILESTONE just means the next fire tries again.

    Bounded to "after HANDOFF REQUESTED" because the pane scrollback can
    contain prior MILESTONE lines (the agent's own narration of the
    handoff token, an earlier discussion that quoted the marker). Counting
    any historical MILESTONE would fire the handoff the moment Yellow first
    injects — cutting off active work mid-subtask.

    Windowing anchors on the FIRST HANDOFF REQUESTED, not the last. The
    Stop hook / stuck-pending watchdog RE-injects HANDOFF REQUESTED on
    later fires, so the pane shape during a stuck Yellow cycle is:

        HANDOFF REQUESTED   (injection #1, opens the cycle)
        ... agent wraps up ...
        ⏺ MILESTONE         (the valid handoff signal)
        HANDOFF REQUESTED   (injection #2, re-injected after MILESTONE)

    Anchoring on the LAST HANDOFF REQUESTED would window the valid
    MILESTONE OUT — the exact bug behind the 2026-06 coord that looped on
    the request forever while context climbed toward auto-compact. The
    first injection bounds the cycle: within a single live pane there is
    only one Yellow cycle (a completed handoff replaces the agent in a
    fresh pane), so any MILESTONE after the first injection belongs to
    THIS cycle.

    The anchor requires the injected shape `HANDOFF REQUESTED:` (token +
    colon — every injection renders it; see inject_handoff_requested),
    not the bare token (codex review iter-5 [P2]): scrollback can
    contain the bare marker in historical narration ("the doc mentions
    HANDOFF REQUESTED handling"), and anchoring there would admit
    pre-cycle MILESTONE lines. A historical quote of the FULL injected
    phrase can still false-anchor — accepted residual; the blast radius
    is a premature-but-valid handoff, and Red at 50% remains the
    safety net.

    Match tolerates a leading turn-glyph / bullet prefix
    (`⏺ MILESTONE`, `  MILESTONE`) via _MILESTONE_LINE_RE but keeps
    `MILESTONE$` anchored, so "MILESTONES" / "MILESTONE: foo" never
    falsely trigger. `>`-prefixed lines are excluded — the injected
    HANDOFF REQUESTED prompt echoes a `> MILESTONE` instruction line that
    must NOT self-trigger (see _MILESTONE_LINE_RE). A bare
    `.strip() == "MILESTONE"` — what this used to do — never matched a
    real Claude Code pane, where the MILESTONE-only turn renders as
    `⏺ MILESTONE`.

    Capture extends into scrollback (10000 lines) so a long-running
    Yellow cycle — the agent has produced many turns of output between
    the HANDOFF REQUESTED injection and finally emitting MILESTONE —
    still finds both markers. Without scrollback the visible 50-line
    pane window outruns busy agents and find_milestone returns False
    forever, leaving the handoff stuck in auto-yellow until 50% / Red.
    """
    out = _capture_pane(session, lines=10000)
    if not out:
        return False
    # Strip ANSI SGR codes BEFORE matching (codex review iter-6 [P2]):
    # a colored pane renders the milestone as e.g.
    # `\x1b[..m⏺\x1b[0m MILESTONE`, which the raw-line regex would miss —
    # leaving auto-yellow stuck re-injecting until Red. capture_recent
    # already strips with the same regex.
    lines = _ANSI_SGR_RE.sub("", out).splitlines()
    # Find the FIRST occurrence of the INJECTED request shape (token +
    # colon) — the line that opened this Yellow cycle. Re-injections land
    # BELOW the agent's MILESTONE, so anchoring on the last one would
    # window a valid MILESTONE out; the colon requirement keeps bare-token
    # historical narration from anchoring too early.
    request_sentinel = HANDOFF_REQUESTED + ":"
    first_request = -1
    for i, line in enumerate(lines):
        if request_sentinel in line:
            first_request = i
            break
    if first_request == -1:
        # No HANDOFF REQUESTED visible — either it scrolled off or
        # never fired. Conservative: don't trigger. The pending check
        # in maybe_trigger would have called this only when the record
        # already had handoff_type=auto-yellow set, so the injection
        # DID fire on a prior turn; if it scrolled off, the agent
        # eventually crosses 50% and emergency triggers via Red.
        return False
    for line in lines[first_request + 1:]:
        if _MILESTONE_LINE_RE.match(line):
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
            # and is now over 50%. Red is the documented safety net —
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
    """Capture the pane, then hand `fleet handoff-write` the doc + queue
    write (the same Go path `fleet handoff <id>` uses).

    Returns True on success (both doc and queue durable on disk),
    False on any failure. Caller is responsible for clearing the
    pre-marked handoff_type when this returns False — without that
    rollback, the agent stays pending forever and is_handoff_pending
    short-circuits every subsequent fire (no retry, no successor,
    wedged until manual repair).
    """
    agent_id = record["id"]
    task_id = record.get("task_id", "")
    project = record.get("project", "")

    # (a) FENCE (correctness — DESIGN-handoff-drain-storm-leak PR4 item 10a).
    # A COORD that was FENCED (a successor took over its lease) must NOT write
    # a handoff doc / enqueue — that would be
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

    # RE-FENCE immediately before publishing (codex PR4 [P1]). capture_recent
    # takes real wall-clock time (tmux capture); a successor could take the
    # lease in that window. Re-proving ownership right before the write
    # narrows the fenced-producer race to the minimum — without it a fenced
    # old coord could still publish a zombie handoff doc/queue (the exact
    # corruption this guard prevents). Coord producers only (workers don't
    # hold the lease).
    if is_coord and _producer_fenced(project):
        print(
            f"fleet-guard: handoff producer for coord agent {agent_id} "
            f"(project {project!r}) was FENCED while preparing the handoff; "
            f"refusing to publish a zombie doc/queue",
            file=sys.stderr,
        )
        return False

    if not _write_handoff(
        agent_id=agent_id,
        handoff_type=handoff_type,
        pct=pct,
        recent_activity=recent,
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
        # Go consumers reap this sidecar in queue.Delete and fleet gc
        # orphan-kicked; keep the <queue-file>.kicked suffix in sync.
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
    fleet_bin = _fleet_binary()
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


def _fleet_binary() -> str | None:
    """Resolve the fleet binary: `FLEET_BIN` (stamped by spawn) when it
    points at an executable, else `fleet` on PATH, else None."""
    fleet_bin = os.environ.get("FLEET_BIN")
    if not fleet_bin or not os.access(fleet_bin, os.X_OK):
        fleet_bin = shutil.which("fleet")
    return fleet_bin or None


# -- doc + queue write (delegated to Go) ------------------------------------

# Coord identity convention: task_id == "coord-<project>" EXACTLY (mirrors
# internal/spawn.IsCoordSpawn). A prefix match would misclassify a worker
# whose slug merely starts with "coord-".
_COORD_TASK_ID_PREFIX = "coord-"

# `fleet handoff-write` reads coord-state.json / tasks.md / the checkpoint
# and shells `gh pr list` (10s cap on the Go side); leave headroom so a
# slow gh degrades inside Go (checkpoint fallback) rather than being
# killed here and dropping the whole handoff.
_HANDOFF_WRITE_TIMEOUT_S = 30.0


def _write_handoff(*, agent_id: str, handoff_type: str, pct: float | None,
                   recent_activity: str) -> str:
    """Run `fleet handoff-write --agent <id> --type <t> [--context-pct <p>]`
    with the pane capture on stdin. Go builds the doc (frontmatter, First
    Action, Active Subagents, Open PRs, Key Decisions, Docs, Open Questions,
    Next Steps from durable coord state; the capture appended to
    Completed), writes it atomically, then writes the spawn-fresh queue
    file — the same path `fleet handoff <id>` takes.

    Returns the doc path on success, "" on any failure (no binary, too-old
    binary without the subcommand, timeout, non-zero exit, unparseable
    output). The queue file is the durable commit point and is written
    by Go only after the doc is complete on disk."""
    fleet_bin = _fleet_binary()
    if not fleet_bin:
        print(
            f"fleet-guard: no fleet binary (FLEET_BIN / PATH); cannot write "
            f"handoff for {agent_id}",
            file=sys.stderr,
        )
        return ""
    args = ["handoff-write", "--agent", agent_id, "--type", handoff_type]
    if pct is not None:
        args += ["--context-pct", repr(float(pct))]
    proc = _run_handoff_write(fleet_bin, args, recent_activity, agent_id)
    if proc is None:
        return ""
    detail = ""
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        # FLEET_BIN is stamped at spawn; an agent spawned by a binary that
        # predates `handoff-write` keeps pointing at it after an upgrade
        # while the new binary sits on PATH. An unknown command wrote
        # nothing, so retrying a DIFFERENT executable is safe — any other
        # failure is not retried (durable artifacts may already exist).
        alt = shutil.which("fleet")
        if (_lease_check_unknown_command(detail) and alt
                and not _same_executable(alt, fleet_bin)):
            print(
                f"fleet-guard: {fleet_bin} lacks handoff-write; retrying "
                f"with {alt}",
                file=sys.stderr,
            )
            proc = _run_handoff_write(alt, args, recent_activity, agent_id)
            if proc is None:
                return ""
            detail = (proc.stderr or proc.stdout or "").strip()
    if proc.returncode != 0:
        print(
            f"fleet-guard: handoff-write for {agent_id} failed "
            f"(exit {proc.returncode}): {detail}",
            file=sys.stderr,
        )
        return ""
    try:
        result = json.loads(proc.stdout)
    except ValueError:
        result = None
    doc_path = result.get("doc_path") if isinstance(result, dict) else None
    if not isinstance(doc_path, str) or not doc_path:
        print(
            f"fleet-guard: handoff-write for {agent_id} returned no doc_path: "
            f"{proc.stdout.strip()!r}",
            file=sys.stderr,
        )
        return ""
    return doc_path


def _run_handoff_write(fleet_bin: str, args: list[str], recent_activity: str,
                       agent_id: str) -> subprocess.CompletedProcess[str] | None:
    """Run `<fleet_bin> handoff-write ...` with the pane capture on stdin and
    FLEET_HOME pinned. None when the process could not run at all (missing /
    non-executable binary, timeout)."""
    env = dict(os.environ)
    env["FLEET_HOME"] = str(health.fleet_home())
    try:
        return subprocess.run(
            [fleet_bin, *args],
            input=recent_activity,
            capture_output=True, text=True,
            timeout=_HANDOFF_WRITE_TIMEOUT_S, check=False, env=env,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        print(
            f"fleet-guard: handoff-write for {agent_id} did not run "
            f"({fleet_bin}): {exc!r}",
            file=sys.stderr,
        )
        return None


def _same_executable(a: str, b: str) -> bool:
    """True when both paths resolve to the same file (symlinks followed), so
    a PATH lookup that lands on FLEET_BIN itself is not treated as an
    alternative binary."""
    try:
        return os.path.samefile(a, b)
    except OSError:
        return os.path.realpath(a) == os.path.realpath(b)


def _clear_pending(agent_id: str) -> None:
    """Roll back a pre-marked handoff_type when _do_handoff failed
    on the producer side (`fleet handoff-write` failure).
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


# -- lease fence + storm back-off (PR4) --------------------------------------

# `fleet lease-check`'s dedicated "not the lease owner" exit code (mirrors
# cmd/fleet/lease_check.go::leaseCheckNotOwnerExit + loop.py).
_LEASE_CHECK_NOT_OWNER_EXIT = 3
_LEASE_CHECK_TIMEOUT_S = 5.0


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
    against: the binary absent (FileNotFoundError), a too-old binary lacking the
    subcommand (exit-1 unknown-command), or a timeout. A transient/legacy
    environment must not wedge a healthy coord."""
    if not project:
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
    except (OSError, subprocess.SubprocessError):
        # OSError covers FileNotFoundError (no binary) AND a non-executable /
        # permission-denied FLEET_BIN; SubprocessError covers TimeoutExpired.
        # All are "lease-check unavailable" -> fail-open (codex PR4 [P2]). An
        # uncaught OSError would escape to maybe_trigger's handler and, on the
        # already-committed red/precompact path, strand the agent with no
        # doc/queue while future fires skip it as committed.
        return False
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
