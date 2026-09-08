from __future__ import annotations

from pathlib import Path

import pytest

import reviewcfg


@pytest.mark.parametrize(
    (
        "has_helper",
        "is_git",
        "unavailable",
        "alpha_engine",
        "alpha_model",
        "beta_model",
        "single_engine_only",
        "alpha_is_helper",
    ),
    [
        (
            True,
            True,
            set(),
            "codex",
            reviewcfg.CODEX_DEFAULT_MODEL,
            reviewcfg.OPUS_FALLBACK[0],
            False,
            True,
        ),
        (
            True,
            False,
            set(),
            "claude",
            reviewcfg.SONNET_FALLBACK[0],
            reviewcfg.OPUS_FALLBACK[0],
            False,
            False,
        ),
        (
            False,
            True,
            set(),
            "claude",
            reviewcfg.SONNET_FALLBACK[0],
            reviewcfg.OPUS_FALLBACK[0],
            False,
            False,
        ),
        (
            True,
            True,
            {reviewcfg.OPUS_FALLBACK[0]},
            "codex",
            reviewcfg.CODEX_DEFAULT_MODEL,
            reviewcfg.OPUS_FALLBACK[1],
            False,
            True,
        ),
        (
            True,
            True,
            {reviewcfg.CODEX_DEFAULT_MODEL},
            "claude",
            reviewcfg.SONNET_FALLBACK[0],
            reviewcfg.OPUS_FALLBACK[0],
            False,
            False,
        ),
        (
            False,
            True,
            set(reviewcfg.SONNET_FALLBACK),
            "claude",
            reviewcfg.OPUS_FALLBACK[0],
            reviewcfg.OPUS_FALLBACK[0],
            True,
            False,
        ),
    ],
)
def test_resolve_slots_claude_dominant_matrix(
    has_helper: bool,
    is_git: bool,
    unavailable: set[str],
    alpha_engine: str,
    alpha_model: str,
    beta_model: str,
    single_engine_only: bool,
    alpha_is_helper: bool,
) -> None:
    resolution = reviewcfg.resolve_slots("claude-code", has_helper, is_git, unavailable)

    assert resolution.alpha.engine == alpha_engine
    assert resolution.alpha.model == alpha_model
    assert resolution.alpha.effort == "high"
    assert resolution.beta.engine == "claude"
    assert resolution.beta.model == beta_model
    assert resolution.beta.effort == "high"
    assert resolution.single_engine_only is single_engine_only
    assert resolution.alpha_is_helper is alpha_is_helper


@pytest.mark.parametrize(
    ("has_helper", "is_git", "unavailable", "alpha_engine", "alpha_model", "single_engine_only", "alpha_is_helper"),
    [
        # Both installed: claude is the optional helper on alpha.
        (True, True, set(), "claude", reviewcfg.OPUS_FALLBACK[0], False, True),
        # Non-git does not disqualify the claude helper (only codex-as-helper
        # needs a git diff base).
        (True, False, set(), "claude", reviewcfg.OPUS_FALLBACK[0], False, True),
        # Helper model fallback.
        (True, True, {reviewcfg.OPUS_FALLBACK[0]}, "claude", reviewcfg.OPUS_FALLBACK[1], False, True),
        # Codex-only host: degrade to one codex reviewer, never touch claude.
        (False, True, set(), "codex", reviewcfg.CODEX_DEFAULT_MODEL, True, False),
        (False, False, set(), "codex", reviewcfg.CODEX_DEFAULT_MODEL, True, False),
        # Helper installed but every claude model is unavailable.
        (True, True, set(reviewcfg.OPUS_FALLBACK), "codex", reviewcfg.CODEX_DEFAULT_MODEL, True, False),
    ],
)
def test_resolve_slots_codex_dominant_matrix(
    has_helper: bool,
    is_git: bool,
    unavailable: set[str],
    alpha_engine: str,
    alpha_model: str,
    single_engine_only: bool,
    alpha_is_helper: bool,
) -> None:
    resolution = reviewcfg.resolve_slots("codex", has_helper, is_git, unavailable)

    assert resolution.beta.engine == "codex"
    assert resolution.beta.model == reviewcfg.CODEX_DEFAULT_MODEL
    assert resolution.alpha.engine == alpha_engine
    assert resolution.alpha.model == alpha_model
    assert resolution.single_engine_only is single_engine_only
    assert resolution.alpha_is_helper is alpha_is_helper


def test_unknown_dominant_resolves_as_claude() -> None:
    assert reviewcfg.resolve_slots("", True, True, set()) == reviewcfg.resolve_slots(
        "claude-code", True, True, set()
    )


def test_slot_engine_and_helper_engine() -> None:
    assert reviewcfg.slot_engine("codex") == "codex"
    assert reviewcfg.slot_engine("claude-code") == "claude"
    assert reviewcfg.slot_engine("") == "claude"
    assert reviewcfg.helper_engine("codex") == "claude-code"
    assert reviewcfg.helper_engine("claude-code") == "codex"


def test_resolve_slots_is_deterministic() -> None:
    unavailable = {reviewcfg.OPUS_FALLBACK[0], reviewcfg.CODEX_DEFAULT_MODEL}

    first = reviewcfg.resolve_slots("claude-code", True, True, unavailable)
    second = reviewcfg.resolve_slots("claude-code", True, True, unavailable)

    assert first == second


def test_reviewcfg_source_has_no_subprocess_or_env_reads() -> None:
    source = Path(reviewcfg.__file__).read_text()

    assert "import subprocess" not in source
    assert "os.environ" not in source
