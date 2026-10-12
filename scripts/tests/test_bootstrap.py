"""Bootstrap invariants without a Docker daemon or downloaded source trees."""

import hashlib
import importlib.util
import io
import json
import pathlib
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = pathlib.Path(__file__).resolve().parents[2]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


class BootstrapTests(unittest.TestCase):
    def test_bad_helper_provenance_does_not_publish_imported_variants(self):
        arena = module("arena_import", ROOT / "scripts/arena.py")
        variant = {
            "id": "new-native",
            "image": "sha256:" + "a" * 64,
            "stack": "enbility",
            "roles": ["EG", "CS"],
            "provenance": {"stack": "enbility"},
        }
        helper = {
            "image": "sha256:" + "b" * 64,
            "provenance": {"stack": "netfault", "sourceSHA256": "c" * 64},
        }
        with tempfile.TemporaryDirectory() as directory:
            arena.STATE = pathlib.Path(directory)
            catalog = arena.STATE / "variants.json"
            catalog.write_text("[]\n")
            archive = arena.STATE / "images.tar"
            archive.write_bytes(b"opaque image archive")
            archive.with_suffix(".tar.json").write_text(
                json.dumps(
                    {
                        "schemaVersion": 1,
                        "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                        "variants": [variant],
                        "netfault": helper,
                    }
                )
            )
            inspected = [
                json.dumps(
                    [
                        {
                            "Id": variant["image"],
                            "Config": {
                                "Labels": {
                                    "org.eebus-arena.provenance": json.dumps(variant["provenance"])
                                }
                            },
                        }
                    ]
                ),
                json.dumps(
                    [
                        {
                            "Id": helper["image"],
                            "Config": {
                                "Labels": {
                                    "org.eebus-arena.provenance": json.dumps({"stack": "foreign"})
                                }
                            },
                        }
                    ]
                ),
            ]
            with (
                patch.object(arena.sys, "argv", ["arena.py", "images-import", str(archive)]),
                patch.object(arena, "call"),
                patch.object(arena.subprocess, "check_output", side_effect=inspected),
                self.assertRaisesRegex(RuntimeError, "helper image provenance mismatch"),
            ):
                arena.main()
            self.assertEqual(catalog.read_text(), "[]\n")
            self.assertFalse((arena.STATE / "netfault.json").exists())

    def test_network_helper_requires_immutable_image_and_source_provenance(self):
        arena = module("arena_helper", ROOT / "scripts/arena.py")
        helper = {
            "image": "sha256:" + "a" * 64,
            "provenance": {"stack": "netfault", "sourceSHA256": "b" * 64},
        }
        arena.validate_helper(helper)
        for invalid in [
            None,
            {**helper, "image": "eebus-arena/netfault:local"},
            {**helper, "provenance": {}},
            {**helper, "provenance": {"stack": "enbility", "sourceSHA256": "b" * 64}},
        ]:
            with self.subTest(helper=invalid), self.assertRaises(RuntimeError):
                arena.validate_helper(invalid)

    def test_all_sources_and_images_are_immutable(self):
        lock = json.loads((ROOT / "recipes/lock.json").read_text())
        for source in lock["sources"].values():
            self.assertRegex(source["commit"], r"^[0-9a-f]{40}$")
            self.assertRegex(source["sha256"], r"^[0-9a-f]{64}$")
        for image in lock["images"].values():
            self.assertRegex(image, r"@sha256:[0-9a-f]{64}$")

    def test_cached_archive_checksum_is_checked_before_extraction(self):
        arena = module("arena_bootstrap", ROOT / "scripts/arena.py")
        with tempfile.TemporaryDirectory() as directory:
            arena.STATE = pathlib.Path(directory)
            digest = hashlib.sha256(b"original").hexdigest()
            arena.LOCK = {
                "sources": {
                    "test": {"repository": "example/test", "commit": "a" * 40, "sha256": digest}
                }
            }
            cache = arena.STATE / "cache"
            cache.mkdir()
            (cache / (digest + ".tar.gz")).write_bytes(b"corrupted")
            with (
                patch.object(
                    arena.urllib.request, "urlopen", side_effect=AssertionError("must use cache")
                ),
                self.assertRaisesRegex(RuntimeError, "checksum mismatch"),
            ):
                arena.fetch()
            self.assertFalse((arena.STATE / "sources/test").exists())

    def test_archive_cannot_write_outside_source_tree(self):
        arena = module("arena_bootstrap", ROOT / "scripts/arena.py")
        content = io.BytesIO()
        with tarfile.open(fileobj=content, mode="w:gz") as bundle:
            member = tarfile.TarInfo("root/../../escaped")
            member.size = 1
            bundle.addfile(member, io.BytesIO(b"x"))
        blob = content.getvalue()
        with tempfile.TemporaryDirectory() as directory:
            arena.STATE = pathlib.Path(directory)
            digest = hashlib.sha256(blob).hexdigest()
            arena.LOCK = {
                "sources": {
                    "test": {"repository": "example/test", "commit": "a" * 40, "sha256": digest}
                }
            }
            cache = arena.STATE / "cache"
            cache.mkdir()
            (cache / (digest + ".tar.gz")).write_bytes(blob)
            with self.assertRaises(tarfile.FilterError):
                arena.fetch()
            self.assertFalse((arena.STATE / "escaped").exists())

    def test_source_symlinks_are_preserved(self):
        prepare = module("arena_prepare", ROOT / "scripts/prepare.py")
        with tempfile.TemporaryDirectory() as directory:
            prepare.ROOT = pathlib.Path(directory)
            prepare.STATE = prepare.ROOT / ".arena"
            source = prepare.STATE / "sources/test"
            source.mkdir(parents=True)
            (source / "real").mkdir()
            (source / "real/data").write_text("source")
            (source / "alias").symlink_to("real", target_is_directory=True)
            (prepare.ROOT / "adapters/test").mkdir(parents=True)
            (prepare.ROOT / "go.mod").write_text("module test\n\ngo 1.27.1\n")
            (prepare.ROOT / "cmd/arena-supervisor").mkdir(parents=True)
            (prepare.ROOT / "internal/resourceprobe").mkdir(parents=True)
            target = prepare.prepare("test", {"upstream": "test"})
            self.assertTrue((target / "upstream/alias").is_symlink())
            self.assertEqual((target / "upstream/alias/data").read_text(), "source")


if __name__ == "__main__":
    unittest.main()
