"""Install jEEBUS source overlays and its verified Gradle dependency locks."""

import hashlib
import io
import json
import shutil
import urllib.request
import zipfile
from pathlib import Path

from scripts.overlays import apply_overlay

SOURCES = {"upstream": "jeebus-cs"}
MODULES = (
    "abstract-controllablesystem",
    "lpc-controllablesystem",
    "lpp-controllablesystem",
    "demo",
)
SPINE_SOURCES = (
    "org/openmuc/jeebus/shipspine/ShipCommunication.java",
    "org/openmuc/jeebus/spine/utils/features/devicediagnosis/HeartbeatDataFunction.java",
)


def spine_archive(cache: Path, spec: dict) -> bytes:
    """Reuse a source JAR only when its contents match the dependency lock."""
    cache.mkdir(parents=True, exist_ok=True)
    archive = cache / (spec["sha256"] + ".jar")
    if archive.exists():
        blob = archive.read_bytes()
        if hashlib.sha256(blob).hexdigest() == spec["sha256"]:
            return blob
    version = spec["version"]
    url = (
        "https://repo.maven.apache.org/maven2/org/openmuc/jeebus/spine/"
        f"{version}/spine-{version}-sources.jar"
    )
    with urllib.request.urlopen(url, timeout=120) as response:
        blob = response.read()
    if hashlib.sha256(blob).hexdigest() != spec["sha256"]:
        raise RuntimeError("jEEBUS SPINE source checksum mismatch")
    archive.write_bytes(blob)
    return blob


def install_spine_sources(java: Path, blob: bytes) -> None:
    # Compile only the two instrumented classes from source. The rest remains
    # in the verified upstream dependency, avoiding a fork of the whole JAR.
    with zipfile.ZipFile(io.BytesIO(blob)) as archive:
        for name in SPINE_SOURCES:
            destination = java / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(archive.read(name))


def install_dependency_locks(upstream: Path, recipes: Path) -> None:
    for module in MODULES:
        shutil.copyfile(
            recipes / f"{module}.lockfile", upstream / "projects" / module / "gradle.lockfile"
        )
    shutil.copyfile(
        recipes / "verification-metadata.xml", upstream / "gradle/verification-metadata.xml"
    )


def write_release_sources(target: Path) -> None:
    """Record owned classes so release verification can inspect their bytecode."""
    sources = {}
    for module in MODULES:
        java = target / "upstream/projects" / module / "src/main/java"
        sources[module] = sorted(
            str(source.relative_to(java).with_suffix(".class")) for source in java.rglob("*.java")
        )
    (target / "release-sources.json").write_text(json.dumps(sources, indent=2) + "\n")


def prepare(target: Path, root: Path, lock: dict) -> None:
    upstream = target / "upstream"
    java = upstream / "projects/demo/src/main/java"
    blob = spine_archive(target.parents[1] / "cache", lock["mavenSources"]["spine"])
    install_spine_sources(java, blob)
    apply_overlay(target, target / "adapter/patches/overlay.patch")

    # RuntimeListener lives in the shared module. Putting the observer in demo
    # would create a dependency from the library back to its application.
    observer = (
        upstream
        / "projects/abstract-controllablesystem/src/main/java"
        / "org/openmuc/jeebus/shipspine/ArenaObserver.java"
    )
    observer.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(target / "adapter/ArenaObserver.java", observer)
    main = java / "org/openmuc/jeebus/usecase/powerlimitation/controllablesystem/demo/Main.java"
    shutil.copyfile(target / "adapter/Main.java", main)
    install_dependency_locks(upstream, root / "recipes/jeebus")
    write_release_sources(target)
