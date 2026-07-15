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
import json
from pathlib import Path

argv_log = os.environ.get("REVIEW_SLOT_ARGV_LOG")
if argv_log:
    with open(argv_log, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(sys.argv) + "\\n")

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


def write_output(tmp_path: Path, text: str, name: str) -> Path:
    path = tmp_path / name
    path.write_text(text, encoding="utf-8")
    return path


def run_slot(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    args: list[str],
    stdout_text: str,
    *,
    stderr_text: str = "",
    exit_code: int = 0,
    counter: Path | None = None,
    argv_log: Path | None = None,
) -> subprocess.CompletedProcess[str]:
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE", str(write_output(tmp_path, stdout_text, "stdout.txt"))
    )
    monkeypatch.setenv(
        "REVIEW_SLOT_STDERR_FILE", str(write_output(tmp_path, stderr_text, "stderr.txt"))
    )
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


def test_claude_clean_false_without_findings_retries_then_blocks(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps({"clean": False, "findings": []}),
            }
        ),
        counter=counter,
    )

    assert result.returncode == 3
    assert result.stdout == ""
    assert counter.read_text() == "3"
    assert "clean=false with no findings" in result.stderr


def test_claude_clean_false_with_nonblocking_finding_exits_clean(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps(
                    {"clean": False, "findings": [{"severity": "P2"}]}
                ),
            }
        ),
    )

    assert result.returncode == 0
    assert result.stdout == ""
    assert json.loads(result.stderr) == [{"severity": "P2"}]


def test_claude_clean_false_with_blocking_finding_exits_blocking(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps(
                    {"clean": False, "findings": [{"severity": "P1"}]}
                ),
            }
        ),
    )

    assert result.returncode == 1
    assert json.loads(result.stdout) == [{"severity": "P1"}]


def test_claude_non_git_task_context_is_prepended_to_prompt(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    argv_log = tmp_path / "argv.jsonl"

    result = run_slot(
        tmp_path,
        monkeypatch,
        [
            "--engine",
            "claude",
            "--model",
            "claude-opus-4-8",
            "--task-context",
            "Build X",
        ],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps({"clean": True, "findings": []}),
            }
        ),
        argv_log=argv_log,
    )

    assert result.returncode == 0
    argv = json.loads(argv_log.read_text().splitlines()[0])
    prompt = argv[-1]
    assert "Task context (what the worker was asked to build):\nBuild X" in prompt
    assert "Review the current working-tree changes in this project" in prompt
    assert "against the acceptance criteria above" in prompt


def test_claude_non_git_without_task_context_uses_generic_prompt(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    argv_log = tmp_path / "argv.jsonl"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps({"clean": True, "findings": []}),
            }
        ),
        argv_log=argv_log,
    )

    assert result.returncode == 0
    argv = json.loads(argv_log.read_text().splitlines()[0])
    prompt = argv[-1]
    assert "Run a raw structured review of the current working-tree changes" in prompt
    assert "Task context (what the worker was asked to build)" not in prompt


def test_codex_rate_limited_exits_skip_without_retry(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "",
        stderr_text="usage limit reached\n",
        exit_code=1,
        counter=counter,
    )

    assert result.returncode == 2
    assert result.stdout == "rate-limited\n"
    assert result.stderr == ""
    assert counter.read_text() == "1"


def test_codex_rate_limited_on_stdout_exits_skip_without_retry(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "usage limit reached\n",
        exit_code=0,
        counter=counter,
    )

    assert result.returncode == 2
    assert result.stdout == "rate-limited\n"
    assert result.stderr == ""
    assert counter.read_text() == "1"


def test_codex_unavailable_exits_skip_without_retry(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "",
        stderr_text="codex: command not found\n",
        exit_code=127,
        counter=counter,
    )

    assert result.returncode == 2
    assert result.stdout == "unavailable\n"
    assert result.stderr == ""
    assert counter.read_text() == "1"


def test_codex_parse_failure_without_skip_signal_retries_then_blocks(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "",
        stderr_text="ordinary parser failure\n",
        exit_code=1,
        counter=counter,
    )

    assert result.returncode == 3
    assert result.stdout == ""
    assert counter.read_text() == "3"
    assert "review slot blocked" in result.stderr


def test_codex_findings_win_over_rate_limit_skip_signal(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "[P0] data loss in reviewer gate\n",
        stderr_text="usage limit reached\n",
        exit_code=1,
    )

    assert result.returncode == 1
    findings = json.loads(result.stdout)
    assert [item["severity"] for item in findings] == ["P0"]
    assert result.stderr == "usage limit reached\n"


def test_codex_nonzero_stdout_without_findings_retries_then_blocks(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex"],
        "Loading config...\nerror: invalid base branch\n",
        stderr_text="",
        exit_code=1,
        counter=counter,
    )

    assert result.returncode == 3
    assert result.stdout == ""
    assert counter.read_text() == "3"
    assert "codex exited nonzero with no findings" in result.stderr


def test_codex_missing_ref_error_retries_then_blocks(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    counter = tmp_path / "counter.txt"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "codex", "--model", "gpt-5.5-codex", "--base", "origin/main"],
        "",
        stderr_text="fatal: invalid reference: origin/main ... not found\n",
        exit_code=1,
        counter=counter,
    )

    assert result.returncode == 3
    assert result.stdout == ""
    assert counter.read_text() == "3"
    assert "review slot blocked" in result.stderr


def test_claude_blocking_finding_preserves_details(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps(
                    {
                        "clean": False,
                        "findings": [
                            {
                                "severity": "p1",
                                "title": "Missing guard",
                                "body": "The mutation can race.",
                                "file": "skills/coordinator/review_slot.py",
                                "line": 42,
                            }
                        ],
                    }
                ),
            }
        ),
    )

    assert result.returncode == 1
    findings = json.loads(result.stdout)
    assert findings == [
        {
            "severity": "P1",
            "title": "Missing guard",
            "body": "The mutation can race.",
            "file": "skills/coordinator/review_slot.py",
            "line": 42,
        }
    ]


def test_claude_json_schema_is_passed_inline(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    argv_log = tmp_path / "claude.argv"

    result = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        json.dumps(
            {
                "type": "result",
                "subtype": "success",
                "session_id": "sess",
                "result": json.dumps({"clean": True, "findings": []}),
            }
        ),
        argv_log=argv_log,
    )

    assert result.returncode == 0
    argv = json.loads(argv_log.read_text())
    schema_value = argv[argv.index("--json-schema") + 1]
    schema = json.loads(schema_value)
    assert isinstance(schema, dict)
    assert schema["type"] == "object"
    assert "properties" in schema
    assert schema_value.lstrip().startswith("{")
    assert not schema_value.endswith(".json")


def test_claude_prompt_matches_git_or_non_git_mode(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    with_base_log = tmp_path / "claude-with-base.argv"
    without_base_log = tmp_path / "claude-without-base.argv"
    clean_stdout = json.dumps(
        {
            "type": "result",
            "subtype": "success",
            "session_id": "sess",
            "result": json.dumps({"clean": True, "findings": []}),
        }
    )

    with_base = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8", "--base", "origin/main"],
        clean_stdout,
        argv_log=with_base_log,
    )
    without_base = run_slot(
        tmp_path,
        monkeypatch,
        ["--engine", "claude", "--model", "claude-opus-4-8"],
        clean_stdout,
        argv_log=without_base_log,
    )

    assert with_base.returncode == 0
    assert without_base.returncode == 0
    with_base_argv = json.loads(with_base_log.read_text())
    without_base_argv = json.loads(without_base_log.read_text())
    assert with_base_argv[-1] == "/review the diff against origin/main"
    assert "/review" not in without_base_argv[-1]
    assert "the diff against" not in without_base_argv[-1]
    assert "working-tree" in without_base_argv[-1]
    assert "structured review" in without_base_argv[-1]
    assert "JSON schema" in without_base_argv[-1]
    assert '{"clean": bool, "findings":' in without_base_argv[-1]


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
