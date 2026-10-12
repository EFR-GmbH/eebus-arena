import unittest

from scripts.tests.test_bootstrap import ROOT, module


class ReviewRegressions(unittest.TestCase):
    def test_import_rejects_unsafe_identity_and_mutable_image(self):
        arena = module("review_bootstrap", ROOT / "scripts/arena.py")
        good = {
            "id": "enbility-test",
            "image": "sha256:" + "a" * 64,
            "stack": "enbility",
            "roles": ["EG", "CS"],
            "provenance": {"stack": "enbility"},
        }
        arena.validate_import_variants([good])
        for key, value in [
            ("id", "../escape"),
            ("id", "--option"),
            ("image", "enbility:latest"),
            ("stack", "../../escape"),
            ("roles", ["CS"]),
            ("provenance", {}),
        ]:
            with self.subTest(key=key, value=value), self.assertRaises(RuntimeError):
                arena.validate_import_variants([{**good, key: value}])


if __name__ == "__main__":
    unittest.main()
