"""dispatch.py tests: prompt assembly + subprocess argv assertions.

The fleet binary is never invoked for real here; tests mock subprocess.run
to assert the exact argv we'd send and to drive return-code paths.
"""
from __future__ import annotations

import os
import subprocess
from unittest.mock import patch

import pytest

import dispatch
import parse


def _make_task(slug: str = "fix-thing-aaaa") -> parse.Task:
    return parse.Task(
        slug=slug,
        status="ready",
        priority="P1",
        spec="Fix the thing.",
        acceptance="Thing is fixed.",
        notes="",
    )


def test_build_worker_prompt_contains_required_sections() -> None:
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards\n\n## Testing\n- TDD only.\n",
        learnings_text="WHEN  AUTHOR  TAG  TASK  BODY\n2026-05-06T10:00:00Z  agent:abcdef01  testing  -  use t.TempDir\n",
    )
    # Required structural sections.
    assert f"You are a Fleet worker for task: {t.slug}" in out
    assert "Project: fleet" in out
    assert "## Task" in out
    assert "Fix the thing." in out
    assert "## Acceptance" in out
    assert "Thing is fixed." in out
    assert "## Standards (the bar — non-negotiable)" in out
    assert "TDD only." in out
    assert "## Relevant prior learnings" in out
    assert "use t.TempDir" in out
    # Workflow tail mentions every required phase.
    for phase in ("branch", "tdd-red", "tdd-green", "tdd-refactor",
                  "review-claude", "review-codex", "push", "done"):
        assert f"--phase {phase}" in out
    # Branch derivation.
    assert f"git checkout -b worker/{t.slug}" in out


def test_build_worker_prompt_omits_learnings_section_when_empty() -> None:
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    assert "## Relevant prior learnings" not in out


def test_build_worker_prompt_omits_learnings_section_on_no_learnings_message() -> None:
    """`fleet learnings list` emits 'no learnings (run ...)' on empty."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="WHEN  AUTHOR  TAG  TASK  BODY\nno learnings (run `fleet learnings add` to record one)",
    )
    assert "## Relevant prior learnings" not in out


def test_build_worker_prompt_truncates_long_learning_rows() -> None:
    t = _make_task()
    long_body = "x" * 5000
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text=f"WHEN  AUTHOR  TAG  TASK  BODY\n2026-05-06T10:00:00Z  agent:abcdef01  testing  -  {long_body}\n",
    )
    # Truncation marker present; raw 5000 chars are not.
    assert "…" in out
    assert "x" * 5000 not in out


def test_build_worker_prompt_oversized_raises() -> None:
    t = _make_task()
    huge_standards = "# Standards\n\n" + ("x" * (20 * 1024))
    with pytest.raises(dispatch.PromptTooLargeError):
        dispatch.build_worker_prompt(
            t, project="fleet",
            standards_md=huge_standards,
            learnings_text="",
        )


def test_build_worker_prompt_handles_empty_spec_and_acceptance() -> None:
    """Operator may add a task with no spec yet — prompt still renders."""
    t = parse.Task(slug="empty-spec-aaaa", status="ready", priority="P2")
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    assert "spec pending" in out
    assert "acceptance pending" in out


def test_build_worker_prompt_custom_branch_and_workers_dir() -> None:
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
        branch="feature/custom",
        workers_dir="/tmp/custom",
    )
    assert "Branch: feature/custom" in out
    assert "git checkout -b feature/custom" in out
    assert "State file:  /tmp/custom/state.json" in out


def test_build_worker_prompt_worktree_pre_created_skips_branch_create() -> None:
    """Codex iter-1 [P1] regress: in cap > 1 mode the coord ran
    `git worktree add -b <branch>` already; the prompt must NOT tell
    the worker to `git checkout -b <branch>` (it would fatal "branch
    already exists"). Verify mode."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
        branch="worker/alpha-1234",
        worktree_pre_created=True,
    )
    # The branch-create command MUST NOT appear under cap>1 mode.
    assert "git checkout -b worker/alpha-1234" not in out
    # The worker is told to verify the prepared worktree branch.
    assert "git rev-parse --abbrev-ref HEAD" in out
    assert "worker/alpha-1234" in out


def test_build_worker_prompt_default_keeps_branch_create() -> None:
    """Single-worker mode (the default) MUST emit the original
    `git checkout -b <branch>` step — worktree-mode is the override,
    not the new default. Byte-identical regression guard."""
    t = _make_task()
    out = dispatch.build_worker_prompt(
        t, project="fleet",
        standards_md="# Standards",
        learnings_text="",
    )
    assert "git checkout -b worker/" in out
    assert "git rev-parse --abbrev-ref HEAD" not in out


# ---------- dispatch_worker subprocess assertions ----------


def test_dispatch_worker_invokes_correct_argv() -> None:
    t = _make_task()
    fake = subprocess.CompletedProcess(
        args=[],
        returncode=0,
        stdout="agent abcdef01 dispatched on tmux session fleet-abcdef01\n",
        stderr="",
    )
    with patch.object(dispatch.subprocess, "run", return_value=fake) as m:
        result = dispatch.dispatch_worker(
            t, project="fleet", cwd="/repo",
            fleet_bin="/usr/local/bin/fleet",
            extra_args=["--mode", "execute"],
        )
    assert result.error == ""
    assert result.agent_id == "abcdef01"
    assert result.branch == f"worker/{t.slug}"
    args = m.call_args[0][0]
    assert args == [
        "/usr/local/bin/fleet", "dispatch", t.slug,
        "--project", "fleet", "--cwd", "/repo",
        "--mode", "execute",
    ]


def test_dispatch_worker_handles_nonzero_exit() -> None:
    t = _make_task()
    fake = subprocess.CompletedProcess(
        args=[], returncode=1, stdout="", stderr="error: project unknown\n",
    )
    with patch.object(dispatch.subprocess, "run", return_value=fake):
        result = dispatch.dispatch_worker(t, project="fleet", cwd="/repo")
    assert result.agent_id == ""
    assert "project unknown" in result.error


def test_dispatch_worker_handles_unparseable_stdout() -> None:
    t = _make_task()
    fake = subprocess.CompletedProcess(
        args=[], returncode=0, stdout="???\n", stderr="",
    )
    with patch.object(dispatch.subprocess, "run", return_value=fake):
        result = dispatch.dispatch_worker(t, project="fleet", cwd="/repo")
    assert result.agent_id == ""
    assert "could not parse agent ID" in result.error


def test_extract_agent_id_prefers_strict_form() -> None:
    """When stdout contains both `agent <id>` and a stray 8-hex token
    (e.g. embedded in a path), the keyword-anchored form wins.

    Regression: an unanchored fallback alone would pick whichever 8-hex
    appears first, including SHAs / tmux session paths.
    """
    text = "/tmp/cafef00d/setup.log\nagent abcdef01 dispatched\n"
    assert dispatch._extract_agent_id(text) == "abcdef01"


def test_extract_agent_id_loose_fallback_only_when_unique() -> None:
    """No `agent <id>` keyword + multiple 8-hex tokens → no extraction.

    Otherwise we'd pick the wrong token off ambiguous output.
    """
    text = "abcdef01 ... cafef00d ..."
    assert dispatch._extract_agent_id(text) == ""


def test_extract_agent_id_loose_fallback_when_unique() -> None:
    """No keyword + exactly one 8-hex → use it (drift-tolerant)."""
    assert dispatch._extract_agent_id("dispatched: abcdef01") == "abcdef01"


def test_dispatch_worker_handles_missing_binary() -> None:
    t = _make_task()
    with patch.object(
        dispatch.subprocess, "run",
        side_effect=FileNotFoundError("no such file"),
    ):
        result = dispatch.dispatch_worker(
            t, project="fleet", cwd="/repo", fleet_bin="/nope/fleet",
        )
    assert "fleet binary not found" in result.error


def test_dispatch_worker_handles_timeout() -> None:
    t = _make_task()
    with patch.object(
        dispatch.subprocess, "run",
        side_effect=subprocess.TimeoutExpired(cmd=["fleet"], timeout=1),
    ):
        result = dispatch.dispatch_worker(
            t, project="fleet", cwd="/repo", timeout_s=1,
        )
    assert "timed out" in result.error


# ---------- inbox stub ----------


def test_write_worker_inbox_atomic_and_under_fleet_home(tmp_path) -> None:
    target = dispatch.write_worker_inbox(
        "abcdef01", "hello worker\n", fleet_home=str(tmp_path),
    )
    expected = tmp_path / "inbox" / "abcdef01.md"
    assert target == str(expected)
    assert expected.read_text() == "hello worker\n"
    # No tmp files left behind.
    assert not [p for p in (tmp_path / "inbox").iterdir() if ".tmp." in p.name]


def test_write_worker_inbox_appends_trailing_newline(tmp_path) -> None:
    target = dispatch.write_worker_inbox(
        "abcdef01", "no newline", fleet_home=str(tmp_path),
    )
    assert open(target).read().endswith("\n")


def test_write_worker_inbox_rejects_invalid_id(tmp_path) -> None:
    with pytest.raises(ValueError):
        dispatch.write_worker_inbox(
            "not-hex", "x", fleet_home=str(tmp_path),
        )
    with pytest.raises(ValueError):
        dispatch.write_worker_inbox(
            "abcdef01extra", "x", fleet_home=str(tmp_path),
        )


def test_write_worker_inbox_uses_env_fleet_home(monkeypatch, tmp_path) -> None:
    monkeypatch.setenv("FLEET_HOME", str(tmp_path))
    target = dispatch.write_worker_inbox("abcdef01", "via env\n")
    assert target.startswith(str(tmp_path))
    assert os.path.exists(target)


# ---------- fetch_standards / fetch_learnings ----------


def test_fetch_standards_returns_stdout_on_zero_exit() -> None:
    fake = subprocess.CompletedProcess(
        args=[], returncode=0, stdout="# Standards\n\n## Testing\n", stderr="",
    )
    with patch.object(dispatch.subprocess, "run", return_value=fake):
        out = dispatch.fetch_standards("fleet")
    assert "# Standards" in out


def test_fetch_standards_returns_empty_on_error() -> None:
    fake = subprocess.CompletedProcess(args=[], returncode=2, stdout="", stderr="boom")
    with patch.object(dispatch.subprocess, "run", return_value=fake):
        assert dispatch.fetch_standards("fleet") == ""


def test_fetch_learnings_passes_limit_arg() -> None:
    fake = subprocess.CompletedProcess(args=[], returncode=0, stdout="", stderr="")
    with patch.object(dispatch.subprocess, "run", return_value=fake) as m:
        dispatch.fetch_learnings("fleet", limit=5)
    args = m.call_args[0][0]
    assert "--limit" in args
    assert "5" in args
    assert "--project" in args
    assert "fleet" in args


def test_fetch_learnings_swallows_missing_binary() -> None:
    with patch.object(
        dispatch.subprocess, "run", side_effect=FileNotFoundError("no fleet"),
    ):
        assert dispatch.fetch_learnings("fleet") == ""
