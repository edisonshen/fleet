"""Design 3 PR4/4 (loop-binder-shellout-d6a1) — the coordinator tick
resolves its repo binding by shelling out to the SINGLE Go binder
(`fleet project resolve-repo --project <p>`), not by reimplementing a
Python tier ladder.

Background (DESIGN-coord-repo-binding-from-project.md, Option C): repo
binding is a property of the PROJECT, owned by Go. The launch cwd is
NEVER a binding tier. The pre-Design-3 Python ladder — its own meta.json
read, a coord-config::repo tier, and an os.getcwd() fallback — recreated
the exact split-authority drift Go now validates one way and Python
validated another. PR4 DELETES that ladder.

This file covers:
  - P1..P7: the new binder shell-out (success / refuse / FLEET_HOME /
    no --persist / bounded timeout / no homegrown ladder remains /
    heuristic helpers retained).
  - The fuzzy origin/basename heuristic (`remote_matches_project`) and
    the coord-config writer (`write_repo_idempotent`) RETAINED by PR4 —
    their behavior is unchanged, so their unit coverage stays.
"""
from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

import coord_config
import loop


# ---------- shared tick scaffolding ----------


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
    """Stub remote_control.bootstrap_remote_control — it shells out."""
    import remote_control
    monkeypatch.setattr(
        remote_control, "bootstrap_remote_control",
        lambda *a, **kw: remote_control.STATUS_OK,
    )


def _spy_tick_locked(monkeypatch):
    """Replace _tick_locked with a spy that records the cwd it was
    handed (positional arg 4) and still runs the real implementation."""
    seen_cwd: list[str] = []
    real = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)
    return seen_cwd


def _fake_resolve(path: str | None, hint: str = ""):
    """Build a stub for the loop._resolve_repo_fn seam.

    Returns (resolved_path, hint) — path None signals refusal."""
    calls: list[dict] = []

    def stub(project, *, home, fleet_bin="fleet", cwd=None):
        calls.append(
            {
                "project": project,
                "home": str(home),
                "fleet_bin": fleet_bin,
                "cwd": cwd,
            }
        )
        return (path, hint)

    stub.calls = calls  # type: ignore[attr-defined]
    return stub


# ---------- P1: tick resolves via the binder ----------


def test_p1_tick_uses_binder_resolved_path(tmp_path: Path, monkeypatch) -> None:
    """The binder returns a checkout path on stdout (rc 0); the tick uses
    it as the bound repo and runs NO Python-side meta/derive/cwd path."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "rainier-checkout"
    repo.mkdir()
    _patch_bootstrap(monkeypatch)

    stub = _fake_resolve(str(repo))
    monkeypatch.setattr(loop, "_resolve_repo_fn", stub)
    seen_cwd = _spy_tick_locked(monkeypatch)

    # cwd is deliberately WRONG (the operator's shell cwd). It must be
    # ignored — only the binder's answer matters.
    wrong_cwd = str(tmp_path / "fleet-checkout")
    os.makedirs(wrong_cwd, exist_ok=True)
    result = loop.tick(project, coord_id="", cwd=wrong_cwd, fleet_home=str(home))

    assert result.skipped is False, f"skipped: {result.reason}"
    assert seen_cwd and seen_cwd[0] == str(repo), (
        f"tick used cwd={seen_cwd!r}; want the binder-resolved {str(repo)!r}"
    )
    assert stub.calls and stub.calls[0]["project"] == project


# ---------- P2: tick refuses on non-zero exit ----------


def test_p2_tick_refuses_on_binder_failure(tmp_path: Path, monkeypatch) -> None:
    """rc != 0 → the tick refuses with the binder's stderr hint, sets a
    reason code, releases the coordinator lock, and skips the tick."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    _patch_bootstrap(monkeypatch)

    hint = (
        "project projects-rainier: no usable checkout — cannot attach a "
        "coordinator.\nRun: fleet project add --project projects-rainier <cwd>"
    )
    monkeypatch.setattr(
        loop, "_resolve_repo_fn", _fake_resolve(None, hint)
    )

    called = {"n": 0}
    real = loop._tick_locked

    def spy(*args, **kwargs):
        called["n"] += 1
        return real(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(project, coord_id="", cwd=str(tmp_path), fleet_home=str(home))

    assert called["n"] == 0, "refuse must short-circuit BEFORE _tick_locked"
    assert result.skipped is True
    assert result.reason == "repo-unresolved", (
        f"expected repo-unresolved reason; got {result.reason!r}"
    )
    assert result.raised >= 1
    assert any(hint in e for e in result.errors), (
        f"expected the binder hint surfaced in errors; got {result.errors}"
    )

    # Lock must be released (not held): a fresh tick on the same project
    # must be able to re-acquire it.
    lock_path = home / "projects" / project / ".locks" / "coordinator.lock"
    fd = loop._try_lock(lock_path, holder_id="other")
    assert fd is not None, "coordinator lock was NOT released after refusal"
    os.close(fd)


# ---------- P3: FLEET_HOME propagated into the subprocess env ----------


def test_p3_fleet_home_propagated_to_subprocess(tmp_path: Path, monkeypatch) -> None:
    """The resolve-repo subprocess env carries FLEET_HOME=<home arg> so a
    custom-home / test / legacy coord resolves against the right ~/.fleet.

    Asserts at the subprocess boundary (loop.subprocess.run) so it pins
    the actual env the binder receives, not just the helper's intent."""
    project = "projects-rainier"
    home = tmp_path / "custom-fleet-home"

    captured: dict = {}

    class _Proc:
        returncode = 0
        stdout = "/some/resolved/checkout\n"
        stderr = ""

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["env"] = kwargs.get("env")
        captured["timeout"] = kwargs.get("timeout")
        return _Proc()

    monkeypatch.setattr(loop.subprocess, "run", fake_run)

    path, hint = loop._resolve_repo_via_binder(project, home=home)

    assert path == "/some/resolved/checkout"
    assert hint == ""
    assert captured["env"] is not None, "subprocess must receive an explicit env"
    assert captured["env"].get("FLEET_HOME") == str(home), (
        f"FLEET_HOME not propagated; got {captured['env'].get('FLEET_HOME')!r}"
    )


# ---------- P4: routine tick does NOT pass --persist ----------


def test_p4_routine_tick_omits_persist(tmp_path: Path, monkeypatch) -> None:
    """The argv the tick builds resolves read-only: it contains
    `resolve-repo --project <p>` and NEVER `--persist` (only the
    operator-initiated attach/recovery/handoff paths persist)."""
    project = "projects-rainier"
    home = tmp_path / "fleet"

    captured: dict = {}

    class _Proc:
        returncode = 0
        stdout = "/resolved\n"
        stderr = ""

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        return _Proc()

    monkeypatch.setattr(loop.subprocess, "run", fake_run)

    loop._resolve_repo_via_binder(project, home=home, fleet_bin="fleet")

    cmd = captured["cmd"]
    assert cmd[:5] == ["fleet", "project", "resolve-repo", "--project", project], (
        f"unexpected argv: {cmd!r}"
    )
    assert "--persist" not in cmd, (
        f"routine tick must NOT persist; argv had --persist: {cmd!r}"
    )


# ---------- P5: bounded timeout — a hung binder refuses, never hangs ----------


def test_p5_binder_timeout_refuses(tmp_path: Path, monkeypatch) -> None:
    """A hanging binder hits the bounded subprocess timeout; the helper
    converts the TimeoutExpired into a refusal (None, hint) rather than
    letting the tick hang indefinitely."""
    project = "projects-rainier"
    home = tmp_path / "fleet"

    captured: dict = {}

    def fake_run(cmd, **kwargs):
        captured["timeout"] = kwargs.get("timeout")
        raise subprocess.TimeoutExpired(cmd, kwargs.get("timeout"))

    monkeypatch.setattr(loop.subprocess, "run", fake_run)

    path, hint = loop._resolve_repo_via_binder(project, home=home)

    assert path is None, "timeout must refuse (return None), not bind"
    assert "timed out" in hint, f"expected timeout hint; got {hint!r}"
    # The call must pass a bounded, positive timeout.
    assert isinstance(captured["timeout"], (int, float)) and captured["timeout"] > 0, (
        f"resolve-repo must use a bounded timeout; got {captured['timeout']!r}"
    )


def test_p5b_binder_timeout_refuses_tick_end_to_end(
    tmp_path: Path, monkeypatch,
) -> None:
    """End-to-end: a binder timeout during tick() refuses the tick (sets
    a reason, skips, releases the lock) instead of hanging."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    _patch_bootstrap(monkeypatch)

    # Restore the REAL binder fn (conftest's autouse stub would otherwise
    # short-circuit to cwd before the subprocess is ever reached).
    monkeypatch.setattr(loop, "_resolve_repo_fn", loop._resolve_repo_via_binder)

    def fake_run(cmd, **kwargs):
        raise subprocess.TimeoutExpired(cmd, kwargs.get("timeout"))

    monkeypatch.setattr(loop.subprocess, "run", fake_run)

    result = loop.tick(project, coord_id="", cwd=str(tmp_path), fleet_home=str(home))
    assert result.skipped is True
    assert result.reason == "repo-unresolved"
    assert any("timed out" in e for e in result.errors), result.errors


# ---------- P6: no homegrown ladder remains ----------


def test_p6_no_python_ladder_remains() -> None:
    """Structural guard: loop.py's repo resolution is a single shell-out
    to the binder, with NO Python-side meta.json read, worktree-derive,
    or os.getcwd() fallback for repo binding; coord_config.py has no dead
    meta/coord-config read tier."""
    loop_src = Path(loop.__file__).read_text()
    cfg_src = Path(coord_config.__file__).read_text()

    # The deleted read helpers must be gone from coord_config.py.
    assert "def read_project_repo_path" not in cfg_src, (
        "coord_config.read_project_repo_path (meta read tier) must be deleted"
    )
    assert "def read_repo" not in cfg_src, (
        "coord_config.read_repo (coord-config read tier) must be deleted"
    )

    # loop.py must not consume them anymore.
    assert "read_project_repo_path" not in loop_src, (
        "loop.py must not read meta.json::repo_path for binding"
    )
    assert "coord_config.read_repo" not in loop_src, (
        "loop.py must not read coord-config.json::repo as a binding tier"
    )

    # The single binder shell-out is present.
    assert "_resolve_repo_via_binder" in loop_src
    assert "resolve-repo" in loop_src

    # No os.getcwd() fallback in the binding-resolution region. The tick's
    # `cwd = cwd or os.getcwd()` default for worker-spawn cwd is allowed
    # (it predates binding resolution and is overwritten by the binder
    # result), but there must be no os.getcwd() inside the resolver helper.
    helper_start = loop_src.index("def _resolve_repo_via_binder")
    helper_end = loop_src.index("\ndef ", helper_start + 1)
    helper_src = loop_src[helper_start:helper_end]
    assert "getcwd" not in helper_src, (
        "the binder helper must never fall back to os.getcwd()"
    )
    # The old reason codes for the deleted ladder must be gone.
    for dead in (
        '"meta-repo-missing"',
        '"coord-config-repo-missing"',
        '"coord-config-repo-not-git"',
    ):
        assert dead not in loop_src, f"dead ladder reason {dead} still present"


# ---------- P7: heuristic helpers retained, unchanged ----------


def test_p7_heuristic_helpers_retained() -> None:
    """git_remote_origin / remote_matches_project survive (now also the
    Go heuristic's reference) and behave as before."""
    assert hasattr(coord_config, "git_remote_origin")
    assert hasattr(coord_config, "remote_matches_project")
    # Behavior parity spot-checks (a subset of the pinned suite below).
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier.git", "projects-rainier"
    )
    assert not coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet.git", "projects-rainier"
    )


# ---------- remote_matches_project: RETAINED behavior (unchanged) ----------
#
# These pin the fuzzy origin/basename heuristic PR4 keeps. The Go binder
# ports this logic for its tier-2 legacy-bridge corroboration, so the
# Python reference must not silently drift.


def test_validate_remote_matches_project_strips_prefix() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier.git", "projects-rainier"
    )


def test_validate_remote_matches_project_bare_name() -> None:
    assert coord_config.remote_matches_project(
        "git@github.com:edisonshen/fleet.git", "fleet"
    )


def test_validate_remote_mismatch() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet.git", "projects-rainier"
    )


def test_validate_remote_empty_url() -> None:
    assert not coord_config.remote_matches_project("", "projects-rainier")


def test_validate_remote_rejects_suffix_repo() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/org/rainier-app.git", "projects-rainier"
    )


def test_validate_remote_rejects_prefix_repo() -> None:
    assert not coord_config.remote_matches_project(
        "git@github.com:foo/fleet-cli.git", "projects-fleet"
    )


def test_validate_remote_rejects_projects_prefixed_lookalike() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/org/projects-rainier-app.git", "projects-rainier"
    )


def test_validate_remote_rejects_nested_path_segment_with_match() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/foo/fleet/cli.git", "projects-fleet"
    )


def test_validate_remote_rejects_underscore_suffix() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/org/rainier_app.git", "projects-rainier"
    )


def test_validate_remote_accepts_no_dot_git_suffix() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet", "projects-fleet"
    )


def test_validate_remote_accepts_scp_style_no_path() -> None:
    assert coord_config.remote_matches_project(
        "git@example.com:fleet.git", "projects-fleet"
    )


def test_validate_remote_accepts_repos_parent_dir_prefix() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/acme/my-project.git", "repos-my-project"
    )


def test_validate_remote_accepts_arbitrary_parent_dir_prefix() -> None:
    assert coord_config.remote_matches_project(
        "git@github.com:user/foo.git", "work-foo"
    )


def test_validate_remote_accepts_hyphenated_repo_name_under_parent() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier-app.git", "projects-rainier-app"
    )


def test_validate_remote_accepts_single_token_project() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet.git", "fleet"
    )


def test_validate_remote_rejects_hyphenated_parent_dir_via_heuristic() -> None:
    assert not coord_config.remote_matches_project(
        "git@github.com:src/my-project.git", "my-org-my-project"
    )


def test_validate_remote_rejects_iter9_suffix_lookalike() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/org/app.git", "projects-rainier-app"
    )


def test_validate_remote_rejects_iter9_suffix_lookalike_short() -> None:
    assert not coord_config.remote_matches_project(
        "https://github.com/x/d.git", "a-b-c-d"
    )


def test_validate_remote_case_insensitive_match() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/acme/MyProject.git", "repos-myproject"
    )


def test_validate_remote_case_insensitive_mixed() -> None:
    assert coord_config.remote_matches_project(
        "git@github.com:edisonshen/RaInIeR.git", "projects-rainier"
    )


def test_validate_remote_accepts_manually_named_hyphenated_project() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/foo/my-project.git", "my-project"
    )


def test_validate_remote_accepts_strict_equal_literal_repo_name() -> None:
    assert coord_config.remote_matches_project(
        "https://github.com/org/projects-rainier.git", "projects-rainier"
    )


# ---------- write_repo_idempotent: RETAINED behavior (unchanged) ----------
#
# The Python mirror of Spawn's coord-config writer. PR4 keeps it; the Go
# binder stamps coord-config.json::repo as a write-only breadcrumb, and
# these tests pin the merge/atomic-write semantics the writer guarantees.


def test_write_creates_config_with_repo_when_missing(tmp_path: Path) -> None:
    cfg = tmp_path / "coord-config.json"
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    assert json.loads(cfg.read_text()) == {"repo": "/Users/op/projects/rainier"}


def test_write_preserves_existing_parallelism(tmp_path: Path) -> None:
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 3}\n')
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    assert json.loads(cfg.read_text()) == {
        "parallelism": 3,
        "repo": "/Users/op/projects/rainier",
    }


def test_write_overwrites_existing_repo_with_respawn_cwd(tmp_path: Path) -> None:
    cfg = tmp_path / "coord-config.json"
    prev_live = tmp_path / "prev-checkout"
    prev_live.mkdir()
    cfg.write_text(json.dumps({"parallelism": 3, "repo": str(prev_live)}))
    new_cwd = tmp_path / "new-checkout"
    new_cwd.mkdir()
    coord_config.write_repo_idempotent(cfg, str(new_cwd))
    data = json.loads(cfg.read_text())
    assert data["repo"] == str(new_cwd)
    assert data["parallelism"] == 3


def test_write_overwrites_stale_existing_repo(tmp_path: Path) -> None:
    cfg = tmp_path / "coord-config.json"
    stale = tmp_path / "deleted-checkout"  # NOT created
    cfg.write_text(json.dumps({"parallelism": 3, "repo": str(stale)}))
    new_repo = tmp_path / "new-checkout"
    new_repo.mkdir()
    coord_config.write_repo_idempotent(cfg, str(new_repo))
    data = json.loads(cfg.read_text())
    assert data["repo"] == str(new_repo)
    assert data["parallelism"] == 3


def test_write_overwrites_empty_repo(tmp_path: Path) -> None:
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 3, "repo": ""}\n')
    coord_config.write_repo_idempotent(cfg, "/Users/op/projects/rainier")
    assert json.loads(cfg.read_text())["repo"] == "/Users/op/projects/rainier"
