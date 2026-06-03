#!/usr/bin/env python3
"""render-design-doc.py — Convert a markdown design doc to a polished HTML page.

Usage:
    python3 scripts/render-design-doc.py docs/DESIGN-foo.md
    # Writes docs/DESIGN-foo.html next to the source.

Why this exists:
    Operator preference (2026-05-15): design docs ship as both formats —
    .md for agents (codex, claude, plan-eng-review skills) and .html
    for humans who want readable rendered output without an editor.

    Operator preference (2026-06-02): the rendered HTML must match the
    "hub" aesthetic codified at ~/.claude/skills/design-doc/template.html
    and exemplified by ~/projects/rainier/docs/recursive-research-system.html:
    a sticky table-of-contents card, numbered sections, decision boxes
    (Q1..Qn), decision/change logs, open threads, teal accent palette,
    dark/light via prefers-color-scheme. This script ports that look onto
    arbitrary fleet DESIGN-*.md source so the .md stays the source of truth
    while the .html reads like a hand-authored hub doc.

Dependencies:
    - Python 3.9+
    - markdown >= 3.4 (pip3 install --user markdown)

Self-contained output:
    The generated .html has all CSS inlined; no external fonts, no JS
    frameworks, no analytics, no CDN <link>/<script src>. Drop it in a
    browser and it renders. Dark/light mode auto-toggles via
    prefers-color-scheme; a print stylesheet produces clean PDF output.

How the hub look is applied to plain markdown:
    1. Top-level `## ` sections are auto-numbered (1, 2, 3 ...) unless the
       heading text already begins with a number — so `## Problem` becomes
       "1. Problem" but `## 3. Foo` is left alone. The ordinal is prepended
       to the rendered heading text (not a CSS counter) so heading anchors
       and ids stay stable.
    2. Headings that look like decision points — text starting with `Q<n>`
       (e.g. `### Q3 — should we ...`) — render inside a `.qbox` card with
       the heading and the prose up to the next same-or-shallower heading.
    3. Fenced code blocks whose content is an ASCII diagram (box-drawing /
       arrow glyphs) render as a centered `.diagram-block` card.
    Everything else is ordinary markdown → HTML, themed by the embedded CSS.
"""
from __future__ import annotations

import argparse
import datetime as _dt
import html
import re
import sys
from pathlib import Path

try:
    import markdown
    from markdown.extensions import Extension
    from markdown.treeprocessors import Treeprocessor
except ImportError:
    sys.stderr.write(
        "error: python markdown library missing.\n"
        "  pip3 install --user markdown\n"
    )
    sys.exit(2)


# --- HTML template -----------------------------------------------------------
#
# Embedded CSS ported from ~/.claude/skills/design-doc/template.html (the
# canonical hub template). System fonts only; no external requests. Teal
# accent, color-scheme: light dark, sticky TOC card, .qbox decision boxes,
# .diagram-block cards, .pill badges, .layer cards, and a print stylesheet.

HTML_TEMPLATE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{title}</title>
<style>
  :root {{
    color-scheme: light dark;
    --bg: #fbfaf7;
    --paper: #fffefb;
    --surface: #f4f1ea;
    --surface-2: #ede8dc;
    --fg: #1d2024;
    --muted: #626b73;
    --subtle: #8a9299;
    --border: #ded7ca;
    --border-strong: #c9bfad;
    --accent: #0f766e;
    --accent-rgb: 15, 118, 110;
    --code-bg: #efebe2;
    --row-alt: #f6f3ec;
    --shadow: 0 1px 2px rgba(36, 32, 24, 0.04), 0 12px 28px rgba(36, 32, 24, 0.06);
  }}
  @media (prefers-color-scheme: dark) {{
    :root {{
      --bg: #101312;
      --paper: #171b19;
      --surface: #1d2320;
      --surface-2: #242b27;
      --fg: #e9e5dc;
      --muted: #a2aaa4;
      --subtle: #727c75;
      --border: #303832;
      --border-strong: #465047;
      --accent: #7bd9c7;
      --accent-rgb: 123, 217, 199;
      --code-bg: #222821;
      --row-alt: #1b211e;
      --shadow: 0 1px 2px rgba(0, 0, 0, 0.22), 0 18px 40px rgba(0, 0, 0, 0.24);
    }}
  }}
  html {{
    background: var(--bg);
    color: var(--fg);
    scroll-behavior: smooth;
    text-rendering: optimizeLegibility;
    -webkit-font-smoothing: antialiased;
    font-kerning: normal;
  }}
  body {{
    background: var(--bg);
    color: var(--fg);
    font: 16px/1.68 ui-sans-serif, -apple-system, BlinkMacSystemFont, "SF Pro Text", "Segoe UI", sans-serif;
    display: grid;
    grid-template-columns: 250px minmax(0, 780px);
    column-gap: 64px;
    max-width: 1180px;
    margin: 0 auto;
    padding: 56px 32px 112px;
  }}
  body > header,
  body > section,
  body > footer {{ grid-column: 2; }}
  header {{
    border-bottom: 1px solid var(--border);
    padding: 0 0 28px;
    margin-bottom: 40px;
  }}
  h1, h2, h3, h4, h5, h6 {{
    font-kerning: normal;
    letter-spacing: 0;
    text-wrap: balance;
    scroll-margin-top: 32px;
  }}
  h1 {{
    font-size: clamp(2.35rem, 4vw, 3.7rem);
    line-height: 1.04;
    font-weight: 720;
    margin: 0 0 12px;
    max-width: 18ch;
  }}
  h2 {{
    font-size: 1.42rem;
    line-height: 1.22;
    margin: 72px 0 18px;
    font-weight: 680;
    padding-top: 2px;
  }}
  section:first-of-type h2 {{ margin-top: 0; }}
  h3 {{
    font-size: 1.08rem;
    line-height: 1.34;
    margin: 36px 0 10px;
    font-weight: 650;
  }}
  h4 {{
    font-size: 0.98rem;
    line-height: 1.4;
    margin: 22px 0 8px;
    font-weight: 650;
    color: var(--accent);
  }}
  h5, h6 {{
    font-size: 0.92rem;
    margin: 18px 0 6px;
    font-weight: 650;
    color: var(--muted);
  }}
  .heading-anchor {{
    opacity: 0;
    border-bottom: 0;
    color: var(--subtle);
    font-weight: 500;
    margin-left: 0.45rem;
    text-decoration: none;
    transition: opacity 140ms ease, color 140ms ease;
  }}
  :is(h1, h2, h3, h4, h5, h6):hover .heading-anchor,
  .heading-anchor:focus {{
    opacity: 1;
    color: var(--accent);
  }}
  .meta {{
    color: var(--muted);
    font-size: 0.86rem;
    line-height: 1.55;
  }}
  .meta code {{ font-size: 0.92em; }}
  p, li {{ margin: 0 0 12px; }}
  p {{ max-width: 72ch; }}
  ul, ol {{ padding-left: 24px; }}
  li > p {{ margin-bottom: 6px; }}
  li > h4 {{ margin-top: 28px; }}
  strong {{ font-weight: 680; }}
  em {{ font-style: italic; }}
  code, pre {{
    font-family: "SF Mono", ui-monospace, Menlo, Consolas, monospace;
    font-size: 0.86em;
  }}
  code {{
    background: var(--code-bg);
    border: 1px solid rgba(var(--accent-rgb), 0.08);
    padding: 1px 5px;
    border-radius: 4px;
  }}
  pre {{
    background: var(--code-bg);
    border: 1px solid var(--border);
    padding: 18px 20px;
    border-radius: 8px;
    overflow-x: auto;
    line-height: 1.45;
  }}
  pre code {{ background: transparent; border: 0; padding: 0; }}
  pre.diagram-block {{
    position: relative;
    text-align: center;
    margin: 28px auto 40px;
    padding: 24px 24px 26px;
    background:
      linear-gradient(180deg, rgba(var(--accent-rgb), 0.07), transparent 55%),
      var(--paper);
    box-shadow: var(--shadow);
  }}
  pre.diagram-block code {{
    display: inline-block;
    min-width: max-content;
    text-align: left;
  }}
  blockquote {{
    border-left: 3px solid var(--accent);
    margin: 20px 0 24px;
    padding: 12px 18px;
    color: var(--fg);
    background: rgba(var(--accent-rgb), 0.08);
    border-radius: 0 8px 8px 0;
  }}
  blockquote p:first-child {{ margin-top: 0; }}
  blockquote p:last-child {{ margin-bottom: 0; }}
  table {{
    border-collapse: separate;
    border-spacing: 0;
    width: 100%;
    margin: 24px 0 36px;
    font-size: 0.91rem;
    line-height: 1.52;
    border: 1px solid var(--border);
    border-radius: 8px;
    overflow: hidden;
    background: var(--paper);
    display: block;
    overflow-x: auto;
  }}
  th, td {{
    text-align: left;
    vertical-align: top;
    padding: 12px 14px;
    border-bottom: 1px solid var(--border);
  }}
  th {{ font-weight: 650; color: var(--fg); background: var(--surface); }}
  tbody tr:last-child td {{ border-bottom: 0; }}
  tbody tr:nth-child(even) td {{ background: var(--row-alt); }}
  tbody tr:hover td {{ background: rgba(var(--accent-rgb), 0.07); }}
  td.rank {{ font-variant-numeric: tabular-nums; white-space: nowrap; font-weight: 600; }}
  a {{ color: var(--accent); text-decoration: none; border-bottom: 1px solid transparent; }}
  a:hover {{ border-bottom-color: var(--accent); }}
  hr {{ border: none; border-top: 1px solid var(--border); margin: 40px 0; }}
  kbd {{
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: 4px;
    padding: 1px 6px;
    font-family: "SF Mono", ui-monospace, Menlo, Consolas, monospace;
    font-size: 0.82em;
    box-shadow: 0 1px 0 var(--border);
  }}
  nav.toc {{
    grid-column: 1;
    grid-row: 2 / span 40;
    position: sticky;
    top: 28px;
    align-self: start;
    max-height: calc(100vh - 56px);
    overflow-y: auto;
    background: var(--paper);
    border: 1px solid var(--border);
    padding: 18px 18px 16px;
    border-radius: 8px;
    font-size: 0.86rem;
    line-height: 1.45;
    box-shadow: var(--shadow);
  }}
  nav.toc strong {{
    display: block;
    margin: 0 0 10px;
    font-size: 0.74rem;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--muted);
    font-weight: 650;
  }}
  nav.toc ol {{ padding-left: 19px; margin: 0; }}
  nav.toc li {{ margin: 5px 0; padding-left: 2px; }}
  nav.toc a {{
    color: var(--fg);
    border-bottom: 0;
    display: block;
    padding: 1px 0;
  }}
  nav.toc a:hover {{ color: var(--accent); border-bottom: 0; }}
  .pill {{
    display: inline-block;
    font-size: 0.72rem;
    line-height: 1.4;
    padding: 2px 7px;
    border-radius: 999px;
    margin-left: 6px;
    font-weight: 650;
    vertical-align: 0.08em;
    background: rgba(var(--accent-rgb), 0.12);
    color: var(--accent);
    border: 1px solid rgba(var(--accent-rgb), 0.2);
  }}
  .qbox {{
    position: relative;
    border: 1px solid var(--border);
    padding: 4px 20px 8px 22px;
    border-radius: 8px;
    margin: 20px 0;
    background:
      linear-gradient(90deg, rgba(var(--accent-rgb), 0.11), transparent 34%),
      var(--paper);
    box-shadow: var(--shadow);
  }}
  .qbox::before {{
    content: "";
    position: absolute;
    left: -1px;
    top: 12px;
    bottom: 12px;
    width: 3px;
    border-radius: 999px;
    background: var(--accent);
  }}
  .qbox > :is(h2, h3, h4, h5, h6):first-child {{
    margin-top: 14px;
    color: var(--accent);
    font-family: "SF Mono", ui-monospace, Menlo, monospace;
    font-size: 0.95rem;
    font-weight: 720;
  }}
  .layer {{
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 20px 22px;
    margin: 18px 0;
    background: var(--paper);
    box-shadow: var(--shadow);
  }}
  .layer > :is(h3, h4):first-child {{ margin-top: 0; }}
  footer {{
    margin-top: 72px;
    padding: 22px 0 0;
    border-top: 1px solid var(--border);
    color: var(--muted);
    font-size: 0.86rem;
  }}
  @media (max-width: 1023px) {{
    body {{ display: block; max-width: 820px; padding: 40px 24px 88px; }}
    header {{ margin-bottom: 24px; }}
    h1 {{ max-width: none; }}
    h2 {{ margin-top: 56px; }}
    nav.toc {{ position: static; max-height: none; margin: 0 0 44px; }}
  }}
  @media (max-width: 720px) {{
    body {{ padding: 32px 18px 72px; }}
    h1 {{ font-size: 2.2rem; }}
    pre.diagram-block {{ text-align: left; padding: 20px 18px; }}
  }}
  @media print {{
    @page {{ margin: 0.75in; }}
    :root {{
      --bg: #ffffff;
      --paper: #ffffff;
      --surface: #ffffff;
      --surface-2: #ffffff;
      --fg: #111111;
      --muted: #555555;
      --subtle: #777777;
      --border: #cccccc;
      --accent: #111111;
      --accent-rgb: 17, 17, 17;
      --code-bg: #f3f3f3;
      --row-alt: #f7f7f7;
      --shadow: none;
    }}
    html {{ scroll-behavior: auto; }}
    body {{ display: block; max-width: none; padding: 0; font-size: 11pt; line-height: 1.5; }}
    nav.toc {{ position: static; box-shadow: none; margin: 0 0 24pt; page-break-after: avoid; max-height: none; }}
    h1 {{ font-size: 28pt; max-width: none; }}
    h2 {{ font-size: 16pt; margin-top: 28pt; page-break-after: avoid; }}
    h3, h4 {{ page-break-after: avoid; }}
    .heading-anchor {{ display: none; }}
    a {{ color: inherit; border-bottom: 0; text-decoration: underline; }}
    pre, table, blockquote, .qbox, .layer {{ break-inside: avoid; box-shadow: none; }}
    table {{ font-size: 9pt; }}
    footer {{ margin-top: 32pt; }}
  }}
</style>
</head>
<body>
<header>
  <h1>{title}</h1>
  <div class="meta">
    <strong>Source:</strong> <code>{source_path}</code> &middot;
    <strong>Rendered:</strong> {rendered_at} &middot;
    agents read the <code>.md</code>, humans read the <code>.html</code>.
  </div>
</header>
<nav class="toc" aria-label="Contents">
  <strong>Contents</strong>
  {toc}
</nav>
{body}
<footer>
  <p>Rendered from <code>{source_path}</code> by <code>scripts/render-design-doc.py</code> in hub style. The <code>.md</code> is the source of truth; regenerate this file after editing it.</p>
</footer>
</body>
</html>
"""


# --- Hub post-processing -----------------------------------------------------
#
# Two passes impose the hub affordances that plain markdown can't express:
#  - a tree processor (runs on the parsed element tree) for section numbering
#    and ASCII-diagram detection;
#  - a string pass over the serialized body for decision boxes (.qbox), which
#    must wrap a heading PLUS its following sibling runs — far simpler on the
#    serialized HTML than on the flat ElementTree markdown produces.

# Box-drawing / arrow glyphs that mark a fenced block as an ASCII diagram
# rather than ordinary source code.
_DIAGRAM_GLYPHS = set(
    "─━│┃┄┅┆┇┈┉┊┋┌┍┎┏┐┑┒┓└┕┖┗┘┙┚┛├┝┞┟┠┡┢┣┤┥┦┧┨┩┪┫"
    "┬┭┮┯┰┱┲┳┴┵┶┷┸┹┺┻┼┽┾┿╀╁╂╃╄╅╆╇╈╉╊╋"
    "═║╔╗╚╝╠╣╦╩╬▲▼◄►▶◀▸◂↑↓←→↔↕⟶⟵"
)

# Heading text that opens a decision point: "Q3", "Q12 — title", "Q1:".
# Tolerate an optional leading "N. " ordinal prefix: the section numberer may
# rewrite a Q heading to "1. Q1 — ..." before wrap_qboxes runs (relevant only
# for h3+ headings nested under a numbered ancestor that itself starts with Q).
_QHEADING_RE = re.compile(r"^(?:\d+\.\s*)?Q\d+\b")

# Heading text that already starts with its own ordinal ("3. Foo", "4.2 Bar",
# "12 — Foo", "5) Foo", "6: Foo"). `ordinal` captures the leading top-level
# number so the counter advances past self-numbered sections and stays
# monotonic. A heading counts as self-numbered only when the leading digits are
# either a DOTTED multi-level number ("4.2 Bar") OR a single number trailed by
# EXPLICIT ordinal punctuation. To avoid swallowing content headings:
#   - "." / ")" / ":" must be followed by whitespace or end-of-text
#     (so "3. Foo" matches but "3.5GHz" does not via this branch),
#   - a dash/em-dash separator must be SPACE-PADDED (" — ", " - ")
#     (so "12 — Foo" matches but the ISO date "2026-06 Roadmap" does not).
# A bare digit run followed only by whitespace + a word ("2026 Roadmap",
# "10 reasons") is section CONTENT, not an ordinal.
_ALREADY_NUMBERED_RE = re.compile(
    r"^(?P<ordinal>\d+)"
    r"(?:"
    r"(?:\.\d+)+(?:[.):]?(?=\s|$)|\s+[—–-]\s)\s*"  # dotted: "4.2", "4.2.", "1.2 — "
    r"|"
    r"(?:[.):](?=\s|$)|\s+[—–-]\s)\s*"             # single num + ordinal punct
    r")"
)


class HubTreeprocessor(Treeprocessor):
    """Apply tree-level hub affordances: top-level section numbering.

    Diagram detection is NOT done here: `fenced_code` (from the `extra`
    bundle) stashes code blocks as raw-HTML placeholders during parsing and
    only substitutes the real `<pre><code>` at serialization time, so the
    `<pre>` element is not in the tree a treeprocessor sees. Diagram cards are
    therefore tagged in a string pass over the serialized body — see
    wrap_diagrams().
    """

    def run(self, root):  # noqa: D401 — markdown API name
        self._number_sections(root)
        return root

    def _number_sections(self, root):
        """Prefix top-level h2 headings with an ordinal and renumber their id.

        `## Problem` -> "1. Problem" with id `1-problem`, matching the
        operator's hub exemplar (DESIGN-docs-publisher.html uses
        `<section id="1-problem">` / `<h2 id="1-problem">`). The toc extension
        (higher priority) has already assigned a plain slug id (`problem`) and
        appended a `.heading-anchor` permalink child whose href points at
        `#problem`; we prefix the ordinal to both the id and the href so the
        anchor still resolves.

        Headings that already begin with a number (`## 3. Foo`, `## 4.2 Bar`)
        keep their slug id and are not re-prefixed, so self-numbered docs
        aren't double-numbered. The running counter advances to the heading's
        own leading ordinal so a mixed doc (`## 1. Background` then `## Problem`)
        numbers monotonically (1, 2, ...) instead of emitting a duplicate `1.`.

        Every ROOT-LEVEL h2 visited here is tagged ``class="hub-section"``.
        Because we iterate only ``root``'s direct children, an h2 nested inside
        a blockquote / list / admonition is never tagged — so the later
        serialized-string passes (wrap_sections, build_toc) can match
        ``class="hub-section"`` to operate on root-level sections only and skip
        nested headings, which would otherwise produce mismatched
        ``<section>``/container tags and pollute the TOC.
        """
        n = 0
        for el in list(root):
            # Tag every ROOT-LEVEL decision heading (h3-h6 starting with `Q<n>`)
            # with `hub-qbox` so wrap_qboxes boxes only root-level Q headings and
            # skips ones nested in a blockquote/list/admonition (which would
            # straddle the container and emit mismatched <div>/<blockquote>).
            if el.tag in ("h3", "h4", "h5", "h6"):
                if _QHEADING_RE.match("".join(el.itertext()).strip()):
                    self._mark(el, "hub-qbox")
                continue
            if el.tag != "h2":
                continue
            self._mark(el, "hub-section")
            text = "".join(el.itertext()).strip()
            m = _ALREADY_NUMBERED_RE.match(text)
            if m:
                # Advance the counter past the heading's own top-level ordinal
                # so subsequent auto-numbered sections stay monotonic.
                n = max(n, int(m.group("ordinal")))
                continue
            n += 1
            slug = el.get("id") or ""
            new_id = f"{n}-{slug}" if slug else str(n)
            el.set("id", new_id)
            # The toc extension appended the permalink anchor as the last child;
            # repoint its href at the renumbered id.
            for child in el:
                if child.get("href") == f"#{slug}":
                    child.set("href", f"#{new_id}")
            # Leading prose lives in el.text (toc moved the anchor into a child).
            el.text = f"{n}. " + (el.text or "")

    @staticmethod
    def _mark(el, cls):
        """Append ``cls`` to a root-level heading's class attribute."""
        existing = el.get("class")
        el.set("class", f"{existing} {cls}" if existing else cls)


class HubExtension(Extension):
    def extendMarkdown(self, md):
        # Priority 4 < toc's 5 so ids/permalinks already exist when we
        # prepend section ordinals to el.text.
        md.treeprocessors.register(HubTreeprocessor(md), "fleet_hub", 4)


_PRE_BLOCK_RE = re.compile(r"<pre\b([^>]*)>(.*?)</pre>", re.IGNORECASE | re.DOTALL)


def wrap_diagrams(body_html: str) -> str:
    """Add class="diagram-block" to <pre> blocks holding ASCII diagrams.

    Sniffs each <pre> block's text content for box-drawing / arrow glyphs;
    three or more marks it as art rather than source code (a stray arrow in a
    code sample won't trigger it, but box diagrams carry many). Operates on
    the serialized body because fenced_code only materializes <pre> at
    serialization time (see HubTreeprocessor docstring).
    """
    def repl(m: re.Match) -> str:
        attrs, inner = m.group(1), m.group(2)
        text = re.sub(r"<[^>]+>", "", inner)
        glyphs = sum(1 for ch in text if ch in _DIAGRAM_GLYPHS)
        if glyphs < 3:
            return m.group(0)
        # Merge into any existing class attribute, else add one.
        if re.search(r'\bclass\s*=', attrs, re.IGNORECASE):
            new_attrs = re.sub(
                r'class\s*=\s*"([^"]*)"',
                lambda c: f'class="{c.group(1)} diagram-block"',
                attrs,
                count=1,
                flags=re.IGNORECASE,
            )
        else:
            new_attrs = attrs + ' class="diagram-block"'
        return f"<pre{new_attrs}>{inner}</pre>"

    return _PRE_BLOCK_RE.sub(repl, body_html)


def wrap_qboxes(body_html: str) -> str:
    """Wrap each `Q<n>` heading + its prose in a `<div class="qbox">`.

    A decision point spans from its heading until the next heading of
    equal-or-shallower depth (or end of doc). This reproduces the
    hand-authored `<div class="qbox" id="qN">` blocks of the hub template,
    derived automatically from `### Q3 — ...` markdown.

    Only h3+ Q headings tagged ``class="hub-qbox"`` by HubTreeprocessor are
    boxed. That marker is stamped on ROOT-LEVEL h3-h6 Q headings only, so:
      - a top-level `## Q1` is never boxed (it is a numbered section boundary;
        wrap_sections splits the body at each `<h2>` afterward, and an h2 qbox
        would straddle that cut), and
      - a `### Q1` nested in a blockquote/list/admonition is never boxed (the
        box would straddle the container and emit mismatched `<div>` tags).
    Nest decision points as plain `### Q<n>` under a `##` to box them.
    """
    heading_re = re.compile(r"<(h[1-6])\b([^>]*)>.*?</\1>", re.IGNORECASE | re.DOTALL)
    headings = [
        (m.start(), m.end(), int(m.group(1)[1]), m.group(0), m.group(2))
        for m in heading_re.finditer(body_html)
    ]

    out: list[str] = []
    last = 0
    i = 0
    while i < len(headings):
        h_start, _h_end, level, html_frag, attrs = headings[i]
        # Box only root-level h3+ Q headings, identified by the hub-qbox marker
        # class (see docstring). Visible-text matching is unreliable here
        # because section numbering may have prefixed the text and because a
        # container-nested heading must NOT be boxed.
        if 'hub-qbox' not in attrs:
            i += 1
            continue
        # End of this decision point: next heading with level <= this one.
        block_end = len(body_html)
        for j in range(i + 1, len(headings)):
            if headings[j][2] <= level:
                block_end = headings[j][0]
                break
        out.append(body_html[last:h_start])
        out.append('<div class="qbox">\n')
        out.append(body_html[h_start:block_end].rstrip())
        out.append("\n</div>\n")
        last = block_end
        # Skip headings consumed inside this block.
        while i < len(headings) and headings[i][0] < block_end:
            i += 1
    out.append(body_html[last:])
    return "".join(out)


_LEADING_H1_RE = re.compile(r"\s*<h1\b[^>]*>.*?</h1>\s*", re.IGNORECASE | re.DOTALL)


def strip_leading_h1(body_html: str) -> str:
    """Drop the source doc's leading ``<h1>`` from the rendered body.

    ``extract_title()`` already lifts the first ``# Title`` into the page
    ``<header>``; leaving the same ``<h1>`` in the body would render the title
    twice (and ``wrap_sections`` would bury it in an ``overview`` section). The
    hub exemplar (DESIGN-docs-publisher.html) carries the title only in the
    header, so strip the first ``<h1>`` if the body opens with one. Headings
    deeper in the doc are untouched.
    """
    return _LEADING_H1_RE.sub("", body_html, count=1) if body_html.lstrip().lower().startswith("<h1") else body_html


# Match only ROOT-LEVEL section headings: an <h2> open tag carrying the
# `hub-section` class (stamped by HubTreeprocessor on root-level h2s only) AND
# an id. Attribute order is not guaranteed by the serializer, so require both
# but allow either order. Nested h2s (inside blockquote/list/admonition) lack
# the class and are skipped — they don't become section boundaries or TOC rows.
_H2_OPEN_RE = re.compile(
    r'<h2\b(?=[^>]*\bclass="[^"]*\bhub-section\b[^"]*")'
    r'[^>]*?\bid="([^"]*)"[^>]*>',
    re.IGNORECASE,
)


def wrap_sections(body_html: str) -> str:
    """Wrap each top-level ``<h2 id=...>`` and its content in ``<section>``.

    Reproduces the hub exemplar skeleton (DESIGN-docs-publisher.html), where
    every numbered ``##`` heading lives inside ``<section id="N-slug">`` so the
    CSS grid (``body > section {{ grid-column: 2 }}``) and sticky-TOC layout
    apply. Any preamble before the first ``<h2>`` is wrapped in an
    ``<section id="overview">`` to keep it in the content column.
    """
    matches = list(_H2_OPEN_RE.finditer(body_html))
    if not matches:
        # No headings: keep everything in one section so the grid still applies.
        inner = body_html.strip()
        return f'<section id="overview">\n{inner}\n</section>\n' if inner else ""

    out: list[str] = []
    preamble = body_html[: matches[0].start()].strip()
    if preamble:
        out.append(f'<section id="overview">\n{preamble}\n</section>\n')

    for idx, m in enumerate(matches):
        sec_id = m.group(1)
        end = matches[idx + 1].start() if idx + 1 < len(matches) else len(body_html)
        chunk = body_html[m.start():end].strip()
        out.append(f'<section id="{sec_id}">\n{chunk}\n</section>\n')
    return "".join(out)


def build_toc(body_html: str) -> str:
    """Build the hub TOC ``<ol>`` from top-level ``<h2 id=...>`` headings.

    Mirrors the exemplar's flat ``nav.toc ol`` of numbered sections (one entry
    per ``##``), not python-markdown's nested ``<ul>``. Link text is the
    heading's visible text with the permalink ``¶`` stripped.
    """
    items: list[str] = []
    for m in _H2_OPEN_RE.finditer(body_html):
        sec_id = m.group(1)
        # Heading text runs from the end of the open tag to its </h2>.
        tail = body_html[m.end():]
        close = tail.lower().find("</h2>")
        inner = tail[:close] if close != -1 else tail
        # Drop the permalink anchor, then strip remaining tags + entities.
        inner = re.sub(
            r'<a\b[^>]*class="heading-anchor"[^>]*>.*?</a>', "", inner,
            flags=re.IGNORECASE | re.DOTALL,
        )
        text = re.sub(r"<[^>]+>", "", inner).strip()
        text = html.unescape(text)
        items.append(
            f'<li><a href="#{html.escape(sec_id)}">{html.escape(text)}</a></li>'
        )
    if not items:
        return ""
    return "<ol>\n  " + "\n  ".join(items) + "\n</ol>"


# --- Conversion --------------------------------------------------------------

def extract_title(md_text: str) -> str:
    """First H1 wins; fall back to first non-blank line."""
    for line in md_text.splitlines():
        m = re.match(r"^#\s+(.+?)\s*$", line)
        if m:
            return m.group(1)
        if line.strip():
            return line.strip()
    return "Design Doc"


def render_markdown(md_text: str) -> tuple[str, str]:
    """Convert markdown to (body_html, toc_html) in the hub style.

    Pure function (no filesystem) so tests can drive it on in-memory fixtures.
    """
    md = markdown.Markdown(
        extensions=[
            "extra",        # tables, fenced_code, def_list, attr_list, etc.
            "toc",
            "sane_lists",
            "admonition",
            HubExtension(),
        ],
        extension_configs={
            "toc": {
                "permalink": "¶",   # ¶
                "permalink_class": "heading-anchor",
                "permalink_title": "Permalink to this section",
                "toc_depth": "2-4",
            },
        },
        output_format="html5",
    )
    body_html = md.convert(md_text)
    body_html = strip_leading_h1(body_html)
    body_html = wrap_diagrams(body_html)
    body_html = wrap_qboxes(body_html)
    # Build the hub TOC from the (now numbered) top-level headings BEFORE
    # wrapping sections — wrap_sections only reflows markup, not ids, but
    # building first keeps the two passes independent.
    toc_html = build_toc(body_html)
    body_html = wrap_sections(body_html)
    return body_html, toc_html


def render(md_path: Path) -> Path:
    """Convert md_path to a sibling .html. Returns the html path."""
    md_text = md_path.read_text(encoding="utf-8")
    title = extract_title(md_text)
    body_html, toc_html = render_markdown(md_text)

    rendered_at = _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")

    out = HTML_TEMPLATE.format(
        title=html.escape(title),
        toc=toc_html,
        body=body_html,
        source_path=html.escape(str(md_path.name)),
        rendered_at=rendered_at,
    )
    html_path = md_path.with_suffix(".html")
    html_path.write_text(out, encoding="utf-8")
    return html_path


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("md_path", help="Path to .md file (writes .html next to it).")
    args = ap.parse_args(argv)

    md_path = Path(args.md_path)
    if not md_path.exists():
        sys.stderr.write(f"error: {md_path}: file not found\n")
        return 1
    if md_path.suffix.lower() != ".md":
        sys.stderr.write(f"error: {md_path}: expected .md suffix\n")
        return 1

    out = render(md_path)
    print(f"wrote {out} ({out.stat().st_size} bytes)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
