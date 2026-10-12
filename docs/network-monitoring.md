# IP families and power monitoring

Arena supports IPv4 (the default), IPv6-only and dual-stack container networks.
Choose **IP mode** during setup. IPv6-only disables Docker IPv4 allocation;
dual-stack allocates both families. An isolated bridge uses a separate ULA /64
for each run and has no external route. IPv4-only containers disable IPv6 in
their own namespace. Host networking is not changed.

```json
{"network":{"mode":"dual-stack"}}
```

The native Go resolver filters discovery addresses and TCP dialing by the
selected family. Avahi uses the matching IPv4/IPv6 configuration; OpenEEBUS
libwebsockets is built with IPv6. Java binds its native SHIP server to the
appropriate address family. Native Go TCP observations appear as
`ship.transport` events with the family and socket addresses. A dual-stack
connection can use either family; enabling both does not assert automatic
failover or that every connection uses IPv6.

For a physical lab, configure `network.interface` and reserve explicit subnets
and container pools for each enabled family. IPv6 subnets must be ULAs. Physical
node configuration accepts `external.ipv6Address` as well as `external.address`;
neither is inferred from the agent's HTTPS endpoint. The currently connected
SMGw was previously qualified over IPv4. IPv6/macvlan hardware interoperability
has not been qualified by this increment.

## MGCP and MPC selection

**MGCP provider** selects one exact EG node, or Off. The controller rejects MGCP
on CS/stresser nodes and rejects more than one MGCP provider, including group
fan-out and imported overrides. Set **Power monitoring → MPC provider** on other
Go/C groups. Individual overrides can disable MPC or choose the MGCP EG without
splitting its group.

```json
{
  "groups":[
    {"id":"eg","variant":"installed-go-eg","role":"EG","count":2,"monitoring":"mpc"},
    {"id":"cs","variant":"installed-c-cs","role":"CS","count":3,"monitoring":"mpc"}
  ],
  "links":[{"from":"eg","to":"cs"}],
  "nodeOverrides":{"eg-002":{"resources":{},"monitoring":"mgcp"}}
}
```

Replace variant names with installed IDs. The rebuilt Go and C variants use
native GCP/MGCP and MU/MPC providers and MA consumers. Each provider has its own
native entity and use-case declaration. The C example's implicit providers are
disabled. An arena selecting MGCP rejects old C variants that cannot disable
those providers. Legacy variants cannot opt into these new controls. The pinned
Java public stack has no MGCP/MPC implementation; its provider option is disabled
and scenarios record unsupported coverage for it.

The node inspector displays native local and received total power (W), consumed
energy (Wh), and produced/feed-in energy (Wh). Missing values remain unavailable.
Saved readings remain visible after stopping. **Publish reading** supplies
calculated, simulated test input through the native provider API; it does not
simulate the receiver or resource telemetry. Consumption is positive and
production is negative, following the public
[MGCP specification](https://www.eebus.org/wp-content/uploads/2023/04/EEBus_UC_TS_MonitoringOfGridConnectionPoint_V1.0.0_public.pdf)
and [MPC specification](https://www.eebus.org/wp-content/uploads/2023/04/EEBus_UC_TS_MonitoringOfPowerConsumption_V1.0.0_public.pdf).

The **MGCP / MPC measurements** scenario checks positive and negative power and
both energy values in each trusted receiver's fresh native SPINE cache. An input
acknowledgement or provider-local reading cannot satisfy these assertions. It
runs at 1×. Each publish uses a persisted input sequence, and a native receiver
read excludes a value already in its cache. Subsequent receivers and scenario
reruns therefore require new values, rather than passing on periodic snapshots
of earlier measurements. Optional phase/current/voltage/frequency scenarios and full conformance
qualification are outside the current implementation.

The pinned Go MGCP provider incorrectly declared the MonitoringAppliance actor.
The preparation overlay narrowly changes that declaration to GridConnectionPoint;
provenance marks it as a `native-protocol-correction`, with the exact diff. The
adapter never substitutes a corrected actor in observations.

## mDNS traffic and faults

The stresser can use the arena mode, IPv4, IPv6, or both. Dual-stack divides one
requested packet rate across two sockets. Announcements contain A, AAAA, or both
records. Per-family attempted/sent/error/received counters and achieved rates
are separate; socket writes do not prove delivery. IPv6 uses `ff02::fb%eth0` and
hop limit 255. Pairing capture and replay remain bounded.

Directional faults select `family: "ipv4" | "ipv6"`. Omission defaults to IPv6 in
an IPv6-only arena and IPv4 otherwise. Disabled families fail preflight. Flower
filters select the exact peer address and, for mDNS ingress, UDP destination port
5353. IPv6 mDNS source addresses are inspected inside the owned sender namespace
by a temporary helper with all capabilities dropped and a read-only root. This
uses the actual link-local address instead of guessing from a MAC or using the
unicast ULA. Physical IPv6 mDNS senders and physical-side enforcement are currently
unsupported. Leases, statistics, cancellation and verified restoration retain
the existing ownership rules.

## Reproducing the checks

Rebuild each stack and `netfault`, then rebuild `orchestrator` and reload variants.
Use an isolated controller data directory so qualification does not interrupt
other arenas. These scripts create, start and stop their own arenas:

```sh
python3 scripts/network_monitoring_integration.py --server http://localhost:8080
python3 scripts/network_monitoring_integration.py --server http://localhost:8080 --guards enbility --scenario lpc --with-monitoring
python3 scripts/network_traffic_integration.py --server http://localhost:8080
```

The traffic script needs the local Docker CLI and the rebuilt `.arena/netfault.json`.
It verifies native netem drop counters as well as application/restoration, using
bounded TCP probes and low-rate mDNS traffic. Results contain native snapshots,
assertions and lease IDs, without identity keys or physical control tokens.
