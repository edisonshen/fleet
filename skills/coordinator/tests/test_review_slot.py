from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

import review_slot


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

me = Path(sys.argv[0]).name.upper()

sleep_s = os.environ.get("REVIEW_SLOT_SLEEP_S")
if sleep_s:
    import time
    time.sleep(float(sleep_s))

stdout_file = os.environ.get(f"REVIEW_SLOT_STDOUT_FILE_{me}") or os.environ.get("REVIEW_SLOT_STDOUT_FILE")
if stdout_file:
    sys.stdout.write(Path(stdout_file).read_text())

stderr_file = os.environ.get("REVIEW_SLOT_STDERR_FILE")
if stderr_file:
    sys.stderr.write(Path(stderr_file).read_text())

sys.exit(int(os.environ.get(f"REVIEW_SLOT_EXIT_CODE_{me}") or os.environ.get("REVIEW_SLOT_EXIT_CODE", "0")))
""",
            encoding="utf-8",
        )
        script.chmod(0o755)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")
    return bin_dir


ATTEMPT_LOG_PREFIX = "[review_slot "


def strip_attempt_log(stderr: str) -> str:
    return "".join(
        line for line in stderr.splitlines(keepends=True)
        if not line.startswith(ATTEMPT_LOG_PREFIX)
    )


def assert_only_attempt_log(stderr: str) -> None:
    lines = stderr.splitlines()
    assert lines, "expected an attempt log line on stderr"
    assert all(line.startswith(ATTEMPT_LOG_PREFIX) for line in lines), stderr


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


def test_claude_parse_failure_retries_once_then_blocks(
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
    assert counter.read_text() == "2"
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
    assert counter.read_text() == "2"
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
    assert json.loads(strip_attempt_log(result.stderr)) == [{"severity": "P2"}]


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
    assert_only_attempt_log(result.stderr)
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
    assert_only_attempt_log(result.stderr)
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
    assert_only_attempt_log(result.stderr)
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
    assert counter.read_text() == "2"
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
    assert strip_attempt_log(result.stderr) == "usage limit reached\n"


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
    assert counter.read_text() == "2"
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
    assert counter.read_text() == "2"
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


CLEAN_ENVELOPE = json.dumps(
    {
        "type": "result",
        "subtype": "success",
        "session_id": "sess",
        "result": json.dumps({"clean": True, "findings": []}),
    }
)

BOTH_ARGS = [
    "--both",
    "--alpha-engine", "codex", "--alpha-model", "gpt-5.5-codex",
    "--beta-engine", "claude", "--beta-model", "claude-opus-4-8",
    "--base", "origin/main",
]


def test_both_runs_slots_concurrently_and_reports_clean(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    argv_log = tmp_path / "argv.jsonl"
    monkeypatch.setenv("REVIEW_SLOT_SLEEP_S", "1.5")
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX", str(write_output(tmp_path, "", "codex.txt"))
    )

    import time
    started = time.monotonic()
    result = run_slot(tmp_path, monkeypatch, BOTH_ARGS, CLEAN_ENVELOPE, argv_log=argv_log)
    elapsed = time.monotonic() - started

    assert result.returncode == 0, result.stderr
    assert elapsed < 2.8, f"slots did not overlap: {elapsed:.1f}s"
    report = json.loads(result.stdout)
    assert report == {
        "alpha": {"exit": 0, "findings": [], "skip_reason": None},
        "beta": {"exit": 0, "findings": [], "skip_reason": None},
    }
    argvs = [json.loads(line) for line in argv_log.read_text().splitlines()]
    names = sorted(Path(a[0]).name for a in argvs)
    assert names == ["claude", "codex"]
    assert "[review_slot alpha codex/gpt-5.5-codex]" in result.stderr
    assert "[review_slot beta claude/claude-opus-4-8]" in result.stderr


def test_both_alpha_blocking_beta_clean_exits_one(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX",
        str(write_output(tmp_path, "[P1] missing guard\n", "codex.txt")),
    )

    result = run_slot(tmp_path, monkeypatch, BOTH_ARGS, CLEAN_ENVELOPE)

    assert result.returncode == 1
    report = json.loads(result.stdout)
    assert report["alpha"]["exit"] == 1
    assert [f["severity"] for f in report["alpha"]["findings"]] == ["P1"]
    assert report["beta"]["exit"] == 0


def test_both_alpha_skipped_beta_clean_exits_zero(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX",
        str(write_output(tmp_path, "usage limit reached\n", "codex.txt")),
    )

    result = run_slot(tmp_path, monkeypatch, BOTH_ARGS, CLEAN_ENVELOPE)

    assert result.returncode == 0
    report = json.loads(result.stdout)
    assert report["alpha"] == {"exit": 2, "findings": [], "skip_reason": "rate-limited"}
    assert report["beta"]["exit"] == 0


def test_both_beta_blocked_wins_over_alpha_findings(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX",
        str(write_output(tmp_path, "[P0] data loss\n", "codex.txt")),
    )

    result = run_slot(tmp_path, monkeypatch, BOTH_ARGS, "not json")

    assert result.returncode == 3
    report = json.loads(result.stdout)
    assert report["alpha"]["exit"] == 1
    assert report["beta"]["exit"] == 3
    assert "review slot blocked after 2 attempts" in result.stderr


def test_effort_is_threaded_to_both_engines(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    argv_log = tmp_path / "argv.jsonl"
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX", str(write_output(tmp_path, "", "codex.txt"))
    )

    result = run_slot(
        tmp_path, monkeypatch, [*BOTH_ARGS, "--effort", "medium"], CLEAN_ENVELOPE, argv_log=argv_log
    )

    assert result.returncode == 0
    argvs = {Path(a[0]).name: a for a in map(json.loads, argv_log.read_text().splitlines())}
    assert argvs["claude"][argvs["claude"].index("--effort") + 1] == "medium"
    assert 'model_reasoning_effort="medium"' in argvs["codex"]


def test_both_requires_all_slot_flags(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    result = run_slot(tmp_path, monkeypatch, ["--both", "--alpha-engine", "codex"], "")
    assert result.returncode == 2
    assert "--both requires" in result.stderr


def test_both_rejects_non_claude_beta_engine(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    args = [
        "--both", "--alpha-engine", "codex", "--alpha-model", "gpt-5.5-codex",
        "--beta-engine", "codex", "--beta-model", "gpt-5.5-codex",
    ]
    result = run_slot(tmp_path, monkeypatch, args, "")
    assert result.returncode == 2
    assert "--beta-engine" in result.stderr


def test_finish_both_beta_skip_is_blocked() -> None:
    clean = review_slot.SlotOutcome(0, [], None, "")
    skipped = review_slot.SlotOutcome(2, [], "rate-limited", "")
    assert review_slot.finish_both(skipped, clean) == 0
    assert review_slot.finish_both(clean, skipped) == 3


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
