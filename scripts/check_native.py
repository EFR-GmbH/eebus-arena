#!/usr/bin/env python3
"""Compile the prepared C and Java adapters using the development image tools."""

import argparse
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def run(*command: str, cwd: Path | None = None, env: dict | None = None) -> None:
    print("+", " ".join(command), flush=True)
    subprocess.run(command, cwd=cwd, env=env, check=True)


def check_c(source: Path, work: Path) -> None:
    prefix = work / "installed"
    lws = work / "websockets"
    run(
        "cmake",
        "-S",
        str(source / "libwebsockets"),
        "-B",
        str(lws),
        "-DCMAKE_BUILD_TYPE=Release",
        f"-DCMAKE_INSTALL_PREFIX={prefix}",
        "-DLWS_IPV6=ON",
        "-DLWS_WITHOUT_TESTAPPS=ON",
        "-DLWS_WITH_LIBUV=OFF",
        "-DLWS_WITH_GLIB=OFF",
        "-DLWS_WITH_LIBEV=OFF",
    )
    run("cmake", "--build", str(lws), "-j", "4")
    run("cmake", "--install", str(lws))
    build = work / "openeebus"
    run(
        "cmake",
        "-S",
        str(source / "upstream"),
        "-B",
        str(build),
        "-DCMAKE_BUILD_TYPE=Release",
        "-DCMAKE_C_FLAGS_RELEASE=-O2 -DNDEBUG",
        "-DOPTION_ASAN=OFF",
        f"-DCMAKE_PREFIX_PATH={prefix}",
    )
    run("cmake", "--build", str(build), "--target", "arena-node", "-j", "4")
    # Link only the exercised cleanup code; this regression test must never
    # start a native device or signal an actual child process.
    test = work / "c-cleanup-test"
    run(
        "cc",
        "-std=c11",
        "-ffunction-sections",
        "-fdata-sections",
        "-Wl,--gc-sections",
        "-I",
        str(source / "upstream"),
        str(ROOT / "adapters/openeebus/tests/main_test.c"),
        "-o",
        str(test),
        "-pthread",
    )
    run(str(test))


def check_java(source: Path, work: Path) -> None:
    upstream = work / "jeebus"
    # Gradle writes .gradle and build directories next to its inputs. Keep the
    # prepared tree read-only so checking it cannot change release provenance.
    shutil.copytree(source / "upstream", upstream, symlinks=True)
    environment = dict(os.environ)
    environment["GRADLE_USER_HOME"] = (
        "/cache/gradle" if Path("/cache").is_dir() else str(work / "gradle")
    )
    run(
        "/opt/gradle/gradle-8.5/bin/gradle",
        "--no-daemon",
        "--max-workers=2",
        ":demo:installDist",
        "--dependency-verification",
        "strict",
        cwd=upstream,
        env=environment,
    )
    tests = sorted((ROOT / "adapters/jeebus/tests").glob("*.java"))
    if tests:
        classes = work / "java-tests"
        # The demo contains the instrumented SPINE classes. They must precede
        # the original dependency JAR, just as in Gradle's application launcher.
        classpath = os.pathsep.join(
            [
                str(upstream / "projects/demo/build/classes/java/main"),
                str(upstream / "projects/demo/build/install/demo/lib/*"),
            ]
        )
        owned_sources = sorted((ROOT / "adapters/jeebus").glob("*.java"))
        run(
            "javac",
            "-Xlint:all",
            "-Werror",
            "-cp",
            classpath,
            "-d",
            str(classes),
            *(str(path) for path in [*owned_sources, *tests]),
        )
        run(
            "java",
            "-ea",
            "-cp",
            f"{classes}:{classpath}",
            "org.openmuc.jeebus.usecase.powerlimitation.controllablesystem.demo.MainTest",
        )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--openeebus", type=Path, default=ROOT / ".arena/generated/openeebus")
    parser.add_argument("--jeebus", type=Path, default=ROOT / ".arena/generated/jeebus")
    parser.add_argument("--only", choices=("c", "java"))
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="arena-native-") as directory:
        work = Path(directory)
        if args.only != "java":
            check_c(args.openeebus, work)
        if args.only != "c":
            check_java(args.jeebus, work)


if __name__ == "__main__":
    main()
