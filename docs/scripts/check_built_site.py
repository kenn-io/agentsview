#!/usr/bin/env python3
from __future__ import annotations

import html
import html.parser
import pathlib
import re
import sys
import urllib.parse
import xml.etree.ElementTree as ET

ROOT = pathlib.Path(__file__).resolve().parents[1]
SITE = ROOT / "site"

ROUTES = [
    "/",
    "/quickstart/",
    "/usage/",
    "/session-intelligence/",
    "/token-usage/",
    "/chat-import/",
    "/insights/",
    "/commands/",
    "/stats/",
    "/session-api/",
    "/configuration/",
    "/remote-access/",
    "/pg-sync/",
    "/duckdb/",
    "/changelog/",
]

REQUIRED_FRAGMENTS = [
    "/configuration/#session-discovery",
    "/token-usage/#how-it-compares-to-ccusage",
    "/session-api/#agentsview-session-usage",
]

REQUIRED_META = {
    ("property", "og:image"): "https://agentsview.io/assets/static/og-image.png",
    ("property", "og:image:width"): "1200",
    ("property", "og:image:height"): "630",
    ("property", "og:type"): "website",
    ("property", "og:site_name"): "AgentsView",
    ("name", "twitter:card"): "summary_large_image",
    ("name", "twitter:image"): "https://agentsview.io/assets/static/og-image.png",
}

REQUIRED_SITEMAP_URLS = [
    "https://agentsview.io/",
]

COMPACT_SVG_MAX_HEIGHTS: dict[str, float] = {}

FORBIDDEN_PATTERNS = [
    "virtual:starlight",
    "@astrojs/starlight",
    "discord-top-link",
    "<aside class=\"md-banner\"",
    "<Tabs",
    "<TabItem",
    "<Card",
    "<CardGrid",
    "<Screenshot",
    "<Aside",
    "set:html",
]

MAX_HTML_UNESCAPE_PASSES = 50

FETCHED_LINK_RELS = {
    "apple-touch-icon",
    "apple-touch-startup-image",
    "icon",
    "manifest",
    "mask-icon",
    "modulepreload",
    "prefetch",
    "preload",
    "prerender",
    "stylesheet",
}

CSS_URL_RE = re.compile(
    r"url\(\s*(?:\"([^\"]*)\"|'([^']*)'|([^)]*?))\s*\)", re.IGNORECASE
)
CSS_IMPORT_RE = re.compile(
    r"@import\s+(?:url\(\s*(?:\"([^\"]*)\"|'([^']*)'|([^)]*?))\s*\)|\"([^\"]*)\"|'([^']*)')",
    re.IGNORECASE,
)


def srcset_urls(value: str) -> list[str]:
    urls: list[str] = []
    index = 0
    while index < len(value):
        while index < len(value) and value[index] in " \t\r\n,":
            index += 1
        if index >= len(value):
            break

        start = index
        if value.startswith("data:", index):
            while index < len(value) and not value[index].isspace():
                index += 1
        else:
            while index < len(value) and not value[index].isspace() and value[index] != ",":
                index += 1

        url = value[start:index]
        if url:
            urls.append(url)

        while index < len(value) and value[index] != ",":
            index += 1
        if index < len(value):
            index += 1

    return urls


def css_url_refs(text: str) -> list[str]:
    refs: list[str] = []
    for match in CSS_URL_RE.finditer(text):
        ref = next((group for group in match.groups() if group is not None), "")
        ref = ref.strip()
        if ref:
            refs.append(ref)
    return refs


def css_import_refs(text: str) -> list[str]:
    refs: list[str] = []
    for match in CSS_IMPORT_RE.finditer(text):
        ref = next((group for group in match.groups() if group is not None), "")
        ref = ref.strip()
        if ref:
            refs.append(ref)
    return refs


def rel_tokens(value: str) -> set[str]:
    return {token.lower() for token in value.split()}


def is_fetched_link_resource(attrs: dict[str, str]) -> bool:
    return bool(rel_tokens(attrs.get("rel", "")) & FETCHED_LINK_RELS)


class LinkParser(html.parser.HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.ids: set[str] = set()
        self.links: list[str] = []
        self.link_attrs: list[dict[str, str]] = []
        self.assets: list[str] = []
        self.style_attrs: list[str] = []
        self.style_blocks: list[str] = []
        self.meta: list[dict[str, str]] = []
        self._in_style = False

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        attr = {key: value or "" for key, value in attrs}
        if "id" in attr:
            self.ids.add(attr["id"])
        if tag == "a" and "href" in attr:
            self.links.append(attr["href"])
            self.link_attrs.append(attr)
        if tag in {"img", "script", "source"} and "src" in attr:
            self.assets.append(attr["src"])
        if tag in {"img", "source"} and "srcset" in attr:
            self.assets.extend(srcset_urls(attr["srcset"]))
        if tag == "video" and "poster" in attr:
            self.assets.append(attr["poster"])
        if tag == "link" and "href" in attr and is_fetched_link_resource(attr):
            self.assets.append(attr["href"])
        if tag == "meta":
            self.meta.append(attr)
        if "style" in attr:
            self.style_attrs.append(attr["style"])
        if tag == "style":
            self._in_style = True

    def handle_data(self, data: str) -> None:
        if self._in_style:
            self.style_blocks.append(data)

    def handle_endtag(self, tag: str) -> None:
        if tag == "style":
            self._in_style = False


def route_to_file(route: str) -> pathlib.Path:
    if route == "/":
        return SITE / "index.html"
    return SITE / route.strip("/") / "index.html"


def is_local_file_path(path: str) -> bool:
    return pathlib.PurePosixPath(path).suffix != ""


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def svg_length(value: str, path: pathlib.Path, attr: str) -> float:
    match = re.fullmatch(r"\s*([0-9]+(?:\.[0-9]+)?)(?:px)?\s*", value)
    if match is None:
        fail(f"invalid SVG {attr} value {value!r} in {path}")
    return float(match.group(1))


def check_compact_svg_assets() -> None:
    for name, max_height in COMPACT_SVG_MAX_HEIGHTS.items():
        path = SITE / name
        if not path.is_file():
            fail(f"missing compact SVG asset {name}")

        try:
            root = ET.parse(path).getroot()
        except ET.ParseError as exc:
            fail(f"invalid compact SVG asset {name}: {exc}")

        height_attr = root.get("height")
        if height_attr is None:
            fail(f"compact SVG asset {name} is missing height")

        height = svg_length(height_attr, path, "height")
        if height > max_height:
            fail(f"compact SVG asset {name} is {height:g}px tall, expected at most {max_height:g}px")


def require_site_child(target: pathlib.Path, reference: str, current: pathlib.Path) -> pathlib.Path:
    resolved = target.resolve()
    site = SITE.resolve()
    if not resolved.is_relative_to(site):
        fail(f"local reference escapes site output: {reference} in {current}")
    return resolved


def target_file(current: pathlib.Path, href: str) -> pathlib.Path | None:
    parsed = urllib.parse.urlparse(href)
    if parsed.scheme or parsed.netloc:
        return None
    decoded_path = urllib.parse.unquote(parsed.path)
    if decoded_path.startswith("/"):
        if is_local_file_path(decoded_path):
            return require_site_child(SITE / decoded_path.lstrip("/"), href, current)
        route = decoded_path if decoded_path.endswith("/") else decoded_path + "/"
        return require_site_child(route_to_file(route), href, current)

    base = current.parent
    path = decoded_path or current.name
    resolved = (base / path).resolve()
    require_site_child(resolved, href, current)
    if resolved.is_dir():
        return require_site_child(resolved / "index.html", href, current)
    if resolved.suffix:
        return resolved
    return require_site_child(resolved / "index.html", href, current)


def parse_html(path: pathlib.Path) -> LinkParser:
    parser = LinkParser()
    parser.feed(path.read_text(encoding="utf-8"))
    return parser


def html_unescape_variants(text: str) -> list[str]:
    variants = [text]
    current = text
    for _ in range(MAX_HTML_UNESCAPE_PASSES):
        unescaped = html.unescape(current)
        if unescaped == current:
            break
        variants.append(unescaped)
        current = unescaped
    return variants


def check_global_metadata(current: pathlib.Path, parser: LinkParser) -> None:
    for (kind, name), value in REQUIRED_META.items():
        found = any(
            meta.get(kind) == name and meta.get("content") == value for meta in parser.meta
        )
        if not found:
            fail(f"missing metadata {kind}={name} with value {value} in {current}")


def check_discord_header_link(current: pathlib.Path, parser: LinkParser) -> None:
    found = any(
        link.get("href") == "https://discord.gg/fDnmxB8Wkq"
        and "agentsview-discord-link" in link.get("class", "").split()
        and link.get("aria-label") == "Join Discord"
        for link in parser.link_attrs
    )
    if not found:
        fail(f"missing Discord header link in {current}")


def check_local_asset(current: pathlib.Path, asset: str) -> pathlib.Path | None:
    parsed = urllib.parse.urlparse(asset)
    if parsed.scheme or parsed.netloc or asset.startswith("data:"):
        return None
    decoded_path = urllib.parse.unquote(parsed.path)
    if not decoded_path:
        return None
    if decoded_path.startswith("/"):
        target = SITE / decoded_path.lstrip("/")
    else:
        target = current.parent / decoded_path
    target = require_site_child(target, asset, current)
    if not target.is_file():
        fail(f"missing asset {asset} referenced by {current}")
    return target


def check_css_text(current: pathlib.Path, text: str, visited: set[pathlib.Path]) -> None:
    for asset in css_import_refs(text):
        target = check_local_asset(current, asset)
        if target is not None and target.suffix.lower() == ".css":
            check_css_assets(target, visited)

    for asset in css_url_refs(text):
        check_local_asset(current, asset)


def check_css_assets(css_file: pathlib.Path, visited: set[pathlib.Path] | None = None) -> None:
    if visited is None:
        visited = set()
    css_file = css_file.resolve()
    if css_file in visited:
        return
    visited.add(css_file)

    text = css_file.read_text(encoding="utf-8", errors="ignore")
    check_css_text(css_file, text, visited)


def fragment_id(fragment: str) -> str:
    return urllib.parse.unquote(fragment)


def main() -> None:
    if not SITE.exists():
        fail("site directory does not exist. Run the Zensical build first.")

    for route in ROUTES:
        path = route_to_file(route)
        if not path.exists():
            fail(f"missing route {route}: {path}")

    if not (SITE / "404.html").exists():
        fail("missing 404.html")
    if not (SITE / "sitemap.xml").exists():
        fail("missing sitemap.xml")
    sitemap_text = (SITE / "sitemap.xml").read_text(encoding="utf-8", errors="ignore")
    for url in REQUIRED_SITEMAP_URLS:
        if f"<loc>{url}</loc>" not in sitemap_text:
            fail(f"missing sitemap URL {url}")

    html_files = list(SITE.rglob("*.html"))
    all_text = "\n".join(
        path.read_text(encoding="utf-8", errors="ignore") for path in html_files
    )
    text_variants = html_unescape_variants(all_text)
    for pattern in FORBIDDEN_PATTERNS:
        if any(pattern in text for text in text_variants):
            fail(f"forbidden generated marker found: {pattern}")

    parsed_by_file: dict[pathlib.Path, LinkParser] = {}
    for path in html_files:
        parsed_by_file[path.resolve()] = parse_html(path)

    for current, parser in parsed_by_file.items():
        check_global_metadata(current, parser)
        check_discord_header_link(current, parser)

    for spec in REQUIRED_FRAGMENTS:
        parsed = urllib.parse.urlparse(spec)
        path = route_to_file(parsed.path)
        parser = parsed_by_file.get(path.resolve())
        if parser is None:
            fail(f"required fragment page missing: {spec}")
        if fragment_id(parsed.fragment) not in parser.ids:
            fail(f"required fragment missing: {spec}")

    css_files: set[pathlib.Path] = set()
    visited_css: set[pathlib.Path] = set()
    for current, parser in parsed_by_file.items():
        for href in parser.links:
            parsed = urllib.parse.urlparse(href)
            if href.startswith("#"):
                fragment = fragment_id(parsed.fragment)
                if fragment and fragment not in parser.ids:
                    fail(f"missing local fragment {href} in {current}")
                continue
            target = target_file(current, href)
            if target is None:
                continue
            if target.suffix == ".html":
                if parsed.fragment:
                    target_parser = parsed_by_file.get(target.resolve())
                    if target_parser is None:
                        fail(f"missing linked page for fragment {href} in {current}")
                    if fragment_id(parsed.fragment) not in target_parser.ids:
                        fail(f"missing fragment {href} in {target}")
                elif not target.exists():
                    fail(f"missing internal page {href} in {current}")
            elif not target.exists():
                fail(f"missing linked file {href} in {current}")

        for asset in parser.assets:
            target = check_local_asset(current, asset)
            if target is not None and target.suffix.lower() == ".css":
                css_files.add(target)

        for css_text in parser.style_attrs:
            for asset in css_url_refs(css_text):
                check_local_asset(current, asset)

        for css_text in parser.style_blocks:
            check_css_text(current, css_text, visited_css)

    for css_file in css_files:
        check_css_assets(css_file, visited_css)

    check_compact_svg_assets()

    print("built site checks passed")


if __name__ == "__main__":
    main()
