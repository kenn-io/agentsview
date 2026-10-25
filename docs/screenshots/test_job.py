import copy
import unittest

from job import compare


class ComparisonTest(unittest.TestCase):
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
