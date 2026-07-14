#!/usr/bin/env python3
"""Run one reviewer slot and normalize its gate result."""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
import tempfile
from typing import Any


MAX_ATTEMPTS = 3
BLOCKING_SEVERITIES = {"P0", "P1"}
FINDING_RE = re.compile(r"\[(P[0-3])\]", re.IGNORECASE)


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
    prompt = "/review the diff"
    if args.base:
        prompt += f" against {args.base}"

    with tempfile.NamedTemporaryFile("w", encoding="utf-8", suffix=".json") as schema:
        json.dump(build_inner_schema(), schema)
        schema.flush()
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
                schema.name,
                prompt,
            ],
            capture_output=True,
            text=True,
        )


def validate_claude_inner(inner: Any) -> list[dict[str, str]]:
    if not isinstance(inner, dict):
        raise ValueError("inner result is not an object")
    if not isinstance(inner.get("clean"), bool):
        raise ValueError("inner result clean field is not a bool")
    findings = inner.get("findings")
    if not isinstance(findings, list):
        raise ValueError("inner result findings field is not a list")

    normalized: list[dict[str, str]] = []
    for finding in findings:
        if not isinstance(finding, dict):
            raise ValueError("finding is not an object")
        severity = finding.get("severity")
        if not isinstance(severity, str):
            raise ValueError("finding severity is not a string")
        severity = severity.upper()
        if severity not in {"P0", "P1", "P2", "P3"}:
            raise ValueError("finding severity is not P0, P1, P2, or P3")
        normalized.append({"severity": severity})
    return normalized


def parse_claude(stdout: str, returncode: int) -> tuple[list[dict[str, str]], str | None]:
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
    command = ["codex", "review"]
    if args.base:
        command.extend(["--base", args.base])
    command.extend(["--config", 'model_reasoning_effort="high"'])
    return subprocess.run(command, capture_output=True, text=True)


def parse_codex(stdout: str, returncode: int) -> tuple[list[dict[str, str]], str | None]:
    if returncode != 0 and not stdout:
        return [], "codex produced no usable output"

    findings: list[dict[str, str]] = []
    for line in stdout.splitlines():
        match = FINDING_RE.search(line)
        if match is None:
            continue
        findings.append({"severity": match.group(1).upper(), "line": line})
    return findings, None


def run_once(args: argparse.Namespace) -> tuple[list[dict[str, str]], str | None, str]:
    try:
        if args.engine == "claude":
            completed = run_claude(args)
            findings, error = parse_claude(completed.stdout, completed.returncode)
        else:
            completed = run_codex(args)
            findings, error = parse_codex(completed.stdout, completed.returncode)
    except OSError as exc:
        return [], str(exc), ""
    return findings, error, completed.stderr


def finish(findings: list[dict[str, str]]) -> int:
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
        findings, error, stderr = run_once(args)
        if error is None:
            if stderr:
                print(stderr, end="", file=sys.stderr)
            return finish(findings)
        last_error = error

    print(f"review slot blocked after {MAX_ATTEMPTS} attempts: {last_error}", file=sys.stderr)
    return 3


if __name__ == "__main__":
    raise SystemExit(main())
