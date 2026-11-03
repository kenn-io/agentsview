from pathlib import Path
import tempfile
import unittest


class PrepareTest(unittest.TestCase):
    def test_dependency_inventory_detects_content_and_link_changes(self):
        import prepare

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "a").write_text("first")
            (root / "link").symlink_to("a")
            first = prepare.inventory(root)
            self.assertEqual(first["link"], {"link": "a"})
            self.assertEqual(first, prepare.inventory(root))
            (root / "a").write_text("second")
            self.assertNotEqual(first, prepare.inventory(root))
            second = prepare.inventory(root)
            (root / "link").unlink()
            (root / "link").symlink_to("b")
            self.assertNotEqual(second, prepare.inventory(root))

    def test_offline_build_rejects_modified_bundle_before_launch(self):
        import prepare
        import corpus

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / "bundle").mkdir()
            (root / "bundle/source.tar").write_bytes(b"changed")
            (root / "prepared.json").write_bytes(corpus.canonical({
                "files": {"source.tar": {"sha256": "0" * 64, "executable": False}},
            }))
            with self.assertRaisesRegex(ValueError, "dependency bundle"):
                prepare.build(root, root / "attempt")
            self.assertFalse((root / "attempt").exists())


if __name__ == "__main__":
    unittest.main()
