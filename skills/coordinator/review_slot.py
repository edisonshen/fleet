#!/usr/bin/env python3
"""Run one reviewer slot (or both concurrently) and normalize the gate result.

Single slot (`--engine --model`), exit codes:
  0 = no P0/P1 findings
  1 = P0/P1 findings (JSON stdout)
  2 = codex slot skipped (reason on stdout)
  3 = blocked

Both slots (`--both --alpha-engine/--alpha-model --beta-engine/--beta-model`)
run in parallel; stdout is one JSON object
  {"alpha": {"exit": n, "findings": [...], "skip_reason": ...}, "beta": {...}}
and the exit code is 3 if either slot blocked, else 1 if either slot has
P0/P1 findings, else 0 (a skipped codex alpha shows exit 2 in the JSON only).
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any


MAX_ATTEMPTS = 2
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


@dataclass(frozen=True)
class SlotSpec:
    engine: str
    model: str
    effort: str
    base: str | None
    task_context: str | None
    name: str = "slot"


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
    args = parser.parse_args()
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
    if args.base:
        prompt = f"/review the diff against {args.base}"
    elif args.task_context:
        prompt = (
            "Task context (what the worker was asked to build):\n"
            f"{args.task_context}\n\n"
            "Review the current working-tree changes in this project against the "
            "acceptance criteria above for correctness, security, and quality. If "
            "the tree is large, focus on the files most plausibly changed for this "
            "task (review at most ~40 files); do not attempt to enumerate an "
            "unrelated whole tree. Return ONLY structured JSON matching the "
            "provided schema {clean, findings[]} — no prose/markdown/fences."
        )
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
        stdin=subprocess.DEVNULL,
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
        return validate_claude_inner(json.loads(result)), None
    except Exception as exc:
        return [], str(exc)


def run_codex(args: SlotSpec) -> subprocess.CompletedProcess[str]:
    if shutil.which("codex") is None:
        raise FileNotFoundError("codex binary not found")
    command = ["codex", "review"]
    if args.base:
        command.extend(["--base", args.base])
    command.extend(["--config", f'model_reasoning_effort="{args.effort}"'])
    return subprocess.run(command, capture_output=True, stdin=subprocess.DEVNULL, text=True)


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


def codex_skip_reason(stdout: str, stderr: str, error: str | None = None) -> str | None:
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
            findings, error = parse_claude(completed.stdout, completed.returncode)
        else:
            completed = run_codex(args)
            findings, error = parse_codex(completed.stdout, completed.returncode)
            if findings:
                return findings, error, completed.stderr, None
            skip_reason = codex_skip_reason(completed.stdout, completed.stderr)
            if skip_reason is not None:
                return [], None, completed.stderr, skip_reason
    except OSError as exc:
        if args.engine == "codex":
            skip_reason = codex_skip_reason("", "", str(exc)) or "unavailable"
            return [], None, "", skip_reason
        return [], str(exc), "", None
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
    if 3 in codes:
        return 3
    if 1 in codes:
        return 1
    return 0


def main() -> int:
    args = parse_args()
    if not args.both:
        spec = SlotSpec(args.engine, args.model, args.effort, args.base, args.task_context)
        return finish_single(run_slot(spec))

    alpha_spec = SlotSpec(
        args.alpha_engine, args.alpha_model, args.effort, args.base, args.task_context, "alpha"
    )
    beta_spec = SlotSpec(
        args.beta_engine, args.beta_model, args.effort, args.base, args.task_context, "beta"
    )
    with ThreadPoolExecutor(max_workers=2) as pool:
        alpha_future = pool.submit(run_slot, alpha_spec)
        beta_future = pool.submit(run_slot, beta_spec)
        return finish_both(alpha_future.result(), beta_future.result())


if __name__ == "__main__":
    raise SystemExit(main())
