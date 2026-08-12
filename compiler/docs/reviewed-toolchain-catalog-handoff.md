# Reviewed modded server toolchain catalog handoff

## Goal

Turn `ReviewedToolchainCatalog` from fake-test input into a generated,
reviewable, checksum-pinned catalog of real dedicated-server toolchains for
local XMCL client instance deployment.

The compiler must accept a local bundle only if its exact:

```text
Minecraft version
loader kind
loader version
Java component + major
runtime catalog revision
```

matches a reviewed toolchain entry. It must never derive an arbitrary download
URL at compile time.

Minecraft IDs are bounded canonical numeric identifiers: legacy `1.x.y` IDs
and reviewed modern IDs such as `26.2` are syntactically valid. Syntax alone
never authorizes a version: paths, whitespace/control characters, URLs,
commands, non-canonical numerals, and every tuple absent from the reviewed
catalog must fail closed.

## Scope

Work only in:

```text
Voxelum/xmcl-shared-node-agent/compiler
```

Do not build/publish the compiler image, run installers, change API/launcher
code, or enable a production builder. This package builds the **catalog
generation and review pipeline** only.

## Sources and trust model

Use official/reviewed metadata endpoints only:

| Loader | Approved metadata/artifact origin |
| --- | --- |
| Forge | `https://maven.minecraftforge.net` |
| Fabric | `https://meta.fabricmc.net`, `https://maven.fabricmc.net` |
| NeoForge | `https://maven.neoforged.net` |
| Quilt | `https://meta.quiltmc.org`, `https://maven.quiltmc.org` |
| Minecraft server artifacts | Mojang launcher metadata / piston hosts |

Mojang runtime catalog revision must be supplied from the reviewed
`runtime-image/runtime-catalog.lock.json` in the repository root and bound to
every toolchain catalog revision.

Requirements:

1. All metadata/artifact fetches use HTTPS, exact allowed hosts, no credentials,
   no redirects, bounded response sizes/timeouts, and strict JSON/XML parsing.
2. Every artifact in generated entries has a direct immutable URL, exact
   expected byte size, SHA-256 computed from the downloaded content, and
   canonical Maven coordinate.
3. If upstream provides SHA-1/SHA-256, verify it before accepting/download
   hashing. A missing or mismatched upstream checksum fails generation.
4. Never auto-merge catalog changes or auto-publish an image. The scheduled
   workflow opens a review PR containing generated changes only.
5. Catalog generation may discover candidates, but only explicit compatible
   loader/Minecraft pairs are emitted. Do not enumerate nonexistent
   combinations.

## Generated catalog shape

Create a tracked `toolchain-catalog.lock.json`, for example:

```json
{
  "schemaVersion": 1,
  "catalogRevision": "sha256-of-raw-lock",
  "runtimeCatalogRevision": "sha256-of-reviewed-runtime-catalog-lock",
  "approvedArtifactHosts": ["maven.minecraftforge.net"],
  "toolchains": [
    {
      "minecraftVersion": "1.20.1",
      "loader": { "kind": "forge", "version": "47.2.0" },
      "java": { "component": "java-runtime-gamma", "major": 17 },
      "jre": {
        "id": "java-runtime-gamma-17",
        "component": "java-runtime-gamma",
        "major": 17,
        "runtimeCatalogRevision": "..."
      },
      "artifacts": [
        {
          "role": "primary",
          "coordinate": "net.minecraftforge:forge:1.20.1-47.2.0:installer",
          "url": "https://...",
          "sizeBytes": 123,
          "sha256": "..."
        }
      ],
      "launchTemplate": "forge-unix-args-v1"
    }
  ]
}
```

The lock must be deterministic (canonical key/list ordering) and validate:

- no duplicate loader/version/Minecraft/Java tuple;
- every artifact host/URL/path/hash/size is valid;
- every JRE component/major appears in the reviewed runtime catalog;
- launch template matches the loader kind;
- all loader-specific required artifacts are present;
- raw lock revision recomputes exactly.

## Support policy

Initial production candidates must cover at least:

```text
Java 8 Forge legacy
Java 16 Forge or Fabric 1.17
Java 17 Fabric
Java 21 NeoForge
Java 25 current official component
Java 25 Fabric `26.2` (`0.19.3`)
```

Do not claim general all-version loader support until those reviewed entries
are real and staging passes. For a local bundle missing from the reviewed
catalog, return a clear `unsupported_compatibility` result and offer no
unverified download fallback.

## Automation

Add:

```text
scripts/update_toolchain_catalog.*
scripts/validate_toolchain_catalog.*
tests/toolchain_catalog.*
.github/workflows/update-toolchain-catalog.yml (repository root)
```

The workflow runs weekly and manually. It:

1. validates the pinned runtime catalog revision;
2. fetches approved metadata;
3. resolves allowed candidate toolchains;
4. downloads/validates artifacts to produce exact SHA-256/size records;
5. validates deterministic lock output;
6. opens a PR only if the lock changes.

It must not invoke the compiler builder, upload content, use Object Storage,
publish GHCR, or create Vultr resources.

## Tests

1. Forge/Fabric/NeoForge/Quilt fixture metadata produce exact deterministic
   coordinates.
2. Redirect, foreign host, missing checksum, checksum mismatch, size mismatch,
   malformed metadata, unsafe version, and duplicate tuple reject.
3. Runtime catalog revision/component/major mismatch rejects.
4. Generated catalog raw SHA/revision validates.
5. New Java 25 official runtime component can be selected when a valid
   reviewed loader candidate exists.
6. Catalog with an unknown template or missing loader-specific artifact
   rejects.

Run `npm test` and `npm run check`.
