#!/usr/bin/env python3
"""Write the immutable cross-artifact node runtime release manifest."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys

SHA256 = re.compile(r"^sha256:[a-f0-9]{64}$")
COMMIT = re.compile(r"^[a-f0-9]{40}$")


def file_record(path: Path) -> dict[str, object]:
    data = path.read_bytes()
    return {
        "filename": path.name,
        "sha256": hashlib.sha256(data).hexdigest(),
        "sizeBytes": len(data),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--commit", required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--image-digest", required=True)
    parser.add_argument("--catalog", type=Path, required=True)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--quota-helper", type=Path, required=True)
    parser.add_argument("--runtime", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    if not COMMIT.fullmatch(args.commit):
        raise ValueError("release commit is invalid")
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", args.tag):
        raise ValueError("release tag is invalid")
    if not SHA256.fullmatch(args.image_digest):
        raise ValueError("runtime image digest is invalid")
    catalog_sha256 = hashlib.sha256(args.catalog.read_bytes()).hexdigest()
    manifest = {
        "schemaVersion": 1,
        "gitCommit": args.commit,
        "tag": args.tag,
        "runtimeCatalogSha256": catalog_sha256,
        "runtimeImage": {
            "name": "ghcr.io/voxelum/xmcl-shared-minecraft-runtime",
            "digest": args.image_digest,
        },
        "artifacts": {
            "nodeAgent": file_record(args.agent),
            "quotaHelper": file_record(args.quota_helper),
            "runtimeEntrypoint": file_record(args.runtime),
        },
    }
    args.output.write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
        newline="\n",
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError) as error:
        print(f"release manifest generation failed: {error}", file=sys.stderr)
        raise SystemExit(1)
