"""Tests for skills/fleet-guard/health.py.

The skill is non-authoritative for record creation — `fleet dispatch` writes
the initial record and the skill only mutates owned fields on every fire. The
tests below pin that contract: missing record returns False; unknown fields
are dropped; preserved fields survive a write; atomic semantics never expose a
torn JSON to a concurrent reader.

FLEET_HOME is redirected to a tmp_path for every test via the autouse fixture.
"""
from __future__ import annotations

import importlib.util
import json
import os
from datetime import datetime, timezone
from pathlib import Path

import pytest

import health


def _load_stop_hook():
    """Load spike/stop-hook.py as a module despite its hyphenated filename
    (not importable via `import`). Used by the table-parity test so the two
    CONTEXT_LIMITS tables can't drift again."""
    # tests/ -> fleet-guard/ -> skills/ -> repo root -> spike/stop-hook.py
    repo_root = Path(__file__).resolve().parents[3]
    hook_path = repo_root / "spike" / "stop-hook.py"
    spec = importlib.util.spec_from_file_location("spike_stop_hook", hook_path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


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
        (39.99, "green"),
        (40.0, "yellow"),
        (40.01, "yellow"),
        (49.99, "yellow"),
        (50.0, "red"),
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

    def test_unknown_model_defaults_to_1m(
        self, tmp_path: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        """Bug D policy change (operator directive 2026-06-10: "if you dont
        know you should flag it, and set default value to 1 million, dont let
        it silo"): an unknown model no longer silos to pct=None — it computes
        against the 1M default AND flags loudly on stderr. The raw model name
        still surfaces for diagnostics."""
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-future-99",
                "usage": {"input_tokens": 500_000},
            },
        }) + "\n", encoding="utf-8")
        pct, model = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 50.0  # 500k / 1_000_000 default
        assert model == "claude-future-99"
        err = capsys.readouterr().err
        assert "claude-future-99" in err
        assert "unknown model" in err.lower()

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

    def test_transcript_with_no_usage_lines_flags_loudly(
        self, tmp_path: Path, capsys: pytest.CaptureFixture,
    ) -> None:
        """Bug D: the no-usage return path is the frozen-health-writer
        fingerprint (RC coord 80de593d, 2026-06-10). It must emit a loud
        stderr diagnostic naming the transcript path instead of silently
        returning (None, None)."""
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({"type": "user", "content": "hi"}) + "\n",
                        encoding="utf-8")
        pct, model = health.read_context_pct({"transcript_path": str(path)})
        assert pct is None
        assert model is None
        err = capsys.readouterr().err
        assert str(path) in err
        assert "no usage" in err.lower()

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


# -- opus-4-8 1M-context regression (fleet-guard-model-limit-db14) -----------

class TestOpus48HandoffFires:
    """REGRESSION (fleet-guard-model-limit-db14, P0): before the fix,
    CONTEXT_LIMITS topped out at claude-opus-4-7 and the live model id is
    "claude-opus-4-8[1m]" (1M context). read_context_pct returned None ->
    threshold(None)="unknown" -> the 40/50 auto-handoff NEVER fired and the
    agent ran to 100% into native auto-compact. These tests prove the proactive
    handoff would now fire on opus-4-8."""

    def _write_transcript(self, tmp_path: Path, model: str, ctx_tokens: int) -> str:
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": model,
                "usage": {
                    "input_tokens": ctx_tokens,
                    # output excluded from context_pct — set high to prove it.
                    "output_tokens": 500_000,
                },
            },
        }) + "\n", encoding="utf-8")
        return str(path)

    def test_opus_4_8_bracket_variant_over_70_is_red(self, tmp_path: Path) -> None:
        # 750k / 1_000_000 = 75% -> RED. The bracketed [1m] variant must
        # resolve to the 1M limit, not fall through to unknown.
        tp = self._write_transcript(tmp_path, "claude-opus-4-8[1m]", 750_000)
        pct, model = health.read_context_pct({"transcript_path": tp})
        assert pct == 75.0
        assert model == "claude-opus-4-8[1m]"  # raw id surfaced verbatim
        assert health.threshold(pct) == "red"

    def test_opus_4_8_bracket_variant_over_40_is_yellow(self, tmp_path: Path) -> None:
        # 450k / 1_000_000 = 45% -> YELLOW.
        tp = self._write_transcript(tmp_path, "claude-opus-4-8[1m]", 450_000)
        pct, _ = health.read_context_pct({"transcript_path": tp})
        assert pct == 45.0
        assert health.threshold(pct) == "yellow"

    def test_opus_4_8_plain_resolves_to_1m(self, tmp_path: Path) -> None:
        # The un-decorated id is in the table directly; 200k -> 20% green.
        tp = self._write_transcript(tmp_path, "claude-opus-4-8", 200_000)
        pct, model = health.read_context_pct({"transcript_path": tp})
        assert pct == 20.0
        assert model == "claude-opus-4-8"
        assert health.threshold(pct) == "green"


# -- _normalize_model --------------------------------------------------------

class TestNormalizeModel:
    @pytest.mark.parametrize("raw,expected", [
        ("claude-opus-4-8", "claude-opus-4-8"),
        ("claude-opus-4-8[1m]", "claude-opus-4-8"),
        ("claude-opus-4-8-20260601", "claude-opus-4-8"),
        ("claude-opus-4-8[1m]", "claude-opus-4-8"),
        ("claude-sonnet-4-6-20251101", "claude-sonnet-4-6"),
        ("", ""),
    ])
    def test_strips_known_decorations(self, raw: str, expected: str) -> None:
        assert health._normalize_model(raw) == expected

    def test_unknown_model_passes_through_unchanged(self) -> None:
        # No family/prefix guessing — an unknown id (after stripping) stays
        # unknown so the table comment's intent holds.
        assert health._normalize_model("claude-future-99") == "claude-future-99"

    def test_dated_and_bracketed_variants_resolve_to_1m_limit(
        self, tmp_path: Path,
    ) -> None:
        """All opus-4-8 decorations resolve to the 1M limit; a genuinely
        unknown model still returns (None, model)."""
        for variant in ("claude-opus-4-8",
                        "claude-opus-4-8[1m]",
                        "claude-opus-4-8-20260601"):
            path = tmp_path / "t.jsonl"
            path.write_text(json.dumps({
                "type": "assistant",
                "message": {
                    "model": variant,
                    "usage": {"input_tokens": 500_000},
                },
            }) + "\n", encoding="utf-8")
            pct, model = health.read_context_pct({"transcript_path": str(path)})
            assert pct == 50.0, f"{variant} did not resolve to 1M limit"
            assert model == variant

        # Genuinely unknown -> 1M default (bug D policy), raw id preserved.
        path = tmp_path / "unknown.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-future-99",
                "usage": {"input_tokens": 500_000},
            },
        }) + "\n", encoding="utf-8")
        pct, model = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 50.0
        assert model == "claude-future-99"


# -- _resolve_limit ([1m] bracket = 1M regardless of base) -------------------

class TestResolveLimit:
    """codex P2 regression (2026-06-03): a "[1m]" bracket tag denotes the
    1M-context variant and must resolve to 1,000,000 even when the base model's
    table entry is 200k. Naively stripping "[1m]" and looking up the base would
    make a 500k context look like 250% and fire the red handoff far too early.
    """

    def test_exact_table_hit(self) -> None:
        assert health._resolve_limit("claude-opus-4-8") == 1_000_000
        assert health._resolve_limit("claude-sonnet-4-6") == 200_000

    def test_one_m_bracket_on_200k_base_resolves_to_1m(self) -> None:
        # claude-sonnet-4-6 base is 200k; the [1m] variant is 1M, NOT 200k.
        assert health._resolve_limit("claude-sonnet-4-6[1m]") == 1_000_000

    def test_one_m_bracket_on_1m_base_resolves_to_1m(self) -> None:
        assert health._resolve_limit("claude-opus-4-8[1m]") == 1_000_000

    def test_non_1m_bracket_strips_to_base(self) -> None:
        # A non-[1m] bracket decoration falls back to the stripped base limit.
        assert health._resolve_limit("claude-sonnet-4-6[beta]") == 200_000

    def test_dated_variant_strips_to_base(self) -> None:
        assert health._resolve_limit("claude-sonnet-4-6-20251101") == 200_000

    def test_substring_1m_in_bracket_does_not_match(self) -> None:
        """codex iter-2 (2026-06-03): the bracket token must equal "1m"
        EXACTLY. A token that merely CONTAINS "1m" ("[v1m]", "[not-1m]") is a
        different variant and must fall back to the stripped base limit, not
        the 1M window."""
        assert health._resolve_limit("claude-sonnet-4-6[v1m]") == 200_000
        assert health._resolve_limit("claude-sonnet-4-6[not-1m]") == 200_000
        # An unknown base with a non-exact-1m bracket falls to the 1M
        # default (bug D policy) — same number as the [1m] tag but via the
        # unknown-model flag path, not the bracket fast path.
        assert health._resolve_limit("claude-future-99[v1m]") == 1_000_000

    def test_one_m_bracket_is_case_insensitive(self) -> None:
        assert health._resolve_limit("claude-sonnet-4-6[1M]") == 1_000_000

    def test_unknown_defaults_to_1m_with_flag(
        self, capsys: pytest.CaptureFixture,
    ) -> None:
        """Bug D policy change: unknown model -> 1_000_000 default + loud
        stderr flag naming the unresolved id (was: None, silent). Empty /
        missing model still returns None — there is no id to flag and the
        caller handles the no-model case explicitly."""
        assert health._resolve_limit("totally-new-model") == 1_000_000
        err = capsys.readouterr().err
        assert "totally-new-model" in err
        assert "unknown model" in err.lower()
        assert health._resolve_limit("claude-future-99[1m]") == 1_000_000  # [1m] tag wins
        assert health._resolve_limit("") is None
        assert health._resolve_limit(None) is None

    def test_bracket_1m_regression_no_unknown_flag(
        self, capsys: pytest.CaptureFixture,
    ) -> None:
        """Regression guard (verified correct this incident — do NOT change
        [1m] semantics): the bracketed 1M variant resolves via the bracket
        fast path with NO unknown-model flag on stderr."""
        assert health._resolve_limit("claude-opus-4-8[1m]") == 1_000_000
        assert capsys.readouterr().err == ""

    def test_fable_5_resolves_from_table_no_flag(
        self, capsys: pytest.CaptureFixture,
    ) -> None:
        """Bug D root cause: the live RC coord (80de593d, 2026-06-10) ran
        claude-fable-5, which was absent from CONTEXT_LIMITS -> context_pct
        null every turn. Bare claude-fable-5 must be a TABLE hit (1M window
        per the harness model catalog), not an unknown-model default — so
        no flag is emitted."""
        assert health._resolve_limit("claude-fable-5") == 1_000_000
        assert capsys.readouterr().err == ""

    def test_sonnet_1m_over_70_fires_red(self, tmp_path: Path) -> None:
        """End-to-end through read_context_pct: a 750k context on
        claude-sonnet-4-6[1m] is 75% of 1M -> red. Under the buggy strip it
        would be 375% of 200k (still red but for the wrong reason); the
        meaningful assertion is the EXACT pct, proving the 1M denominator."""
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-sonnet-4-6[1m]",
                "usage": {"input_tokens": 750_000},
            },
        }) + "\n", encoding="utf-8")
        pct, model = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 75.0  # 750k / 1M, NOT 750k / 200k = 375.0
        assert model == "claude-sonnet-4-6[1m]"
        assert health.threshold(pct) == "red"

    def test_sonnet_1m_under_40_is_green(self, tmp_path: Path) -> None:
        """The bug's user-visible symptom: a [1m]-base-200k model at 30% of 1M
        (300k tokens) must read green, not the 150% the strip-to-base bug
        would produce (which would spuriously trigger handoff)."""
        path = tmp_path / "t.jsonl"
        path.write_text(json.dumps({
            "type": "assistant",
            "message": {
                "model": "claude-sonnet-4-6[1m]",
                "usage": {"input_tokens": 300_000},
            },
        }) + "\n", encoding="utf-8")
        pct, _ = health.read_context_pct({"transcript_path": str(path)})
        assert pct == 30.0
        assert health.threshold(pct) == "green"


# -- CONTEXT_LIMITS table parity ---------------------------------------------

class TestContextLimitsParity:
    """The CONTEXT_LIMITS table is duplicated in health.py and
    spike/stop-hook.py (the skill and the spike must compute the same pct
    against the same transcript). Assert they are identical so a future edit
    to one without the other re-introduces the stale-table bug class
    (fleet-guard-model-limit-db14)."""

    def test_tables_are_identical(self) -> None:
        stop_hook = _load_stop_hook()
        assert health.CONTEXT_LIMITS == stop_hook.CONTEXT_LIMITS

    def test_opus_4_8_present_in_both(self) -> None:
        stop_hook = _load_stop_hook()
        assert health.CONTEXT_LIMITS.get("claude-opus-4-8") == 1_000_000
        assert stop_hook.CONTEXT_LIMITS.get("claude-opus-4-8") == 1_000_000

    def test_fable_5_present_in_both(self) -> None:
        """Bug D: claude-fable-5 (operator's model as of 2026-06-10; 1M
        window per the harness model catalog) must be in BOTH tables."""
        stop_hook = _load_stop_hook()
        assert health.CONTEXT_LIMITS.get("claude-fable-5") == 1_000_000
        assert stop_hook.CONTEXT_LIMITS.get("claude-fable-5") == 1_000_000

    def test_resolve_limit_agrees_across_modules(self) -> None:
        """The shared resolution policy (_resolve_limit) must produce the same
        limit in both modules for representative ids, including the [1m] tag and
        dated/unknown variants."""
        stop_hook = _load_stop_hook()
        for mid in ("claude-opus-4-8",
                    "claude-opus-4-8[1m]",
                    "claude-opus-4-8-20260601",
                    "claude-fable-5",
                    "claude-fable-5[1m]",
                    "claude-sonnet-4-6",
                    "claude-sonnet-4-6[1m]",
                    "claude-sonnet-4-6[beta]",
                    "claude-sonnet-4-6[1M]",
                    "claude-sonnet-4-6[v1m]",
                    "claude-sonnet-4-6[not-1m]",
                    "claude-future-99",
                    "claude-future-99[1m]",
                    "claude-future-99[v1m]",
                    ""):
            assert health._resolve_limit(mid) == stop_hook._resolve_limit(mid), mid


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
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Happy path: recorded pid matches a live process → return True
        without touching the record. We use our own pid (the test process)
        which is guaranteed alive.

        Codex iter-3 hardening: reconcile_pid also checks whether the
        recorded pid LOOKS like the right process via
        _recorded_pid_looks_like_wrapper. For this test we stub the
        wrapper check to False so a live python pid is treated as the
        engine — the test isolates the live-pid path, not the
        wrapper-detection path (that has its own coverage)."""
        monkeypatch.setattr(health, "_recorded_pid_looks_like_wrapper",
                            lambda pid, agent_id, check_disambiguator=True: False)
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


class TestReconcileLiveWrapperShellTriggersResolve:
    """Codex iter-3 hardening (2026-05-14): a live recorded pid is
    not sufficient to skip reconciliation when that pid is the
    wrapper shell. The Go-side resolver falls back to the pane pid
    (the shell) on timeout; the shell outlives the engine, so the
    Stop hook must re-resolve to the real claude pid instead of
    short-circuiting on _pid_alive."""

    def test_live_wrapper_pid_triggers_reresolve(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Recorded pid is the live wrapper shell. _pid_alive returns
        True, but _recorded_pid_looks_like_wrapper detects it as a
        shell → re-resolve via _resolve_self_pid → write the real
        claude pid."""
        path = _seed_record(fleet_home_tmp, "agent_wrap", pid=os.getpid())
        # Stub the wrapper-detector to return True (simulates a live
        # shell ancestor recorded earlier as the timeout fallback).
        monkeypatch.setattr(health, "_recorded_pid_looks_like_wrapper",
                            lambda pid, agent_id, check_disambiguator=True: True)
        # Resolver returns a different (real claude) pid.
        real_claude_pid = 99887766
        monkeypatch.setattr(health, "_resolve_self_pid",
                            lambda agent_id: real_claude_pid)
        ok = health.reconcile_pid("agent_wrap")
        assert ok is True
        record = json.loads(path.read_text(encoding="utf-8"))
        assert record["pid"] == real_claude_pid, (
            f"recorded pid should have been re-resolved from wrapper "
            f"to real claude pid {real_claude_pid}; got {record['pid']}"
        )

    def test_live_wrapper_pid_noop_when_resolver_returns_same_pid(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """When _resolve_self_pid returns the same pid we already
        have (resolver couldn't do better), reconcile must return
        True without rewriting the record. Avoids a churn-write that
        produces no change."""
        my_pid = os.getpid()
        path = _seed_record(fleet_home_tmp, "agent_same", pid=my_pid)
        before = path.read_text(encoding="utf-8")
        monkeypatch.setattr(health, "_recorded_pid_looks_like_wrapper",
                            lambda pid, agent_id, check_disambiguator=True: True)
        monkeypatch.setattr(health, "_resolve_self_pid",
                            lambda agent_id: my_pid)
        ok = health.reconcile_pid("agent_same")
        assert ok is True
        assert path.read_text(encoding="utf-8") == before

    def test_recorded_pid_looks_like_wrapper_returns_false_for_dead_pid(
        self, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Sanity: probing argv for a non-existent pid returns False
        (ps yields empty stdout) so we don't return True spuriously.

        Patch subprocess.run to return empty stdout (mirrors what
        `ps -p <dead-pid>` does in production); conftest.py's autouse
        no-op fixture patches Popen which the real subprocess.run
        would otherwise hit and crash on under test."""
        import subprocess as _sp

        class _FakeCompleted:
            stdout = ""

        monkeypatch.setattr(_sp, "run", lambda *a, **k: _FakeCompleted())
        assert health._recorded_pid_looks_like_wrapper(9999999, "x") is False

    def test_non_coord_worker_skips_disambiguator_check(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Codex iter-6 (2026-05-14): plain workers (task_id NOT
        starting with 'coord-') don't carry the fleet-coord-<id>
        disambiguator. reconcile_pid must NOT force a re-resolve on
        every Stop hook just because the worker's argv lacks the
        needle. Stays on the fast _pid_alive path."""
        # Seed a worker record (task_id='demo-task' is non-coord).
        path = _seed_record(fleet_home_tmp, "worker01",
                            pid=os.getpid(), task_id="demo-task")
        before = path.read_text(encoding="utf-8")
        # Stub _recorded_pid_looks_like_wrapper to assert it was
        # called with check_disambiguator=False.
        calls = []
        original = health._recorded_pid_looks_like_wrapper

        def _wrap(pid, agent_id, check_disambiguator=True):
            calls.append((pid, agent_id, check_disambiguator))
            # For the test process pid (our pid, python3), shell
            # detection returns False; with disambiguator disabled
            # we expect False overall.
            return False  # no wrapper detected → fast-path

        monkeypatch.setattr(health, "_recorded_pid_looks_like_wrapper", _wrap)
        ok = health.reconcile_pid("worker01")
        assert ok is True
        # Record unchanged (fast-path).
        assert path.read_text(encoding="utf-8") == before
        # The wrapper-detector WAS called, but with disambiguator
        # disabled.
        assert calls, "wrapper-detector was not invoked"
        pid_arg, agent_arg, check_arg = calls[0]
        assert check_arg is False, (
            f"non-coord worker should call wrapper-detector with "
            f"check_disambiguator=False; got True"
        )

    def test_coord_spawn_agent_uses_disambiguator_check(
        self, fleet_home_tmp: Path,
        monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Coord-spawn agents (task_id starts with 'coord-') get the
        full disambiguator check because their engine argv carries
        `fleet-coord-<id>`."""
        path = _seed_record(fleet_home_tmp, "coord01",
                            pid=os.getpid(), task_id="coord-demo")
        calls = []

        def _wrap(pid, agent_id, check_disambiguator=True):
            calls.append((pid, agent_id, check_disambiguator))
            return False

        monkeypatch.setattr(health, "_recorded_pid_looks_like_wrapper", _wrap)
        ok = health.reconcile_pid("coord01")
        assert ok is True
        assert calls, "wrapper-detector was not invoked"
        _, _, check_arg = calls[0]
        assert check_arg is True, (
            f"coord-spawn agent should call wrapper-detector with "
            f"check_disambiguator=True; got False"
        )


class TestResolveSelfPidShellSkip:
    """Codex iter-3 hardening (2026-05-14): the disambiguator match
    in _resolve_self_pid must skip shell wrappers. Otherwise a `sh -c
    "claude --remote-control fleet-coord-X ..."` ancestor — which
    carries the disambiguator transitively — would be returned as the
    resolved pid, and the wrapper can outlive the engine, leaving dead
    coords misclassified as alive.

    The unit tests below pin the helper directly, bypassing the
    ps shell-out, by patching subprocess.run output."""

    def _set_ps_output(
        self, monkeypatch: pytest.MonkeyPatch, lines: list[str],
    ) -> None:
        """Patch subprocess.run inside health._resolve_self_pid to
        return a synthetic `ps -o pid,ppid,args -ax` output."""
        import subprocess as _sp
        header = "  PID  PPID ARGS\n"
        body = "\n".join(lines) + "\n"
        out = header + body

        class _FakeCompleted:
            def __init__(self, stdout: str) -> None:
                self.stdout = stdout

        def _fake_run(*args, **kwargs):  # type: ignore[no-untyped-def]
            return _FakeCompleted(out)

        monkeypatch.setattr(_sp, "run", _fake_run)

    def test_is_shell_argv_pins_shell_token(self) -> None:
        """Sanity: the helper recognizes the common shell command
        names with optional leading path."""
        assert health._is_shell_argv("sh -c foo")
        assert health._is_shell_argv("/bin/bash -c foo")
        assert health._is_shell_argv("zsh -i")
        assert not health._is_shell_argv("claude --foo")
        assert not health._is_shell_argv("codex review")
        assert not health._is_shell_argv("")

    def test_disambiguator_match_skips_shell_wrapper(
        self, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """Coord-spawn shape: bash hook is our pid (Z); ancestor is
        claude (X) which has the disambiguator; ancestor's ancestor is
        `sh -c "claude ... fleet-coord-X"` (X') which ALSO has the
        disambiguator transitively. The resolver must return X (the
        claude), NOT X' (the wrapper shell)."""
        agent_id = "abcd1234"
        needle = f"fleet-coord-{agent_id}"
        # Simulate ancestry: our_pid (bash) → claude → sh wrapper → tmux server.
        # os.getpid() returns this test's pid; we register that in procs as
        # the bash hook process.
        our_pid = os.getpid()
        claude_pid = 90001
        wrapper_pid = 90002
        self._set_ps_output(monkeypatch, [
            f"{our_pid} {claude_pid} bash /tmp/hook.sh",
            f"{claude_pid} {wrapper_pid} claude --remote-control {needle} --dangerously-skip-permissions",
            f"{wrapper_pid} 1 sh -c claude --remote-control {needle} --dangerously-skip-permissions",
        ])
        got = health._resolve_self_pid(agent_id)
        assert got == claude_pid, (
            f"resolver returned {got}; want {claude_pid} (the claude, "
            f"NOT the wrapper shell {wrapper_pid})"
        )

    def test_disambiguator_only_on_shell_returns_none_via_engine_path(
        self, monkeypatch: pytest.MonkeyPatch,
    ) -> None:
        """When claude is already dead and only the wrapper shell with
        the disambiguator remains in our ancestry, the resolver must
        NOT return the shell pid. It returns None (or whatever the
        engine-match path produces), letting reconcile_pid bail
        rather than rewrite the record to point at a wrapper that can
        outlive the engine."""
        agent_id = "abcd1234"
        needle = f"fleet-coord-{agent_id}"
        our_pid = os.getpid()
        wrapper_pid = 90002
        # Our parent is the wrapper shell — no claude in the chain
        # anymore (drift case).
        self._set_ps_output(monkeypatch, [
            f"{our_pid} {wrapper_pid} bash /tmp/hook.sh",
            f"{wrapper_pid} 1 sh -c claude --remote-control {needle}",
        ])
        got = health._resolve_self_pid(agent_id)
        assert got != wrapper_pid, (
            f"resolver returned wrapper pid {wrapper_pid}; must skip "
            "shell ancestors carrying the disambiguator transitively"
        )
