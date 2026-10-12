#!/usr/bin/env python3
"""Reproducible arena bootstrap. Python standard library only."""

import argparse
import datetime
import hashlib
import io
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tarfile
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]
STATE = ROOT / ".arena"
LOCK = json.loads((ROOT / "recipes/lock.json").read_text())


def call(*args, cwd=ROOT):
    config = STATE / "docker-config"
    config.mkdir(parents=True, exist_ok=True)
    environment = {**os.environ, "DOCKER_CONFIG": os.environ.get("DOCKER_CONFIG", str(config))}
    if "DOCKER_HOST" not in environment:
        context = subprocess.run(
            ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"],
            capture_output=True,
            text=True,
        )
        if context.returncode == 0 and context.stdout.strip():
            environment["DOCKER_HOST"] = context.stdout.strip()
    result = subprocess.run(args, cwd=cwd, env=environment)
    if result.returncode:
        raise RuntimeError(
            f"{args[0]} {args[1] if len(args) > 1 else ''} failed (exit {result.returncode})"
        )


def fetch():
    for name, source in LOCK["sources"].items():
        target = STATE / "sources" / name
        marker = target / ".arena-source.json"
        if marker.exists() and json.loads(marker.read_text()) == source:
            continue
        archive = STATE / "cache" / (source["sha256"] + ".tar.gz")
        archive.parent.mkdir(parents=True, exist_ok=True)
        if not archive.exists():
            url = f"https://codeload.github.com/{source['repository']}/tar.gz/{source['commit']}"
            print(f"Fetching {name} at {source['commit']}", flush=True)
            with urllib.request.urlopen(url, timeout=120) as response:
                blob = response.read()
            if hashlib.sha256(blob).hexdigest() != source["sha256"]:
                raise RuntimeError(f"Archive checksum mismatch: {name}")
            archive.write_bytes(blob)
        blob = archive.read_bytes()
        if hashlib.sha256(blob).hexdigest() != source["sha256"]:
            raise RuntimeError(f"Cached archive checksum mismatch: {name}")
        if target.exists():
            shutil.rmtree(target)
        target.mkdir(parents=True)
        with tarfile.open(fileobj=io.BytesIO(blob)) as bundle:
            for member in bundle.getmembers():
                parts = pathlib.PurePosixPath(member.name).parts[1:]
                if not parts:
                    continue
                member.name = str(pathlib.PurePosixPath(*parts))
                bundle.extract(member, target, filter="data")
        marker.write_text(json.dumps(source, indent=2) + "\n")


def validate_import_variants(variants):
    import re

    if not isinstance(variants, list):
        raise RuntimeError("Image variants must be a list")
    for variant in variants:
        if not isinstance(variant, dict) or not re.fullmatch(
            r"[a-z][a-z0-9-]{0,47}", str(variant.get("id", ""))
        ):
            raise RuntimeError("Variant must have a safe ID")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", str(variant.get("image", ""))):
            raise RuntimeError("Variant must have an immutable image ID")
        roles = {
            "enbility": ["EG", "CS"],
            "openeebus": ["EG", "CS"],
            "jeebus": ["CS"],
            "mdns-stresser": ["STRESSER"],
        }
        stack = variant.get("stack")
        if (
            stack not in roles
            or variant.get("roles") != roles[stack]
            or not isinstance(variant.get("provenance"), dict)
            or variant["provenance"].get("stack") != stack
        ):
            raise RuntimeError("Variant capabilities do not match stack")


def validate_helper(helper):
    if not isinstance(helper, dict) or not re.fullmatch(
        r"sha256:[0-9a-f]{64}", str(helper.get("image", ""))
    ):
        raise RuntimeError("Network helper must have an immutable image ID")
    provenance = helper.get("provenance")
    if (
        not isinstance(provenance, dict)
        or provenance.get("stack") != "netfault"
        or not re.fullmatch(r"[0-9a-f]{64}", str(provenance.get("sourceSHA256", "")))
    ):
        raise RuntimeError("Network helper source provenance is missing; rebuild it")


def prepare():
    fetch()
    call(sys.executable, str(ROOT / "scripts/prepare.py"))


def clean_artifacts(*, cache=False, docker_cache=False, dry_run=False):
    """Remove regenerable outputs without traversing runtime state or symlinked parents."""
    paths = [STATE / "generated", ROOT / "bin", ROOT / "web/dist", ROOT / "internal/webui/dist"]
    if cache:
        paths.extend(
            [STATE / "cache", STATE / "sources", STATE / "dev-cache", ROOT / "web/node_modules"]
        )
    existing = [path for path in paths if path.exists() or path.is_symlink()]
    # Validate the whole plan before removing anything. A linked build tree is
    # detached at its own path; linked ancestors must never redirect deletion.
    for path in existing:
        if not path.is_relative_to(ROOT):
            raise RuntimeError(f"Cleanup path is outside the repository: {path}")
        for parent in path.parents:
            if parent == ROOT:
                break
            if parent.is_symlink():
                raise RuntimeError(f"Refusing cleanup through symlinked directory: {parent}")
    for path in existing:
        print(f"{'Would remove' if dry_run else 'Removing'} {path.relative_to(ROOT)}", flush=True)
        if not dry_run:
            if path.is_symlink() or not path.is_dir():
                path.unlink()
            else:
                shutil.rmtree(path)
    if not existing:
        print("No local build artifacts to remove.", flush=True)
    if docker_cache:
        command = ("docker", "builder", "prune", "--all", "--force")
        if dry_run:
            print(
                "Would prune unused cache in the current Docker builder (shared with other projects)."
            )
        else:
            print(
                "Pruning unused cache in the current Docker builder (shared with other projects).",
                flush=True,
            )
            call(*command)


def build(what):
    if what == "netfault":
        build_netfault()
        return
    if what == "mdns-stresser":
        import prepare as preparation

        preparation.mdns_stresser()
    elif what != "orchestrator":
        prepare()
    if what == "all":
        build("netfault")
    targets = (
        ["enbility", "openeebus", "jeebus", "mdns-stresser", "orchestrator"]
        if what == "all"
        else [what]
    )
    for target in targets:
        candidate = f"eebus-arena/{target}:build-candidate"
        args = [
            "docker",
            "build",
            "--platform",
            LOCK["toolchains"]["platform"],
            "--progress=plain",
            "-f",
            f"docker/{target}.Dockerfile",
            "-t",
            candidate,
        ]
        for key, image in LOCK["images"].items():
            args.extend(["--build-arg", f"{key.upper()}_IMAGE={image}"])
        if target != "orchestrator":
            provenance = json.loads((STATE / "generated" / target / "provenance.json").read_text())
            args.extend(
                [
                    "--label",
                    "org.eebus-arena.provenance=" + json.dumps(provenance, separators=(",", ":")),
                ]
            )
        args.extend(["--build-arg", f"DEBIAN_SNAPSHOT={LOCK['toolchains']['debianSnapshot']}", "."])
        call(*args)
        image = subprocess.check_output(
            ["docker", "image", "inspect", candidate, "--format", "{{.Id}}"], text=True
        ).strip()
        call(sys.executable, str(ROOT / "scripts/verify_release.py"), target, "--image", image)
        call("docker", "image", "tag", image, f"eebus-arena/{target}:local")
    register()


def register():
    STATE.mkdir(exist_ok=True)
    variants = (
        json.loads((STATE / "variants.json").read_text())
        if (STATE / "variants.json").exists()
        else []
    )
    # The image's embedded manifest is the source of truth, including for older
    # installed revisions after the bootstrap itself has been updated.
    for variant in variants:
        proc = subprocess.run(
            ["docker", "image", "inspect", variant["image"]], capture_output=True, text=True
        )
        if proc.returncode:
            continue
        details = json.loads(proc.stdout)[0]
        encoded = details["Config"].get("Labels", {}).get("org.eebus-arena.provenance")
        if not encoded:
            raise RuntimeError("Installed arena image has no provenance: " + variant["id"])
        actual = json.loads(encoded)
        if actual["stack"] != variant["stack"]:
            raise RuntimeError("Installed variant stack does not match image: " + variant["id"])
        variant["provenance"] = actual
    for stack, roles in [
        ("enbility", ["EG", "CS"]),
        ("openeebus", ["EG", "CS"]),
        ("jeebus", ["CS"]),
        ("mdns-stresser", ["STRESSER"]),
    ]:
        image = f"eebus-arena/{stack}:local"
        proc = subprocess.run(
            ["docker", "image", "inspect", image, "--format", "{{.Id}}"],
            capture_output=True,
            text=True,
        )
        if proc.returncode:
            continue
        details = json.loads(
            subprocess.check_output(["docker", "image", "inspect", image], text=True)
        )[0]
        encoded = details["Config"].get("Labels", {}).get("org.eebus-arena.provenance")
        if not encoded:
            continue
        provenance = json.loads(encoded)
        if provenance["stack"] != stack:
            raise RuntimeError("Image provenance stack mismatch")
        digest = proc.stdout.strip()
        variant = {
            "builtAt": datetime.datetime.now(datetime.UTC).isoformat(),
            "id": stack + "-" + provenance["recipeHash"][:12],
            "stack": stack,
            "label": stack
            + " "
            + next(iter(provenance["sources"].values()), {"commit": "local"})["commit"][:7]
            + " / "
            + provenance["recipeHash"][:8],
            "roles": roles,
            "image": digest,
            "provenance": provenance,
        }
        if any(v["id"] == variant["id"] and v["image"] != digest for v in variants):
            variant["id"] += "-" + digest.removeprefix("sha256:")[:8]
        if not any(v["id"] == variant["id"] for v in variants):
            variants.append(variant)
    (STATE / "variants.json").write_text(json.dumps(variants, indent=2) + "\n")
    tag_installed(variants)


def tag_installed(variants):
    # Keep revisions from becoming dangling images when :local moves forward.
    for variant in variants:
        image = variant["image"]
        installed = subprocess.run(["docker", "image", "inspect", image], capture_output=True)
        if installed.returncode == 0:
            call("docker", "image", "tag", image, f"eebus-arena/{variant['stack']}:{variant['id']}")


def up(port, monitoring=False):
    if not (STATE / "variants.json").exists():
        raise RuntimeError("Build the stack images first: python3 scripts/arena.py build")
    STATE.mkdir(exist_ok=True)
    endpoint = (
        os.environ.get("DOCKER_HOST")
        or subprocess.check_output(
            ["docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"], text=True
        ).strip()
    )
    if not endpoint.startswith("unix://"):
        raise RuntimeError("Only a local Unix Docker socket is supported")
    socket = endpoint.removeprefix("unix://")
    compose = {
        "name": "eebus-arena-control",
        "services": {
            "arena": {
                "image": "eebus-arena/orchestrator:local",
                "ports": [f"127.0.0.1:{port}:8080"],
                "environment": {
                    "ARENA_DATA": "/data",
                    "ARENA_HOST_DATA": str(STATE),
                    "ARENA_LISTEN": ":8080",
                },
                "volumes": [f"{STATE}:/data", f"{socket}:/var/run/docker.sock"],
                "init": True,
                "labels": {"org.eebus-arena.control": "true"},
            }
        },
    }
    compose["services"]["arena"]["environment"]["ARENA_CONTROL_IMAGE"] = subprocess.check_output(
        ["docker", "image", "inspect", "eebus-arena/orchestrator:local", "--format", "{{.Id}}"],
        text=True,
    ).strip()
    if hasattr(os, "getuid") and pathlib.Path(socket).exists():
        compose["services"]["arena"]["user"] = f"{os.getuid()}:{os.getgid()}"
        # Ask the engine: a user namespace can map the socket's group to nobody
        # in this process.
        socket_group = subprocess.check_output(
            [
                "docker",
                "run",
                "--rm",
                "--entrypoint",
                "stat",
                "--mount",
                f"type=bind,src={socket},dst=/var/run/docker.sock",
                "eebus-arena/orchestrator:local",
                "-c",
                "%g",
                "/var/run/docker.sock",
            ],
            text=True,
        ).strip()
        if not socket_group.isdecimal():
            raise RuntimeError("Could not determine Docker socket group")
        compose["services"]["arena"]["group_add"] = [socket_group]
    compose["services"]["arena"]["environment"]["DOCKER_CONFIG"] = "/data/docker-config"
    if monitoring:
        for name in ["grafana", "prometheus"]:
            call("docker", "pull", LOCK["images"][name])
        compose["services"]["prometheus"] = {
            "image": LOCK["images"]["prometheus"],
            "ports": ["127.0.0.1:9091:9090"],
            "volumes": [f"{ROOT}/monitoring/prometheus.yml:/etc/prometheus/prometheus.yml:ro"],
            "labels": {"org.eebus-arena.control": "true"},
        }
        compose["services"]["grafana"] = {
            "image": LOCK["images"]["grafana"],
            "ports": ["127.0.0.1:3001:3000"],
            "environment": {
                "GF_AUTH_ANONYMOUS_ENABLED": "true",
                "GF_AUTH_ANONYMOUS_ORG_ROLE": "Viewer",
            },
            "volumes": [
                f"{ROOT}/monitoring/grafana/provisioning:/etc/grafana/provisioning:ro",
                f"{ROOT}/monitoring/grafana/dashboards:/var/lib/grafana/dashboards:ro",
            ],
            "labels": {"org.eebus-arena.control": "true"},
        }
    path = STATE / "control.compose.json"
    path.write_text(json.dumps(compose, indent=2) + "\n")
    call("docker", "compose", "-f", str(path), "up", "-d", "--pull", "never", "--remove-orphans")
    print(f"Arena: http://localhost:{port}")


def pin_source(name: str, commit: str, output: pathlib.Path) -> None:
    """Write a reviewed source pin without changing the baseline lock."""
    if not re.fullmatch(r"[a-f0-9]{40}", commit):
        raise RuntimeError("Commit must be a full lowercase SHA")
    source = dict(LOCK["sources"][name])
    source["commit"] = commit
    blob = urllib.request.urlopen(
        f"https://codeload.github.com/{source['repository']}/tar.gz/{commit}", timeout=120
    ).read()
    source["sha256"] = hashlib.sha256(blob).hexdigest()
    override = (
        json.loads(output.read_text()) if output.exists() else {"schemaVersion": 1, "sources": {}}
    )
    override.setdefault("sources", {})[name] = source
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(override, indent=2) + "\n")
    print(output)


def print_example(preset: str) -> None:
    """Build a sample topology from installed variants."""
    catalog = json.loads((STATE / "variants.json").read_text())
    latest = {}
    for variant in catalog:
        latest[variant["stack"]] = variant
    if "enbility" not in latest:
        raise RuntimeError("Build enbility before generating examples")
    clients = [v for v in latest.values() if "CS" in v["roles"]]
    count = 50 if preset == "stress" else len(clients)
    groups = [{"id": "eg", "variant": latest["enbility"]["id"], "role": "EG", "count": 1}] + [
        {
            "id": "cs-" + v["stack"],
            "variant": v["id"],
            "role": "CS",
            "count": count // len(clients) + (i < count % len(clients)),
        }
        for i, v in enumerate(clients)
    ]
    print(
        json.dumps(
            {
                "schemaVersion": 1,
                "definition": {
                    "schemaVersion": 1,
                    "name": preset + " arena",
                    "scenario": "lpc" if preset == "small" else preset,
                    "seed": 42,
                    "timeScale": 1,
                    "durationSeconds": 1800,
                    "groups": groups,
                },
            },
            indent=2,
        )
    )


def export_images(archive_path: pathlib.Path) -> None:
    """Bind an image archive to its variant and helper provenance."""
    catalog = json.loads((STATE / "variants.json").read_text())
    images = list(dict.fromkeys(v["image"] for v in catalog))
    helper = (
        json.loads((STATE / "netfault.json").read_text())
        if (STATE / "netfault.json").exists()
        else None
    )
    if helper:
        validate_helper(helper)
        images.append(helper["image"])
    call("docker", "save", "-o", str(archive_path), *dict.fromkeys(images))
    with archive_path.open("rb") as archive:
        digest = hashlib.file_digest(archive, "sha256").hexdigest()
    manifest = {"schemaVersion": 1, "sha256": digest, "variants": catalog}
    if helper:
        manifest["netfault"] = helper
    archive_path.with_suffix(archive_path.suffix + ".json").write_text(
        json.dumps(manifest, indent=2) + "\n"
    )


def import_images(archive_path: pathlib.Path) -> None:
    """Validate every imported image before publishing the local catalog."""
    manifest = json.loads(archive_path.with_suffix(archive_path.suffix + ".json").read_text())
    if manifest.get("schemaVersion") != 1:
        raise RuntimeError("Unsupported image archive manifest")
    with archive_path.open("rb") as archive:
        if hashlib.file_digest(archive, "sha256").hexdigest() != manifest["sha256"]:
            raise RuntimeError("Image archive checksum mismatch")
    validate_import_variants(manifest.get("variants"))
    helper = manifest.get("netfault")
    if helper:
        validate_helper(helper)
    call("docker", "load", "-i", str(archive_path))
    STATE.mkdir(exist_ok=True)
    catalog = (
        json.loads((STATE / "variants.json").read_text())
        if (STATE / "variants.json").exists()
        else []
    )
    for variant in manifest["variants"]:
        image = json.loads(
            subprocess.check_output(["docker", "image", "inspect", variant["image"]], text=True)
        )[0]
        if image["Id"] != variant["image"]:
            raise RuntimeError("Image ID mismatch")
        encoded = image["Config"].get("Labels", {}).get("org.eebus-arena.provenance")
        if not encoded or json.loads(encoded) != variant["provenance"]:
            raise RuntimeError("Image provenance mismatch")
        roles = {
            "enbility": ["EG", "CS"],
            "openeebus": ["EG", "CS"],
            "jeebus": ["CS"],
            "mdns-stresser": ["STRESSER"],
        }
        if variant["stack"] != variant["provenance"]["stack"] or variant["roles"] != roles.get(
            variant["stack"]
        ):
            raise RuntimeError("Variant capabilities do not match stack")
        existing = next((v for v in catalog if v["id"] == variant["id"]), None)
        if existing and existing["image"] != variant["image"]:
            raise RuntimeError("Imported variant conflicts with installed image")
        if not existing:
            catalog.append(variant)
    if helper:
        image = json.loads(
            subprocess.check_output(["docker", "image", "inspect", helper["image"]], text=True)
        )[0]
        if (
            image["Id"] != helper["image"]
            or json.loads(
                image["Config"].get("Labels", {}).get("org.eebus-arena.provenance", "null")
            )
            != helper["provenance"]
        ):
            raise RuntimeError("Network helper image provenance mismatch")
    # Validate every archive member before publishing either catalog.
    (STATE / "variants.json").write_text(json.dumps(catalog, indent=2) + "\n")
    if helper:
        (STATE / "netfault.json").write_text(json.dumps(helper, indent=2) + "\n")
    tag_installed(catalog)


def build_netfault() -> None:
    """Verify the native network helper before recording its image identity."""
    source_hash = hashlib.sha256()
    for path in [ROOT / "go.mod", *sorted((ROOT / "cmd/arena-netfault").glob("*.go"))]:
        if not path.name.endswith("_test.go"):
            source_hash.update(
                str(path.relative_to(ROOT)).encode() + b"\0" + path.read_bytes() + b"\0"
            )
    provenance = {
        "schemaVersion": 1,
        "stack": "netfault",
        "sourceSHA256": source_hash.hexdigest(),
        "toolchains": LOCK["toolchains"],
        "images": {"go": LOCK["images"]["go"], "debian": LOCK["images"]["debian"]},
    }
    args = [
        "docker",
        "build",
        "--platform",
        LOCK["toolchains"]["platform"],
        "--progress=plain",
        "-f",
        "docker/netfault.Dockerfile",
        "-t",
        "eebus-arena/netfault:local",
    ]
    for key, image in LOCK["images"].items():
        args.extend(["--build-arg", f"{key.upper()}_IMAGE={image}"])
    args.extend(
        [
            "--build-arg",
            f"DEBIAN_SNAPSHOT={LOCK['toolchains']['debianSnapshot']}",
            "--build-arg",
            "PROVENANCE=" + json.dumps(provenance, separators=(",", ":")),
            ".",
        ]
    )
    call(*args)
    call(
        sys.executable,
        str(ROOT / "scripts/verify_release.py"),
        "netfault",
        "--image",
        "eebus-arena/netfault:local",
    )
    image = subprocess.check_output(
        ["docker", "image", "inspect", "eebus-arena/netfault:local", "--format", "{{.Id}}"],
        text=True,
    ).strip()
    STATE.mkdir(exist_ok=True)
    (STATE / "netfault.json").write_text(
        json.dumps({"image": image, "provenance": provenance}, indent=2) + "\n"
    )


def parse_args():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--lock", type=pathlib.Path, help="Recipe override JSON; merged over the baseline lock"
    )
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("fetch")
    pin = commands.add_parser("pin")
    pin.add_argument("source", choices=list(LOCK["sources"]))
    pin.add_argument("commit", help="Full 40-character commit SHA")
    pin.add_argument("--output", type=pathlib.Path, required=True)
    commands.add_parser("prepare")
    cleaner = commands.add_parser("clean", help="Remove regenerable local build artifacts")
    cleaner.add_argument(
        "--dry-run", action="store_true", help="Preview cleanup without deleting anything"
    )
    cleaner.add_argument(
        "--cache",
        action="store_true",
        help="Also remove downloaded sources and development/dependency caches",
    )
    cleaner.add_argument(
        "--docker-cache",
        action="store_true",
        help="Also prune unused Docker builder cache, shared with other projects",
    )
    commands.add_parser("register")
    for name in ["images-export", "images-import"]:
        image_command = commands.add_parser(name)
        image_command.add_argument("file", type=pathlib.Path)
    example = commands.add_parser("example")
    example.add_argument(
        "preset",
        choices=["small", "stress", "restart", "heartbeat", "lpp"],
        default="small",
        nargs="?",
    )
    builder = commands.add_parser("build")
    builder.add_argument(
        "target",
        nargs="?",
        choices=[
            "all",
            "enbility",
            "openeebus",
            "jeebus",
            "mdns-stresser",
            "netfault",
            "orchestrator",
        ],
        default="all",
    )
    starter = commands.add_parser("up")
    starter.add_argument("--port", type=int, default=8080)
    starter.add_argument("--monitoring", action="store_true")
    commands.add_parser("down")
    return parser.parse_args()


def main() -> None:
    args = parse_args()
    if args.lock:
        override = json.loads(args.lock.read_text())
        for section in ["sources", "images", "toolchains", "mavenSources"]:
            LOCK[section].update(override.get(section, {}))
        os.environ["ARENA_LOCK"] = str(args.lock.resolve())
    if args.command == "pin":
        pin_source(args.source, args.commit, args.output)
    elif args.command == "fetch":
        fetch()
    elif args.command == "prepare":
        prepare()
    elif args.command == "clean":
        clean_artifacts(cache=args.cache, docker_cache=args.docker_cache, dry_run=args.dry_run)
    elif args.command == "example":
        print_example(args.preset)
    elif args.command == "images-export":
        export_images(args.file)
    elif args.command == "images-import":
        import_images(args.file)
    elif args.command == "register":
        register()
    elif args.command == "build":
        build(args.target)
    elif args.command == "up":
        up(args.port, args.monitoring)
    elif args.command == "down":
        call("docker", "compose", "-f", str(STATE / "control.compose.json"), "down")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
