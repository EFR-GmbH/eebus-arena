#!/usr/bin/env python3
"""Exercise retained resources, campaign scheduling and exploration on owned arenas."""

import argparse
import collections
import json
import pathlib
import statistics
import subprocess
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://localhost:8080")
    parser.add_argument(
        "--suite",
        choices=["resources", "collection", "campaign", "explore", "faults"],
        default="resources",
    )
    parser.add_argument(
        "--output", type=pathlib.Path, default=pathlib.Path(".arena/verification/expansion")
    )
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)
    args.output.chmod(0o700)

    def api(path, body=None, method=None):
        request = urllib.request.Request(
            args.server + "/api/v1" + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Content-Type": "application/json"},
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(error.read().decode()) from error

    latest = {}
    for variant in sorted(api("/state")["variants"].values(), key=lambda v: v.get("builtAt", "")):
        latest[variant["stack"]] = variant

    def group(name, stack, role, count=1):
        return {
            "id": name,
            "variant": latest[stack]["id"],
            "role": role,
            "count": count,
            "resources": {"cpuMillis": 1000, "memoryMiB": 256, "pidsLimit": 128},
        }

    def command(name, target, cmd, params):
        return {
            "name": name,
            "target": target,
            "action": "command",
            "command": cmd,
            "params": params,
        }

    def wait(name, seconds):
        return {"name": name, "action": "wait", "seconds": seconds}

    groups = [
        group("eg", "enbility", "EG"),
        group("cs", "openeebus" if args.suite == "resources" else "enbility", "CS"),
    ]
    definition = {
        "schemaVersion": 3,
        "name": args.suite + " expansion qualification",
        "seed": 42,
        "timeScale": 1,
        "scenario": "manual",
        "durationSeconds": 180,
        "groups": groups,
    }
    profile = {
        "version": 1,
        "seed": 42,
        "services": 1000,
        "rate": 1000,
        "burst": 100,
        "churn": 100,
        "ttl": 120,
        "durationSeconds": 45,
        "pattern": "advertise",
        "service": "_ship._tcp",
    }
    if args.suite == "collection":
        groups[:] = [
            group("eg", "enbility", "EG"),
            group("go", "enbility", "CS", 17),
            group("c", "openeebus", "CS", 17),
            group("java", "jeebus", "CS", 16),
        ]
    elif args.suite in ["resources", "campaign", "faults"]:
        groups.append(group("noise", "mdns-stresser", "STRESSER"))
        if args.suite == "resources":
            definition["scenario"] = "custom"
            groups[1]["resources"]["cpuMillis"] = 500
            definition["steps"] = [
                wait("Baseline", 15),
                command(
                    "Start controlled mDNS ramp",
                    "noise-001",
                    "mdns-stress.start",
                    {"profile": profile},
                ),
                wait("Measure load", 30),
                command("Stop mDNS load", "noise-001", "mdns-stress.stop", {}),
                wait("Recovery", 15),
                {
                    "name": "Avahi recovery measurement",
                    "target": "cs-001",
                    "action": "resource",
                    "resource": {
                        "component": "Avahi",
                        "metric": "cpuPercent",
                        "max": 100,
                        "windowSeconds": 5,
                    },
                },
            ]
        elif args.suite == "campaign":
            definition["scenario"] = "campaign"
            definition["campaign"] = {
                "lanes": [
                    {
                        "id": "load",
                        "domain": "real",
                        "steps": [
                            command(
                                "Start mDNS", "noise-001", "mdns-stress.start", {"profile": profile}
                            ),
                            wait("Load window", 15),
                            command("Stop mDNS", "noise-001", "mdns-stress.stop", {}),
                        ],
                    },
                    {
                        "id": "restart",
                        "domain": "real",
                        "offsetSeconds": 3,
                        "steps": [
                            {
                                "name": "Restart receiver",
                                "target": "cs-001",
                                "action": "lifecycle",
                                "command": "restart",
                            }
                        ],
                    },
                    {
                        "id": "recovery",
                        "after": ["load", "restart"],
                        "domain": "real",
                        "steps": [wait("Settle", 3)],
                    },
                ]
            }
    run = api("/runs", {"schemaVersion": 3, "definition": definition})["id"]
    print(args.suite, run, flush=True)
    try:
        if args.suite == "explore":
            bundle = api("/runs/" + run + "/export")
            # Created setups have no processes and can be reversibly archived.
            api("/runs/" + run, {}, "DELETE")
            ski = bundle["identities"]["eg-001"]["ski"]
            bundle["definition"].update(
                scenario="explore",
                exploration={
                    "rounds": 8,
                    "settleSeconds": 2,
                    "actions": [
                        command("Fast heartbeat", "cs-001", "heartbeat.interval", {"seconds": 1}),
                        command("Slow heartbeat", "cs-001", "heartbeat.interval", {"seconds": 2}),
                    ],
                    "invariants": [
                        {
                            "name": "Native SHIP remains connected",
                            "target": "cs-001",
                            "action": "assert",
                            "expect": {
                                "path": ["peers", ski],
                                "equals": "connected",
                                "withinSeconds": 15,
                                "realTime": True,
                            },
                        }
                    ],
                },
            )
            run = api("/runs", bundle)["id"]
            print("Seeded model", run, flush=True)
        api("/runs/" + run + "/start", {})
        fault_checks = []
        if args.suite == "faults":
            time.sleep(4)
            api(
                "/runs/" + run + "/nodes/noise-001/commands",
                {"command": "mdns-stress.start", "params": {"profile": profile}},
            )
            api(
                "/runs/" + run + "/nodes/cs-001/commands",
                {"command": "heartbeat.interval", "params": {"seconds": 1}},
            )
            for label, params in [
                (
                    "delay-jitter-loss-bandwidth",
                    {"delayMillis": 100, "jitterMillis": 20, "lossPercent": 5, "rateKbit": 1000},
                ),
                ("terminated-helper", {"partition": True}),
                (
                    "mdns-ingress",
                    {"lossPercent": 50, "traffic": "mdns", "from": "noise-001", "to": "cs-001"},
                ),
            ]:
                fault = {
                    "from": "cs-001",
                    "to": "eg-001",
                    "traffic": "unicast",
                    "durationSeconds": 6,
                    **params,
                }
                lease = api("/runs/" + run + "/faults", fault)
                expires = time.monotonic() + 20
                applied = False
                killed = False
                while time.monotonic() < expires:
                    timeline = api("/runs/" + run + "/timeline?prefix=fault.&limit=500")["events"]
                    events = [e for e in timeline if e.get("data", {}).get("id") == lease["id"]]
                    applied = applied or any(e["type"] == "fault.applied" for e in events)
                    if label == "terminated-helper" and applied and not killed:
                        info = json.loads(
                            subprocess.check_output(
                                ["docker", "inspect", lease["helper"]], text=True
                            )
                        )[0]
                        labels = info["Config"]["Labels"]
                        if (
                            labels.get("org.eebus-arena.helper.run") != run
                            or labels.get("org.eebus-arena.fault") != lease["id"]
                        ):
                            raise RuntimeError("Fault helper ownership changed")
                        subprocess.run(
                            ["docker", "kill", lease["helper"]],
                            check=True,
                            stdout=subprocess.DEVNULL,
                        )
                        killed = True
                    restored = any(e["type"] == "fault.restored" for e in events)
                    remaining = api("/state")["runs"][run].get("runtime", {}).get("faults", [])
                    if restored and not any(f["id"] == lease["id"] for f in remaining):
                        break
                    time.sleep(0.25)
                else:
                    raise RuntimeError(label + " did not restore its lease")
                if not applied:
                    raise RuntimeError(label + " has no native application evidence")
                fault_checks.append(
                    {"profile": label, "applied": applied, "terminated": killed, "events": events}
                )
                print(label, "applied and restored", flush=True)
            api(
                "/runs/" + run + "/nodes/noise-001/commands",
                {"command": "mdns-stress.stop", "params": {}},
            )
        deadline = time.monotonic() + 240
        observed = []
        while time.monotonic() < deadline:
            state = api("/state")
            snapshot = state["runs"][run]
            if args.suite == "faults":
                break
            if args.suite == "collection":
                observed.append(snapshot)
                if len(observed) >= 40:
                    break
            elif snapshot["status"] in ["complete", "failed", "interrupted"]:
                break
            time.sleep(1)
        else:
            raise RuntimeError("Qualification deadline exceeded")
        history = {}
        if args.suite == "collection":
            samples = [n.get("processMetrics", {}) for r in observed for n in r["nodes"]]
            costs = [s["collectionMillis"] for s in samples if "collectionMillis" in s]
            intervals = [s["intervalSeconds"] for s in samples if s.get("intervalSeconds", 0) > 0]
            collectors = [
                p["cpuPercent"]
                for s in samples
                for p in s.get("processes", [])
                if p.get("component") == "Collector" and "cpuPercent" in p
            ]

            def distribution(values):
                return (
                    {
                        "count": len(values),
                        "median": statistics.median(values),
                        "p95": sorted(values)[int((len(values) - 1) * 0.95)],
                        "max": max(values),
                    }
                    if values
                    else None
                )

            history = {
                "nodes": len(snapshot["nodes"]),
                "collectionMillis": distribution(costs),
                "intervalSeconds": distribution(intervals),
                "collectorCPUPercent": distribution(collectors),
                "controlMetrics": state.get("controlMetrics"),
            }
        else:
            for n in snapshot["nodes"]:
                history[n["id"]] = api(
                    "/runs/" + run + "/resources?node=" + n["id"] + "&points=240"
                )
        summary = {
            "suite": args.suite,
            "id": run,
            "status": snapshot["status"],
            "assertions": snapshot["assertions"],
            "analysis": history,
            "faultChecks": fault_checks,
            "workloads": {
                n["id"]: n.get("snapshot", {}).get("mdns")
                for n in snapshot["nodes"]
                if n.get("role") == "STRESSER"
            },
        }
        (args.output / (args.suite + "-summary.json")).write_text(
            json.dumps(summary, indent=2) + "\n"
        )
        print(
            snapshot["status"],
            dict(collections.Counter(a["outcome"] for a in snapshot["assertions"])),
            flush=True,
        )
        print("Resource evidence saved", flush=True)
        if snapshot["status"] in ["failed", "interrupted"] or any(
            a["outcome"] in ["fail", "inconclusive"] for a in snapshot["assertions"]
        ):
            raise RuntimeError("Qualification did not pass; inspect the saved summary")
    finally:
        api("/runs/" + run + "/stop", {})
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            if api("/state")["runs"][run]["status"] in ["stopped", "failed"]:
                break
            time.sleep(0.5)
        with urllib.request.urlopen(
            args.server + "/api/v1/runs/" + run + "/evidence", timeout=120
        ) as response:
            path = args.output / (run + ".tar.gz")
            path.write_bytes(response.read())
            path.chmod(0o600)


if __name__ == "__main__":
    main()
