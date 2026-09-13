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
import shutil
import subprocess
import sys
from dataclasses import dataclass
from typing import TYPE_CHECKING, Callable

import pytest

_SKILL_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _SKILL_DIR not in sys.path:
    sys.path.insert(0, _SKILL_DIR)

if TYPE_CHECKING:
    from loop import TickResult

_REPO_ROOT = os.path.dirname(os.path.dirname(_SKILL_DIR))
_SCENARIO_SH = os.path.join(_REPO_ROOT, "scripts", "scenario.sh")


@dataclass(frozen=True)
class FleetSandbox:
    """A `scripts/scenario.sh up` sandbox: the BUILT fleet binary, an isolated
    FLEET_HOME + FLEET_TMUX_SOCKET, and the fake `claude` first on PATH.

    bin      path of the fleet binary (FLEET_DEV_BIN)
    home     the sandbox FLEET_HOME (/tmp/fleet-test-pytest-XXXXXX)
    project  the project tag `up` registered (FLEET_SCENARIO_PROJECT)
    env      the exports `up` printed, merged over os.environ
    run(cmd) run <cmd> (argv list) inside the sandbox env, capture output
    tick()   one REAL `loop.tick` against the sandbox (cap=1, cwd=<home>/repo)
    """

    bin: str
    home: str
    project: str
    env: dict[str, str]
    run: Callable[..., subprocess.CompletedProcess[str]]
    tick: Callable[..., "TickResult"]


def _scenario(args: list[str], env: dict[str, str]) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["bash", _SCENARIO_SH, *args],
        cwd=_REPO_ROOT,
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )


@pytest.fixture(scope="session")
def fleet_sandbox():
    """Session-scoped `scripts/scenario.sh up` … `down`.

    Builds ./cmd/fleet once per pytest session (or reuses $FLEET_DEV_BIN when
    set), stands the sandbox up under /tmp/fleet-test-pytest-XXXXXX with its
    own tmux socket, and tears it all down — sessions, socket, home — after
    the last test that used it. Skips when tmux or go is unavailable, the
    same way the Go e2e lanes do.
    """
    for tool in ("tmux", "go", "bash"):
        if shutil.which(tool) is None:
            pytest.skip(f"{tool} not installed")

    up_env = dict(os.environ)
    up_env.setdefault("FLEET_SCENARIO_PROJECT", "pytest")
    up = _scenario(["up", "--slug", "pytest"], up_env)
    if up.returncode != 0:
        pytest.fail(f"scenario.sh up failed (exit {up.returncode}):\n{up.stderr}")

    env = dict(up_env)
    for line in up.stdout.splitlines():
        if not line.startswith("export "):
            continue
        key, _, value = line[len("export ") :].partition("=")
        if key == "PATH":
            value = value.replace("$PATH", env.get("PATH", ""))
        env[key] = value
    home = env["FLEET_HOME"]
    fleet_bin = env["FLEET_DEV_BIN"]
    project = env["FLEET_SCENARIO_PROJECT"]

    def run(cmd: list[str], **kw) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            cmd, cwd=home, env=env, capture_output=True, text=True, check=False, **kw
        )

    # The exports `up` printed, i.e. what a shell would have after eval.
    exports = {k: v for k, v in env.items() if os.environ.get(k) != v}

    def tick(**kw) -> "TickResult":
        import loop

        # loop.tick spawns `fleet dispatch` children which read FLEET_HOME /
        # FLEET_TMUX_SOCKET / PATH from the process env — point them at the
        # sandbox for the duration of the tick only.
        saved = {k: os.environ.get(k) for k in exports}
        os.environ.update(exports)
        try:
            return loop.tick(
                project,
                coord_id=kw.pop("coord_id", ""),
                cwd=kw.pop("cwd", os.path.join(home, "repo")),
                fleet_home=home,
                fleet_bin=fleet_bin,
                cap=kw.pop("cap", 1),
                **kw,
            )
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    try:
        yield FleetSandbox(
            bin=fleet_bin, home=home, project=project, env=env, run=run, tick=tick
        )
    finally:
        down = _scenario(["down"], env)
        assert down.returncode == 0, f"scenario.sh down failed:\n{down.stderr}"
        assert not os.path.exists(home), f"scenario.sh down left {home}"


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
def _stub_lease_check(monkeypatch):
    """Default: the parent-lease ownership proof returns "owner".

    DESIGN-handoff-drain-storm-leak PR4 made `loop.tick` prove parent-lease
    ownership by shelling out to `fleet lease-check`. The vast majority of
    coordinator tests exercise UNRELATED
    tick behavior and don't model a coord-run lease supervisor parent — they
    must not shell out for the proof, nor pay for a real epoch read. This
    autouse fixture substitutes the seam with a stub that returns "owner"
    (proven), restoring the pre-PR4 "tick proceeds" behavior.

    The dedicated lease-fence test (test_lease_fence.py) overrides
    `loop._lease_check_fn` itself to return "fenced"; a later monkeypatch in
    the test body wins over this autouse default.
    """
    import loop

    def _proven(project, *, home, fleet_bin="fleet"):
        return "owner"

    monkeypatch.setattr(loop, "_lease_check_fn", _proven)


@pytest.fixture(autouse=True)
def _stub_coord_lease_identity(monkeypatch):
    """Default: the coord-identity read (the deleted coord-spawn marker's
    replacement, D3) names nobody.

    `loop._classify_lock_busy` gate 5 shells out to `fleet coord-owner --json`
    to decide "never self-exit when the lease names me". The vast majority of
    coordinator tests never reach that gate and must not shell out to the real
    binary (the CI trap + it perturbs timing-sensitive tests). This autouse
    fixture substitutes the seam with a stub that names nobody ("", "") so gate
    5 falls through to the other conservatism gates — the pre-D3 "marker absent"
    behavior. Tests exercising the lease-names-us skip override this in their
    own body (a later monkeypatch wins).
    """
    import loop

    def _no_lease_identity(project, *, home, fleet_bin="fleet"):
        return "", "", False, False

    monkeypatch.setattr(loop, "_coord_lease_identity_fn", _no_lease_identity)


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
