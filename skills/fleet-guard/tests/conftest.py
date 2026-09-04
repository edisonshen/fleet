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


# rc-listener-bootstrap-sk-3e98: defense-in-depth env-gate for fleet-
# guard tests. handoff.first_action() returns the bash bootstrap as a
# STRING (it's never exec'd from Python — operator/agent runs it
# manually), so this isn't strictly required today. But the gate keeps
# fleet-guard symmetric with the coordinator-side gate AND insulates
# against a future change that adds an exec'd bootstrap path here
# (issue #56-style auto-spawn in handoff itself).
#
# Set at module-import time so any test that loads the handoff module
# inherits the gate before the first import; the autouse session
# fixture below re-asserts it for hostile-test scenarios.
os.environ.setdefault("FLEET_RC_BOOTSTRAP_DISABLED", "1")


@pytest.fixture(autouse=True, scope="session")
def _disable_rc_bootstrap_session_fleet_guard() -> None:
    """Mirror of skills/coordinator/tests/conftest.py session gate.
    Lives here for defense in depth: handoff.first_action returns
    markdown today (no exec under pytest), but any future fleet-guard
    code that shells out to `claude remote-control` will inherit the
    gate without extra wiring."""
    os.environ["FLEET_RC_BOOTSTRAP_DISABLED"] = "1"


@pytest.fixture(autouse=True)
def _producer_not_fenced_by_default(monkeypatch: pytest.MonkeyPatch) -> None:
    """Default: the handoff producer is NOT fenced.

    DESIGN-handoff-drain-storm-leak PR4 made `_do_handoff` prove parent-lease
    ownership via `fleet lease-check`. In
    dev / CI / homebrew the real `fleet` binary IS on PATH, so a test that
    seeds no lease record would get exit-3 "fenced" and the producer would
    refuse to write — breaking every unrelated handoff-write test. This
    autouse fixture stubs the fence to "not fenced" so those tests exercise
    the WRITE path; test_producer_fence.py overrides it per-test to assert
    the fence + back-off behavior (per-test monkeypatch wins over autouse).
    Mirrors the coordinator-side conftest._stub_lease_check."""
    import handoff  # noqa: WPS433 — sibling skill module on sys.path

    monkeypatch.setattr(handoff, "_producer_fenced", lambda _project: False)


@pytest.fixture(autouse=True)
def _stub_enrich_doc(monkeypatch: pytest.MonkeyPatch) -> None:
    """Default: the post-write narrative enrichment (`fleet handoff-enrich`)
    is a no-op. It shells out to the real `fleet` binary when one is on
    PATH and rewrites the doc in place; unrelated handoff-write tests
    assert on the bytes `write_doc` produced. test_enrich_doc.py re-patches
    per-test to exercise the real subprocess wiring."""
    import handoff  # noqa: WPS433 — sibling skill module on sys.path

    monkeypatch.setattr(handoff, "_enrich_doc", lambda _doc_path: False)


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
