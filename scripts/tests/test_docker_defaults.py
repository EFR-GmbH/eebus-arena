"""Keep standalone Dockerfile defaults consistent with the build lock."""

import json
import re
import unittest

from scripts.tests.test_bootstrap import ROOT


class DockerDefaultsTests(unittest.TestCase):
    def test_from_image_defaults_match_locked_build_arguments(self):
        images = json.loads((ROOT / "recipes/lock.json").read_text())["images"]
        for path in sorted((ROOT / "docker").glob("*.Dockerfile")):
            text = path.read_text()
            preamble = text.split("FROM ", 1)[0]
            defaults = dict(re.findall(r"^ARG (\w+_IMAGE)(?:=(\S+))?$", preamble, re.MULTILINE))
            references = set(re.findall(r"^FROM \$\{(\w+_IMAGE)\}", text, re.MULTILINE))
            for name in references:
                with self.subTest(dockerfile=path.name, image=name):
                    self.assertEqual(
                        defaults.get(name), images[name.removesuffix("_IMAGE").lower()]
                    )


if __name__ == "__main__":
    unittest.main()
