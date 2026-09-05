"""pytest config for fleet-guard skill tests.

The skill ships as a flat directory of .py files invoked via `python3 main.py`,
not as an installed package. To let tests import sibling modules (health,
handoff, inbox) without adding `__init__.py` (which would change how the
skill is discovered by Claude Code), we put the skill directory on sys.path.
"""
from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

_SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)

_REPO_ROOT = Path(_SKILL_DIR).parent.parent


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


@pytest.fixture(scope="session")
def fleet_bin(tmp_path_factory: pytest.TempPathFactory) -> Path | None:
    """Build the `fleet` binary from this checkout once per session.

    The auto-handoff doc + queue write is `fleet handoff-write` — the same
    Go path `fleet handoff <id>` takes — so the skill tests that assert on
    doc/queue bytes run the REAL binary rather than a Python re-render
    (which is the mirrored-logic drift this design removes). Building
    from the checkout (not `fleet` on PATH) pins the tests to the code
    under review. Without a Go toolchain (None) the skill falls back to
    `fleet` on PATH exactly as it does in production."""
    go = shutil.which("go")
    if go is None:
        return None
    out = tmp_path_factory.mktemp("fleet-bin") / "fleet"
    subprocess.run(
        [go, "build", "-o", str(out), "./cmd/fleet"],
        cwd=_REPO_ROOT, check=True, capture_output=True, text=True,
    )
    return out


@pytest.fixture(autouse=True)
def _fleet_bin_env(fleet_bin: Path | None, monkeypatch: pytest.MonkeyPatch) -> None:
    """Point FLEET_BIN at the freshly built binary so `handoff-write` (and
    any other shellout that honors FLEET_BIN) runs the checkout's Go code,
    never a stale `fleet` on PATH. Tests that assert on binary resolution
    itself (TestKickDrain) re-set / delete FLEET_BIN per-test; a per-test
    monkeypatch wins over this autouse default."""
    if fleet_bin is None:
        monkeypatch.delenv("FLEET_BIN", raising=False)
    else:
        monkeypatch.setenv("FLEET_BIN", str(fleet_bin))


@pytest.fixture(autouse=True)
def _silence_kick_drain(monkeypatch: pytest.MonkeyPatch) -> None:
    """Producer-triggers-drain (`main._on_stop` / `_on_precompact` call
    `kick_drain_if_pending` after their tail writes) launches a real
    `fleet drain` subprocess when `fleet` is on PATH — which it is in
    dev / CI / homebrew environments. That drain reads FLEET_HOME,
    finds the queue file the test just wrote, and races to delete /
    consume it. A subprocess.Popen that no-ops `fleet drain` (and ONLY
    that — `subprocess.run` is built on Popen, so `fleet handoff-write`
    and every other shellout must still reach the real one) eliminates
    the race for every test by default; tests that want to assert on the
    real kick re-patch subprocess.Popen per-test (per-test monkeypatch
    overrides autouse). Lives in conftest.py so every test file
    inherits it."""
    import handoff  # noqa: WPS433 — sibling skill module on sys.path

    real_popen = subprocess.Popen

    def _popen(args: Any, *rest: Any, **kwargs: Any) -> Any:
        if isinstance(args, (list, tuple)) and list(args)[1:2] == ["drain"]:
            class _FakeProc:
                pass
            return _FakeProc()
        return real_popen(args, *rest, **kwargs)

    monkeypatch.setattr(handoff.subprocess, "Popen", _popen)
