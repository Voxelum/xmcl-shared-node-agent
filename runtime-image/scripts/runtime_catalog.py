"""Mojang-first, checksum-pinned Java runtime catalog support."""

from __future__ import annotations

import hashlib
import json
import posixpath
import re
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from typing import Any, Callable
from urllib.parse import urlparse
from urllib.request import Request, urlopen

MINECRAFT_VERSION_MANIFEST = "https://launchermeta.mojang.com/mc/game/version_manifest_v2.json"
JAVA_RUNTIME_MANIFEST = (
    "https://launchermeta.mojang.com/v1/products/java-runtime/"
    "2ec0cc96c44e5a76b9c8b7c39df7210883d12871/all.json"
)
PLATFORM = "linux"
ARCHITECTURE = "x64"
BASELINE_REQUIREMENTS = {
    ("jre-legacy", 8),
    ("java-runtime-alpha", 16),
    ("java-runtime-gamma", 17),
    ("java-runtime-delta", 21),
}
HEX = {"sha1": re.compile(r"^[a-f0-9]{40}$"), "sha256": re.compile(r"^[a-f0-9]{64}$")}
MAJOR_FROM_ZULU = re.compile(r"(?:zulu|jre)(\d+)", re.IGNORECASE)


class CatalogError(RuntimeError):
    """The upstream metadata cannot safely produce a catalog."""


FetchBytes = Callable[[str], bytes]


def canonical_json(value: Any) -> bytes:
    return json.dumps(value, indent=2, sort_keys=True, separators=(",", ": ")).encode("utf-8")


def parse_json(data: bytes, description: str) -> dict[str, Any]:
    try:
        value = json.loads(data)
    except json.JSONDecodeError as error:
        raise CatalogError(f"{description} is not JSON") from error
    if not isinstance(value, dict):
        raise CatalogError(f"{description} is not an object")
    return value


def check_hash(value: str, algorithm: str, subject: str) -> None:
    if not isinstance(value, str) or not HEX[algorithm].fullmatch(value):
        raise CatalogError(f"{subject} has an invalid {algorithm}")


def https_url(url: Any, hosts: set[str], subject: str) -> str:
    if not isinstance(url, str):
        raise CatalogError(f"{subject} has no URL")
    parsed = urlparse(url)
    if parsed.scheme != "https" or parsed.hostname not in hosts or parsed.username or parsed.password:
        raise CatalogError(f"{subject} has an untrusted URL")
    return url


def verify_hashes(data: bytes, hashes: dict[str, str], subject: str) -> None:
    if not hashes:
        raise CatalogError(f"{subject} has no checksum")
    for algorithm, expected in hashes.items():
        check_hash(expected, algorithm, subject)
        actual = hashlib.new(algorithm, data).hexdigest()
        if actual != expected:
            raise CatalogError(f"{subject} {algorithm} mismatch")


def metadata_hashes(value: dict[str, Any], subject: str, *, require_sha1: bool = True) -> dict[str, str]:
    hashes = {
        algorithm: value[algorithm]
        for algorithm in ("sha1", "sha256")
        if algorithm in value
    }
    if require_sha1 and "sha1" not in hashes:
        raise CatalogError(f"{subject} has no SHA-1")
    for algorithm, digest in hashes.items():
        check_hash(digest, algorithm, subject)
    return hashes


def fetch_url(url: str) -> bytes:
    request = Request(url, headers={"User-Agent": "xmcl-runtime-catalog/1"})
    for attempt in range(4):
        try:
            with urlopen(request, timeout=60) as response:
                if response.status != 200:
                    raise CatalogError(f"GET {url} returned {response.status}")
                return response.read()
        except Exception:
            if attempt == 3:
                raise
            time.sleep(2**attempt)
    raise AssertionError("unreachable")


def fetch_json(fetch: FetchBytes, url: str, description: str, hashes: dict[str, str] | None = None) -> dict[str, Any]:
    try:
        data = fetch(url)
    except Exception as error:
        raise CatalogError(f"cannot fetch {description}") from error
    if hashes:
        verify_hashes(data, hashes, description)
    return parse_json(data, description)


def required_java_versions(version_manifest: dict[str, Any], fetch: FetchBytes) -> list[dict[str, Any]]:
    versions = version_manifest.get("versions")
    if not isinstance(versions, list):
        raise CatalogError("Minecraft version manifest has no versions array")

    candidates: list[tuple[str, str, dict[str, str], str]] = []
    for version in versions:
        if not isinstance(version, dict):
            raise CatalogError("Minecraft version manifest contains an invalid version")
        url = https_url(version.get("url"), {"piston-meta.mojang.com"}, "version metadata")
        hashes = metadata_hashes(version, f"version metadata {version.get('id')}")
        version_id = version.get("id")
        if not isinstance(version_id, str):
            raise CatalogError("Minecraft version metadata has no ID")
        candidates.append((version_id, url, hashes, str(version.get("type", ""))))

    def read_requirement(candidate: tuple[str, str, dict[str, str], str]) -> tuple[str, int] | None:
        version_id, url, hashes, _ = candidate
        metadata = fetch_json(fetch, url, f"Minecraft version {version_id}", hashes)
        java = metadata.get("javaVersion")
        if java is None:
            return None
        if not isinstance(java, dict):
            raise CatalogError(f"Minecraft version {version_id} has invalid javaVersion")
        component = java.get("component")
        major = java.get("majorVersion")
        if not isinstance(component, str) or not component or not isinstance(major, int) or major < 1:
            raise CatalogError(f"Minecraft version {version_id} has invalid Java requirement")
        return component, major

    # Every listed version is considered so a future Java major cannot be silently skipped.
    with ThreadPoolExecutor(max_workers=12) as pool:
        discovered = list(pool.map(read_requirement, candidates))

    requirements = set(BASELINE_REQUIREMENTS)
    requirements.update(item for item in discovered if item is not None)
    return [
        {"component": component, "major": major}
        for component, major in sorted(requirements, key=lambda item: (item[1], item[0]))
    ]


def safe_runtime_path(path: Any) -> str:
    if not isinstance(path, str) or not path or path.startswith("/") or "\\" in path:
        raise CatalogError("runtime manifest has an unsafe path")
    parts = path.split("/")
    if any(part in ("", ".", "..") for part in parts):
        raise CatalogError("runtime manifest has an unsafe path")
    return path


def normalized_runtime_files(manifest: dict[str, Any]) -> list[dict[str, Any]]:
    files = manifest.get("files")
    if not isinstance(files, dict):
        raise CatalogError("runtime manifest has no files object")
    normalized: list[dict[str, Any]] = []
    for path, entry in files.items():
        path = safe_runtime_path(path)
        if not isinstance(entry, dict):
            raise CatalogError(f"runtime file {path} is invalid")
        kind = entry.get("type")
        if kind == "directory":
            normalized.append({"path": path, "type": "directory"})
            continue
        if kind == "link":
            target = entry.get("target")
            if not isinstance(target, str) or target.startswith("/") or "\\" in target:
                raise CatalogError(f"runtime link {path} is unsafe")
            resolved_target = posixpath.normpath(posixpath.join(posixpath.dirname(path), target))
            if resolved_target == ".." or resolved_target.startswith("../"):
                raise CatalogError(f"runtime link {path} escapes the runtime")
            normalized.append({"path": path, "target": target, "type": "link"})
            continue
        if kind != "file":
            raise CatalogError(f"runtime file {path} has unsupported type")
        downloads = entry.get("downloads")
        if not isinstance(downloads, dict) or not isinstance(downloads.get("raw"), dict):
            raise CatalogError(f"runtime file {path} has no raw download")
        download = downloads["raw"]
        hashes = metadata_hashes(download, f"runtime file {path}")
        url = https_url(download.get("url"), {"piston-data.mojang.com"}, f"runtime file {path}")
        size = download.get("size")
        if not isinstance(size, int) or size < 0:
            raise CatalogError(f"runtime file {path} has invalid size")
        normalized.append(
            {
                "executable": bool(entry.get("executable", False)),
                "hashes": hashes,
                "path": path,
                "size": size,
                "type": "file",
                "url": url,
            },
        )
    if not any(item["type"] == "file" and item["path"] == "bin/java" for item in normalized):
        raise CatalogError("runtime manifest does not contain bin/java")
    return sorted(normalized, key=lambda item: item["path"])


def official_resolution(
    component: str,
    major: int,
    runtime_manifest: dict[str, Any],
    fetch: FetchBytes,
) -> dict[str, Any] | None:
    platform = runtime_manifest.get(PLATFORM)
    if not isinstance(platform, dict):
        raise CatalogError("Java runtime manifest has no Linux catalog")
    records = platform.get(component)
    if not records:
        return None
    if not isinstance(records, list):
        raise CatalogError(f"Java runtime component {component} is invalid")

    candidates: list[tuple[str, str, dict[str, str], int]] = []
    for record in records:
        if not isinstance(record, dict):
            raise CatalogError(f"Java runtime component {component} contains an invalid record")
        availability = record.get("availability", {})
        if not isinstance(availability, dict) or availability.get("progress") != 100:
            continue
        descriptor = record.get("manifest")
        version = record.get("version")
        if not isinstance(descriptor, dict) or not isinstance(version, dict):
            raise CatalogError(f"Java runtime component {component} has malformed metadata")
        url = https_url(descriptor.get("url"), {"piston-meta.mojang.com"}, f"{component} manifest")
        hashes = metadata_hashes(descriptor, f"{component} manifest")
        released = version.get("released")
        name = version.get("name")
        if not isinstance(released, str) or not isinstance(name, str):
            raise CatalogError(f"Java runtime component {component} has invalid version metadata")
        size = descriptor.get("size")
        if not isinstance(size, int) or size < 0:
            raise CatalogError(f"{component} manifest has invalid size")
        candidates.append((released, url, hashes, size))

    # A malformed official candidate is an error, not a reason to downgrade to Zulu.
    for released, url, hashes, size in sorted(candidates, reverse=True):
        manifest = fetch_json(fetch, url, f"{component} runtime manifest", hashes)
        return {
            "component": component,
            "files": normalized_runtime_files(manifest),
            "major": major,
            "manifest": {"hashes": hashes, "size": size, "url": url},
            "released": released,
            "source": "mojang",
        }
    return None


def zulu_resolution(component: str, major: int, zulu: dict[str, Any]) -> dict[str, Any]:
    entries = zulu.get(component)
    if not isinstance(entries, list):
        raise CatalogError(f"no official {component} runtime and no Zulu fallback catalog entry")
    candidates: list[dict[str, Any]] = []
    for entry in entries:
        if not isinstance(entry, dict) or entry.get("os") != PLATFORM or entry.get("architecture") != ARCHITECTURE:
            continue
        url = https_url(entry.get("url"), {"static.azul.com", "cdn.azul.com"}, f"Zulu {component}")
        match = MAJOR_FROM_ZULU.search(url)
        if not match or int(match.group(1)) != major:
            continue
        digest = entry.get("sha256")
        check_hash(digest, "sha256", f"Zulu {component}")
        features = entry.get("features")
        if not isinstance(features, list) or not all(isinstance(feature, str) for feature in features):
            raise CatalogError(f"Zulu {component} has invalid features")
        size = entry.get("size")
        if not isinstance(size, int) or size < 0:
            raise CatalogError(f"Zulu {component} has invalid size")
        candidates.append({"features": sorted(features), "sha256": digest, "size": size, "url": url})
    if not candidates:
        raise CatalogError(f"no usable Linux x64 fallback for {component} Java {major}")
    archive = sorted(candidates, key=lambda item: (len(item["features"]), item["features"], item["url"]))[0]
    return {
        "archive": archive,
        "component": component,
        "major": major,
        "released": "",
        "source": "zulu",
    }


def build_catalog(
    version_manifest: dict[str, Any],
    runtime_manifest: dict[str, Any],
    zulu_catalog: dict[str, Any],
    fetch: FetchBytes,
) -> dict[str, Any]:
    requirements = required_java_versions(version_manifest, fetch)
    resolutions: list[dict[str, Any]] = []
    for requirement in requirements:
        component = requirement["component"]
        major = requirement["major"]
        resolution = official_resolution(component, major, runtime_manifest, fetch)
        resolutions.append(resolution or zulu_resolution(component, major, zulu_catalog))

    chosen: list[dict[str, Any]] = []
    for major in sorted({item["major"] for item in resolutions}):
        candidates = [item for item in resolutions if item["major"] == major]
        official = [item for item in candidates if item["source"] == "mojang"]
        selected = max(official or candidates, key=lambda item: (item.get("released", ""), item["component"]))
        chosen.append({"component": selected["component"], "major": major})

    return {
        "platform": {"architecture": ARCHITECTURE, "os": PLATFORM},
        "requirements": requirements,
        "runtimes": chosen,
        "schemaVersion": 1,
        "sources": {
            "javaRuntimeManifest": JAVA_RUNTIME_MANIFEST,
            "minecraftVersionManifest": MINECRAFT_VERSION_MANIFEST,
            "zuluFallback": "catalog/zulu-fallback.json",
        },
        "resolutions": sorted(resolutions, key=lambda item: (item["major"], item["component"])),
    }


def generate_catalog(zulu_catalog: dict[str, Any], fetch: FetchBytes = fetch_url) -> dict[str, Any]:
    version_manifest = fetch_json(
        fetch,
        MINECRAFT_VERSION_MANIFEST,
        "Minecraft version manifest",
    )
    runtime_manifest = fetch_json(
        fetch,
        JAVA_RUNTIME_MANIFEST,
        "Mojang Java runtime manifest",
    )
    return build_catalog(version_manifest, runtime_manifest, zulu_catalog, fetch)
