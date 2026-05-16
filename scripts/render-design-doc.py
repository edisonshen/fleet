#!/usr/bin/env python3
"""render-design-doc.py — Convert a markdown design doc to a polished HTML page.

Usage:
    python3 scripts/render-design-doc.py docs/DESIGN-foo.md
    # Writes docs/DESIGN-foo.html next to the source.

Why this exists:
    Operator preference (2026-05-15): design docs ship as both formats —
    .md for agents (codex, claude, plan-eng-review skills) and .html
    for humans who want readable rendered output without an editor.

Dependencies:
    - Python 3.9+
    - markdown >= 3.4 (pip3 install --user markdown)

Self-contained output:
    The generated .html has all CSS inlined; no external fonts, no
    JS frameworks, no analytics. Drop in a browser and it renders.
    Dark/light mode auto-toggles via prefers-color-scheme.
"""
from __future__ import annotations

import argparse
import html
import re
import sys
from pathlib import Path

try:
    import markdown
except ImportError:
    sys.stderr.write(
        "error: python markdown library missing.\n"
        "  pip3 install --user markdown\n"
    )
    sys.exit(2)


# --- HTML template -----------------------------------------------------------

# Embedded CSS — system fonts only, no external requests.
# Dark mode via prefers-color-scheme.
HTML_TEMPLATE = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{title}</title>
<style>
:root {{
  --bg: #fdfdfc;
  --fg: #1a1a1a;
  --muted: #6b6b6b;
  --accent: #0066cc;
  --border: #e3e3e0;
  --code-bg: #f4f4f1;
  --code-fg: #1a1a1a;
  --table-stripe: #f9f9f7;
  --blockquote-border: #d0d0cd;
  --blockquote-bg: #f7f7f4;
  --kbd-bg: #f0f0ec;
}}
@media (prefers-color-scheme: dark) {{
  :root {{
    --bg: #1a1a1a;
    --fg: #e8e8e6;
    --muted: #9a9a98;
    --accent: #7ab8ff;
    --border: #333331;
    --code-bg: #252523;
    --code-fg: #e8e8e6;
    --table-stripe: #222220;
    --blockquote-border: #444442;
    --blockquote-bg: #222220;
    --kbd-bg: #2e2e2c;
  }}
}}
* {{ box-sizing: border-box; }}
html, body {{
  margin: 0;
  padding: 0;
  background: var(--bg);
  color: var(--fg);
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
  font-size: 16px;
  line-height: 1.6;
  -webkit-font-smoothing: antialiased;
}}
.wrap {{
  display: grid;
  grid-template-columns: 240px 1fr;
  max-width: 1240px;
  margin: 0 auto;
  padding: 2rem 1.5rem;
  gap: 2rem;
}}
@media (max-width: 900px) {{
  .wrap {{ grid-template-columns: 1fr; }}
  .toc {{ position: static !important; max-height: none !important; }}
}}
.toc {{
  position: sticky;
  top: 1rem;
  align-self: start;
  max-height: calc(100vh - 2rem);
  overflow-y: auto;
  font-size: 14px;
  padding-right: 0.5rem;
}}
.toc h2 {{
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--muted);
  margin: 0 0 0.5rem 0;
  font-weight: 600;
}}
.toc ul {{
  list-style: none;
  padding: 0;
  margin: 0;
}}
.toc ul ul {{ padding-left: 1rem; }}
.toc li {{ margin: 0.25rem 0; }}
.toc a {{
  color: var(--fg);
  text-decoration: none;
  display: block;
  padding: 0.15rem 0;
  border-left: 2px solid transparent;
  padding-left: 0.5rem;
  margin-left: -0.5rem;
  transition: color 0.1s, border-color 0.1s;
}}
.toc a:hover {{
  color: var(--accent);
  border-left-color: var(--accent);
}}
main {{
  min-width: 0;
}}
.doc-meta {{
  font-size: 13px;
  color: var(--muted);
  margin-bottom: 2rem;
  padding-bottom: 1rem;
  border-bottom: 1px solid var(--border);
}}
.doc-meta strong {{ color: var(--fg); }}
h1, h2, h3, h4, h5, h6 {{
  margin: 1.8em 0 0.5em 0;
  line-height: 1.25;
  font-weight: 600;
}}
h1 {{ font-size: 2rem; margin-top: 0; }}
h2 {{ font-size: 1.5rem; padding-bottom: 0.3rem; border-bottom: 1px solid var(--border); }}
h3 {{ font-size: 1.2rem; }}
h4 {{ font-size: 1.05rem; }}
h5, h6 {{ font-size: 1rem; color: var(--muted); }}
h1 .anchor, h2 .anchor, h3 .anchor, h4 .anchor, h5 .anchor, h6 .anchor {{
  visibility: hidden;
  margin-left: 0.3rem;
  text-decoration: none;
  color: var(--muted);
  font-weight: 400;
}}
h1:hover .anchor, h2:hover .anchor, h3:hover .anchor,
h4:hover .anchor, h5:hover .anchor, h6:hover .anchor {{
  visibility: visible;
}}
p {{ margin: 0.8em 0; }}
a {{ color: var(--accent); text-decoration: underline; text-decoration-thickness: 1px; text-underline-offset: 2px; }}
a:hover {{ text-decoration-thickness: 2px; }}
ul, ol {{ padding-left: 1.5rem; }}
li {{ margin: 0.3em 0; }}
strong {{ font-weight: 600; }}
em {{ font-style: italic; }}
code {{
  font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  font-size: 0.92em;
  background: var(--code-bg);
  color: var(--code-fg);
  padding: 0.15em 0.4em;
  border-radius: 3px;
}}
pre {{
  background: var(--code-bg);
  color: var(--code-fg);
  padding: 1rem 1.2rem;
  border-radius: 5px;
  overflow-x: auto;
  font-size: 13.5px;
  line-height: 1.5;
  border: 1px solid var(--border);
}}
pre code {{
  background: none;
  padding: 0;
  border-radius: 0;
  font-size: inherit;
}}
blockquote {{
  border-left: 3px solid var(--blockquote-border);
  background: var(--blockquote-bg);
  margin: 1em 0;
  padding: 0.5em 1em;
  color: var(--fg);
  font-style: normal;
}}
blockquote p:first-child {{ margin-top: 0; }}
blockquote p:last-child {{ margin-bottom: 0; }}
table {{
  border-collapse: collapse;
  width: 100%;
  margin: 1em 0;
  font-size: 14px;
  display: block;
  overflow-x: auto;
}}
thead {{ background: var(--table-stripe); }}
th, td {{
  border: 1px solid var(--border);
  padding: 0.5rem 0.8rem;
  text-align: left;
  vertical-align: top;
}}
th {{ font-weight: 600; }}
tbody tr:nth-child(even) {{ background: var(--table-stripe); }}
hr {{
  border: none;
  border-top: 1px solid var(--border);
  margin: 2em 0;
}}
kbd {{
  background: var(--kbd-bg);
  border: 1px solid var(--border);
  border-radius: 3px;
  padding: 0.1em 0.4em;
  font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  font-size: 0.9em;
  box-shadow: 0 1px 0 var(--border);
}}
.toc-anchor {{
  display: inline-block;
  width: 0;
  height: 0;
  overflow: hidden;
}}
input[type="checkbox"][disabled] {{ margin-right: 0.4em; }}
</style>
</head>
<body>
<div class="wrap">
  <nav class="toc" aria-label="table of contents">
    <h2>Contents</h2>
    {toc}
  </nav>
  <main>
    <div class="doc-meta">
      <strong>Source:</strong> <code>{source_path}</code> &mdash;
      <strong>Rendered:</strong> {rendered_at} &mdash;
      Agents read the <code>.md</code>; humans read the <code>.html</code>.
    </div>
    {body}
  </main>
</div>
</body>
</html>
"""


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


def render(md_path: Path) -> Path:
    """Convert md_path to a sibling .html. Returns the html path."""
    md_text = md_path.read_text(encoding="utf-8")
    title = extract_title(md_text)

    md = markdown.Markdown(
        extensions=[
            "extra",        # tables, fenced_code, def_list, attr_list
            "toc",
            "sane_lists",
            "admonition",
        ],
        extension_configs={
            "toc": {
                "permalink": "¶",
                "permalink_class": "anchor",
                "permalink_title": "Permalink to this section",
                "toc_depth": "2-4",
            },
        },
        output_format="html5",
    )
    body_html = md.convert(md_text)
    toc_html = md.toc

    import datetime as _dt
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
