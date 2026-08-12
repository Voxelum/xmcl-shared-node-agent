# Reviewed loader builder handoff

## Scope

Implement the reviewed builder behind `CompilerWorker` in this package only:

```text
Voxelum/xmcl-shared-node-agent/compiler
```

It consumes a validated `.xmcl-server-bundle` from an already-working local
client instance and produces immutable dedicated-server content. Do not modify
the launcher, API, node agent, billing, Vultr resources, or public route gates.

## Existing guarantees

The worker already:

- obtains one exact compiler input GET grant and one immutable content PUT
  grant;
- validates archive paths/hashes and rejects `server.sh`, local Java paths,
  Docker options, and local EULA files;
- binds Minecraft/loader/Java component/major/runtime catalog revision to the
  frozen API manifest;
- fails closed with `FailClosedRuntimeBuilder`.

Replace that fail-closed builder only with a builder that has every required
reviewed toolchain input explicitly injected.

## Builder inputs

```text
runtime catalog revision
Java component + major
Minecraft version
loader kind + loader version
validated local instance files + hashes
resolved mod/config intent
reviewed toolchain catalog
```

The compiler must not use launcher-provided commands, URLs, Java paths, JVM
arguments, Docker image names, or server scripts.

## Approved loader families

Support server assembly through deterministic, version-derived artifact
coordinates:

```text
forge
fabric
neoforge
quilt
```

All artifact origins must be allowlisted and a requested artifact must match
its expected coordinate/version/hash from a reviewed toolchain catalog. Do not
use generic arbitrary URL fetching.

For each family, the builder:

1. Chooses the exact reviewed JRE from the same runtime catalog revision.
2. Acquires only approved installer/server artifacts with strict HTTPS, no
   redirect, response size, checksum, and timeout rules.
3. Runs an installer in an ephemeral non-root builder sandbox with no secrets,
   no Docker socket, read-only base filesystem, bounded workspace/PID/memory,
   and limited egress only while acquiring approved artifacts.
4. Produces server runtime files in the output content root.
5. Copies only validated server-relevant local bundle content.
6. Generates `.xmcl/runtime.json` and `.xmcl/launch.sh` itself.
7. Packages deterministic `.tar.zst` immutable content and validates every
   output path/hash before the existing compiler worker uploads it.

No generated server runtime may carry a user-provided shell script. The
generated launcher has fixed `exec "$XMCL_JAVA" ...` behavior with compiler
controlled arguments only.

## Toolchain catalog

Create a versioned reviewed toolchain catalog separate from user bundles. It
must include:

- the raw runtime-catalog revision it is compatible with;
- approved artifact host(s) and coordinate rules;
- exact artifact URLs and SHA-256 once approved;
- selected JRE component/major;
- output launch plan template owned by the compiler.

Initially implement artifact resolution and installer execution behind
interfaces with deterministic fake implementations for tests. Production
composition must still fail closed when the reviewed catalog, verified JRE
root, sandbox runner, or artifact downloader is absent.

Do not claim that Forge/Fabric/NeoForge/Quilt compilation is live until
real reviewed artifacts and toolchains have been installed.

## Required tests

1. Each loader kind resolves only a known approved toolchain coordinate.
2. Unknown loader version/catalog revision/Java component/major rejects before
   artifact download.
3. Redirect, wrong host, oversize, hash mismatch, and installer failure leave
   no published output.
4. Builder output contains generated runtime descriptor and launcher, but never
   local `server.sh`, local EULA, local Java, or arbitrary command.
5. Output is deterministic for the same frozen bundle/toolchain revision.
6. Output grant remains immutable (`If-None-Match: *`) and no List/Delete
   operation is used.
7. A fake Java 8 Forge, Java 16 Forge/Fabric, Java 17 Fabric, Java 21
   NeoForge, and Java 25 current component fixture each exercises the
   corresponding toolchain selection.

Run `npm test` and `npm run check`.
