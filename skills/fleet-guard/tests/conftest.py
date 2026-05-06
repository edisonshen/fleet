"""pytest config for fleet-guard skill tests.

The skill ships as a flat directory of .py files invoked via `python3 main.py`,
not as an installed package. To let tests import sibling modules (health,
handoff, inbox, ids) without adding `__init__.py` (which would change how the
skill is discovered by Claude Code), we put the skill directory on sys.path.
"""
from __future__ import annotations

import os
import sys
from typing import Any

import pytest

_SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)


@pytest.fixture(autouse=True)
def _silence_kick_drain(monkeypatch: pytest.MonkeyPatch) -> None:
    """Producer-triggers-drain (`main._on_stop` / `_on_precompact` call
    `kick_drain_if_pending` after their tail writes) launches a real
    `fleet drain` subprocess when `fleet` is on PATH — which it is in
    dev / CI / homebrew environments. That drain reads FLEET_HOME,
    finds the queue file the test just wrote, and races to delete /
    consume it. Module-level no-op subprocess.Popen eliminates the
    race for every test by default; tests that want to assert on the
    real kick re-patch subprocess.Popen per-test (per-test monkeypatch
    overrides autouse). Lives in conftest.py so every test file
    inherits it."""
    import handoff  # noqa: WPS433 — sibling skill module on sys.path

    def _noop_popen(*_args: Any, **_kwargs: Any) -> Any:
        class _FakeProc:
            pass
        return _FakeProc()

    monkeypatch.setattr(handoff.subprocess, "Popen", _noop_popen)
