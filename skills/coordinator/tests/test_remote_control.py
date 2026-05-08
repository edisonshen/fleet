"""Tests for skills/coordinator/remote_control.py.

Issue #56: every fresh coordinator must self-inject /remote-control.
The bootstrap function is idempotent + fail-soft. Coverage:

  - daemon spawn: pgrep already-running (no-op via the pgrep guard
    inside the bash payload), pgrep not-running (Popen fires).
  - inbox seed: writes ~/.fleet/inbox/<coord_id>.md once, body
    includes the literal `/remote-control` slash command, atomic
    rename leaves no .tmp leftover, fail-soft on missing inbox dir.
  - bootstrap: marker-file gates re-runs; success path writes the
    marker; inbox failure preserves the lack of marker so next tick
    retries; missing project / coord_id returns False silently.
  - integration with loop._tick_locked: the bootstrap call fires
    once on the first tick, no-op on subsequent ticks (marker
    present), errors don't bubble.
"""
from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import pytest

import remote_control


@pytest.fixture
def fleet_home(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Per-test fleet home. The bootstrap module reads FLEET_HOME via
    its own _resolve_home; tests pass fleet_home= explicitly to be
    deterministic."""
    home = tmp_path / "fleet"
    home.mkdir()
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


# ---------- spawn_daemon_if_needed ----------


class _FakePopen:
    """Capture subprocess.Popen invocations without launching a real
    process. The skill never blocks on the daemon, so we don't need a
    return value beyond the call record."""

    def __init__(self) -> None:
        self.calls: list[tuple[list[str], dict[str, Any]]] = []
        self.raise_on_call: Exception | None = None

    def __call__(self, args: list[str], **kwargs: Any) -> Any:
        if self.raise_on_call is not None:
            raise self.raise_on_call
        self.calls.append((list(args), dict(kwargs)))
        # Return a sentinel; the skill ignores it.
        return object()


@pytest.fixture
def fake_popen(monkeypatch: pytest.MonkeyPatch) -> _FakePopen:
    fake = _FakePopen()
    monkeypatch.setattr(remote_control.subprocess, "Popen", fake)
    return fake


class TestSpawnDaemonIfNeeded:
    def test_invokes_bash_with_pgrep_guard(self, fake_popen: _FakePopen) -> None:
        """The shell command must include the pgrep guard so an already-
        running daemon is not double-launched. We don't actually run
        the bash; we assert the args carry the guard form."""
        ok = remote_control.spawn_daemon_if_needed()
        assert ok is True
        assert len(fake_popen.calls) == 1
        args, _kwargs = fake_popen.calls[0]
        assert args[0] == "bash"
        assert args[1] == "-c"
        cmd = args[2]
        # pgrep guard verbatim — same form as handoff.FIRST_ACTION's
        # bash block, so fresh-coord and handoff-replacement converge
        # on a single daemon process.
        assert 'pgrep -f "claude remote-control"' in cmd
        assert "nohup claude remote-control" in cmd
        # Coord-specific log path so operator can distinguish
        # bootstrap vs handoff invocations.
        assert "/tmp/claude-rc-coord.log" in cmd
        # Background `&` keeps the daemon alive past the coord tick.
        assert cmd.rstrip().endswith("&")

    def test_detached_session(self, fake_popen: _FakePopen) -> None:
        """start_new_session=True detaches from the coord's process
        group, so the daemon survives tick exit + parent SIGHUP."""
        remote_control.spawn_daemon_if_needed()
        _args, kwargs = fake_popen.calls[0]
        assert kwargs.get("start_new_session") is True

    def test_devnull_streams(self, fake_popen: _FakePopen) -> None:
        """stdout/stderr must be DEVNULL so any pgrep noise doesn't
        leak onto the coord's stdout (which the skill emits a JSON
        summary on)."""
        import subprocess as _sp
        remote_control.spawn_daemon_if_needed()
        _args, kwargs = fake_popen.calls[0]
        assert kwargs.get("stdin") == _sp.DEVNULL
        assert kwargs.get("stdout") == _sp.DEVNULL
        assert kwargs.get("stderr") == _sp.DEVNULL

    def test_filenotfound_returns_false_no_raise(
        self, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """bash missing on the host: log + return False, never raise.
        This is the documented fail-soft posture — coord proceeds, the
        operator just doesn't get auto-pairing."""
        fake = _FakePopen()
        fake.raise_on_call = FileNotFoundError("bash")
        monkeypatch.setattr(remote_control.subprocess, "Popen", fake)
        ok = remote_control.spawn_daemon_if_needed()
        assert ok is False

    def test_generic_exception_returns_false_no_raise(
        self, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Any other Popen failure (e.g. resource limits) is logged
        + returns False. Same fail-soft posture."""
        fake = _FakePopen()
        fake.raise_on_call = OSError("EMFILE")
        monkeypatch.setattr(remote_control.subprocess, "Popen", fake)
        ok = remote_control.spawn_daemon_if_needed()
        assert ok is False


# ---------- seed_inbox ----------


class TestSeedInbox:
    def test_writes_inbox_file_at_canonical_path(self, fleet_home: Path) -> None:
        """Path matches fleet-guard inbox.read_pending —
        ~/.fleet/inbox/<coord_id>.md."""
        ok = remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        assert ok is True
        target = fleet_home / "inbox" / "abcd1234.md"
        assert target.exists()

    def test_body_contains_remote_control_slash_command(
        self, fleet_home: Path,
    ) -> None:
        """The agent must recognize and execute the slash command on
        delivery. Body MUST include the literal `/remote-control`."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        body = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        assert "/remote-control" in body

    def test_no_tmp_leftover_on_success(self, fleet_home: Path) -> None:
        """Atomic rename leaves no .tmp file in the inbox dir."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        for entry in (fleet_home / "inbox").iterdir():
            assert ".tmp" not in entry.name, (
                f"leftover tmp file: {entry.name}"
            )

    def test_idempotent_on_re_call(self, fleet_home: Path) -> None:
        """Second call overwrites cleanly — same path, body unchanged.
        (Bootstrap caller gates re-call via the marker file; this test
        proves the seed itself doesn't corrupt on re-call.)"""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        first = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        ok = remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        assert ok is True
        second = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        assert first == second

    def test_body_ends_with_newline(self, fleet_home: Path) -> None:
        """fleet-guard's deliver() rstrips trailing whitespace on
        delivery, but the on-disk file should end with a newline so
        future scanners + manual inspection see a clean file."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        body = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        assert body.endswith("\n")

    def test_creates_inbox_dir_if_missing(
        self, tmp_path: Path,
    ) -> None:
        """seed_inbox must create the inbox dir if absent — fresh
        FLEET_HOME mounts have nothing under it."""
        empty_home = tmp_path / "empty"
        empty_home.mkdir()
        # No inbox/ subdir yet.
        ok = remote_control.seed_inbox("abcd1234", fleet_home=empty_home)
        assert ok is True
        assert (empty_home / "inbox" / "abcd1234.md").exists()

    def test_failsoft_on_unwritable_inbox(
        self, fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If mkstemp raises (e.g. ENOSPC, permissions), seed_inbox
        returns False without raising. Coord tick proceeds."""

        def boom(*_args: Any, **_kwargs: Any) -> Any:
            raise OSError("ENOSPC")

        monkeypatch.setattr(remote_control.tempfile, "mkstemp", boom)
        ok = remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        assert ok is False


# ---------- bootstrap_remote_control ----------


class TestBootstrap:
    def test_first_call_writes_marker_and_inbox(
        self, fleet_home: Path, fake_popen: _FakePopen,
    ) -> None:
        """End-to-end happy path: marker absent → daemon attempted +
        inbox seeded + marker written. Returns True."""
        ok = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert ok is True
        assert (fleet_home / "inbox" / "abcd1234.md").exists()
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert marker.exists()
        # Daemon was attempted exactly once.
        assert len(fake_popen.calls) == 1

    def test_marker_present_returns_false_noop(
        self, fleet_home: Path, fake_popen: _FakePopen,
    ) -> None:
        """Second tick: marker exists → no-op. Daemon NOT re-attempted,
        inbox NOT re-written. Returns False (didn't bootstrap)."""
        # Pre-create the marker.
        proj_dir = fleet_home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / ".remote-control-bootstrap-abcd1234").touch()
        # Sanity: inbox file does NOT exist before the call.
        inbox_target = fleet_home / "inbox" / "abcd1234.md"
        assert not inbox_target.exists()
        ok = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert ok is False
        # No daemon spawn attempt.
        assert fake_popen.calls == []
        # No inbox write.
        assert not inbox_target.exists()

    def test_per_coord_marker_isolation(
        self, fleet_home: Path, fake_popen: _FakePopen,
    ) -> None:
        """A new coord_id (post-handoff replacement) re-bootstraps,
        even when an old coord's marker exists. The marker filename
        carries the coord_id suffix so old + new are independent."""
        proj_dir = fleet_home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        # Old coord's marker.
        (proj_dir / ".remote-control-bootstrap-aaaa1111").touch()
        # New coord boots:
        ok = remote_control.bootstrap_remote_control(
            "myproj", "bbbb2222", fleet_home=fleet_home,
        )
        assert ok is True
        # New coord's marker exists.
        assert (proj_dir / ".remote-control-bootstrap-bbbb2222").exists()
        # Old coord's marker still exists (we don't touch it).
        assert (proj_dir / ".remote-control-bootstrap-aaaa1111").exists()
        # Daemon attempt fired for the new coord.
        assert len(fake_popen.calls) == 1

    def test_missing_project_returns_false(
        self, fleet_home: Path, fake_popen: _FakePopen,
    ) -> None:
        """Empty project arg: silent noop, returns False. No marker,
        no inbox, no daemon attempt."""
        ok = remote_control.bootstrap_remote_control(
            "", "abcd1234", fleet_home=fleet_home,
        )
        assert ok is False
        assert fake_popen.calls == []

    def test_missing_coord_id_returns_false(
        self, fleet_home: Path, fake_popen: _FakePopen,
    ) -> None:
        """Empty coord_id arg: silent noop, returns False."""
        ok = remote_control.bootstrap_remote_control(
            "myproj", "", fleet_home=fleet_home,
        )
        assert ok is False
        assert fake_popen.calls == []

    def test_inbox_failure_preserves_no_marker_for_retry(
        self, fleet_home: Path, fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If seed_inbox fails, the marker is NOT written — next tick
        retries the bootstrap. Daemon spawn is still attempted (pgrep
        guards re-launch)."""
        # Force seed_inbox to return False without touching disk.
        monkeypatch.setattr(
            remote_control, "seed_inbox", lambda *_args, **_kwargs: False,
        )
        ok = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert ok is False
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert not marker.exists()
        # Daemon was still attempted — pgrep guards re-launch on retry.
        assert len(fake_popen.calls) == 1

    def test_marker_write_failure_returns_false(
        self, fleet_home: Path, fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If the marker tmp+rename fails after a successful inbox seed,
        bootstrap returns False so the next tick retries. The inbox
        seed already landed; on retry seed_inbox is idempotent."""
        original_mkstemp = remote_control.tempfile.mkstemp
        seen_calls: list[str] = []

        def selective_mkstemp(*args: Any, **kwargs: Any) -> Any:
            # First mkstemp call is for the inbox; allow it. Second is
            # for the marker; fail it.
            seen_calls.append(kwargs.get("dir", ""))
            if len(seen_calls) == 1:
                return original_mkstemp(*args, **kwargs)
            raise OSError("EROFS")

        monkeypatch.setattr(
            remote_control.tempfile, "mkstemp", selective_mkstemp,
        )
        ok = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert ok is False
        # Inbox file landed.
        assert (fleet_home / "inbox" / "abcd1234.md").exists()
        # Marker did not.
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert not marker.exists()

    def test_daemon_spawn_failure_still_writes_marker(
        self, fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If the bash spawn raises (no bash on host), the inbox seed
        still goes out and the marker still gets written. The agent
        runs /remote-control on its next turn; if the daemon is missing
        the slash command will surface the failure to the operator."""

        def fail(*_args: Any, **_kwargs: Any) -> Any:
            raise FileNotFoundError("bash")

        monkeypatch.setattr(remote_control.subprocess, "Popen", fail)
        ok = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert ok is True
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert marker.exists()
        assert (fleet_home / "inbox" / "abcd1234.md").exists()


# ---------- integration with loop._tick_locked ----------


class TestLoopIntegration:
    """The fresh-coord auto-inject is wired into loop._tick_locked.
    Verify the call fires once on the first tick + is a no-op on
    subsequent ticks (marker gating). Errors must not bubble out.
    """

    def test_first_tick_fires_bootstrap(
        self,
        tmp_path: Path,
        fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Run loop.tick() against an empty project. Bootstrap fires
        on the first tick (Popen invoked + inbox file present + marker
        file present)."""
        import loop
        home = tmp_path / "fleet"
        home.mkdir()
        proj_dir = home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / "tasks.md").write_text(
            "# fleet-tasks/v1\n", encoding="utf-8",
        )
        # Stub out fleet-cli runs so the tick doesn't fork real binaries.
        monkeypatch.setattr(
            loop.dispatch_mod, "fetch_standards", lambda *a, **kw: "",
        )
        monkeypatch.setattr(
            loop.dispatch_mod, "fetch_learnings", lambda *a, **kw: "",
        )
        result = loop.tick(
            "myproj",
            coord_id="abcd1234",
            cwd=str(tmp_path),
            fleet_home=str(home),
            now_unix=1735689600.0,
        )
        # Bootstrap fired: Popen invoked once.
        assert len(fake_popen.calls) == 1
        # Inbox seeded.
        assert (home / "inbox" / "abcd1234.md").exists()
        # Marker written.
        assert (
            proj_dir / ".remote-control-bootstrap-abcd1234"
        ).exists()
        # No errors bubbled into the result.
        assert "remote-control bootstrap" not in " ".join(result.errors)

    def test_second_tick_skips_bootstrap(
        self,
        tmp_path: Path,
        fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Marker is durable across ticks: second tick is a no-op."""
        import loop
        home = tmp_path / "fleet"
        home.mkdir()
        proj_dir = home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / "tasks.md").write_text(
            "# fleet-tasks/v1\n", encoding="utf-8",
        )
        # Pre-stamp the marker as if Tick 1 completed.
        (proj_dir / ".remote-control-bootstrap-abcd1234").touch()
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
        # Marker present → bootstrap skipped → Popen NOT invoked.
        assert fake_popen.calls == []
        # Inbox file NOT created on this tick.
        assert not (home / "inbox" / "abcd1234.md").exists()

    def test_bootstrap_exception_recorded_not_raised(
        self,
        tmp_path: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """A bug inside remote_control that raises must surface in
        TickResult.errors but never abort the tick. fleet-guard
        discipline."""
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

        def boom(*_args: Any, **_kwargs: Any) -> bool:
            raise RuntimeError("synthetic bug")

        monkeypatch.setattr(
            loop.remote_control, "bootstrap_remote_control", boom,
        )
        # Should not raise.
        result = loop.tick(
            "myproj",
            coord_id="abcd1234",
            cwd=str(tmp_path),
            fleet_home=str(home),
            now_unix=1735689600.0,
        )
        assert any(
            "remote-control bootstrap" in e for e in result.errors
        ), result.errors

    def test_no_coord_id_skips_bootstrap_silently(
        self,
        tmp_path: Path,
        fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """coord_id="" (e.g. running under tests with FLEET_AGENT_ID
        unset): bootstrap returns False, no inbox or marker. Tick
        continues normally."""
        import loop
        home = tmp_path / "fleet"
        home.mkdir()
        proj_dir = home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / "tasks.md").write_text(
            "# fleet-tasks/v1\n", encoding="utf-8",
        )
        # Ensure FLEET_AGENT_ID is unset (loop.tick reads it as the
        # coord_id default).
        monkeypatch.delenv("FLEET_AGENT_ID", raising=False)
        monkeypatch.setattr(
            loop.dispatch_mod, "fetch_standards", lambda *a, **kw: "",
        )
        monkeypatch.setattr(
            loop.dispatch_mod, "fetch_learnings", lambda *a, **kw: "",
        )
        result = loop.tick(
            "myproj",
            cwd=str(tmp_path),
            fleet_home=str(home),
            now_unix=1735689600.0,
        )
        # No inbox / no marker.
        assert not list((home / "inbox").iterdir()) if (home / "inbox").exists() else True
        # No bubble error.
        assert not any(
            "remote-control bootstrap" in e for e in result.errors
        )
        # And the bootstrap function was effectively a no-op (Popen not
        # invoked because bootstrap_remote_control returned early).
        # Note: spawn_daemon_if_needed runs only inside bootstrap, and
        # the early-return in bootstrap_remote_control short-circuits
        # before reaching spawn_daemon_if_needed.
        assert fake_popen.calls == []
