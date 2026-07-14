from __future__ import annotations

from pathlib import Path

import pytest

import reviewcfg


@pytest.mark.parametrize(
    (
        "has_codex",
        "is_git",
        "unavailable",
        "alpha_engine",
        "alpha_model",
        "beta_model",
        "single_claude_only",
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
        ),
        (
            True,
            False,
            set(),
            "claude",
            reviewcfg.SONNET_FALLBACK[0],
            reviewcfg.OPUS_FALLBACK[0],
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
        ),
        (
            True,
            True,
            {reviewcfg.OPUS_FALLBACK[0]},
            "codex",
            reviewcfg.CODEX_DEFAULT_MODEL,
            reviewcfg.OPUS_FALLBACK[1],
            False,
        ),
        (
            True,
            True,
            {reviewcfg.CODEX_DEFAULT_MODEL},
            "claude",
            reviewcfg.SONNET_FALLBACK[0],
            reviewcfg.OPUS_FALLBACK[0],
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
        ),
    ],
)
def test_resolve_slots_matrix(
    has_codex: bool,
    is_git: bool,
    unavailable: set[str],
    alpha_engine: str,
    alpha_model: str,
    beta_model: str,
    single_claude_only: bool,
) -> None:
    resolution = reviewcfg.resolve_slots(has_codex, is_git, unavailable)

    assert resolution.alpha.engine == alpha_engine
    assert resolution.alpha.model == alpha_model
    assert resolution.alpha.effort == "high"
    assert resolution.beta.engine == "claude"
    assert resolution.beta.model == beta_model
    assert resolution.beta.effort == "high"
    assert resolution.single_claude_only is single_claude_only


def test_resolve_slots_is_deterministic() -> None:
    unavailable = {reviewcfg.OPUS_FALLBACK[0], reviewcfg.CODEX_DEFAULT_MODEL}

    first = reviewcfg.resolve_slots(True, True, unavailable)
    second = reviewcfg.resolve_slots(True, True, unavailable)

    assert first == second


def test_reviewcfg_source_has_no_subprocess_or_env_reads() -> None:
    source = Path(reviewcfg.__file__).read_text()

    assert "import subprocess" not in source
    assert "os.environ" not in source
