#!/usr/bin/env python3
"""Build and run the shared development toolbox."""

import argparse
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
IMAGE = "eebus-arena/dev:local"


def run(*command: str, cwd: Path = ROOT) -> None:
    """Print the command so a failed local or CI check is easy to reproduce."""
    print("+", " ".join(command), flush=True)
    subprocess.run(command, cwd=cwd, check=True)


def docker_environment() -> dict[str, str]:
    environment = dict(os.environ)
    if not environment.get("DOCKER_HOST"):
        environment["DOCKER_HOST"] = subprocess.check_output(
            ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
            text=True,
        ).strip()
    environment.pop("DOCKER_CONTEXT", None)
    # Buildx writes activity metadata even for a read-only source checkout.
    # Keep those writes in project state instead of the user's Docker settings.
    config = ROOT / ".arena/dev-docker-config"
    config.mkdir(parents=True, exist_ok=True)
    environment["DOCKER_CONFIG"] = str(config)
    return environment


def in_container(action: str) -> None:
    environment = docker_environment()
    if action == "build":
        subprocess.run(
            ["docker", "build", "-f", "docker/dev.Dockerfile", "-t", IMAGE, "."],
            cwd=ROOT,
            env=environment,
            check=True,
        )
        return
    cache = ROOT / ".arena/dev-cache"
    cache.mkdir(parents=True, exist_ok=True)
    mount = f"type=bind,src={ROOT},dst=/workspace"
    if action != "format":
        mount += ",readonly"
    subprocess.run(
        [
            "docker",
            "run",
            "--rm",
            "--user",
            f"{os.getuid()}:{os.getgid()}",
            "--mount",
            mount,
            "--mount",
            f"type=bind,src={cache},dst=/cache",
            "--env",
            "ARENA_DEV_CONTAINER=1",
            "--env",
            "GOCACHE=/cache/go-build",
            "--env",
            "GOMODCACHE=/cache/go-modules",
            "--env",
            "RUFF_CACHE_DIR=/cache/ruff",
            "--env",
            "PYTHONDONTWRITEBYTECODE=1",
            IMAGE,
            "python3",
            "scripts/dev.py",
            action,
        ],
        cwd=ROOT,
        env=environment,
        check=True,
    )


def source_files(*patterns: str) -> list[str]:
    return sorted(
        str(path.relative_to(ROOT)) for pattern in patterns for path in ROOT.glob(pattern)
    )


def format_sources(*, check: bool) -> None:
    run("ruff", "check", *([] if check else ["--fix"]), "scripts", "adapters")
    run("ruff", "format", *(["--check"] if check else []), "scripts", "adapters")
    c_files = source_files("adapters/**/*.c", "adapters/**/*.h", "adapters/**/*.c.inc")
    run("clang-format-14", *(["--dry-run", "--Werror"] if check else ["-i"]), *c_files)
    java_options = ["--dry-run", "--set-exit-if-changed"] if check else ["--replace"]
    run(
        "java",
        "-jar",
        "/opt/google-java-format.jar",
        *java_options,
        *source_files("adapters/**/*.java"),
    )
    go_files = source_files("cmd/**/*.go", "internal/**/*.go", "adapters/**/*.go")
    if check:
        changed = subprocess.check_output(["gofmt", "-l", *go_files], cwd=ROOT, text=True)
        if changed:
            raise RuntimeError("Run python3 scripts/dev.py format to format:\n" + changed)
    else:
        run("gofmt", "-w", *go_files)
    if check:
        run(
            "cppcheck",
            "--enable=warning,performance,portability",
            "--error-exitcode=1",
            "--std=c11",
            "--suppress=missingIncludeSystem",
            "adapters/openeebus",
        )


def test_sources() -> None:
    run("python3", "-m", "unittest", "discover", "-s", "scripts/tests")
    run("go", "vet", "./...")
    run("go", "test", "-race", "./...")
    adapter = ROOT / ".arena/generated/enbility/adapter"
    if not (adapter / "go.mod").exists():
        raise RuntimeError("Prepare the adapters first: python3 scripts/arena.py prepare")
    run("go", "vet", "-mod=readonly", "./...", cwd=adapter)
    run("go", "test", "-mod=readonly", "-race", "./...", cwd=adapter)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("build", "format", "check", "test", "native"))
    args = parser.parse_args()
    if not os.getenv("ARENA_DEV_CONTAINER"):
        in_container(args.action)
    elif args.action in ("format", "check"):
        format_sources(check=args.action == "check")
    elif args.action == "test":
        test_sources()
    elif args.action == "native":
        run("python3", "scripts/check_native.py")
    else:
        parser.error("Build the development image from the host")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
