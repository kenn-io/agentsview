import json
import os
from pathlib import Path
import subprocess
import sqlite3
import sys
import tempfile
import unittest

import corpus


class CaptureTest(unittest.TestCase):
    def test_corpus_copy_ignores_restored_modification_times(self):
        import capture

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = root / "source"
            source.mkdir()
            paths = [source / "a" / "session.jsonl", source / "b" / "session.jsonl"]
            for path, text in zip(paths, ["first", "other"]):
                path.parent.mkdir()
                path.write_text(text)
            for attempt, times in enumerate([(10, 20), (20, 10)]):
                for path, timestamp in zip(paths, times):
                    os.utime(path, ns=(timestamp * 1_000_000_000,) * 2)
                target = root / str(attempt)
                capture.copy_corpus(source, target, "2024-01-01T00:00:00Z")
                for path, timestamp, text in zip(paths, times, ["first", "other"]):
                    copied = target / path.relative_to(source)
                    self.assertEqual(copied.read_text(), text)
                    self.assertEqual(copied.stat().st_mtime_ns, 1704067200000000000)
                    self.assertEqual(path.stat().st_mtime_ns, timestamp * 1_000_000_000)

    def test_missing_explicit_inputs_fail_without_running_docker(self):
        result = subprocess.run(
            ["bash", str(Path(__file__).with_name("run.sh"))],
            capture_output=True, text=True,
            env={"PATH": "/usr/bin:/bin", "SOURCE_DB": "/nonexistent/personal.db"},
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("CORPUS_MANIFEST", result.stderr)

    def test_image_inventory_rejects_missing_extra_and_invalid_outputs(self):
        import capture

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            expected = ["screenshots/dashboard.png", "screenshots/message.png"]
            (root / "screenshots").mkdir()
            (root / expected[0]).write_bytes(b"\x89PNG\r\n\x1a\nexample-a")
            with self.assertRaisesRegex(ValueError, "inventory"):
                capture.image_hashes(root, expected)
            (root / expected[1]).write_bytes(b"\x89PNG\r\n\x1a\nexample-b")
            hashes = capture.image_hashes(root, expected)
            self.assertEqual(set(hashes), set(expected))
            (root / "screenshots/stale.png").write_bytes(b"old")
            with self.assertRaisesRegex(ValueError, "inventory"):
                capture.image_hashes(root, expected)
            (root / "screenshots/stale.png").unlink()
            (root / expected[1]).write_bytes(b"error instead of a PNG")
            with self.assertRaisesRegex(ValueError, "PNG"):
                capture.image_hashes(root, expected)

    def test_changed_application_is_rejected_before_capture(self):
        import capture

        with tempfile.TemporaryDirectory() as temp:
            application = Path(temp)
            (application / "agentsview").write_bytes(b"changed binary")
            (application / "build.json").write_bytes(corpus.canonical({
                "application_sha256": "0" * 64,
                "application_revision": "a" * 40,
            }))
            with self.assertRaisesRegex(ValueError, "application checksum"):
                capture.verify_application(application)

    def test_config_uses_reported_providers_and_corpus_roots(self):
        import fixture
        import tomllib

        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "config.toml"
            fixture.configure({"source_roots": {"claude": ["claude/projects"]}},
                              {"session_providers": [{"id": "claude"}, {"id": "codex"},
                                                     {"id": "gemini"}]},
                              Path("/corpus"), output)
            actual = tomllib.loads(output.read_text())
            self.assertEqual(actual["agents"]["claude"]["dirs"], ["/corpus/claude/projects"])
            self.assertEqual(actual["agents"]["codex"]["dirs"], [])
            self.assertEqual(actual["agents"]["gemini"]["dirs"], [])
            self.assertEqual(actual["disabled_agents"], ["codex", "gemini"])
            with self.assertRaisesRegex(ValueError, "provider"):
                fixture.configure({"source_roots": {"claude": ["claude/projects"]}},
                                  {"session_providers": []}, Path("/corpus"), output)

    def test_fixture_accepts_canonical_tool_events_and_preserves_transcripts(self):
        import fixture

        with tempfile.TemporaryDirectory() as temp:
            database = Path(temp) / "sessions.db"
            with sqlite3.connect(database) as connection:
                schema = Path(__file__).resolve().parents[2] / "internal/db/schema.sql"
                connection.executescript(schema.read_text())
                connection.execute("""INSERT INTO sessions
                    (id, project, started_at, parent_session_id) VALUES
                    ('session-a', 'project-a', '2026-10-19T10:00:00Z', NULL),
                    ('child-a', 'project-a', '2026-10-19T10:01:00Z', 'session-a')""")
                connection.execute("""INSERT INTO messages
                    (id, session_id, ordinal, role, content, thinking_text)
                    VALUES (1, 'session-a', 0, 'assistant', '```py\nprint(1)\n```', 'Consider the result')""")
                connection.execute("""INSERT INTO tool_calls
                    (id, message_id, session_id, tool_name, category, call_index, result_content)
                    VALUES (1, 1, 'session-a', 'Read', 'read', 0, '')""")
                connection.execute("""INSERT INTO tool_result_events
                    (session_id, tool_call_message_ordinal, call_index, source, status, content)
                    VALUES ('session-a', 0, 0, 'tool_result', 'success', '# Output\nA useful result')""")
            manifest = {
                "reference_now": "2026-10-20T12:00:00Z", "source_roots": {"claude": ["claude"]},
                "targets": {name: {"session_id": "session-a", "message_ordinal": 0}
                            for name in ("rich", "thinking", "code_block", "tool_output")},
                "fixtures": {"starred_session_ids": ["session-a"]},
            }
            fixture.apply(manifest, database)
            with sqlite3.connect(database) as connection:
                self.assertEqual(connection.execute(
                    "SELECT machine, created_at, local_modified_at FROM sessions WHERE id='session-a'"
                ).fetchone(), ("dev-laptop", "2026-10-19T10:00:00Z", "2026-10-20T12:00:00Z"))
                self.assertEqual(connection.execute(
                    "SELECT content FROM tool_result_events"
                ).fetchone()[0], "# Output\nA useful result")
                self.assertEqual(connection.execute(
                    "SELECT session_id, created_at FROM starred_sessions"
                ).fetchone(), ("session-a", "2026-10-20T12:00:00Z"))
            manifest["targets"]["thinking"]["session_id"] = "absent"
            with self.assertRaisesRegex(ValueError, "missing parsed session"):
                fixture.apply(manifest, database)


if __name__ == "__main__":
    unittest.main()
