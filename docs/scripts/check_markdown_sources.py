#!/usr/bin/env python3
from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
FRONTMATTER_LIMIT = 80
ADMONITION_DIRECTIVE_RE = re.compile(
    r'!!!\s+[A-Za-z][\w-]*(?:\s+"[^"]+")?\s*'
)


def public_markdown_files() -> list[pathlib.Path]:
    return [
        path
        for path in sorted(ROOT.glob("*.md"))
        if path.name != "README.md"
    ]


def check_frontmatter(path: pathlib.Path, lines: list[str], errors: list[str]) -> None:
    if not lines or lines[0] != "---":
        errors.append(f"{path.relative_to(ROOT)}: missing YAML frontmatter")
        return

    closing = next(
        (
            index
            for index, line in enumerate(lines[1:FRONTMATTER_LIMIT], start=1)
            if line == "---"
        ),
        None,
    )
    if closing is None:
        errors.append(
            f"{path.relative_to(ROOT)}: missing closing YAML frontmatter delimiter"
        )
        return

    frontmatter = "\n".join(lines[1:closing])
    if not re.search(r"^title:\s+\S", frontmatter, flags=re.MULTILINE):
        errors.append(f"{path.relative_to(ROOT)}: missing title in YAML frontmatter")
    if not re.search(r"^description:\s+\S", frontmatter, flags=re.MULTILINE):
        errors.append(
            f"{path.relative_to(ROOT)}: missing description in YAML frontmatter"
        )


def check_admonitions(path: pathlib.Path, lines: list[str], errors: list[str]) -> None:
    for index, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith("!!! "):
            continue
        if ADMONITION_DIRECTIVE_RE.fullmatch(stripped) is None:
            errors.append(
                f"{path.relative_to(ROOT)}:{index + 1}: "
                "malformed or collapsed admonition"
            )
            continue

        following = next(
            (
                candidate
                for candidate in lines[index + 1 :]
                if candidate.strip()
            ),
            None,
        )
        if following is None or not following.startswith("    "):
            errors.append(
                f"{path.relative_to(ROOT)}:{index + 1}: "
                "admonition body must be indented"
            )


def main() -> None:
    errors: list[str] = []

    for path in public_markdown_files():
        lines = path.read_text(encoding="utf-8").splitlines()
        check_frontmatter(path, lines, errors)
        check_admonitions(path, lines, errors)

    if errors:
        print("docs markdown source check failed:", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        raise SystemExit(1)

    print("docs markdown source checks passed")


if __name__ == "__main__":
    main()
