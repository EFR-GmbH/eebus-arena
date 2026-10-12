#!/usr/bin/env python3
"""Pairing and mDNS suites against an Arena controller; only cleans up its own runs."""

import argparse
import collections
import json
import pathlib
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://localhost:8080")
    parser.add_argument(
        "--suite",
        choices=[
            "smoke",
            "negative",
            "recovery",
            "replacement",
            "startup",
            "multi-target",
            "pairing-load",
            "mdns",
        ],
        default="smoke",
    )
    parser.add_argument("--eg-variant")
    parser.add_argument("--cs-variant")
    parser.add_argument("--duration", type=int, default=60)
    parser.add_argument("--require-pass", action="store_true")
    parser.add_argument(
        "--output", type=pathlib.Path, default=pathlib.Path(".arena/verification/pairing")
    )
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)

    def api(path, body=None):
        request = urllib.request.Request(
            args.server + "/api/v1" + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(error.read().decode()) from error

    variants = api("/state")["variants"]
    latest = {}
    for v in sorted(variants.values(), key=lambda v: v.get("builtAt", "")):
        latest[v["stack"]] = v
    if "enbility" not in latest:
        raise RuntimeError("Build the enbility adapter first")
    eg = variants[args.eg_variant] if args.eg_variant else latest["enbility"]
    cs = variants[args.cs_variant] if args.cs_variant else latest["enbility"]

    def command(name, target, cmd, params=None, expect=None):
        step = {
            "name": name,
            "target": target,
            "action": "command",
            "command": cmd,
            "params": params or {},
        }
        if expect:
            step["expect"] = expect
        return step

    def assertion(name, target, path, value, seconds=90):
        return {
            "name": name,
            "target": target,
            "action": "assert",
            "expect": {"path": path, "equals": value, "withinSeconds": seconds, "realTime": True},
        }

    def announce(target="eg-001", fault=""):
        return command(
            "Announce " + (fault or "valid pairing"),
            target,
            "pairing.announce",
            {"targetNode": "cs-001", "credentialFault": fault},
            {
                "target": "cs-001",
                "event": "pairing.validation" if fault else "pairing.trust",
                "match": {"ok": False} if fault else {"trusted": True},
                "withinSeconds": 90,
                "realTime": True,
            },
        )

    cases = []
    if args.suite == "smoke":
        if args.eg_variant or args.cs_variant:
            cases = [("pairing smoke", eg, cs, "pairing", [])]
        else:
            stacks = [latest[s] for s in ["enbility", "openeebus"] if s in latest]
            cases = [
                (a["stack"] + " to " + b["stack"], a, b, "pairing", [])
                for a in stacks
                for b in stacks
            ]
    elif args.suite == "startup":
        cases = [
            (
                "announcer starts before listener",
                eg,
                cs,
                "custom",
                [
                    {
                        "name": "Stop listener",
                        "target": "cs-001",
                        "action": "lifecycle",
                        "command": "stop",
                    },
                    command(
                        "Announce while listener is offline",
                        "eg-001",
                        "pairing.announce",
                        {"targetNode": "cs-001"},
                    ),
                    {
                        "name": "Late listener starts",
                        "target": "cs-001",
                        "action": "lifecycle",
                        "command": "start",
                    },
                    assertion(
                        "Late listener accepted pairing",
                        "cs-001",
                        ["pairing", "listener", "active"],
                        False,
                    ),
                ],
            )
        ]
    elif args.suite == "multi-target":
        cases = [("one announcer multiple listeners", eg, cs, "pairing", [])]
    elif args.suite == "pairing-load":
        for pattern, service in [
            ("advertise", "_ship._tcp"),
            ("invalid", "_shippairing._tcp"),
            ("query", "_shippairing._tcp"),
        ]:
            profile = {
                "version": 1,
                "seed": 42,
                "services": 1000,
                "rate": 100,
                "burst": 100,
                "churn": 20,
                "ttl": 120,
                "durationSeconds": 300,
                "pattern": pattern,
                "service": service,
            }
            cases.append(
                (
                    "fresh pairing during " + pattern + " " + service,
                    eg,
                    cs,
                    "custom",
                    [
                        command(
                            "Start discovery load",
                            "noise-001",
                            "mdns-stress.start",
                            {"profile": profile},
                        ),
                        announce(),
                    ],
                )
            )
    elif args.suite == "negative":
        for fault in ["wrong-secret", "wrong-target-id", "wrong-fingerprint"]:
            cases.append(
                (
                    fault,
                    eg,
                    cs,
                    "custom",
                    [
                        announce(fault=fault),
                        assertion("No learned trust", "cs-001", ["pairing", "trusted"], {}, 3),
                    ],
                )
            )
    elif args.suite == "recovery":
        # Snapshot trust/transport checks are generated after stable node SKIs are known.
        cases = [("durable pairing recovery", eg, cs, "custom", [announce()])]
    elif args.suite == "replacement":
        cases = [("real-time control-unit replacement", eg, cs, "custom", [announce()])]
    else:
        cases = [("pairing under mDNS load", eg, cs, "mdns-stress", [])]
    reports = []
    for title, a, b, scenario, steps in cases:
        groups = [
            {"id": "eg", "variant": a["id"], "role": "EG", "count": 1},
            {"id": "cs", "variant": b["id"], "role": "CS", "count": 1},
        ]
        links = [{"from": "eg", "to": "cs", "mode": "ship-pairing"}]
        if args.suite == "multi-target":
            groups[1]["count"] = 2
        if args.suite == "replacement":
            groups.append({"id": "replacement", "variant": a["id"], "role": "EG", "count": 1})
            links.append({"from": "replacement", "to": "cs", "mode": "ship-pairing"})
            steps += [
                command(
                    "Competing announcement",
                    "replacement-001",
                    "pairing.announce",
                    {"targetNode": "cs-001"},
                    {
                        "target": "cs-001",
                        "event": "pairing.trust",
                        "match": {"trusted": True},
                        "absent": True,
                        "withinSeconds": 5,
                    },
                ),
                command(
                    "Withdraw competing announcement",
                    "replacement-001",
                    "pairing.stop",
                    {"targetNode": "cs-001"},
                ),
                {
                    "name": "Original goes offline and native window expires",
                    "target": "eg-001",
                    "action": "lifecycle",
                    "command": "stop",
                    "expect": {
                        "target": "cs-001",
                        "event": "pairing.replacement",
                        "match": {"state": "expired"},
                        "withinSeconds": 1000,
                        "realTime": True,
                    },
                },
                announce("replacement-001"),
            ]
        if args.suite in ["mdns", "pairing-load"]:
            if "mdns-stresser" not in latest:
                raise RuntimeError("Build mdns-stresser first")
            groups.append(
                {
                    "id": "noise",
                    "variant": latest["mdns-stresser"]["id"],
                    "role": "STRESSER",
                    "count": 1,
                }
            )
        definition = {
            "schemaVersion": 2,
            "name": title,
            "seed": 42,
            "timeScale": 1,
            "scenario": scenario,
            "durationSeconds": args.duration,
            "groups": groups,
            "links": links,
        }
        if steps:
            definition["steps"] = steps
        # Provision once to obtain fixed identities; import the setup with the completed steps.
        staged = api("/runs", {"schemaVersion": 2, "definition": definition})["id"]
        bundle = api("/runs/" + staged + "/export")
        # Created setups have no running resources; keep provisioning out of cleanup ownership lists.
        request = urllib.request.Request(
            args.server + "/api/v1/runs/" + staged,
            method="DELETE",
            data=b"{}",
            headers={"Content-Type": "application/json"},
        )
        urllib.request.urlopen(request).close()
        ski = {n: i["ski"] for n, i in bundle["identities"].items()}
        if args.suite == "pairing-load":
            profile = steps[0]["params"]["profile"]
            profile.update(
                forId=bundle["pairing"]["cs-001"]["shipId"],
                forPar=bundle["pairing"]["cs-001"]["fingerprint"],
            )
            steps += [
                assertion(
                    "SHIP connects under load", "cs-001", ["peers", ski["eg-001"]], "connected"
                )
            ]
            for uc in ["lpc", "lpp"]:
                steps += [
                    assertion(
                        uc + " discovered under load",
                        "eg-001",
                        ["partners", uc, ski["cs-001"]],
                        True,
                    ),
                    command(
                        uc + " write under load",
                        "eg-001",
                        "limit.set",
                        {
                            "usecase": uc,
                            "targetNode": "cs-001",
                            "value": 1200 if uc == "lpc" else -1200,
                            "active": True,
                            "durationSeconds": 60,
                        },
                        {
                            "target": "cs-001",
                            "path": [uc, "active"],
                            "equals": True,
                            "withinSeconds": 30,
                        },
                    ),
                ]
            steps += [command("Stop discovery load", "noise-001", "mdns-stress.stop")]
        if args.suite == "startup":
            steps += [
                assertion("Late listener connects", "cs-001", ["peers", ski["eg-001"]], "connected")
            ]
        if args.suite == "recovery":
            for node, other in [("cs-001", "eg-001"), ("eg-001", "cs-001")]:
                steps += [
                    {
                        "name": "Restart " + node,
                        "target": node,
                        "action": "lifecycle",
                        "command": "restart",
                    },
                    assertion("Reconnected " + node, node, ["peers", ski[other]], "connected"),
                ]
            steps += [
                assertion(
                    "Replay window retained", "cs-001", ["pairing", "listener", "active"], False
                )
            ]
        if args.suite == "replacement":
            steps += [
                assertion(
                    "Replacement connects", "cs-001", ["peers", ski["replacement-001"]], "connected"
                ),
                {
                    "name": "Old peer cannot reconnect",
                    "target": "eg-001",
                    "action": "lifecycle",
                    "command": "start",
                    "expect": {
                        "target": "cs-001",
                        "event": "ship.state",
                        "match": {"ski": ski["eg-001"], "state": "connected"},
                        "absent": True,
                        "withinSeconds": 30,
                        "realTime": True,
                    },
                },
            ]
        if steps:
            bundle["definition"]["steps"] = steps
        run_id = api("/runs", bundle)["id"]
        print(title, run_id, flush=True)
        try:
            api("/runs/" + run_id + "/start", {})
            deadline = time.monotonic() + args.duration + 1800
            while True:
                run = api("/state")["runs"][run_id]
                if run["status"] in ["complete", "failed", "interrupted", "stopped"]:
                    break
                if time.monotonic() > deadline:
                    raise RuntimeError("Suite deadline exceeded: " + run_id)
                time.sleep(1)
            reports.append(run)
            print(
                run["status"],
                dict(collections.Counter(x["outcome"] for x in run["assertions"])),
                flush=True,
            )
        finally:
            api("/runs/" + run_id + "/stop", {})
            run = api("/state")["runs"][run_id]
            (args.output / (run_id + ".json")).write_text(json.dumps(run, indent=2) + "\n")
            with urllib.request.urlopen(
                args.server + "/api/v1/runs/" + run_id + "/evidence", timeout=120
            ) as response:
                path = args.output / (run_id + ".tar.gz")
                path.write_bytes(response.read())
                path.chmod(0o600)
    (args.output / "summary.json").write_text(json.dumps(reports, indent=2) + "\n")
    if (
        any(r["status"] != "complete" for r in reports)
        or args.require_pass
        and any(
            not r["assertions"] or any(a["outcome"] != "pass" for a in r["assertions"])
            for r in reports
        )
    ):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
