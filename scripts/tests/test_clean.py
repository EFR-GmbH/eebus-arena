"""Cleanup boundaries protect runtime evidence and inputs outside the checkout."""

import contextlib
import io
import pathlib
import tempfile
import unittest
from unittest.mock import patch

from scripts.tests.test_bootstrap import ROOT, module


class CleanupTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.arena = module("arena_cleanup", ROOT / "scripts/arena.py")
        self.arena.ROOT = self.root
        self.arena.STATE = self.root / ".arena"

    def file(self, name):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(name)
        return path

    def clean(self, **options):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.arena.clean_artifacts(**options)
        return output.getvalue()

    def test_default_removes_outputs_and_preserves_runtime_and_cached_inputs(self):
        outputs = [
            self.file(name)
            for name in (
                ".arena/generated/go/binary",
                "bin/arena",
                "web/dist/app.js",
                "internal/webui/dist/app.js",
            )
        ]
        retained = [
            self.file(name)
            for name in (
                ".arena/runs/run-one/identities/key.pem",
                ".arena/dropped/run-two/events.ndjson",
                ".arena/physical/device/setup.json",
                ".arena/verification/evidence.json",
                ".arena/variants.json",
                ".arena/netfault.json",
                ".arena/control.compose.json",
                ".arena/cache/archive",
                ".arena/sources/source/file",
                ".arena/dev-cache/go-build/file",
                "web/node_modules/package/file",
                "internal/webui/static/index.html",
                "tmp/report.md",
            )
        ]
        with patch.object(self.arena, "call") as docker:
            self.clean()
            docker.assert_not_called()
        self.assertTrue(all(not path.exists() for path in outputs))
        self.assertTrue(
            all(path.read_text() == str(path.relative_to(self.root)) for path in retained)
        )

    def test_cache_cleanup_and_repeat_are_safe(self):
        paths = [
            self.file(name)
            for name in (
                ".arena/cache/archive",
                ".arena/sources/source/file",
                ".arena/dev-cache/go-build/file",
                "web/node_modules/package/file",
            )
        ]
        self.clean(cache=True)
        self.assertTrue(all(not path.exists() for path in paths))
        self.assertIn("No local build artifacts", self.clean(cache=True))

    def test_dry_run_previews_every_option_without_deletion_or_docker(self):
        artifact = self.file(".arena/generated/go/file")
        cached = self.file(".arena/cache/archive")
        with patch.object(self.arena, "call") as docker:
            output = self.clean(cache=True, docker_cache=True, dry_run=True)
            docker.assert_not_called()
        self.assertIn("Would remove .arena/generated", output)
        self.assertIn("Would remove .arena/cache", output)
        self.assertIn("Would prune", output)
        self.assertTrue(artifact.exists() and cached.exists())

    def test_explicit_docker_option_prunes_only_builder_cache(self):
        with patch.object(self.arena, "call") as docker:
            self.clean(docker_cache=True)
            docker.assert_called_once_with("docker", "builder", "prune", "--all", "--force")

    def test_symlink_targets_survive_and_linked_parents_fail_before_any_deletion(self):
        with tempfile.TemporaryDirectory() as outside:
            external = pathlib.Path(outside)
            (external / "keep").write_text("retained")
            self.arena.STATE.mkdir()
            linked = self.arena.STATE / "generated"
            linked.symlink_to(external, target_is_directory=True)
            self.clean()
            self.assertFalse(linked.is_symlink())
            self.assertEqual((external / "keep").read_text(), "retained")
            artifact = self.file(".arena/generated/go/file")
            (external / "dist").mkdir()
            (self.root / "web").symlink_to(external, target_is_directory=True)
            with self.assertRaisesRegex(RuntimeError, "symlinked directory"):
                self.clean()
            self.assertTrue(artifact.exists())
            self.assertTrue((external / "dist").exists())

    def test_internal_symlinks_and_broken_artifact_link_are_not_followed(self):
        evidence = self.file(".arena/runs/run-one/evidence")
        generated = self.arena.STATE / "generated"
        generated.mkdir()
        (generated / "link").symlink_to(evidence.parent, target_is_directory=True)
        self.clean()
        self.assertTrue(evidence.exists())
        generated.symlink_to(self.root / "missing", target_is_directory=True)
        self.clean()
        self.assertFalse(generated.is_symlink())

    def test_clean_command_parses_and_dispatches_without_a_docker_dependency(self):
        with (
            patch.object(
                self.arena.sys,
                "argv",
                ["arena.py", "clean", "--cache", "--docker-cache", "--dry-run"],
            ),
            patch.object(self.arena, "clean_artifacts") as cleanup,
            patch.object(self.arena, "call") as docker,
        ):
            self.arena.main()
            cleanup.assert_called_once_with(cache=True, docker_cache=True, dry_run=True)
            docker.assert_not_called()


if __name__ == "__main__":
    unittest.main()
