"""Issue #175 — coord skill must derive worktree base from the project's
recorded repo path, NOT from os.getcwd().

Repro:
  - Coord spawned for `projects-rainier` with cwd inside `projects-fleet`.
  - `loop.tick("projects-rainier")` runs `git worktree add` against the
    fleet repo because `cwd = cwd or os.getcwd()` (loop.py around line 151)
    silently picks up the wrong repo.
  - Worktree lands in fleet's checkout, branch `worker/<slug>` is created
    in fleet's tree, worker references rainier task but points at fleet
    source.

Fix surface:
  - `~/.fleet/projects/<p>/coord-config.json` gains a `repo` field
    (absolute path to the project's git checkout).
  - `loop.tick()` reads it at tick start. Use as cwd for `git worktree add`.
  - Fallback to `os.getcwd()` ONLY when missing. Emit warning into
    `TickResult.errors`.
  - Validate via `git -C <repo> remote get-url origin`. Heuristic: strip
    the `projects-` prefix from the project name and check the remote URL
    contains the bare name. Mismatch → BLOCKED (no dispatch, error in
    TickResult, raised++).

These tests exercise the coord_config + loop integration. Each test
short-circuits the tick at the parse step (no tasks.md → ParseError) so
we only assert the repo-resolution path, not the full tick machinery.
"""
from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path
from unittest.mock import patch

import pytest

import coord_config
import loop


# ---------- coord_config.py: read/write helpers ----------


def test_read_returns_none_when_file_missing(tmp_path: Path) -> None:
    """Missing coord-config.json → read returns None (caller falls back)."""
    assert coord_config.read_repo(tmp_path / "coord-config.json") is None


def test_read_returns_none_when_field_missing(tmp_path: Path) -> None:
    """coord-config.json without `repo` field → None."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 3}\n')
    assert coord_config.read_repo(cfg) is None


def test_read_returns_none_when_field_empty(tmp_path: Path) -> None:
    """Empty/whitespace-only `repo` value → None."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"repo": "   "}\n')
    assert coord_config.read_repo(cfg) is None


def test_read_returns_stripped_repo_path(tmp_path: Path) -> None:
    """Valid `repo` value → returned with whitespace stripped."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 2, "repo": "/Users/op/projects/rainier"}\n')
    assert coord_config.read_repo(cfg) == "/Users/op/projects/rainier"


def test_read_handles_malformed_json(tmp_path: Path) -> None:
    """Malformed JSON → None (don't crash the tick)."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text("not json\n")
    assert coord_config.read_repo(cfg) is None


def test_validate_remote_matches_project_strips_prefix() -> None:
    """`projects-rainier` matches `github.com/edisonshen/rainier.git`."""
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier.git",
        "projects-rainier",
    )


def test_validate_remote_matches_project_bare_name() -> None:
    """Project name without `projects-` prefix matches bare URL."""
    assert coord_config.remote_matches_project(
        "git@github.com:edisonshen/fleet.git",
        "fleet",
    )


def test_validate_remote_mismatch() -> None:
    """Repo points at fleet but project is rainier → no match."""
    assert not coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet.git",
        "projects-rainier",
    )


def test_validate_remote_empty_url() -> None:
    """Empty remote URL → no match (don't false-positive on '')."""
    assert not coord_config.remote_matches_project("", "projects-rainier")


# ---------- coord_config.py: idempotent write ----------


def test_write_creates_config_with_repo_when_missing(tmp_path: Path) -> None:
    """write_repo_idempotent on missing file → creates {"repo": "..."}."""
    cfg = tmp_path / "coord-config.json"
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    data = json.loads(cfg.read_text())
    assert data == {"repo": "/Users/op/projects/rainier"}


def test_write_preserves_existing_parallelism(tmp_path: Path) -> None:
    """write on existing config with parallelism but no repo → merges."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 3}\n')
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    data = json.loads(cfg.read_text())
    assert data == {"parallelism": 3, "repo": "/Users/op/projects/rainier"}


def test_write_idempotent_preserves_existing_repo(tmp_path: Path) -> None:
    """existing non-empty repo → NOT overwritten (operator-set wins)."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text(
        '{"parallelism": 3, "repo": "/Users/op/projects/rainier-fork"}\n'
    )
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    data = json.loads(cfg.read_text())
    assert data["repo"] == "/Users/op/projects/rainier-fork"


def test_write_overwrites_empty_repo(tmp_path: Path) -> None:
    """existing empty `repo` field → overwrites (treat as unset)."""
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 3, "repo": ""}\n')
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    data = json.loads(cfg.read_text())
    assert data["repo"] == "/Users/op/projects/rainier"


# ---------- loop.tick: integration ----------


def _seed_fleet_home(tmp: Path, project: str) -> Path:
    """Create the minimal ~/.fleet skeleton tick() needs."""
    home = tmp / "fleet"
    (home / "inbox").mkdir(parents=True)
    (home / "agents").mkdir(parents=True)
    proj_dir = home / "projects" / project
    proj_dir.mkdir(parents=True)
    # Empty tasks.md so parse.read succeeds + tick falls through cleanly.
    (proj_dir / "tasks.md").write_text("# Tasks\n")
    return home


def _patch_bootstrap(monkeypatch):
    """Stub remote_control.bootstrap_remote_control — it shells out
    otherwise."""
    import remote_control
    monkeypatch.setattr(
        remote_control, "bootstrap_remote_control",
        lambda *a, **kw: remote_control.STATUS_OK,
    )


def test_tick_reads_repo_from_coord_config(
    tmp_path: Path, monkeypatch,
) -> None:
    """coord-config.json::repo set → tick uses it as cwd, not os.getcwd().

    Verified via the resolved-cwd path: we patch _tick_locked-internal
    consumers to assert the cwd we pass downstream is the configured
    repo, not the test's getcwd().
    """
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "rainier-checkout"
    repo.mkdir()
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(
        json.dumps({"parallelism": 1, "repo": str(repo)}) + "\n"
    )
    _patch_bootstrap(monkeypatch)
    # Stub the remote-validation shell-out — return matching origin.
    monkeypatch.setattr(
        coord_config, "git_remote_origin",
        lambda repo_path: f"git@github.com:edisonshen/rainier.git",
    )

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        # cwd is positional arg 4 of _tick_locked
        # (result, project, project_dir, coord_id, cwd, cap, ...)
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    # cwd= deliberately wrong (the operator's shell cwd).
    wrong_cwd = str(tmp_path / "fleet-checkout")
    os.makedirs(wrong_cwd, exist_ok=True)
    result = loop.tick(
        project, coord_id="", cwd=wrong_cwd, fleet_home=str(home),
    )
    assert result.skipped is False, f"skipped: {result.reason}"
    assert seen_cwd, "expected _tick_locked to be called once"
    assert seen_cwd[0] == str(repo), (
        f"tick used cwd={seen_cwd[0]!r}; want {str(repo)!r} from coord-config.json::repo"
    )


def test_tick_missing_repo_falls_back_with_warning(
    tmp_path: Path, monkeypatch,
) -> None:
    """coord-config.json missing or no `repo` field → fallback to caller cwd
    + warning surfaced via TickResult.errors."""
    project = "projects-fleet"
    home = _seed_fleet_home(tmp_path, project)
    # No coord-config.json on disk.
    _patch_bootstrap(monkeypatch)

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    fallback_cwd = str(tmp_path / "fleet-checkout")
    os.makedirs(fallback_cwd, exist_ok=True)
    result = loop.tick(
        project, coord_id="", cwd=fallback_cwd, fleet_home=str(home),
    )
    assert result.skipped is False, f"skipped: {result.reason}"
    assert seen_cwd and seen_cwd[0] == fallback_cwd, (
        f"fallback should use caller cwd; got {seen_cwd}"
    )
    # Warning should appear in TickResult.errors.
    assert any(
        "coord-config.json" in e and "repo" in e for e in result.errors
    ), f"expected fallback warning in errors; got {result.errors}"


def test_tick_remote_mismatch_refuses_dispatch(
    tmp_path: Path, monkeypatch,
) -> None:
    """coord-config.json::repo set, but `git remote get-url origin` doesn't
    match the project name → skipped, error surfaced, raised++."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "fleet-checkout"  # WRONG repo for rainier
    repo.mkdir()
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(
        json.dumps({"repo": str(repo)}) + "\n"
    )
    _patch_bootstrap(monkeypatch)
    # Remote points at fleet, not rainier.
    monkeypatch.setattr(
        coord_config, "git_remote_origin",
        lambda repo_path: "git@github.com:edisonshen/fleet.git",
    )

    # If tick proceeded past the guard, _tick_locked would run.
    called = {"n": 0}
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        called["n"] += 1
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path),
        fleet_home=str(home),
    )
    assert called["n"] == 0, (
        "remote-mismatch must short-circuit BEFORE _tick_locked runs"
    )
    assert result.skipped is True
    assert result.reason == "coord-config-repo-mismatch"
    assert result.raised >= 1
    assert any(
        "remote" in e.lower() and project in e for e in result.errors
    ), f"expected mismatch error mentioning project; got {result.errors}"


def test_remote_match_heuristic_strips_projects_prefix(
    tmp_path: Path, monkeypatch,
) -> None:
    """Project name `projects-rainier` must match a remote URL containing
    bare `rainier` (the `projects-` prefix is a fleet bookkeeping convention,
    not part of the github org/repo path)."""
    # Direct unit test of the heuristic.
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier.git", "projects-rainier"
    )
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier.git", "rainier"
    )
    # Negative: project=rainier should NOT match a fleet remote.
    assert not coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet.git", "projects-rainier"
    )
