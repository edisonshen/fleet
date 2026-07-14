#!/usr/bin/env python3
"""Run one reviewer slot and normalize its gate result.

Exit codes:
  0 = no P0/P1 findings
  1 = P0/P1 findings (JSON stdout)
  2 = codex slot skipped (reason on stdout)
  3 = blocked
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
from typing import Any


MAX_ATTEMPTS = 3
BLOCKING_SEVERITIES = {"P0", "P1"}
FINDING_RE = re.compile(r"\[(P[0-3])\]", re.IGNORECASE)
RATE_LIMIT_RE = re.compile(
    r"usage limit|rate limit|too many requests|out of token|quota",
    re.IGNORECASE,
)
UNAVAILABLE_RE = re.compile(
    r"\bcodex: command not found\b|\bcommand not found: codex\b",
    re.IGNORECASE,
)


def build_inner_schema() -> dict[str, Any]:
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
                    },
                    "required": ["severity"],
                    "additionalProperties": True,
                },
            },
        },
        "required": ["clean", "findings"],
        "additionalProperties": True,
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--engine", choices=("codex", "claude"), required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--effort", default="high")
    parser.add_argument("--base")
    return parser.parse_args()


def run_claude(args: argparse.Namespace) -> subprocess.CompletedProcess[str]:
    if args.base:
        prompt = f"/review the diff against {args.base}"
    else:
        prompt = (
            "Run a raw structured review of the current working-tree changes in this "
            "project against the task acceptance criteria for correctness, security, "
            "and quality. Do not invoke any slash command. Return only structured "
            "JSON output conforming to the provided JSON schema: "
            '{"clean": bool, "findings": [{"severity": "P0|P1|P2|P3", "...": "..."}]}. '
            "Do not include prose, markdown, or code fences."
        )

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
        text=True,
    )


def validate_claude_inner(inner: Any) -> list[dict[str, Any]]:
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
        return validate_claude_inner(json.loads(result)), None
    except Exception as exc:
        return [], str(exc)


def run_codex(args: argparse.Namespace) -> subprocess.CompletedProcess[str]:
    if shutil.which("codex") is None:
        raise FileNotFoundError("codex binary not found")
    command = ["codex", "review"]
    if args.base:
        command.extend(["--base", args.base])
    command.extend(["--config", 'model_reasoning_effort="high"'])
    return subprocess.run(command, capture_output=True, text=True)


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


def codex_skip_reason(stderr: str, error: str | None = None) -> str | None:
    text = "\n".join(part for part in (stderr, error or "") if part)
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
        else:
            completed = run_codex(args)
            skip_reason = (
                codex_skip_reason(completed.stderr)
                if completed.returncode != 0
                else None
            )
            if skip_reason is not None:
                return [], None, completed.stderr, skip_reason
            findings, error = parse_codex(completed.stdout, completed.returncode)
    except OSError as exc:
        if args.engine == "codex":
            skip_reason = codex_skip_reason("", str(exc)) or "unavailable"
            return [], None, "", skip_reason
        return [], str(exc), "", None
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
