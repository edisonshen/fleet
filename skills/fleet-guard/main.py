#!/usr/bin/env python3
"""fleet-guard hook entry point.

Wired into ~/.claude/settings.json by `fleet init` for three hooks:
Stop, PreCompact, SessionStart. Reads the JSON payload on stdin, dispatches
to the right handler, prints any injection text to stdout, returns 0.

Failure contract (SKILL.md): the hook must never block the agent's turn.
Every code path is wrapped in a try/except that logs to stderr and returns 0.
A skill crash leaves the agent running with stale health data; the TUI
polling fallback (1s) is the safety net.
"""
from __future__ import annotations

import io
import json
import os
import sys
from typing import TextIO

# Allow sibling imports when invoked as `python3 main.py`. With
# Path-based imports the skill runs identically whether installed at
# ~/.claude/skills/fleet-guard/ (production) or executed in-tree (dev).
_SKILL_DIR = os.path.dirname(os.path.abspath(__file__))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)

import handoff  # noqa: E402
import health   # noqa: E402
import inbox    # noqa: E402


def main(stdin: TextIO | None = None) -> int:
    """Hook entry. Returns the exit code (always 0). stdin is parameterized
    for tests; production calls it with no argument and reads sys.stdin."""
    raw = (stdin or sys.stdin).read()
    try:
        payload = json.loads(raw) if raw else {}
    except Exception as exc:
        print(f"fleet-guard: payload parse error: {exc}", file=sys.stderr)
        return 0
    if not isinstance(payload, dict):
        return 0

    agent_id = os.environ.get("FLEET_AGENT_ID", "").strip()
    if not agent_id:
        # Agent isn't under Fleet supervision — exit silently without
        # touching disk. This is the documented out-of-Fleet path.
        return 0

    session = f"fleet-{agent_id}"
    hook_name = payload.get("hook_event_name", "")
    injections: list[str] = []

    try:
        if hook_name == "Stop":
            _on_stop(payload, agent_id, session, injections)
        elif hook_name == "PreCompact":
            _on_precompact(payload, agent_id, session)
        elif hook_name == "SessionStart":
            _on_session_start(agent_id, injections)
        elif hook_name == "UserPromptSubmit":
            _on_user_prompt_submit(agent_id)
        # Any other hook event: silent no-op. Future hooks land here without
        # changes to this dispatch table.
    except Exception as exc:
        print(f"fleet-guard: dispatch error in {hook_name}: {exc}",
              file=sys.stderr)
        return 0

    if injections:
        body = "\n\n".join(injections)
        # Claude Code's Stop hook does NOT auto-inject plain stdout
        # into the next turn's context (verified empirically: scanning
        # all transcripts found zero matches for the literal injection
        # prefix). The supported mechanism is JSON with
        # decision="block" + reason=<text>; Claude refuses to stop and
        # treats `reason` as the next assistant-side input. Without
        # this format the agent never sees HANDOFF REQUESTED, never
        # emits MILESTONE, and stays stuck in auto-yellow forever.
        #
        # Other hooks (SessionStart) keep plain-stdout for now — they
        # don't have a documented "block" semantic, and the inbox-on-
        # resume path is less load-bearing than auto-handoff.
        if hook_name == "Stop":
            print(json.dumps({"decision": "block", "reason": body}))
        else:
            sys.stdout.write(body)
    return 0


def _on_stop(payload: dict, agent_id: str, session: str,
             injections: list[str]) -> None:
    """Stop fires after every assistant turn. Four concerns, in order:
    update health JSON with context_pct, deliver any pending operator
    inbox message, evaluate the handoff state machine, then resolve
    needs_input based on whether anything is being injected. Inbox runs
    first so the agent sees operator context BEFORE deciding whether to
    wrap with MILESTONE.

    needs_input semantics: true iff this Stop leaves claude truly idle.
    If the hook injects an inbox message OR a HANDOFF REQUESTED prompt
    via stdout, claude immediately starts another turn — that is
    "working", not "waiting". Marking it waiting in those paths would
    pin the TUI's "waiting" badge on agents that are actively
    processing the injected content (caught by codex review iter-2 P2
    on inbox-driven Stops).
    """
    pct, _model = health.read_context_pct(payload)
    health.update_record(
        agent_id,
        context_pct=pct,
        context_source="hook",
    )

    inbox_body = inbox.read_pending(agent_id)
    if inbox_body is not None:
        injections.append(inbox.deliver(inbox_body))
        # Only clear inbox_pending on a successful archive. If the rename
        # fails, the file persists and gets re-delivered next fire — the
        # flag must stay set so the TUI's banner agrees with the actual
        # state of disk.
        if inbox.archive(agent_id):
            health.update_record(agent_id, inbox_pending=False)

    handoff_inject = handoff.maybe_trigger(
        payload, agent_id=agent_id, session=session,
    )
    if handoff_inject is not None:
        injections.append(handoff_inject)

    # Settle needs_input AFTER injection decisions: empty injections
    # means a real idle Stop (operator must type next), non-empty means
    # claude has work to do on the next turn. UserPromptSubmit clears
    # the flag on the operator's first human prompt either way.
    #
    # has_pending_question splits the idle case: if the agent ended on
    # a question for the operator (heuristic in handoff.detect_question),
    # the TUI labels it "asking"; otherwise "idle". When the agent has
    # work queued (injections present), both flags are False — the
    # agent isn't waiting at all.
    needs_input = len(injections) == 0
    has_question = (
        needs_input
        and handoff.detect_question(handoff.capture_recent(session))
    )
    health.update_record(
        agent_id,
        needs_input=needs_input,
        has_pending_question=has_question,
    )

    # Kick drain LAST, after every hook write to the agent record has
    # landed (codex iter-5 P1: avoid the archive/update_record race).
    #
    # AND only when this Stop made no injections — i.e. the agent is
    # idle. If we just gave it work (inbox message OR a fresh
    # HANDOFF REQUESTED prompt), drain would race the agent's next
    # turn: drain's `/exit` could land while claude is processing
    # the injected content, splitting/duplicating that work (codex
    # iter-6 P2). The kick defers safely — the queue file persists,
    # and the next Stop (after the agent's injected turn settles
    # without producing more injections) will pick it up.
    if not injections:
        handoff.kick_drain_if_pending(agent_id)


def _on_precompact(payload: dict, agent_id: str, session: str) -> None:
    """PreCompact fires just before context compaction. Stdout is ignored
    by Claude Code on this hook — the compaction is already in motion —
    so emergency_trigger only writes the doc + queue."""
    handoff.emergency_trigger(payload, agent_id=agent_id, session=session)
    # Same ordering rationale as _on_stop: kick drain after the hook's
    # last write. emergency_trigger today does no follow-up writes
    # itself, but keeping the kick at the hook's tail is the
    # invariant that makes future writes safe (and matches _on_stop's
    # shape, so the two paths don't diverge).
    handoff.kick_drain_if_pending(agent_id)


def _on_user_prompt_submit(agent_id: str) -> None:
    """UserPromptSubmit fires when the operator submits a prompt to the
    agent. That is the moment claude transitions from waiting → working,
    so clear needs_input AND has_pending_question. Pairs with Stop,
    which recomputes both flags.

    has_pending_question must clear here too: the operator just answered
    whatever the agent asked, so the question is no longer pending. Left
    set, the TUI would mislabel a working agent as "asking" until the
    next Stop fires (codex review iter for asking/idle split: P2).

    Stdout is ignored by Claude Code on this hook (the prompt is already
    being processed); we touch only the agent record.
    """
    health.update_record(
        agent_id,
        needs_input=False,
        has_pending_question=False,
    )


def _on_session_start(agent_id: str, injections: list[str]) -> None:
    """SessionStart fires once per session (resume or fresh). Deliver any
    pending inbox message — the operator may have queued context while the
    agent was idle. No threshold evaluation: context is fresh.

    needs_input semantics mirror Stop: true iff no injection is being
    sent, false if SessionStart hands claude an inbox message that
    starts work immediately. Without the late settle, a resumed agent
    with pending operator context would show "waiting" while it is
    actually processing the injected message (codex review iter-2 P2).
    """
    inbox_body = inbox.read_pending(agent_id)
    if inbox_body is not None:
        injections.append(inbox.deliver(inbox_body))
        # Only clear inbox_pending on a successful archive. If the rename
        # fails, the file persists and gets re-delivered next fire — the
        # flag must stay set so the TUI's banner agrees with the actual
        # state of disk.
        if inbox.archive(agent_id):
            health.update_record(agent_id, inbox_pending=False)
    # Clear has_pending_question on SessionStart: a resumed agent
    # is at a fresh prompt regardless of what it ended on last
    # session. Without this clear, a record that crashed mid-Yellow
    # with has_pending_question=true on disk would render as
    # "asking" forever after the resume — codex review iter for
    # asking/idle split caught this exact path: seed the flag,
    # fire SessionStart, watch it stay set.
    health.update_record(
        agent_id,
        needs_input=(len(injections) == 0),
        has_pending_question=False,
    )


if __name__ == "__main__":
    sys.exit(main())
