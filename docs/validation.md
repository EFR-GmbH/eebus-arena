# Validation

Last run on 2026-10-06: Linux x86_64, Docker Engine 29.7.2, Compose 5.4.0, about
32 GB RAM. This is development evidence. It is not EEBUS certification and not a
benchmark of the 16 GiB reference host. Logs and evidence archives of these runs
are not part of the repository.

## Automated checks

| Check | Result |
|---|---|
| `go test -race ./...` | Pass. Covers identity import, lifecycle ownership, HTTP origin and host checks, command correlation, telemetry incarnations and gaps, immutable live events, observation freshness, resource availability, heartbeat prerequisites |
| `go vet ./...` | Pass |
| Python tests | 12 pass: immutable pins, archive checksum, extraction path traversal, patch context drift, symlinks, release compiler flags, no promotion after a failed audit |
| Web build | `npm ci` and the production build pass |
| hadolint 2.15.1 | Pass on all four Dockerfiles, info rules included, no suppressions |
| actionlint 1.7.12 | Pass |
| Image builds | All four build, pass `verify_release.py` and are registered |
| Build contexts | All four contexts exported and inspected |

CI uses toolchain images pinned by digest and actions pinned by commit SHA
(checkout v7.0.1, setup-python v7.0.0). The same checks ran locally. The
workflow has not run on GitHub yet.

Manual checks:

- Browser: create and start an arena, native limit, heartbeat and discovery
  controls, pause/resume, kill/start/restart, exact export and import. Desktop
  and mobile layouts.
- Image archive round trip: 22 installed revisions, checksum and embedded
  provenance verified, archive of about 751 MB.
- Monitoring: Grafana health OK, Prometheus arena target up.

## Native scenarios

The 12-case matrix runs LPC and LPP for each supported EG/CS pair, with all
three stacks. All containers initialized their adapters and kept running until
the runner removed its arena.

| Run | Pass | Fail | Inconclusive |
|---|---:|---:|---:|
| Matrix 1 | 46 | 1 | 1 |
| Matrix 2, images rebuilt after the audit | 46 | 0 | 2 |

- Matrix 1: OpenEEBUS EG to enbility CS, LPP deactivation, was inconclusive
  because the command response deadline expired. OpenEEBUS EG to jEEBUS CS, LPP
  deactivation, failed with a rejected or timed-out SPINE write.
- Matrix 2: the two inconclusive checks are the LPP activation and deactivation
  observations of OpenEEBUS EG to enbility CS.

These are interoperability findings, not a passing protocol matrix. A receiver
observation does not replace a missing sender acknowledgement.

The jEEBUS adapter handles the nullable active limit of the native state. This
keeps the periodic observations alive in the controlled and autonomous states
without inventing an effective limit.

More runs with real containers:

- enbility EG to jEEBUS CS, 10x heartbeat scenario: discovery, both active
  limits, a fresh controlled state and the failsafe transition passed (5
  assertions). Heartbeats were restored afterwards.
- enbility EG and CS restart scenario: 5 assertions passed, including unchanged
  SKI, new adapter incarnation and trusted reconnection.
- Custom scenario with three nodes: pause and resume two CS, then fresh native
  snapshots. All 6 assertions passed.

## 51-node stress run

Nodes: 1 enbility EG, 17 enbility CS, 17 OpenEEBUS CS, 16 jEEBUS CS. The run
completed the real-time 1,800 s seeded workload after discovery and the baseline
checks.

- The memory assertion **passed**. The peak of nodes plus controller and
  monitoring was 4.21 GiB, against a 12 GiB budget. There were 356 samples and
  none were incomplete.
- Overall: 373 pass, 120 fail, 30 inconclusive, memory assertion included. The
  protocol failures and timeouts under load are in the evidence. This is not an
  interoperability pass.
- Other containers, the native matrix, UI use and builds ran on the same host.
  Docker cgroup memory includes page cache, so it is not a heap measurement.

The run used a jEEBUS build from before the nullable-limit fix. Still open:

- a 30-minute rerun with the final images,
- a run on a 16 GiB reference host,
- performance qualification of the 201-node ceiling.

Image sizes reported by Docker: about 15 MB (Go), 86-87 MB (C), 163-168 MB
(Java), 157 MB (controller). Image size is not a RAM estimate.

## Release artifact audit

The audit found that the Go executables were already optimized and stripped.
OpenEEBUS and libwebsockets, the linked Java runtime and the copied Docker and
Compose tools still had static symbol tables. The Docker CLI also had DWARF
sections. Locally compiled Java classes had variable debug tables. All of this
was fixed and the images were rebuilt.

`scripts/verify_release.py` inspects the image files without starting the
application. The bootstrap runs it on each candidate image before it moves the
`:local` tag and registers the variants. It checks ELF sections, Go build
metadata, the embedded C compiler command manifests, locally compiled Java
classes (identified by a source manifest) and Java runtime options. Maven
dependencies bundled into the first-party JARs keep their upstream metadata and
are excluded from the variable-table check.

| Artifact | Verified |
|---|---|
| enbility | Go 1.27.1 default optimization, CGO off, trimpath, no DWARF or static symbol table |
| OpenEEBUS + libwebsockets | Release build, explicit `-O2 -DNDEBUG`, no debug or sanitizer flags, stripped executable and libraries |
| jEEBUS | 26 stripped runtime ELF files, 33 locally compiled classes without variable debug tables, source and line metadata kept |
| Controller + Docker/Compose | Go 1.27.1 default optimization, CGO off, trimpath, all three executables stripped |

The C manifests hold 221 OpenEEBUS and 228 libwebsockets compile commands. They
cover all configured targets, also those outside the runtime dependency graph.
The Java runtime is Temurin 21.0.12.1+1 in mixed mode with `UseCompiler=true`,
`TieredCompilation=true` and `TieredStopAtLevel=4`. Java relies on the JIT, not
on GCC's `-O2`.

The Python tests guard against later optimization overrides, debug and sanitizer
flags, re-enabled assertions, empty compile manifests, truncated artifacts, and
promotion or registration after a failed audit.

These checks cover the artifacts and protocol smoke behavior. They do not show a
RAM or throughput improvement.

## Build contexts

Each Dockerfile has its own allowlist, with a deny-all fallback in the root
`.dockerignore`. Docker exported each filtered context through a scratch probe.
The inventories confirmed that no identities, logs, caches, other stacks,
`node_modules` or earlier frontend builds were present. The sizes are summed
source file sizes, not compressed transfer sizes or image sizes.

| Context | Files | Source bytes |
|---|---:|---:|
| Previous shared filter, same working tree | 4,556 | 57,766,477 |
| enbility | 908 | 7,773,399 |
| OpenEEBUS | 1,163 | 9,163,260 |
| jEEBUS | 39 | 243,023 |
| Controller | 20 | 203,651 |

`scripts/verify_build_contexts.py` repeats this check. CI runs it for the
controller, which needs no upstream sources.

## Limits

- Compose on Linux is the only implemented backend. Kubernetes, Docker Desktop
  smoke tests, writable-layer disk accounting and upstream patch submissions
  are open. Directional IPv4 delay/loss/partition was added in the expansion below.
- The Go and C demos have no observable CS use-case state machine.
- Global SPINE packet-drop counts are unavailable. Telemetry retention and
  sequence gaps are reported separately.

## Pairing and mDNS increment (2026-10-10)

The new controller and stresser pass `go test -race ./...` and `go vet ./...`.
The generated enbility adapter also passes race tests and vet. All 14 Python
checks pass. The web production build is reproducible, hadolint passes on all
five Dockerfiles, and all five build contexts pass the isolation audit.
`golangci-lint` was unavailable on the host.

New enbility, OpenEEBUS pairing, mDNS stresser and controller images build and
pass their release audits. The existing jEEBUS image was rebuilt for regression
coverage. UI verification covers automatic pairing links, group edits, and the
traffic-profile editor. Creating and starting an enbility pairing arena in the UI
passed all eight readiness/write checks; stopped-state reset removed only the
selected listener state and retained its provisioning.

| Native test | Result |
|---|---|
| enbility announcer → enbility listener | 8 pass |
| OpenEEBUS announcer → enbility listener | 8 pass |
| Either announcer → OpenEEBUS listener | Native `invalid-record`; `txtversFirst:false` |
| Wrong secret, target ID, target fingerprint | 6 pass across three cases |
| Restart listener and announcer with durable trust | 6 pass |
| Announcer before listener | 5 pass |
| One enbility announcer, two listeners | 16 pass |
| Real-time replacement and old-peer rejection | 7 pass, including the native 15-minute timeout |
| Captured replay after listener restart | Replay rejected after native timeout; fresh replacement accepted |
| Baseline, mDNS ramps, sustained load, recovery | 44 pass |
| Fresh pairing during advertisements, invalid TXT, and queries | 24 pass across three cases |

The short mDNS run used 100 and 1,000 synthetic services at 10, 100 and 1,000
packets/s. Its two-second sustained interval verifies operation, not long-term
capacity. Peak sampled nodes plus controller/monitoring memory was 0.07 GiB,
with five complete samples. Workload counters distinguish socket writes, send
errors and skipped work; they do not measure packet delivery.

The 12-case pre-trust regression with the pairing-capable C variant completed
with 40 pass, 1 fail and 5 inconclusive assertions. The failure was LPC partner
discovery from enbility to OpenEEBUS. Inconclusive results were native write
timeouts: two enbility → jEEBUS LPP checks and three OpenEEBUS → enbility LPC/LPP
checks. These remain visible protocol findings, not an interoperability pass.

## Resource and lab expansion (2026-10-10)

All controller, lab-agent, collector and network-helper Go race tests and vet pass.
The generated native enbility adapter passes its race tests and vet in the pinned
Go image. All 16 Python tests pass; scripts compile. The frontend build exactly
matches the embedded assets reviewed in the browser. Pinned hadolint passes all
six Dockerfiles, and all six allowlisted build contexts pass. New Go/C/Java,
stresser, helper and controller images build and pass release audits. The final
controller and stresser were rebuilt after the last code changes. Host
`golangci-lint` remains unavailable.

| Development qualification | Evidence |
|---|---|
| 51 mixed Go/C/Java nodes | 1864 collector readings; measured interval p95 1.001 s, collection cost p95 0.842 ms, collector CPU p95 about 1% of one CPU; controller RSS about 40 MiB |
| mDNS baseline/load/recovery | Three passing command/resource checks; Avahi sampled CPU peak 51.06% under a 500-millicore quota, with separate native/D-Bus/collector traces; zero writer drops/errors |
| CPU and memory limits | Exact inspected runtime limits; 100-millicore, 32 MiB temporary OpenEEBUS node recorded CPU throttling and Docker OOMKilled after an intentional bounded allocation |
| Concurrent campaign | Three passing checks with overlapping mDNS load and receiver restart followed by dependent recovery |
| Seeded native exploration | Eight rounds, sixteen passing command/invariant checks; actual enabled sets and choices retained |
| Network faults | Native application/restoration for delay/jitter/loss/rate and partition; abrupt helper termination recovered through a second helper; scoped mDNS ingress recorded 3000 qdisc drops |
| Physical SMGw native stub | Eight fresh learned-pairing/SHIP/LPC/LPP checks pass; both endpoints restart and recover; pinned agent reattaches after controller restart |
| Physical path fault | Container-side partition toward explicit device IPv4 recorded seven drops and verified restoration |
| Evidence reopening | Real resource archive uploads and reopens at the same Avahi peak; archived execution rejected; gzip corruption, traversal, duplicates and cross-origin uploads have regression tests |
| UI | Desktop/mobile, system light/dark resource views; full node peak coverage, UTC timeline, campaign/exploration errors, physical controls and scoped fault editor reviewed |

The current SMGw does not contain STAG. The test uses a temporary native enbility
EG on device `han`, connected through host `eth0`; firmware, power and device UTC
were not changed. Its CPU/RAM totals describe the whole device. The deployment
script refuses an existing installation and has not yet been exercised end to end;
the equivalent individual build/deploy steps were verified.

These are short development runs on the current host, not the 16 GiB reference
qualification or a 30-minute final-image soak. OOM was deliberately induced in an
owned bounded container. Its last collector counter did not observe an OOM before
the container stopped; Docker's native flag supplies that evidence separately.
Independent process peaks cannot be summed as a simultaneous total.

Complete native clock interfaces, Avahi/dependency clock coverage, runtime drift
acknowledgements and 1× equivalence remain unfinished. Faster 2×/5×/10× profiles
fail preflight; the older partial 10× capability cannot enable them. Historical
10× results above do not qualify complete acceleration. Physical-side faults,
automated exploration shrinking/replay, cgroup v1 and private/PSS metrics also
remain open. See [current scope](expansion-plan.md) and [configuration](expansion.md).

The authorized host OpenCode 2.0.22 review completed on 2026-10-11 using
`github-copilot/claude-opus-5.5#high`. The live catalog was empty, so selection used
the cached catalog; all 17 persisted assistant messages confirm the requested
provider, model and reasoning variant. Its seven reported defects were verified
and fixed: stopped-run cleanup state, stale fault leases, failed-compaction retry
cost and sample loss, physical process-group termination, state API token exposure,
resource/timeline polling cost and physical TLS connection churn. Certificate
fingerprints remain visible because they are public verification data. The
oversized exploration-event concern was also confirmed and fixed with compact
action references and action-size bounds. The swap-accounting concern was not
confirmed; exact constraint enforcement remains deliberately required.

Regression tests cover these fixes, including actual supervisor process groups,
controller responsiveness during evidence pinning and one TLS connection across
multiple physical polls. Go race tests and vet pass after the fixes; the frontend
build remains reproducible and all 16 Python tests pass. A fresh Opus review of the
fix diff returned provider HTTP 404 before findings; this is not a clean follow-up
verdict. The separate UI finish review previously passed its four reported fixes.

A final archive lifecycle regression also passes: a dropped evidence archive can
be restored with its retained resource peak and read-only status intact.

The updated temporary SMGw agent passed all eight pairing/LPC/LPP checks again.
A real hard kill left no native or supervisor processes; starting a fresh native
incarnation restored SHIP, and the run stopped/released successfully. API state
did not contain the saved device token. All three network-fault profiles passed
again. Five reads of the same small archived history rewrote metadata five times
before the fix and zero times afterward, returning the same 54 series and 3113
points. Median request times were 10.89 ms and 9.43 ms respectively; these five
requests do not qualify performance at the full retention cap.

## IP families and power monitoring (2026-10-11)

Controller/stresser/helper race tests and vet pass, as do the generated native
Go adapter's race tests and vet. All 16 Python tests pass and the production frontend builds.
Rebuilt Go, C, Java, stresser, helper and controller images pass release audits;
all six allowlisted build contexts and pinned hadolint checks pass. Host
`golangci-lint` is unavailable.

| Final native qualification | Result |
|---|---|
| IPv4, IPv6-only, dual stack × Go/C MGCP EG | Six cases: 114 pass, six explicitly unsupported Java monitoring checks, zero fail/inconclusive |
| Original MGCP/MPC receiver checks | 96 reported pass; their per-publish freshness claim is superseded by the 2026-10-12 review and requalification below |
| Trusted SHIP to Go/C/Java CS | 18 pass across the six cases |
| LPC with MGCP/MPC enabled | Earlier three-family regression: 36 pass; exact earlier image manifests retained separately |
| mDNS socket traffic | Five three-second profiles at 40 packets/s; zero send errors; dual stack sends 60 packets per family from one shared budget |
| Directional partition and restoration | Eight bounded leases across enabled families; native netem drops in every case, including 201 mDNS drops per lease |
| UI | Desktop/mobile setup, one exact EG MGCP selector, disabled Java provider choice, native reading table and retained stopped-run readings; no document overflow or console errors |

`scripts/network_monitoring_integration.py` and
`scripts/network_traffic_integration.py` reproduce these checks. Final runs used
`enbility-b5dbadafb00b`, `openeebus-ed998f72731c`, `jeebus-3c13b8fbfa47` and
`mdns-stresser-b2285895c05d`. The local upstream addendum at
`tmp/openeebus-upstream/network-monitoring.md` retains exact run IDs, immutable
image IDs, native patch diffs and allowlisted assertions/readings. It excludes
identities, credentials and complete bundles.

A repeated matrix run exposed an Arena ingestion defect: Docker delivered an
older native stdout capability record after newer stderr, and the global log
watermark discarded it. The watcher now applies native sequence/incarnation and
container ownership gates to that record. Its regression test and the final
six-case rerun pass. This was an Arena observation defect, not a Java SHIP failure.

These runs use pre-trusted identities and 1× timing on isolated Linux Docker
bridges. They do not requalify learned pairing, full MGCP/MPC conformance, optional
phase/current/voltage measurements, dual-stack address failover, sustained load
or physical SMGw IPv6. Java has no native monitoring implementation at its pinned
revision. The Go provider's native MGCP actor declaration is corrected by an
explicitly classified, provenance-recorded overlay; C receiver values remain
native observations. See [configuration and scope](network-monitoring.md).

## Opus review and corrections (2026-10-12)

Host OpenCode 2.0.22 completed an independent review of commit `5c6a187` through
`github-copilot/claude-opus-5.5#high`. All 40 persisted assistant messages confirm
the requested provider, model, reasoning variant and review agent. The live model
catalog was unavailable; selection used the cached catalog. The review used a
fresh session, isolated credential/configuration state and a read-only tracked
source snapshot. Its raw report and model evidence are retained locally under
`tmp/reviews/opus-5c6a187-20261012/`.

The three main findings were verified and fixed: monitoring assertions could
pass on cached energy values, fault-form selections survived switching arenas,
and native resolver patches did not fail on missing context. Each monitoring
publish now uses a persisted counter and checks a native receiver baseline;
periodic snapshots of an unchanged cache cannot pass. The two pinned browsers,
two added-record sites and two removal sites require exact patch counts. The
fault panel remounts when the arena changes.

Additional corrections preserve Debian's Avahi settings while changing only IP
families and interface selection, reject incompatible stresser families at
preflight, validate Java's network mode, and keep native transport observations
outside the pairing trace limiter. Address-inspection helpers now have swap,
privilege and log bounds; startup cleanup verifies ownership before removing
orphaned inspectors from known runs. An unpaired Go zeroconf node does not browse
pairing services, so that part of the observer concern was not confirmed.

The suggested change to production-energy signs was rejected after checking the
published [MGCP §2.6.1](https://www.eebus.org/wp-content/uploads/2023/04/EEBus_UC_TS_MonitoringOfGridConnectionPoint_V1.0.0_public.pdf)
and [MPC §2.5.1](https://www.eebus.org/wp-content/uploads/2023/04/EEBus_UC_TS_MonitoringOfPowerConsumption_V1.0.0_public.pdf)
sign conventions. Both include exchanged energy and require negative production
values. Positive totals in C examples do not establish the normative convention.

The earlier 96 assertions demonstrated cached values but did not prove a fresh
transmission for every publish. They remain historical outcomes and are
superseded as freshness evidence. The corrected six-case, three-family matrix
passes 96 fresh native measurement assertions and 18 trusted SHIP checks, with
six explicitly unsupported Java monitoring checks. All three mDNS/fault cases
pass again. Go controller/helper/stresser race tests and vet, generated native Go
race tests and vet, 17 Python tests, the frontend build and affected image release
audits pass. Regression tests include a successful provider ACK with a fresh
unchanged receiver snapshot, counter persistence/collisions, patch drift,
stresser-family compatibility, transport delivery under pairing suppression,
and inspector cleanup ownership/resource bounds.

Two additional dual-stack cases, with Go/C MGCP providers respectively, pass
both their initial scenario and a rerun without restarting peers. Each rerun
passes 16 measurement checks with disjoint values, and its persisted sequence
continues from 16 to 32. A native runtime comparison preserves the complete
packaged Avahi configuration except the selected families and interface. A
controller restart removes an owned orphan inspector, preserves a test inspector
marked for another controller, and leaves no owned qualification fixtures after
explicit cleanup.

The review itself was static; fixes are verified by these tests and native runs,
without a second Opus verdict. Physical IPv6, full monitoring conformance and
accelerated clocks remain outside this qualification.
