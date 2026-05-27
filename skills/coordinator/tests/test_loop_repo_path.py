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
    """Empty remote URL → no match (don't false-positive on '').

    NOTE: this returns False, but loop.tick's CALLER treats empty
    origin as a 'cannot validate' soft path (local-only repo support),
    NOT as a mismatch. The function itself stays honest about
    empty input. See test_tick_local_only_repo_no_origin_warns_but_proceeds."""
    assert not coord_config.remote_matches_project("", "projects-rainier")


# ---------- coord_config.remote_matches_project: false-positive guards ----------
#
# review iter-3 regression suite: the original delimited-substring
# heuristic false-positive-matched `projects-rainier` against any URL
# whose repo segment merely *contained* `rainier` bordered by `-`/`_`/
# `:`/`/` (e.g., `rainier-app.git`, `projects-rainier-app.git`). That
# defeats issue #175's safety goal — the whole point of the remote
# check is to refuse dispatch when the configured checkout doesn't
# belong to the project. Pin the strict segment-equality behavior so
# the looser heuristic can't slip back in via a future "be more
# permissive" refactor.


def test_validate_remote_rejects_suffix_repo() -> None:
    """`projects-rainier` must NOT match a `rainier-app.git` remote.

    Repro: operator stamps coord-config.json::repo with a checkout
    whose origin points at a sibling repo sharing the prefix. The
    original delimited-substring heuristic accepted this because
    `-` was a valid right-delimiter; the strict segment match rejects
    it (`tail=rainier-app` != `bare=rainier`).
    """
    assert not coord_config.remote_matches_project(
        "https://github.com/org/rainier-app.git",
        "projects-rainier",
    )


def test_validate_remote_rejects_prefix_repo() -> None:
    """`projects-fleet` must NOT match a `fleet-cli.git` remote."""
    assert not coord_config.remote_matches_project(
        "git@github.com:foo/fleet-cli.git",
        "projects-fleet",
    )


def test_validate_remote_rejects_projects_prefixed_lookalike() -> None:
    """`projects-rainier` must NOT match `projects-rainier-app.git`."""
    assert not coord_config.remote_matches_project(
        "https://github.com/org/projects-rainier-app.git",
        "projects-rainier",
    )


def test_validate_remote_rejects_nested_path_segment_with_match() -> None:
    """`projects-fleet` must NOT match a URL with `fleet` as an
    intermediate path component (e.g., `https://host/fleet/cli.git`)."""
    # Only the BASENAME (after the final `/`) is compared. A repo named
    # `cli` whose path happens to traverse `fleet/` is not a fleet repo.
    assert not coord_config.remote_matches_project(
        "https://github.com/foo/fleet/cli.git",
        "projects-fleet",
    )


def test_validate_remote_rejects_underscore_suffix() -> None:
    """Underscore-delimited suffixes (`rainier_app`) must NOT match."""
    assert not coord_config.remote_matches_project(
        "https://github.com/org/rainier_app.git",
        "projects-rainier",
    )


def test_validate_remote_accepts_no_dot_git_suffix() -> None:
    """Remotes without `.git` suffix (`https://host/org/fleet`) must
    still match — `git remote get-url` returns whatever the operator
    set, which may omit `.git`."""
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet",
        "projects-fleet",
    )


def test_validate_remote_accepts_scp_style_no_path() -> None:
    """SCP-style `git@host:repo.git` (no nested org/) still parses."""
    assert coord_config.remote_matches_project(
        "git@example.com:fleet.git",
        "projects-fleet",
    )


# ---------- review iter-4 (codex P1): generic TagForPath shape ----------
#
# Fleet project tags come from internal/projects.TagForPath, which
# constructs `<parent>-<base>` for any path — NOT just `projects-<repo>`.
# A `fleet project add /repos/my-project` yields tag `repos-my-project`;
# the origin URL basename is `my-project`, so a strict `tag == basename`
# match would reject the legitimate registration. The iter-3 heuristic
# stripped only `projects-` prefix and broke this case. iter-4 strips
# the first `-` token (the parent-dir half) generically, then matches
# the repo-basename half.


def test_validate_remote_accepts_repos_parent_dir_prefix() -> None:
    """`repos-my-project` (from `fleet project add /repos/my-project`)
    must match a `my-project.git` remote — TagForPath is generic,
    not specific to `projects/`."""
    assert coord_config.remote_matches_project(
        "https://github.com/acme/my-project.git",
        "repos-my-project",
    )


def test_validate_remote_accepts_arbitrary_parent_dir_prefix() -> None:
    """Any parent-dir prefix from TagForPath works (`work-foo`, etc.)."""
    assert coord_config.remote_matches_project(
        "git@github.com:user/foo.git",
        "work-foo",
    )


def test_validate_remote_accepts_hyphenated_repo_name_under_parent() -> None:
    """`projects-rainier-app` (project `rainier-app` under
    `~/projects/`) must match `rainier-app.git`. The basename half
    of the tag retains internal hyphens."""
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/rainier-app.git",
        "projects-rainier-app",
    )


def test_validate_remote_accepts_single_token_project() -> None:
    """A tag with no `-` at all (single-segment project) matches whole."""
    assert coord_config.remote_matches_project(
        "https://github.com/edisonshen/fleet.git",
        "fleet",
    )


def test_validate_remote_accepts_hyphenated_parent_dir() -> None:
    """Parent dirs with internal hyphens (`/src/my-org/my-project`)
    yield tag `my-org-my-project`. Origin basename `my-project` must
    still match.

    review iter-5 (codex P2 finding): the iter-4 split-first-`-`-token
    code yielded bare=`org-my-project`, which would refuse this
    legitimate registration. The suffix-equality heuristic accepts it
    correctly (tag endswith `-my-project`)."""
    assert coord_config.remote_matches_project(
        "git@github.com:src/my-project.git",
        "my-org-my-project",
    )


def test_validate_remote_accepts_double_hyphenated_parent() -> None:
    """More-than-2-segment tag (`a-b-c-d` from `/a-b/c-d`) still works."""
    assert coord_config.remote_matches_project(
        "https://github.com/x/c-d.git",
        "a-b-c-d",
    )


def test_validate_remote_empty_url_returns_false() -> None:
    """Empty remote URL → False. CALLER (loop.tick) must treat this as
    'cannot validate' (local-only repo path) rather than 'mismatch' —
    see test_tick_local_only_repo_no_origin_warns_but_proceeds below."""
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


def test_write_overwrites_existing_repo_with_respawn_cwd(tmp_path: Path) -> None:
    """iter-9 (codex P1 resolution): respawn always overwrites the
    `repo` field with the new cwd, even when the previous value
    points at a live directory.

    Rationale: for legacy / no-meta.json projects, coord-config.json::repo
    is purely the spawn-time signal. Preserving an older live value
    traps projects in the #175 wrong-repo state — operator can't fix it
    by respawning from the correct checkout. Operators wanting a
    permanent fork pin should use `fleet project add` to set
    meta.json::repo_path (which wins over coord-config in loop.tick).
    """
    cfg = tmp_path / "coord-config.json"
    prev_live = tmp_path / "prev-checkout"
    prev_live.mkdir()  # live, but operator's now respawning from elsewhere
    cfg.write_text(
        json.dumps({"parallelism": 3, "repo": str(prev_live)})
    )
    new_cwd = tmp_path / "new-checkout"
    new_cwd.mkdir()
    coord_config.write_repo_idempotent(cfg, str(new_cwd))
    data = json.loads(cfg.read_text())
    assert data["repo"] == str(new_cwd), (
        "respawn must overwrite even a live previous value"
    )
    # Sibling fields preserved:
    assert data["parallelism"] == 3


def test_write_overwrites_stale_existing_repo(tmp_path: Path) -> None:
    """iter-9 still overwrites stale paths (this was the iter-8
    contract; iter-9 generalizes it to all non-empty values)."""
    cfg = tmp_path / "coord-config.json"
    stale = tmp_path / "deleted-checkout"  # NOT created
    cfg.write_text(
        json.dumps({"parallelism": 3, "repo": str(stale)})
    )
    new_repo = tmp_path / "new-checkout"
    new_repo.mkdir()
    coord_config.write_repo_idempotent(cfg, str(new_repo))
    data = json.loads(cfg.read_text())
    assert data["repo"] == str(new_repo)
    assert data["parallelism"] == 3


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


def test_tick_remote_mismatch_warns_but_proceeds(
    tmp_path: Path, monkeypatch,
) -> None:
    """coord-config.json::repo set, but `git remote get-url origin` doesn't
    match the project name → warn in TickResult.errors, but PROCEED
    (the configured `repo` field is authoritative).

    iter-6 (codex P1 finding): the earlier "refuse if mismatch"
    behavior broke legitimate registrations where the operator
    cloned into a custom-named directory (e.g., `~/work/fleet-v2`
    cloned from `fleet.git`, or `fleet project add ~/forks/rainier-v3`
    pointing at the rainier remote). The heuristic can't distinguish
    custom-name from actual-wrong-repo, so it must not block. The
    warning surfaces in errors[] so operators see the signal."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    # Custom-named checkout: operator cloned rainier into "v3-checkout"
    # for whatever reason; origin still points at the rainier remote.
    # The heuristic-match would fail (basename "v3-checkout" vs tag
    # "projects-rainier"), but operator's intent is clear.
    repo = tmp_path / "v3-checkout"
    repo.mkdir()
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(json.dumps({"repo": str(repo)}) + "\n")
    _patch_bootstrap(monkeypatch)
    # Remote points at fleet, not rainier — would trip the heuristic.
    monkeypatch.setattr(
        coord_config, "git_remote_origin",
        lambda repo_path: "git@github.com:edisonshen/fleet.git",
    )

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path),
        fleet_home=str(home),
    )
    # MUST proceed (configured repo is authoritative):
    assert result.skipped is False, (
        f"mismatch should warn + proceed; got skipped: {result.reason!r}"
    )
    assert seen_cwd and seen_cwd[0] == str(repo), (
        f"configured repo must still be the cwd; got {seen_cwd}"
    )
    # MUST surface the heuristic-mismatch warning:
    assert any(
        "does not match" in e and project in e for e in result.errors
    ), f"expected mismatch warning mentioning project; got {result.errors}"


def test_tick_remote_mismatch_does_not_set_skip_reason(
    tmp_path: Path, monkeypatch,
) -> None:
    """Regression guard: iter-6 contract change. result.reason must NOT
    be set to the old `coord-config-repo-mismatch` value (that string
    was specific to the refuse-dispatch path which no longer fires)."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "wrong-named-checkout"
    repo.mkdir()
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(json.dumps({"repo": str(repo)}) + "\n")
    _patch_bootstrap(monkeypatch)
    monkeypatch.setattr(
        coord_config, "git_remote_origin",
        lambda repo_path: "git@github.com:edisonshen/fleet.git",
    )

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path),
        fleet_home=str(home),
    )
    assert result.reason != "coord-config-repo-mismatch", (
        f"iter-6 dropped this skip reason; got {result.reason!r}"
    )


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


def test_tick_local_only_repo_no_origin_warns_but_proceeds(
    tmp_path: Path, monkeypatch,
) -> None:
    """coord-config.json::repo set, but the checkout has NO `origin`
    remote (local-only repo from `fleet project add /path/to/local`).
    `git_remote_origin` returns "" — caller must NOT treat this as
    mismatch (which would refuse dispatch). Instead: surface a soft
    warning + proceed with the configured repo as cwd.

    review iter-4 (codex P1 finding): the iter-3 strict-equality match
    caused `git_remote_origin("") → ""` + `remote_matches_project("",
    p) → False` to falsely trip the mismatch refuse-dispatch branch.
    Local-only repos are a supported project shape; refusing them is
    a regression."""
    project = "local-only-repo"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "local-only-checkout"
    repo.mkdir()
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(json.dumps({"repo": str(repo)}) + "\n")
    _patch_bootstrap(monkeypatch)
    # Local-only repo: no origin → git_remote_origin returns "".
    monkeypatch.setattr(
        coord_config, "git_remote_origin",
        lambda repo_path: "",
    )

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path / "wrong-cwd"),
        fleet_home=str(home),
    )
    # MUST NOT refuse dispatch:
    assert result.skipped is False, (
        f"local-only repo wrongly skipped: reason={result.reason!r}"
    )
    assert seen_cwd and seen_cwd[0] == str(repo), (
        f"tick should still use configured repo as cwd; got {seen_cwd}"
    )
    # MUST surface a soft warning so the operator knows validation
    # was skipped:
    assert any(
        "no `origin`" in e or "no origin" in e for e in result.errors
    ), f"expected local-only soft warning in errors; got {result.errors}"


def test_tick_stale_repo_path_refuses_dispatch(
    tmp_path: Path, monkeypatch,
) -> None:
    """coord-config.json::repo points at a path that no longer exists
    (operator deleted/moved the checkout, or typo'd the path) →
    refuse dispatch with `coord-config-repo-missing`, NOT silently
    treat as local-only.

    review iter-5 (codex P2 finding): `git remote get-url origin`
    against a missing directory returns "" (per git_remote_origin's
    swallow-on-failure contract), which iter-4 mapped to 'local-only
    repo, proceed.' That let dispatches land against a nonexistent
    cwd and fail later with an opaque error. Refuse explicitly so
    the operator gets a clear coord-config error pointing at the
    actual problem."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    stale_repo = tmp_path / "deleted-checkout"  # NOT created
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(json.dumps({"repo": str(stale_repo)}) + "\n")
    _patch_bootstrap(monkeypatch)

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
        "stale-repo must short-circuit BEFORE _tick_locked runs"
    )
    assert result.skipped is True
    assert result.reason == "coord-config-repo-missing", (
        f"expected coord-config-repo-missing reason; got {result.reason!r}"
    )
    assert result.raised >= 1
    assert any(
        "not a directory" in e for e in result.errors
    ), f"expected stale-path error; got {result.errors}"


def test_tick_meta_json_repo_path_wins_over_coord_config(
    tmp_path: Path, monkeypatch,
) -> None:
    """meta.json::repo_path is operator-authoritative (set by
    `fleet project add`); when present AND different from
    coord-config.json::repo, meta.json wins outright + warning surfaces.

    iter-7 (codex P1 resolution): the #175 bug is exactly "coord
    spawned from wrong cwd → coord-config.json::repo points at the
    wrong repo." meta.json::repo_path is set explicitly by
    `fleet project add` and is NOT subject to that bug. When both
    sources exist and disagree, meta.json is the source of truth.
    The URL heuristic (which can't distinguish custom-name from
    wrong-repo) is no longer load-bearing in this case."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    # meta.json points at the REAL rainier checkout.
    correct_repo = tmp_path / "rainier-correct"
    correct_repo.mkdir()
    # coord-config.json::repo points at fleet (the #175 bug scenario).
    wrong_spawn_repo = tmp_path / "fleet-checkout"
    wrong_spawn_repo.mkdir()
    proj_dir = home / "projects" / project
    (proj_dir / "meta.json").write_text(
        json.dumps({"schema": "v1", "repo_path": str(correct_repo)}) + "\n"
    )
    (proj_dir / "coord-config.json").write_text(
        json.dumps({"repo": str(wrong_spawn_repo)}) + "\n"
    )
    _patch_bootstrap(monkeypatch)

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path / "wrong-cwd"),
        fleet_home=str(home),
    )
    assert result.skipped is False, (
        f"meta.json should win + proceed; got skipped: {result.reason!r}"
    )
    # MUST use meta.json's path, NOT coord-config's.
    assert seen_cwd and seen_cwd[0] == str(correct_repo), (
        f"meta.json::repo_path must win over coord-config.json::repo; "
        f"got cwd={seen_cwd[0]!r}, want {str(correct_repo)!r}"
    )
    # Warning should announce the override:
    assert any(
        "meta.json" in e and "coord-config.json" in e
        and "differs" in e for e in result.errors
    ), f"expected meta-vs-coord-config divergence warning; got {result.errors}"


def test_tick_meta_json_repo_path_used_silently_when_matches(
    tmp_path: Path, monkeypatch,
) -> None:
    """When meta.json::repo_path == coord-config.json::repo, no
    divergence warning fires (they agree)."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "rainier"
    repo.mkdir()
    proj_dir = home / "projects" / project
    (proj_dir / "meta.json").write_text(
        json.dumps({"schema": "v1", "repo_path": str(repo)}) + "\n"
    )
    (proj_dir / "coord-config.json").write_text(
        json.dumps({"repo": str(repo)}) + "\n"
    )
    _patch_bootstrap(monkeypatch)

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path),
        fleet_home=str(home),
    )
    assert result.skipped is False
    assert seen_cwd[0] == str(repo)
    # No divergence warning, no missing-coord-config warning.
    divergence_warnings = [
        e for e in result.errors if "differs from meta.json" in e
    ]
    assert divergence_warnings == [], (
        f"matching paths should not warn; got {divergence_warnings}"
    )


def test_tick_meta_json_repo_path_only_no_coord_config(
    tmp_path: Path, monkeypatch,
) -> None:
    """meta.json::repo_path present + coord-config.json::repo absent →
    meta.json wins silently (legacy projects + new `fleet project add`)."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "rainier"
    repo.mkdir()
    proj_dir = home / "projects" / project
    (proj_dir / "meta.json").write_text(
        json.dumps({"schema": "v1", "repo_path": str(repo)}) + "\n"
    )
    # No coord-config.json on disk.
    _patch_bootstrap(monkeypatch)

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path / "wrong-cwd"),
        fleet_home=str(home),
    )
    assert result.skipped is False
    assert seen_cwd[0] == str(repo)


def test_tick_meta_json_stale_path_refuses(
    tmp_path: Path, monkeypatch,
) -> None:
    """meta.json::repo_path points at a missing directory → refuse with
    meta-repo-missing reason. Symmetric to the coord-config stale-path
    refuse from iter-5."""
    project = "projects-rainier"
    home = _seed_fleet_home(tmp_path, project)
    stale_repo = tmp_path / "deleted"  # NOT created
    proj_dir = home / "projects" / project
    (proj_dir / "meta.json").write_text(
        json.dumps({"schema": "v1", "repo_path": str(stale_repo)}) + "\n"
    )
    _patch_bootstrap(monkeypatch)

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
    assert called["n"] == 0
    assert result.skipped is True
    assert result.reason == "meta-repo-missing"
    assert result.raised >= 1


def test_read_project_repo_path_returns_none_when_missing(
    tmp_path: Path,
) -> None:
    """meta.json absent → None (fall through to coord-config path)."""
    assert coord_config.read_project_repo_path(tmp_path) is None


def test_read_project_repo_path_returns_none_when_field_missing(
    tmp_path: Path,
) -> None:
    """meta.json without repo_path field → None."""
    (tmp_path / "meta.json").write_text(json.dumps({"schema": "v1"}))
    assert coord_config.read_project_repo_path(tmp_path) is None


def test_read_project_repo_path_returns_stripped(tmp_path: Path) -> None:
    """Valid repo_path → returned with whitespace stripped."""
    (tmp_path / "meta.json").write_text(
        json.dumps({"schema": "v1", "repo_path": "  /repos/foo  "})
    )
    assert coord_config.read_project_repo_path(tmp_path) == "/repos/foo"


def test_tick_accepts_generic_parent_dir_tag(
    tmp_path: Path, monkeypatch,
) -> None:
    """`repos-my-project` tag (from `fleet project add /repos/my-project`)
    must dispatch successfully when origin is `.../my-project.git`.

    review iter-4 (codex P1 finding): the iter-3 heuristic stripped
    only the literal `projects-` prefix, leaving non-`projects-` tags
    broken. The generic strip-first-`-`-token approach handles every
    TagForPath shape."""
    project = "repos-my-project"
    home = _seed_fleet_home(tmp_path, project)
    repo = tmp_path / "my-project-checkout"
    repo.mkdir()
    cfg = home / "projects" / project / "coord-config.json"
    cfg.write_text(json.dumps({"repo": str(repo)}) + "\n")
    _patch_bootstrap(monkeypatch)
    monkeypatch.setattr(
        coord_config, "git_remote_origin",
        lambda repo_path: "https://github.com/acme/my-project.git",
    )

    seen_cwd: list[str] = []
    real_tick_locked = loop._tick_locked

    def spy(*args, **kwargs):
        seen_cwd.append(args[4])
        return real_tick_locked(*args, **kwargs)

    monkeypatch.setattr(loop, "_tick_locked", spy)

    result = loop.tick(
        project, coord_id="", cwd=str(tmp_path / "wrong-cwd"),
        fleet_home=str(home),
    )
    assert result.skipped is False, (
        f"generic parent-dir tag wrongly skipped: reason={result.reason!r}"
    )
    assert seen_cwd and seen_cwd[0] == str(repo)
