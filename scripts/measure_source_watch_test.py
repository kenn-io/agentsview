"""Run the measurement CLI against controlled filesystem inputs."""
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path

SCRIPT = Path(__file__).with_name("measure-source-watch.py")


class MeasurementTest(unittest.TestCase):
    def run_report(self, *args):
        result = subprocess.run(
            [sys.executable, str(SCRIPT), "--synthetic-files", "3", "--rows",
             "513", "--passes", "2", *map(str, args)],
            check=True, capture_output=True, text=True,
        )
        return json.loads(result.stdout), result.stdout

    def test_metadata_aggregates_and_cache_roundtrips(self):
        with tempfile.TemporaryDirectory() as scratch:
            base = Path(scratch).resolve()
            source = base / "private-project"
            source.mkdir()
            (source / "private-session.jsonl").write_text("PRIVATE_TRANSCRIPT_SENTINEL")
            (source / "ignored.bin").touch()
            (source / "nested").mkdir()
            (source / "nested" / "another.json").touch()
            report, encoded = self.run_report("--root", source, "--scratch-parent", base)
            for observation in (report["live"][0]["first_pass"],
                                report["live"][0]["last_pass"]):
                self.assertEqual(observation["candidates"], 2)
                self.assertEqual(observation["directories"], 2)
                self.assertEqual(observation["symlinks"], 0)
                self.assertEqual(observation["errors"], 0)
                self.assertFalse(observation["truncated"])
            for private in (str(base), "private-project", "private-session",
                            "PRIVATE_TRANSCRIPT_SENTINEL"):
                self.assertNotIn(private, encoded)
            layouts = report["cache_comparison"]
            self.assertEqual({r["layout"] for r in layouts},
                             {"full_paths", "interned_dirs", "prefix_pages"})
            self.assertTrue(all(r["roundtrip_files"] == 513 for r in layouts))
            self.assertEqual(list(base.iterdir()), [source])
            self.assertEqual((source / "private-session.jsonl").read_text(),
                             "PRIVATE_TRANSCRIPT_SENTINEL")

    def test_entry_limit(self):
        with tempfile.TemporaryDirectory() as scratch:
            base = Path(scratch).resolve()
            source = base / "source"
            source.mkdir()
            for i in range(4):
                (source / f"{i}.jsonl").touch()
            report, _ = self.run_report("--root", source, "--max-entries", "2")
            self.assertTrue(report["live"][0]["last_pass"]["truncated"])

    def test_directory_link_is_not_followed(self):
        with tempfile.TemporaryDirectory() as scratch:
            base = Path(scratch).resolve()
            source = base / "source"
            source.mkdir()
            (source / "private.jsonl").touch()
            link = base / "link"
            if sys.platform == "win32":
                # NTFS junction creation does not require symlink privileges.
                subprocess.run(["cmd", "/c", "mklink", "/J", str(link), str(source)],
                               check=True, capture_output=True)
            else:
                link.symlink_to(source, target_is_directory=True)
            report, _ = self.run_report("--root", link)
            self.assertEqual(report["live"][0]["first_pass"]["candidates"], 0)
            self.assertTrue(report["live"][0]["first_pass"]["skipped_root"])
            report, _ = self.run_report("--root", base)
            self.assertEqual(report["live"][0]["first_pass"]["candidates"], 1)
            self.assertEqual(report["live"][0]["first_pass"]["symlinks"], 1)

    def test_absent_load_average_is_reported_as_unavailable(self):
        spec = importlib.util.spec_from_file_location("measure_source_watch", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with mock.patch.object(os, "getloadavg", None, create=True):
            self.assertIsNone(module.load_average())


if __name__ == "__main__":
    unittest.main()
