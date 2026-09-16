from __future__ import annotations

import json
import subprocess
from pathlib import Path

import finisher


VERIFICATION = """# Verification

## Gates
- go test ./... PASS

## Evidence — before
fails

## Evidence — after
passes
"""


class FakeRun:
    """Scripted subprocess.run: matches a command prefix → (rc, stdout, stderr)."""

    def __init__(self, rules: list[tuple[list[str], int, str, str]] | None = None) -> None:
        self.rules = rules or []
        self.calls: list[dict] = []

    def __call__(self, cmd, *, capture_output, text, timeout, check, cwd=None, input=None):
        self.calls.append({"cmd": list(cmd), "cwd": cwd, "input": input})
        for prefix, rc, out, err in self.rules:
            if cmd[: len(prefix)] == prefix:
                return subprocess.CompletedProcess(cmd, rc, out, err)
        return subprocess.CompletedProcess(cmd, 0, "", "")

    def cmds(self) -> list[list[str]]:
        return [c["cmd"] for c in self.calls]

    def has(self, *prefix: str) -> bool:
        return any(c[: len(prefix)] == list(prefix) for c in self.cmds())


def _seed(home: Path, slug: str, *, verification: str | None = VERIFICATION) -> Path:
    wd = home / "projects" / "proj" / "workers" / slug
    wd.mkdir(parents=True)
    (wd / "state.json").write_text(json.dumps({
        "slug": slug, "phase": "review-done",
        "review_alpha_status": "passed", "review_alpha_engine": "codex",
        "review_alpha_model": "gpt-5.5-codex", "review_alpha_rounds": 1,
        "review_beta_status": "passed", "review_beta_engine": "claude",
        "review_beta_model": "opus", "review_beta_rounds": 2,
    }))
    if verification is not None:
        (wd / "verification.md").write_text(verification, encoding="utf-8")
    return wd


def _finisher(home: Path, run: FakeRun, *, is_git: bool = True, gen: int = 3) -> finisher.Finisher:
    return finisher.Finisher(
        slug="t-1", project="proj", fleet_bin="fleet", fleet_home=home,
        repo_dir="/wt/t-1", branch="worker/t-1", is_git=is_git,
        dispatch_generation=gen, run=run,
    )


def _update_cmds(run: FakeRun) -> list[list[str]]:
    return [c for c in run.cmds() if c[:3] == ["fleet", "workers", "update"]]


# ---- git happy path ----

def test_git_success_pushes_creates_pr_and_marks_done(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["git", "log"], 0, "feat: add thing\nfix: typo\n", ""),
        (["gh", "pr", "list"], 0, "", ""),
        (["gh", "pr", "create"], 0, "https://github.com/o/r/pull/9\n", ""),
    ])
    res = _finisher(tmp_path, run).run()

    assert res.ok, res
    assert res.pr_url == "https://github.com/o/r/pull/9"
    updates = _update_cmds(run)
    assert updates[0][3:] == ["t-1", "--project", "proj", "--dispatch-generation", "3", "--phase", "push"]
    assert updates[-1][-6:] == ["--phase", "done", "--pr-url", "https://github.com/o/r/pull/9", "--exit", "0"]
    push = next(c for c in run.calls if c["cmd"][:2] == ["git", "push"])
    assert push["cmd"] == ["git", "push", "-u", "origin", "worker/t-1"]
    assert push["cwd"] == "/wt/t-1"
    # order: phase=push before git push before gh pr create before phase=done
    kinds = [tuple(c[:3]) for c in run.cmds()]
    assert kinds.index(("fleet", "workers", "update")) < kinds.index(("git", "push", "-u"))
    assert kinds.index(("git", "push", "-u")) < kinds.index(("gh", "pr", "create"))


def test_pr_body_has_summary_review_and_verbatim_verification(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["git", "log"], 0, "feat: add thing\nfix: typo\n", ""),
        (["gh", "pr", "create"], 0, "https://x/pr/1\n", ""),
    ])
    _finisher(tmp_path, run).run()
    create = next(c for c in run.calls if c["cmd"][:3] == ["gh", "pr", "create"])
    cmd, body = create["cmd"], create["input"]
    assert cmd[cmd.index("--title") + 1] == "feat: add thing"
    assert cmd[cmd.index("--base") + 1] == "main"
    assert cmd[cmd.index("--head") + 1] == "worker/t-1"
    assert body.startswith("## Summary\n- feat: add thing\n- fix: typo\n")
    assert "## Review\n- alpha (codex/gpt-5.5-codex): passed (rounds: 1)\n" \
           "- beta (claude/opus): passed (rounds: 2)\n" in body
    assert body.rstrip().endswith("## Verification\n" + VERIFICATION.rstrip())


def test_alpha_skipped_reason_in_pr_body(tmp_path: Path) -> None:
    wd = _seed(tmp_path, "t-1")
    st = json.loads((wd / "state.json").read_text())
    st.update(review_alpha_status="skipped", review_alpha_skip_reason="rate-limited")
    (wd / "state.json").write_text(json.dumps(st))
    run = FakeRun([(["gh", "pr", "create"], 0, "https://x/pr/1\n", "")])
    _finisher(tmp_path, run).run()
    body = next(c for c in run.calls if c["cmd"][:3] == ["gh", "pr", "create"])["input"]
    assert "- alpha (codex/gpt-5.5-codex): skipped:rate-limited" in body


def test_existing_open_pr_is_edited_not_recreated(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["gh", "pr", "list"], 0, "https://x/pr/42\n", ""),
        (["git", "log"], 0, "fix: second attempt\n", ""),
    ])
    res = _finisher(tmp_path, run).run()
    assert res.ok and res.pr_url == "https://x/pr/42"
    assert not run.has("gh", "pr", "create")
    edit = next(c for c in run.calls if c["cmd"][:3] == ["gh", "pr", "edit"])
    assert edit["cmd"][3] == "https://x/pr/42"
    assert edit["cmd"][edit["cmd"].index("--title") + 1] == "fix: second attempt"
    assert "## Verification\n" + VERIFICATION.rstrip() in edit["input"]
    assert "pr updated" in res.steps


def test_existing_pr_edit_failure_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["gh", "pr", "list"], 0, "https://x/pr/42\n", ""),
        (["gh", "pr", "edit"], 1, "", "gh: HTTP 403"),
    ])
    res = _finisher(tmp_path, run).run()
    assert res.blocked_reason.startswith("finisher: gh pr edit failed — gh: HTTP 403")
    assert not any("done" in c for c in _update_cmds(run))


def test_resume_from_phase_push_reruns_whole_sequence(tmp_path: Path) -> None:
    wd = _seed(tmp_path, "t-1")
    st = json.loads((wd / "state.json").read_text())
    st["phase"] = "push"
    (wd / "state.json").write_text(json.dumps(st))
    run = FakeRun([(["gh", "pr", "list"], 0, "https://x/pr/42\n", "")])
    res = _finisher(tmp_path, run).run()
    assert res.ok and res.pr_url == "https://x/pr/42"
    assert run.has("git", "push", "-u")
    assert _update_cmds(run)[0][-2:] == ["--phase", "push"]
    assert _update_cmds(run)[-1][-6:-2] == ["--phase", "done", "--pr-url", "https://x/pr/42"]


def test_rejected_push_retries_with_force_with_lease_only(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["git", "push", "-u"], 1, "", "! [rejected] worker/t-1 -> worker/t-1 (non-fast-forward)"),
        (["gh", "pr", "create"], 0, "https://x/pr/1\n", ""),
    ])
    res = _finisher(tmp_path, run).run()
    assert res.ok, res
    pushes = [c for c in run.cmds() if c[:2] == ["git", "push"]]
    assert pushes[1] == ["git", "push", "--force-with-lease", "origin", "worker/t-1"]
    assert not any("--force" in c and "--force-with-lease" not in c for c in pushes)


# ---- blocked paths ----

def test_review_gate_rejection_blocks_without_push(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["fleet", "workers", "update", "t-1", "--project", "proj", "--dispatch-generation", "3", "--phase", "push"],
         1, "", "phase=push requires review: review_beta_status=\"\""),
    ])
    res = _finisher(tmp_path, run).run()
    assert res.blocked_reason.startswith("finisher: review gate rejected at phase=push")
    assert "review_beta_status" in res.blocked_reason
    assert not run.has("git", "push")
    assert not run.has("gh", "pr", "create")
    blocked = [c for c in _update_cmds(run) if "blocked" in c]
    assert blocked and blocked[0][-2] == "--reason"


def test_missing_verification_blocks_before_push(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1", verification=None)
    run = FakeRun()
    res = _finisher(tmp_path, run).run()
    assert res.blocked_reason.startswith("finisher: no verification evidence (")
    assert not run.has("git", "push")
    assert not any("--phase" in c and "push" in c for c in _update_cmds(run))


def test_empty_gates_section_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1", verification="## Gates\n\n## Evidence — after\nok\n")
    res = _finisher(tmp_path, FakeRun()).run()
    assert "no verification evidence" in res.blocked_reason


def test_empty_evidence_after_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1", verification="## Gates\n- pass\n\n## Evidence — after\n\n## Notes\nx\n")
    res = _finisher(tmp_path, FakeRun()).run()
    assert "no verification evidence" in res.blocked_reason


def test_push_failure_blocks_with_error(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([(["git", "push"], 128, "", "fatal: could not read from remote")])
    res = _finisher(tmp_path, run).run()
    assert res.blocked_reason == "finisher: git push failed — fatal: could not read from remote"
    assert not run.has("gh", "pr", "create")
    assert not any("done" in c for c in _update_cmds(run))


def test_pr_create_failure_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([(["gh", "pr", "create"], 1, "", "gh: HTTP 422 validation failed")])
    res = _finisher(tmp_path, run).run()
    assert res.blocked_reason.startswith("finisher: gh pr create failed — gh: HTTP 422")
    assert not any("done" in c for c in _update_cmds(run))


def test_done_write_failure_is_error_not_blocked(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["gh", "pr", "create"], 0, "https://x/pr/1\n", ""),
        (["fleet", "workers", "update", "t-1", "--project", "proj", "--dispatch-generation", "3", "--phase", "done"],
         1, "", "stale generation"),
    ])
    res = _finisher(tmp_path, run).run()
    assert not res.ok and not res.blocked_reason
    assert "phase=done write failed" in res.error and "https://x/pr/1" in res.error


def test_blocked_write_failure_surfaces_error(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["git", "push"], 1, "", "nope"),
        (["fleet", "workers", "update", "t-1", "--project", "proj", "--dispatch-generation", "3", "--phase", "blocked"],
         1, "", "fleet: not found"),
    ])
    res = _finisher(tmp_path, run).run()
    assert res.blocked_reason
    assert "phase=blocked write failed" in res.error


def test_command_timeout_blocks_instead_of_raising(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun()
    inner = run.__call__

    def raising(cmd, **kw):
        if cmd[:2] == ["gh", "pr"]:
            raise subprocess.TimeoutExpired(cmd, kw["timeout"])
        return inner(cmd, **kw)

    res = _finisher(tmp_path, raising).run()
    assert res.blocked_reason == "finisher: gh pr list timed out after 90s"
    assert res.error == ""
    assert any("blocked" in c for c in _update_cmds(run))


def test_missing_binary_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun()
    inner = run.__call__

    def raising(cmd, **kw):
        if cmd[0] == "git":
            raise FileNotFoundError("git")
        return inner(cmd, **kw)

    res = _finisher(tmp_path, raising).run()
    assert res.blocked_reason.startswith("finisher: git push -u could not run:")


def test_fleet_binary_failure_is_error(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")

    def raising(cmd, **kw):
        raise FileNotFoundError("fleet")

    res = _finisher(tmp_path, raising).run()
    assert not res.ok
    assert "phase=blocked write failed" in res.error


# ---- non-git ----

def test_non_git_marks_done_without_push_or_pr_and_notes_verification(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun()
    res = _finisher(tmp_path, run, is_git=False).run()
    assert res.ok and res.pr_url == ""
    assert not run.has("git")
    assert not run.has("gh")
    updates = _update_cmds(run)
    assert len(updates) == 1
    assert updates[0][-4:] == ["--phase", "done", "--exit", "0"]
    assert "--pr-url" not in updates[0]
    kinds = [tuple(c[:3]) for c in run.cmds()]
    assert kinds.index(("fleet", "tasks", "note")) < kinds.index(("fleet", "workers", "update"))
    note = next(c for c in run.cmds() if c[:3] == ["fleet", "tasks", "note"])
    assert "## Verification" in note[-1] and "passes" in note[-1]
    assert (tmp_path / "projects" / "proj" / "workers" / "t-1" / finisher.NOTE_MARKER).exists()


def test_non_git_note_failure_keeps_phase_and_retries_once(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([(["fleet", "tasks", "note"], 1, "", "disk full")])
    res = _finisher(tmp_path, run, is_git=False).run()
    assert not res.ok and "tasks note failed" in res.error
    assert _update_cmds(run) == []
    # retry after the note lands: note is written once, then phase=done
    run2 = FakeRun()
    res2 = _finisher(tmp_path, run2, is_git=False).run()
    assert res2.ok
    assert sum(1 for c in run2.cmds() if c[:3] == ["fleet", "tasks", "note"]) == 1
    run3 = FakeRun()
    _finisher(tmp_path, run3, is_git=False).run()
    assert not run3.has("fleet", "tasks", "note")
    assert _update_cmds(run3)[0][-4:] == ["--phase", "done", "--exit", "0"]


def test_non_git_missing_verification_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1", verification=None)
    run = FakeRun()
    res = _finisher(tmp_path, run, is_git=False).run()
    assert "no verification evidence" in res.blocked_reason
    assert not any("done" in c for c in _update_cmds(run))


def test_non_git_review_gate_rejection_blocks(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([
        (["fleet", "workers", "update", "t-1", "--project", "proj", "--dispatch-generation", "3", "--phase", "done"],
         1, "", "phase=done requires review"),
    ])
    res = _finisher(tmp_path, run, is_git=False).run()
    assert res.blocked_reason.startswith("finisher: review gate rejected")


# ---- helpers ----

def test_section_nonempty() -> None:
    assert finisher.section_nonempty("## Gates\nx\n", "## Gates")
    assert not finisher.section_nonempty("## Gates\n\n## Other\nx\n", "## Gates")
    assert not finisher.section_nonempty("## Other\nx\n", "## Gates")


def test_dispatch_generation_zero_omits_flag(tmp_path: Path) -> None:
    _seed(tmp_path, "t-1")
    run = FakeRun([(["gh", "pr", "create"], 0, "https://x/pr/1\n", "")])
    _finisher(tmp_path, run, gen=0).run()
    assert all("--dispatch-generation" not in c for c in _update_cmds(run))
