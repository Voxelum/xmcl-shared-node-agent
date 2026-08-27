import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import * as zlib from "node:zlib";
import {
  DeterministicFakeArtifactDownloader,
  DeterministicFakeJreRegistry,
  DeterministicFakeSandboxRunner,
  ReviewedRuntimeBuilder,
  ReviewedToolchainCatalog,
  StrictArtifactDownloader,
} from "../src/reviewed-builder.mjs";

const encoder = new TextEncoder();
const revision = "a".repeat(64);

function sha(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function bytes(value) {
  return encoder.encode(value);
}

function primaryCoordinate(minecraftVersion, loader) {
  switch (loader.kind) {
    case "forge":
      return `net.minecraftforge:forge:${minecraftVersion}-${loader.version}:installer`;
    case "fabric":
      return `net.fabricmc:fabric-loader:${loader.version}`;
    case "neoforge":
      return `net.neoforged:neoforge:${loader.version}:installer`;
    case "quilt":
      return `org.quiltmc:quilt-loader:${loader.version}`;
    default:
      throw new Error("unknown fixture loader");
  }
}

function toolchainFixture({
  minecraftVersion = "1.21.1",
  kind = "fabric",
  version = "0.16.10",
  component = "java-runtime-delta",
  major = 21,
} = {}) {
  const loader = { kind, version };
  const java = { component, major };
  const coordinate = primaryCoordinate(minecraftVersion, loader);
  const artifactBytes = bytes(`reviewed:${coordinate}`);
  const jreBytes = bytes(`reviewed-jre:${component}:${major}`);
  const jre = {
    id: `jre-${component}-${major}`,
    sha256: sha(jreBytes),
    component,
    major,
    runtimeCatalogRevision: revision,
  };
  const toolchain = {
    minecraftVersion,
    loader,
    java,
    jre,
    artifacts: [{
      role: "primary",
      coordinate,
      url: `https://toolchain.example/${encodeURIComponent(coordinate)}.jar`,
      sha256: sha(artifactBytes),
      sizeBytes: artifactBytes.byteLength,
    }],
    launchTemplate: kind === "forge" ? "forge-unix-args-v1" :
      kind === "neoforge" ? "neoforge-unix-args-v1" :
      kind === "fabric" ? "fabric-server-jar-v1" : "quilt-server-jar-v1",
  };
  return { toolchain, artifactBytes, jre };
}

function catalogFor(toolchains, options) {
  return new ReviewedToolchainCatalog({
    schemaVersion: 1,
    catalogVersion: "review-1",
    runtimeCatalogRevision: revision,
    approvedArtifactHosts: ["toolchain.example"],
    toolchains: toolchains.map((fixture) => fixture.toolchain),
  }, options);
}

function bundleFor(toolchain, localFiles = [{
  path: "instance/mods/example.jar",
  bytes: bytes("local-mod"),
}]) {
  const files = localFiles.map((file) => ({
    path: file.path,
    sha256: sha(file.bytes),
    sizeBytes: file.bytes.byteLength,
  })).sort((left, right) => left.path.localeCompare(right.path));
  return {
    manifest: {
      minecraftVersion: toolchain.minecraftVersion,
      loader: { ...toolchain.loader },
      javaRequirement: { ...toolchain.java },
      runtimeCatalog: { sha256: revision },
      files,
    },
    entries: new Map(localFiles.map((file) => [file.path, { bytes: file.bytes }])),
  };
}

function builderFor(fixture, { files, fail = false, downloader } = {}) {
  return new ReviewedRuntimeBuilder({
    toolchainCatalog: catalogFor([fixture]),
    verifiedJres: new DeterministicFakeJreRegistry([fixture.jre]),
    sandboxRunner: new DeterministicFakeSandboxRunner({ files, fail }),
    artifactDownloader: downloader ?? new DeterministicFakeArtifactDownloader(
      new Map([[fixture.toolchain.artifacts[0].coordinate, fixture.artifactBytes]]),
    ),
  });
}

function buildInput(fixture, localFiles) {
  return {
    bundle: bundleFor(fixture.toolchain, localFiles),
    frozenManifest: { compatibility: { runtimeCatalog: { sha256: revision } } },
    expectedContentKey: "shared-hosting/account/service/compiler-content/content.tar.zst",
  };
}

test("reviewed catalog resolves only exact Forge, Fabric, NeoForge, and Quilt fixtures", async () => {
  const fixtures = [
    toolchainFixture({ minecraftVersion: "1.12.2", kind: "forge", version: "14.23.5", component: "jre-legacy", major: 8 }),
    toolchainFixture({ minecraftVersion: "1.16.5", kind: "forge", version: "36.2.39", component: "java-runtime-alpha", major: 16 }),
    toolchainFixture({ minecraftVersion: "1.16.5", kind: "fabric", version: "0.11.7", component: "java-runtime-alpha", major: 16 }),
    toolchainFixture({ minecraftVersion: "1.18.2", kind: "fabric", version: "0.14.22", component: "java-runtime-beta", major: 17 }),
    toolchainFixture({ minecraftVersion: "1.21.1", kind: "neoforge", version: "21.1.115", component: "java-runtime-delta", major: 21 }),
    toolchainFixture({ minecraftVersion: "1.21.8", kind: "quilt", version: "0.28.0", component: "java-runtime-epsilon", major: 25 }),
  ];

  for (const fixture of fixtures) {
    const downloader = new DeterministicFakeArtifactDownloader(new Map([
      [fixture.toolchain.artifacts[0].coordinate, fixture.artifactBytes],
    ]));
    const sandboxRunner = new DeterministicFakeSandboxRunner();
    const builder = new ReviewedRuntimeBuilder({
      toolchainCatalog: catalogFor([fixture]),
      verifiedJres: new DeterministicFakeJreRegistry([fixture.jre]),
      sandboxRunner,
      artifactDownloader: downloader,
    });
    const built = await builder.build(buildInput(fixture));
    assert.deepEqual(downloader.calls, [fixture.toolchain.artifacts[0].coordinate]);
    assert.equal(sandboxRunner.calls[0].plan.family, fixture.toolchain.loader.kind);
    assert.deepEqual(sandboxRunner.calls[0].plan.java, fixture.toolchain.java);
    assert.equal(built.archive.subarray(0, 4).toString(), Uint8Array.of(0x28, 0xb5, 0x2f, 0xfd).toString());
    assert.deepEqual(
      built.content.entries.map((entry) => entry.path),
      [...built.content.entries.map((entry) => entry.path)].sort(),
    );
    assert.ok(built.content.entries.some((entry) => entry.path === ".xmcl/runtime.json"));
    assert.ok(built.content.entries.some((entry) => entry.path === ".xmcl/launch.sh"));
    assert.ok(!built.content.entries.some((entry) => /(?:^|\/)(?:server\.sh|eula\.txt|java)$/i.test(entry.path)));
  }
});

test("unknown reviewed compatibility rejects before artifact download", async () => {
  const fixture = toolchainFixture();
  const downloader = new DeterministicFakeArtifactDownloader(new Map([
    [fixture.toolchain.artifacts[0].coordinate, fixture.artifactBytes],
  ]));
  const builder = builderFor(fixture, { downloader });
  const unknowns = [
    { loader: { ...fixture.toolchain.loader, version: "0.0.0" } },
    { runtimeCatalog: { sha256: "b".repeat(64) } },
    { javaRequirement: { ...fixture.toolchain.java, component: "other-java" } },
    { javaRequirement: { ...fixture.toolchain.java, major: 22 } },
  ];
  for (const replacement of unknowns) {
    const input = buildInput(fixture);
    input.bundle.manifest = { ...input.bundle.manifest, ...replacement };
    await assert.rejects(() => builder.build(input), /unsupported_compatibility/);
  }
  assert.deepEqual(downloader.calls, []);
});

test("production catalog restriction rejects unsupported families before download", async () => {
  const fabric = toolchainFixture();
  const neoforge = toolchainFixture({
    minecraftVersion: "1.21.1",
    kind: "neoforge",
    version: "21.1.115",
  });
  const downloader = new DeterministicFakeArtifactDownloader(new Map([
    [fabric.toolchain.artifacts[0].coordinate, fabric.artifactBytes],
  ]));
  const builder = new ReviewedRuntimeBuilder({
    toolchainCatalog: catalogFor([fabric, neoforge], {
      supportedFamilies: ["neoforge"],
    }),
    verifiedJres: new DeterministicFakeJreRegistry([fabric.jre, neoforge.jre]),
    sandboxRunner: new DeterministicFakeSandboxRunner(),
    artifactDownloader: downloader,
  });
  await assert.rejects(() => builder.build(buildInput(fabric)),
    /unsupported_compatibility/);
  assert.deepEqual(downloader.calls, []);
});

test("builder remains fail-closed without all verified production adapters", async () => {
  const fixture = toolchainFixture();
  const builder = new ReviewedRuntimeBuilder({ toolchainCatalog: catalogFor([fixture]) });
  await assert.rejects(() => builder.build(buildInput(fixture)), /compiler_unavailable/);
});

test("strict artifact downloader rejects wrong hosts, redirects, oversize payloads, and bad hashes", async () => {
  const fixture = toolchainFixture();
  const artifact = fixture.toolchain.artifacts[0];
  let calls = 0;
  const downloader = new StrictArtifactDownloader({
    fetchImpl: async (_url, options) => {
      calls += 1;
      assert.equal(options.headers["accept-encoding"], "identity");
      return new Response(fixture.artifactBytes, {
        status: 200,
        headers: { "content-length": String(fixture.artifactBytes.byteLength) },
      });
    },
  });
  await assert.rejects(
    () => downloader.download({ ...artifact, url: "https://unreviewed.example/installer.jar" }, {
      approvedHosts: ["toolchain.example"],
    }),
    /artifact_host_rejected/,
  );
  assert.equal(calls, 0);

  const redirected = new StrictArtifactDownloader({
    fetchImpl: async () => ({
      status: 200,
      redirected: true,
      headers: new Headers({ "content-length": String(artifact.sizeBytes) }),
    }),
  });
  await assert.rejects(
    () => redirected.download(artifact, { approvedHosts: ["toolchain.example"] }),
    /artifact_download_failed/,
  );

  const encoded = new StrictArtifactDownloader({
    fetchImpl: async () => new Response(fixture.artifactBytes, {
      status: 200,
      headers: {
        "content-encoding": "gzip",
        "content-length": String(artifact.sizeBytes),
      },
    }),
  });
  await assert.rejects(
    () => encoded.download(artifact, { approvedHosts: ["toolchain.example"] }),
    /artifact_download_failed/,
  );

  const oversized = new StrictArtifactDownloader({
    fetchImpl: async () => new Response(bytes("too-long"), {
      status: 200,
      headers: { "content-length": "8" },
    }),
  });
  await assert.rejects(
    () => oversized.download(artifact, { approvedHosts: ["toolchain.example"] }),
    /artifact_size_mismatch/,
  );

  const badHash = new StrictArtifactDownloader({
    fetchImpl: async () => new Response(bytes("x".repeat(artifact.sizeBytes)), {
      status: 200,
      headers: { "content-length": String(artifact.sizeBytes) },
    }),
  });
  await assert.rejects(
    () => badHash.download(artifact, { approvedHosts: ["toolchain.example"] }),
    /artifact_hash_mismatch/,
  );

  const timedOut = new StrictArtifactDownloader({
    fetchImpl: async () => new Promise(() => {}),
    timeoutMs: 1,
  });
  await assert.rejects(
    () => timedOut.download(artifact, { approvedHosts: ["toolchain.example"] }),
    /artifact_download_timeout/,
  );
});

test("installer failure and unsafe sandbox output produce no immutable archive", async () => {
  const fixture = toolchainFixture();
  await assert.rejects(
    () => builderFor(fixture, { fail: true }).build(buildInput(fixture)),
    /installer_failed/,
  );
  await assert.rejects(
    () => builderFor(fixture, {
      files: new Map([["run.sh", { bytes: bytes("#!/bin/sh"), mode: 0o755 }]]),
    }).build(buildInput(fixture)),
    /invalid_installer_output/,
  );
});

test("reviewed content is deterministic and excludes launcher-supplied shell, EULA, and Java files", async () => {
  const fixture = toolchainFixture();
  const first = await builderFor(fixture).build(buildInput(fixture));
  const second = await builderFor(fixture).build(buildInput(fixture));
  assert.deepEqual(first.archive, second.archive);
  assert.deepEqual(first.content, second.content);
  if (typeof zlib.zstdDecompressSync === "function") {
    assert.equal(
      new TextDecoder().decode(zlib.zstdDecompressSync(first.archive).subarray(257, 262)),
      "ustar",
    );
  }
  assert.equal(first.descriptor.launch.path, ".xmcl/launch.sh");
  assert.equal(first.descriptor.launch.arguments.length, 0);
  assert.ok(first.content.entries.every((entry) =>
    entry.path === ".xmcl/launch.sh" || !/\.(?:sh|bat|cmd|ps1|exe)$/i.test(entry.path),
  ));
  assert.ok(first.content.entries.every((entry) => !/(?:^|\/)(?:eula\.txt|java(?:w)?(?:\.exe)?)(?:$|\/)/i.test(entry.path)));
});
