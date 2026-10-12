#!/usr/bin/env python3
"""Inspect built arena images without starting their processes. Stdlib only."""

import argparse
import io
import json
import pathlib
import re
import shlex
import struct
import subprocess
import tarfile
import tempfile
import zipfile

STACKS = ("enbility", "openeebus", "jeebus", "mdns-stresser", "orchestrator", "netfault")
REQUIRED = {
    "netfault": {"arena-netfault"},
    "enbility": {"arena-node", "arena-supervisor"},
    "mdns-stresser": {"arena-node", "arena-supervisor"},
    "openeebus": {"arena-node", "arena-supervisor"},
    "jeebus": {"opt/java/bin/java", "opt/java/lib/server/libjvm.so", "arena-supervisor"},
    "orchestrator": {
        "arena",
        "usr/local/bin/docker",
        "usr/local/lib/docker/cli-plugins/docker-compose",
    },
}
JAVA_MODULES = (
    "abstract-controllablesystem",
    "lpc-controllablesystem",
    "lpp-controllablesystem",
    "demo",
)


def elf_sections(data):
    """Return ELF section contents; reject malformed or unsupported files."""
    if data[:4] != b"\x7fELF" or len(data) < 64 or data[4] not in (1, 2) or data[5] not in (1, 2):
        raise ValueError("expected an ELF binary")
    endian = "<" if data[5] == 1 else ">"
    wide = data[4] == 2
    offset = struct.unpack_from(endian + ("Q" if wide else "I"), data, 40 if wide else 32)[0]
    stride, count, names_index = struct.unpack_from(endian + "HHH", data, 58 if wide else 46)
    fmt = endian + ("IIQQQQIIQQ" if wide else "IIIIIIIIII")
    if (
        not count
        or names_index >= count
        or stride < struct.calcsize(fmt)
        or offset + stride * count > len(data)
    ):
        raise ValueError("invalid ELF section table")
    headers = [struct.unpack_from(fmt, data, offset + i * stride) for i in range(count)]

    def contents(header):
        start, size = header[4:6]
        if header[1] == 8:  # SHT_NOBITS occupies memory, not bytes in the file.
            return b""
        if start + size > len(data):
            raise ValueError("ELF section outside file")
        return data[start : start + size]

    names = contents(headers[names_index])
    sections = {}
    for header in headers:
        name_offset = header[0]
        end = names.find(b"\0", name_offset)
        if name_offset >= len(names) or end < 0:
            raise ValueError("invalid ELF section name")
        name = names[name_offset:end].decode("ascii")
        sections[name] = (header[1], contents(header))
    return sections


def verify_elf(data):
    sections = elf_sections(data)
    debug = [n for n in sections if n.startswith((".debug_", ".zdebug_")) or n == ".gnu_debugdata"]
    if debug or any(kind == 2 for kind, _ in sections.values()):
        raise ValueError(
            f"unstripped ELF: debug={debug}, static symbol table={any(k == 2 for k, _ in sections.values())}"
        )
    return {"bytes": len(data), "stripped": True}


def go_build_settings(data, own_build=True):
    blob = elf_sections(data).get(".go.buildinfo", (0, b""))[1]
    if not blob.startswith(b"\xff Go buildinf:") or len(blob) < 32 or not blob[15] & 2:
        raise ValueError("missing supported Go build information")
    cursor = 32

    def string():
        nonlocal cursor
        size, shift = 0, 0
        while True:
            if cursor >= len(blob) or shift >= 64:
                raise ValueError("invalid Go build string")
            value = blob[cursor]
            cursor += 1
            size |= (value & 127) << shift
            if value < 128:
                break
            shift += 7
        end = cursor + size
        if end > len(blob):
            raise ValueError("truncated Go build string")
        value = blob[cursor:end]
        cursor = end
        return value

    version, module = string().decode(), string()
    # Go's module information has a 16-byte framing marker at each end.
    if len(module) < 32:
        raise ValueError("missing Go module information")
    settings = {}
    for line in module[16:-16].decode().splitlines():
        if line.startswith("build\t"):
            key, value = line[6:].split("=", 1)
            settings[key] = value
    flags = settings.get("-gcflags", "")
    if re.search(r"(?:^|[\s=\"])-(?:N|l)(?:$|[\s\"])", flags):
        raise ValueError("Go optimization or inlining disabled")
    if settings.get("-compiler") != "gc":
        raise ValueError("expected standard Go compiler")
    if own_build and (settings.get("CGO_ENABLED") != "0" or settings.get("-trimpath") != "true"):
        raise ValueError("expected a pure-Go build with trimpath")
    if any(settings.get(k) == "true" for k in ("-race", "-asan", "-msan")):
        raise ValueError("instrumented Go build")
    return {"version": version, "settings": settings, "optimizations": "compiler defaults"}


def verify_compile_commands(data):
    commands = json.loads(data)
    if not commands:
        raise ValueError("empty C compilation manifest")
    for entry in commands:
        args = entry.get("arguments") or shlex.split(entry["command"])
        levels = [a for a in args if re.fullmatch(r"-O(?:[0-3sgz]|fast)?", a)]
        if not levels or levels[-1] != "-O2" or "-DNDEBUG" not in args or "-UNDEBUG" in args:
            raise ValueError(f"not an -O2/-DNDEBUG compilation: {entry['file']}")
        if any(a.startswith("-fsanitize") or (a.startswith("-g") and a != "-g0") for a in args):
            raise ValueError(f"debug or sanitizer C compilation: {entry['file']}")
    return {"translationUnits": len(commands), "optimization": "-O2", "ndebug": True}


class ClassReader:
    def __init__(self, data):
        self.data, self.cursor = data, 0

    def take(self, size):
        end = self.cursor + size
        if end > len(self.data):
            raise ValueError("truncated Java class")
        value = self.data[self.cursor : end]
        self.cursor = end
        return value

    def number(self, size):
        return int.from_bytes(self.take(size), "big")


def class_attributes(data):
    reader = ClassReader(data)
    if reader.take(4) != b"\xca\xfe\xba\xbe":
        raise ValueError("expected Java class")
    reader.take(4)
    pool, index, count = {}, 1, reader.number(2)
    sizes = {
        3: 4,
        4: 4,
        5: 8,
        6: 8,
        7: 2,
        8: 2,
        9: 4,
        10: 4,
        11: 4,
        12: 4,
        15: 3,
        16: 2,
        17: 4,
        18: 4,
        19: 2,
        20: 2,
    }
    while index < count:
        tag = reader.number(1)
        if tag == 1:
            pool[index] = reader.take(reader.number(2))
        elif tag in sizes:
            reader.take(sizes[tag])
        else:
            raise ValueError(f"unsupported Java constant tag: {tag}")
        index += 2 if tag in (5, 6) else 1
    found = set()

    def attributes(stream):
        for _ in range(stream.number(2)):
            name = pool[stream.number(2)]
            payload = stream.take(stream.number(4))
            found.add(name)
            if name == b"Code":
                code = ClassReader(payload)
                code.take(4)
                code.take(code.number(4))
                code.take(code.number(2) * 8)
                attributes(code)

    reader.take(6)
    reader.take(reader.number(2) * 2)
    for _ in range(2):  # fields and methods
        for _ in range(reader.number(2)):
            reader.take(6)
            attributes(reader)
    attributes(reader)
    if reader.cursor != len(data):
        raise ValueError("unexpected Java class trailing data")
    return found


def verify_jar(data, sources):
    count, source_files, line_tables, found = 0, 0, 0, set()
    sources = set(sources)
    with zipfile.ZipFile(io.BytesIO(data)) as jar:
        for name in jar.namelist():
            if name.endswith(".class"):
                owner = name.split("$", 1)[0].removesuffix(".class") + ".class"
                if owner not in sources:
                    continue  # bnd bundles also contain untouched Maven dependencies.
                attrs = class_attributes(jar.read(name))
                if attrs & {b"LocalVariableTable", b"LocalVariableTypeTable"}:
                    raise ValueError(f"Java variable debug metadata: {name}")
                count += 1
                source_files += b"SourceFile" in attrs
                line_tables += b"LineNumberTable" in attrs
                found.add(owner)
    if not count or sources - found:
        raise ValueError(f"missing compiled Java sources: {sorted(sources - found)}")
    return {
        "classes": count,
        "variableDebugTables": False,
        "classesWithSourceFile": source_files,
        "classesWithLineNumbers": line_tables,
    }


def verify_image(stack, image=None):
    image = image or f"eebus-arena/{stack}:local"
    details = json.loads(subprocess.check_output(["docker", "image", "inspect", image], text=True))[
        0
    ]
    result = {"stack": stack, "image": details["Id"], "elf": {}, "compilation": {}, "jars": {}}
    if stack == "jeebus":
        env = dict(item.split("=", 1) for item in details["Config"].get("Env", []))
        options = shlex.split(
            " ".join(
                env.get(key, "") for key in ("JAVA_OPTS", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS")
            )
        )
        if any(
            option in ("-Xint", "-XX:-UseCompiler", "-Xdebug")
            or option.startswith("-agentlib:jdwp")
            or re.fullmatch(r"-XX:TieredStopAtLevel=[0-3]", option)
            for option in options
        ):
            raise ValueError("debug or unoptimized Java runtime options")
        result["javaOptions"] = options
    java_sources, jars = {}, {}
    container = subprocess.check_output(
        ["docker", "create", "--network", "none", "--entrypoint", "/unused", details["Id"]],
        text=True,
    ).strip()
    try:
        with tempfile.TemporaryFile() as archive:
            subprocess.run(["docker", "export", container], stdout=archive, check=True)
            archive.seek(0)
            with tarfile.open(fileobj=archive, mode="r|") as files:
                for member in files:
                    name = member.name.removeprefix("./")
                    if not member.isfile():
                        continue
                    native = name in REQUIRED[stack]
                    native |= (
                        stack == "jeebus"
                        and name.startswith("opt/java/")
                        and (name.endswith(".so") or name.startswith("opt/java/bin/"))
                    )
                    native |= stack == "openeebus" and name.startswith(
                        "usr/local/lib/libwebsockets.so"
                    )
                    manifest = (
                        stack == "openeebus"
                        and name.startswith("build-info/")
                        and name.endswith("-compile-commands.json")
                    )
                    source_manifest = stack == "jeebus" and name == "build-info/jeebus-sources.json"
                    jar = (
                        stack == "jeebus"
                        and name.startswith("app/lib/")
                        and any(
                            pathlib.PurePosixPath(name).name.startswith(m + "-")
                            for m in JAVA_MODULES
                        )
                        and name.endswith(".jar")
                    )
                    if not (native or manifest or source_manifest or jar):
                        continue
                    data = files.extractfile(member).read()
                    if native:
                        result["elf"][name] = verify_elf(data)
                        if name == "arena-supervisor":
                            result["collectorGo"] = go_build_settings(data)
                        if name in ("arena-node", "arena") and stack in (
                            "enbility",
                            "mdns-stresser",
                            "orchestrator",
                            "netfault",
                        ):
                            result["go"] = go_build_settings(data)
                        elif stack == "orchestrator":
                            result.setdefault("goTools", {})[name] = go_build_settings(
                                data, own_build=False
                            )
                    elif manifest:
                        result["compilation"][name] = verify_compile_commands(data)
                    elif source_manifest:
                        java_sources = json.loads(data)
                    else:
                        jars[name] = data
        missing = REQUIRED[stack] - result["elf"].keys()
        if missing:
            raise ValueError(f"missing native binaries: {sorted(missing)}")
        if stack == "openeebus" and not any(
            name.startswith("usr/local/lib/libwebsockets.so") for name in result["elf"]
        ):
            raise ValueError("missing native libwebsockets")
        if stack == "openeebus" and set(result["compilation"]) != {
            "build-info/openeebus-compile-commands.json",
            "build-info/libwebsockets-compile-commands.json",
        }:
            raise ValueError("missing C compilation manifests")
        if stack == "jeebus":
            if set(java_sources) != set(JAVA_MODULES) or len(jars) != len(JAVA_MODULES):
                raise ValueError("missing first-party Java modules or source manifest")
            for name, data in jars.items():
                module = next(
                    m for m in JAVA_MODULES if pathlib.PurePosixPath(name).name.startswith(m + "-")
                )
                result["jars"][name] = verify_jar(data, java_sources[module])
        return result
    finally:
        subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stacks", nargs="*", choices=STACKS)
    parser.add_argument("--image", help="Exact image ID/tag; requires one stack")
    parser.add_argument("--output", type=pathlib.Path, help="Save the JSON report")
    args = parser.parse_args()
    stacks = args.stacks or STACKS
    if args.image and len(stacks) != 1:
        parser.error("--image requires exactly one stack")
    reports = [verify_image(stack, args.image) for stack in stacks]
    report = json.dumps(reports, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(report)
    print(report, end="")


if __name__ == "__main__":
    main()
