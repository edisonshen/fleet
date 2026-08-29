"""worktree.py + worktree-mode dispatch tests.

Two layers of coverage:

  1. worktree.py unit tests — argv assertions for `git worktree add` /
     `remove`, the safe-path guard, idempotent "already exists" path,
     and ENOENT-tolerant remove.
  2. loop.py integration tests — drive a tick with cap=2 against a
     mocked fleet binary + git binary; assert worktree create / remove
     argv lands at the right moments and cap=1 ticks are byte-identical
     to v0.2.0 (no git invocations, no worktree fields).

Subprocess is mocked throughout so tests stay fast and don't need a
real git on PATH. We intercept `subprocess.run` per-module (one mock
per module under test) so the wrong mock can't mask a regression.
"""
from __future__ import annotations

import dataclasses
import inspect
import os
import shutil
import subprocess
from unittest.mock import patch, MagicMock

import pytest

import dispatch as dispatch_mod
import loop
import parse
import worktree as worktree_mod


# ---------- worktree.py unit tests ----------


def _ok(stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=0, stdout=stdout, stderr=stderr)


def _err(stderr: str, returncode: int = 1) -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=returncode, stdout="", stderr=stderr)


def _maybe_claims_emulator(cmd, kwargs):
    """Emulate `fleet claims acquire-prompt` + `release` for loop.py
    tests that intercept subprocess.run.

    PR1 dispatch-lifecycle migration: loop.py now calls
    `fleet claims acquire-prompt` to write the inbox file and
    `fleet claims release` to unlink it on terminal transitions.
    Tests that patched dispatch_mod.subprocess.run pre-PR1 didn't
    need to handle these argv shapes; this helper preserves the
    existing fixture API by emulating the side effects + envelope
    automatically. Returns None when cmd isn't a claims call (caller
    falls through to its own routing).
    """
    if len(cmd) < 4:
        return None
    env = kwargs.get("env") or os.environ
    fleet_home = env.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
    if cmd[1:3] == ["claims", "acquire-prompt"]:
        agent_id = cmd[3]
        inbox_dir = os.path.join(fleet_home, "inbox")
        os.makedirs(inbox_dir, exist_ok=True)
        path = os.path.join(inbox_dir, f"{agent_id}.md")
        body = kwargs.get("input") or ""
        if body and not body.endswith("\n"):
            body = body + "\n"
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(body)
        envelope = (
            f'{{"outcome":"acquired","dispatch_id":"{agent_id}",'
            f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'
        )
        return _ok(stdout=envelope)
    if cmd[1:3] == ["claims", "release"]:
        agent_id = cmd[3]
        path = os.path.join(fleet_home, "inbox", f"{agent_id}.md")
        outcome = "released"
        try:
            os.unlink(path)
        except FileNotFoundError:
            outcome = "already_released"
        except OSError:
            pass
        envelope = (
            f'{{"outcome":"{outcome}","dispatch_id":"{agent_id}",'
            f'"kind":"coord_prompt_inbox","path":"{path}"}}\n'
        )
        return _ok(stdout=envelope)
    return None


def test_compute_worktree_path_invokes_fleet_cli() -> None:
    fake = _ok(
        stdout="/Users/op/.fleet/projects/proj/worktrees/alpha-1234/\n",
    )
    with patch.object(worktree_mod.subprocess, "run", return_value=fake) as m:
        path = worktree_mod.compute_worktree_path(
            "proj", "alpha-1234", fleet_bin="/usr/local/bin/fleet",
        )
    assert path == "/Users/op/.fleet/projects/proj/worktrees/alpha-1234"
    args = m.call_args[0][0]
    assert args == ["/usr/local/bin/fleet", "workers", "worktree-path", "--project", "proj", "alpha-1234"]


def test_compute_worktree_path_returns_empty_on_error() -> None:
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("boom")):
        assert worktree_mod.compute_worktree_path("proj", "alpha-1234") == ""
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no fleet")):
        assert worktree_mod.compute_worktree_path("proj", "alpha-1234") == ""


def test_create_worktree_invokes_git_with_correct_argv(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    fake = _ok()
    with patch.object(worktree_mod.subprocess, "run", return_value=fake) as m:
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.error == ""
    assert res.path == wt
    args = m.call_args[0][0]
    assert args == ["git", "-C", repo, "worktree", "add", wt, "-b", "worker/alpha-1234"]
    # Parent dir created so git doesn't trip on a missing prefix.
    assert os.path.isdir(os.path.dirname(wt))


def test_create_worktree_passes_base_when_provided(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        worktree_mod.create_worktree(repo, wt, "worker/alpha-1234", base="main")
    args = m.call_args[0][0]
    assert args[-1] == "main"


def test_create_worktree_base_is_last_arg_not_dash_b_target(tmp_path) -> None:
    """Coord-worktree-stale fix: base must be appended AFTER `-b <branch>`
    so git creates `worker/<slug>` starting at <base>, not the local
    HEAD. argv order matters: `worktree add <wt> -b <branch> <base>`."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        worktree_mod.create_worktree(
            repo, wt, "worker/alpha-1234", base="origin/main",
        )
    args = m.call_args[0][0]
    assert args == [
        "git", "-C", repo, "worktree", "add", wt, "-b",
        "worker/alpha-1234", "origin/main",
    ]


# ---------- worktree.py: default-branch resolution + fetch ----------


def test_resolve_default_branch_strips_remote_prefix() -> None:
    """`git symbolic-ref --short refs/remotes/origin/HEAD` returns
    `origin/<branch>`; we strip the `origin/` to get the bare name."""
    with patch.object(
        worktree_mod.subprocess, "run", return_value=_ok(stdout="origin/main\n"),
    ) as m:
        assert worktree_mod.resolve_default_branch("/repo") == "main"
    args = m.call_args[0][0]
    assert args == [
        "git", "-C", "/repo", "symbolic-ref", "--short",
        "refs/remotes/origin/HEAD",
    ]


def test_resolve_default_branch_handles_non_main_default() -> None:
    with patch.object(
        worktree_mod.subprocess, "run",
        return_value=_ok(stdout="origin/master\n"),
    ):
        assert worktree_mod.resolve_default_branch("/repo") == "master"


def test_resolve_default_branch_falls_back_to_main_on_error() -> None:
    """Unset origin/HEAD (bare clone, never set) or a git error → "main"
    so dispatch still proceeds with a sane base."""
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("fatal: ref not a symbolic ref\n")):
        assert worktree_mod.resolve_default_branch("/repo") == "main"
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no git")):
        assert worktree_mod.resolve_default_branch("/repo") == "main"
    assert worktree_mod.resolve_default_branch("") == "main"


def test_fetch_remote_invokes_git_fetch_with_argv() -> None:
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        res = worktree_mod.fetch_remote("/repo", "main")
    assert res.error == ""
    args = m.call_args[0][0]
    assert args == ["git", "-C", "/repo", "fetch", "origin", "main"]


def test_fetch_remote_surfaces_error_non_fatally() -> None:
    """Offline / no-remote fetch returns an error string the caller logs;
    it does NOT raise (caller proceeds with the existing origin ref)."""
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("fatal: could not read from remote\n")):
        res = worktree_mod.fetch_remote("/repo", "main")
    assert res.path == ""
    assert "could not read from remote" in res.error
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no git")):
        res = worktree_mod.fetch_remote("/repo", "main")
    assert "fetch_remote:" in res.error


def test_fetch_remote_refuses_empty() -> None:
    with patch.object(worktree_mod.subprocess, "run") as m:
        res = worktree_mod.fetch_remote("", "main")
    assert "empty repo/branch" in res.error
    assert m.call_count == 0


def test_ref_exists_true_when_rev_parse_succeeds() -> None:
    """ref_exists verifies the base resolves to a commit before we hand
    it to `git worktree add` (codex [P2] guard)."""
    with patch.object(
        worktree_mod.subprocess, "run", return_value=_ok(stdout="deadbeef\n"),
    ) as m:
        assert worktree_mod.ref_exists("/repo", "origin/main") is True
    args = m.call_args[0][0]
    assert args == [
        "git", "-C", "/repo", "rev-parse", "--verify", "--quiet",
        "origin/main^{commit}",
    ]


def test_ref_exists_false_when_ref_absent() -> None:
    """Missing origin/<default> (no remote / never fetched) → False so
    the caller falls back to local HEAD instead of a fatal base."""
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("", returncode=1)):
        assert worktree_mod.ref_exists("/repo", "origin/main") is False


def test_ref_exists_false_on_subprocess_exception_and_empty() -> None:
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no git")):
        assert worktree_mod.ref_exists("/repo", "origin/main") is False
    assert worktree_mod.ref_exists("", "origin/main") is False
    assert worktree_mod.ref_exists("/repo", "") is False


def test_is_ancestor_true_argv_and_false_paths() -> None:
    """is_ancestor gates upstream-as-base: True only when local HEAD is
    contained in the upstream (codex [P1] #2)."""
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        assert worktree_mod.is_ancestor("/repo", "HEAD", "origin/main") is True
    args = m.call_args[0][0]
    assert args == [
        "git", "-C", "/repo", "merge-base", "--is-ancestor", "HEAD", "origin/main",
    ]
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("", returncode=1)):
        assert worktree_mod.is_ancestor("/repo", "HEAD", "origin/main") is False
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no git")):
        assert worktree_mod.is_ancestor("/repo", "HEAD", "origin/main") is False
    assert worktree_mod.is_ancestor("/repo", "", "origin/main") is False
    assert worktree_mod.is_ancestor("/repo", "HEAD", "") is False


# ---------- worktree.py: current_branch + resolve_worker_base ----------


def test_current_branch_returns_name() -> None:
    with patch.object(
        worktree_mod.subprocess, "run", return_value=_ok(stdout="feat/x\n"),
    ) as m:
        assert worktree_mod.current_branch("/repo") == "feat/x"
    args = m.call_args[0][0]
    assert args == ["git", "-C", "/repo", "symbolic-ref", "--short", "-q", "HEAD"]


def test_current_branch_empty_on_detached_head_or_error() -> None:
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("", returncode=1)):
        assert worktree_mod.current_branch("/repo") == ""
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no git")):
        assert worktree_mod.current_branch("/repo") == ""
    assert worktree_mod.current_branch("") == ""


def test_upstream_ref_returns_configured_upstream() -> None:
    """upstream_ref reads the branch's ACTUAL @{upstream}, which may be
    differently-named than the local branch (codex [P2])."""
    with patch.object(
        worktree_mod.subprocess, "run", return_value=_ok(stdout="origin/main\n"),
    ) as m:
        assert worktree_mod.upstream_ref("/repo", "integration") == "origin/main"
    args = m.call_args[0][0]
    assert args == [
        "git", "-C", "/repo", "rev-parse", "--abbrev-ref",
        "--symbolic-full-name", "integration@{upstream}",
    ]


def test_upstream_ref_empty_when_no_upstream() -> None:
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("", returncode=128)):
        assert worktree_mod.upstream_ref("/repo", "local-only") == ""
    with patch.object(worktree_mod.subprocess, "run", side_effect=FileNotFoundError("no git")):
        assert worktree_mod.upstream_ref("/repo", "x") == ""
    assert worktree_mod.upstream_ref("", "x") == ""


def test_resolve_worker_base_uses_configured_upstream_same_name() -> None:
    """On a checked-out branch whose upstream is origin/<same name>, the
    worker branches off that fresh upstream — NOT the remote default."""
    runs = [
        _ok(stdout="integration\n"),     # current_branch
        _ok(stdout="origin/integration\n"),  # upstream_ref
    ]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        wb = worktree_mod.resolve_worker_base("/repo")
    assert wb.base == "origin/integration"
    assert wb.fetch_remote == "origin"
    assert wb.fetch_branch == "integration"


def test_resolve_worker_base_honors_differently_named_upstream() -> None:
    """codex [P2]: `git checkout -b integration --track origin/main` tracks
    origin/MAIN. resolve_worker_base must fetch + base off the CONFIGURED
    upstream (origin/main), not the bogus origin/integration."""
    runs = [
        _ok(stdout="integration\n"),   # current_branch
        _ok(stdout="origin/main\n"),   # upstream_ref → differently named
    ]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        wb = worktree_mod.resolve_worker_base("/repo")
    assert wb.base == "origin/main"
    assert wb.fetch_remote == "origin"
    assert wb.fetch_branch == "main"


def test_resolve_worker_base_local_only_branch_uses_local_head() -> None:
    """A purely-local stacked branch (no upstream) → base="" (local HEAD)
    so the operator's commits aren't dropped; no fetch attempted."""
    runs = [
        _ok(stdout="stacked-local\n"),   # current_branch
        _err("", returncode=128),        # upstream_ref → no upstream
    ]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        wb = worktree_mod.resolve_worker_base("/repo")
    assert wb.base == ""
    assert wb.fetch_remote == ""
    assert wb.fetch_branch == ""


def test_resolve_worker_base_detached_head_falls_back_to_default() -> None:
    """Detached HEAD has no current branch → fall back to the remote
    default branch (resolve_default_branch)."""
    runs = [
        _err("", returncode=1),          # current_branch → detached
        _ok(stdout="origin/master\n"),   # resolve_default_branch: origin/HEAD
    ]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        wb = worktree_mod.resolve_worker_base("/repo")
    assert wb.base == "origin/master"
    assert wb.fetch_remote == "origin"
    assert wb.fetch_branch == "master"


def test_split_remote_ref() -> None:
    assert worktree_mod._split_remote_ref("origin/main") == ("origin", "main")
    assert worktree_mod._split_remote_ref("origin/feat/x") == ("origin", "feat/x")
    assert worktree_mod._split_remote_ref("upstream/dev") == ("upstream", "dev")
    assert worktree_mod._split_remote_ref("bare") == ("origin", "bare")
    assert worktree_mod._split_remote_ref("") == ("", "")


def test_create_worktree_idempotent_on_already_exists_when_registered(tmp_path) -> None:
    """Coord crash mid-tick can leave the wt on disk + registered; resume
    must succeed. We verify the path is registered via `git worktree
    list --porcelain` before treating "already exists" as idempotent
    (codex iter-2 [P2])."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    add_err = _err(f"fatal: '{wt}' already exists\n")
    list_ok = _ok(stdout=f"worktree {wt}\nHEAD abc123\nbranch refs/heads/worker/alpha-1234\n\n")
    runs = [add_err, list_ok]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.error == ""
    assert res.path == wt


def test_create_worktree_idempotent_on_already_checked_out_when_registered(tmp_path) -> None:
    """Variant phrasing: `is already checked out at <ref>` is also a
    worktree-path collision and is idempotent IFF the path is a
    registered worktree."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    add_err = _err(f"fatal: '{wt}' is already checked out at /elsewhere\n")
    list_ok = _ok(stdout=f"worktree {wt}\n\n")
    runs = [add_err, list_ok]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.error == ""
    assert res.path == wt


def test_create_worktree_already_exists_but_not_registered_is_error(tmp_path) -> None:
    """Codex iter-2 [P2] regress: a stale non-empty directory at wt_path
    also triggers `'<path>' already exists`, but it's NOT a real
    worktree. Without the registry verify, we'd hand the worker a
    non-checkout cwd and the first git step would fail. With the
    verify, we surface the error so the operator can clean up."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    add_err = _err(f"fatal: '{wt}' already exists\n")
    # `git worktree list --porcelain` does NOT include wt — confirms the
    # path is occupied by a stale dir, not a registered worktree.
    list_no_match = _ok(stdout=f"worktree {repo}\n\n")
    runs = [add_err, list_no_match]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.path == ""
    assert "not a registered git worktree" in res.error


def test_create_worktree_branch_already_exists_retries_without_dash_b(tmp_path) -> None:
    """Codex iter-2 [P1]: when the branch persists (open PR) but the
    worktree was cleaned up, `git worktree add -b <branch>` fatals
    "branch already exists". Retry without `-b` so the existing branch
    is checked out into the new worktree — otherwise the task can
    never be re-dispatched after its first PR opens."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    add_err = _err("fatal: A branch named 'worker/alpha-1234' already exists.\n")
    retry_ok = _ok()
    runs = [add_err, retry_ok]
    calls: list = []

    def _run(cmd, *a, **k):
        calls.append(list(cmd))
        return runs.pop(0)

    with patch.object(worktree_mod.subprocess, "run", side_effect=_run):
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.error == ""
    assert res.path == wt
    # First call: with `-b`. Second call: without `-b` (reuse branch).
    assert calls[0] == ["git", "-C", repo, "worktree", "add", wt, "-b", "worker/alpha-1234"]
    assert calls[1] == ["git", "-C", repo, "worktree", "add", wt, "worker/alpha-1234"]


def test_create_worktree_branch_reuse_failure_surfaces_error(tmp_path) -> None:
    """If the retry-without-`-b` also fails (e.g. the branch is checked
    out in another worktree), we surface the error rather than
    silently succeeding."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    add_err = _err("fatal: A branch named 'worker/alpha-1234' already exists.\n")
    retry_err = _err("fatal: 'worker/alpha-1234' is already checked out at /other/wt\n")
    runs = [add_err, retry_err]
    with patch.object(worktree_mod.subprocess, "run", side_effect=lambda *a, **k: runs.pop(0)):
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.path == ""
    assert "branch worker/alpha-1234 reuse failed" in res.error


def test_create_worktree_surfaces_real_git_error(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / "wt" / "alpha-1234")
    with patch.object(worktree_mod.subprocess, "run", return_value=_err("fatal: not a git repository\n")):
        res = worktree_mod.create_worktree(repo, wt, "worker/alpha-1234")
    assert res.path == ""
    assert "not a git repository" in res.error


def test_remove_worktree_refuses_main_repo(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    res = worktree_mod.remove_worktree(repo, repo)
    assert "refuse unsafe path" in res.error


def test_remove_worktree_refuses_path_outside_projects_tree(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    rogue = str(tmp_path / "elsewhere" / "alpha-1234")
    res = worktree_mod.remove_worktree(repo, rogue)
    assert "refuse unsafe path" in res.error


def test_remove_worktree_refuses_empty_path(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    res = worktree_mod.remove_worktree(repo, "")
    assert "refuse unsafe path" in res.error


def test_remove_worktree_default_force_false_dirty_guarded(tmp_path) -> None:
    # DESIGN §4.1: force=False is now the DEFAULT. A clean tree (porcelain
    # returns empty) is removed WITHOUT --force, after a dirty-guard probe.
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    os.makedirs(wt)

    def fake_run(cmd, **kw):
        if "status" in cmd and "--porcelain" in cmd:
            return _ok("")  # clean
        return _ok()

    with patch.object(worktree_mod.subprocess, "run", side_effect=fake_run) as m:
        res = worktree_mod.remove_worktree(repo, wt)
    assert res.outcome == worktree_mod.OUTCOME_REMOVED
    assert res.error == ""
    calls = [c[0][0] for c in m.call_args_list]
    # Dirty-guard probe ran; remove ran WITHOUT --force; prune ran.
    assert ["git", "-C", wt, "status", "--porcelain"] in calls
    assert ["git", "-C", repo, "worktree", "remove", wt] in calls
    assert ["git", "-C", repo, "worktree", "remove", "--force", wt] not in calls
    assert ["git", "-C", repo, "worktree", "prune"] in calls


def test_remove_worktree_dirty_parks_not_removed(tmp_path) -> None:
    # FAILS-ON-PARENT: parent force=True would delete this. New default
    # parks a dirty tree (porcelain non-empty) and never calls remove.
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    os.makedirs(wt)

    def fake_run(cmd, **kw):
        if "status" in cmd and "--porcelain" in cmd:
            return _ok(" M file.py\n")  # DIRTY
        if cmd[3:5] == ["worktree", "remove"]:
            raise AssertionError("dirty tree must NOT be removed")
        return _ok()

    with patch.object(worktree_mod.subprocess, "run", side_effect=fake_run):
        res = worktree_mod.remove_worktree(repo, wt)
    assert res.outcome == worktree_mod.OUTCOME_DIRTY_PARKED
    assert res.error != ""


def test_remove_worktree_no_force_when_force_false(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    os.makedirs(wt)
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        worktree_mod.remove_worktree(repo, wt, force=False)
    first_call = m.call_args_list[0][0][0]
    assert "--force" not in first_call


def test_remove_worktree_treats_missing_path_as_success(tmp_path) -> None:
    """ENOENT is success — we just need the worktree gone."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "gone-1234")
    # Don't create wt; remove_worktree should not invoke `git worktree remove`.
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        res = worktree_mod.remove_worktree(repo, wt)
    assert res.error == ""
    # Only `worktree prune` called — no `remove`.
    for call in m.call_args_list:
        assert "remove" not in call[0][0]


# ---------- worktree.py: tick-start orphan cleanup ----------


def test_prune_worktrees_invokes_git_with_correct_argv(tmp_path) -> None:
    """Tick-start orphan cleanup shells `git -C <repo> worktree prune`
    with no extra flags. Idempotent — git only drops registry entries
    whose dirs are missing, never touches a live checkout."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m:
        worktree_mod.prune_worktrees(repo)
    assert m.call_count == 1
    args = m.call_args[0][0]
    assert args == ["git", "-C", repo, "worktree", "prune"]


def test_prune_worktrees_silent_on_git_error(tmp_path, capsys) -> None:
    """Git failures (non-zero exit) are logged to stderr but never
    raise — orphan cleanup is best-effort and must not abort the tick."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    with patch.object(
        worktree_mod.subprocess, "run",
        return_value=_err("fatal: not a git repository\n"),
    ):
        # Must not raise.
        worktree_mod.prune_worktrees(repo)
    captured = capsys.readouterr()
    assert "worktree prune failed" in captured.err
    assert "not a git repository" in captured.err


def test_prune_worktrees_silent_on_subprocess_exception(tmp_path, capsys) -> None:
    """FileNotFoundError (no `git` on PATH) and TimeoutExpired log to
    stderr but never raise."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    with patch.object(
        worktree_mod.subprocess, "run",
        side_effect=FileNotFoundError("git not found"),
    ):
        worktree_mod.prune_worktrees(repo)
    captured = capsys.readouterr()
    assert "worktree prune failed" in captured.err


def test_prune_worktrees_empty_repo_is_noop(tmp_path) -> None:
    """Empty repo string: defensive no-op, nothing shells out."""
    with patch.object(worktree_mod.subprocess, "run") as m:
        worktree_mod.prune_worktrees("")
    assert m.call_count == 0


# ---------- loop.py: cap=1 single-worker mode (regression guard) ----------


def _ready_task(slug: str = "alpha-1234") -> parse.Task:
    return parse.Task(
        slug=slug, status="ready", priority="P1",
        spec="Do the thing.", acceptance="Thing done.",
    )


def test_dispatch_ready_cap1_does_not_invoke_worktree(tmp_path) -> None:
    """Single-worker mode is byte-identical to v0.2.0-style behavior
    minus the `fleet dispatch` shell-out (issue #84 Phase A): no git,
    no worktree resolution, no worktree field on the dispatch action.

    Since the skill no longer calls `fleet dispatch`, we assert via
    the rendered DISPATCH instruction (one per dispatched task) and
    by checking subprocess never receives a `dispatch` argv.
    """
    t = _ready_task()

    calls: list[list[str]] = []

    def _runner(cmd, *args, **kwargs):
        calls.append(list(cmd))
        # PR1 dispatch-lifecycle: emulate fleet claims acquire-prompt.
        emu = _maybe_claims_emulator(cmd, kwargs)
        if emu is not None:
            return emu
        return _ok()

    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_runner):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd="/repo", cap=1,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )
    assert len(actions) == 1
    assert actions[0].error == ""
    assert actions[0].worktree == ""  # cap=1 → no worktree field
    # Issue #84 Phase A: dispatch instruction is the new spawn signal.
    assert actions[0].dispatch_instruction.startswith("DISPATCH:")
    # No git invocations in cap=1 mode.
    assert not any(c[0] == "git" for c in calls), f"unexpected git: {calls}"
    # No `fleet workers worktree-path` invocations either.
    assert not any(
        len(c) >= 3 and c[1] == "workers" and c[2] == "worktree-path" for c in calls
    ), f"unexpected worktree-path resolve: {calls}"
    # Issue #84 Phase A: NO `fleet dispatch` subprocess shell-out.
    assert not any(
        len(c) >= 2 and c[1] == "dispatch" for c in calls
    ), f"unexpected `fleet dispatch` call: {calls}"


# ---------- loop.py: cap=2 worktree-mode dispatch ----------


def _make_subprocess_router(routes: dict, calls: list | None = None):
    """Build a side_effect callable that routes by argv prefix.

    Each route maps a tuple of leading argv tokens → CompletedProcess.
    Falls through to a default _ok() so unmatched calls don't crash.
    `calls` is an optional list to record argv into for assertions.

    `dispatch_mod.subprocess` and `worktree_mod.subprocess` are the
    SAME subprocess module — patching one patches both. We pass a
    single router that handles git + fleet argv shapes.
    """
    def _run(cmd, *_args, **_kwargs):
        if calls is not None:
            calls.append(list(cmd))
        for prefix, result in routes.items():
            if tuple(cmd[: len(prefix)]) == prefix:
                return result
        # PR1 dispatch-lifecycle: emulate fleet claims acquire-prompt
        # when no test-specific route matches.
        emu = _maybe_claims_emulator(cmd, _kwargs)
        if emu is not None:
            return emu
        return _ok()
    return _run


def test_dispatch_ready_cap2_creates_worktree_and_passes_cwd(tmp_path) -> None:
    """cap > 1: each task gets a worktree at the path returned by
    `fleet workers worktree-path`, `git worktree add` runs with the
    expected argv, and the rendered DISPATCH block points at the
    inbox file written for that worker (issue #84 Phase A — no more
    `fleet dispatch` subprocess)."""
    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    repo = "/repo"

    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path"):
            _ok(stdout=wt_path + "\n"),
        # current_branch: coord is checked out on `main` (codex [P1] —
        # honor the coord's branch, not the remote default).
        ("git", "-C", repo, "symbolic-ref", "--short", "-q", "HEAD"):
            _ok(stdout="main\n"),
        # configured upstream of `main` → origin/main (codex [P2] — read
        # the real @{upstream}, don't assume origin/<current>).
        ("git", "-C", repo, "rev-parse", "--abbrev-ref",
         "--symbolic-full-name"): _ok(stdout="origin/main\n"),
        ("git", "-C", repo, "fetch"): _ok(),
        # ref_exists guard: origin/main resolves → use it as base.
        ("git", "-C", repo, "rev-parse", "--verify"): _ok(stdout="deadbeef\n"),
        # ancestry gate: local HEAD is an ancestor of origin/main (local
        # adds nothing) → safe to use upstream as base (codex [P1] #2).
        ("git", "-C", repo, "merge-base", "--is-ancestor"): _ok(),
        ("git", "-C", repo, "worktree", "add"): _ok(),
    }
    calls: list = []

    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_subprocess_router(routes, calls),
         ):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 1, f"expected one action, got: {actions}"
    assert actions[0].error == ""
    assert actions[0].worktree == wt_path
    assert actions[0].branch == "worker/alpha-1234"

    # Coord-worktree-stale fix: the worker branches off the FRESH
    # origin/main tip, not local HEAD. We must (1) fetch origin main and
    # (2) pass base="origin/main" to `git worktree add` so a just-merged
    # dependency PR is present in the worker's tree.
    git_calls = [c for c in calls if c[:1] == ["git"]]
    assert ["git", "-C", repo, "fetch", "origin", "main"] in git_calls, \
        f"expected origin fetch before worktree add; saw: {git_calls}"
    assert any(
        c == ["git", "-C", repo, "worktree", "add", wt_path, "-b",
              "worker/alpha-1234", "origin/main"]
        for c in git_calls
    ), f"expected `git worktree add ... origin/main` call missing; saw: {git_calls}"

    # Issue #84 Phase A: no `fleet dispatch` shell-out. The DISPATCH
    # block is the spawn signal; agent_id is minted in-process.
    assert not any(
        len(c) >= 2 and c[1] == "dispatch" for c in calls
    ), f"unexpected `fleet dispatch` call: {calls}"
    block = actions[0].dispatch_instruction
    assert block.startswith("DISPATCH: alpha-1234")
    assert "agent_id: " in block
    assert "run_in_background: true" in block
    # The prompt_file path is what the coord agent reads + passes to
    # the Agent tool — must point at the inbox under fleet_home.
    assert str(tmp_path) in block
    assert "/inbox/" in block


def test_dispatch_ready_default_cap_dispatches_three_worktrees(tmp_path) -> None:
    """At the shipped default cap, 3 workers dispatch, each into its own
    worktree, and the 4th ready task is held back."""
    # Disjoint file scopes — the conflict heuristic is conservative and
    # would serialize tasks that share (or omit) a Files line.
    tasks = []
    for i in range(4):
        t = _ready_task(f"task-{i}")
        tasks.append(dataclasses.replace(t, spec=f"Files: pkg/mod{i}/x.go"))
    repo = "/repo"
    wt_root = tmp_path / ".fleet" / "projects" / "proj" / "worktrees"

    def _run(cmd, *_args, **kwargs):
        if cmd[1:3] == ["workers", "worktree-path"]:
            return _ok(stdout=str(wt_root / cmd[-1]) + "\n")
        return _make_subprocess_router({})(cmd, *_args, **kwargs)

    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_run):
        actions = loop._dispatch_ready(
            tasks=tasks, project="proj", cwd=repo, cap=loop.DEFAULT_CAP,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    dispatched = [a for a in actions if a.error == ""]
    assert len(dispatched) == loop.DEFAULT_CAP == 3, f"actions: {actions}"
    assert [a.slug for a in dispatched] == ["task-0", "task-1", "task-2"]
    assert [a.worktree for a in dispatched] == [
        str(wt_root / f"task-{i}") for i in range(3)
    ], "each worker must get its own worktree"


def test_dispatch_ready_cap2_marks_prompt_worktree_pre_created(tmp_path) -> None:
    """Codex iter-1 [P1] regress guard: cap > 1 dispatch must pass
    `worktree_pre_created=True` to build_worker_prompt so the worker's
    first-turn prompt skips `git checkout -b <branch>` (the coord
    already ran `git worktree add -b <branch>` and the branch exists).

    We assert via the rendered prompt that the inbox file received —
    `git rev-parse --abbrev-ref HEAD` is the verify-cwd step that the
    pre-created branch path emits."""
    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    repo = "/repo"
    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path"):
            _ok(stdout=wt_path + "\n"),
        ("git", "-C", repo, "worktree", "add"): _ok(),
    }
    fleet_home_dir = str(tmp_path / "fleet_home")

    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_subprocess_router(routes),
         ):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=fleet_home_dir,
        )
    assert len(actions) == 1
    assert actions[0].error == ""
    # Read the inbox file the dispatch path wrote (issue #84 Phase A:
    # the agent_id is minted by dispatch.mint_agent_id; we look up
    # the path on the action rather than guessing the token).
    assert actions[0].agent_id, "dispatch action missing minted agent_id"
    inbox_path = os.path.join(fleet_home_dir, "inbox", f"{actions[0].agent_id}.md")
    with open(inbox_path, "r", encoding="utf-8") as fh:
        prompt = fh.read()
    assert "git checkout -b worker/alpha-1234" not in prompt, \
        "cap>1 prompt must not re-create the pre-existing branch"
    assert "git rev-parse --abbrev-ref HEAD" in prompt, \
        "cap>1 prompt must instruct worker to verify the prepared worktree"


def test_dispatch_ready_cap2_skips_when_worktree_path_unresolvable(tmp_path) -> None:
    """If `fleet workers worktree-path` errors, the task is recorded
    with an error and dispatch never runs (no worker to clean up)."""
    t = _ready_task("alpha-1234")
    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path"): _err("boom"),
    }
    calls: list = []
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_subprocess_router(routes, calls),
         ):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd="/repo", cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )
    assert len(actions) == 1
    assert "worktree-path resolution failed" in actions[0].error
    # Issue #84 Phase A: the path bails out before minting an
    # agent_id, so no DISPATCH instruction is emitted.
    assert actions[0].dispatch_instruction == ""
    # No fleet dispatch attempted (it's been removed entirely; pin
    # the no-shell-out invariant here too).
    assert not any(len(c) >= 2 and c[1] == "dispatch" for c in calls), calls
    # No git worktree add either.
    assert not any(c[:1] == ["git"] for c in calls), calls


def test_dispatch_ready_cap2_skips_when_git_worktree_add_fails(tmp_path) -> None:
    """Real git error on `worktree add` skips this task; loop continues
    so a stale wt for one slug doesn't poison all of cap."""
    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path"): _ok(stdout=wt_path + "\n"),
        ("git", "-C", "/repo", "worktree", "add"):
            _err("fatal: not a git repository\n"),
    }
    calls: list = []
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_subprocess_router(routes, calls),
         ):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd="/repo", cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )
    assert len(actions) == 1
    assert "not a git repository" in actions[0].error
    # Issue #84 Phase A: no DISPATCH instruction when worktree create
    # fails — agent_id mint + inbox write are both downstream of the
    # worktree gate.
    assert actions[0].dispatch_instruction == ""
    # No fleet dispatch attempted (the shell-out is gone; assertion
    # remains for future regression).
    assert not any(len(c) >= 2 and c[1] == "dispatch" for c in calls), calls


# ---------- loop.py: parallelism config ----------


def test_default_cap_is_three() -> None:
    """Unconfigured projects dispatch 3 workers in worktree mode."""
    assert loop.DEFAULT_CAP == 3
    assert inspect.signature(loop.tick).parameters["cap"].default == 3


def test_load_parallelism_returns_zero_when_missing(tmp_path) -> None:
    """No coord-config.json → 0 (caller falls through to DEFAULT_CAP)."""
    assert loop._load_parallelism(tmp_path) == 0


def test_load_parallelism_reads_valid_int(tmp_path) -> None:
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 3}\n')
    assert loop._load_parallelism(tmp_path) == 3


def test_load_parallelism_clamps_high(tmp_path) -> None:
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": 9999}\n')
    assert loop._load_parallelism(tmp_path) == 50


def test_load_parallelism_rejects_non_int(tmp_path) -> None:
    cfg = tmp_path / "coord-config.json"
    cfg.write_text('{"parallelism": "two"}\n')
    assert loop._load_parallelism(tmp_path) == 0
    cfg.write_text('{"parallelism": true}\n')  # bool subclass of int
    assert loop._load_parallelism(tmp_path) == 0


def test_load_parallelism_handles_malformed_json(tmp_path) -> None:
    cfg = tmp_path / "coord-config.json"
    cfg.write_text("not json\n")
    assert loop._load_parallelism(tmp_path) == 0


def test_load_parallelism_falls_back_to_fleet_home(tmp_path) -> None:
    """`fleet init` writes ~/.fleet/coord-config.json; a project with no
    parallelism of its own inherits it."""
    home = tmp_path / "home"
    project_dir = tmp_path / "projects" / "p"
    home.mkdir()
    project_dir.mkdir(parents=True)
    (home / "coord-config.json").write_text('{"parallelism": 4}\n')
    assert loop._load_parallelism(project_dir, home) == 4


def test_load_parallelism_project_wins_over_fleet_home(tmp_path) -> None:
    home = tmp_path / "home"
    project_dir = tmp_path / "projects" / "p"
    home.mkdir()
    project_dir.mkdir(parents=True)
    (home / "coord-config.json").write_text('{"parallelism": 4}\n')
    (project_dir / "coord-config.json").write_text('{"parallelism": 1}\n')
    assert loop._load_parallelism(project_dir, home) == 1


# ---------- loop.py: terminal-state worktree cleanup ----------


def test_apply_sentinel_done_pr_removes_worktree_in_cap2_mode(tmp_path) -> None:
    """TASK_DONE_PR triggers `git worktree remove` when the task block
    has a worktree path set. The branch lives on (PR is open); we just
    free the working tree."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt_path = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    os.makedirs(wt_path)
    t = parse.Task(
        slug="alpha-1234", status="in-progress", priority="P1",
        worktree=wt_path, branch="worker/alpha-1234",
    )
    action = loop._SentinelAction(
        slug="alpha-1234", kind="task_done_pr",
        payload="https://github.com/x/y/pull/1", dispatch_generation=0,
    )

    def fake_run(cmd, **kw):
        # Branch-identity probe → expected branch; clean porcelain.
        if "rev-parse" in cmd and "--abbrev-ref" in cmd:
            return _ok("worker/alpha-1234\n")
        if "status" in cmd and "--porcelain" in cmd:
            return _ok("")
        return _ok()

    with patch.object(worktree_mod.subprocess, "run", side_effect=fake_run) as m_wt, \
         patch.object(loop, "_run_fleet") as m_fleet:
        loop._apply_sentinel(
            action, "proj", "fleet",
            repo=repo, tasks_by_slug={t.slug: t}, full_tasks_by_slug={t.slug: t},
        )
    # DESIGN §4.1: `git worktree remove <wt>` ran WITHOUT --force.
    git_calls = [c[0][0] for c in m_wt.call_args_list]
    assert any(c[3:5] == ["worktree", "remove"] and "--force" not in c for c in git_calls), git_calls
    assert not any("--force" in c for c in git_calls), git_calls
    # tasks.md mutations happened in the right order.
    fleet_calls = [c[0][0] for c in m_fleet.call_args_list]
    assert any("pr_url=https://github.com/x/y/pull/1" in " ".join(c) for c in fleet_calls)
    assert any("status=in-review" in " ".join(c) for c in fleet_calls)
    # Worktree field cleared after remove so re-dispatch starts fresh.
    assert any("worktree=" in c[-1] and c[-1].endswith("worktree=") for c in fleet_calls)
    # Clean reap → NOT parked.
    assert not any("parked=" in " ".join(c) for c in fleet_calls)


def test_apply_sentinel_cap1_does_not_remove_worktree(tmp_path) -> None:
    """Single-worker mode: tasks_by_slug is None, repo is empty —
    sentinel apply is byte-identical to v0.2.0, no git invocations."""
    action = loop._SentinelAction(
        slug="alpha-1234", kind="task_done_pr",
        payload="https://github.com/x/y/pull/1",
    )
    with patch.object(worktree_mod.subprocess, "run") as m_wt, \
         patch.object(loop, "_run_fleet"):
        loop._apply_sentinel(action, "proj", "fleet")  # no repo / tasks_by_slug
    assert m_wt.call_count == 0


def test_apply_reconcile_done_removes_worktree_in_cap2_mode(tmp_path) -> None:
    """CI-merged → status=done; reconcile cleans the worktree even
    though no sentinel arrived (the merge happened on GitHub)."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt_path = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    os.makedirs(wt_path)
    t = parse.Task(
        slug="alpha-1234", status="in-review", priority="P1",
        worktree=wt_path, branch="worker/alpha-1234",
    )
    action = loop._ReconcileAction(
        slug="alpha-1234", new_status="done", clear_worker=True,
    )

    def fake_run(cmd, **kw):
        if "rev-parse" in cmd and "--abbrev-ref" in cmd:
            return _ok("worker/alpha-1234\n")
        if "status" in cmd and "--porcelain" in cmd:
            return _ok("")
        return _ok()

    with patch.object(worktree_mod.subprocess, "run", side_effect=fake_run) as m_wt, \
         patch.object(loop, "_run_fleet"):
        loop._apply_reconcile(
            action, "proj", "fleet",
            repo=repo, tasks_by_slug={t.slug: t},
        )
    git_calls = [c[0][0] for c in m_wt.call_args_list]
    # DESIGN §4.1: removed WITHOUT --force (clean + branch-corroborated).
    assert any(c[3:5] == ["worktree", "remove"] and wt_path in c for c in git_calls), git_calls
    assert not any("--force" in c for c in git_calls), git_calls


def test_apply_reconcile_in_review_keeps_worktree(tmp_path) -> None:
    """Status flip to in-review (CI green, not merged) keeps the
    worktree alive — re-dispatch on rebase or push-fix needs it."""
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt_path = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    t = parse.Task(slug="alpha-1234", status="in-progress", priority="P1", worktree=wt_path)
    action = loop._ReconcileAction(slug="alpha-1234", new_status="in-review")
    with patch.object(worktree_mod.subprocess, "run") as m_wt, \
         patch.object(loop, "_run_fleet"):
        loop._apply_reconcile(
            action, "proj", "fleet",
            repo=repo, tasks_by_slug={t.slug: t},
        )
    # No `git worktree remove` call.
    for call in m_wt.call_args_list:
        cmd = call[0][0]
        assert "remove" not in cmd, f"unexpected remove: {cmd}"


# ---------- REAL-GIT integration: stale-base regression ----------
#
# coord-worktree-stale-bas-b40b. The mocked tests above pin argv shape;
# this drives a REAL git repo to prove the integrated behavior: when a
# dependency PR has merged to origin/<default> AFTER the coord's last
# pull (origin ahead of local HEAD), a dispatched worker's worktree must
# contain the dependency's merged file. With the fix (fetch + base=
# origin/<default>) it does; the pre-fix code (branch off local HEAD)
# leaves the file ABSENT — fails-on-parent.


_GIT_ENV = {
    **os.environ,
    "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
    "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t",
}


def _git(repo: str, *args: str) -> str:
    proc = subprocess.run(
        ["git", "-C", repo, *args],
        capture_output=True, text=True, env=_GIT_ENV, check=True,
    )
    return proc.stdout.strip()


@pytest.fixture
def _stale_clone(tmp_path):
    """Build: a bare origin with two commits on main, and a clone whose
    local main is pinned to the FIRST commit (stale — origin advanced
    after the clone's last fetch). The second commit adds dep.txt, the
    "merged dependency" file the worker must see.

    Returns (clone_path, origin_default_branch).
    """
    git = shutil.which("git")
    if not git:
        pytest.skip("git not on PATH")

    origin = str(tmp_path / "origin.git")
    seed = str(tmp_path / "seed")
    # Seed working repo → push two commits to a bare origin.
    subprocess.run(["git", "init", "-q", "-b", "main", seed], check=True, env=_GIT_ENV)
    (tmp_path / "seed" / "base.txt").write_text("base\n")
    _git(seed, "add", "base.txt")
    _git(seed, "commit", "-q", "-m", "c1 base")
    c1 = _git(seed, "rev-parse", "HEAD")
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", origin], check=True, env=_GIT_ENV)
    _git(seed, "remote", "add", "origin", origin)
    _git(seed, "push", "-q", "origin", "main")

    # Clone (its local main now points at c1). Set origin/HEAD so
    # resolve_default_branch finds "main".
    clone = str(tmp_path / "clone")
    subprocess.run(["git", "clone", "-q", origin, clone], check=True, env=_GIT_ENV)
    _git(clone, "remote", "set-head", "origin", "main")

    # Origin advances: add the dependency file via the seed repo, push.
    (tmp_path / "seed" / "dep.txt").write_text("merged dependency code\n")
    _git(seed, "add", "dep.txt")
    _git(seed, "commit", "-q", "-m", "c2 add dependency")
    _git(seed, "push", "-q", "origin", "main")

    # Sanity: clone's LOCAL main is still at c1 (stale, pre-merge); it
    # has NOT fetched the dependency commit. This is the bug's premise.
    assert _git(clone, "rev-parse", "HEAD") == c1
    assert not os.path.exists(os.path.join(clone, "dep.txt"))
    return clone, "main"


def test_real_git_worker_worktree_contains_merged_dependency(_stale_clone, tmp_path):
    """fails-on-parent: with the fix, the worker worktree branches off the
    freshly-fetched origin/main and CONTAINS dep.txt. The pre-fix code
    branched off the clone's stale local HEAD (c1) → dep.txt absent."""
    clone, default = _stale_clone
    assert worktree_mod.resolve_default_branch(clone) == default

    # The exact sequence the dispatch path runs: fetch origin <default>,
    # then create the worktree off origin/<default>.
    fetch_res = worktree_mod.fetch_remote(clone, default)
    assert fetch_res.error == "", fetch_res.error

    wt = str(tmp_path / "wt" / "alpha-1234")
    res = worktree_mod.create_worktree(
        clone, wt, "worker/alpha-1234", base=f"origin/{default}",
    )
    assert res.error == "", res.error

    # The dependency's merged file is present in the worker's tree.
    dep = os.path.join(wt, "dep.txt")
    assert os.path.exists(dep), (
        "worker worktree is missing the merged dependency file — it "
        "branched off stale local HEAD instead of origin/main"
    )
    assert open(dep).read() == "merged dependency code\n"


def test_real_git_branch_off_local_head_misses_dependency(_stale_clone, tmp_path):
    """Pins the BUG: branching the worktree off the clone's local HEAD
    (no base, no fetch — old behavior) yields a tree WITHOUT dep.txt.
    Guards against a regression that drops the fetch+base wiring."""
    clone, _default = _stale_clone
    wt = str(tmp_path / "wt-stale" / "alpha-1234")
    # Old behavior: base="" → branch off local HEAD (c1, stale).
    res = worktree_mod.create_worktree(clone, wt, "stale/alpha-1234")
    assert res.error == "", res.error
    assert not os.path.exists(os.path.join(wt, "dep.txt")), (
        "expected stale tree to miss the dependency — if this file "
        "exists the fixture is not modeling origin-ahead-of-local"
    )


# ---------- REAL-GIT integration: no-origin-ref fallback (codex [P2]) ----------
#
# A repo with NO `origin` remote (or one whose origin/<default> was never
# fetched) has no origin/<default> object. Forcing base="origin/<default>"
# would make `git worktree add` fatal "invalid reference", skipping the
# dispatch FOREVER — a regression vs the old base="" (local HEAD) path.
# The ref_exists guard must fall back to local HEAD so dispatch proceeds.


def test_ref_exists_false_for_unfetched_origin_real_git(tmp_path):
    """A freshly-init repo with no remote has no origin/main object →
    ref_exists returns False (drives the local-HEAD fallback)."""
    if not shutil.which("git"):
        pytest.skip("git not on PATH")
    repo = str(tmp_path / "lonely")
    subprocess.run(["git", "init", "-q", "-b", "main", repo], check=True, env=_GIT_ENV)
    (tmp_path / "lonely" / "f.txt").write_text("x\n")
    _git(repo, "add", "f.txt")
    _git(repo, "commit", "-q", "-m", "c1")
    # No origin remote at all → origin/main can't resolve.
    assert worktree_mod.ref_exists(repo, "origin/main") is False
    # Local HEAD obviously resolves.
    assert worktree_mod.ref_exists(repo, "HEAD") is True


def test_real_git_dispatch_falls_back_to_local_head_without_origin(tmp_path):
    """fails-on-parent: a real cap=2 dispatch against a repo with NO
    origin remote must still create the worker worktree (off local HEAD).
    Pre-fix (force base=origin/main) → `git worktree add` fatals and the
    dispatch is skipped; this guards the codex [P2] regression."""
    if not shutil.which("git"):
        pytest.skip("git not on PATH")
    repo = str(tmp_path / "noremote")
    subprocess.run(["git", "init", "-q", "-b", "main", repo], check=True, env=_GIT_ENV)
    (tmp_path / "noremote" / "base.txt").write_text("base\n")
    _git(repo, "add", "base.txt")
    _git(repo, "commit", "-q", "-m", "c1 base")

    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / "wt" / "alpha-1234")

    # Real git for worktree ops; only stub the fleet CLI (worktree-path)
    # and the dispatch helpers (shared _git_router shells real git so
    # current_branch / fetch / ref_exists / worktree add hit the repo).
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_git_router(repo, wt_path)):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 1, f"expected one action, got: {actions}"
    assert actions[0].error == "", f"dispatch wedged: {actions[0].error}"
    assert actions[0].worktree == wt_path
    # The worktree was really created off local HEAD (base.txt present).
    assert os.path.exists(os.path.join(wt_path, "base.txt"))


# ---------- REAL-GIT integration: honor the coord's checked-out branch (codex [P1]) ----------
#
# The operator can deliberately check the coord out on a stacked/integration
# branch ahead of origin/<default>. The worker must branch off THAT branch's
# fresh upstream (origin/<integration>), not origin/<default> — otherwise it
# drops the integration branch's commits. We also prove freshness: a dependency
# that merged to origin/<integration> after the clone's last pull must appear.


def _git_router(repo, wt_path):
    """Router that shells real git but stubs the fleet worktree-path CLI."""
    real_run = subprocess.run

    def _router(cmd, *args, **kwargs):
        if cmd[:3] == ["/usr/local/bin/fleet", "workers", "worktree-path"]:
            return _ok(stdout=wt_path + "\n")
        if cmd[:1] == ["git"]:
            return real_run(*([cmd] + list(args)), **kwargs)
        emu = _maybe_claims_emulator(cmd, kwargs)
        if emu is not None:
            return emu
        return _ok()
    return _router


def test_real_git_dispatch_honors_stacked_integration_branch(tmp_path):
    """fails-on-parent (vs the origin/<default>-forcing variant): the coord
    is checked out on `integration` (ahead of origin/main). The worker tree
    must contain (a) the integration branch's own commit AND (b) a dependency
    merged to origin/integration after the clone's last pull — while NOT
    being based on origin/main."""
    if not shutil.which("git"):
        pytest.skip("git not on PATH")
    origin = str(tmp_path / "origin.git")
    seed = str(tmp_path / "seed")
    subprocess.run(["git", "init", "-q", "-b", "main", seed], check=True, env=_GIT_ENV)
    (tmp_path / "seed" / "base.txt").write_text("base\n")
    _git(seed, "add", "base.txt")
    _git(seed, "commit", "-q", "-m", "c1 main base")
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", origin], check=True, env=_GIT_ENV)
    _git(seed, "remote", "add", "origin", origin)
    _git(seed, "push", "-q", "origin", "main")
    # Operator builds an integration branch with its own commit, pushes it.
    _git(seed, "checkout", "-q", "-b", "integration")
    (tmp_path / "seed" / "integration.txt").write_text("integration-only work\n")
    _git(seed, "add", "integration.txt")
    _git(seed, "commit", "-q", "-m", "c2 integration feature")
    _git(seed, "push", "-q", "origin", "integration")

    # Clone, check out integration tracking origin/integration.
    clone = str(tmp_path / "clone")
    subprocess.run(["git", "clone", "-q", origin, clone], check=True, env=_GIT_ENV)
    _git(clone, "checkout", "-q", "-b", "integration", "origin/integration")
    int_c2 = _git(clone, "rev-parse", "HEAD")

    # origin/integration advances with a dependency commit; clone is stale.
    (tmp_path / "seed" / "dep.txt").write_text("merged dependency on integration\n")
    _git(seed, "add", "dep.txt")
    _git(seed, "commit", "-q", "-m", "c3 dep on integration")
    _git(seed, "push", "-q", "origin", "integration")
    # Clone's local integration still at c2 (pre-dep), no dep.txt yet.
    assert _git(clone, "rev-parse", "HEAD") == int_c2
    assert not os.path.exists(os.path.join(clone, "dep.txt"))

    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / "wt" / "alpha-1234")
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_git_router(clone, wt_path)):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd=clone, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 1 and actions[0].error == "", f"actions: {actions}"
    # (b) Fresh: the dependency merged to origin/integration is present.
    assert os.path.exists(os.path.join(wt_path, "dep.txt")), (
        "worker tree missing the fresh dependency from origin/integration"
    )
    # (a) Honors the integration branch: its own commit's file is present.
    # (If the worker were based on origin/main this would be absent.)
    assert os.path.exists(os.path.join(wt_path, "integration.txt")), (
        "worker tree missing integration branch's own commit — it was "
        "wrongly based on origin/main instead of origin/integration"
    )


def test_real_git_dispatch_honors_differently_named_upstream(tmp_path):
    """fails-on-parent (vs assuming origin/<current>): the coord is on a
    LOCAL branch `work` that tracks origin/main (different name). When
    origin/main advances, the worker must fetch + branch off origin/main
    (the real @{upstream}) — not the nonexistent origin/work, which the
    origin/<current> assumption would miss, dropping it to stale local
    HEAD (codex [P2])."""
    if not shutil.which("git"):
        pytest.skip("git not on PATH")
    origin = str(tmp_path / "origin.git")
    seed = str(tmp_path / "seed")
    subprocess.run(["git", "init", "-q", "-b", "main", seed], check=True, env=_GIT_ENV)
    (tmp_path / "seed" / "base.txt").write_text("base\n")
    _git(seed, "add", "base.txt")
    _git(seed, "commit", "-q", "-m", "c1 base")
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", origin], check=True, env=_GIT_ENV)
    _git(seed, "remote", "add", "origin", origin)
    _git(seed, "push", "-q", "origin", "main")

    clone = str(tmp_path / "clone")
    subprocess.run(["git", "clone", "-q", origin, clone], check=True, env=_GIT_ENV)
    # Local branch `work` tracking origin/main (DIFFERENT name).
    _git(clone, "checkout", "-q", "-b", "work", "--track", "origin/main")
    local_c1 = _git(clone, "rev-parse", "HEAD")
    assert worktree_mod.upstream_ref(clone, "work") == "origin/main"

    # origin/main advances with a dependency; clone's local `work` is stale.
    (tmp_path / "seed" / "dep.txt").write_text("merged dependency on main\n")
    _git(seed, "add", "dep.txt")
    _git(seed, "commit", "-q", "-m", "c2 dep on main")
    _git(seed, "push", "-q", "origin", "main")
    assert _git(clone, "rev-parse", "HEAD") == local_c1
    assert not os.path.exists(os.path.join(clone, "dep.txt"))

    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / "wt" / "alpha-1234")
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_git_router(clone, wt_path)):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd=clone, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 1 and actions[0].error == "", f"actions: {actions}"
    # The dependency from the REAL upstream (origin/main) is present — proves
    # we read @{upstream} rather than assuming origin/work (which doesn't exist).
    assert os.path.exists(os.path.join(wt_path, "dep.txt")), (
        "worker tree missing the dependency from origin/main — the "
        "differently-named upstream was not resolved (stale local HEAD)"
    )


def test_real_git_dispatch_preserves_local_commits_ahead_of_upstream(tmp_path):
    """fails-on-parent (vs always-use-upstream): the coord's branch has an
    upstream BUT local commits AHEAD of it (operator's un-pushed work). The
    worker must branch off local HEAD so those commits survive — the
    ancestry gate must NOT swap in the upstream (codex [P1] #2)."""
    if not shutil.which("git"):
        pytest.skip("git not on PATH")
    origin = str(tmp_path / "origin.git")
    seed = str(tmp_path / "seed")
    subprocess.run(["git", "init", "-q", "-b", "main", seed], check=True, env=_GIT_ENV)
    (tmp_path / "seed" / "base.txt").write_text("base\n")
    _git(seed, "add", "base.txt")
    _git(seed, "commit", "-q", "-m", "c1 base")
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", origin], check=True, env=_GIT_ENV)
    _git(seed, "remote", "add", "origin", origin)
    _git(seed, "push", "-q", "origin", "main")

    clone = str(tmp_path / "clone")
    subprocess.run(["git", "clone", "-q", origin, clone], check=True, env=_GIT_ENV)
    # Operator makes a LOCAL commit on main, ahead of origin/main, un-pushed.
    (tmp_path / "clone" / "local_wip.txt").write_text("operator local work\n")
    _git(clone, "add", "local_wip.txt")
    _git(clone, "commit", "-q", "-m", "local wip ahead of upstream")
    # Sanity: local HEAD is NOT an ancestor of origin/main (it's ahead).
    assert worktree_mod.is_ancestor(clone, "HEAD", "origin/main") is False

    t = _ready_task("alpha-1234")
    wt_path = str(tmp_path / "wt" / "alpha-1234")
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_git_router(clone, wt_path)):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd=clone, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 1 and actions[0].error == "", f"actions: {actions}"
    # The operator's local-only commit survives in the worker tree.
    assert os.path.exists(os.path.join(wt_path, "local_wip.txt")), (
        "worker tree dropped the operator's local commit — it was wrongly "
        "based on origin/main instead of local HEAD"
    )


# ---------- codex iter-5 [P2]: legacy pending-acquire reuse across gens ----------


def _legacy_pending_coord_state(slug: str, agent_id: str) -> dict:
    """A pre-migration bare-STRING pending-acquire entry (no dispatch_kind /
    dispatch_generation record shape) for `slug`."""
    return {"pending_acquire_agent_ids": {slug: agent_id}}


def test_dispatch_ready_legacy_pending_NOT_reused_on_gen_gt0(tmp_path) -> None:
    """codex iter-5 [P2]: a legacy bare-string pending-acquire entry must
    NOT be reused once the task row authority is gen>0 (e.g. floored to 1
    on a prior requeue). Reusing the legacy gen-0 prompt would have
    _apply_dispatch skip the generation persist + run an ungated
    `fleet workers update` that the gen>0 CAS rejects, wedging the slug.
    The legacy entry must be FORGOTTEN and a fresh id minted."""
    t = _ready_task("legdisp-aaaa")
    t.dispatch_generation = 1  # row already advanced past legacy 0
    legacy_id = "0ff10001"
    coord_state = _legacy_pending_coord_state("legdisp-aaaa", legacy_id)

    def _runner(cmd, *args, **kwargs):
        emu = _maybe_claims_emulator(cmd, kwargs)
        return emu if emu is not None else _ok()

    with patch.object(dispatch_mod, "fetch_standards", return_value="# S"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_runner):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd="/repo", cap=1,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
            coord_state=coord_state,
        )

    assert len(actions) == 1 and actions[0].error == "", f"actions: {actions}"
    # The fresh dispatch reaches gen 2 (next_gen = 1 + 1), NOT the legacy
    # gen 0, and does NOT reuse the legacy id.
    assert actions[0].dispatch_generation == 2, actions[0].dispatch_generation
    assert actions[0].agent_id != legacy_id, "legacy gen-0 id wrongly reused on gen>0 row"
    # The legacy pending entry was forgotten.
    remaining = loop.supervisor_mod.load_pending_acquire_agent_id_map(coord_state)
    assert remaining.get("legdisp-aaaa") in (None, actions[0].agent_id), remaining


def test_dispatch_ready_legacy_pending_reused_on_gen0(tmp_path) -> None:
    """The legacy reuse path STILL applies while the row is gen 0 (true
    pre-migration first attempt): reuse the legacy id + keep gen 0 so the
    already-acquired ungated prompt still matches."""
    t = _ready_task("legdisp-bbbb")
    t.dispatch_generation = 0
    legacy_id = "0ff10002"
    coord_state = _legacy_pending_coord_state("legdisp-bbbb", legacy_id)

    def _runner(cmd, *args, **kwargs):
        emu = _maybe_claims_emulator(cmd, kwargs)
        return emu if emu is not None else _ok()

    with patch.object(dispatch_mod, "fetch_standards", return_value="# S"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=_runner):
        actions = loop._dispatch_ready(
            tasks=[t], project="proj", cwd="/repo", cap=1,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
            coord_state=coord_state,
        )

    assert len(actions) == 1 and actions[0].error == "", f"actions: {actions}"
    assert actions[0].dispatch_generation == 0, "legacy gen-0 reuse must keep gen 0"
    assert actions[0].agent_id == legacy_id, "legacy id should be reused on a gen-0 row"
