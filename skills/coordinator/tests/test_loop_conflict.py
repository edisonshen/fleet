"""loop._dispatch_ready conflict-aware tests (cap > 1).

The conflict heuristic itself lives in conflict.py and is exercised by
test_conflict.py. This file covers loop.py's conservative wrapper:

  - cap > 1 with two ready tasks whose Files: declarations overlap →
    only the first dispatches; the second is skipped this tick.
  - cap > 1 with disjoint Files: declarations → both dispatch.
  - cap = 1 → wrapper is bypassed entirely (regression-safe path).
  - candidate has no Files: line and a worker is already in-flight →
    skipped (conservative; could touch anything).
  - direct unit test of loop._has_conflict_with_inflight.

Subprocess + worktree creation are mocked the same way as
test_dispatch_worktree.py: a single side_effect router routes git +
fleet argv shapes to canned CompletedProcess responses.
"""
from __future__ import annotations

import os
import subprocess
from unittest.mock import patch

import dispatch as dispatch_mod
import loop
import parse


def _ok(stdout: str = "", stderr: str = "") -> subprocess.CompletedProcess:
    return subprocess.CompletedProcess(args=[], returncode=0, stdout=stdout, stderr=stderr)


def _ready(slug: str, spec: str = "Do the thing.") -> parse.Task:
    return parse.Task(
        slug=slug, status="ready", priority="P1",
        spec=spec, acceptance="", notes="",
    )


def _in_progress(slug: str, spec: str = "") -> parse.Task:
    return parse.Task(
        slug=slug, status="in-progress", priority="P1",
        spec=spec, acceptance="", notes="",
        worker_pid=99999,
    )


def _claims_emulator(cmd, kwargs):
    """Emulate fleet claims acquire-prompt: write inbox + emit envelope.

    PR1 dispatch-lifecycle: loop.py shells out to `fleet claims
    acquire-prompt` rather than writing the inbox directly. Tests
    that intercept subprocess.run need to emulate the CLI's side
    effects (file written) AND its JSON envelope output so the
    Python caller can parse a valid response.
    """
    if len(cmd) < 4 or cmd[1:3] != ["claims", "acquire-prompt"]:
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


def _make_router(routes: dict, calls: list | None = None):
    """Route subprocess.run argv prefixes to canned responses, defaulting
    to _ok() so unmatched calls don't crash the test fixture."""
    def _run(cmd, *_args, **_kwargs):
        if calls is not None:
            calls.append(list(cmd))
        for prefix, result in routes.items():
            if tuple(cmd[: len(prefix)]) == prefix:
                return result
        emu = _claims_emulator(cmd, _kwargs)
        if emu is not None:
            return emu
        return _ok()
    return _run


# ---------- _has_conflict_with_inflight unit ----------


def test_has_conflict_with_inflight_empty_inflight_returns_false() -> None:
    """No in-flight workers → never a conflict, even with no Files line."""
    candidate = _ready("alpha-1234", spec="No paths here.")
    assert loop._has_conflict_with_inflight(candidate, []) is False


def test_has_conflict_with_inflight_no_files_is_conservative() -> None:
    """Candidate without Files: → treated as "could touch anything"."""
    candidate = _ready("alpha-1234", spec="No paths here.")
    other = _in_progress("bravo-1234", spec="Files: internal/agent/agent.go")
    assert loop._has_conflict_with_inflight(candidate, [other]) is True


def test_has_conflict_with_inflight_inflight_no_files_is_conservative() -> None:
    """Symmetric: in-flight task without Files: blocks every candidate."""
    candidate = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    other = _in_progress("bravo-1234", spec="No paths here either.")
    assert loop._has_conflict_with_inflight(candidate, [other]) is True


def test_has_conflict_with_inflight_disjoint_files_returns_false() -> None:
    candidate = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    other = _in_progress("bravo-1234", spec="Files: internal/agent/agent.go")
    assert loop._has_conflict_with_inflight(candidate, [other]) is False


def test_has_conflict_with_inflight_overlapping_files_returns_true() -> None:
    candidate = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    other = _in_progress("bravo-1234", spec="Files: internal/tasks/tasks.go")
    assert loop._has_conflict_with_inflight(candidate, [other]) is True


def test_has_conflict_with_inflight_self_slug_skipped() -> None:
    """Same-slug pair (stale snapshot) is not a conflict."""
    candidate = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    same = _in_progress("alpha-1234", spec="Files: internal/tasks/tasks.go")
    assert loop._has_conflict_with_inflight(candidate, [same]) is False


def test_has_conflict_with_inflight_inline_path_is_not_scope_declaration() -> None:
    """Codex iter-1 [P1] regress: a Spec narrative that *mentions* a
    file in prose ("Investigate panic in cmd/fleet/main.go") must NOT
    count as a scope declaration. Without an explicit `Files:` line
    the candidate is treated as "could touch anything" and is skipped
    while any worker is in flight — even if the inline-mentioned path
    happens to be disjoint from the in-flight task's declared scope.
    """
    candidate = _ready(
        "alpha-1234",
        spec="Investigate panic in cmd/fleet/main.go",
    )
    other = _in_progress(
        "bravo-1234", spec="Files: internal/agent/agent.go",
    )
    # `extract_paths` (legacy) would surface `cmd/fleet/main.go` from
    # the candidate's prose and judge the pair disjoint → optimistic
    # non-conflict. The wrapper must override that and skip.
    assert loop._has_conflict_with_inflight(candidate, [other]) is True


def test_has_conflict_with_inflight_inline_path_in_inflight_is_not_scope() -> None:
    """Symmetric: in-flight task with only an inline mention (no Files:
    line) is opaque, not declared. Candidate skipped even with disjoint
    declared scope.
    """
    candidate = _ready(
        "alpha-1234", spec="Files: internal/tasks/tasks.go",
    )
    other = _in_progress(
        "bravo-1234",
        spec="Refactoring loop.py for the auth flow.",
    )
    assert loop._has_conflict_with_inflight(candidate, [other]) is True


def test_has_conflict_with_inflight_partial_overlap_in_a_set() -> None:
    """One disjoint and one overlapping in-flight → still True."""
    candidate = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    disjoint = _in_progress(
        "bravo-1234", spec="Files: internal/agent/agent.go",
    )
    overlap = _in_progress(
        "charlie-1234", spec="Files: internal/tasks/tasks.go",
    )
    assert loop._has_conflict_with_inflight(
        candidate, [disjoint, overlap],
    ) is True


# ---------- _dispatch_ready cap > 1 integration ----------


def test_dispatch_ready_cap2_skips_overlapping_task(tmp_path) -> None:
    """Two ready tasks share a file path; one dispatches, one is skipped.

    The first task in priority+slug order claims the file scope; the
    second sees it in `in_flight_after_dispatch` and skips this tick.
    """
    repo = "/repo"
    wt_a = str(tmp_path / ".fleet/projects/proj/worktrees/alpha-1234")
    wt_b = str(tmp_path / ".fleet/projects/proj/worktrees/bravo-1234")
    a = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    b = _ready("bravo-1234", spec="Files: internal/tasks/tasks.go")

    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path", "--project", "proj", "alpha-1234"):
            _ok(stdout=wt_a + "\n"),
        ("/usr/local/bin/fleet", "workers", "worktree-path", "--project", "proj", "bravo-1234"):
            _ok(stdout=wt_b + "\n"),
        ("git", "-C", repo, "worktree", "add"): _ok(),
    }
    calls: list = []
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_router(routes, calls),
         ):
        actions = loop._dispatch_ready(
            tasks=[a, b], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    # Only one action — the second task was skipped before any dispatch
    # work happened, so it doesn't appear as an error-bearing action.
    assert len(actions) == 1, f"expected one dispatch, got: {actions}"
    assert actions[0].error == ""
    assert actions[0].slug == "alpha-1234"
    # Issue #84 Phase A: dispatch instruction emitted for the
    # dispatched task only.
    assert actions[0].dispatch_instruction.startswith("DISPATCH: alpha-1234")
    # `git worktree add` ran exactly once — for alpha only. The skipped
    # task must NOT have created its worktree (the conflict gate runs
    # before the worktree-creation block).
    git_add_calls = [
        c for c in calls
        if len(c) >= 5 and c[:5] == ["git", "-C", repo, "worktree", "add"]
    ]
    assert len(git_add_calls) == 1, (
        f"expected one worktree add for the dispatched task; saw: {git_add_calls}"
    )
    assert wt_a in git_add_calls[0]
    assert all(wt_b not in c for c in git_add_calls), (
        f"skipped task must not create its worktree: {git_add_calls}"
    )
    # Issue #84 Phase A: no `fleet dispatch` shell-out anywhere.
    disp_calls = [c for c in calls if len(c) >= 2 and c[1] == "dispatch"]
    assert len(disp_calls) == 0, f"unexpected fleet dispatch calls: {disp_calls}"


def test_dispatch_ready_cap2_dispatches_disjoint_tasks(tmp_path) -> None:
    """Disjoint Files: declarations → both tasks dispatch in the same tick."""
    repo = "/repo"
    wt_a = str(tmp_path / ".fleet/projects/proj/worktrees/alpha-1234")
    wt_b = str(tmp_path / ".fleet/projects/proj/worktrees/bravo-1234")
    a = _ready("alpha-1234", spec="Files: internal/tasks/tasks.go")
    b = _ready("bravo-1234", spec="Files: internal/agent/agent.go")

    # Issue #84 Phase A: no `fleet dispatch` subprocess. The router
    # only resolves worktree-path + git worktree add; dispatch is
    # in-process via mint_agent_id + write_inbox + format_instruction.
    def router(cmd, *_args, **_kwargs):
        if cmd[:3] == ["/usr/local/bin/fleet", "workers", "worktree-path"]:
            slug = cmd[-1]
            return _ok(stdout=(wt_a if slug == "alpha-1234" else wt_b) + "\n")
        if cmd[:5] == ["git", "-C", repo, "worktree", "add"]:
            return _ok()
        return _ok()

    calls: list = []

    def recorder(cmd, *args, **kwargs):
        calls.append(list(cmd))
        emu = _claims_emulator(cmd, kwargs)
        if emu is not None:
            return emu
        return router(cmd, *args, **kwargs)

    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(dispatch_mod.subprocess, "run", side_effect=recorder):
        actions = loop._dispatch_ready(
            tasks=[a, b], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 2, f"expected two dispatches, got: {actions}"
    assert all(a.error == "" for a in actions), actions
    slugs = sorted(a.slug for a in actions)
    assert slugs == ["alpha-1234", "bravo-1234"]
    # Issue #84 Phase A: each dispatch yields a fresh DISPATCH block
    # with a distinct minted agent_id; coord agent invokes Agent
    # tool once per block.
    blocks = [a.dispatch_instruction for a in actions]
    assert all(b.startswith("DISPATCH: ") for b in blocks)
    agent_ids = sorted(a.agent_id for a in actions)
    assert len(set(agent_ids)) == 2, f"duplicate agent_ids: {agent_ids}"
    # No `fleet dispatch` subprocess shell-out — pin the invariant.
    disp_calls = [c for c in calls if len(c) >= 2 and c[1] == "dispatch"]
    assert len(disp_calls) == 0, f"unexpected fleet dispatch calls: {disp_calls}"


def test_dispatch_ready_cap2_skips_no_files_when_inflight_present(tmp_path) -> None:
    """Conservative no-files behavior: a ready task with no Files: line
    is skipped if any worker is already in-flight (could touch anything).
    """
    repo = "/repo"
    wt_b = str(tmp_path / ".fleet/projects/proj/worktrees/bravo-1234")
    in_flight = _in_progress(
        "alpha-1234", spec="Files: internal/tasks/tasks.go",
    )
    candidate = _ready(
        "bravo-1234", spec="Refactor the auth flow.",  # no Files: line
    )

    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path"):
            _ok(stdout=wt_b + "\n"),
        ("git", "-C", repo, "worktree", "add"): _ok(),
    }
    calls: list = []
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_router(routes, calls),
         ):
        actions = loop._dispatch_ready(
            tasks=[in_flight, candidate], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    # active=1 (in_flight counts), cap=2 — there's headroom. But the
    # candidate has no Files: line and gets conservatively skipped.
    assert actions == [], (
        f"no-files candidate should be conservatively skipped, got: {actions}"
    )
    # No git worktree add. Issue #84 Phase A: no `fleet dispatch`
    # subprocess (the path was removed entirely).
    assert not any(c[:5] == ["git", "-C", repo, "worktree", "add"] for c in calls), calls
    assert not any(len(c) >= 2 and c[1] == "dispatch" for c in calls), calls


# ---------- cap = 1 regression guard ----------


def test_dispatch_ready_cap1_unaffected_by_conflict_logic(tmp_path) -> None:
    """cap=1 bypasses the conflict wrapper entirely. A ready task whose
    spec has no Files: line dispatches even though it would otherwise be
    conservatively skipped. Single-worker mode is byte-identical to
    v0.2.0 — overlap is impossible (only one in-progress worker at a
    time) so the wrapper would be a no-op anyway, but the bypass is
    explicit so future tightening of the heuristic can't accidentally
    regress single-worker mode.
    """
    candidate = _ready(
        "alpha-1234", spec="Refactor the auth flow.",  # no Files:
    )
    # Issue #84 Phase A: no `fleet dispatch` shell-out anymore. The
    # router has nothing to match for the dispatch path; the in-process
    # mint+inbox+format flow runs unmocked.
    routes: dict = {}
    calls: list = []
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_router(routes, calls),
         ):
        actions = loop._dispatch_ready(
            tasks=[candidate], project="proj", cwd="/repo", cap=1,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )

    assert len(actions) == 1
    assert actions[0].error == ""
    # Issue #84 Phase A: spawn signal is the DISPATCH instruction.
    assert actions[0].dispatch_instruction.startswith("DISPATCH: alpha-1234")
    # No git invocations in cap=1 mode.
    assert not any(c[:1] == ["git"] for c in calls), calls
    # No `fleet dispatch` shell-out either.
    disp_calls = [c for c in calls if len(c) >= 2 and c[1] == "dispatch"]
    assert len(disp_calls) == 0


def test_dispatch_ready_cap2_overlap_clears_after_first_completes(tmp_path) -> None:
    """Across-tick semantics: a task skipped this tick is reconsidered
    next tick once the conflicting worker is gone from in_flight.

    Drive two _dispatch_ready calls back-to-back. First call: tasks A
    (in-flight) and B (ready, overlap) — B skipped. Second call: A is
    no longer in tasks list (simulating it transitioned to in-review or
    done), B becomes the only candidate and dispatches.
    """
    repo = "/repo"
    wt_b = str(tmp_path / ".fleet/projects/proj/worktrees/bravo-1234")
    a = _in_progress("alpha-1234", spec="Files: internal/tasks/tasks.go")
    b = _ready("bravo-1234", spec="Files: internal/tasks/tasks.go")

    routes = {
        ("/usr/local/bin/fleet", "workers", "worktree-path"):
            _ok(stdout=wt_b + "\n"),
        ("git", "-C", repo, "worktree", "add"): _ok(),
    }
    with patch.object(dispatch_mod, "fetch_standards", return_value="# Standards"), \
         patch.object(dispatch_mod, "fetch_learnings", return_value=""), \
         patch.object(
             dispatch_mod.subprocess, "run",
             side_effect=_make_router(routes),
         ):
        # Tick 1: A in-flight, B ready and overlapping → B skipped.
        actions_1 = loop._dispatch_ready(
            tasks=[a, b], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )
        assert actions_1 == []
        # Tick 2: A no longer present (simulating done) → B dispatches.
        actions_2 = loop._dispatch_ready(
            tasks=[b], project="proj", cwd=repo, cap=2,
            fleet_bin="/usr/local/bin/fleet",
            fleet_home=str(tmp_path),
        )
        assert len(actions_2) == 1
        assert actions_2[0].slug == "bravo-1234"
        assert actions_2[0].error == ""
        # Issue #84 Phase A: the across-tick re-consideration emits a
        # DISPATCH block (the spawn signal) for B.
        assert actions_2[0].dispatch_instruction.startswith("DISPATCH: bravo-1234")
