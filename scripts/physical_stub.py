#!/usr/bin/env python3
"""Build and deploy one temporary, native enbility EG/CS stub over existing SSH."""

import argparse
import hashlib
import json
import os
import pathlib
import re
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("host", help="Existing SSH host/alias")
    parser.add_argument(
        "--ssh-config", type=pathlib.Path, default=pathlib.Path.home() / ".ssh/config"
    )
    parser.add_argument("--address", required=True, help="Device management IPv4 address")
    parser.add_argument("--arch", choices=["arm", "arm64", "amd64"], default="arm")
    parser.add_argument(
        "--output",
        type=pathlib.Path,
        required=True,
        help="Private deployment files, including token",
    )
    args = parser.parse_args()
    if not re.fullmatch(r"[a-zA-Z0-9_.@-]+", args.host):
        parser.error("Use a simple existing SSH host/alias")
    import ipaddress

    ipaddress.IPv4Address(args.address)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True, mode=0o700)
    if any(output.iterdir()):
        parser.error("Output must be empty; preserve existing transport identity when updating")
    lock = json.loads((ROOT / "recipes/lock.json").read_text())
    source = ROOT / ".arena/generated/enbility"
    if not (source / "provenance.json").is_file():
        parser.error("Prepare the pinned sources first: python3 scripts/arena.py prepare")
    go = lock["images"]["go"]
    env = ["-e", "GOOS=linux", "-e", "GOARCH=" + args.arch, "-e", "CGO_ENABLED=0"]
    if args.arch == "arm":
        env += ["-e", "GOARM=7"]
    user = [
        "--user",
        str(os.getuid()) + ":" + str(os.getgid()),
        "-e",
        "GOCACHE=/tmp/go-build",
        "-e",
        "GOMODCACHE=/tmp/go-mod",
    ]
    common = [
        "docker",
        "run",
        "--rm",
        "--platform",
        lock["toolchains"]["platform"],
        *user,
        *env,
        "-v",
        str(output) + ":/out",
        "-v",
        str(ROOT) + ":/repo:ro",
    ]
    # Each source tree gets an isolated writable module cache; source is read-only.
    for work, package, name in [
        ("/repo/.arena/generated/enbility/adapter", ".", "arena-node"),
        ("/repo", "./cmd/arena-supervisor", "arena-supervisor"),
        ("/repo", "./cmd/arena-agent", "arena-agent"),
    ]:
        run(
            *common,
            "-w",
            work,
            go,
            "go",
            "build",
            "-mod=readonly",
            "-trimpath",
            "-ldflags=-s -w",
            "-o",
            "/out/" + name,
            package,
        )
    # Provision TLS with the host's UTC; embedded lab clocks may still be 1970.
    run(
        "docker",
        "run",
        "--rm",
        "--platform",
        lock["toolchains"]["platform"],
        *user,
        "-v",
        str(output) + ":/out",
        "-v",
        str(ROOT) + ":/repo:ro",
        "-w",
        "/repo",
        go,
        "go",
        "run",
        "./cmd/arena-agent",
        "-init",
        "-root",
        "/out",
    )
    manifest = {
        "nativeSHA256": hashlib.sha256((output / "arena-node").read_bytes()).hexdigest(),
        "arch": args.arch,
        "nativeProvenance": json.loads((source / "provenance.json").read_text()),
    }
    (output / "deployment.json").write_text(json.dumps(manifest, indent=2) + "\n")
    ssh = [
        "ssh",
        "-F",
        str(args.ssh_config),
        "-o",
        "BatchMode=yes",
        "-o",
        "ConnectTimeout=8",
        args.host,
    ]
    # Refuse to overwrite an existing agent or firmware installation.
    run(*ssh, "test ! -e /tmp/arena-stub && mkdir -m 700 /tmp/arena-stub")
    run(
        "scp",
        "-F",
        str(args.ssh_config),
        *[
            str(output / name)
            for name in [
                "arena-node",
                "arena-supervisor",
                "arena-agent",
                "agent-cert.pem",
                "agent-key.pem",
                "token",
                "deployment.json",
            ]
        ],
        args.host + ":/tmp/arena-stub/",
    )
    run(
        *ssh,
        "chmod 700 /tmp/arena-stub/arena-* && chmod 600 /tmp/arena-stub/*.pem /tmp/arena-stub/token /tmp/arena-stub/deployment.json",
    )
    run(
        *ssh,
        "nohup /tmp/arena-stub/arena-agent -listen "
        + args.address
        + ":8843 >/tmp/arena-stub/agent.log 2>&1 </dev/null & echo $! >/tmp/arena-stub/agent.pid",
    )
    print("Agent: https://" + args.address + ":8843; private token: " + str(output / "token"))


if __name__ == "__main__":
    main()
