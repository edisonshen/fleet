"""Deterministic finisher: run inside the coordinator tick once the
reviewer has written phase=review-done (or a prior tick died at phase=push).

    git:      verification gate -> phase=push (review gate) -> git push
              -> gh pr create | gh pr edit (open PR on the branch) -> phase=done
    non-git:  verification gate -> tasks note (## Verification, once)
              -> phase=done

Every step is idempotent so a tick that dies mid-run can redo the whole
sequence on the next tick. Any push / gh failure (including a timeout or a
missing binary) flips the worker to phase=blocked with the error inlined;
only a failure to write state at all is returned as `error` (the tick logs
it and retries).
"""
from __future__ import annotations

import json
import os
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

RunFn = Callable[..., subprocess.CompletedProcess]

FLEET_TIMEOUT_S = 30.0
GIT_PUSH_TIMEOUT_S = 180.0
GH_TIMEOUT_S = 90.0

NOTE_MARKER = "finisher-note.done"


class CommandError(Exception):
    """An external command could not run (timeout, missing binary, OS error)."""


@dataclass
class FinishResult:
    slug: str
    pr_url: str = ""
    blocked_reason: str = ""
    error: str = ""
    steps: list[str] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return not self.blocked_reason and not self.error


def section_nonempty(text: str, header: str) -> bool:
    """True iff the `## <header>` section of a markdown body has a non-blank line."""
    inside = False
    for line in text.splitlines():
        if line.rstrip() == header:
            inside = True
            continue
        if line.startswith("## "):
            inside = False
            continue
        if inside and line.strip():
            return True
    return False


def verification_gate(verification_md: Path) -> str:
    """Return "" when verification.md carries evidence, else the blocked reason."""
    try:
        text = verification_md.read_text(encoding="utf-8")
    except OSError:
        return f"finisher: no verification evidence ({verification_md})"
    if not section_nonempty(text, "## Gates") or not section_nonempty(text, "## Evidence — after"):
        return f"finisher: no verification evidence ({verification_md})"
    return ""


def _one_line(text: str, limit: int = 300) -> str:
    s = " ".join((text or "").split())
    return s if len(s) <= limit else s[: limit - 1] + "…"


def _review_section(state: dict) -> str:
    def s(k: str) -> str:
        v = state.get(k, "")
        return str(v) if v is not None else ""

    alpha = s("review_alpha_status") or "unknown"
    if alpha == "skipped" and s("review_alpha_skip_reason"):
        alpha = f"skipped:{s('review_alpha_skip_reason')}"
    return "\n".join([
        "## Review",
        f"- alpha ({s('review_alpha_engine')}/{s('review_alpha_model')}): "
        f"{alpha} (rounds: {s('review_alpha_rounds') or 0})",
        f"- beta ({s('review_beta_engine')}/{s('review_beta_model')}): "
        f"{s('review_beta_status') or 'unknown'} (rounds: {s('review_beta_rounds') or 0})",
    ])


def pr_body(state: dict, commit_subjects: list[str], verification_text: str) -> str:
    summary = "\n".join(f"- {c}" for c in commit_subjects[:3]) or "- (no commit subjects)"
    return "\n".join([
        "## Summary",
        summary,
        "",
        _review_section(state),
        "",
        "## Verification",
        verification_text.rstrip(),
        "",
    ])


class Finisher:
    def __init__(
        self,
        *,
        slug: str,
        project: str,
        fleet_bin: str,
        fleet_home: Path,
        repo_dir: str,
        branch: str,
        is_git: bool,
        dispatch_generation: int = 0,
        base: str = "main",
        run: RunFn | None = None,
    ) -> None:
        self.slug = slug
        self.project = project
        self.fleet_bin = fleet_bin
        self.workers_dir = fleet_home / "projects" / project / "workers" / slug
        self.repo_dir = repo_dir
        self.branch = branch
        self.is_git = is_git
        self.gen = int(dispatch_generation)
        self.base = base
        self._run = run or subprocess.run
        self.result = FinishResult(slug=slug)

    # ---- process helpers ----

    def _exec(self, cmd: list[str], *, timeout: float, cwd: str | None = None,
              stdin: str | None = None) -> subprocess.CompletedProcess:
        try:
            return self._run(
                cmd, capture_output=True, text=True, timeout=timeout,
                check=False, cwd=cwd, input=stdin,
            )
        except subprocess.TimeoutExpired as exc:
            raise CommandError(f"{' '.join(cmd[:3])} timed out after {timeout:.0f}s") from exc
        except OSError as exc:
            raise CommandError(f"{' '.join(cmd[:3])} could not run: {exc}") from exc

    def _fleet(self, args: list[str]) -> subprocess.CompletedProcess:
        prev = os.environ.get("FLEET_TICK")
        os.environ["FLEET_TICK"] = "1"
        try:
            return self._exec([self.fleet_bin, *args], timeout=FLEET_TIMEOUT_S)
        finally:
            if prev is None:
                os.environ.pop("FLEET_TICK", None)
            else:
                os.environ["FLEET_TICK"] = prev

    def _workers_update(self, *extra: str) -> subprocess.CompletedProcess:
        args = ["workers", "update", self.slug, "--project", self.project]
        if self.gen > 0:
            args += ["--dispatch-generation", str(self.gen)]
        return self._fleet([*args, *extra])

    def _block(self, reason: str) -> FinishResult:
        reason = _one_line(reason)
        self.result.blocked_reason = reason
        try:
            proc = self._workers_update("--phase", "blocked", "--reason", reason)
        except CommandError as exc:
            return self._fail(f"finisher: phase=blocked write failed after {reason!r}: {exc}")
        if proc.returncode != 0:
            self.result.error = (
                f"finisher: phase=blocked write failed after {reason!r}: "
                f"{_one_line(proc.stderr or proc.stdout)}"
            )
        return self.result

    def _fail(self, error: str) -> FinishResult:
        self.result.error = _one_line(error)
        return self.result

    # ---- steps ----

    def _read_state(self) -> dict:
        try:
            data = json.loads((self.workers_dir / "state.json").read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {}
        return data if isinstance(data, dict) else {}

    def _git(self, *args: str, timeout: float = GIT_PUSH_TIMEOUT_S) -> subprocess.CompletedProcess:
        return self._exec(["git", *args], timeout=timeout, cwd=self.repo_dir)

    def _push(self) -> str:
        proc = self._git("push", "-u", "origin", self.branch)
        if proc.returncode == 0:
            self.result.steps.append("push")
            return ""
        err = proc.stderr or proc.stdout
        if "rejected" in err or "non-fast-forward" in err or "fetch first" in err:
            proc = self._git("push", "--force-with-lease", "origin", self.branch)
            if proc.returncode == 0:
                self.result.steps.append("push --force-with-lease")
                return ""
            err = proc.stderr or proc.stdout
        return f"finisher: git push failed — {_one_line(err)}"

    def _existing_pr(self) -> str:
        proc = self._exec(
            ["gh", "pr", "list", "--head", self.branch, "--state", "open",
             "--json", "url", "--jq", ".[0].url // empty"],
            timeout=GH_TIMEOUT_S, cwd=self.repo_dir,
        )
        return proc.stdout.strip() if proc.returncode == 0 else ""

    def _commit_subjects(self) -> list[str]:
        proc = self._git(
            "log", "--reverse", "--format=%s", f"{self.base}..{self.branch}",
            timeout=FLEET_TIMEOUT_S,
        )
        if proc.returncode != 0:
            return []
        return [ln for ln in proc.stdout.splitlines() if ln.strip()]

    def _title_body(self, verification_text: str) -> tuple[str, str]:
        subjects = self._commit_subjects()
        title = subjects[0] if subjects else self.slug
        return title, pr_body(self._read_state(), subjects, verification_text)

    def _edit_pr(self, url: str, verification_text: str) -> str:
        title, body = self._title_body(verification_text)
        proc = self._exec(
            ["gh", "pr", "edit", url, "--title", title, "--body-file", "-"],
            timeout=GH_TIMEOUT_S, cwd=self.repo_dir, stdin=body,
        )
        if proc.returncode != 0:
            return f"finisher: gh pr edit failed — {_one_line(proc.stderr or proc.stdout)}"
        return ""

    def _create_pr(self, verification_text: str) -> tuple[str, str]:
        title, body = self._title_body(verification_text)
        proc = self._exec(
            ["gh", "pr", "create", "--base", self.base, "--head", self.branch,
             "--title", title, "--body-file", "-"],
            timeout=GH_TIMEOUT_S, cwd=self.repo_dir, stdin=body,
        )
        if proc.returncode != 0:
            return "", f"finisher: gh pr create failed — {_one_line(proc.stderr or proc.stdout)}"
        url = ""
        for ln in reversed(proc.stdout.splitlines()):
            if ln.strip().startswith("http"):
                url = ln.strip()
                break
        if not url:
            return "", f"finisher: gh pr create returned no URL — {_one_line(proc.stdout)}"
        return url, ""

    # ---- entry points ----

    def run(self) -> FinishResult:
        try:
            return self._run_git() if self.is_git else self._run_non_git()
        except CommandError as exc:
            return self._block(f"finisher: {exc}")

    def _run_git(self) -> FinishResult:
        verification_md = self.workers_dir / "verification.md"
        if reason := verification_gate(verification_md):
            return self._block(reason)
        proc = self._workers_update("--phase", "push")
        if proc.returncode != 0:
            return self._block(
                "finisher: review gate rejected at phase=push — "
                + _one_line(proc.stderr or proc.stdout)
            )
        self.result.steps.append("phase=push")
        if err := self._push():
            return self._block(err)
        verification_text = verification_md.read_text(encoding="utf-8")
        url = self._existing_pr()
        if url:
            if err := self._edit_pr(url, verification_text):
                return self._block(err)
            self.result.steps.append("pr updated")
        else:
            url, err = self._create_pr(verification_text)
            if err:
                return self._block(err)
            self.result.steps.append("pr created")
        proc = self._workers_update("--phase", "done", "--pr-url", url, "--exit", "0")
        if proc.returncode != 0:
            return self._fail(
                f"finisher: phase=done write failed ({url}): "
                f"{_one_line(proc.stderr or proc.stdout)}"
            )
        self.result.pr_url = url
        self.result.steps.append("phase=done")
        return self.result

    def _run_non_git(self) -> FinishResult:
        verification_md = self.workers_dir / "verification.md"
        if reason := verification_gate(verification_md):
            return self._block(reason)
        marker = self.workers_dir / NOTE_MARKER
        if not marker.exists():
            note = (
                "finisher: local diff shipped in place (non-git)\n\n## Verification\n"
                + verification_md.read_text(encoding="utf-8")
            )
            proc = self._fleet(["tasks", "note", self.slug, "--project", self.project, note])
            if proc.returncode != 0:
                return self._fail(
                    "finisher: tasks note failed: " + _one_line(proc.stderr or proc.stdout)
                )
            tmp = marker.with_suffix(".tmp")
            tmp.write_text("", encoding="utf-8")
            os.replace(tmp, marker)
            self.result.steps.append("note")
        proc = self._workers_update("--phase", "done", "--exit", "0")
        if proc.returncode != 0:
            return self._block(
                "finisher: review gate rejected — " + _one_line(proc.stderr or proc.stdout)
            )
        self.result.steps.append("phase=done")
        return self.result


def run_finisher(**kwargs) -> FinishResult:
    return Finisher(**kwargs).run()
