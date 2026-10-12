# Adapter capabilities and gaps

For new SHIP pairing, enbility supports multiple announcement targets and a native
addCu listener, with durable trust and replay state. The separate OpenEEBUS
pairing recipe pins PR #73 and exposes one active announcement target, its native
listener and durable state. Baseline OpenEEBUS and jEEBUS have no pairing
capabilities. See [pairing tests](pairing-testing.md).

The OpenEEBUS listener currently rejects tested records with `invalid-record`
and `txtversFirst:false`. Listener recovery and replacement remain unqualified.
Its pinned tree also never calls the native `NotifyMessageExchange` hook; the
replacement suite can assess that timer behavior once initial acceptance works.

The independent `mdns-stresser` exposes start, stop, status, snapshot, and
`observe.mdns`. It has no EG or CS role. It generates bounded DNS-SD traffic and
replays only captured pairing response packets.

| Stack | EG | CS | Limit observation | CS state observation | Heartbeat controls | Discovery refresh |
|---|---|---|---|---|---|---|
| enbility dev | LPC + LPP | LPC + LPP API demo | Native API snapshots | Full CS machine unavailable | Stop/start, interval | Native mDNS request |
| OpenEEBUS | HEMS LPC + LPP | Heat-pump LPC + LPP demo | Native listener callbacks | Full CS machine unavailable | Stop/start, interval | Events only |
| jEEBUS | Public EG demo unavailable | Public LPC + LPP CS demo | Native write callbacks | Native state + effective limit | Suppress/resume, interval | Events via native logs |

The UI enables commands from the capabilities an adapter reports after native
initialization, not from the stack name. A restart clears the capabilities until
the new process reports them.

## Runtime images

- **enbility**: static Go executable in `scratch`, with the pure-Go mDNS
  provider.
- **OpenEEBUS**: stripped C executable, TLS, cJSON, libwebsockets, Avahi, D-Bus
  and mDNS NSS. Avahi and D-Bus run inside the node and count towards its
  resources.
- **jEEBUS**: a `jlink` runtime, the upstream CS modules and their locked
  dependencies. Heap and metaspace are set in its Dockerfile. Container limits
  are configured separately.

## Observations

`spine.response` is the latency of a matched request/result pair for an EG limit
write. Parse, processing, send and response failures are separate events where
the native code exposes them.

A telemetry sequence gap is **not** a SPINE packet drop. Transport silence,
missing hooks and unsupported state observations do not allow a trustworthy
global drop count. The arena reports the coverage as unavailable, not as zero
loss.

## Overlays

Overlays keep the upstream behavior. The timer and control hooks and the
observation callbacks are listed in the provenance, together with the source
commits.

The Go and C examples have no full LPC/LPP CS state machine. This is a coverage
gap of the upstream examples. Negative-write rejection, state transitions and
timeouts must be judged from the native observations of the selected stack, not
supplied by the test harness.

## IP families and monitoring

Rebuilt variants declare their network and provider controls in `arenaFeatures`
provenance. Native Go/C offer selectable MGCP (EG only) or MPC and native MA
consumers. The current Java variant supports all three container network modes
but has no MGCP/MPC provider or consumer. Optional measurements and hardware IPv6
remain unqualified. See [details and source correction](network-monitoring.md).
