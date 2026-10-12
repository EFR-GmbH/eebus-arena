"""Instrument OpenEEBUS examples without replacing native protocol decisions."""

import shutil
from pathlib import Path

from scripts.overlays import apply_overlay

SOURCES = {"upstream": "openeebus", "libwebsockets": "libwebsockets"}


def prepare(target: Path, root: Path, lock: dict) -> None:
    upstream = target / "upstream"
    adapter = target / "adapter"
    pairing = (upstream / "src/ship/api/ship_pairing_interface.h").exists()
    overlay = "pairing" if pairing else "overlay"
    apply_overlay(target, adapter / "patches" / f"{overlay}.patch")
    shutil.copytree(adapter, upstream / "arena")

    # These hooks need the example's private structs, so they must compile in
    # its translation unit. Keep their source in C files rather than Python.
    for example, directory in (("hems", "hems"), ("hpsrv", "heat_pump")):
        source = upstream / "examples" / directory / f"{example}.c"
        with source.open("a") as output:
            output.write("\n")
            output.write((adapter / f"{example}_hooks.c.inc").read_text())
