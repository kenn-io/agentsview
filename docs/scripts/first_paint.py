#!/usr/bin/env python3
"""Make every page of the assembled site paint with its fonts in place.

Embeds each /fonts/*.woff2 a stylesheet references as a data URL, so a font is
loaded once its stylesheet is, and inlines hold-first-paint.js right after each
page's <meta charset>, so Chromium does not paint a frame in a fallback face.
"""

from __future__ import annotations

import argparse
import base64
import pathlib
import re

HOLD_SCRIPT = pathlib.Path(__file__).resolve().parent / "hold-first-paint.js"

# The stylesheets that declare @font-face rules, relative to the site root.
FONT_STYLESHEETS = ("styles/site.css", "docs/stylesheets/extra.css")

FONT_URL_RE = re.compile(r'url\("/fonts/([^"/]+\.woff2)"\)')
CHARSET_RE = re.compile(r'<meta charset="utf-8">', re.IGNORECASE)
# Browsers only look for the charset declaration in a page's first 1024 bytes.
CHARSET_SCAN_BYTES = 1024


def embed_fonts(site: pathlib.Path, stylesheet: pathlib.Path) -> None:
    text = stylesheet.read_text(encoding="utf-8")

    def data_url(match: re.Match[str]) -> str:
        font = site / "fonts" / match.group(1)
        encoded = base64.b64encode(font.read_bytes()).decode("ascii")
        return f'url("data:font/woff2;base64,{encoded}")'

    embedded, count = FONT_URL_RE.subn(data_url, text)
    if count == 0:
        raise SystemExit(f"{stylesheet}: no /fonts/*.woff2 references to embed")
    stylesheet.write_text(embedded, encoding="utf-8")


def hold_first_paint(page: pathlib.Path, script: str) -> None:
    text = page.read_text(encoding="utf-8")
    match = CHARSET_RE.search(text)
    if match is None:
        raise SystemExit(f'{page}: no <meta charset="utf-8"> to follow')
    if len(text[: match.end()].encode("utf-8")) > CHARSET_SCAN_BYTES:
        raise SystemExit(
            f"{page}: <meta charset> is past the first {CHARSET_SCAN_BYTES} bytes"
        )
    page.write_text(
        f"{text[: match.end()]}\n<script>{script}</script>{text[match.end() :]}",
        encoding="utf-8",
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("site", type=pathlib.Path, help="assembled site directory")
    site: pathlib.Path = parser.parse_args().site

    for stylesheet in FONT_STYLESHEETS:
        embed_fonts(site, site / stylesheet)
    script = HOLD_SCRIPT.read_text(encoding="utf-8")
    pages = sorted(site.rglob("*.html"))
    if not pages:
        raise SystemExit(f"{site}: no HTML pages")
    for page in pages:
        hold_first_paint(page, script)


if __name__ == "__main__":
    main()
