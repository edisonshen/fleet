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

import os
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
    """Emulate `fleet claims acquire-prompt` for loop.py tests that
    intercept subprocess.run.

    PR1 dispatch-lifecycle migration: loop.py now calls
    `fleet claims acquire-prompt` to write the inbox file. Tests that
    patched dispatch_mod.subprocess.run pre-PR1 didn't need to handle
    this argv shape; this helper preserves the existing fixture API by
    writing the inbox file + returning the JSON envelope automatically
    whenever the patched run sees the claims argv. Returns None when
    cmd isn't a claims call (caller falls through to its own routing).
    """
    if len(cmd) < 3 or cmd[1:3] != ["claims", "acquire-prompt"]:
        return None
    agent_id = cmd[3]
    env = kwargs.get("env") or os.environ
    fleet_home = env.get("FLEET_HOME") or os.path.expanduser("~/.fleet")
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


def test_remove_worktree_invokes_git_with_force(tmp_path) -> None:
    repo = str(tmp_path / "repo")
    os.makedirs(repo)
    wt = str(tmp_path / ".fleet" / "projects" / "proj" / "worktrees" / "alpha-1234")
    os.makedirs(wt)
    fake = _ok()
    with patch.object(worktree_mod.subprocess, "run", return_value=fake) as m:
        res = worktree_mod.remove_worktree(repo, wt)
    assert res.error == ""
    # Two calls: `worktree remove --force` then `worktree prune`.
    calls = [c[0][0] for c in m.call_args_list]
    assert ["git", "-C", repo, "worktree", "remove", "--force", wt] in calls
    assert ["git", "-C", repo, "worktree", "prune"] in calls


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

    # `git worktree add` ran with the expected argv.
    git_calls = [c for c in calls if c[:1] == ["git"]]
    assert any(
        c == ["git", "-C", repo, "worktree", "add", wt_path, "-b", "worker/alpha-1234"]
        for c in git_calls
    ), f"expected `git worktree add` call missing; saw: {git_calls}"

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
        payload="https://github.com/x/y/pull/1",
    )
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m_wt, \
         patch.object(loop, "_run_fleet") as m_fleet:
        loop._apply_sentinel(
            action, "proj", "fleet",
            repo=repo, tasks_by_slug={t.slug: t},
        )
    # `git worktree remove --force <wt>` ran.
    git_calls = [c[0][0] for c in m_wt.call_args_list]
    assert any("remove" in c and "--force" in c for c in git_calls), git_calls
    # tasks.md mutations happened in the right order.
    fleet_calls = [c[0][0] for c in m_fleet.call_args_list]
    assert any("pr_url=https://github.com/x/y/pull/1" in " ".join(c) for c in fleet_calls)
    assert any("status=in-review" in " ".join(c) for c in fleet_calls)
    # Worktree field cleared after remove so re-dispatch starts fresh.
    assert any("worktree=" in c[-1] and c[-1].endswith("worktree=") for c in fleet_calls)


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
        worktree=wt_path,
    )
    action = loop._ReconcileAction(
        slug="alpha-1234", new_status="done", clear_worker=True,
    )
    with patch.object(worktree_mod.subprocess, "run", return_value=_ok()) as m_wt, \
         patch.object(loop, "_run_fleet"):
        loop._apply_reconcile(
            action, "proj", "fleet",
            repo=repo, tasks_by_slug={t.slug: t},
        )
    git_calls = [c[0][0] for c in m_wt.call_args_list]
    assert any("remove" in c and wt_path in c for c in git_calls), git_calls


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
