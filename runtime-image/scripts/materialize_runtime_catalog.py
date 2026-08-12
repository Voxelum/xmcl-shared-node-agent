#!/usr/bin/env python3
"""Materialize only checksum-pinned Java runtimes from the reviewed catalog."""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import shutil
import stat
import sys
import tarfile

from runtime_catalog import CatalogError, fetch_url, verify_hashes
from validate_runtime_catalog import read_catalog, validate

ROOT = Path(__file__).resolve().parents[1]


def target(root: Path, relative: str) -> Path:
    candidate = (root / relative).resolve()
    if root.resolve() not in candidate.parents:
        raise CatalogError(f"unsafe output path: {relative}")
    return candidate


def download(path: Path, url: str, hashes: dict[str, str]) -> None:
    data = fetch_url(url)
    verify_hashes(data, hashes, url)
    path.parent.mkdir(parents=True, exist_ok=True)
    partial = path.with_name(f"{path.name}.partial")
    partial.write_bytes(data)
    partial.replace(path)


def extract_zulu(archive: Path, destination: Path) -> None:
    staging = destination.with_name(f"{destination.name}.extract")
    shutil.rmtree(staging, ignore_errors=True)
    staging.mkdir(parents=True)
    with tarfile.open(archive, "r:*") as content:
        for member in content.getmembers():
            if member.name.startswith("/") or ".." in Path(member.name).parts:
                raise CatalogError("Zulu archive has an unsafe path")
            output = target(staging, member.name)
            if member.isdir():
                output.mkdir(parents=True, exist_ok=True)
            elif member.isfile():
                output.parent.mkdir(parents=True, exist_ok=True)
                source = content.extractfile(member)
                if source is None:
                    raise CatalogError("Zulu archive cannot be read")
                with output.open("wb") as destination_file:
                    shutil.copyfileobj(source, destination_file)
                os.chmod(output, member.mode & 0o777)
            elif member.issym() or member.islnk():
                if Path(member.linkname).is_absolute() or ".." in Path(member.linkname).parts:
                    raise CatalogError("Zulu archive has an unsafe link")
                output.parent.mkdir(parents=True, exist_ok=True)
                os.symlink(member.linkname, output)
            else:
                raise CatalogError("Zulu archive has an unsupported member")
    children = list(staging.iterdir())
    source = children[0] if len(children) == 1 and children[0].is_dir() else staging
    shutil.rmtree(destination, ignore_errors=True)
    source.replace(destination)
    if staging.exists():
        shutil.rmtree(staging)


def materialize(catalog: dict, assets: Path) -> None:
    resolutions = {(item["component"], item["major"]): item for item in catalog["resolutions"]}
    for selected in catalog["runtimes"]:
        resolution = resolutions[(selected["component"], selected["major"])]
        runtime_root = assets / "jre" / str(selected["major"])
        shutil.rmtree(runtime_root, ignore_errors=True)
        if resolution["source"] == "mojang":
            for file in resolution["files"]:
                output = target(runtime_root, file["path"])
                if file["type"] == "directory":
                    output.mkdir(parents=True, exist_ok=True)
                elif file["type"] == "link":
                    output.parent.mkdir(parents=True, exist_ok=True)
                    os.symlink(file["target"], output)
                else:
                    download(output, file["url"], file["hashes"])
                    if file["executable"]:
                        output.chmod(output.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
        else:
            archive = assets / ".catalog-archives" / f"jre-{selected['major']}.tar.gz"
            download(archive, resolution["archive"]["url"], {"sha256": resolution["archive"]["sha256"]})
            extract_zulu(archive, runtime_root)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--lock", type=Path, default=ROOT / "runtime-catalog.lock.json")
    parser.add_argument("--assets", type=Path, default=ROOT / "runtime-assets")
    args = parser.parse_args()
    catalog = read_catalog(args.lock)
    validate(catalog)
    args.assets.mkdir(parents=True, exist_ok=True)
    materialize(catalog, args.assets)
    validate(catalog, args.assets)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (CatalogError, OSError, tarfile.TarError) as error:
        print(f"runtime materialization failed: {error}", file=sys.stderr)
        raise SystemExit(1)
