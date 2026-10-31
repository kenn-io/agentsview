import copy
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

from job import ROOT, compare, preview_source


class ComparisonTest(unittest.TestCase):
    @unittest.skipUnless(shutil.which("bash") and shutil.which("git"), "requires Bash and Git")
    def test_preview_uses_capture_inventory_with_older_preparation(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            old = root / "old"
            for name in ("screenshots/update-generated-assets-branch.sh", "assets/hydrate-assets.sh"):
                target = old / "docs" / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT.parent / name, target)
            (old / "docs/screenshots/expected-assets.txt").write_text("screenshots/retired.png\n")
            prepared = root / "prepared"
            (prepared / "bundle").mkdir(parents=True)
            archive_path = prepared / "bundle/docs-source.tar"
            with tarfile.open(archive_path, "w") as archive:
                archive.add(old / "docs", arcname="docs")
            sealed = hashlib.sha256(archive_path.read_bytes()).hexdigest()
            expected = (ROOT / "expected-assets.txt").read_text().splitlines()
            assets = root / "assets"
            for name in expected:
                path = assets / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"reviewed image")
            environment = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
            environment.update({
                "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                "GIT_AUTHOR_NAME": "Capture Fixture", "GIT_AUTHOR_EMAIL": "docs@example.invalid",
                "GIT_COMMITTER_NAME": "Capture Fixture", "GIT_COMMITTER_EMAIL": "docs@example.invalid",
                "GIT_AUTHOR_DATE": "2024-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2024-01-01T00:00:00Z",
                "AGENTSVIEW_DOCS_USE_LOCAL_ASSET_BRANCHES": "1",
            })

            def run(directory, *command, check=True):
                result = subprocess.run(command, cwd=directory, env=environment, capture_output=True, text=True)
                if check:
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                return result

            rejected = run(old, "bash", "docs/screenshots/update-generated-assets-branch.sh",
                           "--source", str(assets), check=False)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertIn("retired.png", rejected.stderr)

            source = root / "preview"
            preview_source(prepared, source)
            run(source, "git", "init", "--quiet", "--template=")
            static = root / "static"
            static.mkdir()
            for name in ("architecture.svg", "og-image.png"):
                (static / name).write_bytes(b"static asset")
            run(static, "git", "init", "--quiet", "--template=")
            run(static, "git", "add", ".")
            run(static, "git", "commit", "-m", "fixture")
            run(source, "git", "fetch", str(static), "HEAD:refs/heads/docs-assets")
            run(source, "bash", "docs/screenshots/update-generated-assets-branch.sh", "--source", str(assets))
            published = run(source, "git", "ls-tree", "-r", "--name-only", "docs-generated-assets")
            self.assertEqual(set(published.stdout.splitlines()), set(expected))
            run(source, "bash", "docs/assets/hydrate-assets.sh")
            hydrated = source / "docs/assets/generated"
            self.assertEqual({p.relative_to(hydrated).as_posix() for p in hydrated.rglob("*") if p.is_file()}, set(expected))
            for name in expected:
                self.assertEqual((hydrated / name).read_bytes(), b"reviewed image")
            self.assertEqual(hashlib.sha256(archive_path.read_bytes()).hexdigest(), sealed)

    def test_changed_images_or_build_reject_the_job(self):
        first = {
            "build": {"application_sha256": "a" * 64},
            "images": {"screenshots/dashboard.png": "b" * 64},
            "runtime": {"browser": "Chromium 148"},
            "capture_files": {"entrypoint.sh": "c" * 64},
            "preview": {"generated_asset_commit": "d" * 40},
        }
        compare(first, copy.deepcopy(first))
        for field in ("build", "images", "runtime", "capture_files", "preview"):
            with self.subTest(field=field):
                second = copy.deepcopy(first)
                key = next(iter(second[field]))
                second[field][key] = "different"
                with self.assertRaises(ValueError):
                    compare(first, second)


if __name__ == "__main__":
    unittest.main()
