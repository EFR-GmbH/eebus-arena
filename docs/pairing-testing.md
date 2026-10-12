# Pairing and discovery workloads

Arena schema v2 separates intended links, startup trust, and observed learned
trust. V1 setups import with their original pre-trusted links. A link with
`"mode":"ship-pairing"` makes the EG a devZ announcer and the CS a devA listener;
it does not pre-trust the announcer on the listener. Additional EG links are
replacement candidates. Automatic pairing scenarios choose the first EG in
stable node order; custom steps explicitly select replacements.

Build the native adapters and traffic generator:

```console
python3 scripts/arena.py build enbility
python3 scripts/arena.py --lock recipes/variants/openeebus-pairing.json build openeebus
python3 scripts/arena.py build mdns-stresser
python3 scripts/arena.py build orchestrator
```

The OpenEEBUS pairing recipe pins PR #73 separately from the baseline. Existing
image variants remain registered. jEEBUS remains available for discovery and
use-case tests; its currently pinned public adapter does not advertise new SHIP
pairing capabilities. The OpenEEBUS announcer exposes one active target at a
time; multi-target scenarios report that capacity as unsupported.

In setup, select **SHIP pairing** as the scenario and **SHIP pairing** as peer
provisioning. For mDNS workloads add an **mDNS stresser**, configure its profile,
and select **mDNS load and recovery**. The stresser is not a protocol peer: it has
no certificate or trust links, but its resources count toward the arena budget.

## Provisioning and native state

Arena creates stable SHIP IDs, SHA-256 certificate fingerprints, and random
16-byte listener secrets. The bundle's `pairing` map contains initial credentials
alongside the existing test identities. It is private test material, like the
exported private keys. Secrets are not command evidence or metrics.

Native learned trust and replay buffers live in `state/NODE/pairing.json`, mounted
writable at `/state`; identity files remain read-only. Storage survives restart,
container recreation and arena stop/start. Corrupt state prevents native startup.
Persistence errors appear in pairing observations rather than resetting trust.

Setup imports start with fresh learned state. Evidence from a stopped arena also
contains final native state files. A running evidence export omits those files
because its processes may still be writing them.

Reset learned trust and replay history while a node and its scenario are stopped:

```console
bin/arena pairing-reset RUN NODE
```

The endpoint is `POST /api/v1/runs/RUN/nodes/NODE/pairing/reset`. Reset leaves the
node certificate and provisioning secret intact and records `pairing.reset`.

## Commands and observations

Use the existing commands endpoint or `bin/arena command`:

```json
{"command":"pairing.announce","params":{"targetNode":"cs-001"}}
```

`pairing.stop` takes the same target. `pairing.status` returns native pairing
status. Announcers provision trust in the supplied listener fingerprint before
announcing, as required by their native APIs. Listener acceptance is a separate
observation. The optional test parameter `credentialFault` accepts
`wrong-secret`, `wrong-target-id`, or `wrong-fingerprint`; it changes the supplied
target for that attempt without changing stored provisioning.

`pairing.discovery`, `pairing.record`, `pairing.validation`, `pairing.listener`,
`pairing.trust`, `pairing.replacement`, `pairing.persistence`, and
`pairing.announcement` carry native observations. Validation reasons remain native;
normalization never changes decisions. Both adapters bound repetitive validation
detail to 100 records per second and emit explicit `pairing.suppressed` counts.
Trust changes and command outcomes are retained separately.

The node inspector shows learned trust and observed stage durations. Durations
start when Arena issues an announcement, and end at fresh native snapshot
observations of trust, SHIP data exchange, and LPC/LPP partner readiness. They are
observation latency, not packet-level timestamps. Prometheus exposes cumulative
`arena_pairing_stage_seconds` histograms. Raw events retain native reasons and
correlation data. An existing connection can already be ready when reannouncing.

Custom expectations can match events as well as snapshots:

```json
{"name":"Pair and observe listener","target":"eg-001","action":"command",
 "command":"pairing.announce","params":{"targetNode":"cs-001"},
 "expect":{"target":"cs-001","event":"pairing.trust","match":{"trusted":true},
           "withinSeconds":90,"realTime":true}}
```

`expect.target` selects an exact node for a cross-peer observation. `absent:true`
requires no matching event throughout the deadline. Loss, suppression, retired
incarnations, disconnected telemetry, and cancellation make an absence result
inconclusive. Event expectations and pairing snapshots use real time; SHIP pairing
links reject accelerated timing. Missing observation capabilities are unsupported.

## mDNS profile v1

```json
{"command":"mdns-stress.start","params":{"profile":{
 "version":1,"seed":42,"services":1000,"rate":1000,"burst":100,
 "churn":20,"ttl":120,"durationSeconds":60,
 "pattern":"churn","service":"_ship._tcp"}}}
```

Patterns: `advertise`, `query`, `churn`, `goodbye`, `stale`, `duplicate`, `invalid`,
and `replay`. Services: `_ship._tcp`, `_shippairing._tcp`, `_http._tcp`. Pairing
records can target `forId` and `forPar`; generated digests intentionally do not
establish authenticated trust. `invalid` varies missing/version/duplicate TXT
fields; it does not emit arbitrary malformed DNS framing.

Service count and rate are bounded to 10,000; burst to 1,000; duration to 86,400
seconds. Traffic is IPv4 multicast within the private Docker bridge. Synthetic
service endpoints use documentation addresses, not working SHIP servers.

The worker records requested and achieved load, successful socket writes, send
errors, skipped scheduling work and received packets. These do not establish
network delivery or packet loss. `mdns-stress.stop` joins the sender before it
acknowledges completion; process restart does not resume a workload automatically.

The receiver retains at most 32 observed pairing response packets, each at most
9,000 bytes. `mdns.capture` evidence includes the packet and its SHA-256 ID. A
`replay` profile requires that `replayId`; arbitrary packet injection is not an
API. The inspector offers captured IDs. Dropped capture counts are explicit.

The built-in scenario runs baseline, 10/100/1,000 packet-per-second load phases,
then recovery. Pairing and use-case checks run inside each phase. After baseline,
these check existing trust and connections under load; they do not reset native
pairing state. The `pairing-load` suite begins noise before a fresh announcement. It always
attempts to stop every stresser on completion or cancellation. The workload
profile can be chosen per stresser group; rate and population follow the ramp.

## Suites

```console
python3 scripts/pairing_integration.py --suite smoke
python3 scripts/pairing_integration.py --suite negative
python3 scripts/pairing_integration.py --suite recovery
python3 scripts/pairing_integration.py --suite startup
python3 scripts/pairing_integration.py --suite multi-target
python3 scripts/pairing_integration.py --suite pairing-load
python3 scripts/pairing_integration.py --suite replacement
python3 scripts/pairing_integration.py --suite mdns --duration 60
python3 scripts/pairing_replay.py
```

Use `--eg-variant ID` and `--cs-variant ID` to compare exact builds. Smoke defaults
to the installed enbility/OpenEEBUS matrix; other suites default to enbility.
`--require-pass` fails on any non-pass protocol assertion. Reports and stopped
native-state evidence are saved under `.arena/verification/pairing`. The runner
only stops its own arenas. Replacement retains the native approximately
15-minute timer and therefore runs considerably longer than smoke tests.

The replay suite captures an accepted native advertisement, restarts the listener,
waits for its native replacement window, and checks replay rejection followed by
fresh replacement acceptance. Its report is separate from the controller's
initial readiness assertions.

## Following increments

The [expansion usage guide](expansion.md) describes the implemented resource,
campaign, fault and physical-stub controls. The SMGw qualification used a native
enbility EG in `/tmp`, not STAG: learned trust, SHIP exchange, LPC/LPP apply and
release passed, and both endpoint restarts recovered. Host `eth0`, device `han`,
protocol address and reserved lab pool are explicit configuration. All these
checks use real time; complete all-stack acceleration is not yet qualified.

The [Arena expansion plan](expansion-plan.md) adds resource history and process
attribution, CPU and RAM constraints with individual node overrides, concurrent
campaigns, a shared failure timeline, directional network faults, reliable time
acceleration across all stacks including pairing, stateful exploration, and
physical devices. Resource collection and analysis work directly in Arena without
Grafana.

Repeat the 51-node, 30-minute baseline and compare the same setup with a stresser.
Qualify long soaks, collector overhead on a 16 GiB host, and the 201-container
ceiling as the new capabilities land.

Packet capture tooling, arbitrary DNS fuzzing, QR entry, accelerated pairing,
Kubernetes, and a replacement SHIP implementation are outside this increment.
