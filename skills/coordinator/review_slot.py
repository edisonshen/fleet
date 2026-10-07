#!/usr/bin/env python3
"""Run one reviewer slot (or both concurrently) and normalize the gate result.

Single slot (`--engine --model`), exit codes:
  0 = no P0/P1 findings
  1 = P0/P1 findings (JSON stdout)
  2 = helper slot skipped (reason on stdout: rate-limited|unavailable)
  3 = blocked

Both slots (`--both --alpha-engine/--alpha-model --beta-engine/--beta-model`)
run in parallel; stdout is one JSON object
  {"alpha": {"exit": n, "findings": [...], "skip_reason": ...}, "beta": {...}}
and the exit code is 3 if either slot blocked (a skipped beta anchor counts
as blocked), else 1 if either slot has P0/P1 findings, else 0 (a skipped
helper alpha shows exit 2 in the JSON only).

Engines:
  claude  `claude -p` with an inline JSON schema. `--base` => `/review`,
          otherwise a raw structured working-tree review.
  codex   `codex review [--base]` when no --task-context (git); with
          --task-context and no --base (non-git project) `codex review`
          has no diff to work from, so run `codex exec --output-schema`
          for a raw structured review instead.

Plan mode (`--plan docs/TASK-PLAN-<slug>.md --project P --slug S`) reviews
a Markdown task plan instead of a diff: both engines get the plan-review
prompt + schema (codex always via `codex exec`), findings carry a
bug|flag|security category, and the result is persisted with the doc's
sha256 to $FLEET_HOME/projects/<P>/plan-reviews/<S>.json (see
plan_review.py). Exit codes are unchanged.

Either engine may be the dominant anchor (beta) or the optional helper
(alpha); a slot only ever execs the ONE binary named by its engine. Exit 2
is reported for a missing/rate-limited binary regardless of engine — the
reviewer prompt decides whether that is acceptable (helper) or blocks
(anchor).
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any

import plan_review


MAX_ATTEMPTS = 2
BLOCKING_SEVERITIES = {"P0", "P1"}
FINDING_RE = re.compile(r"\[(P[0-3])\]", re.IGNORECASE)
RATE_LIMIT_RE = re.compile(
    r"usage limit|rate limit|too many requests|out of token|quota",
    re.IGNORECASE,
)
UNAVAILABLE_RE = re.compile(
    r"\b(?:codex|claude): command not found\b|\bcommand not found: (?:codex|claude)\b",
    re.IGNORECASE,
)


def build_inner_schema() -> dict[str, Any]:
    """Strict schema shared by both engines.

    Codex's structured-output API rejects schemas that leave
    `additionalProperties` open or have optional properties (HTTP 400),
    so every object is closed and every property is required. Claude
    accepts the same shape.
    """
    return {
        "type": "object",
        "properties": {
            "clean": {"type": "boolean"},
            "findings": {
                "type": "array",
                "items": {
                    "type": "object",
                    "properties": {
                        "severity": {"type": "string", "enum": ["P0", "P1", "P2", "P3"]},
                        "file": {"type": "string"},
                        "line": {"type": "integer"},
                        "summary": {"type": "string"},
                    },
                    "required": ["severity", "file", "line", "summary"],
                    "additionalProperties": False,
                },
            },
        },
        "required": ["clean", "findings"],
        "additionalProperties": False,
    }


def structured_review_prompt(task_context: str | None) -> str:
    if task_context:
        return (
            "Task context (what the worker was asked to build):\n"
            f"{task_context}\n\n"
            "Review the current working-tree changes in this project against the "
            "acceptance criteria above for correctness, security, and quality. If "
            "the tree is large, focus on the files most plausibly changed for this "
            "task (review at most ~40 files); do not attempt to enumerate an "
            "unrelated whole tree. Return ONLY structured JSON matching the "
            "provided schema {clean, findings[]} — no prose/markdown/fences."
        )
    return (
        "Run a raw structured review of the current working-tree changes in this "
        "project against the task acceptance criteria for correctness, security, "
        "and quality. Do not invoke any slash command. Return only structured "
        "JSON output conforming to the provided JSON schema: "
        '{"clean": bool, "findings": [{"severity": "P0|P1|P2|P3", "...": "..."}]}. '
        "Do not include prose, markdown, or code fences."
    )


@dataclass(frozen=True)
class SlotSpec:
    engine: str
    model: str
    effort: str
    base: str | None
    task_context: str | None
    name: str = "slot"
    plan: str | None = None
    plan_sha: str | None = None
    plan_instructions: str = ""


def slot_schema(spec: SlotSpec) -> dict[str, Any]:
    return plan_review.build_plan_schema() if spec.plan else build_inner_schema()


def slot_prompt(spec: SlotSpec) -> str:
    if spec.plan:
        return plan_review.plan_review_prompt(
            spec.plan, spec.plan_sha or "", spec.plan_instructions
        )
    return structured_review_prompt(spec.task_context)


@dataclass
class SlotOutcome:
    exit_code: int
    findings: list[dict[str, Any]]
    skip_reason: str | None
    stderr: str
    error: str | None = None


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--engine", choices=("codex", "claude"))
    parser.add_argument("--model")
    parser.add_argument("--both", action="store_true")
    parser.add_argument("--alpha-engine", choices=("codex", "claude"))
    parser.add_argument("--alpha-model")
    parser.add_argument("--beta-engine", choices=("codex", "claude"))
    parser.add_argument("--beta-model")
    parser.add_argument("--effort", default="high")
    parser.add_argument("--base")
    parser.add_argument("--task-context")
    parser.add_argument("--plan", help="review this task-plan Markdown doc instead of a diff")
    parser.add_argument("--project", help="plan mode: project the record is filed under")
    parser.add_argument("--slug", help="plan mode: task slug the record is filed under")
    args = parser.parse_args()
    if args.plan:
        if not args.project or not args.slug:
            parser.error("--plan requires --project and --slug")
        if args.base:
            parser.error("--plan reviews a document, not a diff; drop --base")
        if not os.path.isfile(args.plan):
            parser.error(f"--plan {args.plan}: no such file")
        try:
            plan_review.record_path(args.project, args.slug)
        except ValueError as exc:
            parser.error(str(exc))
    if args.both:
        missing = [
            flag
            for flag, value in (
                ("--alpha-engine", args.alpha_engine),
                ("--alpha-model", args.alpha_model),
                ("--beta-engine", args.beta_engine),
                ("--beta-model", args.beta_model),
            )
            if not value
        ]
        if missing:
            parser.error(f"--both requires {', '.join(missing)}")
    elif not args.engine or not args.model:
        parser.error("--engine and --model are required (or use --both)")
    return args


def run_claude(args: SlotSpec) -> subprocess.CompletedProcess[str]:
    if shutil.which("claude") is None:
        raise FileNotFoundError("claude binary not found")
    if args.base and not args.plan:
        prompt = f"/review the diff against {args.base}"
    else:
        prompt = slot_prompt(args)

    return subprocess.run(
        [
            "claude",
            "-p",
            "--model",
            args.model,
            "--effort",
            args.effort,
            "--output-format",
            "json",
            "--json-schema",
            json.dumps(slot_schema(args)),
            prompt,
        ],
        capture_output=True,
        stdin=subprocess.DEVNULL,
        text=True,
    )


def validate_inner(inner: Any, plan: bool = False) -> list[dict[str, Any]]:
    if not isinstance(inner, dict):
        raise ValueError("inner result is not an object")
    if not isinstance(inner.get("clean"), bool):
        raise ValueError("inner result clean field is not a bool")
    findings = inner.get("findings")
    if not isinstance(findings, list):
        raise ValueError("inner result findings field is not a list")

    normalized: list[dict[str, Any]] = []
    for finding in findings:
        if not isinstance(finding, dict):
            raise ValueError("finding is not an object")
        severity = finding.get("severity")
        if not isinstance(severity, str):
            raise ValueError("finding severity is not a string")
        severity = severity.upper()
        if severity not in {"P0", "P1", "P2", "P3"}:
            raise ValueError("finding severity is not P0, P1, P2, or P3")
        normalized_finding = dict(finding)
        normalized_finding["severity"] = severity
        if plan:
            category = finding.get("category")
            if not isinstance(category, str) or category.lower() not in plan_review.CATEGORIES:
                raise ValueError("plan finding category is not bug, flag, or security")
            normalized_finding["category"] = category.lower()
        normalized.append(normalized_finding)
    if inner["clean"] is False and not normalized:
        raise ValueError("inner result is inconsistent: clean=false with no findings")
    return normalized


def parse_claude(
    stdout: str, returncode: int, plan: bool = False
) -> tuple[list[dict[str, Any]], str | None]:
    del returncode
    try:
        envelope = json.loads(stdout)
        if not isinstance(envelope, dict):
            raise ValueError("envelope is not an object")
        result = envelope["result"]
        if not isinstance(result, str):
            raise ValueError("envelope result is not a string")
        return validate_inner(json.loads(result), plan), None
    except Exception as exc:
        return [], str(exc)


def codex_uses_exec(args: SlotSpec) -> bool:
    """`codex review` only reviews diffs: plan mode and non-git slots use exec."""
    return bool(args.plan) or (not args.base and bool(args.task_context))


def run_codex(args: SlotSpec) -> subprocess.CompletedProcess[str]:
    if shutil.which("codex") is None:
        raise FileNotFoundError("codex binary not found")
    effort_cfg = ["--config", f'model_reasoning_effort="{args.effort}"']
    if codex_uses_exec(args):
        # The schema must be a file for codex; keep it next to nothing
        # persistent (tmp dir, removed after the run). No -m: `codex review`
        # also ignores --model and lets config.toml pick the model, and a
        # ChatGPT-plan account rejects explicit model ids it does not own.
        with tempfile.TemporaryDirectory(prefix="fleet-review-") as tmp:
            schema_path = os.path.join(tmp, "schema.json")
            with open(schema_path, "w", encoding="utf-8") as fh:
                json.dump(slot_schema(args), fh)
            command = [
                "codex", "exec",
                "--skip-git-repo-check",
                "--sandbox", "read-only",
                "--output-schema", schema_path,
                *effort_cfg,
                slot_prompt(args),
            ]
            return subprocess.run(
                command, capture_output=True, stdin=subprocess.DEVNULL, text=True,
            )
    command = ["codex", "review"]
    if args.base:
        command.extend(["--base", args.base])
    command.extend(effort_cfg)
    return subprocess.run(command, capture_output=True, stdin=subprocess.DEVNULL, text=True)


def parse_codex_exec(
    stdout: str, returncode: int, plan: bool = False
) -> tuple[list[dict[str, Any]], str | None]:
    """`codex exec --output-schema` prints the final JSON message on stdout."""
    if plan and returncode != 0:
        return [], f"codex exec exited {returncode}"
    text = stdout.strip()
    start = text.find("{")
    if start < 0:
        return [], "codex exec produced no JSON object"
    try:
        return validate_inner(json.loads(text[start:]), plan), None
    except Exception as exc:
        return [], str(exc)


def parse_codex(stdout: str, returncode: int) -> tuple[list[dict[str, Any]], str | None]:
    findings: list[dict[str, Any]] = []
    for line in stdout.splitlines():
        match = FINDING_RE.search(line)
        if match is None:
            continue
        findings.append({"severity": match.group(1).upper(), "line": line})
    if findings:
        return findings, None
    if returncode != 0:
        return [], "codex exited nonzero with no findings"
    return findings, None


def skip_reason_for(stdout: str, stderr: str, error: str | None = None) -> str | None:
    text = "\n".join(part for part in (stdout, stderr, error or "") if part)
    if RATE_LIMIT_RE.search(text):
        return "rate-limited"
    if UNAVAILABLE_RE.search(text):
        return "unavailable"
    return None


def run_once(args: SlotSpec) -> tuple[list[dict[str, Any]], str | None, str, str | None]:
    try:
        if args.engine == "claude":
            completed = run_claude(args)
            findings, error = parse_claude(
                completed.stdout, completed.returncode, bool(args.plan)
            )
            # claude's stdout is a JSON envelope; only a failed run (parse
            # error / nonzero exit) can carry a rate-limit or missing-binary
            # signal worth turning into a skip.
            if not findings and error is not None:
                skip_reason = skip_reason_for(completed.stdout, completed.stderr, error)
                if skip_reason is not None:
                    return [], None, completed.stderr, skip_reason
        else:
            completed = run_codex(args)
            if codex_uses_exec(args):
                findings, error = parse_codex_exec(
                    completed.stdout, completed.returncode, bool(args.plan)
                )
            else:
                findings, error = parse_codex(completed.stdout, completed.returncode)
            if findings:
                return findings, error, completed.stderr, None
            skip_reason = skip_reason_for(completed.stdout, completed.stderr)
            if skip_reason is not None:
                return [], None, completed.stderr, skip_reason
    except OSError as exc:
        skip_reason = skip_reason_for("", "", str(exc)) or "unavailable"
        return [], None, "", skip_reason
    return findings, error, completed.stderr, None


def has_blocking(findings: list[dict[str, Any]]) -> bool:
    return any(
        finding.get("severity", "").upper() in BLOCKING_SEVERITIES
        for finding in findings
    )


def log(spec: SlotSpec, message: str) -> None:
    print(f"[review_slot {spec.name} {spec.engine}/{spec.model}] {message}", file=sys.stderr)


def run_slot(spec: SlotSpec) -> SlotOutcome:
    last_error = "unknown parse failure"
    for attempt in range(1, MAX_ATTEMPTS + 1):
        started = time.monotonic()
        findings, error, stderr, skip_reason = run_once(spec)
        elapsed = time.monotonic() - started
        if skip_reason is not None:
            log(spec, f"attempt {attempt}/{MAX_ATTEMPTS} skipped ({skip_reason}) in {elapsed:.0f}s")
            return SlotOutcome(2, [], skip_reason, "")
        if error is None:
            code = 1 if has_blocking(findings) else 0
            log(spec, f"attempt {attempt}/{MAX_ATTEMPTS} exit {code}, {len(findings)} finding(s) in {elapsed:.0f}s")
            return SlotOutcome(code, findings, None, stderr)
        last_error = error
        log(spec, f"attempt {attempt}/{MAX_ATTEMPTS} unparseable in {elapsed:.0f}s: {error}")

    return SlotOutcome(
        3, [], None, "",
        error=f"review slot blocked after {MAX_ATTEMPTS} attempts: {last_error}",
    )


def finish_single(outcome: SlotOutcome) -> int:
    if outcome.exit_code == 2:
        print(outcome.skip_reason)
        return 2
    if outcome.exit_code == 3:
        print(outcome.error, file=sys.stderr)
        return 3
    if outcome.stderr:
        print(outcome.stderr, end="", file=sys.stderr)
    if outcome.exit_code == 1:
        print(json.dumps(outcome.findings))
        return 1
    if outcome.findings:
        print(json.dumps(outcome.findings), file=sys.stderr)
    return 0


def finish_both(alpha: SlotOutcome, beta: SlotOutcome) -> int:
    for outcome in (alpha, beta):
        if outcome.stderr:
            print(outcome.stderr, end="", file=sys.stderr)
        if outcome.error:
            print(outcome.error, file=sys.stderr)
    report = {
        name: {
            "exit": outcome.exit_code,
            "findings": outcome.findings,
            "skip_reason": outcome.skip_reason,
        }
        for name, outcome in (("alpha", alpha), ("beta", beta))
    }
    print(json.dumps(report))
    codes = {alpha.exit_code, beta.exit_code}
    if 3 in codes or beta.exit_code == 2:
        return 3
    if 1 in codes:
        return 1
    return 0


def slot_summary(spec: SlotSpec, outcome: SlotOutcome) -> dict[str, Any]:
    return {
        "engine": spec.engine,
        "model": spec.model,
        "exit": outcome.exit_code,
        "skip_reason": outcome.skip_reason,
        "error": outcome.error,
        "findings": outcome.findings,
    }


def persist_plan_record(
    args: argparse.Namespace,
    sha: str,
    code: int,
    slots: dict[str, dict[str, Any]],
) -> None:
    record = plan_review.build_record(
        project=args.project,
        slug=args.slug,
        doc=os.path.abspath(args.plan),
        sha=sha,
        exit_code=code,
        slots=slots,
    )
    path = plan_review.write_record(record)
    print(f"[review_slot] plan review recorded: {path} (exit {code})", file=sys.stderr)


def plan_changed(plan: str, sha: str) -> bool:
    """True (and logged) when the doc changed while reviewers were reading it."""
    if plan_review.doc_sha256(plan) == sha:
        return False
    print(
        f"[review_slot] {plan} changed during the review; not recording it — re-run",
        file=sys.stderr,
    )
    return True


def main() -> int:
    args = parse_args()
    plan = os.path.abspath(args.plan) if args.plan else None
    sha = plan_review.doc_sha256(plan) if plan else None
    instructions = plan_review.instructions_block(plan) if plan else ""

    def spec(engine: str, model: str, name: str = "slot") -> SlotSpec:
        return SlotSpec(
            engine, model, args.effort, args.base, args.task_context, name, plan, sha,
            instructions,
        )

    if not args.both:
        single = spec(args.engine, args.model)
        outcome = run_slot(single)
        code = finish_single(outcome)
        if plan and sha:
            if plan_changed(plan, sha):
                return 3
            persist_plan_record(args, sha, code, {"slot": slot_summary(single, outcome)})
        return code

    alpha_spec = spec(args.alpha_engine, args.alpha_model, "alpha")
    beta_spec = spec(args.beta_engine, args.beta_model, "beta")
    with ThreadPoolExecutor(max_workers=2) as pool:
        alpha_future = pool.submit(run_slot, alpha_spec)
        beta_future = pool.submit(run_slot, beta_spec)
        alpha, beta = alpha_future.result(), beta_future.result()
    code = finish_both(alpha, beta)
    if plan and sha:
        if plan_changed(plan, sha):
            return 3
        persist_plan_record(args, sha, code, {
            "alpha": slot_summary(alpha_spec, alpha),
            "beta": slot_summary(beta_spec, beta),
        })
    return code


if __name__ == "__main__":
    raise SystemExit(main())
