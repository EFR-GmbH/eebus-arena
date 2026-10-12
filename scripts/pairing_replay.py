#!/usr/bin/env python3
"""Replay a captured native advertisement after a listener restart and native timeout."""

import argparse
import base64
import io
import json
import pathlib
import tarfile
import time
import urllib.error
import urllib.request


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--server", default="http://localhost:8080")
    p.add_argument("--eg-variant")
    p.add_argument("--cs-variant")
    p.add_argument(
        "--output", type=pathlib.Path, default=pathlib.Path(".arena/verification/pairing-replay")
    )
    args = p.parse_args()
    args.output.mkdir(parents=True, exist_ok=True, mode=0o700)

    def api(path, body=None):
        req = urllib.request.Request(
            args.server + "/api/v1" + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=120) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            raise RuntimeError(error.read().decode()) from error

    variants = api("/state")["variants"]
    latest = {}
    for variant in sorted(variants.values(), key=lambda v: v.get("builtAt", "")):
        latest[variant["stack"]] = variant
    eg = variants[args.eg_variant] if args.eg_variant else latest["enbility"]
    cs = variants[args.cs_variant] if args.cs_variant else latest["enbility"]
    definition = {
        "schemaVersion": 2,
        "name": "Captured pairing replay after restart",
        "scenario": "pairing",
        "timeScale": 1,
        "seed": 42,
        "groups": [
            {"id": "eg", "variant": eg["id"], "role": "EG", "count": 1},
            {"id": "cs", "variant": cs["id"], "role": "CS", "count": 1},
            {"id": "replacement", "variant": eg["id"], "role": "EG", "count": 1},
            {
                "id": "capture",
                "variant": latest["mdns-stresser"]["id"],
                "role": "STRESSER",
                "count": 1,
            },
        ],
        "links": [
            {"from": "eg", "to": "cs", "mode": "ship-pairing"},
            {"from": "replacement", "to": "cs", "mode": "ship-pairing"},
        ],
    }
    run_id = api("/runs", {"schemaVersion": 2, "definition": definition})["id"]
    prefix = "/runs/" + run_id
    print(run_id, flush=True)
    report = {"runId": run_id, "assertions": []}

    def state():
        return api("/state")["runs"][run_id]

    def node(id):
        return next(n for n in state()["nodes"] if n["id"] == id)

    def wait(description, predicate, seconds=120):
        deadline = time.monotonic() + seconds
        while not predicate():
            if time.monotonic() > deadline:
                raise RuntimeError(description + " timed out")
            time.sleep(1)
        print(description, flush=True)

    def command(id, name, params):
        result = api(prefix + "/nodes/" + id + "/commands", {"command": name, "params": params})
        if result["data"].get("ok") is not True:
            raise RuntimeError(str(result["data"]))
        return result

    def evidence():
        with urllib.request.urlopen(
            args.server + "/api/v1" + prefix + "/evidence", timeout=120
        ) as response:
            return response.read()

    def events():
        with tarfile.open(fileobj=io.BytesIO(evidence()), mode="r:gz") as archive:
            result = []
            for member in sorted(archive.getmembers(), key=lambda m: m.name, reverse=True):
                if member.name.startswith("events.ndjson"):
                    result.extend(
                        json.loads(line) for line in archive.extractfile(member) if line.strip()
                    )
            return result

    try:
        api(prefix + "/start", {})
        wait("Initial native pairing and readiness", lambda: state()["status"] == "complete")
        assertions = state()["assertions"]
        if not assertions or any(a["outcome"] != "pass" for a in assertions):
            raise RuntimeError("Initial native pairing failed: " + str(assertions))
        captured = events()
        digests = {
            e["data"].get("digest")
            for e in captured
            if e.get("nodeId") == "cs-001"
            and e["type"] == "pairing.validation"
            and e["data"].get("ok") is True
        }
        captures = [
            e["data"]
            for e in captured
            if e["type"] == "mdns.capture"
            and any(d and d.encode() in base64.b64decode(e["data"]["packet"]) for d in digests)
        ]
        if not captures:
            raise RuntimeError("No captured advertisement matches an accepted native digest")
        replay_id = captures[0]["id"]
        report["captureId"] = replay_id
        before = node("cs-001")["incarnation"]
        api(prefix + "/nodes/eg-001/actions", {"action": "stop"})
        api(prefix + "/nodes/cs-001/actions", {"action": "restart"})
        wait(
            "Listener restarted with durable trust",
            lambda: (
                node("cs-001").get("incarnation") not in (None, before)
                and bool(node("cs-001").get("snapshot", {}).get("pairing", {}).get("trusted"))
            ),
        )
        wait(
            "Native listener replacement window expired",
            lambda: (
                node("cs-001")
                .get("snapshot", {})
                .get("pairing", {})
                .get("listener", {})
                .get("active")
                is True
            ),
            1020,
        )
        incarnation = node("cs-001")["incarnation"]
        cursor = api("/state")["cursor"]
        profile = {
            "version": 1,
            "seed": 42,
            "services": 1,
            "rate": 10,
            "burst": 10,
            "churn": 0,
            "ttl": 120,
            "durationSeconds": 10,
            "pattern": "replay",
            "service": "_shippairing._tcp",
            "replayId": replay_id,
        }
        command("capture-001", "mdns-stress.start", {"profile": profile})
        time.sleep(12)
        command("capture-001", "mdns-stress.stop", {})
        observed = [e for e in events() if e.get("index", 0) > cursor]
        if any(
            e["type"]
            in ("telemetry.gap", "pairing.suppressed", "evidence.rotated", "telemetry.disconnected")
            for e in observed
        ):
            raise RuntimeError("Replay observation contains missing telemetry")
        native = [
            e
            for e in observed
            if e.get("nodeId") == "cs-001" and e.get("incarnation") == incarnation
        ]
        if any(e["type"] == "pairing.trust" and e["data"].get("trusted") for e in native):
            raise RuntimeError("Captured advertisement established trust again")
        rejected = [
            e
            for e in native
            if e["type"] == "pairing.validation"
            and e["data"].get("ok") is False
            and any(
                word in e["data"].get("reason", "").lower() for word in ("replay", "already-seen")
            )
        ]
        if not rejected:
            raise RuntimeError("Native replay rejection was not observed")
        report["assertions"].append(
            {"id": "durable-replay-rejection", "outcome": "pass", "reason": rejected[0]["data"]}
        )
        command("replacement-001", "pairing.announce", {"targetNode": "cs-001"})
        fingerprint = node("replacement-001")["fingerprint"]
        wait(
            "Fresh replacement accepted after replay rejection",
            lambda: any(
                i.get("fingerprint", "").lower() == fingerprint.lower()
                for i in node("cs-001")
                .get("snapshot", {})
                .get("pairing", {})
                .get("trusted", {})
                .values()
            ),
        )
        report["assertions"].append({"id": "fresh-pairing-positive-control", "outcome": "pass"})
    except Exception as error:
        report["error"] = str(error)
        raise
    finally:
        api(prefix + "/stop", {})
        (args.output / (run_id + ".json")).write_text(json.dumps(report, indent=2) + "\n")
        path = args.output / (run_id + ".tar.gz")
        path.write_bytes(evidence())
        path.chmod(0o600)


if __name__ == "__main__":
    main()
