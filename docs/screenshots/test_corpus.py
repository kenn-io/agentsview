import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("corpus.py")


class CorpusTest(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.source = self.root / "intake"
        self.source.mkdir()
        self.original = (
            '{"type":"user","sessionId":"private-session",'
            '"message":{"content":"Read /work/private-project/file.txt\\n'
            'and say \\"hello\\""},"uuid":"message-a"}\n'
            '{"type":"assistant","sessionId":"private-session",'
            '"parentUuid":"message-a","message":{"content":['
            '{"type":"tool_use","id":"tool-a","name":"Read",'
            '"input":{"file_path":"/work/private-project/file.txt"}}]}}\n'
        )
        (self.source / "conversation.jsonl").write_text(self.original)
        self.recipe = {
            "schema": 1,
            "generation": "example-1",
            "generator_revision": "a" * 40,
            "reference_now": "2026-10-20T12:00:00Z",
            "timezone": "America/Chicago",
            "source_roots": {"claude": ["claude/projects"]},
            "targets": {
                "rich": {"session_id": "session-a"},
                "thinking": {"session_id": "session-a"},
                "code_block": {"session_id": "session-a"},
                "tool_output": {"session_id": "session-a", "message_ordinal": 1},
            },
            "replacements": {
                "private-session": "session-a",
                "/work/private-project": "/workspace/project-a",
            },
            "blocked_terms": ["private-project", "hidden-value"],
            "files": [{
                "source": "conversation.jsonl",
                "path": "claude/projects/project-a/session-a.jsonl",
                "format": "jsonl",
                "sha256": hashlib.sha256(self.original.encode()).hexdigest(),
            }],
        }

    def run_cli(self, *args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *map(str, args)],
            capture_output=True, text=True,
        )

    def generate(self, name="generation"):
        recipe = self.root / "recipe.json"
        recipe.write_text(json.dumps(self.recipe))
        return self.run_cli("generate", "--recipe", recipe,
                            "--source", self.source, "--output", self.root / name)

    def test_generation_preserves_references_and_is_reproducible(self):
        first = self.generate()
        self.assertEqual(first.returncode, 0, first.stderr)
        second = self.generate("again")
        self.assertEqual(second.returncode, 0, second.stderr)
        destination = self.root / "generation"
        transcript = destination / self.recipe["files"][0]["path"]
        records = [json.loads(line) for line in transcript.read_text().splitlines()]
        self.assertEqual(records[0]["sessionId"], "session-a")
        self.assertEqual(records[0]["message"]["content"],
                         'Read /workspace/project-a/file.txt\nand say "hello"')
        self.assertEqual(records[1]["parentUuid"], "message-a")
        self.assertEqual(records[1]["message"]["content"][0]["input"],
                         {"file_path": "/workspace/project-a/file.txt"})
        for file in destination.rglob("*"):
            if file.is_file():
                self.assertEqual(file.read_bytes(),
                                 (self.root / "again" / file.relative_to(destination)).read_bytes())
        self.assertEqual(self.run_cli("verify", destination / "manifest.json").returncode, 0)
        manifest = json.loads((destination / "manifest.json").read_text())
        self.assertEqual(manifest["reference_now"], "2026-10-20T12:00:00Z")
        self.assertNotIn("private-project", (destination / "manifest.json").read_text())

    def test_changed_intake_is_rejected_before_output(self):
        (self.source / "conversation.jsonl").write_text(self.original + "\n")
        result = self.generate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("input checksum", result.stderr)
        self.assertFalse((self.root / "generation").exists())

    def test_remaining_private_text_is_rejected(self):
        self.recipe["blocked_terms"].append("Read")
        result = self.generate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("blocked text", result.stderr)
        self.assertNotIn("private-project", result.stderr)
        self.assertFalse((self.root / "generation").exists())

    def test_opaque_content_requires_explicit_omission(self):
        self.original = '{"type":"reasoning","encrypted_content":"opaque-value","summary":[{"text":"Useful reasoning"}]}\n'
        (self.source / "conversation.jsonl").write_text(self.original)
        self.recipe["files"][0]["sha256"] = hashlib.sha256(self.original.encode()).hexdigest()
        result = self.generate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("opaque content", result.stderr)
        self.recipe["omit_fields"] = ["encrypted_content"]
        result = self.generate()
        self.assertEqual(result.returncode, 0, result.stderr)
        sanitized = json.loads((self.root / "generation" / self.recipe["files"][0]["path"]).read_text())
        self.assertEqual(sanitized, {"type": "reasoning", "summary": [{"text": "Useful reasoning"}]})

    def test_existing_generation_is_never_replaced(self):
        output = self.root / "generation"
        output.mkdir()
        (output / "keep.txt").write_text("previous result")
        result = self.generate()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((output / "keep.txt").read_text(), "previous result")

    def test_verifier_rejects_changed_and_unlisted_files(self):
        self.assertEqual(self.generate().returncode, 0)
        output = self.root / "generation"
        (output / "extra.txt").write_text("unreviewed")
        result = self.run_cli("verify", output / "manifest.json")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("inventory", result.stderr)
        (output / "extra.txt").unlink()
        (output / self.recipe["files"][0]["path"]).write_text("{}\n")
        result = self.run_cli("verify", output / "manifest.json")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum", result.stderr)

    def test_invalid_recipe_inputs_are_rejected(self):
        for field, value in [("path", "../outside.jsonl"),
                             ("source", "/etc/passwd"),
                             ("format", "sqlite")]:
            with self.subTest(field=field):
                old = self.recipe["files"][0][field]
                self.recipe["files"][0][field] = value
                result = self.generate()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "generation").exists())
                self.recipe["files"][0][field] = old
        self.recipe["reference_now"] = "today"
        result = self.generate()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("reference_now", result.stderr)


if __name__ == "__main__":
    unittest.main()
