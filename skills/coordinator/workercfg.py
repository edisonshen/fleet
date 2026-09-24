"""Pure worker-model resolver: pick a budget tier per task.

Availability is an input (same discipline as reviewcfg): no probing, no
environment reads. The coord decides the tier, the DISPATCH block carries
model/effort/fallback lines, and the coord session applies them when it
invokes the Agent tool / spawn_agent.

Tier ladder (cheapest -> strongest):

    simple   small, mechanical, single-file work
    coding   an ordinary implementation task (the default)
    complex  cross-cutting / architectural / protocol work

Tier comes from, in order:
  1. the task's explicit `complexity:` bullet,
  2. a heuristic over spec/acceptance/deps/priority (`classify_task`),
  3. one step up per prior dispatch attempt (`escalate`), so a task that
     failed or was rejected in review retries on a stronger model.
"""
from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Iterable

import parse

ENGINE_CLAUDE_CODE = "claude-code"
ENGINE_CODEX = "codex"

TIER_SIMPLE = "simple"
TIER_CODING = "coding"
TIER_COMPLEX = "complex"
TIERS: tuple[str, ...] = (TIER_SIMPLE, TIER_CODING, TIER_COMPLEX)
DEFAULT_TIER = TIER_CODING

CODEX_TIER_MODELS: dict[str, str] = {
    TIER_SIMPLE: "gpt-5.6-luna",
    TIER_CODING: "gpt-5.6-terra",
    TIER_COMPLEX: "gpt-5.6-sol",
}
CODEX_TIER_EFFORT: dict[str, str] = {
    TIER_SIMPLE: "medium",
    TIER_CODING: "medium",
    TIER_COMPLEX: "high",
}
CODEX_FALLBACK: tuple[str, ...] = ("gpt-5.5", "gpt-5.4")

CLAUDE_MODELS: tuple[str, ...] = ("claude-opus-5-5", "claude-opus-5")
CLAUDE_EFFORT = "medium"
FALLBACK_EFFORT = "medium"


@dataclass(frozen=True)
class WorkerModel:
    engine: str
    tier: str
    model: str
    effort: str
    # Models to try, in order, if `model` is rejected at spawn time. All
    # run at FALLBACK_EFFORT.
    fallbacks: tuple[str, ...] = ()


def is_tier(value: str) -> bool:
    return value in TIERS


def escalate(tier: str, prior_attempts: int) -> str:
    """Bump `tier` one step per prior dispatch, capped at the top."""
    if tier not in TIERS:
        tier = DEFAULT_TIER
    steps = max(0, int(prior_attempts))
    idx = min(TIERS.index(tier) + steps, len(TIERS) - 1)
    return TIERS[idx]


def resolve_worker_model(
    coord_engine: str,
    tier: str,
    unavailable: Iterable[str] = (),
) -> WorkerModel:
    """Map (engine, tier) to model/effort plus the fallback ladder.

    `unavailable` removes models from consideration; the first survivor
    of tier-model -> fallbacks becomes `model`. An exhausted ladder
    returns model="" (the coord inherits its own model).
    """
    if tier not in TIERS:
        tier = DEFAULT_TIER
    gone = set(unavailable)
    if coord_engine == ENGINE_CODEX:
        ladder = [(CODEX_TIER_MODELS[tier], CODEX_TIER_EFFORT[tier])]
        ladder += [(m, FALLBACK_EFFORT) for m in CODEX_FALLBACK]
    else:
        ladder = [(m, CLAUDE_EFFORT) for m in CLAUDE_MODELS]
    live = [(m, e) for m, e in ladder if m not in gone]
    if not live:
        return WorkerModel(engine=coord_engine, tier=tier, model="", effort="")
    model, effort = live[0]
    return WorkerModel(
        engine=coord_engine,
        tier=tier,
        model=model,
        effort=effort,
        fallbacks=tuple(m for m, _ in live[1:]),
    )


# ---------- heuristic tiering ----------

_COMPLEX_WORDS = re.compile(
    r"\b(refactor|migrat\w*|redesign|architect\w*|concurren\w*|race|"
    r"protocol|schema|lock\w*|atomic\w*|crash|recover\w*|handoff|"
    r"cross-cutting|end-to-end|multi-\w+)\b",
    re.IGNORECASE,
)
_SIMPLE_WORDS = re.compile(
    r"\b(typo|rename|bump|comment|docstring|doc[s]?[- ]only|wording|"
    r"lint|gofmt|flag|log line|help text|one-liner|trivial)\b",
    re.IGNORECASE,
)
_TABLE_ROW = re.compile(r"^\s*\|\s*S\d+\b", re.MULTILINE)
_PLAN_DOC = re.compile(r"docs/[\w./-]*(PLAN|DESIGN)[\w./-]*\.md", re.IGNORECASE)


def infer_tier(task: parse.Task) -> str:
    """Score the task text and pick a tier. Deterministic, pure."""
    text = f"{task.spec}\n{task.acceptance}"
    words = len(text.split())
    rows = len(_TABLE_ROW.findall(text))
    score = 0
    if words > 600:
        score += 2
    elif words > 250:
        score += 1
    if rows > 12:
        score += 2
    elif rows > 6:
        score += 1
    if _PLAN_DOC.search(text):
        score += 1
    if len(task.depends_on) >= 2:
        score += 1
    if task.priority == "P0":
        score += 1
    if _COMPLEX_WORDS.search(text):
        score += 1
    if words < 120 and rows <= 2 and _SIMPLE_WORDS.search(text):
        score -= 2
    if score >= 3:
        return TIER_COMPLEX
    if score < 0:
        return TIER_SIMPLE
    return TIER_CODING


def classify_task(task: parse.Task) -> str:
    """Explicit `complexity:` bullet wins; otherwise infer."""
    if is_tier(task.complexity):
        return task.complexity
    return infer_tier(task)


def resolve_for_task(
    coord_engine: str,
    task: parse.Task,
    *,
    unavailable: Iterable[str] = (),
    prior_attempts: int | None = None,
) -> WorkerModel:
    """classify -> escalate by prior dispatches -> resolve.

    prior_attempts defaults to the task row's dispatch_generation (the
    number of dispatches already made; 0 on a first dispatch).
    """
    attempts = task.dispatch_generation if prior_attempts is None else prior_attempts
    tier = escalate(classify_task(task), attempts)
    return resolve_worker_model(coord_engine, tier, unavailable)
