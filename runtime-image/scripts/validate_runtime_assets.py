#!/usr/bin/env python3
"""Validate the runtime image asset manifest and an optional extracted bundle."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
LOCK = ROOT / "runtime-assets.lock.json"
REQUIRED = {"busybox"}
SHA256 = re.compile(r"^[a-f0-9]{64}$")
PINNED_BASE = (
    "FROM gcr.io/distroless/base-debian12@"
    "sha256:d2add786f2a5f43d1ab3ae54cd3193de929d0d12c378ff60891921f56f3e47ff"
)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--require-files", action="store_true")
    args = parser.parse_args()

    lock = json.loads(LOCK.read_text(encoding="utf-8"))
    if lock.get("schemaVersion") != 2 or set(lock.get("assets", {})) != REQUIRED:
        raise ValueError("runtime asset lock has an invalid schema or asset set")
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    if PINNED_BASE not in dockerfile:
        raise ValueError("runtime image base is not the reviewed immutable digest")

    for name, value in lock["assets"].items():
        if not isinstance(value.get("source"), str) or not value["source"]:
            raise ValueError(f"{name} has no provenance")
        digest = value.get("sha256")
        if not isinstance(digest, str):
            raise ValueError(f"{name} has no SHA-256")
        if args.require_files and not SHA256.fullmatch(digest):
            raise ValueError(f"{name} must have a real lowercase SHA-256 before release")

    if not args.require_files:
        return 0

    assets = ROOT / "runtime-assets"
    for name, value in lock["assets"].items():
        path = assets / name
        if not path.is_file():
            raise ValueError(f"missing verified asset file: {name}")
        actual = sha256_file(path)
        if actual != value["sha256"]:
            raise ValueError(f"SHA-256 mismatch for {name}")
    runtime = assets / "xmcl-shared-minecraft-runtime"
    if not runtime.is_file() or not runtime.stat().st_mode & 0o111:
        raise ValueError("same-release runtime binary is missing or not executable")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"runtime asset validation failed: {error}", file=sys.stderr)
        raise SystemExit(1)
