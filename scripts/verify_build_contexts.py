#!/usr/bin/env python3
"""Inspect Docker's filtered build contexts without building application images."""

import argparse
import json
import os
import pathlib
import subprocess
import sys
import tarfile
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
TARGETS = ("enbility", "openeebus", "jeebus", "mdns-stresser", "orchestrator", "netfault")
REQUIRED = {
    "netfault": ("go.mod", "cmd/arena-netfault/main.go"),
    "mdns-stresser": ("go.mod", "cmd/mdns-stresser/main.go", "internal/mdnsstress/wire.go"),
    "enbility": (
        "adapter/go.mod",
        "adapter/main.go",
        "eebus/go.mod",
        "ship/go.mod",
        "spine/go.mod",
    ),
    "openeebus": (
        "upstream/CMakeLists.txt",
        "upstream/arena/main.c",
        "libwebsockets/CMakeLists.txt",
    ),
    "jeebus": (
        "release-sources.json",
        "upstream/build.gradle",
        "upstream/gradle/verification-metadata.xml",
        "upstream/projects/demo/gradle.lockfile",
    ),
    "orchestrator": (
        "go.mod",
        "cmd/arena/main.go",
        "internal/webui/web.go",
        "web/package.json",
        "web/package-lock.json",
        "web/src/main.tsx",
    ),
}


def verify(target, files):
    """Reject unrelated state or stack inputs, and empty/incomplete contexts."""
    prefix = "" if target in ("orchestrator", "netfault") else f".arena/generated/{target}/"
    required = {prefix + path for path in REQUIRED[target]}
    if prefix:
        required.add(prefix + "provenance.json")
    missing = required - files
    if missing:
        raise RuntimeError(f"{target}: required files missing: {', '.join(sorted(missing))}")
    for name in files:
        parts = pathlib.PurePosixPath(name).parts
        if any(
            part in (".git", ".github", "node_modules", "__pycache__") for part in parts
        ) or name.endswith(".pyc"):
            raise RuntimeError(f"{target}: unnecessary file in context: {name}")
        if prefix and not name.startswith(prefix):
            raise RuntimeError(
                f"{target}: context includes a different stack or arena state: {name}"
            )
        if not prefix:
            if name != "go.mod" and not name.startswith(("cmd/", "internal/", "web/")):
                raise RuntimeError(f"{target}: unrelated repository file in context: {name}")
            if name.startswith("internal/webui/static/") or name.endswith("_test.go"):
                raise RuntimeError(f"{target}: obsolete build output or tests in context: {name}")
    if prefix and not any(name.startswith(prefix + "licenses/") for name in files):
        raise RuntimeError(f"{target}: upstream licenses missing")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("targets", nargs="*", help="Targets to inspect; default: all five")
    parser.add_argument("--output", type=pathlib.Path, help="Save JSON context inventories")
    args = parser.parse_args()
    targets = args.targets or list(TARGETS)
    if any(target not in TARGETS for target in targets):
        parser.error("Targets must be: " + ", ".join(TARGETS))
    for target in targets:
        if (
            target not in ("orchestrator", "netfault")
            and not (ROOT / ".arena/generated" / target / "provenance.json").is_file()
        ):
            raise RuntimeError("Prepare the source trees first: python3 scripts/arena.py prepare")
    report = {}
    with tempfile.TemporaryDirectory(prefix="eebus-arena-contexts-") as directory:
        stage = pathlib.Path(directory)
        environment = dict(os.environ)
        if not environment.get("DOCKER_HOST"):
            environment["DOCKER_HOST"] = subprocess.check_output(
                ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
                text=True,
            ).strip()
        environment.pop("DOCKER_CONTEXT", None)
        # Isolate buildx bookkeeping while retaining the chosen Docker endpoint.
        environment.setdefault("DOCKER_CONFIG", str(stage / "docker-config"))
        for target in targets:
            dockerfile = stage / (target + ".Dockerfile")
            dockerfile.write_text("FROM scratch\nCOPY . /context/\n")
            ignore = ROOT / "docker" / (target + ".Dockerfile.dockerignore")
            dockerfile.with_suffix(".Dockerfile.dockerignore").write_bytes(ignore.read_bytes())
            archive = stage / (target + ".tar")
            result = subprocess.run(
                [
                    "docker",
                    "build",
                    "--progress=plain",
                    "-f",
                    str(dockerfile),
                    "--output",
                    "type=tar,dest=" + str(archive),
                    str(ROOT),
                ],
                capture_output=True,
                text=True,
                env=environment,
            )
            if result.returncode:
                raise RuntimeError(f"{target}: context export failed:\n{result.stderr[-4000:]}")
            with tarfile.open(archive) as bundle:
                members = [
                    member for member in bundle.getmembers() if member.isfile() or member.issym()
                ]
            files = {member.name.removeprefix("context/") for member in members}
            verify(target, files)
            size = sum(member.size for member in members if member.isfile())
            report[target] = {"files": len(files), "bytes": size, "paths": sorted(files)}
            print(f"{target}: {len(files)} files, {size:,} bytes", flush=True)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
