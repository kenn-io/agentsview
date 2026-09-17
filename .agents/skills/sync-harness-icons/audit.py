#!/usr/bin/env python3
"""Report harnesses that have no mark on the docs landing grid or in kit-ui.

Reads the provider list from docs/internal/session-format-sources.md, the chip
and sprite lists from docs/website/index.html, and (optionally) the HarnessIcon
registry from a kit-ui checkout. Matching is by normalized name, so the output
is a review list for a person or agent, not a verdict.

Usage: audit.py [--kit-ui PATH]   (run from the repository root)
"""
import argparse
import pathlib
import re
import sys

ROOT = pathlib.Path.cwd()
if not (ROOT / "go.mod").exists() or not (ROOT / "docs/website/index.html").exists():
    sys.exit("run from the root of an agentsview checkout")

# Provider keys that never get a chip: importers and IDE/CLI variants whose
# brand is already on the grid under another product. Extend deliberately.
INTENTIONALLY_UNLISTED = {
    "claude-ai": "chat export importer, not a harness",
    "chatgpt": "chat export importer, not a harness",
    "gemini-apps": "chat export importer, not a harness",
    "kiro-ide": "same brand as kiro",
    "antigravity-cli": "same brand as antigravity",
}


SUFFIXES = ("cli", "ide", "agent", "code", "tui")


def norm(s: str) -> str:
    return re.sub(r"[^a-z0-9]", "", re.sub(r"\(.*?\)", "", s).lower())


def stem(s: str) -> str:
    """Normalize and strip product-type suffixes: 'Kilo Code' and 'Kiro CLI' stem to their brand."""
    n = norm(s)
    changed = True
    while changed:
        changed = False
        for suf in SUFFIXES:
            if n.endswith(suf) and len(n) > len(suf):
                n, changed = n[: -len(suf)], True
    return n


def providers():
    text = (ROOT / "docs/internal/session-format-sources.md").read_text()
    return re.findall(r"^## (.+?) \(`([a-z0-9-]+)`\)\s*$", text, flags=re.M)


def chips():
    html = (ROOT / "docs/website/index.html").read_text()
    names = re.findall(r'agent-chip-name">([^<]+)<', html)
    symbols = re.findall(r'<symbol id="i-([a-z0-9-]+)"', html)
    text_glyphs = re.findall(r'agent-chip-glyph">([^<]*)</span><span class="agent-chip-name">([^<]+)<', html)
    return names, symbols, [n for _, n in text_glyphs]


def kit_ui_ids(path: pathlib.Path):
    reg = (path / "src/lib/components/harness-icon.ts").read_text()
    ids = set(re.findall(r'\{ id: "([a-z0-9-]+)"', reg))
    agents = set()
    for m in re.finditer(r"agents: \[([^\]]*)\]", reg):
        agents.update(re.findall(r'"([^"]+)"', m.group(1)))
    return ids, agents


def matches(display: str, key: str, names) -> bool:
    """A chip covers a provider when the names agree exactly, agree after
    stripping product-type suffixes, the chip equals the provider key, or the
    chip ends with the provider name (Claude Cowork covers Cowork)."""
    want = {norm(display), stem(display), norm(key)}
    for n in names:
        c, cs = norm(n), stem(n)
        if c in want or cs in want:
            return True
        if len(norm(display)) >= 5 and c.endswith(norm(display)):
            return True
    return False


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--kit-ui", type=pathlib.Path, help="path to a kit-ui checkout")
    args = ap.parse_args()

    provs = providers()
    names, symbols, text_glyphs = chips()
    print(f"providers: {len(provs)}  chips: {len(names)}  sprite symbols: {len(symbols)}")
    if text_glyphs:
        print("chips still using a text tag: " + ", ".join(text_glyphs))

    missing = [(d, k) for d, k in provs if k not in INTENTIONALLY_UNLISTED and not matches(d, k, names)]
    print("\nproviders with no landing-grid chip (review each):")
    for d, k in missing or [("none", "")]:
        print(f"  {d} ({k})" if k else "  none")

    if args.kit_ui:
        ids, agents = kit_ui_ids(args.kit_ui)
        print(f"\nkit-ui HarnessIcon ids: {len(ids)}")
        sprite_only = sorted(set(symbols) - ids)
        print("sprite symbols with no kit-ui id: " + (", ".join(sprite_only) or "none"))
        agent_norms = {norm(a) for a in agents}
        chip_only = sorted(n for n in names if norm(n) not in agent_norms)
        print("chips not named in any kit-ui agents list: " + (", ".join(chip_only) or "none"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
