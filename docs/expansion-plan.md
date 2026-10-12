# Arena expansion plan

Expand Arena with concurrent campaigns, network faults, a synchronized failure
timeline, reliable time acceleration across all stacks, stateful exploration,
and physical devices. Resource analysis and node constraints come first: every
later experiment needs to show which component consumed CPU or memory, when it
happened, and whether a configured limit affected the result. All analysis must
work in Arena without Prometheus or Grafana.

This plan records the target scope. The tested implementation below is at 1×;
complete accelerated clocks remain unfinished and cannot be selected successfully.
See [using the expansion](expansion.md) for configuration and limitations.

## Implementation status — 2026-10-11

| Area | Implemented and verified | Remaining qualification or work |
|---|---|---|
| Resources | Independent supervisor collection, Avahi/D-Bus/native attribution, raw history and minute aggregates, peak charts, resource budgets, read-only evidence import | Longer soaks, cgroup v1 and private/PSS metrics; partial minute windows report gaps |
| Constraints | Group defaults and individual CPU/RAM/process overrides, exact runtime inspection, no extra swap | Live limit editing is outside this change |
| Campaigns | Up to eight concurrent lanes, dependency validation, clock domains, cancellation and shared node mutation gates | Broader scenario recipes |
| Network faults | Directional IPv4/IPv6 delay/jitter/loss/rate/partition, scoped mDNS ingress, bounded leases, native statistics and verified restoration; killed-helper recovery | Physical-side fault enforcement and external IPv6 mDNS senders |
| EEBUS monitoring | One selectable EG MGCP provider, MPC on other Go/C nodes, native local/received values, six-case IP/stack matrix | Java native use cases, optional measurements and hardware IPv6 |
| Timeline | Retained filtered journal, UTC receipt time and protocol elapsed time, peak-to-event navigation | Packet capture and differential comparison |
| Exploration | Seeded choices from observed native capabilities/prerequisites, positive invariants and saved choices | Automated shrinking and replay orchestration |
| Physical lab | Configurable host/device interfaces and network, pinned TLS agent, temporary native enbility stub, learned pairing trust, endpoint restart recovery | STAG, firmware/power controls and external quota enforcement |
| Time acceleration | Explicit full-coverage build qualification and preflight; old partial 10× rejected | Native clock interfaces, dependency clocks, runtime acknowledgements/drift and 1× equivalence for 2×/5×/10× |

The 51-node mixed Go/C/Java collection check measured a 1.001-second interval
at the 95th percentile, collector CPU at about 1% of one CPU at that percentile,
and controller RSS about 40 MiB. This was a short development qualification,
not a long soak. A controlled mDNS baseline/load/recovery experiment attributed
an Avahi peak of 51.1% of one CPU under a 500-millicore quota. Another 100-millicore,
32 MiB node showed CPU throttling and Docker's native OOMKilled evidence.

Native observations establish protocol results. Unsupported observations,
missing intervals and storage limits remain explicit; they do not become passes.
The detailed sections below remain the target acceptance contract, including the
items still outstanding in this table.

## Resource history and process attribution

### Collection

Retain timestamped resource samples throughout arena operation, including startup,
idle periods, campaigns and recovery. Show baseline, peak, sustained pressure and
recovery for the arena, each node, and processes inside each container.

Use a versioned collection contract with independent freshness and coverage for
container and process metrics. A lightweight collector inside each node can read
local `/proc` without requiring a shell in scratch images. Give its output and
native adapter output one framed publisher so resource collection cannot corrupt
protocol events. Keep Docker cgroup samples as the source for container totals.
Count the collector's resource use and identify its process separately.

Process identity includes container identity, PID and process start time; PID reuse
must create a new series. Recognize Avahi, D-Bus, the native stack and collector
using executable identity and image metadata. Preserve other processes and
unclassified usage instead of assigning the container total to a guessed service.
Reset counter baselines after process or container replacement.

Start with a one second collection target and an optional 250 ms diagnostic mode
for selected nodes. Record actual measurement intervals, skipped samples and
collection cost. Bound concurrency, process enumeration, output queues and
retention. Qualify the overhead on 51 nodes before enabling broad high frequency
collection; a requested interval is not a claim that every sample met it.

### Measurements and meaning

| Measurement | Required interpretation |
|---|---|
| CPU use | Counter delta over the measured interval; 100 percent means one logical CPU and totals may exceed 100 percent |
| CPU allowance used | Use relative to the effective quota and CPU affinity; unavailable when that allowance cannot be established |
| CPU throttling | Throttled periods and duration where the runtime exposes them |
| Container memory | Cgroup usage, working set, applicable limit and available cache breakdown |
| Process memory | RSS and available private or proportional memory, with the metric named explicitly |
| Memory pressure | Available pressure, allocation failure and OOM evidence |
| Network traffic | RX and TX rates derived from valid counter intervals, alongside mDNS workload counters |

Process RSS can count shared pages more than once. Do not sum it into an invented
container memory total or label RSS as heap use. Runtime heap metrics can be added
as separate adapter capabilities later. Distinguish sampled memory peaks from any
kernel high water marks that are available.

CPU peaks describe the busiest measured interval, not an instantaneous maximum.
Missing intervals remain gaps. Summaries must account for measurement durations,
counter resets and coverage instead of treating missing values as zero.

### Analysis inside Arena

Add a resource view with CPU and memory charts, a shared time cursor, selectable
nodes and a process breakdown. Show configured and effective limits, throttling,
OOMs, current use, sampled peaks, interval distributions and time above chosen
thresholds. Selecting a peak opens the surrounding protocol and workload events.

An illustrative result should be expressible directly in the UI:

> During the mDNS burst, cs-003 Avahi reached 90 percent of one CPU over a measured
> interval. Its container used 112 percent of one CPU, and pairing readiness took
> longer than during baseline.

The view must show the measurement interval, source stresser, requested and
achieved traffic, service population, and baseline and recovery windows. Present
temporal association as association; attributing a spike to Avahi does not by
itself establish which peer caused it. Controlled baseline, load and recovery
phases help test that hypothesis.

Support performance assertions such as sustained CPU above a threshold, memory
growth between windows, failure to recover, CPU throttling and OOM occurrence.
Keep these outcomes separate from protocol assertions. Unknown coverage produces
an inconclusive result when it could change the assertion.

Persist series independently from the event journal so samples do not overwhelm
the live event stream. Serve time windows and resolutions through a resource
history API. Downsample with minimum, maximum, count and coverage so spikes remain
visible. Include retained raw samples, aggregates, metric definitions and gap
markers in evidence exports; reopening a stopped arena must work without Grafana.
Storage bounds and any discarded history must be visible.

### Acceptance

- A controlled mDNS ramp exposes Avahi CPU and memory separately from the native
  OpenEEBUS process and D-Bus where those processes are present.
- A controlled short CPU burst remains visible after chart downsampling and an
  evidence export and import.
- Process restart, PID reuse, controller restart and unavailable readings create
  correct series boundaries or explicit gaps.
- Collection has measured CPU, memory and storage overhead, and does not block
  command handling or native observations.
- The resource view works in both system appearances and with keyboard controls.

## Resource constraints for nodes

Add CPU and RAM settings to group defaults and explicit overrides for individual
nodes. Keep resource configuration independent of protocol role: EG, CS and mDNS
stressers all support the same container controls.

The proposed resource object uses `cpuMillis` for quota, `memoryMiB` for the hard
memory limit, and an optional `pidsLimit`. A quota of 500 allows half of one CPU;
1000 allows one CPU. It does not pin a process to a particular core. Document the
memory and swap policy separately so a RAM limit has a predictable meaning.

Use a schema revision with backward compatible imports of the current
`memoryMB` setting. Reject conflicting old and new settings. Node overrides
inherit omitted fields from the group; represent an explicit unlimited setting
separately from omission. Validate bounds and target IDs before container creation.

Apply the resulting limits through Compose and inspect the effective runtime
configuration after startup. Show requested and effective values in setup,
inspector, resource charts, bundles and evidence. Fail visibly if a requested
constraint could not be applied. Keep initial changes in setup; live quota or
memory changes can follow with separate action and restoration evidence.

Physical devices declare resource observation and control capabilities separately.
A container setting cannot imply that Arena constrained an external device.

Acceptance includes a CPU quota test with visible throttling, a deliberately
small memory limit with native OOM evidence, different limits for two nodes in one
group, and export and import preserving their effective definitions. Missing OOM
observation coverage cannot satisfy an assertion that no OOM occurred.

## Reliable time acceleration across stacks

Make accelerated experiments use one consistent protocol clock across enbility,
OpenEEBUS and jEEBUS, their participating dependencies, and the campaign runner.
Start by qualifying 1x, 2x, 5x and 10x. Reliability requires complete clock coverage
for the chosen scenario, consistent elapsed-time checks and evidence that the
accelerated run preserves the native protocol behavior being tested.

### Clock contract

Define two named time domains. Protocol time drives native protocol timers,
elapsed-time decisions and campaign actions expressed in virtual seconds. Real
monotonic time drives resource sampling, CPU counter deltas, observed transport
latency, controller health, cleanup and infrastructure watchdogs. Keep UTC receipt
timestamps for evidence and display both elapsed time domains in the timeline.

Use a common arena virtual epoch and rate anchored to a real monotonic clock.
Adapters acknowledge the clock configuration before participating. Report clock
offset, drift and callback lateness against declared tolerances. Keep the factor
fixed during a campaign initially; changing it with armed timers requires a
separately verified rescheduling contract.

Nominal durations on the wire retain their protocol units. Absolute protocol
timestamps and their freshness checks use the same declared clock. Classify
certificate validity, security timestamps and persistence ages individually;
changing host UTC or accelerating TLS certificate expiry is not the mechanism.
Record clock epoch, rate and domain with persisted deadlines and replay history.
Restart and import must preserve remaining protocol age according to an explicit
policy, including whether time advances while the arena is stopped. Reject an
incompatible clock context rather than silently renewing native state.

### Native timer coverage

Audit timer creation, current-time reads, elapsed-time comparisons, scheduler
queues and persistent timestamps in the pinned sources. Route them through small
native clock interfaces: Go clocks and timer factories, the C timer abstraction
and matching time reads, and Java clocks and scheduled executors. Instrument
locked dependency sources where necessary and retain those changes in provenance.
Native state machines and acceptance decisions remain upstream behavior.

| Area | Required coverage for an accelerated scenario |
|---|---|
| SHIP | Handshake deadlines, retry and reconnect scheduling, connection state timers |
| Pairing | Announcement and settling, replacement windows, replay retention and all associated elapsed-time checks |
| SPINE and use cases | Heartbeat generation and expiry, request deadlines, initialization, limit and failsafe expiration where implemented |
| Discovery | Query scheduling, announcements, TTL expiry, cache aging and participating responder timers |
| Campaigns | Scheduled actions, waits and observation deadlines with an explicit time domain |

Include Avahi and other discovery dependencies when their clocks affect the
scenario. A faster heartbeat beside an unscaled Avahi cache or replacement window
cannot qualify as complete acceleration. Add timer diagnostics for armed deadline,
remaining virtual duration and actual callback time, with bounded detail.

Replace the broad capability flag with versioned declarations of clock support,
timer coverage, supported factors and qualified scenario profiles. Keep legacy
imports explicit about their partial timing behavior. Before execution, check
every participating stack and dependency. Missing coverage makes the requested
profile unsupported; offer 1x before starting instead of silently mixing rates.
Absent native use-case or pairing support remains a separate capability gap.

Treat kernel socket deadlines, TLS transport, Docker lifecycle and network helper
operations as real infrastructure. Inventory their interaction with accelerated
native deadlines and qualify that boundary for each scenario. A shortened native
deadline can encounter real transport or CPU bottlenecks; record those effects
and any breached clock tolerance. Preserve native findings while marking timing
equivalence inconclusive when its required clock evidence is invalid.

### Workloads and resources

Declare campaign waits, network delay and workload duration as real or virtual.
For a virtual network delay, record both the requested virtual value and applied
real delay. Packet rates and bandwidth retain explicit physical units; multiplying
traffic with the clock factor is a separately selected workload policy. Record
requested and achieved load under that policy.

CPU percentages, memory history, collection intervals and capacity qualification
always use real time. An accelerated protocol experiment reports its factor and
both durations. Long soaks and performance acceptance still run for their stated
real duration. Record useful end-to-end speedup separately from the configured
clock rate, including setup and real infrastructure waits.

### Qualification and acceptance

- Exercise timer cancellation, reset, rounding, spurious wakeups and concurrent
  expiry. Validate current-time reads and remaining-time queries alongside timer
  scheduling so time is scaled exactly once.
- Run supported stack pairings at each qualified factor against a 1x reference.
  Compare per-peer transitions, causal ordering, invariants and normalized virtual
  deadlines; allow declared transport jitter rather than demanding identical
  ordering of independent concurrent events.
- A supported 15-minute pairing replacement window expires after 900 virtual
  seconds, approximately 90 real seconds at 10x within a stated tolerance. Check
  old-peer rejection, replay rejection and fresh replacement acceptance as well
  as timer expiry. Initial native pairing acceptance is a prerequisite.
- Cover heartbeat loss, failsafe and limit expiry where native state is observable,
  discovery expiry and reconnection. Exercise restart, persisted replay state,
  controller recovery and cancellation at accelerated time.
- Repeat under mDNS load and node CPU/RAM constraints. Record achieved speedup,
  drift, callback lateness, observation coverage and collector overhead. Qualify
  factors for the tested topology and resource envelope.
- Fail preflight for missing clock coverage and show clock faults or drift breaches
  in the timeline. A faster run alone cannot satisfy timing qualification.

Physical devices use 1x unless their native runtime explicitly supports and
qualifies the same clock contract. A device without that capability makes a fully
accelerated mixed campaign unsupported; qualify its interoperability at real time.

## Concurrent campaigns

Extend the existing step model with named concurrent lanes, scheduled offsets,
explicit dependencies, observation gates and bounded duration. Existing custom
steps retain their ordered behavior. Schedule against the declared real or virtual
monotonic clock and record planned, issued and completed times in both domains,
plus scheduling delay.

Each lane selects exact nodes or a stable group expansion. Serialize conflicting
mutations on one node or fault target, and record the ordering decision. Allow
observations and nonconflicting workloads to continue in parallel. Cancellation
joins workload workers and attempts every registered restoration action.

Save the seed, resolved targets, actual action schedule, initial state policy,
identities, immutable image IDs and clock configuration. Replaying actions
reproduces workload choices; qualified native timers follow protocol time, while
OS scheduling and infrastructure retain real time. Interrupted campaigns preserve
their completed actions and results. After controller restart, reconcile active
faults and clean up rather than silently replaying uncertain mutations.

Initial recipes cover fresh pairing during mDNS load, simultaneous cold starts,
reconnect storms, LPC/LPP writes during restarts, and recovery after combined
network and CPU pressure. Let users preview lanes and targets before running.

Acceptance requires observable overlap between lanes, recorded scheduling delay,
repeatable action selection, and cleanup after cancellation or controller loss.

## Directional network faults

Model each impairment by source, destination, direction, traffic selector,
parameters and lifetime. Support partition, delay, jitter, loss and bandwidth
limits. A selector can distinguish mDNS from SHIP using resolved runtime endpoints
and protocols; do not assume every device exposes the same SHIP port.

Implement faults with a dedicated Linux network helper for owned arena namespaces
and bridges. Keep network privileges out of stack adapters. Start with partition
and delay, then add jitter, loss and bandwidth after verifying their interaction.

Unicast connections can use per-peer selectors. Multicast faults need explicit
sender, receiver or multicast-domain scope: an EG-CS trust link alone does not
describe a multicast path. Show the affected traffic and participants in the UI.

Record request, application, verification and removal separately. Include active
rules and helper health in evidence. Keep a durable restoration ledger and clean
up on normal completion, cancellation, restart reconciliation and arena stop.
Overlapping faults need defined composition or rejection. Mark rules invalid when
container recreation changes their namespace or endpoint, then reconcile them.

Acceptance checks both the intended direction and the unaffected reverse path,
separates discovery failure from established connection failure, and verifies
restoration using configuration evidence and an independent traffic observation.
Configured random loss is a requested impairment, not a measured delivery rate.

## Failure timeline

Join campaign actions, native pairing and trust observations, SHIP transitions,
LPC/LPP writes, lifecycle events, network faults and resource history on one view.
Selecting an assertion opens its observation interval, exact participants,
commands, relevant resource peaks and any evidence gaps.

Use controller receipts and sequence information as a common reference. Preserve
native timestamps, sampling windows, incarnation boundaries and clock uncertainty.
Show real and virtual elapsed time, the active factor, coverage and drift warnings.
Expose clock skew for external devices; do not invent precise cross-device order.

Provide filters by node, process, peer, lane and event type, with expandable raw
evidence. Distinguish command acknowledgement, native acceptance, data exchange
and use-case readiness. A spike remains visible even when filtered event detail
has been suppressed. Include the timeline data in portable evidence.

Acceptance uses a combined mDNS, pairing and restart campaign: a failed assertion
must open the relevant interval and show resource attribution without an external
dashboard. Retention gaps and stale observations remain visible at every zoom.

## Stateful exploration

Define bounded action generators with prerequisites, weights, seed and maximum
steps or duration. Begin with pair, withdraw, restart, reconnect, replace, retry,
and existing heartbeat and limit commands. Add network actions after their cleanup
contract is established. Use qualified acceleration profiles for supported native
timers, including SHIP pairing; preserve 1x reference recipes.

Use a model of test actions and expected invariants; native implementations remain
the source of protocol state. Supported invariants include rejected credentials
not creating trust, durable trust surviving restart, retired peers remaining
rejected after replacement, and recovery after removing a fault. Enable each check
only with the capabilities and observation coverage needed to judge it.

Journal generated choices, resolved targets, prerequisite observations and the
first violated invariant. Save the executed sequence as a replayable campaign.
Observation gaps produce an inconclusive invariant rather than a false failure
or pass. Automatic sequence minimization is a later feature.

Acceptance reproduces action choices from a seed, exercises multiple valid
sequences, reports native failures with their evidence, and avoids issuing actions
whose prerequisites are absent or whose native capability is unsupported.

## Physical devices

Add an external node type with stable device identity, endpoint metadata,
provisioning method and explicit observation and control capabilities. External
nodes are visible participants without requiring a local container image.

Begin with one device reached through a lab gateway on a dedicated interface or
network segment. The gateway must provide discovery connectivity between the
arena and device; a TCP proxy alone is insufficient for mDNS. Record interface,
address and discovery observations in the setup and evidence. Test the supported
multicast arrangement before extending it to larger mixed topologies.

Use native device credentials and pairing mechanisms. Support manual provisioning
as an explicit recorded step where automation is unavailable. Reuse the adapter
contract for vendor or lab agents that can supply commands and observations.
Expose unsupported lifecycle, process metrics and resource controls individually.
Use 1x for external devices without qualified native clock support.

Apply network faults only to traffic through the configured lab gateway. Record
which traffic bypasses it. Device CPU and RAM may come from an optional device
agent; never present gateway resources as device resources. Show unavailable
device measurements while continuing to display container and gateway history.

Acceptance pairs one real device with a containerized peer, performs one native
LPC or LPP interaction, records evidence in the common timeline, and verifies
recovery from a fault on the controlled path. Its capability display accurately
describes commands, observations and resource constraints that Arena can provide.

## Delivery order

| Phase | Deliverable | Completion evidence |
|---|---|---|
| 1 | Resource history, process attribution, CPU and RAM constraints, node overrides, built-in charts | Attributed Avahi load, retained peaks, enforced limits, measured collector overhead |
| 2 | Shared timeline, clock contract and concurrent campaign scheduler | Overlapping pairing, mDNS and restart recipe with both time domains, portable evidence and verified cleanup |
| 3 | Reliable acceleration across Go, C and Java stacks and participating dependencies | Timer coverage, factor preflight, 1x equivalence and accelerated pairing replacement with measured drift |
| 4 | Directional network faults | Verified isolation and impairment, independent recovery checks, cancellation and restart cleanup |
| 5 | Stateful exploration | Seeded action sequences, qualified timing, supported invariants and replayable failures |
| 6 | Physical device attachment | One real device, working discovery, native use-case interaction and controlled fault recovery at its supported clock rate |

Repeat the 51-node, 30-minute workload with final images and a comparable mDNS
campaign. Qualify sustained sampling and campaign overhead on the 16 GiB reference
host, then extend soaks and assess the 201-container ceiling. Keep protocol
results, resource acceptance and collection completeness separate.

Revision comparison, automatic failure reduction and broader mDNS profile work
are deferred. Kubernetes and a replacement SHIP implementation remain outside
this plan. Focused packet capture can support fault verification later; it is not
a prerequisite for retaining resource history or providing the timeline.
