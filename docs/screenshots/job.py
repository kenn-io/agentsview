#!/usr/bin/env python3
"""Build, capture, and preview twice; retain a receipt only when outputs agree."""

import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile

from capture import run as capture
from container import run as run_container
from corpus import canonical, digest, verify
from prepare import build, capture_source, inventory, immutable_image

ROOT = Path(__file__).resolve().parent


def preview_source(prepared, source):
    source.mkdir()
    with tarfile.open(prepared / "bundle/docs-source.tar") as archive:
        archive.extractall(source, filter="data")
    # Publication and hydration belong to the capture revision. The retained
    # application supplies the docs content and dependencies, which may be older.
    for name in ("screenshots/expected-assets.txt",
                 "screenshots/update-generated-assets-branch.sh",
                 "assets/hydrate-assets.sh"):
        target = source / "docs" / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT.parent / name, target)


def preview(prepared, assets, output, reference_now):
    manifest = json.loads((prepared / "prepared.json").read_bytes())
    output.mkdir()
    source = output / "source"
    preview_source(prepared, source)
    command = ["docker", "run", "--rm", "--network=none", "--platform=linux/amd64",
               "--user", f"{os.getuid()}:{os.getgid()}", "-e", "HOME=/tmp/preview-home"]
    for origin, target, readonly in [
        (source, "/src", False), (prepared / "bundle", "/bundle", True),
        (assets, "/screenshots", True), (output, "/out", False), (ROOT, "/capture", True),
    ]:
        if "," in str(origin):
            raise ValueError("preview paths cannot contain commas")
        command += ["--mount", f"type=bind,source={origin},target={target}" +
                    (",readonly" if readonly else "")]
    environment = {
        "REFERENCE_NOW": reference_now,
        "STATIC_ASSETS_REVISION": manifest["static_assets_revision"],
        "SOURCE_DATE_EPOCH": str(int(datetime.fromisoformat(reference_now).timestamp())),
    }
    for key, value in environment.items():
        command += ["-e", f"{key}={value}"]
    command += ["--entrypoint", "/bin/bash", immutable_image(manifest["builder_image"]),
                "/capture/preview.sh"]
    run_container(command, output / "container.id", output / "preview.log")
    return {
        "generated_asset_commit": (output / "generated-asset-commit.txt").read_text().strip(),
        "docs_revision": manifest["docs_revision"],
        "static_assets_revision": manifest["static_assets_revision"],
        "site": inventory(output / "site"),
    }


def compare(first, second):
    for field in ("build", "images", "runtime", "capture_files"):
        if first[field] != second[field]:
            raise ValueError(f"independent captures differ: {field}")
    if first["preview"]["generated_asset_commit"] != second["preview"]["generated_asset_commit"]:
        raise ValueError("independent generated asset commits differ")


def run(args):
    corpus = args.corpus.resolve()
    manifest = verify(corpus)
    if digest(corpus.read_bytes()) != args.corpus_sha256:
        raise ValueError("corpus checksum differs from request")
    prepared = args.prepared.resolve()
    prepared_bytes = (prepared / "prepared.json").read_bytes()
    if digest(prepared_bytes) != args.prepared_sha256:
        raise ValueError("preparation checksum differs from sealed execution inputs")
    inputs = json.loads(prepared_bytes)
    if capture_source() != args.capture_revision:
        raise ValueError("capture source differs from sealed execution inputs")
    # Validate all retained inputs before allocating an attempt.
    if inventory(prepared / "bundle") != inputs["files"]:
        raise ValueError("retained dependencies differ from sealed inputs")
    output = args.output.resolve()
    output.mkdir(parents=True)
    receipts = []
    for number in (1, 2):
        attempt = output / f"run-{number}"
        attempt.mkdir()
        build(prepared, attempt / "build")
        capture(argparse.Namespace(corpus=corpus, corpus_sha256=args.corpus_sha256,
                                   image=inputs["runtime_image"],
                                   application=attempt / "build/application",
                                   output=attempt / "capture"))
        receipt = json.loads((attempt / "capture/receipt.json").read_bytes())
        receipt["preview"] = preview(prepared, attempt / "capture/assets",
                                     attempt / "preview", manifest["reference_now"])
        receipts.append(receipt)
    compare(*receipts)
    receipt = {
        "schema": 1, "corpus_sha256": args.corpus_sha256,
        "prepared_sha256": args.prepared_sha256, "runs": receipts,
        "capture_revision": args.capture_revision,
        "publication_assets": "run-1/capture/assets", "preview": "run-1/preview/site",
    }
    (output / "receipt.json").write_bytes(canonical(receipt))
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option, variable, kind in [
        ("corpus", "CORPUS_MANIFEST", Path), ("corpus-sha256", "CORPUS_SHA256", str),
        ("prepared", "PREPARED_INPUTS", Path), ("prepared-sha256", "PREPARED_SHA256", str),
        ("capture-revision", "CAPTURE_REVISION", str),
        ("output", "CAPTURE_OUTPUT", Path),
    ]:
        parser.add_argument("--" + option, type=kind, default=os.environ.get(variable), help=variable)
    args = parser.parse_args()
    for option, variable in [("corpus", "CORPUS_MANIFEST"), ("corpus_sha256", "CORPUS_SHA256"),
                             ("prepared", "PREPARED_INPUTS"), ("prepared_sha256", "PREPARED_SHA256"),
                             ("capture_revision", "CAPTURE_REVISION"),
                             ("output", "CAPTURE_OUTPUT")]:
        if not getattr(args, option):
            parser.error(f"{variable} or --{option.replace('_', '-')} is required")
    os.umask(0o077)
    try:
        print(digest(canonical(run(args))))
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"Capture job failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
