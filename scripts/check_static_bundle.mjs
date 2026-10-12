import { createHash } from "node:crypto";
import { readdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const mode = process.argv[2];
const snapshotPath = process.argv[3];
const root = path.resolve("internal/webui/static");

if (!["snapshot", "compare"].includes(mode) || !snapshotPath) {
  console.error("Usage: node scripts/check_static_bundle.mjs snapshot|compare SNAPSHOT_FILE");
  process.exit(2);
}

async function filesUnder(directory) {
  const files = [];
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const fullPath = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      files.push(...await filesUnder(fullPath));
    } else if (entry.isFile()) {
      files.push(path.relative(root, fullPath).split(path.sep).join("/"));
    }
  }
  return files.sort();
}

const current = {};
for (const file of await filesUnder(root)) {
  current[file] = createHash("sha256").update(await readFile(path.join(root, file))).digest("hex");
}

if (mode === "snapshot") {
  await writeFile(snapshotPath, JSON.stringify(current));
} else {
  const baseline = JSON.parse(await readFile(snapshotPath, "utf8"));
  const changed = [...new Set([...Object.keys(baseline), ...Object.keys(current)])]
    .filter((file) => baseline[file] !== current[file]);
  if (changed.length) {
    console.error(`Generated static bundle differs from the checked-in files:\n${changed.join("\n")}`);
    process.exit(1);
  }
}
