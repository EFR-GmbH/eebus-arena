"""Prepare upstream source trees with the arena overlays. Patch drift fails the run."""

import argparse
import difflib
import hashlib
import json
import os
import pathlib
import shutil
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
STATE = ROOT / ".arena"
LOCK = json.loads((ROOT / "recipes/lock.json").read_text())
if os.getenv("ARENA_LOCK"):
    override = json.loads(pathlib.Path(os.environ["ARENA_LOCK"]).read_text())
    for section in ["sources", "images", "toolchains", "mavenSources"]:
        LOCK[section].update(override.get(section, {}))


def prepare(stack: str, sources: dict[str, str]) -> pathlib.Path:
    """Copy pinned inputs into a disposable tree, preserving upstream symlinks."""
    target = STATE / "generated" / stack
    if target.exists():
        shutil.rmtree(target)
    target.mkdir(parents=True)
    for dest, src in sources.items():
        shutil.copytree(STATE / "sources" / src, target / dest, symlinks=True)
    shutil.copytree(
        ROOT / "adapters" / stack,
        target / "adapter",
        ignore=shutil.ignore_patterns("__pycache__", "*.pyc"),
    )
    runtime = target / "runtime"
    runtime.mkdir()
    for directory in ["cmd/arena-supervisor", "internal/resourceprobe"]:
        for source in sorted((ROOT / directory).glob("*.go")):
            if source.name.endswith("_test.go"):
                continue
            destination = runtime / source.relative_to(ROOT)
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination)
    shutil.copy2(ROOT / "go.mod", runtime / "go.mod")
    licenses = target / "licenses"
    licenses.mkdir()
    for src in sources.values():
        for source in (STATE / "sources" / src).iterdir():
            if source.is_file() and source.name.upper().startswith(
                ("LICENSE", "COPYING", "NOTICE")
            ):
                shutil.copy2(source, licenses / (src + "-" + source.name))
    return target


def provenance(stack: str, target: pathlib.Path, sources: dict[str, str]) -> None:
    """Bind observations to the exact native edits, recipes, and runtime sources."""
    patches = []
    for dest, src in sources.items():
        original = STATE / "sources" / src
        for file in sorted((target / dest).rglob("*")):
            rel = file.relative_to(target / dest)
            source = original / rel
            if file.is_file() and source.is_file() and file.read_bytes() != source.read_bytes():
                diff = "".join(
                    difflib.unified_diff(
                        source.read_text().splitlines(True),
                        file.read_text().splitlines(True),
                        fromfile=f"a/{dest}/{rel}",
                        tofile=f"b/{dest}/{rel}",
                    )
                )
                patches.append(
                    {
                        "path": str(rel),
                        "sha256": hashlib.sha256(diff.encode()).hexdigest(),
                        "diff": diff,
                        "kind": "native-protocol-correction"
                        if stack == "enbility" and str(rel) == "usecases/gcp/mgcp/usecase.go"
                        else "control-observation",
                    }
                )
    hashes = {}
    for path in sorted((ROOT / "adapters" / stack).rglob("*")):
        if path.is_file() and "__pycache__" not in path.parts and path.suffix != ".pyc":
            hashes[str(path.relative_to(ROOT))] = hashlib.sha256(path.read_bytes()).hexdigest()
    for path in sorted((ROOT / "recipes" / stack).rglob("*")):
        if path.is_file():
            hashes[str(path.relative_to(ROOT))] = hashlib.sha256(path.read_bytes()).hexdigest()
    hashes["docker/" + stack + ".Dockerfile"] = hashlib.sha256(
        (ROOT / "docker" / (stack + ".Dockerfile")).read_bytes()
    ).hexdigest()
    ignore = "docker/" + stack + ".Dockerfile.dockerignore"
    hashes[ignore] = hashlib.sha256((ROOT / ignore).read_bytes()).hexdigest()
    for name in ("scripts/prepare.py", "scripts/overlays.py"):
        hashes[name] = hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
    for source in sorted((target / "runtime").rglob("*")):
        if source.is_file():
            hashes[str(source.relative_to(target))] = hashlib.sha256(
                source.read_bytes()
            ).hexdigest()
    result = {
        "schemaVersion": 1,
        "stack": stack,
        "sources": {s: LOCK["sources"][s] for s in sources.values()},
        "patches": patches,
        "adapterFiles": hashes,
        "images": LOCK["images"],
        "toolchains": LOCK["toolchains"],
    }
    if stack == "jeebus":
        result["mavenSources"] = LOCK["mavenSources"]
    result["arenaFeatures"] = {"networkModes": ["ipv4", "ipv6", "dual-stack"]}
    if stack in ("enbility", "openeebus"):
        result["arenaFeatures"].update(
            {"monitoringSelection": 1, "monitoringProviders": ["mgcp", "mpc"]}
        )
    result["recipeHash"] = hashlib.sha256(json.dumps(result, sort_keys=True).encode()).hexdigest()
    (target / "provenance.json").write_text(json.dumps(result, indent=2) + "\n")


def prepare_stack(stack: str) -> None:
    """Run a recipe with explicit inputs, then fingerprint the finished tree."""
    from adapters.enbility import prepare as enbility
    from adapters.jeebus import prepare as jeebus
    from adapters.openeebus import prepare as openeebus

    recipe = {"enbility": enbility, "jeebus": jeebus, "openeebus": openeebus}[stack]
    target = prepare(stack, recipe.SOURCES)
    recipe.prepare(target, ROOT, LOCK)
    provenance(stack, target, recipe.SOURCES)


def mdns_stresser() -> None:
    """Stage the first-party helper without pulling in third-party adapters."""
    target = STATE / "generated/mdns-stresser"
    if target.exists():
        shutil.rmtree(target)
    target.mkdir(parents=True)
    files = [ROOT / "go.mod"]
    for directory in [
        "cmd/mdns-stresser",
        "cmd/arena-supervisor",
        "internal/resourceprobe",
        "internal/labagent",
        "internal/mdnsstress",
        "internal/arena",
    ]:
        files.extend(sorted((ROOT / directory).glob("*.go")))
    hashes = {}
    for file in files:
        if file.name.endswith("_test.go"):
            continue
        rel = file.relative_to(ROOT)
        dest = target / rel
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(file, dest)
        hashes[str(rel)] = hashlib.sha256(file.read_bytes()).hexdigest()
    for name in [
        "docker/mdns-stresser.Dockerfile",
        "docker/mdns-stresser.Dockerfile.dockerignore",
        "scripts/prepare.py",
    ]:
        hashes[name] = hashlib.sha256((ROOT / name).read_bytes()).hexdigest()
    (target / "licenses").mkdir()
    shutil.copy2(ROOT / "NOTICE.md", target / "licenses/NOTICE.md")
    result = {
        "schemaVersion": 1,
        "stack": "mdns-stresser",
        "arenaFeatures": {"networkModes": ["ipv4", "ipv6", "dual-stack"]},
        "sources": {},
        "patches": [],
        "adapterFiles": hashes,
        "images": LOCK["images"],
        "toolchains": LOCK["toolchains"],
    }
    result["recipeHash"] = hashlib.sha256(json.dumps(result, sort_keys=True).encode()).hexdigest()
    (target / "provenance.json").write_text(json.dumps(result, indent=2) + "\n")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--stack",
        choices=("all", "enbility", "openeebus", "jeebus", "mdns-stresser"),
        default="all",
    )
    parser.add_argument(
        "--state",
        type=pathlib.Path,
        help="Alternate state directory containing sources/ and cache/",
    )
    args = parser.parse_args()
    global STATE
    if args.state:
        STATE = args.state.resolve()
    # Support both `python scripts/prepare.py` and `python -m scripts.prepare`.
    sys.path.insert(0, str(ROOT))
    if args.stack in ("all", "mdns-stresser"):
        mdns_stresser()
    stacks = ("enbility", "openeebus", "jeebus") if args.stack == "all" else (args.stack,)
    for stack in stacks:
        if stack != "mdns-stresser":
            prepare_stack(stack)


if __name__ == "__main__":
    main()
