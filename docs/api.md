# API and scenarios

The server accepts JSON under `/api/v1`. Actions need `Content-Type:
application/json`. Browser mutations must be same-origin. The UI, the CLI and
the integration script all use this API.

| Method | Path | Behavior |
|---|---|---|
| GET | `/state` | Variants, runs, nodes, counters, observations, resource samples and stream cursor |
| POST | `/variants/reload` | Reload the immutable local image catalog |
| POST | `/runs` | Validate and create a setup/identity bundle. Returns `{ "id": "run-…" }` |
| POST | `/runs/{run}/start` | Start Compose asynchronously, then run the selected non-manual scenario |
| POST | `/runs/{run}/stop` | Cancel a scenario or start, remove the arena's containers and bridge |
| POST | `/runs/{run}/test` | Run the saved scenario again and replace its assertion list |
| POST | `/runs/{run}/nodes/{node}/actions` | `{ "action": "start|stop|kill|restart|pause|resume" }` |
| POST | `/runs/{run}/nodes/{node}/commands` | Capability-checked adapter command and its matched result |
| DELETE | `/runs/{run}` | Drop an inactive or corrupt arena. Identities and evidence are kept. 409 if active |
| POST | `/runs/{run}/restore` | Restore a dropped arena |
| GET | `/runs/{run}/export` | Exact definition, test identities, image IDs and provenance |
| GET | `/runs/{run}/evidence` | Journal, run snapshot, bundle and composition as tar.gz |
| POST | `/evidence` | Bounded gzip/tar evidence import, reopened as an archived read-only run |
| GET | `/runs/{run}/resources` | Retained series, peaks, measured coverage, gaps and truncation |
| GET | `/runs/{run}/timeline` | Retained filtered UTC journal with original source time and protocol elapsed time |
| POST | `/runs/{run}/faults` | Bounded directional IPv4 impairment lease |
| DELETE | `/runs/{run}/faults/{fault}` | Restore a lease and verify native cleanup |
| GET | `/events` | SSE with `Last-Event-ID` or `?after=EPOCH:INDEX` (legacy numeric cursors accepted). Restart and retention gaps are explicit |
| GET | `/metrics` *(server root)* | Prometheus text metrics |

Pairing and mDNS extensions use schema v2. Schema v3 adds resource constraints,
sampling, campaigns, network faults, exploration and external nodes. V1 imports
retain their pre-trusted behavior. See [pairing workloads](pairing-testing.md) and
[expansion configuration](expansion.md).

Resource queries accept `node`, RFC 3339 `from`/`to`, and `points` (16–2000), or
`windowSeconds` (1–31536000) without `from`. Stopped/archived rolling windows end
at the last retained sample. Timeline queries accept `node`, `prefix`, `from`,
`to` and `limit`. Faster execution requires full native clock qualification;
the current built variants run at 1× only.

## Setup bundle

```json
{
  "schemaVersion": 1,
  "definition": {
    "schemaVersion": 1,
    "name": "LPC experiment",
    "seed": 42,
    "timeScale": 1,
    "scenario": "lpc",
    "durationSeconds": 1800,
    "groups": [
      {"id": "eg", "variant": "INSTALLED_EG_VARIANT", "role": "EG", "count": 1},
      {"id": "cs", "variant": "INSTALLED_CS_VARIANT", "role": "CS", "count": 50}
    ]
  }
}
```

Nodes are named `GROUP-001`, `GROUP-002` and so on. A group ID uses lowercase
letters, digits and hyphens, starts with a letter and has at most 48
characters. A count is 1-200 and the total is 2-201. `memoryMB` optionally
limits each container of a group. Unsupported roles and duplicate identities are
rejected.

Every group needs a trust relationship. A single EG trusts all CS groups, both
ways. With several EGs, add `links: [{"from":"eg-group","to":"cs-group"}]`. Each
link expands to the cross product of the two groups.

Identities and exact variant metadata are generated on creation if they are
missing. An exported `identities` entry holds the node's SKI, PEM certificate
and PKCS8 key. An import checks and reuses them. The seed does not regenerate
private keys.

## Commands

```json
{"command":"heartbeat.set","params":{"enabled":false}}
```

```json
{"command":"heartbeat.interval","params":{"seconds":60}}
```

```json
{"command":"limit.set","params":{"targetNode":"cs-001","usecase":"lpc","value":1200,"active":true,"durationSeconds":600}}
```

Use `targetNode` for a trusted partner by name, or `targetSki`. For LPP the
signed limit is negative.

The enbility EG also has `constraints.read` and `failsafe.set`. A failsafe write
only acknowledges that it was sent. Acceptance needs a receiver observation.

Capability names that start with `observe.` or `timing.` declare features. They
are not commands. Unsupported commands are rejected.

## Custom steps

Set `scenario` to `custom` and add an ordered `steps` list to the definition. A
target is one node or a whole group. Group targets run in stable node order.
Wait and observation deadlines follow the selected time scale. Commands and
transport deadlines stay in real time.

```json
[
  {"name": "Pause clients", "target": "cs", "action": "lifecycle", "command": "pause"},
  {"name": "Hold fault", "action": "wait", "seconds": 5},
  {"name": "Resume clients", "target": "cs", "action": "lifecycle", "command": "resume"},
  {"name": "Suppress EG heartbeats", "target": "eg-001", "action": "command",
   "command": "heartbeat.set", "params": {"enabled": false}},
  {"name": "Observe failsafe", "target": "java-cs", "action": "assert",
   "expect": {"path": ["lpc", "state"], "equals": "FAILSAFE", "withinSeconds": 150}},
  {"name": "Resume EG heartbeats", "target": "eg-001", "action": "command",
   "command": "heartbeat.set", "params": {"enabled": true}}
]
```

Supported actions are `command`, `lifecycle`, `wait`, `assert`, `resource` and
`fault`. Resource windows and fault lifetimes are real time. A command or
lifecycle step can also carry an `expect`. Its `path` points into the target's
observed snapshot, and the value must equal the given JSON within the deadline.

Unknown targets, malformed actions, missing expectations and unbounded deadlines
are rejected. An unsupported command gives an *unsupported* assertion. An
interrupted observation wait gives an *inconclusive* one.

## Adapter protocol v1

The controller adds a random command `id`. Every stdout event has `type`,
`nodeId`, `incarnation`, a monotonically increasing `sequence`, an RFC 3339 `at`
and `data`. `command.result` also has `commandId` and `data.ok`, plus `error` on
failure.

A snapshot event replaces the observed snapshot. `ship.state` and
`usecase.limit` can update it between snapshots. Capabilities are sent once per
process incarnation. Snapshots include `localSki` from the adapter’s certificate.
Unchanged snapshots refresh live state without adding journal or SSE entries.
Resource samples are exposed in `/state` and `/metrics` rather than SSE.

Before every lifecycle or command call, the controller checks the container's
ownership labels. Only adapter stdout can satisfy an assertion. Stderr logs
cannot. Parser errors, rejected messages, command timeouts, missing events and
evidence rotation are separate events. None of them becomes a certification
result or a global SPINE drop rate.
