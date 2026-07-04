"""coord-state sibling-key preservation (curated-handoff-context, Deliverable A).

`fleet checkpoint doc` / `fleet checkpoint decision` (Go CLI) write
coord-state.json:session_docs / recent_decisions out-of-band, under
coordinator.lock. The Python tick is NOT a writer of session_docs — its
one obligation is to PRESERVE both keys through its
_load_coord_state -> mutate -> _save_coord_state cycle (load-mutate-save
the whole dict, never reconstruct). A tick that rebuilt the dict from
known keys would clobber a doc/decision the CLI just recorded, and the
successor coord's "Docs (this session)" / "Key Decisions" would lose it.

These tests pin that contract at the load/save seam.
"""
from __future__ import annotations

import json
from pathlib import Path

import loop


SESSION_DOCS = [
    {
        "path": "docs/DESIGN-handoff-curated-context.md",
        "role": "authored",
        "ts": "2026-07-02T00:00:00Z",
    },
    {
        "path": "docs/TASK-PLAN-curated-handoff-context-5fdd.md",
        "role": "implementing",
        "ts": "2026-07-02T00:01:00Z",
    },
]

RECENT_DECISIONS = [
    "dispatched worker foo (gen 1)",
    "Stopped rebase of PR #224 — superseded PR for an operator-paused task",
]


def _seed_state(path: Path) -> dict:
    state = {
        "worker_agent_ids": {"foo": "agentaaaa"},
        "tick_count": 7,
        "session_docs": SESSION_DOCS,
        "recent_decisions": RECENT_DECISIONS,
        "recent_completions": ["reconciled bar → done"],
    }
    path.write_text(json.dumps(state), encoding="utf-8")
    return state


def test_load_returns_whole_dict_including_cli_owned_keys(tmp_path: Path):
    """_load_coord_state must surface the CLI-owned keys verbatim — a
    loader that projected onto known keys would silently drop them."""
    state_path = tmp_path / "coord-state.json"
    _seed_state(state_path)

    state = loop._load_coord_state(state_path)

    assert state["session_docs"] == SESSION_DOCS
    assert state["recent_decisions"] == RECENT_DECISIONS


def test_tick_style_load_mutate_save_preserves_cli_owned_keys(tmp_path: Path):
    """The tick's load -> mutate-own-keys -> save cycle must ride
    session_docs + recent_decisions through untouched."""
    state_path = tmp_path / "coord-state.json"
    _seed_state(state_path)

    state = loop._load_coord_state(state_path)
    # Mutations the tick actually performs: its own bookkeeping keys.
    state["tick_count"] = state.get("tick_count", 0) + 1
    state["worker_agent_ids"] = {"foo": "agentaaaa", "baz": "agentbbbb"}
    loop._save_coord_state(state_path, state)

    out = json.loads(state_path.read_text(encoding="utf-8"))
    assert out["session_docs"] == SESSION_DOCS
    assert out["recent_decisions"] == RECENT_DECISIONS
    assert out["tick_count"] == 8
    assert out["worker_agent_ids"] == {"foo": "agentaaaa", "baz": "agentbbbb"}


def test_python_side_never_writes_session_docs(tmp_path: Path):
    """The Go CLI is the ONLY session_docs writer. Fresh state through the
    Python load/save cycle must not invent the key."""
    state_path = tmp_path / "coord-state.json"

    state = loop._load_coord_state(state_path)  # missing file → {}
    state["tick_count"] = 1
    loop._save_coord_state(state_path, state)

    out = json.loads(state_path.read_text(encoding="utf-8"))
    assert "session_docs" not in out
