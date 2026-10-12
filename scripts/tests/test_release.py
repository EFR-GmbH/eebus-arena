"""Reject compiler flag regressions that artifact stripping alone cannot catch."""

import json
import unittest
from unittest.mock import patch

from scripts.tests.test_bootstrap import ROOT, module
from scripts.verify_release import elf_sections, verify_compile_commands


class ReleaseTests(unittest.TestCase):
    def manifest(self, flags, command=False):
        args = ["/usr/bin/cc", *flags, "-o", "node.o", "-c", "/src/node.c"]
        entry = {"file": "/src/node.c"}
        entry["command" if command else "arguments"] = " ".join(args) if command else args
        return json.dumps([entry]).encode()

    def test_release_commands_and_argument_arrays(self):
        for command in (False, True):
            with self.subTest(command=command):
                result = verify_compile_commands(self.manifest(["-O2", "-DNDEBUG"], command))
                self.assertEqual(result["translationUnits"], 1)

    def test_later_optimization_override_cannot_pass(self):
        for override in ("-O0", "-O1", "-Os", "-Og", "-O3"):
            with self.subTest(override=override), self.assertRaisesRegex(ValueError, "not an -O2"):
                verify_compile_commands(self.manifest(["-O2", "-DNDEBUG", override]))

    def test_effective_last_optimization_is_used(self):
        verify_compile_commands(self.manifest(["-O0", "-O2", "-DNDEBUG"]))

    def test_debug_sanitizers_and_assertions_cannot_pass(self):
        for flags in (
            ["-O2"],
            ["-O2", "-DNDEBUG", "-UNDEBUG"],
            ["-O2", "-DNDEBUG", "-g3"],
            ["-O2", "-DNDEBUG", "-fsanitize=address"],
        ):
            with self.subTest(flags=flags), self.assertRaises(ValueError):
                verify_compile_commands(self.manifest(flags))

    def test_empty_manifest_cannot_pass(self):
        with self.assertRaisesRegex(ValueError, "empty"):
            verify_compile_commands(b"[]")

    def test_truncated_or_non_elf_artifact_cannot_pass(self):
        for data in (b"", b"not an executable", b"\x7fELF" + bytes(10)):
            with self.subTest(data=data), self.assertRaises(ValueError):
                elf_sections(data)

    def test_failed_artifact_audit_does_not_promote_or_register_image(self):
        arena = module("release_bootstrap", ROOT / "scripts/arena.py")
        calls = []

        def call(*args):
            calls.append(args)
            if len(args) > 1 and args[1].endswith("verify_release.py"):
                raise RuntimeError("artifact audit failed")

        with (
            patch.object(arena, "call", side_effect=call),
            patch.object(arena.subprocess, "check_output", return_value="sha256:" + "a" * 64),
            patch.object(arena, "register") as register,
        ):
            with self.assertRaisesRegex(RuntimeError, "artifact audit failed"):
                arena.build("orchestrator")
            register.assert_not_called()
        self.assertFalse(any(args[:3] == ("docker", "image", "tag") for args in calls))


if __name__ == "__main__":
    unittest.main()
