#!/usr/bin/env python3
"""Generate the tracked Java runtime catalog from official Mojang metadata."""

from __future__ import annotations

import argparse
import json
from pathlib import Path

from runtime_catalog import CatalogError, canonical_json, generate_catalog

ROOT = Path(__file__).resolve().parents[1]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=ROOT / "runtime-catalog.lock.json")
    parser.add_argument("--zulu-catalog", type=Path, default=ROOT / "catalog" / "zulu-fallback.json")
    args = parser.parse_args()
    zulu = json.loads(args.zulu_catalog.read_text(encoding="utf-8"))
    catalog = generate_catalog(zulu)
    output = args.output.resolve()
    if ROOT not in output.parents:
        raise CatalogError("catalog output must be inside the repository")
    output.write_bytes(canonical_json(catalog) + b"\n")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (CatalogError, OSError, json.JSONDecodeError) as error:
        print(f"runtime catalog update failed: {error}")
        raise SystemExit(1)
