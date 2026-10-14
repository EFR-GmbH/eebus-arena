import React, { useEffect, useMemo, useState } from "react";
import "./analysis.css";
import { nodeOptionLabel } from "./labels";

type Point = {
  at: string;
  min: number;
  max: number;
  mean: number;
  last: number;
  samples: number;
  seconds: number;
};
type Series = {
  id: string;
  nodeId: string;
  processId?: string;
  name: string;
  component: string;
  metric: string;
  source: string;
  peak: number;
  peakAt: string;
  points: Point[];
};
type History = {
  from: string;
  to: string;
  bucketSeconds: number;
  status: {
    queueDrops: number;
    writeErrors: number;
    rotations: number;
    retentionDrops?: number;
  };
  gaps: { at: string; nodeId: string; error?: string }[];
  series: Series[];
  truncated?: boolean;
};
type AnalysisNode = {
  id: string;
  stack?: string;
  role?: string;
  resources?: Record<string, number>;
  effectiveResources?: Record<string, any>;
  processMetrics?: {
    intervalSeconds?: number;
    collectionMillis?: number;
    cgroup?: Record<string, number>;
    clockOffsetSeconds?: number;
    deviceAt?: string;
  };
  resourceAt?: string;
  external?: { label?: string };
};
type AnalysisRun = {
  id: string;
  status: string;
  nodes: AnalysisNode[];
  definition: { timeScale: number; network?: { mode?: string } };
  runtime?: Record<string, any>;
};
export type EvidenceFocus = { at: string; node?: string };

function useEvidence<T>(url: string, live: boolean) {
  const [data, setData] = useState<T>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    setData(undefined);
    setError("");
    setLoading(true);
    async function update() {
      try {
        const response = await fetch(url, { signal: abort.signal });
        const value = await response.json();
        if (!response.ok) throw Error(value.error ?? response.statusText);
        if (!abort.signal.aborted) {
          setData(value);
          setError("");
        }
      } catch (e) {
        if (!abort.signal.aborted) setError(String(e));
      } finally {
        if (!abort.signal.aborted) {
          setLoading(false);
          if (live) timer = setTimeout(update, 15000);
        }
      }
    }
    void update();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
  }, [url, live]);
  return { data, error, loading };
}
const clock = (at: string) => new Date(at).toLocaleTimeString();
const utcStamp = (at: string) =>
  new Date(at).toISOString().replace("T", " ").replace("Z", " UTC");
const valueText = (value: number, memory: boolean) =>
  memory ? `${(value / 1048576).toFixed(1)} MiB` : `${value.toFixed(1)}%`;

function Chart({
  series,
  history,
  memory,
  cursor,
  onCursor,
}: {
  series: Series[];
  history: History;
  memory: boolean;
  cursor: number;
  onCursor: (v: number) => void;
}) {
  const from = Date.parse(history.from),
    to = Date.parse(history.to),
    span = Math.max(1, to - from);
  const maximum =
    Math.max(memory ? 1048576 : 100, ...series.map((s) => s.peak)) * 1.12;
  const x = (at: string) => 58 + ((Date.parse(at) - from) / span) * 560;
  const y = (value: number) => 186 - (value / maximum) * 160;
  return (
    <figure className="resource-chart">
      <figcaption>
        <strong>{memory ? "Memory" : "CPU"}</strong>
        <span>
          {memory
            ? "MiB · container working set · device used · process RSS"
            : "100% = one CPU · interval averages"}
        </span>
      </figcaption>
      <svg
        viewBox="0 0 640 220"
        role="img"
        aria-label={`${memory ? "Memory" : "CPU"} sampled peaks over real time`}
      >
        {[0, 0.5, 1].map((f) => (
          <g key={f}>
            <line
              className="chart-grid"
              x1="58"
              x2="618"
              y1={y(maximum * f)}
              y2={y(maximum * f)}
            />
            <text x="50" y={y(maximum * f) + 4} textAnchor="end">
              {memory
                ? ((maximum * f) / 1048576).toFixed(0)
                : (maximum * f).toFixed(0)}
            </text>
          </g>
        ))}
        {history.gaps.map((g, i) => (
          <line
            key={i}
            className="chart-gap"
            x1={x(g.at)}
            x2={x(g.at)}
            y1="26"
            y2="186"
          />
        ))}
        {series.map((s, i) => {
          let last = -Infinity;
          let previousInterval = 0;
          const path = s.points
            .map((p) => {
              const at = Date.parse(p.at);
              const move =
                at - last >
                  Math.max(history.bucketSeconds, previousInterval, p.seconds) *
                    1500 ||
                history.gaps.some(
                  (g) =>
                    g.nodeId === s.nodeId &&
                    Date.parse(g.at) > last &&
                    Date.parse(g.at) <= at,
                );
              previousInterval = p.seconds;
              last = at;
              return `${move ? "M" : "L"}${x(p.at)},${y(p.max)}`;
            })
            .join(" ");
          return (
            <path
              key={s.id}
              className={`series-line series-${i % 6}`}
              d={path}
            />
          );
        })}
        <line
          className="chart-cursor"
          x1={58 + (cursor / 1000) * 560}
          x2={58 + (cursor / 1000) * 560}
          y1="26"
          y2="186"
        />
        <text x="58" y="210">
          {clock(history.from)}
        </text>
        <text x="618" y="210" textAnchor="end">
          {clock(history.to)} · real time
        </text>
      </svg>
      <label className="chart-slider">
        <span>
          Time cursor ·{" "}
          {clock(new Date(from + (span * cursor) / 1000).toISOString())}
        </span>
        <input
          type="range"
          min="0"
          max="1000"
          value={cursor}
          onChange={(e) => onCursor(+e.target.value)}
        />
      </label>
      <ul className="chart-legend">
        {series.map((s, i) => {
          const at = from + (span * cursor) / 1000;
          const point = s.points.find(
            (p) =>
              at >= Date.parse(p.at) &&
              at < Date.parse(p.at) + history.bucketSeconds * 1000,
          );
          return (
            <li key={s.id}>
              <span className={`series-key series-${i % 6}`} />
              {s.nodeId} {s.processId ? `· ${s.name}` : ""}
              <small>{s.source}</small>
              <small>
                {point
                  ? `${memory ? (point.max / 1048576).toFixed(1) + " MiB" : point.max.toFixed(1) + "%"} peak · ${point.samples} samples`
                  : "No sample at cursor"}
              </small>
            </li>
          );
        })}
      </ul>
    </figure>
  );
}

export function ResourcePanel({
  run,
  onFocus,
}: {
  run: AnalysisRun;
  onFocus: (focus: EvidenceFocus) => void;
}) {
  const [node, setNode] = useState("");
  const [cursor, setCursor] = useState(1000);
  const [windowMinutes, setWindowMinutes] = useState(0);
  const url = `/api/v1/runs/${run.id}/resources?points=240&node=${encodeURIComponent(node)}${windowMinutes ? `&windowSeconds=${windowMinutes * 60}` : ""}`;
  const { data, error, loading } = useEvidence<History>(
    url,
    !["created", "stopped", "archived", "corrupt"].includes(run.status),
  );
  const selected = run.nodes.find((n) => n.id === node);
  const processRows = useMemo(() => {
    const rows = new Map<string, { cpu?: Series; ram?: Series }>();
    for (const s of data?.series ?? [])
      if (s.processId) {
        const row = rows.get(s.processId) ?? {};
        if (s.metric === "cpuPercent") row.cpu = s;
        if (s.metric === "rssBytes") row.ram = s;
        rows.set(s.processId, row);
      }
    return [...rows.values()].sort(
      (a, b) => (b.cpu?.peak ?? -1) - (a.cpu?.peak ?? -1),
    );
  }, [data]);
  function containerSeries(memory: boolean) {
    const all = data?.series ?? [];
    const container = all.filter(
      (s) =>
        !s.processId &&
        (memory
          ? ["workingSetBytes", "memoryBytes", "deviceUsedBytes"].includes(
              s.metric,
            )
          : s.metric === "cpuPercent"),
    );
    const best = container.filter(
      (s) =>
        !container.some(
          (other) =>
            other.nodeId === s.nodeId &&
            ((other.metric === "workingSetBytes" &&
              s.metric === "memoryBytes") ||
              (other.source === "cgroup" &&
                s.source === "docker" &&
                other.metric === s.metric)),
        ),
    );
    return best;
  }
  function chartSeries(memory: boolean) {
    const processes = node
      ? processRows
          .slice(0, 4)
          .flatMap((row) =>
            (memory ? row.ram : row.cpu) ? [memory ? row.ram! : row.cpu!] : [],
          )
      : [];
    return [...containerSeries(memory).slice(0, 8), ...processes].slice(0, 8);
  }
  const gap =
    data &&
    (data.gaps.length > 0 ||
      data.status.queueDrops > 0 ||
      data.status.writeErrors > 0 ||
      (data.status.retentionDrops ?? 0) > 0 ||
      data.truncated);
  return (
    <section className="analysis-panel">
      <div className="analysis-heading">
        <div>
          <h2>Resources</h2>
          <p className="muted">
            Sampled peaks and process attribution throughout the arena.
          </p>
        </div>
        <div className="analysis-controls">
          <label>
            Node
            <select value={node} onChange={(e) => setNode(e.target.value)}>
              <option value="">All nodes · container totals</option>
              {run.nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {nodeOptionLabel(n)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Window
            <select
              value={windowMinutes}
              onChange={(e) => {
                const minutes = +e.target.value;
                setWindowMinutes(minutes);
              }}
            >
              <option value="0">Whole arena</option>
              <option value="5">Last 5 minutes</option>
              <option value="30">Last 30 minutes</option>
              <option value="120">Last 2 hours</option>
            </select>
          </label>
        </div>
      </div>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {loading && <p role="status">Loading retained resource samples…</p>}
      {gap && (
        <p className="coverage-note" role="status">
          Incomplete measurement coverage · {data.gaps.length} visible gaps,{" "}
          {data.status.queueDrops + (data.status.retentionDrops ?? 0)} dropped
          samples, {data.status.writeErrors} write errors. Missing values are
          not zero.
        </p>
      )}
      {data && data.status.rotations > 0 && (
        <p className="coverage-note">
          Raw history has rotated {data.status.rotations} times. Minute
          summaries preserve earlier sampled peaks; older individual intervals
          may be unavailable.
        </p>
      )}
      {selected && (
        <div className="resource-constraints">
          <strong>{selected.id}</strong>
          <span>
            Requested CPU{" "}
            {selected.resources?.cpuMillis
              ? `${selected.resources.cpuMillis} millicores`
              : "unlimited"}{" "}
            · RAM{" "}
            {selected.resources?.memoryMiB
              ? `${selected.resources.memoryMiB} MiB`
              : "unlimited"}
          </span>
          <span>
            {selected.effectiveResources?.verified
              ? ["stopped", "archived"].includes(run.status)
                ? "Last recorded limits verified"
                : "Runtime limits verified"
              : selected.external
                ? "External device · container limits do not apply"
                : "Runtime limits unverified"}
          </span>
          {selected.processMetrics && (
            <span>
              {["stopped", "archived"].includes(run.status)
                ? "Last measured interval"
                : "Measured interval"}{" "}
              {selected.processMetrics.intervalSeconds?.toFixed(3)} s ·
              collector {selected.processMetrics.collectionMillis?.toFixed(2)}{" "}
              ms
            </span>
          )}
        </div>
      )}
      {selected?.processMetrics?.cgroup && (
        <div className="resource-constraints">
          <span>
            CPU allowance used:{" "}
            {selected.processMetrics.cgroup.cpuAllowancePercent == null
              ? "Unknown"
              : `${selected.processMetrics.cgroup.cpuAllowancePercent.toFixed(1)}%`}
          </span>
          <span>
            Throttled:{" "}
            {selected.processMetrics.cgroup.cpuThrottledSeconds == null
              ? "Unknown"
              : `${selected.processMetrics.cgroup.cpuThrottledSeconds.toFixed(3)} s`}{" "}
            · {selected.processMetrics.cgroup.cpuThrottledPeriods ?? "unknown"}{" "}
            periods
          </span>
          <span>
            OOM kills: {selected.processMetrics.cgroup.oomKills ?? "Unknown"}
          </span>
        </div>
      )}
      {selected?.resourceAt &&
        Date.now() - Date.parse(selected.resourceAt) > 10000 &&
        !["stopped", "created", "archived"].includes(run.status) && (
          <p className="coverage-note">
            The latest process sample is stale. Retained values do not describe
            current use.
          </p>
        )}
      {selected?.processMetrics?.clockOffsetSeconds != null &&
        Math.abs(selected.processMetrics.clockOffsetSeconds) > 1 && (
          <p className="coverage-note">
            Device UTC is unsynchronized ({selected.processMetrics.deviceAt}).
            The timeline uses controller receipt time; CPU intervals still use
            the device’s real elapsed time.
          </p>
        )}
      {data && data.series.length === 0 ? (
        <p className="empty">
          No retained samples yet. Process attribution requires an image built
          with the Arena collector.
        </p>
      ) : (
        data && (
          <>
            <div className="resource-charts">
              <Chart
                series={chartSeries(false)}
                history={data}
                memory={false}
                cursor={cursor}
                onCursor={setCursor}
              />
              <Chart
                series={chartSeries(true)}
                history={data}
                memory
                cursor={cursor}
                onCursor={setCursor}
              />
            </div>
            <div className="analysis-actions">
              {!node && run.nodes.length > 8 && (
                <span className="muted">
                  Charts show up to eight series. Select a node for its complete
                  process breakdown.
                </span>
              )}
              <button
                onClick={() =>
                  onFocus({
                    at: new Date(
                      Date.parse(data.from) +
                        ((Date.parse(data.to) - Date.parse(data.from)) *
                          cursor) /
                          1000,
                    ).toISOString(),
                    node: node || undefined,
                  })
                }
              >
                Inspect events at cursor
              </button>
              <span className="muted">
                Charts retain the maximum of each{" "}
                {data.bucketSeconds.toFixed(2)} s bucket. Values between samples
                are unknown.
              </span>
            </div>
            <h3>{node ? "Processes" : "Container peaks"}</h3>
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>{node ? "Process / component" : "Node"}</th>
                    <th>CPU peak</th>
                    <th>{node ? "RSS peak" : "Memory peak"}</th>
                    <th>Evidence</th>
                  </tr>
                </thead>
                <tbody>
                  {(node
                    ? processRows
                    : run.nodes.flatMap((n) => {
                        const cpu = containerSeries(false).find(
                          (s) => s.nodeId === n.id,
                        );
                        const ram = containerSeries(true).find(
                          (s) => s.nodeId === n.id,
                        );
                        return cpu || ram ? [{ cpu, ram }] : [];
                      })
                  ).map((row, i) => {
                    const s = row.cpu ?? row.ram!;
                    return (
                      <tr key={s.id ?? i}>
                        <td>
                          {node ? s.name : s.nodeId}
                          <small>
                            {s.component} · {s.source}
                            {s.processId
                              ? ` · ${s.processId.split("/").at(-1)}`
                              : ""}
                          </small>
                        </td>
                        <td>
                          {row.cpu ? valueText(row.cpu.peak, false) : "Unknown"}
                        </td>
                        <td>
                          {row.ram ? valueText(row.ram.peak, true) : "Unknown"}
                        </td>
                        <td>
                          <button
                            onClick={() =>
                              onFocus({ at: s.peakAt, node: s.nodeId })
                            }
                          >
                            Inspect {clock(s.peakAt)}
                          </button>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
            {node && processRows.length === 0 && (
              <p className="empty">
                No process samples for this node. Container observations remain
                available.
              </p>
            )}
            <p className="muted">
              CPU peaks average the measured interval. RSS may count shared
              pages; process values are not a container memory total. A nearby
              workload is an association, not proof of causation.
            </p>
          </>
        )
      )}
    </section>
  );
}

export function FaultPanel({ run }: { run: AnalysisRun }) {
  const [from, setFrom] = useState(run.nodes[0]?.id ?? ""),
    [to, setTo] = useState(run.nodes[1]?.id ?? "");
  const mode = run.definition.network?.mode ?? "ipv4";
  const [family, setFamily] = useState(mode === "ipv6" ? "ipv6" : "ipv4");
  const [traffic, setTraffic] = useState("unicast"),
    [delay, setDelay] = useState(100),
    [jitter, setJitter] = useState(0),
    [loss, setLoss] = useState(0),
    [rate, setRate] = useState(0),
    [duration, setDuration] = useState(30),
    [partition, setPartition] = useState(false);
  const [busy, setBusy] = useState(false),
    [result, setResult] = useState("");
  const enforcer = run.nodes.find(
    (n) => n.id === (traffic === "mdns" ? to : from),
  );
  const faults = (run.runtime?.faults ?? []) as {
    id: string;
    node: string;
    state: string;
    profile: {
      from: string;
      to: string;
      traffic: string;
      family?: string;
      durationSeconds: number;
    };
    started: string;
  }[];
  async function request(path: string, method: string, body?: unknown) {
    setBusy(true);
    setResult("");
    try {
      const response = await fetch(`/api/v1/runs/${run.id}/faults${path}`, {
        method,
        headers: { "Content-Type": "application/json" },
        body: body ? JSON.stringify(body) : undefined,
      });
      const value = await response.json();
      if (!response.ok) throw Error(value.error);
      setResult(
        method === "DELETE"
          ? "Restoration requested. Check the timeline for confirmed cleanup."
          : "Fault requested. The timeline records application and restoration.",
      );
    } catch (e) {
      setResult(String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="analysis-panel">
      <h2>Directional network faults</h2>
      <p className="muted">
        One peer flow inside an owned container namespace. Fault durations use
        real time.
      </p>
      <div className="fault-form">
        <label>
          From
          <select value={from} onChange={(e) => setFrom(e.target.value)}>
            {run.nodes.map((n) => (
              <option key={n.id} value={n.id}>
                {nodeOptionLabel(n)}
              </option>
            ))}
          </select>
        </label>
        <label>
          To
          <select value={to} onChange={(e) => setTo(e.target.value)}>
            {run.nodes.map((n) => (
              <option key={n.id} value={n.id}>
                {nodeOptionLabel(n)}
              </option>
            ))}
          </select>
        </label>
        <label>
          IP family
          <select value={family} onChange={(e) => setFamily(e.target.value)}>
            {mode !== "ipv6" && <option value="ipv4">IPv4</option>}
            {mode !== "ipv4" && <option value="ipv6">IPv6</option>}
          </select>
        </label>
        <label>
          Traffic
          <select value={traffic} onChange={(e) => setTraffic(e.target.value)}>
            <option value="unicast">Unicast · outgoing to peer</option>
            <option value="mdns">mDNS · received from peer</option>
          </select>
        </label>
        {[
          { label: "Delay (ms)", value: delay, set: setDelay, max: 10000 },
          { label: "Jitter (ms)", value: jitter, set: setJitter, max: delay },
          { label: "Loss (%)", value: loss, set: setLoss, max: 100 },
          {
            label: "Rate (kbit/s) · 0 unlimited",
            value: rate,
            set: setRate,
            max: 1000000,
          },
          {
            label: "Duration (real seconds)",
            value: duration,
            set: setDuration,
            max: 600,
          },
        ].map((field) => (
          <label key={field.label}>
            {field.label}
            <input
              type="number"
              min={field.set === setDuration ? 1 : 0}
              max={field.max}
              value={field.value}
              onChange={(e) => field.set(+e.target.value)}
            />
          </label>
        ))}
      </div>
      <label className="fault-partition">
        <input
          type="checkbox"
          checked={partition}
          onChange={(e) => setPartition(e.target.checked)}
        />{" "}
        Partition this flow
      </label>
      {enforcer?.external && (
        <p className="coverage-note">
          This direction would require modifying the physical device’s network.
          Choose a container as the enforcer.
        </p>
      )}
      <button
        className="primary"
        disabled={
          busy ||
          from === to ||
          !!enforcer?.external ||
          !["running", "complete", "testing", "interrupted"].includes(
            run.status,
          )
        }
        onClick={() =>
          void request("", "POST", {
            from,
            to,
            traffic,
            family,
            delayMillis: delay,
            jitterMillis: jitter,
            lossPercent: loss,
            rateKbit: rate,
            partition,
            durationSeconds: duration,
          })
        }
      >
        {busy ? "Applying…" : "Apply fault"}
      </button>
      {result && <p role="status">{result}</p>}
      <h3>Active fault leases</h3>
      {faults.length === 0 ? (
        <p className="empty">No active faults.</p>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th>Direction</th>
                <th>State</th>
                <th>Real duration</th>
                <th>Recovery</th>
              </tr>
            </thead>
            <tbody>
              {faults.map((f) => (
                <tr key={f.id}>
                  <td>
                    {f.profile.from} → {f.profile.to}
                    <small>
                      {f.profile.family === "ipv6" ? "IPv6" : "IPv4"} ·{" "}
                      {f.profile.traffic} · {f.node}
                    </small>
                  </td>
                  <td>{f.state}</td>
                  <td>
                    {f.profile.durationSeconds} s · {clock(f.started)}
                  </td>
                  <td>
                    <button
                      disabled={busy}
                      onClick={() => void request(`/${f.id}`, "DELETE")}
                    >
                      Restore
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

type TimelineEvent = {
  index: number;
  epoch?: string;
  at: string;
  receivedAt?: string;
  sourceAt?: string;
  protocolSeconds?: number;
  nodeId?: string;
  type: string;
  data?: unknown;
};
export function TimelinePanel({
  run,
  focus,
  onClear,
}: {
  run: AnalysisRun;
  focus?: EvidenceFocus;
  onClear: () => void;
}) {
  const [prefix, setPrefix] = useState("");
  const [node, setNode] = useState(focus?.node ?? "");
  useEffect(() => {
    setNode(focus?.node ?? "");
  }, [focus]);
  const query = new URLSearchParams({ limit: "500", prefix, node });
  if (focus) {
    query.set("from", new Date(Date.parse(focus.at) - 15000).toISOString());
    query.set("to", new Date(Date.parse(focus.at) + 15000).toISOString());
  }
  const { data, error, loading } = useEvidence<{
    events: TimelineEvent[];
    truncated: boolean;
    rotated: boolean;
    matched: number;
  }>(
    `/api/v1/runs/${run.id}/timeline?${query}`,
    !["created", "stopped", "archived", "corrupt"].includes(run.status),
  );
  return (
    <section className="analysis-panel">
      <div className="analysis-heading">
        <div>
          <h2>Event timeline</h2>
          <p className="muted">
            UTC receipt time · {run.definition.timeScale}× protocol clock
          </p>
        </div>
        <div className="analysis-controls">
          <label>
            Events
            <select value={prefix} onChange={(e) => setPrefix(e.target.value)}>
              <option value="">All events</option>
              {[
                ["pairing.", "Pairing"],
                ["ship.", "SHIP connections"],
                ["mdns.", "mDNS discovery and traffic"],
                ["resource.", "Resources"],
                ["campaign.", "Campaign"],
                ["fault.", "Faults"],
                ["usecase.", "Use cases"],
              ].map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          <label>
            Node
            <select value={node} onChange={(e) => setNode(e.target.value)}>
              <option value="">All nodes</option>
              {run.nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {nodeOptionLabel(n)}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>
      {focus && (
        <div className="focus-banner">
          <strong>±15 seconds around {utcStamp(focus.at)}</strong>
          <button onClick={onClear}>Show full timeline</button>
        </div>
      )}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {loading && <p role="status">Loading retained events…</p>}
      {data?.truncated && (
        <p className="coverage-note">
          Showing the latest 500 of {data.matched} matching events. Narrow the
          node or event filter.
        </p>
      )}
      {data?.rotated && (
        <p className="coverage-note">
          The evidence journal has rotated. Earlier events may be outside
          retention.
        </p>
      )}
      {data && data.events.length === 0 && (
        <p className="empty">No matching retained events in this interval.</p>
      )}
      <div className="retained-timeline">
        {data?.events.map((e, i) => (
          <div key={`${e.epoch}/${e.index}/${i}`}>
            <time title={e.receivedAt ?? e.at}>
              {utcStamp(e.receivedAt ?? e.at)}
              {e.protocolSeconds != null && (
                <small>+{e.protocolSeconds.toFixed(2)} virtual s</small>
              )}
            </time>
            <strong>{e.nodeId ?? "Arena"}</strong>
            <span>{e.type}</span>
            <details>
              <summary>Payload</summary>
              <pre>{JSON.stringify(e.data, null, 2)}</pre>
              <small>Native timestamp: {e.sourceAt ?? e.at}</small>
            </details>
          </div>
        ))}
      </div>
    </section>
  );
}
