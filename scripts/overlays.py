"""Apply reviewed native-source patches to disposable build trees."""

import hashlib
import json
import os
import subprocess
from pathlib import Path


def apply_overlay(target: Path, patch: Path) -> None:
    """Require the recorded preimages before applying an entire unified diff.

    Git can relocate a hunk when similar code appears elsewhere. Checking the
    full input first prevents an upstream update from silently moving an
    observation hook to the wrong branch. A null digest means a new file.
    """
    target = target.resolve()
    expected = json.loads(patch.with_suffix(".json").read_text())
    # The manifest is also the review boundary: every path in the diff needs a
    # preimage entry, including new files. Do not allow an added hunk to bypass it.
    changed = set()
    for line in patch.read_text().splitlines():
        if line.startswith(("--- ", "+++ ")):
            name = line[4:]
            if name != "/dev/null":
                if not name.startswith(("a/", "b/")):
                    raise RuntimeError(f"Unsupported overlay path: {name}")
                changed.add(name[2:])
    if changed != set(expected):
        raise RuntimeError(f"Overlay manifest does not match patch paths: {patch}")
    for name, digest in expected.items():
        path = target / name
        if not path.resolve().is_relative_to(target):
            raise RuntimeError(f"Overlay path escapes build tree: {name}")
        actual = hashlib.sha256(path.read_bytes()).hexdigest() if path.is_file() else None
        if actual != digest or (digest is None and path.exists()):
            raise RuntimeError(
                f"Patch context mismatch in {path}: expected {digest}, got {actual}. "
                f"Review and rebase {patch.name} against the pinned upstream source."
            )

    environment = dict(os.environ)
    # Generated trees usually live inside this repository. Prevent Git from
    # discovering its parent index and silently skipping paths outside the cwd.
    environment["GIT_CEILING_DIRECTORIES"] = str(target.parent)
    for variable in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"):
        environment.pop(variable, None)
    command = ["git", "apply", "--whitespace=nowarn"]
    for options in (["--check"], []):
        result = subprocess.run(
            [*command, *options, str(patch.resolve())],
            cwd=target,
            env=environment,
            capture_output=True,
            text=True,
            check=False,
        )
        if result.returncode:
            raise RuntimeError(f"Cannot apply {patch}:\n{result.stderr.strip()}")
