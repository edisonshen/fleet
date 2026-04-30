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
        sys.stdout.write("\n\n".join(injections))
    return 0


def _on_stop(payload: dict, agent_id: str, session: str,
             injections: list[str]) -> None:
    """Stop fires after every assistant turn. Three concerns, in order:
    update health JSON, deliver any pending operator inbox message, then
    evaluate the handoff state machine. Inbox runs first so the agent
    sees operator context BEFORE deciding whether to wrap with MILESTONE.
    """
    pct, _model = health.read_context_pct(payload)
    # Set needs_input=true: claude has finished a turn and is now waiting
    # for the operator to type something. UserPromptSubmit clears it on the
    # next operator turn. The TUI uses this to show a "waiting" badge so
    # the operator can spot which agent is blocked on them at a glance.
    health.update_record(
        agent_id,
        context_pct=pct,
        context_source="hook",
        needs_input=True,
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


def _on_precompact(payload: dict, agent_id: str, session: str) -> None:
    """PreCompact fires just before context compaction. Stdout is ignored
    by Claude Code on this hook — the compaction is already in motion —
    so emergency_trigger only writes the doc + queue."""
    handoff.emergency_trigger(payload, agent_id=agent_id, session=session)


def _on_user_prompt_submit(agent_id: str) -> None:
    """UserPromptSubmit fires when the operator submits a prompt to the
    agent. That is the moment claude transitions from waiting → working,
    so clear needs_input. Pairs with Stop, which sets needs_input=true.

    Stdout is ignored by Claude Code on this hook (the prompt is already
    being processed); we touch only the agent record.
    """
    health.update_record(agent_id, needs_input=False)


def _on_session_start(agent_id: str, injections: list[str]) -> None:
    """SessionStart fires once per session (resume or fresh). Deliver any
    pending inbox message — the operator may have queued context while the
    agent was idle. No threshold evaluation: context is fresh."""
    inbox_body = inbox.read_pending(agent_id)
    if inbox_body is not None:
        injections.append(inbox.deliver(inbox_body))
        # Only clear inbox_pending on a successful archive. If the rename
        # fails, the file persists and gets re-delivered next fire — the
        # flag must stay set so the TUI's banner agrees with the actual
        # state of disk.
        if inbox.archive(agent_id):
            health.update_record(agent_id, inbox_pending=False)


if __name__ == "__main__":
    sys.exit(main())
