#!/usr/bin/env python3
"""Create and verify immutable native transcript generations."""

import argparse
from datetime import datetime
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import sys
import tempfile
from zoneinfo import ZoneInfo


def canonical(value):
    return (json.dumps(value, ensure_ascii=False, sort_keys=True,
                       separators=(",", ":"), allow_nan=False) + "\n").encode()


def digest(data):
    return hashlib.sha256(data).hexdigest()


def object_pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def decode(data):
    return json.loads(data, object_pairs_hook=object_pairs)


def relative_path(value):
    if not isinstance(value, str):
        raise ValueError("file paths must be strings")
    path = PurePosixPath(value)
    if (not value or path.is_absolute() or ".." in path.parts
            or "\\" in value or str(path) != value or value == "."):
        raise ValueError("expected a canonical relative path")
    return path


def read_file(root, name):
    path = relative_path(name)
    current = root
    for part in path.parts:
        current = current / part
        if current.is_symlink():
            raise ValueError("corpus inputs must not be symlinks")
    return current.read_bytes()


def metadata(value):
    if value.get("schema") != 1:
        raise ValueError("unsupported corpus schema")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", value.get("generation", "")):
        raise ValueError("invalid generation name")
    if not re.fullmatch(r"[0-9a-f]{40}", value.get("generator_revision", "")):
        raise ValueError("generator_revision must be a full Git commit")
    now = value.get("reference_now", "")
    try:
        if not now.endswith("Z") or "T" not in now:
            raise ValueError()
        datetime.fromisoformat(now)
    except (TypeError, ValueError):
        raise ValueError("reference_now must be an explicit UTC timestamp") from None
    ZoneInfo(value["timezone"])
    roots = value.get("source_roots", {})
    if not roots or set(roots) - {"claude", "codex"}:
        raise ValueError("source_roots must select Claude or Codex roots")
    for paths in roots.values():
        if not isinstance(paths, list) or not paths:
            raise ValueError("each provider needs at least one source root")
        for path in paths:
            relative_path(path)
    targets = value.get("targets", {})
    for name in ("rich", "thinking", "code_block", "tool_output"):
        target = targets.get(name, {})
        if not isinstance(target.get("session_id"), str) or not target["session_id"]:
            raise ValueError(f"missing session target: {name}")
    ordinal = targets["tool_output"].get("message_ordinal")
    if type(ordinal) is not int or ordinal < 0:
        raise ValueError("tool_output needs a nonnegative message_ordinal")


def verify(manifest_path):
    manifest_path = Path(manifest_path)
    if manifest_path.name != "manifest.json" or manifest_path.is_symlink():
        raise ValueError("expected the generation's manifest.json")
    raw = manifest_path.read_bytes()
    manifest = decode(raw)
    metadata(manifest)
    if raw != canonical(manifest):
        raise ValueError("manifest is not canonical")
    root = manifest_path.parent
    expected = {"manifest.json"}
    files = manifest.get("files", [])
    if not files:
        raise ValueError("corpus has no files")
    for file in files:
        path = str(relative_path(file["path"]))
        if path in expected:
            raise ValueError("duplicate corpus path")
        expected.add(path)
        if digest(read_file(root, path)) != file["sha256"]:
            raise ValueError(f"corpus checksum mismatch: {path}")
    actual = set()
    for path in root.rglob("*"):
        if path.is_symlink():
            raise ValueError("corpus inventory contains a symlink")
        if path.is_file():
            actual.add(path.relative_to(root).as_posix())
    if actual != expected:
        raise ValueError("corpus inventory differs from manifest")
    for paths in manifest["source_roots"].values():
        for path in paths:
            if not (root / path).is_dir() or not any(
                    file.startswith(path + "/") for file in expected):
                raise ValueError("source root contains no corpus files")
    return manifest


def generate(recipe_path, source_root, output):
    recipe_bytes = Path(recipe_path).read_bytes()
    recipe = decode(recipe_bytes)
    metadata(recipe)
    output = Path(output)
    if output.exists() or output.is_symlink():
        raise ValueError("output generation already exists")
    replacements = recipe["replacements"]
    if not isinstance(replacements, dict) or any(
            not key or not isinstance(value, str) for key, value in replacements.items()):
        raise ValueError("replacements must map nonempty text to text")
    terms = recipe["blocked_terms"]
    if not isinstance(terms, list) or any(
            not isinstance(term, str) or not term.strip() for term in terms):
        raise ValueError("blocked_terms must be an explicit list of nonempty text")
    blocked = [term.casefold() for term in [*terms, *replacements]]
    omitted = recipe.get("omit_fields", [])
    if not isinstance(omitted, list) or any(
            not isinstance(key, str) or not key for key in omitted):
        raise ValueError("omit_fields must list explicit JSON field names")
    pattern = re.compile("|".join(re.escape(key) for key in
                                 sorted(replacements, key=lambda key: (-len(key), key))))

    def scrub(value):
        if isinstance(value, str):
            changed = pattern.sub(lambda match: replacements[match[0]], value) if replacements else value
            if any(term in changed.casefold() for term in blocked):
                raise ValueError("blocked text remains after sanitization")
            return changed
        if isinstance(value, list):
            return [scrub(item) for item in value]
        if isinstance(value, dict):
            if value.get("encrypted_content") and "encrypted_content" not in omitted:
                raise ValueError("opaque content requires explicit omission")
            pairs = [(scrub(key), scrub(item)) for key, item in value.items()
                     if key not in omitted]
            return object_pairs(pairs)
        return value

    manifest = {key: recipe[key] for key in (
        "schema", "generation", "generator_revision", "reference_now",
        "timezone", "source_roots", "targets",
    )}
    manifest["recipe_sha256"] = digest(recipe_bytes)
    manifest["generator_sha256"] = digest(Path(__file__).read_bytes())
    manifest["fixtures"] = scrub(recipe.get("fixtures", {}))
    manifest["files"] = []
    selected = recipe["files"]
    if not isinstance(selected, list) or not selected:
        raise ValueError("recipe must select native files")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".corpus-", dir=output.parent) as temp:
        staging = Path(temp)
        seen = {"manifest.json"}
        for file in sorted(selected, key=lambda item: item["path"]):
            name = str(relative_path(file["path"]))
            if name in seen:
                raise ValueError("duplicate corpus path")
            seen.add(name)
            scrub(name)
            data = read_file(Path(source_root), file["source"])
            if digest(data) != file["sha256"]:
                raise ValueError("input checksum differs from frozen selection")
            if file["format"] == "jsonl":
                data = b"".join(canonical(scrub(decode(line)))
                                for line in data.splitlines() if line.strip())
            elif file["format"] == "json":
                data = canonical(scrub(decode(data)))
            elif file["format"] == "text":
                data = scrub(data.decode("utf-8")).encode()
            else:
                raise ValueError("file format must be json, jsonl, or text")
            path = staging / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            manifest["files"].append({"path": name, "sha256": digest(data)})
        # Metadata is already curated; reject any field that still needs redaction.
        if scrub(manifest) != manifest:
            raise ValueError("manifest metadata must already be sanitized")
        (staging / "manifest.json").write_bytes(canonical(manifest))
        verify(staging / "manifest.json")
        output.mkdir()  # Exclusive creation preserves existing generations.
        try:
            for path in sorted(staging.iterdir(), key=lambda path: path.name == "manifest.json"):
                shutil.move(str(path), output / path.name)
        except BaseException:
            shutil.rmtree(output)
            raise
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    create = sub.add_parser("generate", help="Sanitize a frozen native-file selection")
    create.add_argument("--recipe", required=True, type=Path)
    create.add_argument("--source", required=True, type=Path)
    create.add_argument("--output", required=True, type=Path)
    check = sub.add_parser("verify", help="Verify an approved generation")
    check.add_argument("manifest", type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.command == "generate":
            manifest = generate(args.recipe, args.source, args.output)
        else:
            manifest = verify(args.manifest)
        print(digest(canonical(manifest)))
    except (OSError, ValueError, KeyError, TypeError) as error:
        # JSON parser errors and OS errors can include private input text/paths.
        if isinstance(error, json.JSONDecodeError):
            message = "invalid JSON in corpus input"
        elif isinstance(error, OSError):
            message = "cannot read or write corpus files"
        elif isinstance(error, (KeyError, TypeError)):
            message = "invalid or missing corpus field"
        else:
            message = str(error)
        print(f"Error: {message}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
