#!/usr/bin/env python3
"""Run a pinned screenshot runtime against explicit, verified inputs."""

import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

from corpus import canonical, digest, verify
from container import run as run_container


ROOT = Path(__file__).resolve().parent


def copy_corpus(source, target, reference_now):
    shutil.copytree(source, target, copy_function=shutil.copyfile)
    timestamp = int(datetime.fromisoformat(reference_now).timestamp() * 1_000_000_000)
    # Source selection uses mtime when duplicate transcripts have equal sizes.
    for path in [*target.rglob("*"), target]:
        os.utime(path, ns=(timestamp, timestamp))


def image_hashes(root, expected):
    actual = {path.relative_to(root).as_posix() for path in root.rglob("*") if path.is_file()}
    if actual != set(expected):
        raise ValueError("screenshot inventory mismatch; missing=" +
                         ",".join(sorted(set(expected) - actual)) + "; extra=" +
                         ",".join(sorted(actual - set(expected))))
    hashes = {}
    for name in sorted(expected):
        path = root / name
        data = path.read_bytes()
        if path.is_symlink() or not data.startswith(b"\x89PNG\r\n\x1a\n"):
            raise ValueError(f"invalid PNG output: {name}")
        hashes[name] = digest(data)
    return hashes


def verify_application(directory):
    build = json.loads((directory / "build.json").read_bytes())
    if digest((directory / "agentsview").read_bytes()) != build["application_sha256"]:
        raise ValueError("application checksum differs from build receipt")
    return build


def run(args):
    manifest_path = args.corpus.resolve()
    manifest = verify(manifest_path)
    if digest(manifest_path.read_bytes()) != args.corpus_sha256:
        raise ValueError("corpus manifest checksum differs from requested generation")
    if not re.fullmatch(r"(?:[^\s]+@)?sha256:[0-9a-f]{64}", args.image):
        raise ValueError("CAPTURE_IMAGE must be an immutable sha256 image reference")
    application = args.application.resolve()
    build = verify_application(application)
    if build["runtime_image"] != args.image:
        raise ValueError("runtime image differs from build receipt")
    output = args.output.resolve()
    if output.exists():
        raise ValueError("CAPTURE_OUTPUT must be a new directory")
    output.mkdir(parents=True)
    corpus_copy = output / "corpus"
    copy_corpus(manifest_path.parent, corpus_copy, manifest["reference_now"])
    assets = output / "assets"
    (assets / "screenshots").mkdir(parents=True)
    cidfile = output / "container.id"
    command = ["docker", "run", "--rm", "--network=none", "--platform=linux/amd64",
               "--hostname=docs-capture", "--shm-size=1g"]
    for source, target, readonly in [
        (corpus_copy, "/corpus", True), (application, "/application", True),
        (ROOT, "/capture", True), (assets / "screenshots", "/output", False),
        (output, "/result", False),
    ]:
        if "," in str(source):
            raise ValueError("capture paths cannot contain commas")
        command += ["--mount", f"type=bind,source={source},target={target}" +
                    (",readonly" if readonly else "")]
    environment = {
        "SCREENSHOT_NOW": manifest["reference_now"],
        "SCREENSHOT_TIMEZONE": manifest["timezone"],
        "SCREENSHOT_MANIFEST": "/corpus/manifest.json",
        "SCREENSHOT_DIR": "/output",
        "HOME": "/state/home",
        "CAPTURE_UID": str(os.getuid()), "CAPTURE_GID": str(os.getgid()),
    }
    for name, value in environment.items():
        command += ["-e", f"{name}={value}"]
    command += ["--entrypoint", "/bin/bash", args.image, "/capture/entrypoint.sh"]
    run_container(command, cidfile, output / "capture.log")
    expected = (ROOT / "expected-assets.txt").read_text().splitlines()
    hashes = image_hashes(assets, expected)
    receipt = {
        "schema": 1, "corpus_sha256": args.corpus_sha256,
        "reference_now": manifest["reference_now"], "timezone": manifest["timezone"],
        "platform": "linux/amd64", "runtime_image": args.image, "build": build,
        "runtime": json.loads((output / "runtime.json").read_text()),
        "capture_files": {path.relative_to(ROOT).as_posix(): digest(path.read_bytes())
                          for path in sorted(ROOT.rglob("*")) if path.is_file()
                          and path.suffix in (".py", ".sh", ".ts", ".txt", ".json")
                          and "node_modules" not in path.parts},
        "images": hashes,
    }
    (output / "receipt.json").write_bytes(canonical(receipt))
    print(output / "receipt.json")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option, variable, kind in [
        ("corpus", "CORPUS_MANIFEST", Path), ("corpus-sha256", "CORPUS_SHA256", str),
        ("image", "CAPTURE_IMAGE", str), ("application", "APPLICATION_DIR", Path),
        ("output", "CAPTURE_OUTPUT", Path),
    ]:
        parser.add_argument("--" + option, default=os.environ.get(variable), type=kind,
                            help=variable)
    args = parser.parse_args()
    for option, variable in [("corpus", "CORPUS_MANIFEST"), ("corpus_sha256", "CORPUS_SHA256"),
                             ("image", "CAPTURE_IMAGE"), ("application", "APPLICATION_DIR"),
                             ("output", "CAPTURE_OUTPUT")]:
        if not getattr(args, option):
            parser.error(f"{variable} or --{option.replace('_', '-')} is required")
    os.umask(0o077)
    try:
        run(args)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"Capture failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
