# EEBUS Arena

On my way home from the 2026 Plugfest, I was thinking about the very busy
network and the problems it caused (and uncovered). The mix of spinning up a
lot of EEBUS peers quickly, monitoring their resources, and the sheer joy of
seeing different open source implementations emerge, led me to sketch an
"arena" for EEBUS stacks. My wish is to get somewhere a tiny bit close to the
great https://sim.tigerbeetle.com/, with sped-up time and extra noise (cable
unplugging, mDNS red node, power cuts), but testing interop could be a nice
byproduct. This is a vibed side-project, take it as such.

A local stress, interop and fault arena for real EEBUS implementations. A Go
controller starts Compose arenas on isolated Docker bridges, gives each node
its own certificate, registers explicit peer SKIs, runs assertions, and streams
resource samples and adapter output to a small React UI.

The initial targets are the pinned **enbility dev stack**, **NIBE OpenEEBUS**,
and the public **jEEBUS controllable-system demo**, but other stack could be added.
Each node runs LPC and LPP together. This is an experiment runner. It is not a
certification tool and not a replacement CS state machine.

## A look inside

A live arena with enbility, OpenEEBUS, jEEBUS and an independent mDNS stresser.
The map and table show the same native node state.

![Mixed-stack arena with live node state](docs/images/arena-overview.png)

CPU and RAM history, with separate process readings for the native stack,
Avahi, D-Bus and the collector. Peaks link to the recorded event interval.

![CPU and RAM history with native process attribution](docs/images/arena-resources.png)

Native SHIP pairing: announcements, learned trust, and the time to SHIP and
use-case readiness are shown separately.

![SHIP pairing with learned trust and stage timing](docs/images/arena-pairing.png)

## Run locally

Tested only with: Linux, Python 3.14.8, Git, Docker Engine 29.7.2, Compose 5.4.0. The
bootstrap script uses only the Python standard library. Go, Node, Java and C
toolchains run in pinned Docker images, so you do not need them locally.

The screenshots use Git LFS. Install Git LFS before cloning;
for an existing checkout, run `git lfs install` followed by `git lfs pull`.

For formatting, linting, and adapter development, see the
[development workflow and code review notes](docs/development.md).

```console
python3 scripts/arena.py build
python3 scripts/arena.py up
```

Open http://localhost:8080. Pick the small interoperability arena or the
1 EG + 50 mixed CS arena, then choose a scenario and a timing profile. Create
the arena and start it. Select a node on the map or in the table to see its
state, resources, logs, capabilities and lifecycle controls.

Choose **Edit arena** on a created or stopped arena to reopen its setup. Increase
a group's count or choose **Add group**, then **Save changes** and **Start arena**.
The saved arena keeps its ID and the identities of retained node IDs. Compatible
node state and pairing credentials are preserved; a changed implementation,
role or physical endpoint starts with fresh node state. **Cancel** leaves the
saved setup untouched. Running arenas must be stopped and cleanup completed
first. A concurrent setup change requires reopening the editor.

Previous setups and results stay under the run's `revisions/` directory; the
evidence archive includes their run snapshots and the continuous event/resource
history. Current assertions are cleared when the revised setup is saved.

Other commands:

```console
python3 scripts/arena.py build enbility      # build one stack
python3 scripts/arena.py build orchestrator  # rebuild the controller and UI
python3 scripts/arena.py register            # rescan built image provenance
python3 scripts/arena.py up --port 8088
python3 scripts/arena.py down                # stop only the controller
python3 scripts/arena.py clean --dry-run     # preview build-artifact cleanup
python3 scripts/arena.py clean               # remove prepared trees and local build outputs
python3 scripts/arena.py clean --cache       # also remove downloaded sources and development caches
```

`clean` removes `.arena/generated`, `bin`, `web/dist`, and `internal/webui/dist`.
`--cache` also removes `.arena/cache`, `.arena/sources`, `.arena/dev-cache`, and
`web/node_modules`. Run cleanup after builds and development checks finish.
Arena identities, run/evidence history, image catalogs, installed Docker images,
and the committed UI assets stay available. `--docker-cache` additionally prunes
all unused cache in the current Docker builder, including cache from other
projects. Combine any cleanup options with `--dry-run` to preview them.

Stop arenas in the UI before you shut down the controller, or their nodes keep
running. On restart the controller reconnects to the labelled containers and
keeps their identities and evidence.

**Drop arena** (or **Select** and **Drop selected**) removes created or stopped
arenas from the list. Their identities and evidence move to `.arena/dropped/`,
and they stay dropped after a restart. **Undo drop** restores them, also after a
page reload in the same tab. Active arenas return HTTP 409. Drop does not free
disk space. The API calls are:

- `DELETE /api/v1/runs/{id}` to drop
- `POST /api/v1/runs/{id}/restore` to undo

Local state lives in `.arena/` (git-ignored). The main parts:

| Path | Content |
|---|---|
| `sources/`, `cache/` | Checksum-verified upstream source archives |
| `generated/` | Upstream trees with the arena overlays applied |
| `variants.json` | Registered images, by immutable image ID |
| `runs/` | Per arena: setup, identities, compose file, evidence |
| `dropped/` | Dropped arenas |

The first build downloads toolchains and dependencies. Plan for several GB of
disk. The reference build is Linux/amd64, also on ARM Docker Desktop hosts,
where emulation affects performance.

### Security

On Linux the controller runs as your UID, with the Docker socket's group. This
gives it broad host privileges. The UI binds to loopback, rejects unknown Host
headers and cross-origin mutations, and has no authentication. Use it only as a
local lab. Exported setups and evidence contain **test private keys** by
design.

## What it does

- 2-201 nodes per arena, explicit EG-CS trust links, group/node CPU and RAM limits, and
  several revisions of one stack side by side.
- Node actions (start, stop, kill, restart, pause, resume) and protocol commands
  (heartbeat suppression and interval, enbility discovery refresh).
- Observations, where the stack exposes them: SHIP state and transitions, mDNS
  events, SPINE write results and latency, receiver limits, jEEBUS CS state.
- Retained CPU/RAM charts, Avahi/D-Bus/native process attribution, real collection
  intervals, throttling/OOM evidence, live output and optional Prometheus metrics.
- Scenarios: discovery, LPC, LPP, heartbeat loss, restart, seeded stress and
  custom steps, concurrent campaigns and seeded native exploration.
- Directional IPv4 impairments with verified cleanup, a shared UTC timeline, and
  configurable physical native stubs on dedicated lab networks.
- Export and import of a setup with its identities, read-only evidence reopening, and image
  archives to reproduce a run on another machine.

Outcomes are **pass**, **fail**, **unsupported** and **inconclusive**. Missing
telemetry is reported as a gap, never as a pass. A SHIP connection does not mean
the use case is ready. The upstream examples differ in observability and in
state-machine coverage, see [adapter capabilities](docs/adapters.md).

See [pairing and mDNS testing](docs/pairing-testing.md) for native SHIP Pairing Service provisioning, durable trust, structured decision traces, traffic profiles, and the new suites.

See [using the expansion](docs/expansion.md) for resource and campaign examples,
physical stub deployment and verification recipes. **Real time is the qualified
profile**; complete all-stack acceleration remains unfinished and faster profiles
are rejected. See the [Arena expansion plan](docs/expansion-plan.md) for resource history and
process attribution, node constraints, concurrent campaigns, network faults,
reliable time acceleration across all stacks, stateful exploration, and physical
devices.

## Headless runs and custom scenarios

Generate a setup from the installed variants:

```console
python3 scripts/arena.py example small > small.json
python3 scripts/arena.py example stress > stress.json
```

Build the Go client, or run it inside the controller container:

```console
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/arena ./cmd/arena
bin/arena create small.json
bin/arena start RUN_ID
bin/arena state
bin/arena export RUN_ID > saved-arena.json
```

Node actions and protocol commands:

```console
bin/arena action RUN_ID NODE_ID pause
bin/arena command RUN_ID NODE_ID command.json
```

Put `--server http://localhost:8088` before the command to use another port.
See [API and scenarios](docs/api.md), the [custom
example](examples/custom-steps.json) and the [JSON
schema](docs/bundle.schema.json).

The stress scenario runs after discovery and the baseline limits, for 30 minutes
by default. Its memory assertion compares the nodes, the controller and the
monitoring containers with a 12 GiB budget. Incomplete or stale samples make it
inconclusive. Performance results are kept apart from protocol assertions. The
201-node ceiling is only a configuration limit. It is not qualified on any host.

## Revisions and reproducibility

Source commits, archive hashes, toolchain versions, base image digests and Java
dependency locks are in `recipes/`. Overlays are in `adapters/`. Each image
embeds its generated diffs, adapter hashes and build recipe, and a run exports
them. Installed revisions get a persistent tag besides the immutable image ID. A
run also records the controller binary hash, Go build info, control image ID and
Docker Engine version.

To test another upstream revision, write an override. The baseline stays
untouched:

```console
python3 scripts/arena.py pin enbility-eebus FULL_40_CHARACTER_SHA --output recipes/variants/my-revision.json
python3 scripts/arena.py --lock recipes/variants/my-revision.json build enbility
```

For a coordinated enbility revision, pin the SHIP and SPINE sources in the same
override. Existing variants stay registered. If the upstream API changed, the
overlay may need an update: a missing patch context or a failed dependency
verification fails the build.

An exported run needs the exact image IDs. To move the images and their catalog
to another host:

```console
python3 scripts/arena.py images-export arena-images.tar
python3 scripts/arena.py images-import arena-images.tar
```

Copy `arena-images.tar.json` too. The archives hold node images and the registered
network fault helper, so build
the orchestrator on the other host, then import the setup in the UI. The archive
checksum and each image's embedded provenance must match the catalog.

Timing is not deterministic. The seed reproduces the workload choices and the
identities reproduce trust. The evidence records what actually ran.

## Build checks

Every build checks the candidate image before it is tagged `:local` and
registered:

- Go: static executables, built with `-trimpath -ldflags='-s -w'`.
- OpenEEBUS and libwebsockets: `-O2 -DNDEBUG`, stripped.
- Java: stripped runtime with the normal HotSpot JIT. Compiled classes keep
  source and line info but no variable tables. Third-party JARs keep their
  upstream metadata.
- Docker and Compose tools: stripped.

C compile manifests and Java source manifests are embedded in the node images. A
failed check keeps the previous `:local` image and catalog. To repeat a check
without starting containers:

```console
python3 scripts/verify_release.py
python3 scripts/verify_release.py openeebus --image sha256:...
```

Each Dockerfile has a sibling `.Dockerfile.dockerignore` allowlist. The
controller build gets only Go and frontend sources. A node build gets only the
generated sources, licenses and provenance of its stack, so identities, logs,
caches and other stacks stay out. The Go stack keeps the upstream tests because
`go mod tidy` reads their dependencies. The filters are part of the node recipe
hash.

To see what Docker passes to each build, without compiling anything:

```console
python3 scripts/verify_build_contexts.py
python3 scripts/verify_build_contexts.py orchestrator
```

Node contexts need prepared sources (`python3 scripts/arena.py prepare`). CI
checks the controller context and lints all six Dockerfiles with hadolint
2.15.1, pinned by digest in `recipes/lock.json`. Any finding, info included,
fails the lint. When you add a build input, update its allowlist and rerun the
context check and the image build.

## IP families and EEBUS monitoring

Choose IPv4, IPv6-only or dual-stack during setup. Select at most one EG as the
MGCP provider; other native Go/C nodes can offer MPC. The inspector shows native
received power/energy readings, and the monitoring scenario verifies SPINE
transmission. Java's current public stack has no monitoring provider.
See [configuration and verification](docs/network-monitoring.md).

## Optional monitoring

```console
python3 scripts/arena.py up --monitoring
```

This pulls the pinned Prometheus and Grafana images and provisions an arena
dashboard. Grafana is at http://localhost:3001 and Prometheus at
http://localhost:9091. Setup, fault controls, logs and assertions stay in the
main UI. The memory of the monitoring containers counts towards the arena
budget.

## Development

```console
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts/tests
cd web && npm ci && npm run build
```

The SPA is embedded in the Go executable. `npm run build` writes it to
`internal/webui/static`. `npm run dev` proxies API calls to a Go server on port
8080.

To test against a running controller:

```console
python3 scripts/integration.py --server http://localhost:8080
python3 scripts/integration.py --stress --duration 1800   # 51-node run
```

Each test saves its evidence and removes only the arenas it created.

More: [architecture](docs/architecture.md) covers the observation/control
boundary, timing profiles, retention and the Kubernetes outlook.
[Validation](docs/validation.md) lists the checks run so far.

## Sponsoring

Thanks for [EFR GmbH](https://www.efr.de/) for letting me build such things in
my working hours.
