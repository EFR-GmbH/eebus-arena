#!/usr/bin/env python3
"""Validate mDNS family counters and peer-specific IPv6 netem cleanup."""

import argparse
import ipaddress
import json
import pathlib
import subprocess
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://127.0.0.1:8103")
    parser.add_argument(
        "--output",
        type=pathlib.Path,
        default=pathlib.Path(".arena/verification/network-monitoring"),
    )
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)

    def api(path, body=None, method=None):
        with urllib.request.urlopen(
            urllib.request.Request(
                args.server + "/api/v1" + path,
                data=None if body is None else json.dumps(body).encode(),
                headers={"Content-Type": "application/json"},
                method=method,
            ),
            timeout=120,
        ) as response:
            return json.load(response)

    latest = {}
    for v in sorted(api("/state")["variants"].values(), key=lambda v: v.get("builtAt", "")):
        if "arenaFeatures" in v["provenance"]:
            latest[v["stack"]] = v
    for mode in ("ipv4", "ipv6", "dual-stack"):
        verify_network_mode(api, latest, mode, args.output)


def verify_network_mode(api, latest, mode, output):
    """Keep each run's callbacks scoped to its own network mode and run ID."""
    d = {
        "schemaVersion": 3,
        "name": "traffic-" + mode,
        "seed": 42,
        "timeScale": 1,
        "scenario": "manual",
        "network": {"mode": mode},
        "groups": [
            {"id": name, "variant": latest[stack]["id"], "role": role, "count": 1}
            for name, stack, role in [
                ("eg", "enbility", "EG"),
                ("cs", "openeebus", "CS"),
                ("noise", "mdns-stresser", "STRESSER"),
            ]
        ],
    }
    run = api("/runs", {"schemaVersion": 3, "definition": d})["id"]
    print(mode, run, flush=True)

    def state():
        return api("/state")["runs"][run]

    def until(predicate, seconds=30):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            s = state()
            if predicate(s):
                return s
            time.sleep(0.25)
        raise RuntimeError(mode + " observation deadline exceeded")

    def command(node, cmd, params):
        result = api(f"/runs/{run}/nodes/{node}/commands", {"command": cmd, "params": params})
        if result["data"].get("ok") is not True:
            raise RuntimeError(result)
        return result

    try:
        api(f"/runs/{run}/start", {})
        until(
            lambda s: s["status"] == "running" and all(n["capabilities"] for n in s["nodes"]),
            90,
        )
        report = {"run": run, "mode": mode, "traffic": [], "faults": []}
        for family in [mode] if mode != "dual-stack" else ["ipv4", "ipv6", "dual-stack"]:
            profile = {
                "version": 1,
                "family": family,
                "seed": 42,
                "services": 20,
                "rate": 40,
                "burst": 2,
                "churn": 20,
                "ttl": 120,
                "durationSeconds": 3,
                "pattern": "advertise",
                "service": "_shippairing._tcp",
            }
            command("noise-001", "mdns-stress.start", {"profile": profile})
            time.sleep(4)
            sample = command("noise-001", "mdns-stress.status", {})["data"]["mdns"]
            enabled = ["ipv4", "ipv6"] if family == "dual-stack" else [family]
            if (
                sample["sendErrors"] != 0
                or not 60 <= sample["sent"] <= 160
                or any(sample["families"][f]["sent"] == 0 for f in enabled)
            ):
                raise RuntimeError("mDNS socket traffic failed: " + str(sample))
            if (
                family == "dual-stack"
                and abs(sample["families"]["ipv4"]["sent"] - sample["families"]["ipv6"]["sent"]) > 1
            ):
                raise RuntimeError("dual-stack packet budget is not shared evenly")
            report["traffic"].append(sample)
        for family in ["ipv4", "ipv6"] if mode == "dual-stack" else [mode]:
            for traffic in ["unicast", "mdns"]:
                command(
                    "noise-001",
                    "mdns-stress.start",
                    {"profile": {**profile, "family": family, "durationSeconds": 8}},
                )
                sender = "noise-001" if traffic == "mdns" else "eg-001"
                lease = api(
                    f"/runs/{run}/faults",
                    {
                        "from": sender,
                        "to": "cs-001",
                        "family": family,
                        "traffic": traffic,
                        "partition": True,
                        "durationSeconds": 5,
                    },
                )
                until(
                    lambda s, lease_id=lease["id"]: any(
                        f["id"] == lease_id and f["state"] == "applied"
                        for f in s.get("runtime", {}).get("faults", [])
                    )
                )
                if traffic == "unicast":
                    current = state()
                    destination = next(n for n in current["nodes"] if n["id"] == "cs-001")
                    source = next(n for n in current["nodes"] if n["id"] == "eg-001")
                    container = json.loads(
                        subprocess.check_output(
                            ["docker", "inspect", destination["container"]], text=True
                        )
                    )[0]
                    network = next(iter(container["NetworkSettings"]["Networks"].values()))
                    peer = str(
                        ipaddress.ip_address(
                            network["GlobalIPv6Address"]
                            if family == "ipv6"
                            else network["IPAddress"]
                        )
                    )
                    helper = json.loads(
                        (
                            pathlib.Path(__file__).resolve().parents[1] / ".arena/netfault.json"
                        ).read_text()
                    )["image"]
                    subprocess.run(
                        [
                            "docker",
                            "run",
                            "--rm",
                            "--network",
                            "container:" + source["container"],
                            "--cap-drop",
                            "ALL",
                            "--read-only",
                            "--memory",
                            "32m",
                            "--pids-limit",
                            "16",
                            "--entrypoint",
                            "timeout",
                            helper,
                            "3",
                            "bash",
                            "-c",
                            f"exec 3<>/dev/tcp/{peer}/4712",
                        ],
                        capture_output=True,
                        timeout=10,
                        check=False,
                    )
                until(lambda s: not s.get("runtime", {}).get("faults", []))
                events = api(f"/runs/{run}/timeline?prefix=fault.&limit=500")["events"]
                stats = [
                    e["data"]
                    for e in events
                    if e["type"] == "fault.statistics" and e["data"]["id"] == lease["id"]
                ]
                dropped = max(
                    (
                        q.get("drops", 0)
                        for observation in stats
                        for q in observation.get("qdisc", [])
                        if q["kind"] == "netem"
                    ),
                    default=0,
                )
                if dropped == 0:
                    raise RuntimeError(
                        f"{mode} {family} {traffic}: filter did not drop any target packets"
                    )
                command("noise-001", "mdns-stress.stop", {})
                # Native helper observations are retained in the controller journal.
                report["faults"].append(
                    {
                        "family": family,
                        "traffic": traffic,
                        "lease": lease["id"],
                        "restored": True,
                        "nativeDrops": dropped,
                    }
                )
        (output / ("traffic-" + mode + ".json")).write_text(json.dumps(report, indent=2) + "\n")
        print(mode, "PASS", flush=True)
    finally:
        api(f"/runs/{run}/stop", {})


if __name__ == "__main__":
    main()
