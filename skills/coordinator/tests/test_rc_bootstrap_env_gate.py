"""Regression tests for FLEET_RC_BOOTSTRAP_DISABLED gate.

rc-listener-bootstrap-sk-3e98 (P0 follow-up to commit 69d4f79):

The Go-side gate at `cmd/fleet/dispatch.go:injectRemoteControlFlag`
prevents the `--remote-control` flag from being added to coord-spawn
claude argv when `FLEET_RC_BOOTSTRAP_DISABLED` is set. But the Python
coordinator skill has a parallel bootstrap path —
`spawn_daemon_if_needed()` shells out to `nohup claude remote-control ...`
on every coord first-tick, and is exercised by `loop.tick()` tests
without a Popen mock. Without a Python-side gate, every `pytest skills/`
run forks a real `claude remote-control` listener that registers with
the operator's Claude Code service and pushes a mobile notification.

This file pins the symmetry with the Go side:

  - When FLEET_RC_BOOTSTRAP_DISABLED is set to any non-empty value,
    spawn_daemon_if_needed() returns early without invoking
    subprocess.Popen. bootstrap_remote_control still seeds the inbox
    + writes the marker (those are non-spawning, deterministic side
    effects). Production behaviour (env unset) is preserved.

  - The session-level autouse fixture in conftest.py sets the env var
    before any test runs. The `enable_rc_bootstrap_for_test(monkeypatch)`
    helper opts back in for the legacy `_FakePopen`-mocked tests that
    assert Popen is invoked.
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import pytest

import remote_control


def enable_rc_bootstrap_for_test(monkeypatch: pytest.MonkeyPatch) -> None:
    """Clear FLEET_RC_BOOTSTRAP_DISABLED for one test so the bootstrap
    path runs end-to-end (subprocess.Popen invoked through the gate).
    Mirror of Go-side cmd/fleet/rc_bootstrap_env_test.go's
    enableRCBootstrapForTest helper. Tests that assert the shape of the
    bash bootstrap command MUST call this so the gate doesn't short-
    circuit; the subprocess.Popen they patch is the _FakePopen that
    captures the call without launching a real listener.

    The session-scoped autouse fixture in conftest sets the env to "1"
    for every test by default; this helper inverts that per-test.
    """
    monkeypatch.delenv("FLEET_RC_BOOTSTRAP_DISABLED", raising=False)


class _RecordingPopen:
    """Record subprocess.Popen invocations without launching a real
    process. Distinct from test_remote_control._FakePopen so the gate
    tests stay self-contained.

    Exposes `bash_bootstrap_calls` which filters to only the bash
    bootstrap-emit invocations (the ones that fork `claude remote-
    control`). Other Popen sites in loop.tick — e.g. `fleet claims
    acquire-prompt` for the inbox lock — are NOT the listener-spawn
    bug we're gating; they're benign internal subprocess calls."""

    def __init__(self) -> None:
        self.calls: list[tuple[list[str], dict[str, Any]]] = []

    def __call__(self, args: list[str], **kwargs: Any) -> Any:
        self.calls.append((list(args), dict(kwargs)))

        # Stub object for callers that expect a Popen-like return.
        # `fleet claims acquire-prompt` is used as a context manager
        # (`with subprocess.Popen(...) as proc:`), so the stub must
        # support __enter__/__exit__ + provide a `communicate` method
        # plus a `returncode` attribute that the caller inspects.
        class _StubProc:
            returncode = 1  # non-zero so callers treat the lock as failed
            stdout = None
            stderr = None

            def __enter__(self_inner):
                return self_inner

            def __exit__(self_inner, *_exc):
                return False

            def communicate(self_inner, *_a, **_kw):
                return ("", "")

            def wait(self_inner, *_a, **_kw):
                return 1

            def kill(self_inner):
                return None

            def poll(self_inner):
                return 1

            def terminate(self_inner):
                return None

        return _StubProc()

    @property
    def bash_bootstrap_calls(
        self,
    ) -> list[tuple[list[str], dict[str, Any]]]:
        """Subset of `calls` that are bash bootstrap invocations
        (argv == ['bash', '-c', '<...nohup claude remote-control...>']).
        This is the load-bearing gate metric: zero here = zero
        listeners spawned by the test run."""
        out = []
        for args, kwargs in self.calls:
            if (
                len(args) >= 3
                and args[0] == "bash"
                and args[1] == "-c"
                and "nohup claude remote-control" in args[2]
            ):
                out.append((args, kwargs))
        return out


@pytest.fixture
def recording_popen(monkeypatch: pytest.MonkeyPatch) -> _RecordingPopen:
    fake = _RecordingPopen()
    monkeypatch.setattr(remote_control.subprocess, "Popen", fake)
    return fake


@pytest.fixture
def fleet_home(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    home.mkdir()
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


class TestSpawnDaemonGate:
    """spawn_daemon_if_needed() is the load-bearing exec path. When the
    env gate is set, it must not invoke subprocess.Popen at all — that's
    the line between 'safe pytest run' and 'operator's mobile pings'.
    """

    def test_env_set_returns_true_without_popen(
        self, recording_popen: _RecordingPopen,
    ) -> None:
        """The autouse conftest sets FLEET_RC_BOOTSTRAP_DISABLED=1 for
        the whole session. spawn_daemon_if_needed() must surface this
        as a no-op success (return True, NO subprocess.Popen call).

        Return True (not False) because: (a) callers like
        bootstrap_remote_control treat False as "spawn failed, log it"
        and we don't want spurious noise in BOOTSTRAP_LOG every tick;
        (b) the production-equivalent path returns True when the
        daemon spawn shell command runs without raising — under the
        gate we're skipping the shell call entirely but the contract
        from the caller's perspective is the same (idempotent no-op).
        """
        # Sanity: the session autouse fixture set the env.
        assert os.environ.get("FLEET_RC_BOOTSTRAP_DISABLED") == "1", (
            "session autouse fixture must set FLEET_RC_BOOTSTRAP_DISABLED=1"
        )
        ok = remote_control.spawn_daemon_if_needed()
        assert ok is True, (
            "gated spawn_daemon_if_needed must return True (no-op success)"
        )
        # spawn_daemon_if_needed has exactly one subprocess.Popen site
        # (the bash bootstrap), so direct comparison is fine here.
        assert recording_popen.calls == [], (
            "gated spawn_daemon_if_needed MUST NOT invoke subprocess.Popen; "
            f"got {len(recording_popen.calls)} Popen call(s)"
        )

    def test_env_unset_invokes_fleet_rc_up(
        self,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """v0.12: with the env cleared, the production code path shells
        out to `fleet rc up <project> --idempotent` via subprocess.run.
        Pins the new Go-controller-owns-spawn contract (S1)."""
        enable_rc_bootstrap_for_test(monkeypatch)
        calls: list[list[str]] = []

        def _fake_run(args, **kwargs):
            calls.append(list(args))
            return type("R", (), {"returncode": 0})()

        monkeypatch.setattr(remote_control.subprocess, "run", _fake_run)
        ok = remote_control.spawn_daemon_if_needed("demo")
        assert ok is True
        assert calls == [
            ["fleet", "rc", "up", "demo", "--respawn-only", "--idempotent"],
        ]

    @pytest.mark.parametrize("value", ["1", "true", "yes", "anything"])
    def test_any_nonempty_value_disables(
        self,
        monkeypatch: pytest.MonkeyPatch,
        value: str,
    ) -> None:
        """Match Go-side semantics: env var set to ANY non-empty value
        disables the bootstrap. cmd/fleet/dispatch.go gates on
        `os.Getenv("FLEET_RC_BOOTSTRAP_DISABLED") != ""`."""
        monkeypatch.setenv("FLEET_RC_BOOTSTRAP_DISABLED", value)
        calls: list[list[str]] = []

        def _fake_run(args, **kwargs):
            calls.append(list(args))
            return type("R", (), {"returncode": 0})()

        monkeypatch.setattr(remote_control.subprocess, "run", _fake_run)
        ok = remote_control.spawn_daemon_if_needed("demo")
        assert ok is True
        assert calls == [], (
            f"value {value!r} must disable the bootstrap; got {calls}"
        )

    def test_empty_string_enables_production_path(
        self,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Edge case: env var EXPLICITLY set to "" is treated as unset
        → production path runs."""
        monkeypatch.setenv("FLEET_RC_BOOTSTRAP_DISABLED", "")
        calls: list[list[str]] = []

        def _fake_run(args, **kwargs):
            calls.append(list(args))
            return type("R", (), {"returncode": 0})()

        monkeypatch.setattr(remote_control.subprocess, "run", _fake_run)
        ok = remote_control.spawn_daemon_if_needed("demo")
        assert ok is True
        assert calls == [
            ["fleet", "rc", "up", "demo", "--respawn-only", "--idempotent"],
        ]


class TestBootstrapRemoteControlGate:
    """bootstrap_remote_control composes spawn_daemon_if_needed +
    seed_inbox + marker write. With the gate set, the spawn is a no-op
    but the seed + marker still land — agents that DO eventually
    receive the `/remote-control` inbox message just see a daemon
    that wasn't auto-spawned (operator can start it manually). The
    point of the gate is to stop pytest runs from forking listeners,
    not to break production code paths under sandboxed tests.
    """

    def test_gated_bootstrap_still_seeds_inbox_and_marker(
        self,
        fleet_home: Path,
        recording_popen: _RecordingPopen,
    ) -> None:
        """End-to-end: with FLEET_RC_BOOTSTRAP_DISABLED set (session
        autouse), bootstrap returns STATUS_OK, inbox file is written,
        marker file is written, BUT the bash bootstrap Popen is NEVER
        called. This is the steady-state for `pytest skills/` runs.

        (bootstrap_remote_control may invoke `fleet claims acquire-
        prompt` via Popen for the inbox lock — that's benign internal
        IPC, not the listener-spawn bug. We filter to
        `bash_bootstrap_calls` to assert on the load-bearing path.)
        """
        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_OK
        assert (fleet_home / "inbox" / "abcd1234.md").exists()
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert marker.exists()
        # The critical assertion: no listener was spawned.
        assert recording_popen.bash_bootstrap_calls == [], (
            "gated bootstrap_remote_control MUST NOT invoke the bash "
            "bootstrap Popen; got "
            f"{len(recording_popen.bash_bootstrap_calls)} bootstrap call(s)"
        )

    def test_ungated_bootstrap_invokes_fleet_rc_up(
        self,
        fleet_home: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """v0.12: with env unset, bootstrap_remote_control shells out
        to `fleet rc up <project> --idempotent` (S1 contract — the Go
        controller owns spawn; Python is a thin caller)."""
        enable_rc_bootstrap_for_test(monkeypatch)
        run_calls: list[list[str]] = []

        def _fake_run(args, **kwargs):
            run_calls.append(list(args))
            return type("R", (), {"returncode": 0})()

        monkeypatch.setattr(remote_control.subprocess, "run", _fake_run)
        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_OK
        # The bootstrap chain calls spawn_daemon_if_needed("myproj"),
        # which shells out exactly once. Other subprocess.run sites
        # (fleet claims) may also fire — filter to the rc-up call.
        rc_calls = [c for c in run_calls if c[:3] == ["fleet", "rc", "up"]]
        assert len(rc_calls) == 1, (
            f"bootstrap must invoke `fleet rc up` exactly once; got {rc_calls}"
        )


class TestBootstrapHonorsNotEnabled:
    """codex round-3 + round-5 P2 (combined contract): when `fleet rc up
    --respawn-only --idempotent` returns exit 10 (not_enabled), the
    outer bootstrap MUST:
      - log STATUS_NOT_ENABLED once,
      - NOT seed the inbox (no daemon = misleading prompt),
      - WRITE the per-coord marker so future ticks skip the function
        entirely (codex round-5 P2: avoid per-tick spam).

    Recovery is operator-driven: `fleet rc up <project>` then
    `fleet rc connect <project>` to attach the running coord. The
    next-handoff coord gets a fresh coord_id → fresh marker → fresh
    evaluation.
    """

    def test_not_enabled_skips_inbox_writes_marker(
        self,
        fleet_home: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        enable_rc_bootstrap_for_test(monkeypatch)

        def _fake_run(args, **kwargs):
            # exit 10 = outcome=not_enabled (Go side rc.OutcomeNotEnabled
            # mapping). spawn_daemon_if_needed treats any non-zero as
            # False per S1 contract.
            return type("R", (), {"returncode": 10})()

        monkeypatch.setattr(remote_control.subprocess, "run", _fake_run)

        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_NOT_ENABLED, (
            f"bootstrap must surface not_enabled when fleet rc up "
            f"returns exit 10; got {status!r}"
        )
        # codex R5: the per-coord marker IS written so future ticks
        # skip — quiet steady-state instead of every-tick spam.
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert marker.exists(), (
            "per-coord bootstrap marker MUST be written on "
            "not_enabled so subsequent ticks no-op (avoid log spam)"
        )
        # But no inbox seed — there's no daemon for /remote-control
        # to attach to. Misleading prompt was the original silo bug.
        assert not (fleet_home / "inbox" / "abcd1234.md").exists(), (
            "inbox seed must NOT fire when RC is not enabled"
        )


class TestLoopTickDoesNotSpawnUnderGate:
    """The original P0 bug: pytest skills/ runs caused listener spawns.
    loop.tick() is the entry the test suite hammers — pin that it
    doesn't fork a real listener under the gate. This is the
    canary test: if it ever spawns Popen with the env set, the gate
    has regressed and operator notifications resume.
    """

    def test_loop_tick_does_not_invoke_popen_with_gate(
        self,
        tmp_path: Path,
        recording_popen: _RecordingPopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Run one loop.tick() against a fresh fleet_home; with the
        autouse session env-var fixture in effect, Popen MUST NOT be
        called for the daemon bootstrap. Other Popen sites in loop.tick
        are stubbed out via dispatch_mod fakes so we observe only the
        bootstrap path."""
        import loop
        home = tmp_path / "fleet"
        home.mkdir()
        proj_dir = home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / "tasks.md").write_text(
            "# fleet-tasks/v1\n", encoding="utf-8",
        )
        monkeypatch.setattr(
            loop.dispatch_mod, "fetch_standards", lambda *a, **kw: "",
        )
        monkeypatch.setattr(
            loop.dispatch_mod, "fetch_learnings", lambda *a, **kw: "",
        )
        loop.tick(
            "myproj",
            coord_id="abcd1234",
            cwd=str(tmp_path),
            fleet_home=str(home),
            now_unix=1735689600.0,
        )
        # CRITICAL: zero bash bootstrap Popen invocations. Other Popen
        # sites in loop.tick (fleet claims acquire-prompt, etc.) are
        # benign internal IPC — only the bash-bootstrap path forks
        # the real `claude remote-control` listener.
        assert recording_popen.bash_bootstrap_calls == [], (
            "loop.tick under FLEET_RC_BOOTSTRAP_DISABLED MUST NOT spawn "
            "a remote-control daemon; got "
            f"{len(recording_popen.bash_bootstrap_calls)} bootstrap "
            f"call(s): {recording_popen.bash_bootstrap_calls}"
        )
