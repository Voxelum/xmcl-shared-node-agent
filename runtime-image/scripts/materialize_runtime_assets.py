#!/usr/bin/env python3
"""Materialize reviewed support assets plus the same-release Go runtime."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import stat
import sys
from urllib.parse import urlparse
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[1]


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runtime-binary", type=Path, required=True)
    parser.add_argument("--assets", type=Path, default=ROOT / "runtime-assets")
    args = parser.parse_args()

    lock = json.loads((ROOT / "runtime-assets.lock.json").read_text(encoding="utf-8"))
    busybox = lock["assets"]["busybox"]
    parsed = urlparse(busybox["source"])
    if parsed.scheme != "https" or parsed.hostname != "busybox.net":
        raise ValueError("busybox source is not approved")
    request = Request(busybox["source"], headers={"User-Agent": "xmcl-runtime-release/1"})
    with urlopen(request, timeout=60) as response:
        data = response.read(32 * 1024 * 1024 + 1)
    if len(data) > 32 * 1024 * 1024 or sha256(data) != busybox["sha256"]:
        raise ValueError("busybox download failed checksum or size validation")
    if not args.runtime_binary.is_file():
        raise ValueError("same-release runtime binary is missing")

    args.assets.mkdir(parents=True, exist_ok=True)
    (args.assets / "busybox").write_bytes(data)
    shutil.copyfile(args.runtime_binary, args.assets / "xmcl-shared-minecraft-runtime")
    for path in (args.assets / "busybox", args.assets / "xmcl-shared-minecraft-runtime"):
        path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as error:
        print(f"runtime asset materialization failed: {error}", file=sys.stderr)
        raise SystemExit(1)
