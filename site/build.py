#!/usr/bin/env python3
"""Render the docs site into _site/.

The markdown under docs/ stays canonical — an agent that clones this
repository reads those files, not this site. Everything here wraps them in a
shell. Pages under site/pages/ are hand-written HTML fragments for the
narrative material that has no markdown source.
"""

import re
import shutil
import sys
from pathlib import Path

try:
    import markdown
except ImportError:
    sys.exit("build.py needs the markdown package: pip install -r site/requirements.txt")

ROOT = Path(__file__).resolve().parent.parent
SITE = ROOT / "site"
OUT = ROOT / "_site"

# Order is the nav order. A markdown source means docs/<slug>.md; None means a
# hand-written fragment at site/pages/<slug>.html.
PAGES = [
    ("index", "Overview", None, "A fleet of coding agents, one git worktree each: started on a GitHub issue, watched while they run, and cleaned up once the work has landed."),
    ("patterns", "Patterns", None, "Handing work to another agent and knowing whether it actually happened: verify don't relay, fixed-slot reports, and what stays in your own head."),
    ("examples", "Examples", None, "Five fan-outs end to end — a Sentry backlog, an issue burn-down, a wide mechanical change, a flaky test, and the case for not fanning out at all."),
    ("dispatch", "Dispatch", "dispatch.md", "Handing a slice of work to another agent and knowing when it is done."),
    ("json", "JSON", "json.md", "The --json output every command takes, what each null means, and why absence and unanswered are different fields."),
    ("pruning", "Pruning", "pruning.md", "What authorises a removal, why git topology never does it alone, and the guards."),
    ("events", "Events", "events.md", "The hooks that adopt and staff automatically, and why they are off until you ask."),
    ("trust", "Trust", "trust.md", "What running unsandboxed means here, and what the install path does and does not prove."),
    ("reference", "Reference", "reference.md", "Exit codes, the errors you are likely to meet, keybindings, and the smaller behaviours."),
]

GITHUB = "https://github.com/steig/worktender/blob/main"

# Every doc ends with a rule and a link back up to the README. The site has a
# nav for that.
FOOTER = re.compile(r"\n---\s*\n+\[← README\]\(\.\./README\.md\)\s*$")


def rewrite_links(html):
    html = html.replace('href="../README.md"', 'href="index.html"')
    html = html.replace('href="../SECURITY.md"', f'href="{GITHUB}/SECURITY.md"')
    # The fragment is optional and has to survive: a cross-document link that
    # points at a section — docs/reference.md and docs/json.md both have one —
    # otherwise keeps its .md and lands nowhere on the site.
    html = re.sub(r'href="(?:\./)?([a-z-]+)\.md(#[^"]*)?"', r'href="\1.html\2"', html)
    return html


def wrap_tables(html):
    """Tables are the one thing here wider than the column, so they scroll."""
    return html.replace("<table>", '<div class="scroll"><table>').replace("</table>", "</table></div>")


def render_markdown(path):
    text = FOOTER.sub("", path.read_text())
    # toc is here for the heading ids alone, not for a table of contents. Three
    # cross-document links point at a section, and without ids every one of them
    # lands at the top of a long page looking like nothing more was written.
    md = markdown.Markdown(extensions=["fenced_code", "tables", "sane_lists", "attr_list", "toc"])
    return wrap_tables(rewrite_links(md.convert(text)))


def nav_html(current):
    rows = []
    for slug, label, _, _ in PAGES:
        here = ' aria-current="page"' if slug == current else ""
        rows.append(f'    <a href="{slug}.html"{here}>{label}</a>')
    return "\n".join(rows)


def main():
    template = (SITE / "template.html").read_text()

    if OUT.exists():
        shutil.rmtree(OUT)
    OUT.mkdir()

    for slug, label, source, description in PAGES:
        if source:
            content = render_markdown(ROOT / "docs" / source)
            title = f"{label} — worktender"
        else:
            content = (SITE / "pages" / f"{slug}.html").read_text()
            title = "worktender" if slug == "index" else f"{label} — worktender"

        page = (template
                .replace("__TITLE__", title)
                .replace("__DESCRIPTION__", description)
                .replace("__BODYCLASS__", slug)
                .replace("__NAV__", nav_html(slug))
                .replace("__CONTENT__", content))
        (OUT / f"{slug}.html").write_text(page)
        print(f"  {slug}.html")

    shutil.copy(SITE / "style.css", OUT / "style.css")
    print(f"built {len(PAGES)} pages into {OUT.relative_to(ROOT)}/")


if __name__ == "__main__":
    main()
