#!/usr/bin/env python3
"""Validate the Java runtime catalog before review or image construction."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import posixpath
import sys
from typing import Any
from urllib.parse import urlparse

from runtime_catalog import BASELINE_REQUIREMENTS, CatalogError, HEX, verify_hashes

ROOT = Path(__file__).resolve().parents[1]


def require_url(url: Any, source: str) -> None:
    parsed = urlparse(url if isinstance(url, str) else "")
    allowed = {"mojang": {"piston-meta.mojang.com", "piston-data.mojang.com"}, "zulu": {"static.azul.com", "cdn.azul.com"}}
    if parsed.scheme != "https" or parsed.hostname not in allowed[source]:
        raise CatalogError(f"{source} runtime has an untrusted URL")


def catalog_key(item: dict[str, Any]) -> tuple[str, int]:
    component, major = item.get("component"), item.get("major")
    if not isinstance(component, str) or not isinstance(major, int) or major < 1:
        raise CatalogError("runtime entry has an invalid component or major")
    return component, major


def read_catalog(path: Path) -> dict[str, Any]:
    try:
        catalog = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise CatalogError("catalog cannot be read") from error
    if catalog.get("schemaVersion") != 1 or catalog.get("platform") != {"architecture": "x64", "os": "linux"}:
        raise CatalogError("catalog schema or platform is invalid")
    return catalog


def validate(catalog: dict[str, Any], assets: Path | None = None) -> None:
    requirements = catalog.get("requirements")
    resolutions = catalog.get("resolutions")
    runtimes = catalog.get("runtimes")
    if not all(isinstance(value, list) for value in (requirements, resolutions, runtimes)):
        raise CatalogError("catalog arrays are invalid")
    requirement_keys = {catalog_key(item) for item in requirements if isinstance(item, dict)}
    if len(requirement_keys) != len(requirements) or not BASELINE_REQUIREMENTS.issubset(requirement_keys):
        raise CatalogError("catalog does not cover all baseline Java requirements")

    resolution_by_key: dict[tuple[str, int], dict[str, Any]] = {}
    for item in resolutions:
        if not isinstance(item, dict):
            raise CatalogError("catalog has an invalid resolution")
        key = catalog_key(item)
        if key in resolution_by_key:
            raise CatalogError("catalog has duplicate resolutions")
        source = item.get("source")
        if source == "mojang":
            manifest = item.get("manifest")
            files = item.get("files")
            if not isinstance(manifest, dict) or not isinstance(files, list):
                raise CatalogError("Mojang runtime is incomplete")
            require_url(manifest.get("url"), "mojang")
            hashes = manifest.get("hashes")
            if not isinstance(hashes, dict) or "sha1" not in hashes:
                raise CatalogError("Mojang runtime manifest has no SHA-1")
            for algorithm, digest in hashes.items():
                if algorithm not in HEX or not isinstance(digest, str) or not HEX[algorithm].fullmatch(digest):
                    raise CatalogError("Mojang runtime manifest checksum is invalid")
            seen_paths: set[str] = set()
            for file in files:
                if not isinstance(file, dict) or not isinstance(file.get("path"), str):
                    raise CatalogError("Mojang runtime file is invalid")
                path = file["path"]
                if path in seen_paths or path.startswith("/") or "\\" in path or ".." in path.split("/"):
                    raise CatalogError("Mojang runtime file path is unsafe")
                seen_paths.add(path)
                if file.get("type") == "file":
                    require_url(file.get("url"), "mojang")
                    hashes = file.get("hashes")
                    if not isinstance(hashes, dict) or "sha1" not in hashes:
                        raise CatalogError("Mojang runtime file has no SHA-1")
                    for algorithm, digest in hashes.items():
                        if algorithm not in HEX or not isinstance(digest, str) or not HEX[algorithm].fullmatch(digest):
                            raise CatalogError("Mojang runtime file checksum is invalid")
                elif file.get("type") not in {"directory", "link"}:
                    raise CatalogError("Mojang runtime file type is invalid")
                elif file["type"] == "link":
                    link = file.get("target")
                    if not isinstance(link, str) or link.startswith("/") or "\\" in link:
                        raise CatalogError("Mojang runtime link is unsafe")
                    resolved = posixpath.normpath(posixpath.join(posixpath.dirname(path), link))
                    if resolved == ".." or resolved.startswith("../"):
                        raise CatalogError("Mojang runtime link escapes the runtime")
            if "bin/java" not in seen_paths:
                raise CatalogError("Mojang runtime has no bin/java")
        elif source == "zulu":
            archive = item.get("archive")
            if not isinstance(archive, dict):
                raise CatalogError("Zulu runtime is incomplete")
            require_url(archive.get("url"), "zulu")
            digest = archive.get("sha256")
            if not isinstance(digest, str) or not HEX["sha256"].fullmatch(digest):
                raise CatalogError("Zulu runtime has no SHA-256")
        else:
            raise CatalogError("runtime source is invalid")
        resolution_by_key[key] = item

    if set(resolution_by_key) != requirement_keys:
        raise CatalogError("catalog requirements and resolutions differ")
    selected_majors: set[int] = set()
    for item in runtimes:
        if not isinstance(item, dict):
            raise CatalogError("catalog has an invalid selected runtime")
        key = catalog_key(item)
        if key not in resolution_by_key or key[1] in selected_majors:
            raise CatalogError("catalog selected runtime is invalid")
        selected_majors.add(key[1])
    if selected_majors != {major for _, major in requirement_keys}:
        raise CatalogError("catalog does not select every required Java major")

    if assets:
        for selected in runtimes:
            resolution = resolution_by_key[catalog_key(selected)]
            root = assets / "jre" / str(selected["major"])
            if resolution["source"] == "mojang":
                for file in resolution["files"]:
                    if file["type"] != "file":
                        continue
                    path = root / file["path"]
                    if not path.is_file():
                        raise CatalogError(f"missing runtime file: {path}")
                    verify_hashes(path.read_bytes(), file["hashes"], str(path))
            else:
                archive = assets / ".catalog-archives" / f"jre-{selected['major']}.tar.gz"
                if not archive.is_file():
                    raise CatalogError(f"missing verified fallback archive: {archive}")
                verify_hashes(archive.read_bytes(), {"sha256": resolution["archive"]["sha256"]}, str(archive))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--lock", type=Path, default=ROOT / "runtime-catalog.lock.json")
    parser.add_argument("--require-files", action="store_true")
    parser.add_argument("--assets", type=Path, default=ROOT / "runtime-assets")
    args = parser.parse_args()
    validate(read_catalog(args.lock), args.assets if args.require_files else None)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except CatalogError as error:
        print(f"runtime catalog validation failed: {error}", file=sys.stderr)
        raise SystemExit(1)
