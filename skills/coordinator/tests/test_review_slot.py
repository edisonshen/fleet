from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest


SCRIPT = Path(__file__).resolve().parents[1] / "review_slot.py"


@pytest.fixture
def shim_bin(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for name in ("claude", "codex"):
        script = bin_dir / name
        script.write_text(
            """#!/usr/bin/env python3
from __future__ import annotations

import os
import sys
from pathlib import Path

argv_log = os.environ.get("REVIEW_SLOT_ARGV_LOG")
if argv_log:
    with open(argv_log, "a", encoding="utf-8") as fh:
        fh.write(" ".join(sys.argv) + "\\n")

counter = os.environ.get("REVIEW_SLOT_COUNTER")
if counter:
    path = Path(counter)
    count = int(path.read_text() or "0") if path.exists() else 0
    path.write_text(str(count + 1))

stdout_file = os.environ.get("REVIEW_SLOT_STDOUT_FILE")
if stdout_file:
    sys.stdout.write(Path(stdout_file).read_text())

stderr_file = os.environ.get("REVIEW_SLOT_STDERR_FILE")
if stderr_file:
    sys.stderr.write(Path(stderr_file).read_text())

sys.exit(int(os.environ.get("REVIEW_SLOT_EXIT_CODE", "0")))
""",
            encoding="utf-8",
        )
        script.chmod(0o755)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")
    return bin_dir


def write_output(tmp_path: Path, text: str) -> Path:
    path = tmp_path / "stdout.txt"
    path.write_text(text, encoding="utf-8")
    return path


def run_slot(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    args: list[str],
    stdout_text: str,
    *,
    exit_code: int = 0,
    counter: Path | None = None,
    argv_log: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    monkeypatch.setenv("REVIEW_SLOT_STDOUT_FILE", str(write_output(tmp_path, stdout_text)))
    monkeypatch.setenv("REVIEW_SLOT_EXIT_CODE", str(exit_code))
    if counter is not None:
        monkeypatch.setenv("REVIEW_SLOT_COUNTER", str(counter))
    else:
        monkeypatch.delenv("REVIEW_SLOT_COUNTER", raising=False)
    if argv_log is not None:
        monkeypatch.setenv("REVIEW_SLOT_ARGV_LOG", str(argv_log))
    else:
        monkeypatch.delenv("REVIEW_SLOT_ARGV_LOG", raising=False)

    return subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        capture_output=True,
        text=True,
    )


@pytest.mark.parametrize(
    ("name", "args", "stdout_text", "want_code", "want_stdout_severities"),
    [
        (
            "claude_clean_envelope",
            ["--engine", "claude", "--model", "claude-opus-4-8"],
            json.dumps(
                {
                    "type": "result",
                    "subtype": "success",
                    "session_id": "sess",
                    "result": json.dumps({"clean": True, "findings": []}),
                }
            ),
            0,
            [],
        ),
        (
            "codex_blocking_findings",
            ["--engine", "codex", "--model", "gpt-5.5-codex"],
            "notes\n[P0] broken invariant\ncontext\n[P1] missing guard\n",
            1,
            ["P0", "P1"],
        ),
        (
            "codex_nonblocking_findings",
            ["--engine", "codex", "--model", "gpt-5.5-codex"],
            "[P2] polish this\n[P3] backlog that\n",
            0,
            [],
        ),
    ],
)
def test_review_slot_table(
    shim_bin: Path,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    name: str,
    args: list[str],
    stdout_text: str,
    want_code: int,
    want_stdout_severities: list[str],
) -> None:
    result = run_slot(tmp_path, monkeypatch, args, stdout_text)

    assert result.returncode == want_code, name
    if want_stdout_severities:
        findings = json.loads(result.stdout)
        assert [item["severity"] for item in findings] == want_stdout_severities
    else:
        assert result.stdout == ""


def test_claude_parse_failure_retries_twice_then_blocks(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        "not json",
        counter=counter,
    )

    assert result.returncode == 3
    assert counter.read_text() == "3"
    assert result.stdout == ""
    assert result.stderr.strip()


def test_codex_base_flag_is_threaded_only_when_set(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    with_base_log = tmp_path / "with-base.argv"
    without_base_log = tmp_path / "without-base.argv"

    with_base = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex", "--base", "origin/main"],
        "",
        argv_log=with_base_log,
    )
    without_base = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "",
        argv_log=without_base_log,
    )

    assert with_base.returncode == 0
    assert without_base.returncode == 0
    with_base_argv = with_base_log.read_text()
    without_base_argv = without_base_log.read_text()
    assert "--base" in with_base_argv
    assert "origin/main" in with_base_argv
    assert "--base" not in without_base_argv
