#!/usr/bin/env python3
"""Qualify native IPv4/IPv6 monitoring on isolated, controller-owned arenas."""

import argparse
import concurrent.futures
import json
import pathlib
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://127.0.0.1:8103")
    parser.add_argument(
        "--modes",
        nargs="+",
        choices=["ipv4", "ipv6", "dual-stack"],
        default=["ipv4", "ipv6", "dual-stack"],
    )
    parser.add_argument(
        "--guards", nargs="+", choices=["enbility", "openeebus"], default=["enbility", "openeebus"]
    )
    parser.add_argument("--scenario", choices=["monitoring", "lpc", "lpp"], default="monitoring")
    parser.add_argument(
        "--with-monitoring",
        action="store_true",
        help="Keep MGCP/MPC enabled during LPC/LPP regression checks",
    )
    parser.add_argument(
        "--output",
        type=pathlib.Path,
        default=pathlib.Path(".arena/verification/network-monitoring"),
    )
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)

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
        if "arenaFeatures" in variant["provenance"]:
            latest[variant["stack"]] = variant

    def exercise(mode, guard):
        name = f"{args.scenario}-{mode}-{guard}"
        groups = [
            {
                "id": "eg",
                "variant": latest[guard]["id"],
                "role": "EG",
                "count": 1,
                "monitoring": "mgcp"
                if args.scenario == "monitoring" or args.with_monitoring
                else "none",
            }
        ]
        for stack, label in [("enbility", "go"), ("openeebus", "c"), ("jeebus", "java")]:
            groups.append(
                {
                    "id": label,
                    "variant": latest[stack]["id"],
                    "role": "CS",
                    "count": 1,
                    "monitoring": "mpc"
                    if (args.scenario == "monitoring" or args.with_monitoring) and stack != "jeebus"
                    else "none",
                }
            )
        definition = {
            "schemaVersion": 3,
            "name": name,
            "seed": 42,
            "timeScale": 1,
            "scenario": args.scenario,
            "groups": groups,
            "network": {"mode": mode},
        }
        run = api("/runs", {"schemaVersion": 3, "definition": definition})["id"]
        print(name, run, flush=True)
        try:
            api(f"/runs/{run}/start", {})
            deadline = time.monotonic() + 300
            while time.monotonic() < deadline:
                state = api("/state")["runs"][run]
                if state["status"] == "complete":
                    break
                if state["status"] in ["failed", "interrupted", "stopped"]:
                    raise RuntimeError("native scenario stopped: " + str(state.get("error")))
                time.sleep(1)
            else:
                raise RuntimeError("scenario exceeded qualification deadline")
            assertions = state["assertions"]
            failures = [a for a in assertions if a["outcome"] not in ["pass", "unsupported"]]
            native = [
                a
                for a in assertions
                if a["id"].startswith("monitoring-") and a["outcome"] == "pass"
            ]
            expected = 16 if args.scenario == "monitoring" else 0
            result = {
                "run": run,
                "mode": mode,
                "guard": guard,
                "scenario": args.scenario,
                "assertions": assertions,
                "nativeMeasurementPasses": len(native),
                "snapshots": {
                    n["id"]: {
                        "stack": n["stack"],
                        "ski": n["ski"],
                        "snapshot": n["snapshot"],
                        "capabilities": n["capabilities"],
                    }
                    for n in state["nodes"]
                },
            }
            (args.output / (name + ".json")).write_text(json.dumps(result, indent=2) + "\n")
            if failures or len(native) != expected or not assertions:
                raise RuntimeError(
                    f"{name}: {len(failures)} failures, {len(native)}/{expected} native measurements: "
                    + str([(a["id"], a["detail"]) for a in failures])
                )
            print(name, "PASS", len(assertions), "assertions", flush=True)
            return result
        finally:
            api(f"/runs/{run}/stop", {})

    errors = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
        tasks = {
            pool.submit(exercise, mode, guard): (mode, guard)
            for mode in args.modes
            for guard in args.guards
        }
        for task in concurrent.futures.as_completed(tasks):
            try:
                task.result()
            except Exception as error:
                errors.append(str(error))
                print("FAIL", error, flush=True)
    if errors:
        raise SystemExit("\n".join(errors))


if __name__ == "__main__":
    main()
