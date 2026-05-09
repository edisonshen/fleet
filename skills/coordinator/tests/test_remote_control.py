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
    def test_invokes_bash_with_narrow_pgrep_guard(
        self, fake_popen: _FakePopen,
    ) -> None:
        """REGRESSION PIN — fix/remote-control-coord-injection (P0):

        The pgrep guard must match ONLY the fleet-coord-prefixed daemon,
        NOT all `claude remote-control` processes. The previous broad
        guard (`pgrep -f "claude remote-control"`) silently skipped the
        fleet-coord daemon launch whenever a fleet-handoff daemon was
        running, breaking mobile claude.ai pairing for every coord on
        the operator's host. This test pins the narrow form symmetric
        with cmd/fleet/dispatch.go's remoteControlDaemonRunning regex.
        """
        ok = remote_control.spawn_daemon_if_needed()
        assert ok is True
        assert len(fake_popen.calls) == 1
        args, _kwargs = fake_popen.calls[0]
        assert args[0] == "bash"
        assert args[1] == "-c"
        cmd = args[2]
        # The guard must reference pgrep -f explicitly.
        assert "pgrep -f " in cmd, (
            f"missing pgrep guard in spawn shell command: {cmd!r}"
        )
        # Anchor on `^claude` so the bootstrap's own bash subprocess
        # (whose argv[0] is "bash") never matches.
        assert "^claude " in cmd, (
            "pgrep pattern must anchor with ^claude to exclude the "
            f"bash bootstrap subprocess; got: {cmd!r}"
        )
        # Must reference the fleet-coord prefix flag-name + value,
        # NOT just `claude remote-control` — the previous bug was a
        # broad match that also caught fleet-handoff daemons.
        assert "--remote-control-session-name-prefix fleet-coord" in cmd, (
            "pgrep pattern must match the fleet-coord prefix flag, "
            f"not just `claude remote-control`; got: {cmd!r}"
        )
        # The unquoted token (live daemon's argv has shell-quotes
        # stripped before exec). A hypothetical longer prefix like
        # `fleet-coordX` should NOT match.
        assert "fleet-coord( |$)" in cmd or "fleet-coord$" in cmd, (
            "pgrep pattern must terminate the fleet-coord token at a "
            f"word boundary so longer prefixes don't false-positive: {cmd!r}"
        )
        assert "nohup claude remote-control" in cmd
        # Coord-specific log path so operator can distinguish
        # bootstrap vs handoff invocations.
        assert "/tmp/claude-rc-coord.log" in cmd
        # Background `&` keeps the daemon alive past the coord tick.
        assert cmd.rstrip().endswith("&")

    def test_pgrep_pattern_does_not_match_fleet_handoff_daemon(
        self, fake_popen: _FakePopen,
    ) -> None:
        """REGRESSION PIN — operator-reported P0:

        The previous broad pgrep pattern matched a fleet-handoff daemon
        and silently skipped the fleet-coord daemon launch. We extract
        the pattern from the bash command and compile it as a regex,
        then verify representative argv strings.

        This is the critical test: a fleet-handoff daemon must NOT
        cause the coord launch to be skipped. The regex must match a
        live fleet-coord daemon (so we don't double-launch it) AND must
        NOT match a fleet-handoff daemon.
        """
        import re as _re

        remote_control.spawn_daemon_if_needed()
        cmd = fake_popen.calls[0][0][2]
        # Extract the pgrep pattern. The shell form is:
        #   pgrep -f '<pattern>' >/dev/null 2>&1 || ...
        # We pull the single-quoted pattern out.
        m = _re.search(r"pgrep -f '([^']+)'", cmd)
        assert m is not None, (
            f"could not locate pgrep -f '<pattern>' in shell cmd: {cmd!r}"
        )
        pattern = m.group(1)
        compiled = _re.compile(pattern)

        cases = [
            (
                "live fleet-coord daemon — must MATCH (avoid double-launch)",
                "claude remote-control --remote-control-session-name-prefix fleet-coord",
                True,
            ),
            (
                "live fleet-coord daemon with trailing args — must MATCH",
                "claude remote-control --remote-control-session-name-prefix fleet-coord --some-other-flag",
                True,
            ),
            (
                "live fleet-handoff daemon — must NOT match (the bug)",
                "claude remote-control --remote-control-session-name-prefix fleet-handoff",
                False,
            ),
            (
                "transient bash bootstrap subprocess — argv[0] is bash, must NOT match",
                'bash -c pgrep -f \'^claude remote-control --remote-control-session-name-prefix fleet-coord( |$)\' >/dev/null 2>&1 || nohup claude remote-control --remote-control-session-name-prefix "fleet-coord"',
                False,
            ),
            (
                "spurious process mentioning fleet-coord in a log path — must NOT match",
                "tail -f /tmp/fleet-coord.log",
                False,
            ),
            (
                "longer prefix fleet-coordX — must NOT match (word-boundary)",
                "claude remote-control --remote-control-session-name-prefix fleet-coordX",
                False,
            ),
        ]
        for name, cmdline, want in cases:
            got = bool(compiled.search(cmdline))
            assert got == want, (
                f"{name}: pattern {pattern!r} against {cmdline!r}: "
                f"got match={got}, want {want}"
            )

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

    def test_body_still_includes_remote_control_literal(
        self, fleet_home: Path,
    ) -> None:
        """The body must still contain the literal `/remote-control`
        substring so operators searching their inbox / docs / tmux
        scrollback can find references to the slash command. Issue #69
        reframed the body from imperative to notification, but the
        literal must remain for discoverability."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        body = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        assert "/remote-control" in body

    def test_body_is_not_imperative(self, fleet_home: Path) -> None:
        """Regression test for issue #69: the inbox body must NOT be
        phrased as an imperative the agent can interpret as a Skill
        invocation. The previous wording ("Run the slash command
        `/remote-control` ...") caused Claude to call Skill(remote-
        control) and error with "remote-control is a UI command, not a
        skill". Reject any leading imperative verb that an LLM would
        read as "do this now"."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        body = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        ).strip()
        # First word must not be an imperative verb directed at the
        # agent. fleet-guard's deliver() prepends "[OPERATOR] " on the
        # delivered line, but the on-disk body itself starts the
        # sentence — that's what the agent reads + acts on.
        first_word = body.split(None, 1)[0].rstrip(":,.").lower()
        forbidden_leads = {"run", "execute", "invoke", "please", "call"}
        assert first_word not in forbidden_leads, (
            f"inbox body starts with imperative-style verb {first_word!r}; "
            "issue #69 requires status-notification framing"
        )
        # Defense in depth: the exact phrase that broke production must
        # never reappear verbatim.
        assert "Run the slash command" not in body

    def test_body_mentions_daemon_started(self, fleet_home: Path) -> None:
        """Regression test for issue #69: the new body must give the
        agent real situational context — that the remote-control
        daemon has been started for this session — rather than
        ordering it to do something. Asserting "daemon" + a started/
        running keyword keeps the change locked in even if future
        wording tweaks happen."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        body = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        ).lower()
        assert "daemon" in body
        # At least one keyword indicating the daemon is up — protects
        # against a future edit dropping the situational framing.
        assert any(kw in body for kw in ("started", "running", "active")), (
            "inbox body must indicate the daemon is started/running/active"
        )

    def test_no_tmp_leftover_on_success(self, fleet_home: Path) -> None:
        """Atomic rename leaves no .tmp file in the inbox dir."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        for entry in (fleet_home / "inbox").iterdir():
            assert ".tmp" not in entry.name, (
                f"leftover tmp file: {entry.name}"
            )

    def test_skip_if_inbox_already_exists(self, fleet_home: Path) -> None:
        """Skip-if-exists posture: don't clobber an existing inbox file
        (operator may have queued a message between dispatch and the
        first tick). Second call returns False, leaves the file
        untouched. The bootstrap caller withholds the marker on this
        False so the next tick retries — by which time fleet-guard's
        Stop hook has delivered + archived the existing message."""
        remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        first = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        ok = remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        assert ok is False
        # File body untouched.
        second = (fleet_home / "inbox" / "abcd1234.md").read_text(
            encoding="utf-8",
        )
        assert first == second

    def test_skip_if_operator_queued_message(self, fleet_home: Path) -> None:
        """Concrete scenario: operator queued an inbox message via
        `fleet message <coord_id>` before the coord's first tick. The
        seed must not clobber the operator's content."""
        inbox_dir = fleet_home / "inbox"
        inbox_dir.mkdir()
        (inbox_dir / "abcd1234.md").write_text(
            "operator note: pause work", encoding="utf-8",
        )
        ok = remote_control.seed_inbox("abcd1234", fleet_home=fleet_home)
        assert ok is False
        # Operator content untouched.
        body = (inbox_dir / "abcd1234.md").read_text(encoding="utf-8")
        assert body == "operator note: pause work"

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

    @pytest.mark.parametrize(
        "bad_id",
        [
            "",
            "ABCD1234",          # uppercase rejected
            "abcd123",           # 7 chars
            "abcd12345",         # 9 chars
            "../etc",            # path traversal
            "abcd/1234",         # slash
            "abcd\x001234",      # null byte
            "ghij1234",          # non-hex
        ],
    )
    def test_rejects_invalid_coord_id(
        self, fleet_home: Path, bad_id: str,
    ) -> None:
        """Defense-in-depth: a coord_id that doesn't match the canonical
        8-lowercase-hex shape is refused. In production FLEET_AGENT_ID
        always conforms (secrets.token_hex(4)); a non-conforming value
        is a bug or a hostile caller. Either way: refuse, don't traverse.
        """
        ok = remote_control.seed_inbox(bad_id, fleet_home=fleet_home)
        assert ok is False
        # No file written under inbox/ for the bad ID.
        if (fleet_home / "inbox").exists():
            assert not list((fleet_home / "inbox").iterdir())


# ---------- bootstrap_remote_control ----------


@pytest.fixture
def isolated_bootstrap_log(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
) -> Path:
    """Per-test bootstrap log path. Tests that exercise the failure
    paths can read this file to assert log lines were written; happy-
    path tests can assert the file is absent (quiet success)."""
    log = tmp_path / "fleet-bootstrap.log"
    monkeypatch.setattr(remote_control, "BOOTSTRAP_LOG", str(log))
    return log


class TestBootstrap:
    def test_first_call_writes_marker_and_inbox(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path,
    ) -> None:
        """End-to-end happy path: marker absent → daemon attempted +
        inbox seeded + marker written. Returns STATUS_OK. No log line
        — the OK path is silent (quiet success per BOOTSTRAP_LOG
        contract)."""
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
        # Daemon was attempted exactly once.
        assert len(fake_popen.calls) == 1
        # Quiet success: no bootstrap log entry written.
        assert not isolated_bootstrap_log.exists(), (
            "happy path should be silent; got log content: "
            + (isolated_bootstrap_log.read_text() if isolated_bootstrap_log.exists() else "")
        )

    def test_marker_present_returns_skipped_marker(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path,
    ) -> None:
        """Second tick: marker exists → no-op. Daemon NOT re-attempted,
        inbox NOT re-written. Returns STATUS_SKIPPED_MARKER. Quiet —
        steady-state path."""
        # Pre-create the marker.
        proj_dir = fleet_home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / ".remote-control-bootstrap-abcd1234").touch()
        # Sanity: inbox file does NOT exist before the call.
        inbox_target = fleet_home / "inbox" / "abcd1234.md"
        assert not inbox_target.exists()
        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_SKIPPED_MARKER
        # No daemon spawn attempt.
        assert fake_popen.calls == []
        # No inbox write.
        assert not inbox_target.exists()
        # Quiet steady-state: no bootstrap log entry written.
        assert not isolated_bootstrap_log.exists()

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
        status = remote_control.bootstrap_remote_control(
            "myproj", "bbbb2222", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_OK
        # New coord's marker exists.
        assert (proj_dir / ".remote-control-bootstrap-bbbb2222").exists()
        # Old coord's marker still exists (we don't touch it).
        assert (proj_dir / ".remote-control-bootstrap-aaaa1111").exists()
        # Daemon attempt fired for the new coord.
        assert len(fake_popen.calls) == 1

    def test_missing_project_returns_skipped_no_args(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path,
    ) -> None:
        """Empty project arg: silent noop, returns STATUS_SKIPPED_NO_ARGS.
        No marker, no inbox, no daemon attempt. Not logged — the no-args
        path fires every tick when FLEET_AGENT_ID is unset and would
        spam the log."""
        status = remote_control.bootstrap_remote_control(
            "", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_SKIPPED_NO_ARGS
        assert fake_popen.calls == []
        assert not isolated_bootstrap_log.exists()

    def test_missing_coord_id_returns_skipped_no_args(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path,
    ) -> None:
        """Empty coord_id arg: silent noop, returns STATUS_SKIPPED_NO_ARGS."""
        status = remote_control.bootstrap_remote_control(
            "myproj", "", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_SKIPPED_NO_ARGS
        assert fake_popen.calls == []
        assert not isolated_bootstrap_log.exists()

    @pytest.mark.parametrize(
        "bad_id",
        ["GHIJ1234", "abcd123", "../sneaky", "abcd/1234"],
    )
    def test_rejects_invalid_coord_id(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path, bad_id: str,
    ) -> None:
        """Bootstrap refuses non-canonical coord_ids: no daemon spawn,
        no inbox seed, no marker, no path traversal. Logs to
        BOOTSTRAP_LOG so the operator sees the rejection."""
        status = remote_control.bootstrap_remote_control(
            "myproj", bad_id, fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_SKIPPED_INVALID
        assert fake_popen.calls == []
        if (fleet_home / "inbox").exists():
            assert not list((fleet_home / "inbox").iterdir())
        # Defensive rejections are logged.
        assert isolated_bootstrap_log.exists(), (
            "invalid coord_id should write to BOOTSTRAP_LOG"
        )
        log = isolated_bootstrap_log.read_text(encoding="utf-8")
        assert "status=skipped_invalid" in log
        assert bad_id in log

    @pytest.mark.parametrize(
        "bad_project",
        ["..", ".", "../etc", "foo/bar", "foo\\bar"],
    )
    def test_rejects_invalid_project(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path, bad_project: str,
    ) -> None:
        """Bootstrap refuses project values that contain path separators
        or are dotted directory references — they'd traverse out of
        ~/.fleet/projects/ when used as a path component. Logged."""
        status = remote_control.bootstrap_remote_control(
            bad_project, "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_SKIPPED_INVALID
        assert fake_popen.calls == []
        # Logged for operator visibility.
        assert isolated_bootstrap_log.exists()
        assert "status=skipped_invalid" in isolated_bootstrap_log.read_text()

    def test_inbox_failure_preserves_no_marker_for_retry(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If seed_inbox fails, the marker is NOT written — next tick
        retries the bootstrap. Daemon spawn is still attempted (pgrep
        guards re-launch). Returns STATUS_FAILED_SEED + log line so the
        operator can grep BOOTSTRAP_LOG."""
        # Force seed_inbox to return False without touching disk.
        monkeypatch.setattr(
            remote_control, "seed_inbox", lambda *_args, **_kwargs: False,
        )
        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_FAILED_SEED
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert not marker.exists()
        # Daemon was still attempted — pgrep guards re-launch on retry.
        assert len(fake_popen.calls) == 1
        # Failure is observable.
        assert isolated_bootstrap_log.exists()
        log = isolated_bootstrap_log.read_text(encoding="utf-8")
        assert "status=failed_seed" in log
        assert "abcd1234" in log

    def test_marker_write_failure_returns_failed_marker(
        self, fleet_home: Path, fake_popen: _FakePopen,
        isolated_bootstrap_log: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If the marker tmp+rename fails after a successful inbox seed,
        bootstrap returns STATUS_FAILED_MARKER so the next tick retries.
        The inbox seed already landed; on retry seed_inbox is idempotent.
        Failure is logged to BOOTSTRAP_LOG so the operator sees why
        bootstrap can't make progress."""
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
        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_FAILED_MARKER
        # Inbox file landed.
        assert (fleet_home / "inbox" / "abcd1234.md").exists()
        # Marker did not.
        marker = (
            fleet_home / "projects" / "myproj"
            / ".remote-control-bootstrap-abcd1234"
        )
        assert not marker.exists()
        # Operator-facing log line is present.
        assert isolated_bootstrap_log.exists()
        log = isolated_bootstrap_log.read_text(encoding="utf-8")
        assert "status=failed_marker" in log

    def test_daemon_spawn_failure_still_writes_marker(
        self, fleet_home: Path, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If the bash spawn raises (no bash on host), the inbox seed
        still goes out and the marker still gets written. The agent
        runs /remote-control on its next turn; if the daemon is missing
        the slash command will surface the failure to the operator.
        Returns STATUS_OK because the bootstrap as a whole succeeded
        (daemon is fail-soft, not a hard prerequisite)."""

        def fail(*_args: Any, **_kwargs: Any) -> Any:
            raise FileNotFoundError("bash")

        monkeypatch.setattr(remote_control.subprocess, "Popen", fail)
        status = remote_control.bootstrap_remote_control(
            "myproj", "abcd1234", fleet_home=fleet_home,
        )
        assert status == remote_control.STATUS_OK
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
        unset): bootstrap returns STATUS_SKIPPED_NO_ARGS, no inbox or
        marker, no bubbled errors. Tick continues normally."""
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

    def test_failed_seed_status_bubbles_to_tick_errors(
        self,
        tmp_path: Path,
        fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """REGRESSION PIN — fix/remote-control-coord-injection (P0):

        When bootstrap returns STATUS_FAILED_SEED, the tick must surface
        it in TickResult.errors so the operator sees something is
        wrong. Previously the function returned False on every failure
        path silently and the operator had no visibility into "why
        didn't bootstrap fire for coord X?". The new contract:
          - STATUS_OK          → silent
          - STATUS_SKIPPED_*   → silent
          - STATUS_FAILED_*    → bubbled to TickResult.errors

        This pins the bubble: an inbox-write failure (a real I/O issue
        the operator should know about) reaches the tick errors list.
        """
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
        # Force seed_inbox to fail. Daemon Popen still fires (pgrep
        # branch), but the marker is withheld and the status comes
        # back as STATUS_FAILED_SEED.
        monkeypatch.setattr(
            loop.remote_control, "seed_inbox",
            lambda *_args, **_kwargs: False,
        )
        # Isolate the bootstrap log so we don't pollute /tmp.
        log_path = tmp_path / "fleet-bootstrap.log"
        monkeypatch.setattr(
            loop.remote_control, "BOOTSTRAP_LOG", str(log_path),
        )
        result = loop.tick(
            "myproj",
            coord_id="abcd1234",
            cwd=str(tmp_path),
            fleet_home=str(home),
            now_unix=1735689600.0,
        )
        # FAILED_SEED bubbled to the tick errors.
        assert any(
            "remote-control bootstrap" in e and "failed_seed" in e
            for e in result.errors
        ), result.errors
        # And the structured log line landed at BOOTSTRAP_LOG.
        assert log_path.exists()
        log = log_path.read_text(encoding="utf-8")
        assert "status=failed_seed" in log
        assert "coord=abcd1234" in log

    def test_skipped_marker_does_not_bubble_to_tick_errors(
        self,
        tmp_path: Path,
        fake_popen: _FakePopen,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Steady-state STATUS_SKIPPED_MARKER must not surface in
        TickResult.errors — that path fires every tick after the first
        successful bootstrap and would spam the error feed."""
        import loop
        home = tmp_path / "fleet"
        home.mkdir()
        proj_dir = home / "projects" / "myproj"
        proj_dir.mkdir(parents=True)
        (proj_dir / "tasks.md").write_text(
            "# fleet-tasks/v1\n", encoding="utf-8",
        )
        # Pre-stamp the marker as if a previous tick succeeded.
        (proj_dir / ".remote-control-bootstrap-abcd1234").touch()
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
        # Steady state: zero errors related to bootstrap.
        assert not any(
            "remote-control bootstrap" in e for e in result.errors
        ), result.errors
