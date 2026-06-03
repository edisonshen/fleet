#!/usr/bin/env python3
"""
Stop-hook probe for Fleet's Week-0 feasibility spike.

Reads a Claude Code Stop hook payload on stdin. Walks transcript_path's JSONL
to compute context_pct from `usage` objects. Records metrics and raw payloads
under ~/.fleet/spike/ for later analysis.

Spike answers it feeds:
  Q1 (availability): lines_with_usage / assistant_turns ratio per fire
  Q2 (latency):      latency_ms per fire (p95 against 500ms bar)
  Q3 (accuracy):     computed_pct vs. /context ground truth (manual compare)

The script is intentionally defensive — never raises, never blocks the host.
On any error, writes an `error` field into metrics.jsonl and exits 0.
"""
from __future__ import annotations

import json
import os
import sys
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path

# Model name -> context-window size in tokens.
# Mirrors skills/fleet-guard/health.py:CONTEXT_LIMITS — keep byte-consistent
# (test_health.py::TestContextLimitsParity asserts the two are identical).
# Update as Anthropic ships new model IDs. Unknown models do NOT silently
# fall back to a guessed limit — see the model-resolution block below for
# why null + context_limit_known=False is preferable to a wrong percentage.
CONTEXT_LIMITS = {
    "claude-opus-4-8":   1_000_000,
    "claude-opus-4-7":   1_000_000,
    "claude-opus-4-6":     200_000,
    "claude-opus-4-5":     200_000,
    "claude-sonnet-4-6":   200_000,
    "claude-sonnet-4-5":   200_000,
    "claude-haiku-4-5":    200_000,
}


# A "[1m]" bracket marker denotes the 1M-context variant regardless of the
# base model's default limit — see skills/fleet-guard/health.py for the full
# rationale (codex P2, 2026-06-03).
ONE_M_BRACKET_TOKENS = 1_000_000


def _normalize_model(raw):
    """Reduce a model id to its CONTEXT_LIMITS lookup key.

    Mirrors skills/fleet-guard/health.py:_normalize_model. Strips a trailing
    bracket variant ("claude-opus-4-8[1m]" -> "claude-opus-4-8") and a trailing
    -YYYYMMDD date pin. Only strips recognized decorations; otherwise-unknown
    ids stay unknown (no family/prefix guessing).
    """
    if not raw:
        return raw
    s = raw
    bracket = s.find("[")
    if bracket >= 0:
        s = s[:bracket]
    if len(s) > 9 and s[-9] == "-" and s[-8:].isdigit():
        s = s[:-9]
    return s


def _resolve_limit(raw):
    """Resolve a model id to its context-window size, or None when unknown.
    Mirrors skills/fleet-guard/health.py:_resolve_limit: exact hit -> "[1m]"
    variant means 1M -> stripped base lookup -> None.
    """
    if not raw:
        return None
    exact = CONTEXT_LIMITS.get(raw)
    if exact is not None:
        return exact
    open_b = raw.find("[")
    if open_b >= 0:
        close_b = raw.find("]", open_b)
        if close_b > open_b and raw[open_b + 1:close_b].strip().lower() == "1m":
            return ONE_M_BRACKET_TOKENS
    return CONTEXT_LIMITS.get(_normalize_model(raw))

SPIKE_DIR = Path.home() / ".fleet" / "spike"
PAYLOAD_DIR = SPIKE_DIR / "payloads"
SNIPPET_DIR = SPIKE_DIR / "transcript-snippets"
METRICS_FILE = SPIKE_DIR / "metrics.jsonl"


def main() -> int:
    t0 = time.perf_counter()
    fire_id = uuid.uuid4().hex[:8]
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    SPIKE_DIR.mkdir(parents=True, exist_ok=True)
    PAYLOAD_DIR.mkdir(parents=True, exist_ok=True)
    SNIPPET_DIR.mkdir(parents=True, exist_ok=True)

    raw = sys.stdin.read()
    record: dict = {"ts": now, "fire_id": fire_id}

    try:
        payload = json.loads(raw)
    except Exception as e:
        record["error"] = f"payload_parse: {e}"
        record["raw_payload_len"] = len(raw)
        _append(METRICS_FILE, record)
        return 0

    record["session_id"] = payload.get("session_id", "")
    record["model"] = payload.get("model", "")
    record["stop_reason"] = payload.get("stop_reason", "")
    record["hook_event_name"] = payload.get("hook_event_name", "")

    # Save the raw payload once per session_id (first fire wins).
    sid = record["session_id"] or "unknown"
    sample_path = PAYLOAD_DIR / f"{sid}.json"
    if not sample_path.exists():
        try:
            sample_path.write_text(json.dumps(payload, indent=2))
        except Exception:
            pass

    transcript_path = payload.get("transcript_path", "")
    record["transcript_path"] = transcript_path

    if not transcript_path or not Path(transcript_path).exists():
        record["error"] = "transcript_path_missing"
        _append(METRICS_FILE, record)
        return 0

    tp = Path(transcript_path)
    try:
        record["transcript_size_bytes"] = tp.stat().st_size
    except Exception:
        record["transcript_size_bytes"] = -1

    # Walk the JSONL once. Count lines, count assistant turns, count usage objects,
    # capture the most recent usage object and the most recent assistant model
    # (the Stop payload doesn't carry `model`, so the transcript is the source).
    total_lines = 0
    assistant_lines = 0
    usage_lines = 0
    last_usage: dict | None = None
    last_usage_line_index = -1
    last_message_model = ""

    try:
        with tp.open("r", encoding="utf-8", errors="replace") as f:
            for i, line in enumerate(f):
                total_lines += 1
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except Exception:
                    continue
                msg = obj.get("message") or {}
                if obj.get("type") == "assistant":
                    assistant_lines += 1
                    m = msg.get("model")
                    if isinstance(m, str) and m:
                        last_message_model = m
                # `usage` may sit at top level or under `message.usage`.
                u = obj.get("usage") or msg.get("usage")
                if isinstance(u, dict):
                    usage_lines += 1
                    last_usage = u
                    last_usage_line_index = i
    except Exception as e:
        record["error"] = f"transcript_walk: {e}"
        _append(METRICS_FILE, record)
        return 0

    # Resolve model: prefer transcript (where it actually lives), fall back to
    # payload (in case future Claude Code versions populate Stop's `model`).
    payload_model = record["model"]
    if last_message_model:
        record["model"] = last_message_model
        record["model_source"] = "transcript"
    elif payload_model:
        record["model_source"] = "payload"
    else:
        record["model_source"] = ""

    record["transcript_total_lines"] = total_lines
    record["transcript_assistant_lines"] = assistant_lines
    record["transcript_lines_with_usage"] = usage_lines
    record["last_usage_line_index"] = last_usage_line_index

    if not last_usage:
        record["error"] = "no_usage_lines_in_transcript"
        record["latency_ms"] = int((time.perf_counter() - t0) * 1000)
        _append(METRICS_FILE, record)
        return 0

    # Save the last usage object for cross-checking with /context output.
    snippet_path = SNIPPET_DIR / f"{sid}-{fire_id}.json"
    try:
        snippet_path.write_text(json.dumps({
            "fire_id": fire_id,
            "ts": now,
            "session_id": sid,
            "last_usage_line_index": last_usage_line_index,
            "usage": last_usage,
        }, indent=2))
    except Exception:
        pass

    # Compute context_pct.
    in_t = int(last_usage.get("input_tokens", 0) or 0)
    out_t = int(last_usage.get("output_tokens", 0) or 0)
    cr_t = int(last_usage.get("cache_read_input_tokens", 0) or 0)
    cc_t = int(last_usage.get("cache_creation_input_tokens", 0) or 0)
    total = in_t + cr_t + cc_t  # output_tokens excluded — they're not in next-turn context

    record["tokens"] = {
        "input": in_t,
        "output": out_t,
        "cache_read": cr_t,
        "cache_creation": cc_t,
        "context_total": total,
    }

    # If the model isn't in our table, do NOT guess a limit — that produces
    # plausible-looking but wrong percentages that poison Q3 (accuracy). The
    # fire still counts for Q1 (we have tokens), but pct stays null until
    # the model is added to CONTEXT_LIMITS.
    # Resolve via the shared policy (exact -> "[1m]"=1M -> stripped base).
    # Mirrors health.py:read_context_pct.
    limit = _resolve_limit(record["model"])
    known = limit is not None
    record["context_limit_known"] = known
    if known:
        record["context_limit"] = limit
        record["computed_pct"] = round(total * 100.0 / limit, 2)
    else:
        record["context_limit"] = None
        record["computed_pct"] = None

    record["latency_ms"] = int((time.perf_counter() - t0) * 1000)
    _append(METRICS_FILE, record)
    return 0


def _append(path: Path, obj: dict) -> None:
    try:
        with path.open("a", encoding="utf-8") as f:
            f.write(json.dumps(obj, separators=(",", ":")) + "\n")
    except Exception:
        # Last-resort: don't take down the host on a write failure.
        pass


if __name__ == "__main__":
    sys.exit(main())
