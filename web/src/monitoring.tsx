import React, { useState } from "react";

type Reading = {
  available: boolean;
  value?: number;
  unit: string;
  error?: string;
  nativeError?: number;
};
type Measurements = Record<string, Reading>;
type Monitoring = {
  offering: string;
  local: Measurements;
  remote: Record<string, Record<string, Measurements>>;
};

export function MonitoringPanel({
  node,
  peers,
  disabled,
  command,
}: {
  node: {
    id: string;
    monitoring?: string;
    capabilities: string[];
    snapshot: Record<string, any>;
    snapshotAt: string;
    monitoringObservation?: {
      at: string;
      incarnation: string;
      values: Record<string, any>;
    };
  };
  peers: { id: string; ski: string }[];
  disabled: boolean;
  command: (name: string, params: unknown) => Promise<void>;
}) {
  const [metric, setMetric] = useState("power");
  const [value, setValue] = useState("1200");
  const [state, setState] = useState("normal");
  const live = node.snapshot.monitoring as Monitoring | undefined;
  const observed =
    live ?? (node.monitoringObservation?.values as Monitoring | undefined);
  const observedAt = live ? node.snapshotAt : node.monitoringObservation?.at;
  const rows: {
    origin: string;
    usecase: string;
    metric: string;
    reading: Reading;
  }[] = [];
  const append = (origin: string, usecase: string, values?: Measurements) => {
    for (const [metric, reading] of Object.entries(values ?? {}))
      rows.push({ origin, usecase, metric, reading });
  };
  append("Local", observed?.offering ?? node.monitoring ?? "", observed?.local);
  for (const [ski, usecases] of Object.entries(observed?.remote ?? {})) {
    const name = peers.find((peer) => peer.ski === ski)?.id ?? ski;
    for (const [usecase, values] of Object.entries(usecases))
      append(name, usecase, values);
  }
  const numeric = Number(value);
  const valid =
    value.trim() !== "" &&
    Number.isFinite(numeric) &&
    Math.abs(numeric) <= 1e12 &&
    (metric !== "energyConsumed" || numeric >= 0) &&
    (metric !== "energyProduced" || numeric <= 0);
  const names: Record<string, string> = {
    power: "Power",
    energyConsumed: "Consumed energy",
    energyProduced: "Produced energy",
  };
  return (
    <section className="inspector-block">
      <h3>Power monitoring</h3>
      {rows.length ? (
        <div
          className="table-scroll"
          tabIndex={0}
          aria-label="Native power readings"
        >
          <table>
            <caption>
              {live ? "Native readings" : "Saved native readings"} ·{" "}
              {observedAt
                ? new Date(observedAt).toLocaleTimeString()
                : "waiting"}
            </caption>
            <thead>
              <tr>
                <th>Source</th>
                <th>Use case</th>
                <th>Measurement</th>
                <th>Value</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={`${row.origin}/${row.usecase}/${row.metric}`}>
                  <td>{row.origin}</td>
                  <td>{row.usecase.toUpperCase()}</td>
                  <td>{names[row.metric] ?? row.metric}</td>
                  <td
                    title={
                      row.reading.error ??
                      (row.reading.nativeError !== undefined
                        ? `Native error ${row.reading.nativeError}`
                        : undefined)
                    }
                  >
                    {row.reading.available && row.reading.value !== undefined
                      ? `${row.reading.value.toLocaleString()} ${row.reading.unit}`
                      : "Unavailable"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="muted">Waiting for native monitoring observations.</p>
      )}
      {node.capabilities.includes("measurement.set") && (
        <>
          <div className="fields compact">
            <label>
              Measurement
              <select
                value={metric}
                onChange={(event) => {
                  const next = event.target.value;
                  setMetric(next);
                  if (next === "energyProduced" && numeric > 0)
                    setValue(String(-numeric));
                  else if (next === "energyConsumed" && numeric < 0)
                    setValue(String(-numeric));
                }}
              >
                <option value="power">Power (W)</option>
                <option value="energyConsumed">Consumed energy (Wh)</option>
                <option value="energyProduced">Produced energy (Wh)</option>
              </select>
            </label>
            <label>
              Value
              <input
                type="number"
                value={value}
                onChange={(event) => setValue(event.target.value)}
                aria-invalid={!valid}
              />
            </label>
            <label>
              Value state
              <select
                value={state}
                onChange={(event) => setState(event.target.value)}
              >
                <option value="normal">Normal</option>
                <option value="outOfRange">Out of range</option>
                <option value="error">Error</option>
              </select>
            </label>
          </div>
          <p className="muted">
            Simulated input. Consumption is positive; production is negative.
          </p>
        </>
      )}
      <div className="node-actions">
        {node.capabilities.includes("measurement.set") && (
          <button
            disabled={disabled || !valid}
            onClick={() =>
              command("measurement.set", { metric, value: numeric, state })
            }
          >
            Publish reading
          </button>
        )}
        <button
          disabled={disabled || !node.capabilities.includes("measurement.read")}
          onClick={() => command("measurement.read", {})}
        >
          Read native values
        </button>
      </div>
    </section>
  );
}
