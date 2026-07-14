"""Pure reviewer slot resolver.

Availability is an input, per the gc_test.go:412 lesson: this module does
not probe commands or read process environment state.
"""
from __future__ import annotations

from dataclasses import dataclass


CODEX_DEFAULT_MODEL = "gpt-5.5-codex"
OPUS_FALLBACK = ["claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-5"]
SONNET_FALLBACK = ["claude-sonnet-5", "claude-sonnet-4-5"]


@dataclass(frozen=True)
class Slot:
    engine: str
    model: str
    effort: str = "high"


@dataclass(frozen=True)
class Resolution:
    alpha: Slot
    beta: Slot
    single_claude_only: bool


def _first_available(models: list[str], unavailable: set[str]) -> str | None:
    for model in models:
        if model not in unavailable:
            return model
    return None


def resolve_slots(
    has_codex: bool, is_git: bool, unavailable: set[str]
) -> Resolution:
    unavailable = set(unavailable or ())

    beta_model = _first_available(OPUS_FALLBACK, unavailable) or OPUS_FALLBACK[-1]
    beta = Slot("claude", beta_model, "high")

    use_codex = (
        has_codex
        and is_git
        and CODEX_DEFAULT_MODEL not in unavailable
    )
    if use_codex:
        return Resolution(
            alpha=Slot("codex", CODEX_DEFAULT_MODEL, "high"),
            beta=beta,
            single_claude_only=False,
        )

    sonnet_model = _first_available(SONNET_FALLBACK, unavailable)
    if sonnet_model is not None and sonnet_model != beta_model:
        return Resolution(
            alpha=Slot("claude", sonnet_model, "high"),
            beta=beta,
            single_claude_only=False,
        )

    return Resolution(
        alpha=Slot("claude", beta_model, "high"),
        beta=beta,
        single_claude_only=True,
    )
