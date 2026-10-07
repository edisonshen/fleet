"""Plan-review mode of review_slot.py: a TASK-PLAN Markdown doc is reviewed
(not a diff) and the result is persisted with the doc's sha256 so the Go
promote gate can tell a fresh review from a stale one."""
from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest

import plan_review
from test_review_slot import run_slot, shim_bin, write_output  # noqa: F401


def envelope(inner: dict) -> str:
    return json.dumps({"type": "result", "subtype": "success", "result": json.dumps(inner)})


@pytest.fixture
def plan_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> tuple[Path, Path]:
    home = tmp_path / "fleet-home"
    monkeypatch.setenv("FLEET_HOME", str(home))
    doc = tmp_path / "docs" / "TASK-PLAN-demo-0001.md"
    doc.parent.mkdir()
    doc.write_text("# Task plan\n\n## Steps\n1. do it\n", encoding="utf-8")
    return home, doc


def plan_args(doc: Path, *extra: str) -> list[str]:
    return ["--plan", str(doc), "--project", "demo", "--slug", "demo-0001", *extra]


def read_record(home: Path) -> dict:
    return json.loads((home / "projects" / "demo" / "plan-reviews" / "demo-0001.json").read_text())


def test_plan_clean_review_persists_record_with_doc_sha(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    home, doc = plan_env
    argv_log = tmp_path / "argv.jsonl"
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(doc, "--engine", "claude", "--model", "claude-opus-4-8"),
        envelope({"clean": True, "findings": []}),
        argv_log=argv_log,
    )
    assert result.returncode == 0, result.stderr
    rec = read_record(home)
    assert rec["doc_sha256"] == hashlib.sha256(doc.read_bytes()).hexdigest()
    assert rec["doc"] == str(doc)
    assert rec["clean"] is True and rec["exit"] == 0 and rec["findings"] == []
    assert rec["slots"]["slot"]["engine"] == "claude"

    argv = json.loads(argv_log.read_text().splitlines()[0])
    prompt = argv[-1]
    assert str(doc) in prompt and "PLAN review" in prompt
    schema = json.loads(argv[argv.index("--json-schema") + 1])
    item = schema["properties"]["findings"]["items"]
    assert item["properties"]["category"]["enum"] == ["bug", "flag", "security"]


def test_plan_blocking_findings_recorded_with_category(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    home, doc = plan_env
    inner = {"clean": False, "findings": [
        {"severity": "p1", "category": "Bug", "section": "Steps", "summary": "wrong file"},
        {"severity": "P3", "category": "flag", "section": "Steps", "summary": "nit"},
    ]}
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(doc, "--engine", "claude", "--model", "claude-opus-4-8"),
        envelope(inner),
    )
    assert result.returncode == 1
    rec = read_record(home)
    assert rec["clean"] is False and rec["exit"] == 1
    assert [(f["severity"], f["category"], f["slot"]) for f in rec["findings"]] == [
        ("P1", "bug", "slot"), ("P3", "flag", "slot"),
    ]


def test_plan_missing_category_is_unparseable_and_blocks(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    home, doc = plan_env
    inner = {"clean": False, "findings": [{"severity": "P2", "section": "x", "summary": "y"}]}
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(doc, "--engine", "claude", "--model", "claude-opus-4-8"),
        envelope(inner),
    )
    assert result.returncode == 3
    assert read_record(home)["clean"] is False


def test_plan_both_uses_codex_exec_and_records_both_slots(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    home, doc = plan_env
    argv_log = tmp_path / "argv.jsonl"
    schema_copy = tmp_path / "schema.json"
    codex_out = json.dumps({"clean": False, "findings": [
        {"severity": "P2", "category": "security", "section": "Auth", "summary": "token in log"},
    ]})
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX", str(write_output(tmp_path, codex_out, "codex.txt"))
    )
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(
            doc, "--both",
            "--alpha-engine", "codex", "--alpha-model", "gpt-5.5-codex",
            "--beta-engine", "claude", "--beta-model", "claude-opus-4-8",
        ),
        envelope({"clean": True, "findings": []}),
        argv_log=argv_log,
        schema_copy=schema_copy,
    )
    assert result.returncode == 0, result.stderr
    codex_argv = next(
        a for a in map(json.loads, argv_log.read_text().splitlines())
        if Path(a[0]).name == "codex"
    )
    assert codex_argv[1] == "exec" and "PLAN review" in codex_argv[-1]
    assert "category" in json.loads(schema_copy.read_text())["properties"]["findings"]["items"]["required"]

    rec = read_record(home)
    assert rec["clean"] is True
    assert set(rec["slots"]) == {"alpha", "beta"}
    assert [(f["category"], f["slot"]) for f in rec["findings"]] == [("security", "alpha")]


def test_plan_beta_skipped_records_blocked(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    home, doc = plan_env
    monkeypatch.setenv(
        "REVIEW_SLOT_STDOUT_FILE_CODEX",
        str(write_output(tmp_path, json.dumps({"clean": True, "findings": []}), "codex.txt")),
    )
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(
            doc, "--both",
            "--alpha-engine", "codex", "--alpha-model", "m",
            "--beta-engine", "claude", "--beta-model", "m",
        ),
        "usage limit reached",
    )
    assert result.returncode == 3
    rec = read_record(home)
    assert rec["clean"] is False
    assert rec["slots"]["beta"]["skip_reason"] == "rate-limited"


@pytest.mark.parametrize(
    ("args", "msg"),
    [
        (["--project", "demo"], "--plan requires --project and --slug"),
        (["--project", "demo", "--slug", "s", "--base", "origin/main"], "drop --base"),
        (["--project", "../x", "--slug", "s"], "invalid project"),
    ],
)
def test_plan_arg_validation(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env, args, msg
) -> None:
    _, doc = plan_env
    result = run_slot(
        tmp_path, monkeypatch,
        ["--plan", str(doc), *args, "--engine", "claude", "--model", "m"],
        "",
    )
    assert result.returncode == 2
    assert msg in result.stderr


def test_plan_missing_doc_rejected(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(tmp_path / "nope.md", "--engine", "claude", "--model", "m"),
        "",
    )
    assert result.returncode == 2 and "no such file" in result.stderr


def test_write_record_is_atomic_and_overwrites(tmp_path: Path) -> None:
    rec = plan_review.build_record(
        project="p", slug="s", doc="/d.md", sha="abc", exit_code=1,
        slots={"beta": {"engine": "claude", "exit": 1, "findings": [{"severity": "P1"}]}},
    )
    path = plan_review.write_record(rec, home=str(tmp_path))
    assert json.loads(path.read_text())["findings"] == [{"severity": "P1", "slot": "beta"}]
    rec2 = dict(rec, exit=0, clean=True, findings=[])
    plan_review.write_record(rec2, home=str(tmp_path))
    assert json.loads(path.read_text())["clean"] is True
    assert sorted(p.name for p in path.parent.iterdir()) == ["s.json"]


def test_plan_codex_exec_nonzero_exit_with_clean_json_is_not_clean(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    home, doc = plan_env
    result = run_slot(
        tmp_path, monkeypatch,
        plan_args(doc, "--engine", "codex", "--model", "gpt-5.5-codex"),
        json.dumps({"clean": True, "findings": []}),
        exit_code=1,
    )
    assert result.returncode == 3, result.stderr
    assert "codex exec exited 1" in result.stderr
    rec = read_record(home)
    assert rec["clean"] is False and rec["exit"] == 3


def test_plan_edited_during_review_is_not_recorded(
    shim_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, plan_env
) -> None:
    import threading

    home, doc = plan_env
    monkeypatch.setenv("REVIEW_SLOT_SLEEP_S", "1.5")
    editor = threading.Timer(
        0.5, lambda: doc.write_text("# Task plan\n\n## Steps\n1. rm -rf /\n", encoding="utf-8")
    )
    editor.start()
    try:
        result = run_slot(
            tmp_path, monkeypatch,
            plan_args(doc, "--engine", "claude", "--model", "claude-opus-4-8"),
            envelope({"clean": True, "findings": []}),
        )
    finally:
        editor.join()
    assert result.returncode == 3
    assert "changed during the review" in result.stderr
    assert not (home / "projects" / "demo" / "plan-reviews" / "demo-0001.json").exists()
