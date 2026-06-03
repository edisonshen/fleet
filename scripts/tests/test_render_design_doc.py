"""Tests for scripts/render-design-doc.py — the hub-style HTML renderer.

These assert the operator's "hub" aesthetic structural markers appear in the
generated HTML (sticky-TOC card, auto-numbered/anchored sections, embedded
<style>, decision boxes, diagram cards), that the output is fully
self-contained (no external <script src=>/<link href=...cdn...>/CDN fonts),
and that the script renders the real fleet DESIGN-*.md docs without error —
including the large docs/DESIGN-coord-worktree-lifecycle.md.

The script's filename has a hyphen, so it cannot be imported as a normal
module; we load it from source via importlib.
"""
from __future__ import annotations

import importlib.util
import re
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "render-design-doc.py"
REPO = SCRIPT.parents[1]          # .../<repo>/scripts/render-design-doc.py
DOCS = REPO / "docs"


def _load_module():
    spec = importlib.util.spec_from_file_location("render_design_doc", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec and spec.loader
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def rdd():
    try:
        import markdown  # noqa: F401
    except ImportError:
        pytest.skip("python markdown library not installed")
    return _load_module()


# A fixture doc exercising every hub affordance: numbered sections, a Q-heading
# decision box, an ASCII diagram, a decision log, and a change log / open
# threads section.
FIXTURE_MD = """# Hub Fixture Doc

## Problem
The first top-level section. Should auto-number to "1.".

## Mental model

```
 ingest  →  score  →  publish
    ▲                    │
    └──────── feedback ◄─┘
```

A box-drawing diagram above should render as a diagram card.

## Open questions

### Q1 — do we cache reads?
**Stake:** yes, in a per-process LRU.

A justification paragraph that belongs inside the Q1 box.

### Q2 — what is the TTL?
**Stake:** five minutes.

## Decision log

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-06-02 | Cache reads | Cuts p99 latency |

## Open threads
- Whether to share the cache across processes.

## Change log
- 2026-06-02 — Initial draft. (coord)
"""


def test_self_contained_no_external_assets(rdd):
    body, toc = rdd.render_markdown(FIXTURE_MD)
    html = rdd.HTML_TEMPLATE.format(
        title="Hub Fixture Doc",
        toc=toc,
        body=body,
        source_path="fixture.md",
        rendered_at="2026-06-02 00:00 UTC",
    )
    # Embedded stylesheet, no external assets of any kind.
    assert "<style>" in html
    assert "<script" not in html.lower()
    assert not re.search(r"<link\b", html, re.IGNORECASE)
    # No CDN / external font / analytics references.
    for needle in ("cdn", "googleapis", "fonts.gstatic", "unpkg",
                   "jsdelivr", "cloudflare", "googletagmanager", "http://",
                   "https://"):
        assert needle not in html.lower(), f"external reference found: {needle}"


def test_sticky_toc_and_dark_light(rdd):
    body, toc = rdd.render_markdown(FIXTURE_MD)
    html = rdd.HTML_TEMPLATE.format(
        title="t", toc=toc, body=body, source_path="f.md", rendered_at="x"
    )
    # Sticky TOC container.
    assert 'nav class="toc"' in html
    assert "position: sticky" in html
    # Dark/light via prefers-color-scheme + color-scheme hint.
    assert "prefers-color-scheme: dark" in html
    assert "color-scheme: light dark" in html


def test_sections_numbered_and_anchored(rdd):
    body, _toc = rdd.render_markdown(FIXTURE_MD)
    # Top-level sections auto-numbered.
    assert re.search(r"<h2[^>]*>\s*1\.\s*Problem", body)
    assert re.search(r"<h2[^>]*>\s*2\.\s*Mental model", body)
    # Anchored: every heading carries an id + a hover ¶ permalink anchor.
    assert re.search(r'<h2 id="problem"', body)
    assert 'class="anchor"' in body


def test_decision_box_renders(rdd):
    body, _toc = rdd.render_markdown(FIXTURE_MD)
    # Q-headings become .qbox decision boxes.
    assert body.count('<div class="qbox">') == 2
    # The box wraps the heading AND its prose.
    m = re.search(r'<div class="qbox">(.*?)</div>', body, re.DOTALL)
    assert m and "Q1" in m.group(1)
    assert "per-process LRU" in m.group(1)


def test_diagram_block_renders(rdd):
    body, _toc = rdd.render_markdown(FIXTURE_MD)
    # ASCII diagram fenced block gets the diagram-block card class.
    assert 'class="diagram-block"' in body
    # A normal code fence (no box glyphs) must NOT get the class.
    plain_body, _ = rdd.render_markdown("```\nx = 1\nprint(x)\n```\n")
    assert "diagram-block" not in plain_body


def test_decision_and_change_log_sections_render(rdd):
    body, toc = rdd.render_markdown(FIXTURE_MD)
    # The decision-log table and change-log list both render.
    assert re.search(r"<h2[^>]*>[^<]*Decision log", body)
    assert "Cuts p99 latency" in body
    assert re.search(r"<h2[^>]*>[^<]*Change log", body)
    assert "Initial draft" in body
    assert re.search(r"<h2[^>]*>[^<]*Open threads", body)
    # All of these appear in the TOC too.
    assert "Decision log" in toc
    assert "Change log" in toc


@pytest.mark.parametrize(
    "doc",
    ["DESIGN-dispatch-durability.md", "DESIGN-coord-worktree-lifecycle.md"],
)
def test_smoke_render_real_docs(rdd, tmp_path, doc):
    """Render the real (large) fleet DESIGN docs without error, in hub style."""
    src = DOCS / doc
    if not src.exists():
        pytest.skip(f"{doc} not present on this branch")
    # Copy into a tmp dir so we don't write .html into the repo during tests
    # (fleet owns its resources: tmp_path is auto-reaped by pytest).
    work = tmp_path / doc
    work.write_text(src.read_text(encoding="utf-8"), encoding="utf-8")
    out = rdd.render(work)
    assert out.exists()
    html = out.read_text(encoding="utf-8")
    # Hub markers present on the real doc.
    assert 'nav class="toc"' in html
    assert "position: sticky" in html
    assert re.search(r"<h2[^>]*>\s*1\.\s", html)        # first section numbered
    assert "<style>" in html
    assert "<script" not in html.lower()
    assert not re.search(r"<link\b", html, re.IGNORECASE)
    # The big doc must be substantial (sanity: it didn't truncate).
    if doc == "DESIGN-coord-worktree-lifecycle.md":
        assert len(html) > 50_000


def test_cli_contract(rdd, tmp_path):
    """`render-design-doc.py docs/foo.md` writes docs/foo.html next to it."""
    md = tmp_path / "DESIGN-cli.md"
    md.write_text("# CLI Doc\n\n## Problem\nBody.\n", encoding="utf-8")
    res = subprocess.run(
        [sys.executable, str(SCRIPT), str(md)],
        capture_output=True, text=True,
    )
    assert res.returncode == 0, res.stderr
    out = md.with_suffix(".html")
    assert out.exists()
    assert "Hub" not in out.name and out.name == "DESIGN-cli.html"


def test_rejects_non_md(rdd, tmp_path):
    bad = tmp_path / "notes.txt"
    bad.write_text("hi", encoding="utf-8")
    res = subprocess.run(
        [sys.executable, str(SCRIPT), str(bad)],
        capture_output=True, text=True,
    )
    assert res.returncode == 1
    assert "expected .md" in res.stderr
