export const stackLabels: Record<string, string> = {
  enbility: "enbility",
  openeebus: "OpenEEBUS",
  jeebus: "jEEBUS",
  "mdns-stresser": "mDNS stresser",
};

export const roleLabels: Record<string, string> = {
  EG: "Energy Guard (EG)",
  CS: "Controllable System (CS)",
  STRESSER: "mDNS stresser",
};

export function nodeOptionLabel(node: {
  id: string;
  stack?: string;
  role?: string;
  external?: { label?: string };
}) {
  return [
    node.external?.label ? `${node.external.label} (${node.id})` : node.id,
    node.stack && (stackLabels[node.stack] ?? node.stack),
    node.role && (roleLabels[node.role] ?? node.role),
  ]
    .filter(Boolean)
    .join(" · ");
}

type VariantLabelData = {
  id: string;
  stack: string;
  label: string;
  builtAt?: string;
};

export function variantOptionLabels(variants: VariantLabelData[]) {
  const latest = new Map<string, number>();
  for (const variant of variants) {
    const time = Date.parse(variant.builtAt ?? "");
    if (Number.isFinite(time))
      latest.set(
        variant.stack,
        Math.max(latest.get(variant.stack) ?? -Infinity, time),
      );
  }
  const labels = new Map<string, string>();
  const duplicates = new Map<string, VariantLabelData[]>();
  for (const variant of variants) {
    let description = variant.label
      .replace(/(?:sha256:)?\b[\da-f]{7,64}\b/gi, "")
      .trim();
    if (description.toLowerCase().startsWith(variant.stack.toLowerCase()))
      description = description.slice(variant.stack.length);
    description = description.replace(/^[\s·/|:-]+|[\s·/|:-]+$/g, "").trim();
    const time = Date.parse(variant.builtAt ?? "");
    const built = Number.isFinite(time)
      ? new Intl.DateTimeFormat(undefined, {
          day: "numeric",
          month: "short",
          year: "numeric",
          hour: "2-digit",
          minute: "2-digit",
        }).format(time)
      : "Build date unavailable";
    const label = [
      stackLabels[variant.stack] ?? variant.stack,
      description,
      time === latest.get(variant.stack) && "Latest build",
      built,
    ]
      .filter(Boolean)
      .join(" · ");
    labels.set(variant.id, label);
    duplicates.set(label, [...(duplicates.get(label) ?? []), variant]);
  }
  for (const [label, group] of duplicates) {
    if (group.length < 2) continue;
    let identities = group.map((v) =>
      v.id.startsWith(`${v.stack}-`) ? v.id.slice(v.stack.length + 1) : v.id,
    );
    if (new Set(identities).size !== identities.length)
      identities = group.map((v) => v.id);
    group.forEach((variant, index) => {
      let length = 8;
      while (
        identities.some(
          (id, other) =>
            other !== index &&
            id.slice(0, length) === identities[index].slice(0, length),
        )
      )
        length += 4;
      labels.set(
        variant.id,
        `${label} · build ${identities[index].slice(0, length)}`,
      );
    });
  }
  return labels;
}

export function capturedPacketLabel(
  index: number,
  capture?: { at: string; data?: Record<string, unknown> },
) {
  const details = [`Captured packet ${index + 1}`];
  if (capture?.data?.family === "ipv4") details.push("IPv4");
  if (capture?.data?.family === "ipv6") details.push("IPv6");
  if (typeof capture?.data?.bytes === "number")
    details.push(`${capture.data.bytes.toLocaleString()} bytes`);
  const time = Date.parse(capture?.at ?? "");
  if (Number.isFinite(time))
    details.push(
      new Intl.DateTimeFormat(undefined, { timeStyle: "medium" }).format(time),
    );
  return details.join(" · ");
}
