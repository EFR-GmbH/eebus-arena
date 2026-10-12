# Using the Arena expansion

Schema v3 adds retained resources, node constraints, concurrent campaigns,
directional faults, native exploration and managed physical stubs. Existing v1
and v2 setups still import. These features work at **1×**; complete all-stack
clock acceleration is unfinished and faster profiles fail preflight.

Build the stacks, helper and controller with `python3 scripts/arena.py build`.
For an existing installation, rebuild the changed stacks and run
`python3 scripts/arena.py build netfault` and
`python3 scripts/arena.py build orchestrator`.

## Resource history and constraints

Open **Resources** to compare retained node totals and attributed processes.
Select a peak to inspect the nearby shared timeline. Avahi, D-Bus, native stack
and collector are independent series; a container's total is not assigned to
whichever process seems most likely. A stopped arena retains its measurements.

CPU 100% means one logical CPU, so a multicore total can exceed 100%. Allowance
percentage compares use with effective quota and affinity. CFS quotas constrain
an average over scheduling periods; a short sample may exceed 100% of allowance.
Process RAM is RSS, which includes shared pages and must not be summed as a
container total. Working set, cache and cgroup memory have their own fields.
Native throttling counters and Docker OOMKilled evidence remain separate.
Physical totals describe the whole device, including its other services.

Group defaults and individual overrides belong in the definition:

```json
{
  "groups": [
    {"id":"eg","variant":"installed-eg","role":"EG","count":1},
    {"id":"cs","variant":"installed-cs","role":"CS","count":3,
     "resources":{"cpuMillis":500,"memoryMiB":128,"pidsLimit":128}}
  ],
  "nodeOverrides": {
    "cs-002":{"resources":{"cpuMillis":100,"memoryMiB":32}},
    "cs-003":{"resources":{"cpuMillis":0}}
  },
  "sampling":{"intervalMillis":1000,"diagnosticNodes":["cs-002"]}
}
```

Replace the variant names with installed IDs. CPU uses millicores (1000 = one
CPU); RAM uses MiB. Omitted overrides inherit, explicit zero is unlimited.
Nonzero bounds are CPU 10–256000, RAM 16–1048576 and processes 16–65536. Hard RAM
and memory+swap limits are equal, allowing no extra swap. Runtime inspection
records the actual values and whether they match. Do not combine legacy
`memoryMB` with `resources.memoryMiB` on the same group. External devices reject
nonzero container constraints.

Normal collection is 1000, 2000 or 5000 real milliseconds. Up to eight selected
diagnostic nodes collect at 250 ms. Actual intervals, warmup, sequence loss,
partial process enumeration, queue drops and storage failures stay visible.
The collector's own CPU and RSS are attributed. Linux cgroup v2 is supported;
cgroup v1 and private/PSS memory are not yet qualified.

A custom step can budget a measured real-time window:

```json
{"name":"Avahi CPU budget","target":"cs-001","action":"resource",
 "resource":{"component":"Avahi","metric":"cpuPercent","max":75,
             "windowSeconds":30,"sustainedSeconds":5}}
```

Metrics are `cpuPercent`, `rssBytes`, `workingSetBytes`, `memoryBytes`,
`oomKills` and `cpuThrottledSeconds`. Without a component, the budget applies to
node totals. `growth:true` measures memory change against a recent pre-window
baseline. Windows span 2–600 real seconds. Missing coverage makes acceptance
inconclusive; an observed violation still fails. Sampled budgets cannot rule out
spikes between measurements.

Raw resource data uses three 32 MiB segments. Rotation compacts discarded data
into minute min/max/weighted mean/last/count/coverage, preserving sampled peaks.
Compaction failure retains old raw data and permits one extra 32 MiB of headroom
for new readings, with at most one retry per minute. If that headroom fills,
`retentionDrops` explicitly counts discarded new samples; raw storage remains
bounded at 128 MiB. Aggregate storage has a 256 MiB cap. A query cutting through an archived minute reports a boundary
gap, because that minute's extrema cannot be cropped reliably. Responses report
truncation at their series, point and gap bounds.

Resource reads wait for queued samples without forcing disk syncs. Changed files
and metadata sync every 15 seconds, on evidence export and on shutdown. Live
Resources and Timeline views refresh every 15 seconds; stopped and archived views
load once per selection. These endpoints still scan retained history, so full-cap
query performance remains a longer-term qualification item.

## Campaigns and exploration

Choose **Campaign** and supply lanes. Each lane runs its own ordered steps; an
`after` dependency waits for the named lanes to pass. Offsets are measured from
campaign start, so a delayed dependency can make a lane late. Lateness is saved.

```json
{"lanes":[
  {"id":"load","domain":"real","steps":[
    {"name":"Observe baseline","action":"wait","seconds":5}]},
  {"id":"restart","domain":"real","offsetSeconds":2,"steps":[
    {"name":"Restart CS","target":"cs-001","action":"lifecycle","command":"restart"}]},
  {"id":"recovery","after":["load","restart"],"domain":"real","steps":[
    {"name":"Settle","action":"wait","seconds":5}]}
]}
```

There are at most eight lanes and 1000 total steps. Cycles, unknown dependencies
and invalid targets fail validation. `domain` is `real` by default or `protocol`.
Offsets and waits follow that domain; collection and fault leases stay real.
Native commands and lifecycle changes to one node share a mutation gate.
Cancellation joins the lane workers before they finish.

**Explore** chooses seeded native commands and lifecycle changes from a supplied
model. It checks observed capabilities, lifecycle and applicable peer trust
before each choice. It records the enabled set and chosen action in
`exploration.choice`: enabled entries reference the definition's action index and
the concrete node ID. Each action must fit in 16 KiB, avoiding repeated parameters
in a potentially large enabled set. Use positive native observations as invariants. Bounds are
1–200 rounds, 1–100 actions, at most 20 invariants and 1–60 protocol seconds of
settling. Physical pause/resume are unavailable. Missing prerequisites produce an
inconclusive result. The seed repeats random choices under the same observed
state; scheduling is not deterministic. Automated shrinking and replay
orchestration remain unfinished.

## Directional network faults

Faults match one IPv4 peer direction. For unicast, the helper shapes egress from
the owned source container to the peer. For mDNS, it shapes ingress into an owned
receiver from one source, filtered to UDP port 5353. Multicast from other peers
does not match that filter. A physical target's explicit protocol address can be
used for container egress; physical-side enforcement is unsupported. IPv6 support and source scoping are
described in [network and monitoring](network-monitoring.md).

```json
{"from":"cs-001","to":"eg-001","traffic":"unicast",
 "delayMillis":100,"jitterMillis":20,"lossPercent":5,
 "rateKbit":1000,"durationSeconds":10}
```

Submit this to `POST /api/v1/runs/RUN/faults`, or put it in a custom step's
`fault` field with `action:"fault"`. `partition:true` means total loss. Bounds
are 1–600 real seconds, delay 0–10000 ms, jitter no larger than delay, loss
0–100%, and rate up to 1000000 kbit/s. The source/receiver must be an exact node
ID. The helper has NET_ADMIN only inside a verified owned namespace and uses
reserved rules and a lease-labelled IFB. It does not alter host qdiscs.

`fault.applied`, kernel `fault.statistics` and `fault.restored` are separate
native observations. Request success does not establish application. Leases
persist across controller restart. Abrupt helper termination triggers a bounded
recovery helper; the lease is removed only after native restoration proof.
Unverified cleanup remains visible and makes stopping the arena report failure.
Use `DELETE /api/v1/runs/RUN/faults/LEASE` for early restoration.

## Physical native stub

The currently connected SMGw does not contain STAG. Arena uses a temporary
native enbility EG stub with a small supervisor and pinned TLS lab agent. It
does not install firmware, set UTC, reboot the device or control power. One agent
owns at most one run/node session. Native pairing state survives stub restart.

For a fresh device deployment:

```console
python3 scripts/arena.py prepare
python3 scripts/physical_stub.py smgw-resc --address 169.254.1.1 --arch arm --output /tmp/my-private-smgw-deployment
```

The output contains private transport keys and a token. The deployer refuses an
existing `/tmp/arena-stub` or nonempty output directory. The current SMGw stub
was deployed and verified using the equivalent individual build/deploy steps;
the combined deployment script has not yet been exercised end to end.

Configure both the host network and device interface explicitly. The tested lab
uses host `eth0`, subnet `192.168.238.0/24`, reserved container pool
`192.168.238.128/28`, device interface `han` and device protocol address
`192.168.238.8`. The management endpoint is `https://169.254.1.1:8843`.
On this device `han` is attached through `eth0`; device `eth0` is not the IPv4
protocol interface. Use a reserved pool appropriate to your dedicated lab.

```json
{
  "network":{"interface":"eth0","subnet":"192.168.238.0/24","ipRange":"192.168.238.128/28"},
  "groups":[
    {"id":"eg","variant":"installed-enbility","role":"EG","count":1,
     "external":{"label":"SMGw native stub","endpoint":"https://169.254.1.1:8843",
                 "tlsFingerprint":"REPLACE_WITH_64_HEX_CERTIFICATE_SHA256",
                 "token":"REPLACE_WITH_64_HEX_PRIVATE_TOKEN",
                 "interface":"han","address":"192.168.238.8"}},
    {"id":"cs","variant":"installed-enbility","role":"CS","count":1,
     "resources":{"cpuMillis":500,"memoryMiB":128}}
  ],
  "links":[{"from":"eg","to":"cs","mode":"ship-pairing"}],
  "scenario":"pairing","timeScale":1
}
```

The current external adapter requires an enbility variant. Start/stop/kill/restart
control the managed stub, not the device OS. Stop the arena to release its native
session and containers. The agent remains available in `/tmp`; reboot clears
this temporary installation. Before manually stopping its listener, check the
saved PID's command line against `/tmp/arena-stub/arena-agent` so an unrelated
process is never targeted.

TLS validity is checked using host UTC against a pinned certificate; the agent
requires a private token. Device UTC may be wrong. Evidence keeps host receipt
time, original device time and offset separately. Device resource rates use
measured monotonic intervals rather than assuming its UTC is synchronized.

## Reopening and verification

Import **evidence tar.gz** to reopen a run's resources, timeline and assertions
as read-only history. Imported nodes cannot be started or controlled. Identity
files and executable setups in the archive are not installed. Archives reject
traversal, duplicate retained segments, damaged gzip checksums and storage bounds.
Exported setup/evidence still contains test private keys and may contain physical
agent credentials; keep full archives private. Upstream reports should use
allowlisted excerpts.

```console
python3 scripts/expansion_integration.py --suite resources
python3 scripts/expansion_integration.py --suite collection
python3 scripts/expansion_integration.py --suite campaign
python3 scripts/expansion_integration.py --suite explore
python3 scripts/expansion_integration.py --suite faults
python3 scripts/constraints_integration.py
```

These recipes use the latest installed variants, export private evidence and
stop only their own runs. The constraints recipe deliberately causes a bounded
OOM inside its own 32 MiB OpenEEBUS container. The collection recipe is a short
51-node development check, not a long soak or capacity certification. See the
[implementation status](expansion-plan.md#implementation-status--2026-10-10) and
[validation record](validation.md) for measured results and remaining work.

## IP families and native monitoring

See [IP families and power monitoring](network-monitoring.md) for IPv6-only and
dual-stack networking, the one-EG MGCP selector, MPC providers, received native
readings and per-family mDNS/fault controls.
