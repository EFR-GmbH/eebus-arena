import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";
import "./themes.css";
import { MonitoringPanel } from "./monitoring";
import {
  ResourcePanel,
  TimelinePanel,
  FaultPanel,
  type EvidenceFocus,
} from "./analysis";

type Variant = {
  builtAt?: string;
  id: string;
  stack: string;
  label: string;
  roles: string[];
  image: string;
  provenance: {
    arenaFeatures?: { monitoringProviders?: string[]; networkModes?: string[] };
  };
};
type StressProfile = {
  family?: string;
  version: number;
  seed: number;
  services: number;
  rate: number;
  burst: number;
  churn: number;
  ttl: number;
  durationSeconds: number;
  pattern: string;
  service: string;
  forId?: string;
  forPar?: string;
  replayId?: string;
};
const defaultStress: StressProfile = {
  version: 1,
  seed: 42,
  services: 100,
  rate: 100,
  burst: 100,
  churn: 20,
  ttl: 120,
  durationSeconds: 60,
  pattern: "churn",
  service: "_ship._tcp",
};
type Group = {
  id: string;
  variant: string;
  role: string;
  count: number;
  memoryMB?: number;
  resources?: { cpuMillis?: number; memoryMiB?: number; pidsLimit?: number };
  external?: {
    label: string;
    endpoint: string;
    tlsFingerprint: string;
    token: string;
    interface: string;
    address?: string;
    ipv6Address?: string;
  };
  stress?: StressProfile;
  monitoring?: string;
};
type Definition = {
  schemaVersion: number;
  name: string;
  seed: number;
  timeScale: number;
  timingProfile?: string;
  scenario: string;
  durationSeconds: number;
  groups: Group[];
  steps?: unknown[];
  links?: { from: string; to: string; mode?: string }[];
  nodeOverrides?: Record<
    string,
    {
      resources: { cpuMillis?: number; memoryMiB?: number; pidsLimit?: number };
      monitoring?: string;
    }
  >;
  sampling?: { intervalMillis: number; diagnosticNodes?: string[] };
  campaign?: unknown;
  exploration?: unknown;
  network?: {
    mode?: string;
    interface?: string;
    subnet?: string;
    ipRange?: string;
    ipv6Subnet?: string;
    ipv6Range?: string;
  };
};
type Node = {
  monitoring?: string;
  id: string;
  group: string;
  stack: string;
  variant: string;
  role: string;
  ski: string;
  trustedSkis: string[];
  intendedPeers?: string[];
  pairingPeers?: string[];
  pairingProgress?: Record<
    string,
    { commandId: string; started: string; stages: Record<string, number> }
  >;
  shipId?: string;
  fingerprint?: string;
  lifecycle: string;
  capabilities: string[];
  counters?: Record<string, number>;
  container?: string;
  snapshot: Record<string, any>;
  snapshotAt: string;
  monitoringObservation?: {
    at: string;
    incarnation: string;
    values: Record<string, any>;
  };
  metrics: Record<string, any>;
  resources?: Record<string, number>;
  effectiveResources?: Record<string, any>;
  processMetrics?: { intervalSeconds?: number; collectionMillis?: number };
  resourceAt?: string;
  external?: { label: string; interface: string };
  lastSeen: string;
  incarnation: string;
};
type Run = {
  id: string;
  setupRevision?: number;
  definition: Definition;
  status: string;
  created: string;
  runtime?: Record<string, any>;
  nodes: Node[];
  assertions: {
    id: string;
    node: string;
    outcome: string;
    detail: string;
    reference: string;
    at: string;
  }[];
  error?: string;
};
type Event = {
  epoch?: string;
  index: number;
  at: string;
  runId?: string;
  nodeId?: string;
  type: string;
  commandId?: string;
  data?: Record<string, any>;
};
type State = {
  catalogErrors?: string[];
  runs: Record<string, Run>;
  variants: Record<string, Variant>;
  cursor: number;
};
const empty: State = { runs: {}, variants: {}, cursor: 0 };
const stackLabels: Record<string, string> = {
  enbility: "enbility",
  openeebus: "OpenEEBUS",
  jeebus: "jEEBUS",
  "mdns-stresser": "mDNS stresser",
};

function StackCue({ stack }: { stack: string }) {
  return (
    <svg
      className={`stack-cue stack-${stack}`}
      viewBox="-16 -16 32 32"
      aria-hidden="true"
    >
      <circle className="node-shape" r={12} />
    </svg>
  );
}

async function api(path: string, body?: unknown, method = "POST") {
  const response = await fetch(
    "/api/v1" + path,
    body === undefined
      ? {}
      : {
          method,
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        },
  );
  const text = await response.text();
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    throw new Error(
      text.trim() ||
        `Controller returned ${response.status}. Retry after it reconnects.`,
    );
  }
  if (!response.ok) throw new Error(value.error ?? response.statusText);
  return value;
}
const cpu = (n: number) =>
  Number.isFinite(n) ? n.toFixed(1) + "%" : "Unavailable";
const bytes = (n: number) =>
  Number.isFinite(n) ? (n / 1024 / 1024).toFixed(1) + " MiB" : "Unavailable";
function sampled(node: Node, key: string, format: (n: number) => string) {
  if (!node.metrics.at) return "Unavailable";
  if (
    !["running", "paused"].includes(node.lifecycle) ||
    Date.now() - Date.parse(node.metrics.at) > 10000
  )
    return "Stale";
  return format(node.metrics[key]);
}
function lifecycleAllowed(node: Node, action: string) {
  if (node.external) {
    if (["pause", "resume"].includes(action)) return false;
    if (action === "start")
      return ["exited", "created"].includes(node.lifecycle);
    return (
      node.lifecycle === "running" &&
      ["stop", "restart", "kill"].includes(action)
    );
  }
  if (!node.container) return false;
  if (action === "start") return ["created", "exited"].includes(node.lifecycle);
  if (action === "resume") return node.lifecycle === "paused";
  if (action === "pause") return node.lifecycle === "running";
  if (action === "restart") return true;
  return ["running", "paused"].includes(node.lifecycle);
}
function nextGroupID(groups: Group[], prefix = "cs") {
  const used = new Set(groups.map((g) => g.id));
  let i = 1;
  while (used.has(`${prefix}-${i}`)) ++i;
  return `${prefix}-${i}`;
}
function canDrop(run: Run) {
  return (
    ["created", "stopped", "corrupt", "archived"].includes(run.status) &&
    !run.nodes.some((node) => node.container)
  );
}
function canEdit(run: Run) {
  return (
    ["created", "stopped"].includes(run.status) &&
    !run.error &&
    !run.nodes.some(
      (node) => node.container || node.lifecycle === "unavailable",
    )
  );
}
function App() {
  const [state, setState] = useState<State>(empty),
    [runId, setRunId] = useState(""),
    [nodeId, setNodeId] = useState(""),
    [events, setEvents] = useState<Event[]>([]),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [connected, setConnected] = useState(false),
    [view, setView] = useState("map"),
    [tab, setTab] = useState("nodes"),
    [filter, setFilter] = useState(""),
    [sort, setSort] = useState("id"),
    [creating, setCreating] = useState(true);
  const [managing, setManaging] = useState(false);
  const [editing, setEditing] = useState<{
    id: string;
    revision: number;
  } | null>(null);
  const [dropSelection, setDropSelection] = useState<string[]>([]);
  const [lastDropped, setLastDropped] = useState<string[]>(() => {
    try {
      const saved = JSON.parse(sessionStorage.getItem("arena-dropped") ?? "[]");
      return Array.isArray(saved)
        ? saved.filter((id) => typeof id === "string")
        : [];
    } catch {
      return [];
    }
  });
  useEffect(() => {
    try {
      sessionStorage.setItem("arena-dropped", JSON.stringify(lastDropped));
    } catch {
      /* Undo still works without browser storage. */
    }
  }, [lastDropped]);
  const [draft, setDraft] = useState<Definition>({
    schemaVersion: 3,
    name: "LPC interoperability",
    seed: 42,
    timeScale: 1,
    scenario: "lpc",
    durationSeconds: 1800,
    groups: [],
  });
  const [evidenceFocus, setEvidenceFocus] = useState<EvidenceFocus>();
  const [jsonErrors, setJsonErrors] = useState<Record<string, string>>({});
  const [variantFilter, setVariantFilter] = useState("");
  const [value, setValue] = useState(1200),
    [usecase, setUsecase] = useState("lpc"),
    [target, setTarget] = useState(""),
    [interval, setHeartbeatInterval] = useState(60),
    [commandResult, setCommandResult] = useState(""),
    [resultNode, setResultNode] = useState("");
  const file = useRef<HTMLInputElement>(null),
    initialized = useRef(false);
  const refreshSerial = useRef(0);
  const refresh = async () => {
    const serial = ++refreshSerial.current;
    try {
      const next = await api("/state");
      if (serial === refreshSerial.current) setState(next);
    } catch (e) {
      if (serial === refreshSerial.current) setError((e as Error).message);
    }
  };
  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 3000);
    return () => {
      clearInterval(timer);
      ++refreshSerial.current;
    };
  }, []);
  useEffect(() => {
    const stream = new EventSource("/api/v1/events");
    stream.onopen = () => setConnected(true);
    stream.onerror = () => setConnected(false);
    let pending: Event[] = [];
    const flush = setInterval(() => {
      if (!pending.length) return;
      const batch = pending;
      pending = [];
      setEvents((old) => {
        const keys = new Set(old.map((e) => `${e.epoch}/${e.index}/${e.type}`));
        return [
          ...old,
          ...batch.filter((e) => {
            const key = `${e.epoch}/${e.index}/${e.type}`;
            if (keys.has(key)) return false;
            keys.add(key);
            return true;
          }),
        ].slice(-1500);
      });
    }, 100);
    stream.onmessage = (e) => {
      const event: Event = JSON.parse(e.data);
      if (event.type === "metrics" || event.type === "snapshot") return;
      pending.push(event);
    };
    return () => {
      clearInterval(flush);
      stream.close();
    };
  }, []);
  const variants = useMemo(
    () =>
      Object.values(state.variants).sort((a, b) =>
        (b.builtAt ?? "").localeCompare(a.builtAt ?? ""),
      ),
    [state.variants],
  );
  const runs = useMemo(
    () =>
      Object.values(state.runs).sort((a, b) =>
        b.created.localeCompare(a.created),
      ),
    [state.runs],
  );
  const variantSearch = useMemo(
    () =>
      new Map(
        variants.map((v) => [
          v.id,
          `${v.id} ${v.label} ${JSON.stringify(v.provenance)}`.toLowerCase(),
        ]),
      ),
    [variants],
  );
  const matchingVariants = useMemo(
    () =>
      new Set(
        variants
          .filter(
            (v) =>
              !variantFilter ||
              variantSearch.get(v.id)!.includes(variantFilter.toLowerCase()),
          )
          .map((v) => v.id),
      ),
    [variants, variantSearch, variantFilter],
  );
  const run = state.runs[runId],
    node = run?.nodes.find((n) => n.id === nodeId);
  function beginEdit() {
    if (!run || !canEdit(run)) return;
    setDraft(structuredClone(run.definition));
    setEditing({ id: run.id, revision: run.setupRevision ?? 0 });
    setJsonErrors({});
    setVariantFilter("");
    setError("");
    setNodeId("");
    setCreating(true);
  }
  function cancelEdit() {
    if (editing) setRunId(editing.id);
    setEditing(null);
    setJsonErrors({});
    setError("");
    setCreating(false);
  }
  useEffect(() => {
    setDropSelection((old) => {
      const next = old.filter(
        (id) => state.runs[id] && canDrop(state.runs[id]),
      );
      return next.length === old.length ? old : next;
    });
  }, [state.runs]);
  function preset(stress: boolean) {
    setVariantFilter("");
    setJsonErrors({});
    const eg =
      variants.find((v) => v.stack === "enbility" && v.roles.includes("EG")) ??
      variants.find((v) => v.roles.includes("EG"));
    if (!eg) return;
    const cs = variants
      .filter((v) => v.roles.includes("CS"))
      .filter((v, i, all) => all.findIndex((x) => x.stack === v.stack) === i);
    setDraft((old) => ({
      ...old,
      name: stress ? "51-node mixed arena" : "LPC interoperability",
      scenario: stress ? "stress" : "lpc",
      steps: undefined,
      links: undefined,
      nodeOverrides: undefined,
      groups: [
        { id: "eg", variant: eg.id, role: "EG", count: 1 },
        ...cs.map((v, i) => ({
          id: "cs-" + v.stack,
          variant: v.id,
          role: "CS",
          count: stress
            ? Math.floor(50 / cs.length) + (i < 50 % cs.length ? 1 : 0)
            : 1,
        })),
      ],
    }));
  }
  useEffect(() => {
    if (variants.length && !initialized.current) {
      initialized.current = true;
      preset(false);
      if (runs.length) {
        setRunId(runs[0].id);
        setCreating(false);
      }
    }
  }, [variants.length]);
  useEffect(() => {
    setCommandResult("");
    setTarget("");
  }, [nodeId, runId]);
  function updateJSON(
    field: "steps" | "links" | "nodeOverrides" | "campaign" | "exploration",
    text: string,
  ) {
    try {
      const parsed = text ? JSON.parse(text) : undefined;
      const object = ["nodeOverrides", "campaign", "exploration"].includes(
        field,
      );
      if (
        parsed !== undefined &&
        (object
          ? typeof parsed !== "object" ||
            Array.isArray(parsed) ||
            parsed === null
          : !Array.isArray(parsed))
      )
        throw new Error("Invalid value");
      setDraft((old) => ({ ...old, [field]: parsed }));
      setJsonErrors((old) => ({ ...old, [field]: "" }));
    } catch {
      setJsonErrors((old) => ({
        ...old,
        [field]: ["nodeOverrides", "campaign", "exploration"].includes(field)
          ? "Enter a valid JSON object."
          : "Enter a valid JSON array.",
      }));
    }
  }
  async function act(work: () => Promise<unknown>) {
    setError("");
    setBusy(true);
    try {
      await work();
      await refresh();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function dropRuns(ids: string[]) {
    const selected = runs.filter((r) => ids.includes(r.id));
    if (!selected.length || selected.some((r) => !canDrop(r))) return;
    const names = selected
      .map((r) => `${r.definition.name} · ${r.id.slice(-6)}`)
      .join("\n");
    if (
      !window.confirm(
        `Drop ${selected.length === 1 ? "this arena" : `${selected.length} arenas`}?\n\n${names}\n\nThey will leave the saved list. Identities and evidence are preserved; Undo drop restores them.`,
      )
    )
      return;
    await act(async () => {
      const dropped: string[] = [],
        failures: string[] = [];
      for (const r of selected) {
        try {
          await api(`/runs/${r.id}`, {}, "DELETE");
          dropped.push(r.id);
        } catch (e) {
          failures.push(`${r.definition.name}: ${(e as Error).message}`);
        }
      }
      if (dropped.length) {
        setLastDropped((old) => [...new Set([...dropped, ...old])]);
        setDropSelection((old) => old.filter((id) => !dropped.includes(id)));
        setEvents((old) =>
          old.filter((event) => !dropped.includes(event.runId ?? "")),
        );
        if (dropped.includes(runId)) {
          const next = runs.find((r) => !dropped.includes(r.id));
          setRunId(next?.id ?? "");
          setNodeId("");
          setCreating(!next);
        }
      }
      await refresh();
      if (failures.length) throw new Error(failures.join("\n"));
    });
  }
  async function undoDrop() {
    await act(async () => {
      const restored: string[] = [],
        failures: string[] = [];
      for (const id of lastDropped) {
        try {
          if (!state.runs[id]) await api(`/runs/${id}/restore`, {});
          restored.push(id);
        } catch (e) {
          failures.push(`${id.slice(-6)}: ${(e as Error).message}`);
        }
      }
      setLastDropped((old) => old.filter((id) => !restored.includes(id)));
      if (restored.length) {
        setRunId(restored[0]);
        setNodeId("");
        setCreating(false);
      }
      await refresh();
      if (failures.length) throw new Error(failures.join("\n"));
    });
  }
  function supportsMonitoring(variant: string, selection: string) {
    return (
      state.variants[
        variant
      ]?.provenance?.arenaFeatures?.monitoringProviders?.includes(selection) ??
      false
    );
  }
  const draftNodes = draft.groups.flatMap((g) =>
    Array.from({ length: Math.max(0, Math.min(200, g.count)) }, (_, i) => ({
      id: `${g.id}-${String(i + 1).padStart(3, "0")}`,
      group: g,
    })),
  );
  const mgcpNodes = draftNodes.filter(
    (n) =>
      (draft.nodeOverrides?.[n.id]?.monitoring ?? n.group.monitoring) ===
      "mgcp",
  );
  function setMGCP(nodeID: string) {
    setDraft((old) => {
      const overrides = { ...old.nodeOverrides };
      for (const [id, override] of Object.entries(overrides))
        if (override.monitoring === "mgcp") {
          overrides[id] = { ...override };
          delete overrides[id].monitoring;
        }
      if (nodeID)
        overrides[nodeID] = {
          ...overrides[nodeID],
          resources: overrides[nodeID]?.resources ?? {},
          monitoring: "mgcp",
        };
      return {
        ...old,
        groups: old.groups.map((g) =>
          g.monitoring === "mgcp" ? { ...g, monitoring: "none" } : g,
        ),
        nodeOverrides: overrides,
      };
    });
  }
  function nodeIndex(id: string, group: string) {
    if (!id.startsWith(group + "-")) return null;
    const suffix = id.slice(group.length + 1);
    return /^[0-9]{3}$/.test(suffix) ? suffix : null;
  }
  function removeGroup(index: number) {
    setDraft((old) => ({
      ...old,
      groups: old.groups.filter((_, i) => i !== index),
      nodeOverrides: Object.fromEntries(
        Object.entries(old.nodeOverrides ?? {}).filter(
          ([id]) => nodeIndex(id, old.groups[index].id) === null,
        ),
      ),
      links: old.links?.filter(
        (l) => l.from !== old.groups[index].id && l.to !== old.groups[index].id,
      ),
    }));
  }
  function changeGroup(index: number, change: Partial<Group>) {
    setDraft((old) => ({
      ...old,
      nodeOverrides: Object.fromEntries(
        Object.entries(old.nodeOverrides ?? {}).flatMap(([id, override]) => {
          const group = old.groups[index],
            next = { ...group, ...change };
          const slot = nodeIndex(id, group.id);
          if (slot === null) return [[id, override]];
          if (+slot > next.count) return [];
          const adjusted = { ...override };
          if (
            (adjusted.monitoring === "mgcp" && next.role !== "EG") ||
            (adjusted.monitoring &&
              adjusted.monitoring !== "none" &&
              !supportsMonitoring(next.variant, adjusted.monitoring))
          )
            delete adjusted.monitoring;
          return [[`${next.id}-${slot}`, adjusted]];
        }),
      ),
      groups: old.groups.map((g, i) =>
        i === index
          ? {
              ...g,
              ...change,
              monitoring:
                change.role === "STRESSER" ||
                (change.role &&
                  change.role !== "EG" &&
                  g.monitoring === "mgcp") ||
                (change.variant &&
                  g.monitoring &&
                  g.monitoring !== "none" &&
                  !supportsMonitoring(change.variant, g.monitoring))
                  ? "none"
                  : (change.monitoring ?? g.monitoring),
              stress:
                change.role && change.role !== "STRESSER"
                  ? undefined
                  : g.stress,
              ...(change.stress ? { stress: change.stress } : {}),
            }
          : g,
      ),
      links: old.links
        ?.map((l) => ({
          ...l,
          from:
            change.id !== undefined && l.from === old.groups[index].id
              ? change.id
              : l.from,
          to:
            change.id !== undefined && l.to === old.groups[index].id
              ? change.id
              : l.to,
        }))
        .filter(
          (l) =>
            change.role !== "STRESSER" ||
            (l.from !== (change.id ?? old.groups[index].id) &&
              l.to !== (change.id ?? old.groups[index].id)),
        ),
    }));
  }
  const total = draft.groups.reduce((n, g) => n + g.count, 0),
    passed = run?.assertions.filter((a) => a.outcome === "pass").length ?? 0,
    failed = run?.assertions.filter((a) => a.outcome === "fail").length ?? 0;
  const editTarget = editing ? state.runs[editing.id] : undefined;
  const editIssue = !editing
    ? ""
    : !editTarget
      ? "This arena is no longer available. Cancel to leave the editor."
      : (editTarget.setupRevision ?? 0) !== editing.revision
        ? "This setup changed in another session. Cancel and reopen Edit arena."
        : !canEdit(editTarget)
          ? "This arena is active or cleanup is incomplete. Stop it before saving."
          : "";
  const filtered = useMemo(
    () =>
      [...(run?.nodes ?? [])]
        .filter(
          (n) =>
            !filter ||
            (
              n.id +
              " " +
              n.stack +
              " " +
              n.role +
              " " +
              n.lifecycle +
              " " +
              JSON.stringify(n.snapshot)
            )
              .toLowerCase()
              .includes(filter.toLowerCase()),
        )
        .sort((a, b) =>
          sort === "cpu"
            ? b.metrics.cpuPercent - a.metrics.cpuPercent
            : sort === "memory"
              ? b.metrics.memoryBytes - a.metrics.memoryBytes
              : a.id.localeCompare(b.id),
        ),
    [run?.nodes, filter, sort],
  );
  async function command(name: string, params: unknown) {
    if (!node || !run) return;
    await act(async () => {
      const result = await api(`/runs/${run.id}/nodes/${node.id}/commands`, {
        command: name,
        params,
      });
      setResultNode(run.id + "/" + node.id);
      setCommandResult(JSON.stringify(result.data, null, 2));
    });
  }
  return (
    <>
      <header>
        <a className="brand arena-logo" href="/" aria-label="EEBUS Arena">
          <svg
            className="logo-mark"
            viewBox="0 0 72 50"
            aria-hidden="true"
            focusable="false"
          >
            <path
              fill="currentColor"
              fillRule="evenodd"
              d="M5 13C5 0 67 0 67 13V35C67 52 5 52 5 35ZM13 13C13 8 59 8 59 13C59 18 13 18 13 13ZM11 36V26.5a2.5 2.5 0 0 1 5 0V36ZM21 39V27.0a3.0 3.0 0 0 1 6 0V39ZM33 40V27.0a3.0 3.0 0 0 1 6 0V40ZM45 39V27.0a3.0 3.0 0 0 1 6 0V39ZM57 36V26.5a2.5 2.5 0 0 1 5 0V36Z"
            />
          </svg>
          <span className="logo-wordmark">
            EEBUS <span className="logo-arena">Arena</span>
          </span>
        </a>
        <span className={"connection " + (connected ? "live" : "")}>
          {connected ? "Live connection" : "Reconnecting…"}
        </span>
        <div className="header-actions">
          <button
            disabled={busy}
            onClick={() => {
              setEditing(null);
              setJsonErrors({});
              setCreating(true);
              setNodeId("");
            }}
          >
            New arena
          </button>
          <button
            disabled={busy || !!editing}
            onClick={() => file.current?.click()}
            title="Import setup or reopen a saved evidence archive"
          >
            Import
          </button>
        </div>
      </header>
      <div className="workspace">
        <nav aria-label="Saved arenas">
          <div className="arena-list-heading">
            <h2>Arenas</h2>
            {runs.length > 0 && (
              <button
                disabled={busy}
                aria-pressed={managing}
                onClick={() => {
                  setManaging(!managing);
                  setDropSelection([]);
                }}
              >
                {managing ? "Done" : "Select"}
              </button>
            )}
          </div>
          {managing && (
            <div className="arena-list-tools">
              <label className="arena-select-all">
                <input
                  type="checkbox"
                  disabled={busy || !runs.some(canDrop)}
                  checked={
                    runs.some(canDrop) &&
                    runs
                      .filter(canDrop)
                      .every((r) => dropSelection.includes(r.id))
                  }
                  onChange={(e) =>
                    setDropSelection(
                      e.target.checked
                        ? runs.filter(canDrop).map((r) => r.id)
                        : [],
                    )
                  }
                />
                Select all inactive
              </label>
              <button
                className="danger"
                disabled={
                  busy ||
                  !dropSelection.length ||
                  dropSelection.some(
                    (id) => !state.runs[id] || !canDrop(state.runs[id]),
                  )
                }
                onClick={() => dropRuns(dropSelection)}
              >
                Drop selected ({dropSelection.length})
              </button>
              <p className="muted">Stop running arenas before dropping them.</p>
            </div>
          )}
          {lastDropped.length > 0 && (
            <button className="undo-drop" disabled={busy} onClick={undoDrop}>
              Undo drop ({lastDropped.length})
            </button>
          )}
          {runs.length === 0 ? (
            <p className="muted">Your experiments will appear here.</p>
          ) : (
            runs.map((r) => (
              <div className="arena-list-row" key={r.id}>
                {managing && (
                  <input
                    type="checkbox"
                    aria-label={`Select ${r.definition.name} · ${r.id.slice(-6)}`}
                    disabled={busy || !canDrop(r)}
                    title={
                      canDrop(r)
                        ? "Select arena to drop"
                        : "Stop this arena first"
                    }
                    checked={dropSelection.includes(r.id)}
                    onChange={(e) =>
                      setDropSelection((old) =>
                        e.target.checked
                          ? [...old, r.id]
                          : old.filter((id) => id !== r.id),
                      )
                    }
                  />
                )}
                <button
                  disabled={busy}
                  className={
                    "run-link " +
                    (runId === r.id && !creating ? "selected" : "")
                  }
                  onClick={() => {
                    setEditing(null);
                    setJsonErrors({});
                    setRunId(r.id);
                    setNodeId("");
                    setCreating(false);
                  }}
                >
                  <strong>{r.definition.name}</strong>
                  <span>
                    {r.nodes.length} nodes · {r.status} · {r.id.slice(-6)}
                  </span>
                </button>
              </div>
            ))
          )}
          <input
            ref={file}
            type="file"
            accept="application/json,.json,application/gzip,.tar.gz,.tgz"
            hidden
            onChange={(e) => {
              const f = e.target.files?.[0];
              if (f)
                act(async () => {
                  let result;
                  if (/\.(tar\.gz|tgz)$/i.test(f.name)) {
                    const response = await fetch("/api/v1/evidence", {
                      method: "POST",
                      body: f,
                      headers: { "Content-Type": "application/gzip" },
                    });
                    result = await response.json();
                    if (!response.ok)
                      throw Error(result.error ?? response.statusText);
                  } else {
                    const bundle = JSON.parse(await f.text());
                    result = await api("/runs", bundle);
                  }
                  setRunId(result.id);
                  setCreating(false);
                });
              e.target.value = "";
            }}
          />
          <button
            disabled={busy}
            onClick={() => act(() => api("/variants/reload", {}))}
          >
            Reload variants
          </button>
        </nav>
        <main>
          {error && (
            <div role="alert" className="error">
              {error}
              <button onClick={() => setError("")} aria-label="Dismiss error">
                Dismiss
              </button>
            </div>
          )}
          {!!state.catalogErrors?.length && (
            <div role="alert" className="error">
              Some catalog entries could not be loaded.{" "}
              {state.catalogErrors.join("; ")}
            </div>
          )}
          {creating ? (
            <section className="setup">
              <div className="setup-heading">
                <h1>{editing ? "Edit arena" : "Set up an arena"}</h1>
                {editing && (
                  <button disabled={busy} onClick={cancelEdit}>
                    Cancel
                  </button>
                )}
              </div>
              {editing && (
                <p className="muted">
                  Existing node IDs keep their identities. Previous results stay
                  in the evidence; save, then start the updated arena.
                </p>
              )}
              {editIssue && (
                <p role="alert" className="error">
                  {editIssue}
                </p>
              )}
              {variants.length === 0 ? (
                <div className="empty">
                  <h2>No stack images registered</h2>
                  <p>Build the pinned images, then reload the catalog.</p>
                  <code>python3 scripts/arena.py build</code>
                  <button
                    disabled={busy}
                    onClick={() => act(() => api("/variants/reload", {}))}
                  >
                    Reload variants
                  </button>
                </div>
              ) : (
                <>
                  {!editing && (
                    <div className="preset">
                      <button onClick={() => preset(false)}>
                        Small interoperability arena
                      </button>
                      <button onClick={() => preset(true)}>
                        1 EG + 50 mixed CS
                      </button>
                    </div>
                  )}
                  <div className="fields">
                    <label>
                      Arena name
                      <input
                        value={draft.name}
                        maxLength={120}
                        onChange={(e) =>
                          setDraft({ ...draft, name: e.target.value })
                        }
                      />
                    </label>
                    <label>
                      Scenario
                      <select
                        value={draft.scenario}
                        onChange={(e) => {
                          const scenario = e.target.value;
                          setJsonErrors({});
                          const eg = draft.groups.find((g) => g.role === "EG");
                          setDraft({
                            ...draft,
                            scenario,
                            campaign:
                              scenario === "campaign"
                                ? (draft.campaign ?? {
                                    lanes: [
                                      {
                                        id: "baseline",
                                        domain: "real",
                                        steps: [
                                          {
                                            name: "Observe baseline",
                                            action: "wait",
                                            seconds: 5,
                                          },
                                        ],
                                      },
                                    ],
                                  })
                                : undefined,
                            exploration:
                              scenario === "explore"
                                ? (draft.exploration ?? {
                                    rounds: 10,
                                    settleSeconds: 2,
                                    actions: [],
                                    invariants: [],
                                  })
                                : undefined,
                            timeScale: ["pairing", "mdns-stress"].includes(
                              scenario,
                            )
                              ? 1
                              : draft.timeScale,
                            links:
                              scenario === "pairing" && eg
                                ? (draft.links?.length
                                    ? draft.links
                                    : draft.groups
                                        .filter((g) => g.role === "CS")
                                        .map((g) => ({ from: eg.id, to: g.id }))
                                  ).map((l) => ({ ...l, mode: "ship-pairing" }))
                                : draft.links,
                          });
                        }}
                      >
                        {[
                          ["manual", "Manual exploration"],
                          ["discovery", "Discovery and trust"],
                          ["lpc", "LPC limits"],
                          ["lpp", "LPP limits"],
                          ["heartbeat", "Heartbeat loss"],
                          ["restart", "Restart recovery"],
                          ["stress", "Stress workload"],
                          ["pairing", "SHIP pairing and readiness"],
                          ["monitoring", "MGCP / MPC measurements"],
                          ["mdns-stress", "mDNS load and recovery"],
                          ["custom", "Custom steps"],
                          ["campaign", "Concurrent campaign"],
                          ["explore", "Stateful exploration"],
                        ].map(([id, title]) => (
                          <option key={id} value={id}>
                            {title}
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      Timing
                      <select
                        value={draft.timeScale}
                        onChange={(e) =>
                          setDraft({ ...draft, timeScale: +e.target.value })
                        }
                      >
                        <option value={1}>Real time</option>
                        {[2, 5, 10].map((rate) => (
                          <option key={rate} value={rate}>
                            {rate}× · full clock qualification required
                          </option>
                        ))}
                      </select>
                    </label>
                    <label>
                      IP mode
                      <select
                        value={draft.network?.mode ?? "ipv4"}
                        onChange={(e) => {
                          const mode = e.target.value;
                          setDraft((old) => ({
                            ...old,
                            network: {
                              ...old.network,
                              mode,
                              ...(mode === "ipv6"
                                ? { subnet: undefined, ipRange: undefined }
                                : {}),
                              ...(mode === "ipv4"
                                ? {
                                    ipv6Subnet: undefined,
                                    ipv6Range: undefined,
                                  }
                                : {}),
                            },
                          }));
                        }}
                      >
                        <option value="ipv4">IPv4</option>
                        <option value="ipv6">IPv6 only</option>
                        <option value="dual-stack">Dual stack</option>
                      </select>
                    </label>
                    <label>
                      MGCP provider
                      <select
                        value={mgcpNodes.length === 1 ? mgcpNodes[0].id : ""}
                        onChange={(e) => setMGCP(e.target.value)}
                        aria-invalid={mgcpNodes.length > 1}
                      >
                        <option value="">Off</option>
                        {draftNodes
                          .filter(
                            (n) =>
                              n.group.role === "EG" &&
                              supportsMonitoring(n.group.variant, "mgcp"),
                          )
                          .map((n) => (
                            <option key={n.id}>{n.id}</option>
                          ))}
                      </select>
                    </label>
                    {mgcpNodes.length > 1 && (
                      <p role="alert">
                        Select one EG for MGCP. An arena allows at most one
                        provider.
                      </p>
                    )}
                    <label>
                      Seed
                      <input
                        type="number"
                        value={draft.seed}
                        onChange={(e) =>
                          setDraft({ ...draft, seed: +e.target.value })
                        }
                      />
                    </label>
                    {["stress", "mdns-stress"].includes(draft.scenario) && (
                      <label>
                        Workload duration (seconds)
                        <input
                          type="number"
                          min={1}
                          max={86400}
                          value={draft.durationSeconds}
                          onChange={(e) =>
                            setDraft({
                              ...draft,
                              durationSeconds: +e.target.value,
                            })
                          }
                        />
                      </label>
                    )}
                  </div>
                  {["campaign", "explore"].includes(draft.scenario) && (
                    <label>
                      {draft.scenario === "campaign"
                        ? "Campaign lanes"
                        : "Exploration model"}
                      <textarea
                        aria-label={
                          draft.scenario === "campaign"
                            ? "Campaign lanes"
                            : "Exploration model"
                        }
                        key={draft.scenario}
                        aria-invalid={
                          !!jsonErrors[
                            draft.scenario === "campaign"
                              ? "campaign"
                              : "exploration"
                          ]
                        }
                        aria-describedby="native-model-error"
                        defaultValue={JSON.stringify(
                          draft.scenario === "campaign"
                            ? draft.campaign
                            : draft.exploration,
                          null,
                          2,
                        )}
                        onChange={(e) =>
                          updateJSON(
                            draft.scenario === "campaign"
                              ? "campaign"
                              : "exploration",
                            e.target.value,
                          )
                        }
                      />
                      <span
                        id="native-model-error"
                        role={
                          jsonErrors[
                            draft.scenario === "campaign"
                              ? "campaign"
                              : "exploration"
                          ]
                            ? "alert"
                            : undefined
                        }
                      >
                        {
                          jsonErrors[
                            draft.scenario === "campaign"
                              ? "campaign"
                              : "exploration"
                          ]
                        }
                      </span>
                      <span className="muted">
                        Native actions and observations remain authoritative.
                        Action and dependency evidence is saved in the timeline.
                      </span>
                    </label>
                  )}
                  {draft.scenario === "custom" && (
                    <label>
                      Scenario steps (JSON)
                      <textarea
                        aria-label="Scenario steps"
                        placeholder='[{"name":"Pause CS","target":"cs-enbility","action":"lifecycle","command":"pause"}]'
                        key={draft.name + draft.scenario}
                        defaultValue={JSON.stringify(
                          draft.steps ?? [],
                          null,
                          2,
                        )}
                        aria-invalid={!!jsonErrors.steps}
                        onChange={(e) => updateJSON("steps", e.target.value)}
                      />
                      {jsonErrors.steps && (
                        <span role="alert">{jsonErrors.steps}</span>
                      )}
                    </label>
                  )}
                  <h2>
                    Node groups <span className="muted">{total} nodes</span>
                  </h2>
                  <label>
                    Find a revision
                    <input
                      list="stack-revisions"
                      placeholder="Stack, recipe or commit"
                      value={variantFilter}
                      onChange={(e) => setVariantFilter(e.target.value)}
                    />
                    <datalist id="stack-revisions">
                      {variants.map((v) => (
                        <option key={v.id} value={v.id}>
                          {v.label}
                        </option>
                      ))}
                    </datalist>
                  </label>
                  <div className="groups">
                    {draft.groups.map((g, i) => (
                      <div className="group" key={i}>
                        <label>
                          Group ID
                          <input
                            value={g.id}
                            onChange={(e) =>
                              changeGroup(i, { id: e.target.value })
                            }
                          />
                        </label>
                        <label>
                          Implementation / revision
                          <select
                            value={g.variant}
                            onChange={(e) => {
                              const v = state.variants[e.target.value];
                              changeGroup(i, {
                                variant: v.id,
                                external:
                                  v.stack === "enbility"
                                    ? g.external
                                    : undefined,
                                role: v.roles.includes(g.role)
                                  ? g.role
                                  : v.roles[0],
                              });
                            }}
                          >
                            {variants
                              .filter(
                                (v) =>
                                  v.id === g.variant ||
                                  matchingVariants.has(v.id),
                              )
                              .map((v) => (
                                <option value={v.id} key={v.id}>
                                  {v.label}
                                </option>
                              ))}
                          </select>
                        </label>
                        <label>
                          Role
                          <select
                            value={g.role}
                            onChange={(e) =>
                              changeGroup(i, { role: e.target.value })
                            }
                          >
                            {state.variants[g.variant]?.roles.map((role) => (
                              <option key={role}>{role}</option>
                            ))}
                          </select>
                        </label>
                        <label>
                          Count
                          <input
                            type="number"
                            min={1}
                            max={g.external ? 1 : 200}
                            disabled={!!g.external}
                            value={g.count}
                            onChange={(e) =>
                              changeGroup(i, { count: +e.target.value })
                            }
                          />
                        </label>
                        <label>
                          RAM limit (MiB)
                          <input
                            disabled={!!g.external}
                            type="number"
                            placeholder="Default"
                            min={0}
                            max={1048576}
                            value={g.resources?.memoryMiB ?? g.memoryMB ?? ""}
                            onChange={(e) =>
                              changeGroup(i, {
                                memoryMB: undefined,
                                resources: {
                                  ...g.resources,
                                  memoryMiB: e.target.value
                                    ? +e.target.value
                                    : undefined,
                                },
                              })
                            }
                          />
                        </label>
                        <button
                          className="remove"
                          aria-label={"Remove " + g.id}
                          onClick={() => removeGroup(i)}
                        >
                          Remove
                        </button>
                        <details className="resource-editor">
                          <summary>CPU and process limits</summary>
                          <div className="resource-fields">
                            <label>
                              CPU quota (millicores)
                              <input
                                type="number"
                                min={0}
                                max={256000}
                                placeholder="Unlimited"
                                value={g.resources?.cpuMillis ?? ""}
                                disabled={!!g.external}
                                onChange={(e) =>
                                  changeGroup(i, {
                                    resources: {
                                      ...g.resources,
                                      cpuMillis: e.target.value
                                        ? +e.target.value
                                        : undefined,
                                    },
                                  })
                                }
                              />
                            </label>
                            <label>
                              Process limit
                              <input
                                type="number"
                                min={0}
                                max={65536}
                                placeholder="Unlimited"
                                value={g.resources?.pidsLimit ?? ""}
                                disabled={!!g.external}
                                onChange={(e) =>
                                  changeGroup(i, {
                                    resources: {
                                      ...g.resources,
                                      pidsLimit: e.target.value
                                        ? +e.target.value
                                        : undefined,
                                    },
                                  })
                                }
                              />
                            </label>
                          </div>
                          {g.external && (
                            <p className="muted">
                              Container limits do not apply to physical nodes.
                            </p>
                          )}
                        </details>
                        {g.role !== "STRESSER" && (
                          <label>
                            Power monitoring
                            <select
                              value={g.monitoring ?? "none"}
                              onChange={(e) =>
                                changeGroup(i, { monitoring: e.target.value })
                              }
                            >
                              <option value="none">Off</option>
                              <option
                                value="mpc"
                                disabled={!supportsMonitoring(g.variant, "mpc")}
                              >
                                MPC provider
                                {!supportsMonitoring(g.variant, "mpc")
                                  ? " · unavailable in this revision"
                                  : ""}
                              </option>
                              {g.monitoring === "mgcp" && (
                                <option value="mgcp">MGCP · selected EG</option>
                              )}
                            </select>
                          </label>
                        )}
                        {state.variants[g.variant]?.stack === "enbility" && (
                          <details className="resource-editor">
                            <summary>Physical lab stub</summary>
                            <label>
                              <span>
                                <input
                                  type="checkbox"
                                  style={{ width: "auto", minHeight: 0 }}
                                  checked={!!g.external}
                                  onChange={(e) => {
                                    changeGroup(i, {
                                      count: 1,
                                      external: e.target.checked
                                        ? {
                                            label:
                                              "SMGw · temporary native stub",
                                            endpoint:
                                              "https://169.254.1.1:8843",
                                            interface: "han",
                                            address: "192.168.238.8",
                                            tlsFingerprint: "",
                                            token: "",
                                          }
                                        : undefined,
                                      resources: undefined,
                                      memoryMB: undefined,
                                    });
                                    if (e.target.checked)
                                      setDraft((old) => ({
                                        ...old,
                                        timeScale: 1,
                                        network: {
                                          ...old.network,
                                          interface:
                                            old.network?.interface || "eth0",
                                          ...((old.network?.mode ?? "ipv4") !==
                                          "ipv6"
                                            ? {
                                                subnet:
                                                  old.network?.subnet ||
                                                  "192.168.238.0/24",
                                                ipRange:
                                                  old.network?.ipRange ||
                                                  "192.168.238.128/28",
                                              }
                                            : {}),
                                        },
                                      }));
                                  }}
                                />{" "}
                                Run this node on the physical device
                              </span>
                            </label>
                            {g.external && (
                              <div className="resource-fields">
                                {(
                                  [
                                    "label",
                                    "endpoint",
                                    "interface",
                                    "address",
                                    "ipv6Address",
                                    "tlsFingerprint",
                                    "token",
                                  ] as const
                                ).map((key) => (
                                  <label key={key}>
                                    {
                                      {
                                        label: "Device label",
                                        endpoint: "Agent HTTPS endpoint",
                                        interface: "Device interface",
                                        address: "Protocol IPv4 address",
                                        ipv6Address: "Protocol IPv6 address",
                                        tlsFingerprint:
                                          "TLS SHA-256 fingerprint",
                                        token: "Agent token",
                                      }[key]
                                    }
                                    <input
                                      type={
                                        key === "token" ? "password" : "text"
                                      }
                                      autoComplete="off"
                                      placeholder={
                                        editing && key === "token"
                                          ? "Blank keeps the saved token for this endpoint"
                                          : undefined
                                      }
                                      value={g.external![key] ?? ""}
                                      onChange={(e) =>
                                        changeGroup(i, {
                                          external: {
                                            ...g.external!,
                                            [key]: e.target.value,
                                          },
                                        })
                                      }
                                    />
                                  </label>
                                ))}
                              </div>
                            )}
                          </details>
                        )}
                        {g.role === "STRESSER" && (
                          <details className="stress-editor">
                            <summary>Traffic profile</summary>
                            <StressEditor
                              value={g.stress ?? defaultStress}
                              onChange={(stress) => changeGroup(i, { stress })}
                            />
                          </details>
                        )}
                      </div>
                    ))}
                  </div>
                  <div className="group-actions">
                    <button
                      disabled={!variants.some((v) => v.roles.includes("CS"))}
                      onClick={() =>
                        setDraft({
                          ...draft,
                          links:
                            draft.links?.length &&
                            draft.groups.filter((g) => g.role === "EG")
                              .length === 1
                              ? [
                                  ...draft.links,
                                  {
                                    from: draft.groups.find(
                                      (g) => g.role === "EG",
                                    )!.id,
                                    to: nextGroupID(draft.groups),
                                    mode: draft.links[0].mode,
                                  },
                                ]
                              : draft.links,
                          groups: [
                            ...draft.groups,
                            {
                              id: nextGroupID(draft.groups),
                              variant: variants.find((v) =>
                                v.roles.includes("CS"),
                              )!.id,
                              role: "CS",
                              count: 1,
                            },
                          ],
                        })
                      }
                    >
                      Add group
                    </button>
                    <button
                      disabled={
                        !variants.some((v) => v.stack === "mdns-stresser")
                      }
                      onClick={() => {
                        const v = variants.find(
                          (v) => v.stack === "mdns-stresser",
                        );
                        if (v)
                          setDraft({
                            ...draft,
                            groups: [
                              ...draft.groups,
                              {
                                id: nextGroupID(draft.groups, "mdns"),
                                variant: v.id,
                                role: "STRESSER",
                                count: 1,
                                stress: { ...defaultStress },
                              },
                            ],
                          });
                      }}
                    >
                      Add mDNS stresser
                    </button>
                  </div>
                  <label>
                    Peer provisioning
                    <select
                      value={
                        draft.links?.some((l) => l.mode === "ship-pairing")
                          ? "ship-pairing"
                          : "pretrusted"
                      }
                      disabled={
                        draft.groups.filter((g) => g.role === "EG").length !==
                          1 && !draft.links?.length
                      }
                      onChange={(e) => {
                        const eg = draft.groups.find((g) => g.role === "EG");
                        const links =
                          draft.links ??
                          draft.groups
                            .filter((g) => g.role === "CS")
                            .map((g) => ({ from: eg!.id, to: g.id }));
                        setDraft({
                          ...draft,
                          timeScale:
                            e.target.value === "ship-pairing"
                              ? 1
                              : draft.timeScale,
                          links: links.map((l) => ({
                            ...l,
                            mode: e.target.value,
                          })),
                        });
                      }}
                    >
                      <option value="pretrusted">Pre-trusted SKIs</option>
                      <option value="ship-pairing">
                        SHIP pairing · native capability required
                      </option>
                    </select>
                  </label>
                  <details>
                    <summary>Trust relationships</summary>
                    <p>
                      Links select intended peers. Pre-trusted links register
                      SKIs at startup; ship-pairing links provision the listener
                      credentials. Multiple EGs need explicit links. Stresser
                      nodes have no trust links.
                    </p>
                    <textarea
                      aria-label="Explicit group links"
                      placeholder='[{"from":"eg","to":"cs-enbility"}]'
                      key={draft.name + JSON.stringify(draft.links)}
                      defaultValue={
                        draft.links ? JSON.stringify(draft.links) : ""
                      }
                      aria-invalid={!!jsonErrors.links}
                      onChange={(e) => updateJSON("links", e.target.value)}
                    />
                    {jsonErrors.links && (
                      <span role="alert">{jsonErrors.links}</span>
                    )}
                  </details>
                  <details>
                    <summary>Individual limits and sampling</summary>
                    <div className="resource-fields">
                      <label>
                        Sampling interval
                        <select
                          value={draft.sampling?.intervalMillis ?? 1000}
                          onChange={(e) =>
                            setDraft({
                              ...draft,
                              sampling: {
                                ...draft.sampling,
                                intervalMillis: +e.target.value,
                              },
                            })
                          }
                        >
                          <option value="1000">1 second</option>
                          <option value="2000">2 seconds</option>
                          <option value="5000">5 seconds</option>
                        </select>
                      </label>
                      <label>
                        Diagnostic nodes · 250 ms
                        <input
                          placeholder="cs-001, cs-002"
                          value={
                            draft.sampling?.diagnosticNodes?.join(", ") ?? ""
                          }
                          onChange={(e) =>
                            setDraft({
                              ...draft,
                              sampling: {
                                intervalMillis:
                                  draft.sampling?.intervalMillis ?? 1000,
                                diagnosticNodes: e.target.value
                                  .split(",")
                                  .map((s) => s.trim())
                                  .filter(Boolean),
                              },
                            })
                          }
                        />
                      </label>
                    </div>
                    <label>
                      Node overrides
                      <textarea
                        aria-label="Node resource overrides"
                        placeholder='{"cs-001":{"resources":{"cpuMillis":500,"memoryMiB":128}}}'
                        defaultValue={
                          draft.nodeOverrides
                            ? JSON.stringify(draft.nodeOverrides, null, 2)
                            : ""
                        }
                        onChange={(e) =>
                          updateJSON("nodeOverrides", e.target.value)
                        }
                        aria-invalid={!!jsonErrors.nodeOverrides}
                      />
                    </label>
                    {jsonErrors.nodeOverrides && (
                      <span role="alert">{jsonErrors.nodeOverrides}</span>
                    )}
                    <p className="muted">
                      Omitted limits inherit the group. Zero means unlimited.
                      1,000 millicores allows one CPU; RAM limits disable extra
                      swap.
                    </p>
                  </details>
                  {(draft.network?.interface ||
                    draft.groups.some((g) => g.external)) && (
                    <details open>
                      <summary>Lab network</summary>
                      <div className="resource-fields">
                        {(
                          [
                            "interface",
                            ...((draft.network?.mode ?? "ipv4") !== "ipv6"
                              ? ["subnet", "ipRange"]
                              : []),
                            ...((draft.network?.mode ?? "ipv4") !== "ipv4"
                              ? ["ipv6Subnet", "ipv6Range"]
                              : []),
                          ] as (keyof NonNullable<Definition["network"]>)[]
                        ).map((key) => (
                          <label key={key}>
                            {
                              {
                                interface: "Host interface",
                                subnet: "IPv4 subnet",
                                ipRange: "IPv4 container pool",
                                ipv6Subnet: "IPv6 ULA subnet",
                                ipv6Range: "IPv6 container pool",
                                mode: "IP mode",
                              }[key]
                            }
                            <input
                              value={draft.network?.[key] ?? ""}
                              onChange={(e) =>
                                setDraft({
                                  ...draft,
                                  network: {
                                    ...draft.network!,
                                    [key]: e.target.value,
                                  },
                                })
                              }
                            />
                          </label>
                        ))}
                      </div>
                      <p className="muted">
                        A dedicated macvlan network connects containers to the
                        lab. Reserve its address pool against other devices.
                      </p>
                    </details>
                  )}
                  {!editing && (
                    <p className="muted">
                      Exports include test private keys so SKIs survive import.
                    </p>
                  )}
                  <button
                    className="primary"
                    disabled={
                      busy ||
                      !!editIssue ||
                      total < 2 ||
                      total > 201 ||
                      mgcpNodes.length > 1 ||
                      !!jsonErrors.links ||
                      Object.values(jsonErrors).some(Boolean)
                    }
                    onClick={() =>
                      act(async () => {
                        const result = editing
                          ? await api(
                              `/runs/${editing.id}`,
                              {
                                definition: draft,
                                revision: editing.revision,
                              },
                              "PUT",
                            )
                          : await api("/runs", {
                              schemaVersion: 3,
                              definition: draft,
                            });
                        setRunId(result.id);
                        setEditing(null);
                        setCreating(false);
                      })
                    }
                  >
                    {busy
                      ? editing
                        ? "Saving…"
                        : "Preparing…"
                      : editing
                        ? "Save changes"
                        : "Create arena"}
                  </button>
                </>
              )}
            </section>
          ) : run ? (
            <>
              <div className="run-heading">
                <div>
                  <h1>{run.definition.name}</h1>
                  <p>
                    {run.nodes.length} nodes · {run.status} ·{" "}
                    {run.definition.timeScale === 1
                      ? "Real time"
                      : `${run.definition.timeScale}× · ${run.definition.timingProfile === "legacy-partial" ? "partial timing" : "qualification required"}`}{" "}
                    · seed {run.definition.seed}
                  </p>
                </div>
                <div className="actions">
                  <button
                    disabled={busy || !canEdit(run)}
                    title={
                      canEdit(run)
                        ? "Change this arena’s setup"
                        : "Stop the arena and finish cleanup before editing"
                    }
                    onClick={beginEdit}
                  >
                    Edit arena
                  </button>
                  <button
                    className="primary"
                    disabled={
                      busy ||
                      !["created", "stopped", "failed"].includes(run.status)
                    }
                    onClick={() => act(() => api(`/runs/${run.id}/start`, {}))}
                  >
                    Start arena
                  </button>
                  <button
                    disabled={
                      busy ||
                      !["running", "complete", "interrupted"].includes(
                        run.status,
                      )
                    }
                    onClick={() => act(() => api(`/runs/${run.id}/test`, {}))}
                  >
                    Run scenario
                  </button>
                  <button
                    disabled={
                      busy ||
                      [
                        "created",
                        "stopped",
                        "corrupt",
                        "dropping",
                        "stopping",
                        "archived",
                      ].includes(run.status)
                    }
                    onClick={() => act(() => api(`/runs/${run.id}/stop`, {}))}
                  >
                    Stop arena
                  </button>
                  {run.status !== "archived" && (
                    <a
                      className="button"
                      href={`/api/v1/runs/${run.id}/export`}
                    >
                      Export setup
                    </a>
                  )}
                  <a
                    className="button"
                    href={`/api/v1/runs/${run.id}/evidence`}
                  >
                    Evidence
                  </a>
                  <button
                    className="danger"
                    disabled={busy || !canDrop(run)}
                    title={
                      canDrop(run)
                        ? "Remove this saved arena; identities and evidence are preserved"
                        : "Stop this arena before dropping it"
                    }
                    onClick={() => dropRuns([run.id])}
                  >
                    Drop arena
                  </button>
                </div>
              </div>
              {run.error && (
                <p className="error" role="alert">
                  {run.error}
                </p>
              )}
              <div className="tabs" role="group" aria-label="Arena views">
                {["nodes", "resources", "faults", "timeline", "results"].map(
                  (t) => (
                    <button
                      aria-pressed={tab === t}
                      key={t}
                      onClick={() => setTab(t)}
                    >
                      {t === "results"
                        ? `Results · ${passed} passed / ${failed} failed`
                        : t[0].toUpperCase() + t.slice(1)}
                    </button>
                  ),
                )}
              </div>
              {tab === "nodes" ? (
                <div
                  className={"arena-layout " + (node ? "has-inspector" : "")}
                >
                  <section>
                    <div className="toolbar">
                      <div className="switch">
                        <button
                          aria-pressed={view === "map"}
                          onClick={() => setView("map")}
                        >
                          Map
                        </button>
                        <button
                          aria-pressed={view === "table"}
                          onClick={() => setView("table")}
                        >
                          Table
                        </button>
                      </div>
                      <label className="search">
                        <span className="sr-only">Filter nodes</span>
                        <input
                          value={filter}
                          onChange={(e) => setFilter(e.target.value)}
                          placeholder="Filter nodes…"
                          title="Filter by name, stack or state"
                        />
                      </label>
                      <label>
                        <span className="sr-only">Sort nodes</span>
                        <select
                          value={sort}
                          onChange={(e) => setSort(e.target.value)}
                        >
                          <option value="id">Sort: Node ID</option>
                          <option value="cpu">Sort: CPU</option>
                          <option value="memory">Sort: Memory</option>
                        </select>
                      </label>
                    </div>
                    {filtered.length === 0 && (
                      <p role="status">No nodes match this filter.</p>
                    )}
                    {view === "map" ? (
                      <NodeMap
                        nodes={run.nodes}
                        visible={filtered.map((n) => n.id)}
                        selected={nodeId}
                        select={setNodeId}
                      />
                    ) : (
                      <div className="table-scroll">
                        <table>
                          <thead>
                            <tr>
                              <th>Node</th>
                              <th>Stack / role</th>
                              <th>State</th>
                              <th>CPU</th>
                              <th>Memory</th>
                            </tr>
                          </thead>
                          <tbody>
                            {filtered.map((n) => (
                              <tr
                                key={n.id}
                                className={nodeId === n.id ? "selected" : ""}
                              >
                                <td>
                                  <button onClick={() => setNodeId(n.id)}>
                                    {n.id}
                                  </button>
                                </td>
                                <td>
                                  <span className="stack-name">
                                    <StackCue stack={n.stack} />
                                    {stackLabels[n.stack] ?? n.stack} · {n.role}
                                  </span>
                                </td>
                                <td>{n.lifecycle}</td>
                                <td>{sampled(n, "cpuPercent", cpu)}</td>
                                <td>{sampled(n, "memoryBytes", bytes)}</td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>
                    )}
                    <div
                      className="legend stack-legend"
                      aria-label="Implementation stacks"
                    >
                      <strong>Stack</strong>
                      {Object.entries(stackLabels).map(([stack, label]) => (
                        <span key={stack}>
                          <StackCue stack={stack} />
                          {label}
                        </span>
                      ))}
                    </div>
                    <div className="legend" aria-label="Node lifecycle">
                      <strong>Border + LED</strong>
                      <span>
                        <i className="running" />
                        Running
                      </span>
                      <span>
                        <i className="paused" />
                        Paused
                      </span>
                      <span>
                        <i className="stopped" />
                        Stopped / exited
                      </span>
                      <span>
                        <i />
                        Created / unknown
                      </span>
                    </div>
                  </section>
                  {node && (
                    <aside className="inspector">
                      <div className="inspector-title">
                        <h2>{node.id}</h2>
                        <button
                          onClick={() => setNodeId("")}
                          aria-label="Close node inspector"
                        >
                          Close
                        </button>
                      </div>
                      <p>
                        <span className="stack-name">
                          <StackCue stack={node.stack} />
                          {stackLabels[node.stack] ?? node.stack} · {node.role}{" "}
                          · {node.lifecycle}
                        </span>
                      </p>
                      <dl>
                        <dt>CPU</dt>
                        <dd>{sampled(node, "cpuPercent", cpu)}</dd>
                        <dt>Memory</dt>
                        <dd>{sampled(node, "memoryBytes", bytes)}</dd>
                        <dt>Network RX / TX</dt>
                        <dd>
                          {sampled(node, "rxBytes", bytes)} /{" "}
                          {sampled(node, "txBytes", bytes)}
                        </dd>
                        <dt>Block IO read / write</dt>
                        <dd>
                          {sampled(node, "blockReadBytes", bytes)} /{" "}
                          {sampled(node, "blockWriteBytes", bytes)}
                        </dd>
                        <dt>Revision</dt>
                        <dd>
                          {state.variants[node.variant]?.label ?? node.variant}
                        </dd>
                        <dt>SKI</dt>
                        <dd className="ski">{node.ski}</dd>
                        <dt>Last observation</dt>
                        <dd>
                          {node.lastSeen && !node.lastSeen.startsWith("0001")
                            ? new Date(node.lastSeen).toLocaleTimeString()
                            : "Awaiting adapter"}
                        </dd>
                      </dl>
                      <div className="node-actions lifecycle-actions">
                        {[
                          "start",
                          "stop",
                          "kill",
                          "restart",
                          "pause",
                          "resume",
                        ].map((action) => (
                          <button
                            key={action}
                            disabled={busy || !lifecycleAllowed(node, action)}
                            onClick={() =>
                              act(() =>
                                api(
                                  `/runs/${run.id}/nodes/${node.id}/actions`,
                                  { action },
                                ),
                              )
                            }
                          >
                            {action}
                          </button>
                        ))}
                      </div>
                      {!!node.pairingPeers?.length && (
                        <PairingPanel
                          node={node}
                          run={run}
                          busy={busy}
                          command={command}
                          reset={() =>
                            act(() =>
                              api(
                                `/runs/${run.id}/nodes/${node.id}/pairing/reset`,
                                {},
                              ),
                            )
                          }
                        />
                      )}
                      {node.role === "STRESSER" && (
                        <StressPanel
                          key={node.id}
                          node={node}
                          busy={busy}
                          command={command}
                          initial={
                            run.definition.groups.find(
                              (g) => g.id === node.group,
                            )?.stress ?? defaultStress
                          }
                        />
                      )}
                      {(node.monitoringObservation ||
                        node.snapshot.monitoring ||
                        node.capabilities.includes("observe.measurement")) && (
                        <MonitoringPanel
                          key={node.id}
                          node={node}
                          peers={run.nodes}
                          disabled={busy || node.lifecycle !== "running"}
                          command={command}
                        />
                      )}
                      <h3>Protocol controls</h3>
                      <div className="node-actions">
                        <button
                          disabled={
                            busy ||
                            node.lifecycle !== "running" ||
                            !node.capabilities.includes("heartbeat.set")
                          }
                          onClick={() =>
                            command("heartbeat.set", { enabled: false })
                          }
                        >
                          Suppress heartbeat
                        </button>
                        <button
                          disabled={
                            busy ||
                            node.lifecycle !== "running" ||
                            !node.capabilities.includes("heartbeat.set")
                          }
                          onClick={() =>
                            command("heartbeat.set", { enabled: true })
                          }
                        >
                          Resume heartbeat
                        </button>
                      </div>
                      <label>
                        Heartbeat interval (seconds)
                        <input
                          type="number"
                          min={1}
                          max={3600}
                          value={interval}
                          onChange={(e) =>
                            setHeartbeatInterval(+e.target.value)
                          }
                        />
                      </label>
                      <div className="node-actions">
                        <button
                          disabled={
                            busy ||
                            node.lifecycle !== "running" ||
                            !node.capabilities.includes("heartbeat.interval")
                          }
                          onClick={() =>
                            command("heartbeat.interval", { seconds: interval })
                          }
                        >
                          Set interval
                        </button>
                        <button
                          disabled={
                            busy ||
                            node.lifecycle !== "running" ||
                            !node.capabilities.includes("mdns.query")
                          }
                          onClick={() => command("mdns.query", {})}
                        >
                          Refresh discovery
                        </button>
                      </div>
                      {node.role === "EG" && (
                        <>
                          <label>
                            Partner
                            <select
                              value={target}
                              onChange={(e) => setTarget(e.target.value)}
                            >
                              <option value="">Select CS</option>
                              {run.nodes
                                .filter(
                                  (n) =>
                                    node.trustedSkis.includes(n.ski) ||
                                    Object.values(
                                      node.snapshot.pairing?.trusted ?? {},
                                    ).some(
                                      (i: any) =>
                                        i.fingerprint?.toUpperCase() ===
                                        n.fingerprint,
                                    ),
                                )
                                .map((n) => (
                                  <option key={n.id} value={n.ski}>
                                    {n.id}
                                  </option>
                                ))}
                            </select>
                          </label>
                          <div className="fields compact">
                            <label>
                              Use case
                              <select
                                value={usecase}
                                onChange={(e) => setUsecase(e.target.value)}
                              >
                                <option value="lpc">LPC</option>
                                <option value="lpp">LPP</option>
                              </select>
                            </label>
                            <label>
                              Limit (W)
                              <input
                                type="number"
                                value={value}
                                onChange={(e) => setValue(+e.target.value)}
                              />
                            </label>
                          </div>
                          <div className="node-actions">
                            <button
                              disabled={
                                busy ||
                                !target ||
                                node.lifecycle !== "running" ||
                                !node.capabilities.includes("limit.set")
                              }
                              onClick={() =>
                                command("limit.set", {
                                  targetSki: target,
                                  usecase,
                                  value:
                                    usecase === "lpp"
                                      ? -Math.abs(value)
                                      : value,
                                  durationSeconds: 600,
                                  active: true,
                                })
                              }
                            >
                              Activate limit
                            </button>
                            <button
                              disabled={
                                busy ||
                                !target ||
                                node.lifecycle !== "running" ||
                                !node.capabilities.includes("limit.set")
                              }
                              onClick={() =>
                                command("limit.set", {
                                  targetSki: target,
                                  usecase,
                                  value:
                                    usecase === "lpp"
                                      ? -Math.abs(value)
                                      : value,
                                  durationSeconds: 600,
                                  active: false,
                                })
                              }
                            >
                              Deactivate
                            </button>
                          </div>
                        </>
                      )}
                      {commandResult &&
                        resultNode === run.id + "/" + node.id && (
                          <pre>{commandResult}</pre>
                        )}
                      <details open>
                        <summary>Observed state</summary>
                        <pre>{JSON.stringify(node.snapshot, null, 2)}</pre>
                      </details>
                      <details>
                        <summary>Protocol counters</summary>
                        <pre>
                          {JSON.stringify(node.counters ?? {}, null, 2)}
                        </pre>
                      </details>
                      <details>
                        <summary>Build provenance</summary>
                        <pre>
                          {JSON.stringify(
                            state.variants[node.variant]?.provenance,
                            null,
                            2,
                          )}
                        </pre>
                      </details>
                      <details>
                        <summary>Adapter capabilities</summary>
                        <ul>
                          {node.capabilities.map((c) => (
                            <li key={c}>{c}</li>
                          ))}
                        </ul>
                      </details>
                      <h3>Live output</h3>
                      <pre className="logs">
                        {events
                          .filter(
                            (e) =>
                              e.runId === run.id &&
                              e.nodeId === node.id &&
                              e.type === "log",
                          )
                          .slice(-100)
                          .map(
                            (e) =>
                              new Date(e.at).toLocaleTimeString() +
                              " " +
                              e.data?.line,
                          )
                          .join("\n") || "Awaiting output…"}
                      </pre>
                    </aside>
                  )}
                </div>
              ) : tab === "resources" ? (
                <ResourcePanel
                  key={run.id}
                  run={run}
                  onFocus={(focus) => {
                    setEvidenceFocus(focus);
                    setTab("timeline");
                  }}
                />
              ) : tab === "faults" ? (
                <FaultPanel key={run.id} run={run} />
              ) : tab === "timeline" ? (
                <TimelinePanel
                  key={run.id}
                  run={run}
                  focus={evidenceFocus}
                  onClear={() => setEvidenceFocus(undefined)}
                />
              ) : (
                <section>
                  <h2>Assertion results</h2>
                  {run.assertions.length === 0 ? (
                    <p className="empty">
                      Run the selected scenario to collect assertions.
                    </p>
                  ) : (
                    <div className="table-scroll">
                      <table>
                        <thead>
                          <tr>
                            <th>Outcome</th>
                            <th>Node / assertion</th>
                            <th>Evidence</th>
                          </tr>
                        </thead>
                        <tbody>
                          {run.assertions.map((a, i) => (
                            <tr key={i}>
                              <td>
                                <span className={"verdict " + a.outcome}>
                                  {a.outcome}
                                </span>
                              </td>
                              <td>
                                {a.node}
                                <br />
                                {a.id}
                              </td>
                              <td>
                                {a.detail}
                                <small>{a.reference}</small>
                                <button
                                  onClick={() => {
                                    setEvidenceFocus({
                                      at: a.at,
                                      node: a.node || undefined,
                                    });
                                    setTab("timeline");
                                  }}
                                >
                                  Inspect interval
                                </button>
                              </td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  )}
                </section>
              )}
            </>
          ) : (
            <div className="empty">
              <h1>Choose an arena</h1>
              <p>Select an experiment or create a new one.</p>
            </div>
          )}
        </main>
      </div>
    </>
  );
}
function NodeMap({
  nodes,
  visible,
  selected,
  select,
}: {
  nodes: Node[];
  visible: string[];
  selected: string;
  select: (id: string) => void;
}) {
  const guards = nodes.filter((n) => n.role === "EG"),
    clients = nodes.filter((n) => n.role === "CS");
  const dense = clients.length > 60;
  const clientRadius = clients.length > 50 && !dense ? 10 : 12;
  const positions = new Map<string, { x: number; y: number }>();
  guards.forEach((n, i) =>
    positions.set(n.id, {
      x: 400 + (i - (guards.length - 1) / 2) * 85,
      y: dense ? 415 : 230,
    }),
  );
  clients.forEach((n, i) => {
    if (dense)
      positions.set(n.id, {
        x: 35 + (i % 20) * 38,
        y: 24 + Math.floor(i / 20) * 36,
      });
    else {
      const angle = (i / clients.length) * Math.PI * 2 - Math.PI / 2;
      positions.set(n.id, {
        x: 400 + Math.cos(angle) * Math.min(280, 150 + clients.length * 3),
        y: 230 + Math.sin(angle) * Math.min(200, 130 + clients.length * 2),
      });
    }
  });
  nodes
    .filter((n) => n.role === "STRESSER")
    .forEach((n, i) =>
      positions.set(n.id, {
        x: 60 + (i % 10) * 70,
        y: 410 - Math.floor(i / 10) * 40,
      }),
    );
  return (
    <svg
      className="node-map"
      viewBox="0 0 800 460"
      role="group"
      aria-label="Arena node map"
    >
      <desc>
        Blue identifies enbility, violet OpenEEBUS, orange jEEBUS, and teal the
        mDNS stresser. The border and LED show lifecycle. Text identifies EG,
        CS, or DNS. Dotted links show intended peers; solid links show connected
        peers.
      </desc>
      {clients
        .filter((n) => visible.includes(n.id))
        .map((n) => {
          const p = positions.get(n.id)!;
          return guards
            .filter(
              (g) =>
                visible.includes(g.id) &&
                (n.trustedSkis.includes(g.ski) ||
                  !!n.intendedPeers?.includes(g.id)),
            )
            .map((g) => {
              const q = positions.get(g.id)!;
              return (
                <line
                  key={n.id + g.id}
                  x1={p.x}
                  y1={p.y}
                  x2={q.x}
                  y2={q.y}
                  className={
                    "trust-link" +
                    (n.snapshot.peers?.[g.ski] === "connected"
                      ? " connected"
                      : " intended")
                  }
                />
              );
            });
        })}
      {nodes
        .filter((n) => visible.includes(n.id))
        .map((n) => {
          const p = positions.get(n.id)!;
          const radius = n.role === "EG" ? 24 : clientRadius;
          const ledOffset = radius / Math.sqrt(2);
          return (
            <g
              key={n.id}
              transform={`translate(${p.x} ${p.y})`}
              role="button"
              tabIndex={0}
              aria-label={`${n.id}, ${n.stack}, ${n.role}, ${n.lifecycle}`}
              onClick={() => select(n.id)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  select(n.id);
                }
              }}
              className={
                "map-node " +
                `stack-${n.stack} ` +
                n.lifecycle +
                (n.id === selected ? " selected" : "")
              }
            >
              <circle
                className="node-selection-ring"
                r={radius + 6}
                aria-hidden="true"
              />
              <circle className="node-shape" r={radius} />
              <text y={n.role === "EG" ? 5 : 4} className="node-role">
                {n.role === "STRESSER" ? "DNS" : n.role}
              </text>
              <circle
                className="node-led"
                cx={ledOffset}
                cy={ledOffset}
                r={n.role === "EG" ? 5 : 4}
                aria-hidden="true"
              />
              {(n.role === "EG" || clients.length <= 20) && (
                <text y={n.role === "EG" ? 38 : 28} className="node-label">
                  {n.id}
                </text>
              )}
              <title>
                {n.id} · {n.stack} · {n.lifecycle}
              </title>
            </g>
          );
        })}
    </svg>
  );
}
function StressEditor({
  value,
  onChange,
}: {
  value: StressProfile;
  onChange: (value: StressProfile) => void;
}) {
  return (
    <div className="fields compact">
      <label>
        IP traffic
        <select
          value={value.family ?? ""}
          onChange={(e) => onChange({ ...value, family: e.target.value })}
        >
          <option value="">Arena IP mode</option>
          <option value="ipv4">IPv4</option>
          <option value="ipv6">IPv6</option>
          <option value="dual-stack">Both · shared packet rate</option>
        </select>
      </label>
      <label>
        Pattern
        <select
          value={value.pattern}
          onChange={(e) => onChange({ ...value, pattern: e.target.value })}
        >
          {[
            "advertise",
            "query",
            "churn",
            "goodbye",
            "stale",
            "duplicate",
            "invalid",
            "replay",
          ].map((p) => (
            <option key={p}>{p}</option>
          ))}
        </select>
      </label>
      <label>
        Service
        <select
          value={value.service}
          onChange={(e) => onChange({ ...value, service: e.target.value })}
        >
          {["_ship._tcp", "_shippairing._tcp", "_http._tcp"].map((p) => (
            <option key={p}>{p}</option>
          ))}
        </select>
      </label>
      {(
        [
          ["services", "Service instances", 1, 10000],
          ["rate", "Packets / second", 1, 10000],
          ["burst", "Maximum burst", 1, 1000],
          ["churn", "Churn (%)", 0, 100],
          ["ttl", "TTL (seconds)", 0, 86400],
          ["durationSeconds", "Duration (seconds)", 1, 86400],
          ["seed", "Seed", 0, 2147483647],
        ] as const
      ).map(([key, label, min, max]) => (
        <label key={key}>
          {label}
          <input
            type="number"
            min={min}
            max={max}
            value={value[key]}
            onChange={(e) => onChange({ ...value, [key]: +e.target.value })}
          />
        </label>
      ))}
      {value.service === "_shippairing._tcp" && (
        <>
          <label>
            Target SHIP ID
            <input
              value={value.forId ?? ""}
              maxLength={128}
              onChange={(e) => onChange({ ...value, forId: e.target.value })}
            />
          </label>
          <label>
            Target fingerprint
            <input
              value={value.forPar ?? ""}
              maxLength={64}
              onChange={(e) => onChange({ ...value, forPar: e.target.value })}
            />
          </label>
        </>
      )}
      {value.pattern === "replay" && (
        <label>
          Captured packet ID
          <input
            value={value.replayId ?? ""}
            onChange={(e) => onChange({ ...value, replayId: e.target.value })}
          />
        </label>
      )}
    </div>
  );
}
function StressPanel({
  node,
  busy,
  command,
  initial,
}: {
  node: Node;
  busy: boolean;
  command: (name: string, params: Record<string, unknown>) => void;
  initial: StressProfile;
}) {
  const [profile, setProfile] = useState(initial);
  const traffic = node.snapshot.mdns;
  return (
    <section>
      <h3>mDNS traffic</h3>
      <p>
        {traffic?.active ? "Sending" : "Idle"} · socket writes{" "}
        {traffic?.sent ?? "—"} · errors {traffic?.sendErrors ?? "—"} · skipped{" "}
        {traffic?.skipped ?? "—"}
      </p>
      <p className="muted">
        Writes do not prove delivery. Up to 32 pairing-packet captures.
      </p>
      {traffic?.families && (
        <div
          className="table-scroll"
          tabIndex={0}
          aria-label="mDNS traffic by IP family"
        >
          <table>
            <thead>
              <tr>
                <th>Family</th>
                <th>Sent</th>
                <th>Errors</th>
                <th>Received</th>
                <th>Packets / s</th>
              </tr>
            </thead>
            <tbody>
              {Object.entries(traffic.families).map(([family, raw]) => {
                const counters = raw as Record<string, number>;
                return (
                  <tr key={family}>
                    <td>{family === "ipv6" ? "IPv6" : "IPv4"}</td>
                    <td>{counters.sent}</td>
                    <td>{counters.sendErrors}</td>
                    <td>{counters.received}</td>
                    <td>{counters.achievedRate.toFixed(1)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <StressEditor value={profile} onChange={setProfile} />
      <div className="node-actions">
        <button
          disabled={
            busy ||
            node.lifecycle !== "running" ||
            traffic?.active ||
            !node.capabilities.includes("mdns-stress.start")
          }
          onClick={() => command("mdns-stress.start", { profile })}
        >
          Start traffic
        </button>
        <button
          disabled={busy || !traffic?.active}
          onClick={() => command("mdns-stress.stop", {})}
        >
          Stop traffic
        </button>
      </div>
      {traffic?.captureIds?.length > 0 && (
        <label>
          Replay captured advertisement
          <select
            value={profile.replayId ?? ""}
            onChange={(e) =>
              setProfile({
                ...profile,
                pattern: "replay",
                replayId: e.target.value,
              })
            }
          >
            <option value="">Choose packet</option>
            {traffic.captureIds.map((id: string) => (
              <option key={id}>{id}</option>
            ))}
          </select>
        </label>
      )}
    </section>
  );
}
function PairingPanel({
  node,
  run,
  busy,
  command,
  reset,
}: {
  node: Node;
  run: Run;
  busy: boolean;
  command: (name: string, params: Record<string, unknown>) => void;
  reset: () => void;
}) {
  const [target, setTarget] = useState("");
  const peers = run.nodes.filter((n) => node.pairingPeers?.includes(n.id));
  const selected = peers.some((n) => n.id === target)
    ? target
    : (peers[0]?.id ?? "");
  const pairing = node.snapshot.pairing;
  return (
    <section>
      <h3>SHIP pairing</h3>
      <p>
        {pairing
          ? `${pairing.role} · listener ${pairing.listener?.active ? "active" : "inactive"}`
          : node.lifecycle === "running"
            ? "Awaiting native pairing observations"
            : "Pairing state unavailable while stopped"}
      </p>
      <dl>
        <dt>SHIP ID</dt>
        <dd className="ski">{node.shipId}</dd>
        <dt>Intended peers</dt>
        <dd>{peers.map((n) => n.id).join(", ")}</dd>
        <dt>Learned trust</dt>
        <dd>
          {Object.values(pairing?.trusted ?? {})
            .map((i: any) => i.shipID || i.ski)
            .join(", ") || "None observed"}
        </dd>
        <dt>Announcements</dt>
        <dd>{pairing?.announcements?.join(", ") || "None"}</dd>
      </dl>
      {Object.keys(node.pairingProgress ?? {}).length > 0 && (
        <div className="table-scroll">
          <table title="Time from announcement to native observation; a dash means not observed">
            <caption>Since announcement · seconds</caption>
            <thead>
              <tr>
                <th>Peer</th>
                <th>Trust</th>
                <th>SHIP</th>
                <th>LPC</th>
                <th>LPP</th>
              </tr>
            </thead>
            <tbody>
              {Object.entries(node.pairingProgress ?? {}).map(
                ([peer, progress]) => (
                  <tr key={peer}>
                    <td>{peer}</td>
                    {["trust", "ship", "lpc", "lpp"].map((stage) => (
                      <td key={stage}>
                        {progress.stages[stage] === undefined
                          ? "—"
                          : progress.stages[stage].toFixed(2)}
                      </td>
                    ))}
                  </tr>
                ),
              )}
            </tbody>
          </table>
        </div>
      )}
      {pairing?.persistenceError && (
        <p role="alert" className="error">
          {pairing.persistenceError}
        </p>
      )}
      {node.role === "EG" && (
        <>
          <label>
            Pairing listener
            <select
              value={selected}
              onChange={(e) => setTarget(e.target.value)}
            >
              {peers.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.id}
                </option>
              ))}
            </select>
          </label>
          <div className="node-actions">
            <button
              disabled={
                busy ||
                node.lifecycle !== "running" ||
                !node.capabilities.includes("pairing.announce")
              }
              onClick={() =>
                command("pairing.announce", { targetNode: selected })
              }
            >
              Announce pairing
            </button>
            <button
              disabled={
                busy ||
                node.lifecycle !== "running" ||
                !node.capabilities.includes("pairing.stop")
              }
              onClick={() => command("pairing.stop", { targetNode: selected })}
            >
              Withdraw announcement
            </button>
          </div>
        </>
      )}
      <div className="node-actions">
        <button
          title="Requires a stopped node. Clears learned trust and replay history; provisioning credentials stay unchanged"
          aria-describedby="pairing-reset-help"
          disabled={
            busy ||
            run.status === "testing" ||
            !["created", "stopped", "exited", "removed"].includes(
              node.lifecycle,
            )
          }
          onClick={reset}
        >
          Reset pairing
        </button>
      </div>
      <p className="muted" id="pairing-reset-help">
        Requires a stopped node. Clears learned trust and replay history; keeps
        provisioning credentials.
      </p>
    </section>
  );
}

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
