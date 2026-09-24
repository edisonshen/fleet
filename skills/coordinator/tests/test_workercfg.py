"""workercfg.py tests: tier -> model ladder, heuristic tiering, escalation."""
from __future__ import annotations

import pytest

import parse
import workercfg


def _task(
    spec: str = "Fix the thing.",
    acceptance: str = "Thing is fixed.",
    *,
    complexity: str = "",
    priority: str = "P1",
    depends_on: list[str] | None = None,
    dispatch_generation: int = 0,
) -> parse.Task:
    return parse.Task(
        slug="thing-aaaa",
        status="ready",
        priority=priority,
        spec=spec,
        acceptance=acceptance,
        notes="",
        complexity=complexity,
        depends_on=list(depends_on or []),
        dispatch_generation=dispatch_generation,
    )


# ---------- resolve_worker_model ----------


@pytest.mark.parametrize(
    ("tier", "model", "effort"),
    [
        ("simple", "gpt-5.6-luna", "medium"),
        ("coding", "gpt-5.6-terra", "medium"),
        ("complex", "gpt-5.6-sol", "high"),
    ],
)
def test_codex_tier_ladder(tier: str, model: str, effort: str) -> None:
    wm = workercfg.resolve_worker_model("codex", tier)
    assert (wm.engine, wm.tier, wm.model, wm.effort) == ("codex", tier, model, effort)
    assert wm.fallbacks == ("gpt-5.5", "gpt-5.4")


def test_codex_unavailable_skips_to_fallback_at_medium() -> None:
    wm = workercfg.resolve_worker_model("codex", "complex", {"gpt-5.6-sol"})
    assert (wm.model, wm.effort, wm.fallbacks) == ("gpt-5.5", "medium", ("gpt-5.4",))
    wm = workercfg.resolve_worker_model("codex", "coding", {"gpt-5.6-terra", "gpt-5.5"})
    assert (wm.model, wm.effort, wm.fallbacks) == ("gpt-5.4", "medium", ())


@pytest.mark.parametrize("tier", workercfg.TIERS)
def test_claude_every_tier_is_opus_medium(tier: str) -> None:
    wm = workercfg.resolve_worker_model("claude-code", tier)
    assert (wm.model, wm.effort) == ("claude-opus-5-5", "medium")
    assert wm.fallbacks == ("claude-opus-5",)
    assert wm.tier == tier


def test_claude_unavailable_falls_to_opus_5() -> None:
    wm = workercfg.resolve_worker_model("claude-code", "coding", ["claude-opus-5-5"])
    assert (wm.model, wm.effort, wm.fallbacks) == ("claude-opus-5", "medium", ())


def test_exhausted_ladder_returns_no_model() -> None:
    wm = workercfg.resolve_worker_model(
        "codex", "simple", {"gpt-5.6-luna", "gpt-5.5", "gpt-5.4"},
    )
    assert wm.model == "" and wm.effort == "" and wm.fallbacks == ()
    assert wm.tier == "simple"


def test_unknown_tier_defaults_to_coding() -> None:
    assert workercfg.resolve_worker_model("codex", "bogus").model == "gpt-5.6-terra"


# ---------- escalate ----------


@pytest.mark.parametrize(
    ("tier", "attempts", "want"),
    [
        ("simple", 0, "simple"),
        ("simple", 1, "coding"),
        ("simple", 2, "complex"),
        ("simple", 9, "complex"),
        ("coding", 1, "complex"),
        ("complex", 3, "complex"),
        ("coding", -1, "coding"),
        ("bogus", 0, "coding"),
    ],
)
def test_escalate(tier: str, attempts: int, want: str) -> None:
    assert workercfg.escalate(tier, attempts) == want


# ---------- heuristic tiering ----------


def test_infer_default_is_coding() -> None:
    assert workercfg.infer_tier(_task()) == "coding"


def test_infer_simple_needs_a_simple_signal() -> None:
    assert workercfg.infer_tier(_task("Fix typo in README.")) == "simple"
    assert workercfg.infer_tier(_task("Rename --foo flag to --bar in help text.")) == "simple"
    # A simple word buried in a long spec does not make it simple.
    long_spec = "Rename the field. " + "Also do many other things. " * 60
    assert workercfg.infer_tier(_task(long_spec)) == "coding"


def test_infer_complex_from_stacked_signals() -> None:
    spec = (
        "Migrate the dispatch protocol to a new schema; see "
        "docs/DESIGN-coord-dispatch.md. Cross-cutting: loop, supervisor, CLI."
    )
    rows = "\n".join(f"| S{i} | scenario {i} | pass |" for i in range(1, 9))
    t = _task(spec, rows, priority="P0", depends_on=["a-1111", "b-2222"])
    assert workercfg.infer_tier(t) == "complex"


def test_infer_single_complex_word_stays_coding() -> None:
    assert workercfg.infer_tier(_task("Add a lock around the write.")) == "coding"


def test_classify_explicit_wins_over_inference() -> None:
    assert workercfg.classify_task(_task("Fix typo.", complexity="complex")) == "complex"
    assert workercfg.classify_task(_task("Fix typo.")) == "simple"


# ---------- resolve_for_task ----------


def test_resolve_for_task_first_dispatch_uses_classified_tier() -> None:
    wm = workercfg.resolve_for_task("codex", _task("Fix typo."))
    assert (wm.tier, wm.model) == ("simple", "gpt-5.6-luna")


def test_resolve_for_task_escalates_by_dispatch_generation() -> None:
    t = _task("Fix typo.", dispatch_generation=1)
    wm = workercfg.resolve_for_task("codex", t)
    assert (wm.tier, wm.model) == ("coding", "gpt-5.6-terra")
    wm = workercfg.resolve_for_task("codex", t, prior_attempts=0)
    assert (wm.tier, wm.model) == ("simple", "gpt-5.6-luna")
    wm = workercfg.resolve_for_task("codex", t, prior_attempts=5)
    assert (wm.tier, wm.model, wm.effort) == ("complex", "gpt-5.6-sol", "high")


def test_resolve_for_task_threads_unavailable() -> None:
    wm = workercfg.resolve_for_task(
        "codex", _task(complexity="coding"), unavailable={"gpt-5.6-terra"},
    )
    assert (wm.model, wm.effort) == ("gpt-5.5", "medium")
