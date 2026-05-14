"""Tests for skills/fleet-guard/health.py.

The skill is non-authoritative for record creation — `fleet dispatch` writes
the initial record and the skill only mutates owned fields on every fire. The
tests below pin that contract: missing record returns False; unknown fields
are dropped; preserved fields survive a write; atomic semantics never expose a
torn JSON to a concurrent reader.

FLEET_HOME is redirected to a tmp_path for every test via the autouse fixture.
"""
from __future__ import annotations

import json
import os
from datetime import datetime, timezone
from pathlib import Path

import pytest

import health


@pytest.fixture(autouse=True)
def fleet_home_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Redirect ~/.fleet to a per-test tmpdir. Without this, tests would write
    to the operator's real ~/.fleet/agents/ — catastrophic in a watched
    directory."""
    home = tmp_path / "fleet"
    monkeypatch.setenv("FLEET_HOME", str(home))
    return home


@pytest.fixture
def transcript_with_usage(tmp_path: Path) -> Path:
    """A two-line transcript: one assistant turn with usage, one user line.
    Models claude-sonnet-4-6 (200k context). Total context tokens = 50k → 25%."""
    path = tmp_path / "transcript.jsonl"
    lines = [
        json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-sonnet-4-6",
                "usage": {
                    "input_tokens": 10_000,
                    "cache_read_input_tokens": 30_000,
                    "cache_creation_input_tokens": 10_000,
                    "output_tokens": 999,  # excluded from context_pct
                },
            },
        }),
        json.dumps({"type": "user", "content": "next prompt"}),
    ]
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return path


def _seed_record(home: Path, agent_id: str, **overrides) -> Path:
    """Write a minimal valid agent record matching agent.Record (Go) at the
    canonical path. Returns the path. Defaults match what `fleet dispatch`
    would produce, but tests can override individual fields."""
    record_dir = home / "agents"
    record_dir.mkdir(parents=True, exist_ok=True)
    base = {
        "schema_version": 1,
        "id": agent_id,
        "pid": 12345,
        "tmux_session": f"fleet-{agent_id}",
        "engine": "claude-code",
        "role": "executor",
        "mode": "execute",
        "task_id": "demo-task",
        "project": "myproject",
        "review_round": None,
        "context_pct": None,
        "context_source": "",
        "last_activity_ts": "2026-04-28T00:00:00Z",
        "blocked": False,
        "blocked_reason": None,
        "blocked_since": None,
        "needs_input": False,
        "inbox_pending": False,
        "handoff_type": None,
        "last_handoff_path": None,
        "handoff_number": 1,
        "cwd": "/home/op/projects/myproject",
        "command": ["claude"],
        "spawned_at": "2026-04-28T00:00:00Z",
    }
    base.update(overrides)
    path = record_dir / f"{agent_id}.json"
    path.write_text(json.dumps(base, indent=2) + "\n", encoding="utf-8")
    return path


# -- threshold ---------------------------------------------------------------

class TestThreshold:
    @pytest.mark.parametrize("pct,expected", [
        (0.0, "green"),
        (49.99, "green"),
        (50.0, "yellow"),
        (50.01, "yellow"),
        (69.99, "yellow"),
        (70.0, "red"),
        (100.0, "red"),
    ])
    def test_boundaries(self, pct: float, expected: str) -> None:
        assert health.threshold(pct) == expected

    def test_none_is_unknown(self) -> None:
        assert health.threshold(None) == "unknown"


# -- read_context_pct --------------------------------------------------------

class TestReadContextPct:
    def test_known_model(self, transcript_with_usage: Path) -> None:
        payload = {"transcript_path": str(transcript_with_usage)}
        pct, model = health.read_context_pct(payload)
        # 50_000 / 200_000 = 25.0% (output_tokens excluded)
        assert pct == 25.0
        assert model == "claude-sonnet-4-6"

    def test_output_tokens_excluded(self, tmp_path: Path) -> None:
        """If output_tokens were summed into context, this would be 50%; with
        them correctly excluded it should be 25%."""
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-sonnet-4-6",
                "usage": {
                    "input_tokens": 50_000,
                    "output_tokens": 50_000,
                },
            },
        }) + "\n", encoding="utf-8")
        pct, _ = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 25.0

    def test_last_usage_wins(self, tmp_path: Path) -> None:
        """When the transcript has multiple usage objects (multi-turn session),
        the most-recent one is the live context size — earlier usage is from
        prior turns and stale."""
        path = tmp_path / "t.jsonl"
        lines = [
            json.dumps({
                "type": "assistant",
                "message": {
                    "model": "claude-sonnet-4-6",
                    "usage": {"input_tokens": 10_000},
                },
            }),
            json.dumps({
                "type": "assistant",
                "message": {
                    "model": "claude-sonnet-4-6",
                    "usage": {"input_tokens": 100_000},
                },
            }),
        ]
        path.write_text("\n".join(lines) + "\n", encoding="utf-8")
        pct, _ = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 50.0

    def test_unknown_model(self, tmp_path: Path) -> None:
        """An unknown model leaves pct=None but still surfaces the model name
        so a future operator can add it to CONTEXT_LIMITS via config."""
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-future-99",
                "usage": {"input_tokens": 1000},
            },
        }) + "\n", encoding="utf-8")
        pct, model = health.read_context_pct({"transcript_path": str(path)})
        assert pct is None
        assert model == "claude-future-99"

    def test_missing_transcript_path(self) -> None:
        pct, model = health.read_context_pct({})
        assert pct is None
        assert model is None

    def test_transcript_path_does_not_exist(self) -> None:
        pct, model = health.read_context_pct({
            "transcript_path": "/nonexistent/transcript.jsonl",
        })
        assert pct is None
        assert model is None

    def test_transcript_with_no_usage_lines(self, tmp_path: Path) -> None:
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({"type": "user", "content": "hi"}) + "\n",
                        encoding="utf-8")
        pct, _ = health.read_context_pct({"transcript_path": str(path)})
        assert pct is None

    def test_malformed_lines_are_skipped(self, tmp_path: Path) -> None:
        """A garbage line in the middle of the JSONL must not crash the walk."""
        path = tmp_path / "t.jsonl"
        path.write_text(
            "this is not json\n"
            + json.dumps({
                "type": "assistant",
                "message": {
                    "model": "claude-sonnet-4-6",
                    "usage": {"input_tokens": 50_000},
                },
            }) + "\n"
            + "also garbage\n",
            encoding="utf-8",
        )
        pct, model = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 25.0
        assert model == "claude-sonnet-4-6"


# -- update_record -----------------------------------------------------------

class TestUpdateRecord:
    def test_missing_record_returns_false(self, fleet_home_tmp: Path) -> None:
        """Skill is not authoritative for record creation. Missing record →
        False, no file written."""
        ok = health.update_record("missing01", context_pct=42.0,
                                  context_source="hook")
        assert ok is False
        assert not (fleet_home_tmp / "agents" / "missing01.json").exists()

    def test_owned_fields_are_written(self, fleet_home_tmp: Path) -> None:
        path = _seed_record(fleet_home_tmp, "agent0001")
        ok = health.update_record(
            "agent0001",
            context_pct=42.5,
            context_source="hook",
            blocked=True,
            blocked_reason="awaiting tool result",
            handoff_type="auto-yellow",
        )
        assert ok is True
        record = json.loads(path.read_text(encoding="utf-8"))
        assert record["context_pct"] == 42.5
        assert record["context_source"] == "hook"
        assert record["blocked"] is True
        assert record["blocked_reason"] == "awaiting tool result"
        assert record["handoff_type"] == "auto-yellow"

    def test_unowned_fields_are_dropped_silently(
        self, fleet_home_tmp: Path,
    ) -> None:
        """If a caller passes a dispatch-owned field, the function must not
        write it. This is the load-bearing safety check from SKILL.md's
        ownership partition: a buggy caller cannot corrupt PID, mode, etc."""
        path = _seed_record(fleet_home_tmp, "agent0002", pid=99999, mode="execute")
        ok = health.update_record(
            "agent0002",
            context_pct=10.0,
            pid=1,           # dispatch-owned, must be ignored
            mode="plan",     # dispatch-owned, must be ignored
            engine="codex",  # dispatch-owned, must be ignored
        )
        assert ok is True
        record = json.loads(path.read_text(encoding="utf-8"))
        assert record["pid"] == 99999
        assert record["mode"] == "execute"
        assert record["engine"] == "claude-code"
        assert record["context_pct"] == 10.0  # owned write went through

    def test_preserves_unknown_future_fields(self, fleet_home_tmp: Path) -> None:
        """If the record contains a field added in a future agent.go schema
        bump (Field6 between v1 fields), the skill must round-trip it
        unchanged. Read-modify-write of unknown keys protects forward
        compat."""
        path = _seed_record(fleet_home_tmp, "agent0003")
        record = json.loads(path.read_text(encoding="utf-8"))
        record["future_field_added_later"] = {"nested": [1, 2, 3]}
        path.write_text(json.dumps(record, indent=2) + "\n", encoding="utf-8")

        ok = health.update_record("agent0003", context_pct=15.0)
        assert ok is True
        new_record = json.loads(path.read_text(encoding="utf-8"))
        assert new_record["future_field_added_later"] == {"nested": [1, 2, 3]}

    def test_last_activity_ts_is_set(self, fleet_home_tmp: Path) -> None:
        path = _seed_record(fleet_home_tmp, "agent0004",
                            last_activity_ts="2026-01-01T00:00:00Z")
        before = datetime.now(timezone.utc)
        ok = health.update_record("agent0004", context_pct=0.0)
        after = datetime.now(timezone.utc)
        assert ok is True
        record = json.loads(path.read_text(encoding="utf-8"))
        ts = datetime.strptime(record["last_activity_ts"],
                               "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)
        # Truncate the bookends to second precision since the format drops
        # subseconds — otherwise `before` could be a fraction of a second
        # ahead of `ts` and falsely fail.
        before = before.replace(microsecond=0)
        after = after.replace(microsecond=0)
        assert before <= ts <= after

    def test_field_order_preserved(self, fleet_home_tmp: Path) -> None:
        """Read-modify-write must not reshuffle key order. Operators eyeball
        these JSON files; consistent order beats consistent alphabet."""
        path = _seed_record(fleet_home_tmp, "agent0005")
        before_keys = list(json.loads(path.read_text(encoding="utf-8")).keys())
        ok = health.update_record("agent0005", context_pct=5.0,
                                  context_source="hook")
        assert ok is True
        after_keys = list(json.loads(path.read_text(encoding="utf-8")).keys())
        assert before_keys == after_keys

    def test_no_temp_file_left_behind_on_success(
        self, fleet_home_tmp: Path,
    ) -> None:
        _seed_record(fleet_home_tmp, "agent0006")
        ok = health.update_record("agent0006", context_pct=1.0)
        assert ok is True
        agents_dir = fleet_home_tmp / "agents"
        leftover = [p for p in agents_dir.iterdir()
                    if p.name.endswith(".tmp") or ".tmp" in p.name]
        assert leftover == [], f"tempfile leaked: {leftover}"

    def test_corrupt_record_returns_false(self, fleet_home_tmp: Path) -> None:
        """If the existing file is unparseable JSON, the skill bails rather
        than overwriting — better stale data than data loss. Operator can
        diagnose; the host agent keeps running."""
        agents_dir = fleet_home_tmp / "agents"
        agents_dir.mkdir(parents=True, exist_ok=True)
        path = agents_dir / "agent0007.json"
        path.write_text("{ this is not valid json", encoding="utf-8")

        ok = health.update_record("agent0007", context_pct=99.0)
        assert ok is False
        # Original (corrupt) content is untouched, not overwritten.
        assert path.read_text(encoding="utf-8") == "{ this is not valid json"


# -- agent_record_path ------------------------------------------------------

class TestPaths:
    def test_fleet_home_env_override(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        monkeypatch.setenv("FLEET_HOME", str(tmp_path / "custom"))
        assert health.fleet_home() == tmp_path / "custom"
        assert health.agent_record_path("abc") == \
            tmp_path / "custom" / "agents" / "abc.json"

    def test_default_is_home_fleet(
        self, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        monkeypatch.delenv("FLEET_HOME", raising=False)
        assert health.fleet_home() == Path.home() / ".fleet"


# -- reconcile_pid ----------------------------------------------------------

class TestReconcilePid:
    """Drift recovery for the P0 spawn-pid bug (2026-05-13).

    Spawn writes the real claude pid into agent.Record.PID via
    resolveEnginePid (internal/spawn/pidresolver.go). But the pid can
    drift if claude internally execs — e.g. on /remote-control reconnect,
    or on Claude Code's auto-update path. The fleet-guard Stop hook fires
    on every assistant turn from inside the claude process itself, so its
    os.getpid() ancestor chain ALWAYS contains the live claude pid.

    reconcile_pid checks if the recorded pid is still alive; on miss, it
    re-resolves from the running process ancestry and rewrites the
    record. Live pid = no-op."""

    def test_returns_false_when_record_missing(
        self, fleet_home_tmp: Path,
    ) -> None:
        """No record on disk → no reconciliation possible. Mirrors
        update_record's missing-record contract."""
        ok = health.reconcile_pid("missing01")
        assert ok is False

    def test_no_op_when_recorded_pid_alive(
        self, fleet_home_tmp: Path,
    ) -> None:
        """Happy path: recorded pid matches a live process → return True
        without touching the record. We use our own pid (the test process)
        which is guaranteed alive."""
        path = _seed_record(fleet_home_tmp, "agent_live", pid=os.getpid())
        before = path.read_text(encoding="utf-8")
        ok = health.reconcile_pid("agent_live")
        assert ok is True
        # Record bytes unchanged — no write happened.
        assert path.read_text(encoding="utf-8") == before

    def test_rewrites_pid_when_recorded_pid_dead(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Drift case: recorded pid is dead, but our ancestor chain has a
        live process → write the live pid into the record.

        We seed a record with pid=1 (init, always alive but NOT our
        ancestor) — wait, init IS alive, so we need a definitely-dead pid.
        Use a very high pid that can't exist. Then stub _resolve_self_pid
        to return our own pid as the resolved successor."""
        # Pick a pid guaranteed dead (max pid on macOS is 99999, on Linux
        # 4194304; 9_999_999 is safely beyond both).
        dead_pid = 9_999_999
        path = _seed_record(fleet_home_tmp, "agent_drift", pid=dead_pid)

        # Stub the self-pid resolver to return our process pid, which
        # the live check will succeed on.
        monkeypatch.setattr(health, "_resolve_self_pid",
                            lambda agent_id: os.getpid())

        ok = health.reconcile_pid("agent_drift")
        assert ok is True
        record = json.loads(path.read_text(encoding="utf-8"))
        assert record["pid"] == os.getpid()

    def test_no_rewrite_when_recorded_dead_and_resolver_returns_none(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """If we can't find a live successor in our ancestor chain (the
        fleet-guard skill is somehow running outside a claude session),
        leave the record alone — better stale data than guessed data.
        Returns False to signal "no reconciliation happened"."""
        dead_pid = 9_999_999
        path = _seed_record(fleet_home_tmp, "agent_orphan", pid=dead_pid)
        before = path.read_text(encoding="utf-8")

        monkeypatch.setattr(health, "_resolve_self_pid",
                            lambda agent_id: None)

        ok = health.reconcile_pid("agent_orphan")
        assert ok is False
        # Record bytes unchanged — no write happened.
        assert path.read_text(encoding="utf-8") == before

    def test_pid_zero_in_record_triggers_resolve(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """pid=0 in the record (legacy or partial write) is treated as
        dead — kick the resolver."""
        path = _seed_record(fleet_home_tmp, "agent_zero", pid=0)
        monkeypatch.setattr(health, "_resolve_self_pid",
                            lambda agent_id: os.getpid())
        ok = health.reconcile_pid("agent_zero")
        assert ok is True
        record = json.loads(path.read_text(encoding="utf-8"))
        assert record["pid"] == os.getpid()
