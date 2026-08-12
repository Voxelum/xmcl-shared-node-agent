#!/usr/bin/env python3
"""Generate the Go agent's compact catalog from the reviewed runtime lock."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import sys

RUNTIME_ROOT = Path(__file__).resolve().parents[1]
REPOSITORY_ROOT = RUNTIME_ROOT.parent
OUTPUT = REPOSITORY_ROOT / "internal" / "runtime" / "catalog.json"


def canonical(value: object) -> str:
    return json.dumps(value, indent=2, ensure_ascii=True) + "\n"


def generate() -> str:
    lock_path = RUNTIME_ROOT / "runtime-catalog.lock.json"
    raw_lock = lock_path.read_bytes()
    lock = json.loads(raw_lock)
    reviewed = json.loads(
        (RUNTIME_ROOT / "reviewed-toolchains.json").read_text(encoding="utf-8")
    )
    if lock.get("schemaVersion") != 1 or reviewed.get("schemaVersion") != 1:
        raise ValueError("reviewed catalog schema is invalid")
    requirements = lock.get("requirements")
    runtimes = lock.get("runtimes")
    toolchains = reviewed.get("toolchains")
    if not all(isinstance(value, list) for value in (requirements, runtimes, toolchains)):
        raise ValueError("reviewed catalog arrays are invalid")
    requirement_keys = {
        (item.get("component"), item.get("major"))
        for item in requirements
        if isinstance(item, dict)
    }
    runtime_majors = {
        item.get("major") for item in runtimes if isinstance(item, dict)
    }
    if len(requirement_keys) != len(requirements) or len(runtime_majors) != len(runtimes):
        raise ValueError("reviewed Java catalog contains duplicates")
    for toolchain in toolchains:
        java = toolchain.get("java", {}) if isinstance(toolchain, dict) else {}
        key = (java.get("component"), java.get("major"))
        if key not in requirement_keys or key[1] not in runtime_majors:
            raise ValueError("toolchain selects an unavailable Java runtime")
    return canonical(
        {
            "schemaVersion": 1,
            "sha256": hashlib.sha256(raw_lock).hexdigest(),
            "requirements": requirements,
            "runtimes": runtimes,
            "toolchains": toolchains,
        }
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    generated = generate()
    if args.check:
        if not OUTPUT.is_file() or OUTPUT.read_text(encoding="utf-8") != generated:
            print("embedded Go runtime catalog is stale", file=sys.stderr)
            return 1
        return 0
    OUTPUT.write_text(generated, encoding="utf-8", newline="\n")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as error:
        print(f"agent catalog sync failed: {error}", file=sys.stderr)
        raise SystemExit(1)
