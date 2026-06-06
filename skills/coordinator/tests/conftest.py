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


# rc-listener-bootstrap-sk-3e98: set FLEET_RC_BOOTSTRAP_DISABLED before
# any test imports the skill modules. Mirrors the Go-side TestMain
# pattern in cmd/fleet/main_test.go. Without this, `loop.tick()` tests
# (and any other path through remote_control.spawn_daemon_if_needed)
# would fork a real `claude remote-control` listener on every test run,
# registering with the operator's Claude Code service and pushing a
# mobile notification per `pytest skills/` invocation.
#
# Process-env vs autouse session fixture: setting the env at module
# import time guarantees coverage even for tests that monkeypatch the
# env in their own fixtures BEFORE the autouse fixture runs (pytest
# resolves env-touching fixtures in declaration order; module-level
# `os.environ` writes happen at collection time which is strictly
# earlier). The session fixture below is belt-and-suspenders for cases
# where a hostile test deleted the env in setup.
os.environ.setdefault("FLEET_RC_BOOTSTRAP_DISABLED", "1")


@pytest.fixture(autouse=True, scope="session")
def _disable_rc_bootstrap_session() -> None:
    """Session-wide gate against accidental `claude remote-control`
    spawns during the pytest run. The env-var read in
    remote_control.spawn_daemon_if_needed (and the parallel sites
    documented in skills/coordinator/SKILL.md) short-circuits when this
    is set to any non-empty value.

    Tests that need the production-default behaviour (the bootstrap
    actually invokes subprocess.Popen with the bash payload) call
    `enable_rc_bootstrap_for_test(monkeypatch)` from
    test_rc_bootstrap_env_gate.py — that helper clears the env for
    one test, then pytest's monkeypatch fixture restores it on teardown.
    """
    os.environ["FLEET_RC_BOOTSTRAP_DISABLED"] = "1"


@pytest.fixture(autouse=True)
def _stub_repo_binder_to_cwd(monkeypatch):
    """Default: resolve the coord's repo binding to the test-supplied cwd.

    Design 3 (loop-binder-shellout-d6a1) made `loop.tick` resolve its repo
    binding by shelling out to `fleet project resolve-repo` (the single Go
    binder). The vast majority of coordinator tests exercise UNRELATED tick
    behavior (pr-watch, reaper, remote-control, dispatch) and pass an
    explicit `cwd=` expecting it to be used as the worktree base — they
    must not have to stand up a real binder + meta.json. This autouse
    fixture substitutes the resolver seam with a stub that echoes the
    test's cwd, restoring the pre-Design-3 "use the caller cwd" behavior
    those tests rely on.

    Tests that specifically exercise the binder shell-out (P1..P7 in
    test_loop_repo_path.py) override `loop._resolve_repo_fn` /
    `loop.subprocess.run` themselves; a later monkeypatch in the test body
    wins over this autouse default.
    """
    import loop

    def _echo_cwd(project, *, home, fleet_bin="fleet", cwd=None):
        return (cwd, "")

    monkeypatch.setattr(loop, "_resolve_repo_fn", _echo_cwd)


@pytest.fixture(autouse=True)
def _disable_supervisor_by_default(monkeypatch):
    """Default: supervisor disabled. Tests that need it set their own
    FLEET_COORD_POLL_INTERVAL_S inside the test body via monkeypatch.

    FLEET_COORD_POLL_BASE_INTERVAL_S=0 pins the legacy single-rate
    driver (poll_interval_s) so existing tests stay byte-identical to
    the v0.2.x supervisor behavior. The new invariant-4 adaptive
    cadence is exercised by tests that override this explicitly via
    monkeypatch + SupervisorConfig() args.
    """
    monkeypatch.setenv("FLEET_COORD_POLL_INTERVAL_S", "0")
    monkeypatch.setenv("FLEET_COORD_POLL_BASE_INTERVAL_S", "0")
