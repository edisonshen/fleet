"""Conversation decision capture (decisions.py): the Stop hook mines the
last operator↔coord exchange from the transcript and records it via
`fleet checkpoint decision`, once per operator turn, coord sessions only."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

import decisions
import main as fleet_main

AGENT = "c0ffee01"


@pytest.fixture(autouse=True)
def home(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    monkeypatch.setenv("FLEET_AGENT_ID", AGENT)
    monkeypatch.setenv("FLEET_ROLE", "coord")
    monkeypatch.delenv("FLEET_PROJECT", raising=False)
    (home / "agents").mkdir(parents=True)
    (home / "agents" / f"{AGENT}.json").write_text(
        json.dumps({"id": AGENT, "is_coord": True, "needs_input": False}))
    return home


@pytest.fixture
def recorded(monkeypatch: pytest.MonkeyPatch) -> list[str]:
    """Stub the CLI shell-out; collect the lines it would have written."""
    lines: list[str] = []

    def fake_record(line: str) -> bool:
        lines.append(line)
        return True

    monkeypatch.setattr(decisions, "record", fake_record)
    return lines


def _user(text, uuid="u1", **extra):
    return {"type": "user", "uuid": uuid,
            "message": {"role": "user", "content": text}, **extra}


def _assistant(*blocks, uuid="a1"):
    return {"type": "assistant", "uuid": uuid,
            "message": {"role": "assistant", "content": list(blocks),
                        "model": "claude-opus-4", "usage": {"input_tokens": 10}}}


def _tool_result():
    return {"type": "user", "uuid": "tr",
            "message": {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "x", "content": "ok"}]}}


def write_transcript(tmp_path: Path, *entries) -> str:
    p = tmp_path / "transcript.jsonl"
    p.write_text("\n".join(json.dumps(e) for e in entries) + "\n")
    return str(p)


def test_last_exchange_pairs_operator_with_final_reply(tmp_path):
    tp = write_transcript(
        tmp_path,
        _user("<command-name>/coordinator</command-name>", uuid="u0"),
        _user("defer the cache refactor, do auth first", uuid="u1"),
        _assistant({"type": "text", "text": "Looking at tasks.md"},
                   {"type": "tool_use", "id": "x", "name": "Bash", "input": {}}),
        _tool_result(),
        _assistant({"type": "text", "text": "Done: parked cache-1234, promoted auth-5678."}),
    )
    key, op, reply = decisions.last_exchange(tp)
    assert key == "u1"
    assert op == "defer the cache refactor, do auth first"
    assert reply == "Done: parked cache-1234, promoted auth-5678."


def test_last_exchange_ignores_injections_and_meta(tmp_path):
    tp = write_transcript(
        tmp_path,
        _user("real operator ask", uuid="u1"),
        _assistant({"type": "text", "text": "ack"}),
        _user("HANDOFF REQUESTED: context at 41%", uuid="u2"),
        _user("[FLEET] coord-guard blocked 1 attempt", uuid="u3"),
        _user("hidden", uuid="u4", isMeta=True),
        _assistant({"type": "text", "text": "MILESTONE: wrapped"}),
    )
    key, op, reply = decisions.last_exchange(tp)
    assert key == "u1"
    assert op == "real operator ask"
    # Reply text after the injections still belongs to the operator turn.
    assert reply == "MILESTONE: wrapped"


def test_last_exchange_strips_operator_inbox_prefix(tmp_path):
    tp = write_transcript(
        tmp_path,
        _user("[OPERATOR] stop rebasing PR #224", uuid="u1"),
        _assistant({"type": "text", "text": "Stopped."}),
    )
    assert decisions.last_exchange(tp) == ("u1", "stop rebasing PR #224", "Stopped.")


def test_last_exchange_none_without_operator_turn(tmp_path):
    tp = write_transcript(tmp_path, _tool_result(), _assistant({"type": "text", "text": "x"}))
    assert decisions.last_exchange(tp) is None
    assert decisions.last_exchange("") is None
    assert decisions.last_exchange(str(tmp_path / "missing.jsonl")) is None


def test_decision_line_flattens_and_truncates():
    line = decisions.decision_line("a\n\n  b " + "x" * 400, "ok\r\nfine")
    assert line.startswith("operator: a b xxx")
    assert " → coord: ok fine" in line
    assert "\n" not in line
    op_part = line.split(" → coord: ")[0]
    assert len(op_part) <= len("operator: ") + decisions.MAX_OPERATOR_CHARS
    assert decisions.decision_line("just a question", "") == "operator: just a question"


def test_capture_records_once_per_operator_turn(tmp_path, recorded):
    tp = write_transcript(
        tmp_path,
        _user("park cache-1234", uuid="u1"),
        _assistant({"type": "text", "text": "Parked cache-1234."}),
    )
    payload = {"hook_event_name": "Stop", "transcript_path": tp}
    assert decisions.capture(payload, AGENT) == "operator: park cache-1234 → coord: Parked cache-1234."
    assert decisions.cursor_path(AGENT).read_text() == "u1"
    # Same turn, second Stop (hook block + continue): no duplicate.
    assert decisions.capture(payload, AGENT) is None
    assert recorded == ["operator: park cache-1234 → coord: Parked cache-1234."]

    # A new operator turn records again.
    tp2 = write_transcript(
        tmp_path,
        _user("park cache-1234", uuid="u1"),
        _assistant({"type": "text", "text": "Parked cache-1234."}),
        _user("now abandon it", uuid="u2"),
        _assistant({"type": "text", "text": "Abandoned."}),
    )
    assert decisions.capture({"transcript_path": tp2}, AGENT) == "operator: now abandon it → coord: Abandoned."
    assert len(recorded) == 2


def test_capture_skips_non_coord_sessions(tmp_path, recorded, monkeypatch):
    tp = write_transcript(tmp_path, _user("hi"), _assistant({"type": "text", "text": "yo"}))
    monkeypatch.setenv("FLEET_ROLE", "worker")
    assert decisions.capture({"transcript_path": tp}, AGENT) is None
    # Role unset: the agent record says is_coord=true (see `home` fixture),
    # but capture requires the explicit spawn stamp — no record fallback.
    monkeypatch.delenv("FLEET_ROLE")
    assert decisions.capture({"transcript_path": tp}, AGENT) is None
    assert recorded == []
    assert not decisions.cursor_path(AGENT).exists()


def test_capture_keeps_cursor_when_record_fails(tmp_path, monkeypatch):
    monkeypatch.setattr(decisions, "record", lambda line: False)
    tp = write_transcript(tmp_path, _user("hi", uuid="u1"), _assistant({"type": "text", "text": "yo"}))
    assert decisions.capture({"transcript_path": tp}, AGENT) is None
    assert not decisions.cursor_path(AGENT).exists()


@pytest.mark.parametrize("text", [
    "use ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab for the push",
    "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
    "key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
    "AKIAIOSFODNN7EXAMPLE is the access key",
    "xoxb-1234567890-abcdefghij",
    "-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
    "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
    "Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
    "password=hunter2hunter2",
    'DB_PASSWORD: "s3cr3t-value"',
    "api_key = 0123456789abcdef",
    "clone https://user:p4ssw0rd@github.com/o/r.git",
])
def test_looks_secret_matches_credential_shapes(text):
    assert decisions.looks_secret(text)


@pytest.mark.parametrize("text", [
    "defer the cache refactor, do auth first",
    "merged 41c0cb9afee3f295201aa6339fe40f1edd17a1c4 into main",
    "the token budget is 200k; context at 41%",
    "rotate the API key for the staging account tomorrow",
    "password reset flow is out of scope",
    "PR #301 https://github.com/edisonshen/fleet/pull/301 is green",
    "",
])
def test_looks_secret_ignores_normal_coord_chat(text):
    assert not decisions.looks_secret(text)


def test_capture_skips_exchange_that_looks_like_a_secret(tmp_path, recorded, capsys):
    # Token is in the operator turn, past the 240-char truncation point:
    # the veto runs on the untruncated text.
    op = "please use this " + ("x" * 250) + " ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"
    tp = write_transcript(
        tmp_path,
        _user(op, uuid="u1"),
        _assistant({"type": "text", "text": "Set."}),
    )
    assert decisions.capture({"transcript_path": tp}, AGENT) is None
    assert recorded == []
    assert "looks like it contains a secret" in capsys.readouterr().err
    # Cursor advances so the same turn is not re-examined every Stop ...
    assert decisions.cursor_path(AGENT).read_text() == "u1"
    assert decisions.capture({"transcript_path": tp}, AGENT) is None
    # ... and the next clean turn records normally.
    tp2 = write_transcript(
        tmp_path,
        _user(op, uuid="u1"),
        _assistant({"type": "text", "text": "Set."}),
        _user("park cache-1234", uuid="u2"),
        _assistant({"type": "text", "text": "Parked."}),
    )
    assert decisions.capture({"transcript_path": tp2}, AGENT) == "operator: park cache-1234 → coord: Parked."

    # Secret in the COORD's reply vetoes too.
    tp3 = write_transcript(
        tmp_path,
        _user("what's in .env?", uuid="u3"),
        _assistant({"type": "text", "text": "STRIPE_KEY=sk_live_abcdefghijklmnopqrstuv"}),
    )
    assert decisions.capture({"transcript_path": tp3}, AGENT) is None
    assert recorded == ["operator: park cache-1234 → coord: Parked."]


def test_record_shells_checkpoint_decision_with_project(monkeypatch, tmp_path):
    fake_bin = tmp_path / "bin" / "fleet"
    fake_bin.parent.mkdir()
    fake_bin.write_text("#!/bin/sh\nexit 0\n")
    fake_bin.chmod(0o755)
    monkeypatch.setenv("FLEET_BIN", str(fake_bin))
    monkeypatch.setenv("FLEET_PROJECT", "myproj")
    calls: list[list[str]] = []

    class _Proc:
        returncode = 0
        stdout = ""
        stderr = ""

    def fake_run(cmd, **kw):
        calls.append(list(cmd))
        return _Proc()

    monkeypatch.setattr(decisions.subprocess, "run", fake_run)
    assert decisions.record("operator: x → coord: y") is True
    assert calls == [[str(fake_bin), "checkpoint", "decision", "--project", "myproj",
                      "operator: x → coord: y"]]


def test_stop_hook_runs_capture_before_handoff(tmp_path, monkeypatch, recorded):
    tp = write_transcript(
        tmp_path,
        _user("ship auth first", uuid="u1"),
        _assistant({"type": "text", "text": "Reprioritized: auth-5678 → P1."}),
    )
    payload = {"hook_event_name": "Stop", "transcript_path": tp}
    import io
    assert fleet_main.main(io.StringIO(json.dumps(payload))) == 0
    assert recorded == ["operator: ship auth first → coord: Reprioritized: auth-5678 → P1."]
