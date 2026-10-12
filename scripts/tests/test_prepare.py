"""Check first-party helper packaging without downloading upstream sources."""

import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from scripts import prepare


class PrepareTests(unittest.TestCase):
    def test_mdns_stresser_bundles_project_notice(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            with patch.object(prepare, "STATE", state):
                prepare.mdns_stresser()
            self.assertEqual(
                (state / "generated/mdns-stresser/licenses/NOTICE.md").read_bytes(),
                (prepare.ROOT / "NOTICE.md").read_bytes(),
            )


if __name__ == "__main__":
    unittest.main()
