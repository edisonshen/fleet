"""Tests for scripts/render-design-doc.py — the hub-style HTML renderer.

These assert the operator's "hub" aesthetic structural markers appear in the
generated HTML (sticky-TOC card, auto-numbered/anchored sections, embedded
<style>, decision boxes, diagram cards), that the output is fully
self-contained (no external <script src=>/<link href=...cdn...>/CDN fonts),
and that the script renders a large doc without error via the file path.

Smoke tests use a SYNTHETIC fixture, never the repo's live docs/DESIGN-*.md:
those are actively-edited design docs whose content must not be committed as
test fixtures here (a stale snapshot would collide with the doc's ongoing
rewrite on merge). A skip-if-present test opportunistically renders a real doc
when one exists in the local checkout, but never commits it.

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


# A self-contained "large doc" fixture for the file-path smoke test. We never
# render the repo's real docs/DESIGN-*.md here: those are live, actively-edited
# design docs whose content must not be committed as test fixtures (a stale
# snapshot would collide with the doc's ongoing rewrite on merge). Instead we
# synthesize a substantial multi-section doc on the fly so the smoke test still
# exercises render() on a big input through the file path.
def _make_large_fixture_md(sections: int = 60) -> str:
    parts = ["# Large Synthetic Design Doc", ""]
    for i in range(sections):
        parts.append(f"## Section {chr(ord('A') + (i % 26))}{i}")
        parts.append("")
        parts.append(
            "A paragraph of body prose explaining this section in enough "
            "detail to make the rendered output substantial. " * 4
        )
        parts.append("")
        parts.append("```")
        parts.append(" a ──▶ b ──▶ c")
        parts.append(" ▲           │")
        parts.append(" └─── loop ◄─┘")
        parts.append("```")
        parts.append("")
        parts.append("| Col | Val |")
        parts.append("|-----|-----|")
        parts.append(f"| row | {i} |")
        parts.append("")
    return "\n".join(parts)


def test_smoke_render_large_synthetic_doc(rdd, tmp_path):
    """Render a large SYNTHETIC doc through the file path without error.

    Uses a generated fixture (not the repo's live docs/DESIGN-*.md) so this
    tooling test never carries a real design doc's content as a committed
    snapshot.
    """
    work = tmp_path / "DESIGN-synthetic-large.md"
    work.write_text(_make_large_fixture_md(), encoding="utf-8")
    out = rdd.render(work)
    assert out.exists()
    html = out.read_text(encoding="utf-8")
    # Hub markers present.
    assert 'nav class="toc"' in html
    assert "position: sticky" in html
    assert re.search(r"<h2[^>]*>\s*1\.\s", html)        # first section numbered
    assert "<style>" in html
    assert "<script" not in html.lower()
    assert not re.search(r"<link\b", html, re.IGNORECASE)
    # Substantial (sanity: it didn't truncate).
    assert len(html) > 50_000


def test_smoke_render_real_docs_if_present(rdd, tmp_path):
    """If a real docs/DESIGN-*.md happens to be present locally, render it.

    Skip-if-absent and NEVER committed here: this only opportunistically
    exercises the renderer against whatever real docs exist in the developer's
    checkout. CI and a clean tooling branch have none of these files, so this
    is a no-op skip there — the synthetic test above is the load-bearing one.
    """
    candidates = sorted(DOCS.glob("DESIGN-*.md")) if DOCS.exists() else []
    if not candidates:
        pytest.skip("no real DESIGN-*.md present (expected on a tooling branch)")
    src = candidates[0]
    work = tmp_path / src.name
    work.write_text(src.read_text(encoding="utf-8"), encoding="utf-8")
    out = rdd.render(work)
    assert out.exists()
    html = out.read_text(encoding="utf-8")
    assert 'nav class="toc"' in html
    assert "<style>" in html
    assert "<script" not in html.lower()


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
