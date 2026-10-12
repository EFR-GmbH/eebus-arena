#!/usr/bin/env python3
"""Run arenas with real containers and save evidence. Removes only its own arenas."""

import argparse
import collections
import datetime
import json
import pathlib
import time
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", default="http://localhost:8080")
    parser.add_argument("--stress", action="store_true")
    parser.add_argument("--duration", type=int, default=1800)
    parser.add_argument("--require-pass", action="store_true")
    parser.add_argument("--output", type=pathlib.Path, default=pathlib.Path(".arena/verification"))
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)
    args.output.chmod(0o700)

    def api(path, body=None):
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(
            args.server + "/api/v1" + path, data=data, headers={"Content-Type": "application/json"}
        )
        with urllib.request.urlopen(request, timeout=120) as response:
            return json.load(response)

    latest = {}
    for variant in sorted(api("/state")["variants"].values(), key=lambda v: v.get("builtAt", "")):
        latest[variant["stack"]] = variant
    required = {"enbility", "openeebus", "jeebus"}
    if not required.issubset(latest):
        raise RuntimeError("Build all three stacks before running integration tests")
    cases = []
    if args.stress:
        groups = [{"id": "eg", "variant": latest["enbility"]["id"], "role": "EG", "count": 1}]
        groups += [
            {"id": "cs-" + stack, "variant": latest[stack]["id"], "role": "CS", "count": count}
            for stack, count in [("enbility", 17), ("openeebus", 17), ("jeebus", 16)]
        ]
        cases.append(("51-node mixed stress", "stress", groups))
    else:
        for eg in ["enbility", "openeebus"]:
            for cs in ["enbility", "openeebus", "jeebus"]:
                for scenario in ["lpc", "lpp"]:
                    groups = [
                        {"id": "eg", "variant": latest[eg]["id"], "role": "EG", "count": 1},
                        {"id": "cs", "variant": latest[cs]["id"], "role": "CS", "count": 1},
                    ]
                    cases.append((eg + " EG → " + cs + " CS / " + scenario, scenario, groups))
    created, reports = [], []
    try:
        for name, scenario, groups in cases:
            bundle = {
                "schemaVersion": 1,
                "definition": {
                    "schemaVersion": 1,
                    "name": name,
                    "scenario": scenario,
                    "seed": 42,
                    "timeScale": 1,
                    "durationSeconds": args.duration,
                    "groups": groups,
                },
            }
            run_id = api("/runs", bundle)["id"]
            created.append(run_id)
            api("/runs/" + run_id + "/start", {})
            deadline = time.monotonic() + args.duration + 600
            while True:
                run = api("/state")["runs"][run_id]
                if run["status"] in ["complete", "failed", "interrupted", "stopped"]:
                    break
                if time.monotonic() > deadline:
                    raise RuntimeError("Run exceeded integration deadline: " + run_id)
                time.sleep(2)
            reports.append(run)
            (args.output / (run_id + ".json")).write_text(json.dumps(run, indent=2) + "\n")
            with urllib.request.urlopen(
                args.server + "/api/v1/runs/" + run_id + "/evidence", timeout=120
            ) as response:
                archive = args.output / (run_id + ".tar.gz")
                archive.write_bytes(response.read())
                archive.chmod(0o600)
            counts = collections.Counter(a["outcome"] for a in run["assertions"])
            print(name, run["status"], dict(counts), flush=True)
            api("/runs/" + run_id + "/stop", {})
            created.remove(run_id)
    finally:
        for run_id in created:
            try:
                api("/runs/" + run_id + "/stop", {})
            except Exception as error:
                print("Cleanup failed for", run_id, error, flush=True)
    summary = {"at": datetime.datetime.now(datetime.UTC).isoformat(), "runs": reports}
    (args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    runtime_ok = all(
        r["status"] == "complete"
        and all(n["lifecycle"] == "running" and n["capabilities"] for n in r["nodes"])
        for r in reports
    )
    protocol_ok = all(
        r["assertions"] and all(a["outcome"] == "pass" for a in r["assertions"]) for r in reports
    )
    if not runtime_ok or args.require_pass and not protocol_ok:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
