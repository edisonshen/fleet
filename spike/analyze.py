#!/usr/bin/env python3
"""
Read ~/.fleet/spike/metrics.jsonl and print a summary that answers the
three feasibility-spike questions. Run after a session of normal Claude
Code usage.

Usage:
    python3 spike/analyze.py
"""
from __future__ import annotations

import json
import statistics
from collections import Counter
from pathlib import Path

METRICS = Path.home() / ".fleet" / "spike" / "metrics.jsonl"


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = (len(s) - 1) * p
    lo, hi = int(k), min(int(k) + 1, len(s) - 1)
    if lo == hi:
        return float(s[lo])
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def main() -> int:
    if not METRICS.exists():
        print(f"no metrics yet: {METRICS}")
        print("install the hook (see spike/README.md), use Claude Code, then re-run this.")
        return 1

    rows: list[dict] = []
    with METRICS.open() as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except Exception:
                pass

    if not rows:
        print("metrics.jsonl is empty.")
        return 1

    print(f"=== Fleet spike summary ({len(rows)} hook fires) ===\n")

    errors = [r for r in rows if r.get("error")]
    ok = [r for r in rows if not r.get("error")]

    print(f"Total fires:    {len(rows)}")
    print(f"Successful:     {len(ok)}")
    print(f"Errors:         {len(errors)}")
    if errors:
        ec = Counter(r.get("error", "?") for r in errors)
        for k, v in ec.most_common():
            print(f"  - {k}: {v}")
    print()

    # Sessions seen.
    sessions = Counter(r.get("session_id", "") for r in rows)
    print(f"Distinct sessions: {len(sessions)}")
    for sid, n in sessions.most_common(5):
        if sid:
            print(f"  {sid[:24]}...  {n} fires")
    print()

    # Models seen.
    models = Counter(r.get("model", "") for r in rows)
    print("Models seen:")
    for m, n in models.most_common():
        print(f"  {m or '<unset>'}: {n}")
    unknown_models = [r for r in ok if not r.get("context_limit_known", True)]
    if unknown_models:
        print(f"  ⚠ {len(unknown_models)} fires used DEFAULT context limit "
              f"(model not in CONTEXT_LIMITS dict)")
    print()

    # Q1 — coverage. Per fire, lines_with_usage / assistant_lines.
    print("=== Q1: Availability / coverage ===")
    coverage = []
    for r in ok:
        a = r.get("transcript_assistant_lines", 0)
        u = r.get("transcript_lines_with_usage", 0)
        if a > 0:
            coverage.append(u / a)
    if coverage:
        print(f"usage_lines / assistant_lines:")
        print(f"  mean    {statistics.mean(coverage):.3f}")
        print(f"  median  {statistics.median(coverage):.3f}")
        print(f"  min     {min(coverage):.3f}")
        print(f"  max     {max(coverage):.3f}")
        if statistics.median(coverage) >= 0.95:
            print("  → PASS — every assistant turn has usage data")
        elif statistics.median(coverage) >= 0.5:
            print("  → PARTIAL — some assistant lines lack usage; investigate which")
        else:
            print("  → CONCERNING — many assistant turns lack usage data")
    else:
        print("  no fires with assistant_lines > 0")
    print()

    # Q2 — latency.
    print("=== Q2: Latency ===")
    latencies = [r.get("latency_ms", 0) for r in ok if "latency_ms" in r]
    if latencies:
        print(f"hook latency (ms) over {len(latencies)} fires:")
        print(f"  mean    {statistics.mean(latencies):.1f}")
        print(f"  median  {statistics.median(latencies):.1f}")
        print(f"  p95     {percentile(latencies, 0.95):.1f}")
        print(f"  p99     {percentile(latencies, 0.99):.1f}")
        print(f"  max     {max(latencies)}")
        p95 = percentile(latencies, 0.95)
        if p95 <= 500:
            print(f"  → PASS — p95 {p95:.0f}ms ≤ 500ms bar")
        else:
            print(f"  → FAIL — p95 {p95:.0f}ms > 500ms bar")
    print()

    # Largest transcript size encountered (informs whether p95 holds at scale).
    sizes = [r.get("transcript_size_bytes", 0) for r in ok]
    sizes = [s for s in sizes if s > 0]
    if sizes:
        print(f"Transcript size (bytes):")
        print(f"  median  {int(statistics.median(sizes))}")
        print(f"  max     {max(sizes)}  ({max(sizes)/1024/1024:.1f} MB)")
    print()

    # Q3 — accuracy. Hook can't compare against /context automatically; just
    # show the latest computed_pct so the operator can eyeball it.
    print("=== Q3: Accuracy (manual check) ===")
    print("Compare the values below against /context output at the same time.")
    print("The most recent fires are most useful for this comparison.\n")
    print(f"{'time (UTC)':<22} {'session':<14} {'model':<22} {'tokens':>10} {'limit':>10} {'pct':>7}")
    print("-" * 95)
    for r in ok[-10:]:
        ts = r.get("ts", "")
        sid = (r.get("session_id") or "")[:12]
        model = (r.get("model") or "?")[:22]
        tok = r.get("tokens", {}).get("context_total", 0)
        lim = r.get("context_limit")
        pct = r.get("computed_pct")
        lim_str = f"{lim:>10}" if lim is not None else f"{'unknown':>10}"
        pct_str = f"{pct:>6.1f}%" if pct is not None else f"{'n/a':>7}"
        print(f"{ts:<22} {sid:<14} {model:<22} {tok:>10} {lim_str} {pct_str}")
    print()
    print("=== verdict ===")
    print("Open spike/q3-checkpoints.md for /context vs computed_pct comparisons.")
    print("Open ~/.fleet/spike/payloads/ for raw Stop hook payloads (one per session).")
    print("Open ~/.fleet/spike/transcript-snippets/ for last-usage objects per fire.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
