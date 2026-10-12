"""Exercise patch drift and atomicity against real Git, without network access."""

import difflib
import hashlib
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from adapters.jeebus.prepare import spine_archive
from scripts.overlays import apply_overlay


class OverlayTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.target = self.root / "generated"
        self.target.mkdir()
        self.patch = self.root / "overlay.patch"

    def recipe(self, changes):
        preimages = {}
        diffs = []
        for name, before, after in changes:
            source = self.target / name
            if before is not None:
                source.parent.mkdir(parents=True, exist_ok=True)
                source.write_text(before)
            preimages[name] = (
                hashlib.sha256(before.encode()).hexdigest() if before is not None else None
            )
            diffs.extend(
                difflib.unified_diff(
                    (before or "").splitlines(True),
                    after.splitlines(True),
                    fromfile=f"a/{name}" if before is not None else "/dev/null",
                    tofile=f"b/{name}",
                )
            )
        self.patch.write_text("".join(diffs))
        self.patch.with_suffix(".json").write_text(json.dumps(preimages))

    def test_apply_inside_parent_repository_and_add_file(self):
        subprocess.run(["git", "init", "--quiet", str(self.root)], check=True)
        self.recipe([("source", "before\n", "after\n"), ("new/hook.go", None, "package api\n")])
        apply_overlay(self.target, self.patch)
        self.assertEqual((self.target / "source").read_text(), "after\n")
        self.assertEqual((self.target / "new/hook.go").read_text(), "package api\n")
        with self.assertRaisesRegex(RuntimeError, "Patch context mismatch"):
            apply_overlay(self.target, self.patch)

    def test_missing_extra_or_changed_context_leaves_all_files_untouched(self):
        for drift in ("", "needle\nneedle\n", "upstream changed\n"):
            with self.subTest(drift=drift):
                self.recipe(
                    [("first", "original\n", "modified\n"), ("second", "needle\n", "hook\n")]
                )
                (self.target / "second").write_text(drift)
                with self.assertRaisesRegex(RuntimeError, "Patch context mismatch"):
                    apply_overlay(self.target, self.patch)
                self.assertEqual((self.target / "first").read_text(), "original\n")
                self.assertEqual((self.target / "second").read_text(), drift)

    def test_invalid_later_hunk_does_not_apply_earlier_hunk(self):
        self.recipe([("first", "one\n", "two\n"), ("second", "three\n", "four\n")])
        self.patch.write_text(self.patch.read_text().replace("-three", "-missing"))
        with self.assertRaisesRegex(RuntimeError, "Cannot apply"):
            apply_overlay(self.target, self.patch)
        self.assertEqual((self.target / "first").read_text(), "one\n")
        self.assertEqual((self.target / "second").read_text(), "three\n")

    def test_new_file_cannot_overwrite_existing_file(self):
        self.recipe([("new", None, "hook\n")])
        (self.target / "new").write_text("existing\n")
        with self.assertRaisesRegex(RuntimeError, "Patch context mismatch"):
            apply_overlay(self.target, self.patch)
        self.assertEqual((self.target / "new").read_text(), "existing\n")

    def test_every_patch_path_needs_a_preimage_entry(self):
        self.recipe([("first", "before\n", "after\n"), ("new", None, "hook\n")])
        manifest = json.loads(self.patch.with_suffix(".json").read_text())
        del manifest["new"]
        self.patch.with_suffix(".json").write_text(json.dumps(manifest))
        with self.assertRaisesRegex(RuntimeError, "manifest does not match"):
            apply_overlay(self.target, self.patch)
        self.assertEqual((self.target / "first").read_text(), "before\n")
        self.assertFalse((self.target / "new").exists())

    def test_symlink_cannot_escape_generated_tree(self):
        self.recipe([("source", "before\n", "after\n")])
        outside = self.root / "outside"
        outside.write_text("before\n")
        (self.target / "source").unlink()
        (self.target / "source").symlink_to(outside)
        with self.assertRaisesRegex(RuntimeError, "escapes build tree"):
            apply_overlay(self.target, self.patch)
        self.assertEqual(outside.read_text(), "before\n")

    def test_verified_java_cache_needs_no_network(self):
        blob = b"verified source jar"
        spec = {"version": "test", "sha256": hashlib.sha256(blob).hexdigest()}
        (self.root / (spec["sha256"] + ".jar")).write_bytes(blob)
        with patch("urllib.request.urlopen", side_effect=AssertionError("unexpected download")):
            self.assertEqual(spine_archive(self.root, spec), blob)

    def test_invalid_java_download_is_not_cached(self):
        spec = {"version": "test", "sha256": hashlib.sha256(b"expected").hexdigest()}
        with patch("urllib.request.urlopen") as download:
            download.return_value.__enter__.return_value.read.return_value = b"wrong"
            with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
                spine_archive(self.root, spec)
        self.assertFalse((self.root / (spec["sha256"] + ".jar")).exists())


if __name__ == "__main__":
    unittest.main()
