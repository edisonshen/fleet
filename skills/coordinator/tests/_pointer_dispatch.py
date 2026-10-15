"""Assertions for the pointer-prompt DISPATCH contract."""
from __future__ import annotations

import os
from pathlib import Path

import dispatch


def block_fields(block: str) -> dict[str, str]:
    fields: dict[str, str] = {}
    for ln in block.splitlines()[1:]:
        if ln.startswith("  ") and ": " in ln:
            key, value = ln.strip().split(": ", 1)
            fields[key] = value
    return fields


def assert_pointer_dispatch(block: str) -> str:
    """Assert the block's `prompt:` line is the short launch prompt naming an
    existing absolute prompt_file. Returns that file's body (the full
    prompt the subagent reads)."""
    fields = block_fields(block)
    prompt_file = fields["prompt_file"]
    assert os.path.isabs(prompt_file), prompt_file
    assert fields["prompt"] == dispatch.launch_prompt(prompt_file)
    assert f"Read {prompt_file} in full" in fields["prompt"]
    body = Path(prompt_file).read_text(encoding="utf-8")
    assert len(fields["prompt"]) < 300 < len(body)
    return body


def assert_one_line_final_report(body: str, slug: str) -> None:
    """Assert the prompt closes with the one-line final-report contract."""
    contract = "\n".join(dispatch.final_report_lines(slug)).strip()
    assert body.rstrip().endswith(contract), body[-600:]
    assert f"  {slug} phase=<final phase or outcome> pr=<PR url or -> " in body
