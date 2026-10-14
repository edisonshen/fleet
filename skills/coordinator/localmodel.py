"""Review model the user set in their own engine config, if any.

codex   model in $CODEX_HOME/config.toml (default ~/.codex), the active
        profile's model first
claude  ANTHROPIC_MODEL, else model in $CLAUDE_CONFIG_DIR/settings.json
        (default ~/.claude)

Keys are review_slot.py --engine values.
"""
from __future__ import annotations

import json
import os
import tomllib


def codex_config_model() -> str | None:
    cfg_dir = os.environ.get("CODEX_HOME") or os.path.join(os.path.expanduser("~"), ".codex")
    try:
        with open(os.path.join(cfg_dir, "config.toml"), "rb") as fh:
            data = tomllib.load(fh)
    except (OSError, tomllib.TOMLDecodeError):
        return None
    profile = data.get("profile")
    profiles = data.get("profiles")
    if isinstance(profile, str) and isinstance(profiles, dict):
        table = profiles.get(profile)
        if isinstance(table, dict) and isinstance(table.get("model"), str) and table["model"]:
            return table["model"]
    model = data.get("model")
    return model if isinstance(model, str) and model else None


def claude_config_model() -> str | None:
    if os.environ.get("ANTHROPIC_MODEL"):
        return os.environ["ANTHROPIC_MODEL"]
    cfg_dir = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.join(os.path.expanduser("~"), ".claude")
    try:
        with open(os.path.join(cfg_dir, "settings.json"), encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, ValueError):
        return None
    model = data.get("model") if isinstance(data, dict) else None
    return model if isinstance(model, str) and model else None


def local_config_model(engine: str) -> str | None:
    return codex_config_model() if engine == "codex" else claude_config_model()


def configured_models() -> dict[str, str]:
    models = {"codex": codex_config_model(), "claude": claude_config_model()}
    return {engine: model for engine, model in models.items() if model}
