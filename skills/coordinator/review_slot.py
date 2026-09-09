#!/usr/bin/env python3
"""Run one reviewer slot and normalize its gate result.

Exit codes:
  0 = no P0/P1 findings
  1 = P0/P1 findings (JSON stdout)
  2 = helper slot skipped (reason on stdout: rate-limited|unavailable)
  3 = blocked

Engines:
  claude  `claude -p` with an inline JSON schema. `--base` => `/review`,
          otherwise a raw structured working-tree review.
  codex   `codex review [--base]` when no --task-context (git); with
          --task-context and no --base (non-git project) `codex review`
          has no diff to work from, so run `codex exec --output-schema`
          for a raw structured review instead.

Either engine may be the dominant anchor or the optional helper; this
script only ever execs the ONE binary named by --engine. Exit 2 is
reported for a missing/rate-limited binary regardless of engine — the
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
from typing import Any


MAX_ATTEMPTS = 3
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


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--engine", choices=("codex", "claude"), required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--effort", default="high")
    parser.add_argument("--base")
    parser.add_argument("--task-context")
    return parser.parse_args()


def run_claude(args: argparse.Namespace) -> subprocess.CompletedProcess[str]:
    if shutil.which("claude") is None:
        raise FileNotFoundError("claude binary not found")
    if args.base:
        prompt = f"/review the diff against {args.base}"
    else:
        prompt = structured_review_prompt(args.task_context)

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
            json.dumps(build_inner_schema()),
            prompt,
        ],
        capture_output=True,
        stdin=subprocess.DEVNULL,
        text=True,
    )


def validate_inner(inner: Any) -> list[dict[str, Any]]:
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
        normalized.append(normalized_finding)
    if inner["clean"] is False and not normalized:
        raise ValueError("inner result is inconsistent: clean=false with no findings")
    return normalized


def parse_claude(stdout: str, returncode: int) -> tuple[list[dict[str, Any]], str | None]:
    del returncode
    try:
        envelope = json.loads(stdout)
        if not isinstance(envelope, dict):
            raise ValueError("envelope is not an object")
        result = envelope["result"]
        if not isinstance(result, str):
            raise ValueError("envelope result is not a string")
        return validate_inner(json.loads(result)), None
    except Exception as exc:
        return [], str(exc)


def codex_uses_exec(args: argparse.Namespace) -> bool:
    """Non-git slot: `codex review` needs a diff base, so use `codex exec`."""
    return not args.base and bool(args.task_context)


def run_codex(args: argparse.Namespace) -> subprocess.CompletedProcess[str]:
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
                json.dump(build_inner_schema(), fh)
            command = [
                "codex", "exec",
                "--skip-git-repo-check",
                "--sandbox", "read-only",
                "--output-schema", schema_path,
                *effort_cfg,
                structured_review_prompt(args.task_context),
            ]
            return subprocess.run(
                command, capture_output=True, stdin=subprocess.DEVNULL, text=True,
            )
    command = ["codex", "review"]
    if args.base:
        command.extend(["--base", args.base])
    command.extend(effort_cfg)
    return subprocess.run(command, capture_output=True, stdin=subprocess.DEVNULL, text=True)


def parse_codex_exec(stdout: str, returncode: int) -> tuple[list[dict[str, Any]], str | None]:
    """`codex exec --output-schema` prints the final JSON message on stdout."""
    del returncode
    text = stdout.strip()
    start = text.find("{")
    if start < 0:
        return [], "codex exec produced no JSON object"
    try:
        return validate_inner(json.loads(text[start:])), None
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


def run_once(args: argparse.Namespace) -> tuple[list[dict[str, Any]], str | None, str, str | None]:
    try:
        if args.engine == "claude":
            completed = run_claude(args)
            findings, error = parse_claude(completed.stdout, completed.returncode)
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
                findings, error = parse_codex_exec(completed.stdout, completed.returncode)
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


def finish(findings: list[dict[str, Any]]) -> int:
    blocking = [
        finding
        for finding in findings
        if finding.get("severity", "").upper() in BLOCKING_SEVERITIES
    ]
    if blocking:
        print(json.dumps(findings))
        return 1
    if findings:
        print(json.dumps(findings), file=sys.stderr)
    return 0


def main() -> int:
    args = parse_args()
    last_error = "unknown parse failure"

    for _ in range(MAX_ATTEMPTS):
        findings, error, stderr, skip_reason = run_once(args)
        if skip_reason is not None:
            print(skip_reason)
            return 2
        if error is None:
            if stderr:
                print(stderr, end="", file=sys.stderr)
            return finish(findings)
        last_error = error

    print(f"review slot blocked after {MAX_ATTEMPTS} attempts: {last_error}", file=sys.stderr)
    return 3


if __name__ == "__main__":
    raise SystemExit(main())
