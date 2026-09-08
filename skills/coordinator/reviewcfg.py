"""Pure reviewer slot resolver.

Availability is an input, per the gc_test.go:412 lesson: this module does
not probe commands or read process environment state.

Slots mirror the dominant engine (the one the coord was launched with):

    beta  = dominant engine's anchor model      -- must pass
    alpha = helper engine (the other one)       -- may be skipped when
            unavailable / rate-limited
          = dominant engine again when the helper is not installed:
            a second distinct model when the dominant engine has one,
            otherwise the same model as beta (single_engine_only).
"""
from __future__ import annotations

from dataclasses import dataclass


ENGINE_CLAUDE_CODE = "claude-code"
ENGINE_CODEX = "codex"

# review_slot.py --engine values (distinct from the coord engine names).
SLOT_CLAUDE = "claude"
SLOT_CODEX = "codex"

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
    # True when alpha re-uses beta's engine AND model because neither the
    # helper engine nor a second dominant-engine model was available.
    single_engine_only: bool
    # True when alpha runs the helper engine (the slot the reviewer may
    # record as `skipped` with a reason). False when alpha is the dominant
    # engine (must pass, or single_engine_only).
    alpha_is_helper: bool


def slot_engine(coord_engine: str) -> str:
    """Map a coord engine name to its review_slot.py --engine value."""
    return SLOT_CODEX if coord_engine == ENGINE_CODEX else SLOT_CLAUDE


def helper_engine(coord_engine: str) -> str:
    return ENGINE_CLAUDE_CODE if coord_engine == ENGINE_CODEX else ENGINE_CODEX


def _first_available(models: list[str], unavailable: set[str]) -> str | None:
    for model in models:
        if model not in unavailable:
            return model
    return None


def _claude_anchor(unavailable: set[str]) -> Slot:
    model = _first_available(OPUS_FALLBACK, unavailable) or OPUS_FALLBACK[-1]
    return Slot(SLOT_CLAUDE, model, "high")


def resolve_slots(
    dominant: str,
    has_helper: bool,
    is_git: bool,
    unavailable: set[str],
) -> Resolution:
    """Resolve the two reviewer slots for a coord running `dominant`.

    dominant:   coord engine ("claude-code" | "codex"). Unknown values
                resolve as claude-code (the pre-mirroring default).
    has_helper: the OTHER engine's CLI is installed on this host.
    is_git:     project is git-mode. `codex review` needs a diff base, so
                a codex slot is only offered as the helper on git projects;
                as the dominant engine it runs regardless (review_slot.py
                falls back to a raw structured `codex exec` review).
    """
    unavailable = set(unavailable or ())

    if dominant == ENGINE_CODEX:
        beta = Slot(SLOT_CODEX, CODEX_DEFAULT_MODEL, "high")
        if has_helper:
            claude = _claude_anchor(unavailable)
            if claude.model not in unavailable:
                return Resolution(
                    alpha=claude, beta=beta,
                    single_engine_only=False, alpha_is_helper=True,
                )
        # Codex has a single reviewer model in Fleet's config, so a
        # codex-only host always degrades to one distinct reviewer.
        return Resolution(
            alpha=beta, beta=beta, single_engine_only=True, alpha_is_helper=False,
        )

    beta = _claude_anchor(unavailable)
    use_codex = has_helper and is_git and CODEX_DEFAULT_MODEL not in unavailable
    if use_codex:
        return Resolution(
            alpha=Slot(SLOT_CODEX, CODEX_DEFAULT_MODEL, "high"),
            beta=beta,
            single_engine_only=False,
            alpha_is_helper=True,
        )

    sonnet_model = _first_available(SONNET_FALLBACK, unavailable)
    if sonnet_model is not None and sonnet_model != beta.model:
        return Resolution(
            alpha=Slot(SLOT_CLAUDE, sonnet_model, "high"),
            beta=beta,
            single_engine_only=False,
            alpha_is_helper=False,
        )

    return Resolution(
        alpha=Slot(SLOT_CLAUDE, beta.model, "high"),
        beta=beta,
        single_engine_only=True,
        alpha_is_helper=False,
    )
