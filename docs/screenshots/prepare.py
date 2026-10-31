#!/usr/bin/env python3
"""Retain build dependencies, then build from them without network access."""

import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tarfile

from corpus import canonical, digest
from container import run as run_container


ROOT = Path(__file__).resolve().parent


def capture_source():
    repository = ROOT.parents[1]
    git = ["git", "-C", str(repository)]
    revision = subprocess.check_output(git + ["rev-parse", "HEAD"]).decode().strip()
    changed = subprocess.check_output(git + ["status", "--porcelain", "--untracked-files=all",
                                              "--", "docs/screenshots", "docs/assets/hydrate-assets.sh"])
    if changed:
        raise ValueError("commit capture tools before sealing execution inputs")
    return revision


def inventory(root):
    files = {}
    for path in sorted(root.rglob("*")):
        name = path.relative_to(root).as_posix()
        if path.is_symlink():
            files[name] = {"link": os.readlink(path)}
        elif path.is_file():
            files[name] = {"sha256": digest(path.read_bytes()),
                           "executable": bool(path.stat().st_mode & 0o111)}
    return files


def extract(bundle, destination):
    destination.mkdir()
    with tarfile.open(bundle / "source.tar") as archive:
        archive.extractall(destination, filter="data")


def immutable_image(value):
    if not re.fullmatch(r"(?:[^\s]+@)?sha256:[0-9a-f]{64}", value):
        raise ValueError("images must use immutable sha256 references")
    return value


def retain_tree(repository, revision, destination):
    """Keep exactly one commit's tree, without history or repository alternates."""
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("source revisions must be full Git commits")
    git = ["git", "-C", str(repository)]
    listing = subprocess.check_output(git + ["ls-tree", "-r", "-t", revision]).decode()
    tree = subprocess.check_output(git + ["rev-parse", revision + "^{tree}"]).decode().strip()
    objects = {revision, tree, *(line.split()[2] for line in listing.splitlines())}
    with destination.open("wb") as stream:
        subprocess.run(git + ["pack-objects", "--stdout"],
                       input=("\n".join(sorted(objects)) + "\n").encode(),
                       stdout=stream, check=True)


def container(image, source, bundle, output, mode, environment, log_path):
    command = ["docker", "run", "--rm", "--platform=linux/amd64",
               "--user", f"{os.getuid()}:{os.getgid()}", "-e", "HOME=/tmp/build-home"]
    if mode == "build":
        command += ["--network=none"]
    for origin, target, readonly in [
        (source, "/src", False), (bundle, "/bundle", mode == "build"),
        (output, "/out", False), (ROOT, "/capture", True),
    ]:
        if "," in str(origin):
            raise ValueError("build paths cannot contain commas")
        command += ["--mount", f"type=bind,source={origin},target={target}" +
                    (",readonly" if readonly else "")]
    for key, value in environment.items():
        command += ["-e", f"{key}={value}"]
    command += ["--entrypoint", "/bin/bash", immutable_image(image), "/capture/build.sh", mode]
    run_container(command, output / "container.id", log_path)


def prepare(args):
    preparation_revision = capture_source()
    immutable_image(args.builder_image)
    immutable_image(args.runtime_image)
    if not re.fullmatch(r"[0-9a-f]{40}", args.revision):
        raise ValueError("application revision must be a full Git commit")
    if not re.fullmatch(r"[A-Za-z0-9._+-]+", args.version):
        raise ValueError("invalid application version")
    if not args.build_date.endswith("Z") or "T" not in args.build_date:
        raise ValueError("build date must be an explicit UTC timestamp")
    datetime.fromisoformat(args.build_date)
    output = args.output.resolve()
    output.mkdir(parents=True)
    bundle = output / "bundle"
    bundle.mkdir()
    with (bundle / "source.tar").open("wb") as stream:
        subprocess.run(["git", "-C", str(args.source), "archive", "--format=tar", args.revision],
                       stdout=stream, check=True)
    docs_revision = args.revision
    if not re.fullmatch(r"[0-9a-f]{40}", docs_revision):
        raise ValueError("documentation revision must be a full Git commit")
    with (bundle / "docs-source.tar").open("wb") as stream:
        subprocess.run(["git", "-C", str(args.source), "archive", "--format=tar", docs_revision],
                       stdout=stream, check=True)
    retain_tree(args.source, args.static_assets_revision, bundle / "static-assets.pack")
    source = output / "preparation-source"
    extract(bundle, source)
    container(args.builder_image, source, bundle, output, "prepare", {}, output / "prepare.log")
    manifest = {
        "schema": 1, "application_revision": args.revision, "version": args.version,
        "build_date": args.build_date, "builder_image": args.builder_image,
        "runtime_image": args.runtime_image, "platform": "linux/amd64",
        "build_script_sha256": digest((ROOT / "build.sh").read_bytes()),
        "docs_revision": docs_revision, "static_assets_revision": args.static_assets_revision,
        "preparation_revision": preparation_revision,
        "files": inventory(bundle),
    }
    (output / "prepared.json").write_bytes(canonical(manifest))
    shutil.rmtree(source)
    return manifest


def build(prepared, output):
    prepared, output = Path(prepared).resolve(), Path(output).resolve()
    manifest_bytes = (prepared / "prepared.json").read_bytes()
    manifest = json.loads(manifest_bytes)
    bundle = prepared / "bundle"
    if inventory(bundle) != manifest["files"]:
        raise ValueError("dependency bundle differs from sealed manifest")
    if digest((ROOT / "build.sh").read_bytes()) != manifest["build_script_sha256"]:
        raise ValueError("build script differs from sealed manifest")
    output.mkdir(parents=True)
    source = output / "source"
    extract(bundle, source)
    application = output / "application"
    application.mkdir()
    container(manifest["builder_image"], source, bundle, application, "build", {
        "AV_VERSION": manifest["version"], "AV_COMMIT": manifest["application_revision"],
        "AV_BUILD_DATE": manifest["build_date"],
        "SOURCE_DATE_EPOCH": str(int(datetime.fromisoformat(manifest["build_date"]).timestamp())),
    }, output / "build.log")
    receipt = {key: manifest[key] for key in (
        "application_revision", "version", "build_date", "builder_image", "runtime_image", "platform",
    )}
    receipt["prepared_sha256"] = digest(manifest_bytes)
    receipt["application_sha256"] = digest((application / "agentsview").read_bytes())
    receipt["frontend"] = inventory(application / "frontend")
    (application / "build.json").write_bytes(canonical(receipt))
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    acquire = commands.add_parser("prepare")
    acquire.add_argument("--source", type=Path, default=os.environ.get("AGENTSVIEW_SRC", ROOT.parents[1]))
    acquire.add_argument("--revision", required=True)
    acquire.add_argument("--static-assets-revision", required=True)
    acquire.add_argument("--version", required=True)
    acquire.add_argument("--build-date", required=True)
    acquire.add_argument("--builder-image", required=True)
    acquire.add_argument("--runtime-image", required=True)
    acquire.add_argument("--output", type=Path, required=True)
    offline = commands.add_parser("build")
    offline.add_argument("--prepared", type=Path, required=True)
    offline.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    try:
        result = prepare(args) if args.command == "prepare" else build(args.prepared, args.output)
        print(digest(canonical(result)))
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"Build failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
