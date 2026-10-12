#!/usr/bin/env python3
"""Qualify throttling and a bounded native OOM in an owned 32 MiB node."""

import argparse
import json
import pathlib
import subprocess
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://localhost:8080")
    parser.add_argument(
        "--output", type=pathlib.Path, default=pathlib.Path(".arena/verification/expansion")
    )
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)
    args.output.chmod(0o700)
    base = args.server.rstrip("/") + "/api/v1"

    def api(path, body=None):
        request = urllib.request.Request(
            base + path,
            data=json.dumps(body).encode() if body is not None else None,
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(error.read().decode()) from error

    latest = {}
    for variant in sorted(
        api("/state")["variants"].values(), key=lambda variant: variant.get("builtAt", "")
    ):
        latest[variant["stack"]] = variant["id"]
    groups = [
        {
            "id": "eg",
            "variant": latest["enbility"],
            "role": "EG",
            "count": 1,
            "resources": {"cpuMillis": 500, "memoryMiB": 128},
        },
        {
            "id": "cs",
            "variant": latest["openeebus"],
            "role": "CS",
            "count": 1,
            "resources": {"cpuMillis": 100, "memoryMiB": 32},
        },
        {
            "id": "noise",
            "variant": latest["mdns-stresser"],
            "role": "STRESSER",
            "count": 1,
            "resources": {"cpuMillis": 1000, "memoryMiB": 128},
        },
    ]
    run = api(
        "/runs",
        {
            "schemaVersion": 3,
            "definition": {
                "schemaVersion": 3,
                "name": "Quota and OOM qualification",
                "scenario": "manual",
                "timeScale": 1,
                "seed": 42,
                "durationSeconds": 180,
                "groups": groups,
            },
        },
    )["id"]
    print(run, flush=True)
    try:
        api("/runs/" + run + "/start", {})
        time.sleep(7)
        api(
            "/runs/" + run + "/nodes/noise-001/commands",
            {
                "command": "mdns-stress.start",
                "params": {
                    "profile": {
                        "version": 1,
                        "seed": 42,
                        "services": 1000,
                        "rate": 1000,
                        "burst": 100,
                        "churn": 100,
                        "ttl": 120,
                        "durationSeconds": 25,
                        "pattern": "advertise",
                        "service": "_ship._tcp",
                    }
                },
            },
        )
        time.sleep(15)
        before = api("/state")["runs"][run]
        node = next(node for node in before["nodes"] if node["id"] == "cs-001")
        container = node["container"]
        info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
        labels = info["Config"]["Labels"]
        if (
            labels.get("org.eebus-arena.run") != run
            or labels.get("org.eebus-arena.node") != "cs-001"
        ):
            raise RuntimeError("Bounded allocation target ownership changed")
        # Only this temporary 32 MiB node receives the larger tmpfs write.
        allocation = subprocess.run(
            [
                "docker",
                "exec",
                container,
                "sh",
                "-c",
                "dd if=/dev/zero of=/dev/shm/arena-oom bs=1M count=48; rm -f /dev/shm/arena-oom",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        print("Bounded allocation exit", allocation.returncode, flush=True)
        time.sleep(7)
        history = api("/runs/" + run + "/resources?node=cs-001&points=240")
        timeline = api("/runs/" + run + "/timeline?node=cs-001&prefix=resource.&limit=500")
        info = json.loads(subprocess.check_output(["docker", "inspect", container], text=True))[0]
        peaks = {
            series["metric"]: series["peak"]
            for series in history["series"]
            if not series.get("processId") and series["source"] == "cgroup"
        }
        result = {
            "id": run,
            "peakCounters": peaks,
            "runtime": node.get("effectiveResources"),
            "oomKilled": info["State"]["OOMKilled"],
            "allocationExit": allocation.returncode,
            "resourceEvents": timeline["events"],
        }
        destination = args.output / "constraints-summary.json"
        destination.write_text(json.dumps(result, indent=2) + "\n")
        destination.chmod(0o600)
        print("Peak counters", peaks, "Docker OOMKilled", info["State"]["OOMKilled"], flush=True)
        if peaks.get("cpuThrottledSeconds", 0) <= 0:
            raise RuntimeError("No native CPU throttling evidence")
        if not (peaks.get("oomKills", 0) > 0 or info["State"]["OOMKilled"]):
            raise RuntimeError("No native OOM evidence")
    finally:
        api("/runs/" + run + "/stop", {})


if __name__ == "__main__":
    main()
