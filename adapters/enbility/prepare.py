"""Install Enbility timing, pairing, and network observation hooks."""

from pathlib import Path

from scripts.overlays import apply_overlay

SOURCES = {"eebus": "enbility-eebus", "ship": "enbility-ship", "spine": "enbility-spine"}


def prepare(target: Path, root: Path, lock: dict) -> None:
    apply_overlay(target, target / "adapter/patches/overlay.patch")
