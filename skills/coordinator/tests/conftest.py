"""pytest config for coordinator skill tests.

The skill ships as a flat directory of .py files invoked via Claude Code,
not as an installed package. To let tests import sibling modules
(parse, dispatch, conflict, loop) without adding `__init__.py` (which
would change how Claude Code discovers the skill), put the skill
directory on sys.path here. Mirrors fleet-guard's conftest discipline.

We also pin `FLEET_COORD_POLL_INTERVAL_S=0` by default so the supervisor
loop (issue #79) is disabled in legacy tests — those tests assert the
behavior of the FIRST tick only (reconcile/drain/dispatch). Supervisor-
specific tests opt in by setting the env var to a non-zero value
locally before calling `loop.tick(...)` (or by exercising
`supervisor.run_supervisor` directly with explicit knobs).
"""
from __future__ import annotations

import os
import sys

import pytest

_SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)


@pytest.fixture(autouse=True)
def _disable_supervisor_by_default(monkeypatch):
    """Default: supervisor disabled. Tests that need it set their own
    FLEET_COORD_POLL_INTERVAL_S inside the test body via monkeypatch."""
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "0")
