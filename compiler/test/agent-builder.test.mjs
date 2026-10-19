import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import test from "node:test";
import { deflateRawSync } from "node:zlib";
import {
  buildKeyFor,
  canonicalizeBuildSpec,
  validateBuildSpecV1,
} from "../src/build-spec.mjs";
import {
  AgentBuilderV1,
  MountedArtifactDownloader,
  createAgentBuilderSandbox,
  prepareSourceBundle,
  writeBuilderOutputs,
} from "../src/agent-builder.mjs";
import {
  CompilerFailure,
  MAX_BUNDLE_ENTRIES,
  readStrictZip,
} from "../src/bundle.mjs";
import { OuterDockerSandboxAdapter } from "../src/outer-docker-sandbox.mjs";
import { quiesceContainerProcesses } from "../src/process-quiescer.mjs";
import {
  DeterministicFakeJreRegistry,
  DeterministicFakeSandboxRunner,
  ReviewedToolchainCatalog,
} from "../src/reviewed-builder.mjs";
import { builderPathsFromArgs } from "../scripts/agent_builder_v1.mjs";

const encoder = new TextEncoder();
const sha = (value) => createHash("sha256").update(value).digest("hex");
const imageA = `ghcr.io/voxelum/xmcl-shared-node-builder@sha256:${"1".repeat(64)}`;
const imageB = `ghcr.io/voxelum/xmcl-shared-minecraft-runtime@sha256:${"2".repeat(64)}`;
const toolchainRevision = "3".repeat(64);
const runtimeRevision = "4".repeat(64);
const canonicalVector = JSON.parse(await readFile(
  new URL("../../internal/builder/testdata/prepared-build-key-v1.fixture.json", import.meta.url),
  "utf8",
));
const neoforgeContract = JSON.parse(await readFile(
  new URL("./fixtures/neoforge-1.21.1-agent-builder-v1.json", import.meta.url),
  "utf8",
));
const lockedCatalog = JSON.parse(await readFile(
  new URL("../toolchain-catalog.lock.json", import.meta.url),
  "utf8",
));

function specFor(source, files = [], dependencies = [], format = "zip") {
  return {
    schemaVersion: 1,
    source: { format, sha256: sha(source) },
    compatibility: {
      minecraftVersion: "1.21.1",
      loader: { kind: "fabric", version: "0.16.10" },
      java: { component: "java-runtime-delta", major: 21 },
    },
    files,
    dependencies,
    builder: {
      schema: "agent_builder_v1",
      image: imageA,
      toolchainCatalogRevision: toolchainRevision,
    },
    runtime: {
      image: imageB,
      runtimeCatalogRevision: runtimeRevision,
    },
    target: { architecture: "amd64", os: "linux" },
  };
}

test("BuildSpecV1 canonical vector is stable and rejects URLs, unknowns, and array reordering", () => {
  const source = zip([]);
  const value = specFor(source);
  const canonical = canonicalizeBuildSpec(value);
  assert.equal(value.source.sha256, "8739c76e681f900923b900c9df0ef75cf421d39cabb54650c4b9ad19b6a76d85");
  assert.equal(canonicalVector.domain, "xmcl-prepared-content-build-key-v1\n");
  assert.equal(canonical, canonicalVector.canonicalJson);
  assert.equal(buildKeyFor(value), canonicalVector.buildKey);
  const unicode = specFor(source, [{
    path: "overrides/config/line\u2028separator.txt",
    targetPath: "config/line\u2028separator.txt",
    sha256: "a".repeat(64),
    sizeBytes: 1,
  }]);
  assert.match(canonicalizeBuildSpec(unicode), /line\\u2028separator/);

  assert.throws(() => validateBuildSpecV1({
    ...value,
    source: { ...value.source, url: "https://object.example/source" },
  }), /invalid_build_spec/);
  assert.throws(() => validateBuildSpecV1({
    ...value,
    source: { format: "xmcl_server_bundle", sha256: value.source.sha256 },
  }), /invalid_build_spec/);
  const fileA = {
    path: "overrides/config/a.txt", targetPath: "config/a.txt",
    sha256: "a".repeat(64), sizeBytes: 1,
  };
  const fileB = {
    path: "overrides/config/b.txt", targetPath: "config/b.txt",
    sha256: "b".repeat(64), sizeBytes: 1,
  };
  assert.throws(() => validateBuildSpecV1({
    ...value,
    files: [fileB, fileA],
  }), /invalid_build_spec/);

  const privateUse = {
    path: "overrides/config/\ue000.txt", targetPath: "config/\ue000.txt",
    sha256: "c".repeat(64), sizeBytes: 1,
  };
  const emoji = {
    path: "overrides/config/\ud83d\ude00.txt", targetPath: "config/\ud83d\ude00.txt",
    sha256: "d".repeat(64), sizeBytes: 1,
  };
  assert.doesNotThrow(() => validateBuildSpecV1({
    ...value,
    files: [privateUse, emoji],
  }));
  assert.throws(() => validateBuildSpecV1({
    ...value,
    files: [emoji, privateUse],
  }), /invalid_build_spec/);
});

test("CLI defaults to the fixed integration paths and permits only explicit test path overrides", () => {
  const defaults = builderPathsFromArgs([]);
  assert.equal(defaults.buildSpec, "/input/build-spec.json");
  assert.equal(defaults.source, "/input/source.pack");
  assert.equal(defaults.dependencyIndex, "/input/dependencies.json");
  assert.equal(defaults.dependencies, "/input/dependencies");
  assert.equal(defaults.output, "/output");
  assert.equal("bubblewrap" in defaults, false);
  const overridden = builderPathsFromArgs([
    "--build-spec", "/fixture/spec.json",
    "--source", "/fixture/source.pack",
    "--dependencies-index", "/fixture/dependencies.json",
    "--dependencies", "/fixture/dependencies",
    "--output", "/fixture/output",
  ]);
  assert.equal(overridden.output, "/fixture/output");
  assert.throws(() => builderPathsFromArgs(["--output", "relative"]), /invalid_builder_invocation/);
  assert.throws(() => builderPathsFromArgs(["--unknown", "/fixture"]), /invalid_builder_invocation/);
});

test("agent_builder_v1 outer Docker adapter invokes only prlimit and the reviewed JRE", async () => {
  const workspaceRoot = resolve(".agent-builder-outer-sandbox-test");
  const jreRoot = resolve(".agent-builder-outer-jre");
  const commands = [];
  const events = [];
  let output;
  const production = createAgentBuilderSandbox({
    jreRegistry: { rootForToken: () => jreRoot },
    workspaceRoot,
    prlimitPath: resolve("prlimit"),
    quiescer: async () => {},
  });
  assert.ok(production instanceof OuterDockerSandboxAdapter);
  assert.equal(production.quiescer, quiesceContainerProcesses);
  const adapter = new OuterDockerSandboxAdapter({
    jreRegistry: { rootForToken: () => jreRoot },
    workspaceRoot,
    prlimitPath: resolve("prlimit"),
    installerRewriter: (value) => value,
    runner: async (command, args, options) => {
      events.push("runner");
      commands.push({ command, args, options });
      output = args.at(-1);
      const unixArgs = resolve(output, "libraries", "net", "neoforged", "neoforge",
        "21.1.115", "unix_args.txt");
      await mkdir(resolve(unixArgs, ".."), { recursive: true });
      await writeFile(unixArgs, "-jar libraries/neoforge-server.jar\n");
    },
    quiescer: async () => {
      events.push("quiescer");
      await writeFile(resolve(output, "quiesced.marker"), "quiesced\n");
    },
  });
  assert.deepEqual(adapter.capabilities, {
    ephemeralWorkspace: true,
    nonRoot: true,
    readOnlyBaseFilesystem: true,
    noSecrets: true,
    noDockerSocket: true,
    boundedResources: true,
    egress: "approved-artifacts-only",
  });
  const artifacts = [
    {
      coordinate: "net.neoforged:neoforge:21.1.115:installer",
      bytes: encoder.encode("exact-installer"),
    },
    {
      coordinate: "com.mojang:minecraft-server:1.21.1:server",
      bytes: encoder.encode("exact-server"),
    },
    {
      coordinate: "net.neoforged:neoforge:21.1.115:universal",
      url: "https://maven.neoforged.net/releases/net/neoforged/neoforge/21.1.115/neoforge-21.1.115-universal.jar",
      bytes: encoder.encode("exact-dependency"),
    },
  ];
  try {
    const result = await adapter.assemble({
      schemaVersion: 1,
      sandbox: {
        ephemeralWorkspace: true,
        nonRoot: true,
        readOnlyBaseFilesystem: true,
        noSecrets: true,
        noDockerSocket: true,
        boundedResources: true,
      },
      jre: { rootToken: "jre-token" },
      plan: {
        family: "neoforge",
        minecraftVersion: "1.21.1",
        loader: { version: "21.1.115" },
      },
      artifacts,
    });
    assert.equal(commands.length, 1);
    const [{ command, args, options }] = commands;
    assert.equal(command, resolve("prlimit"));
    assert.equal(args.includes("/usr/bin/bwrap"), false);
    assert.equal(args.includes("--unshare-all"), false);
    assert.equal(args[args.indexOf("--") + 1], resolve(jreRoot, "bin", "java"));
    const prlimitArgs = [
      "--as=3221225472", "--fsize=268435456", "--nproc=256", "--cpu=240",
    ];
    const jvmResourceArgs = [
      "-Xms128m", "-Xmx1024m", "-Xss512k",
      "-XX:MaxMetaspaceSize=512m",
      "-XX:CompressedClassSpaceSize=256m",
      "-XX:ReservedCodeCacheSize=128m",
      "-XX:MaxDirectMemorySize=256m",
      "-XX:ActiveProcessorCount=2",
    ];
    assert.deepEqual(args.slice(0, args.indexOf("--")), prlimitArgs);
    assert.deepEqual(args.filter((arg) => arg.startsWith("-X")), jvmResourceArgs);
    // The live boundary probe must exercise the same limits as this actual invocation.
    const liveSource = await readFile(new URL(
      "../../internal/docker/builder_live_test.go", import.meta.url,
    ), "utf8");
    const probe = liveSource.match(/const builderOuterSandboxLiveProbe = `([^`]+)`/)[1];
    const invocation = probe.replace(/\\\r?\n/g, " ")
      .split("\n").find((line) => line.startsWith("prlimit "));
    assert.ok(invocation, "live probe must invoke prlimit directly");
    const probeArgs = invocation.trim().split(/\s+/).slice(1);
    assert.deepEqual(probeArgs, [
      ...prlimitArgs, "--", "/opt/xmcl/jres/java-runtime-delta-21/bin/java",
      ...jvmResourceArgs, "-version",
    ]);
    assert.ok(args.includes("--offline"));
    assert.equal(options.env && Object.keys(options.env).length, 0);
    assert.equal(options.cwd, args.at(-1));
    assert.ok(result.files.has(
      "libraries/net/neoforged/neoforge/21.1.115/unix_args.txt",
    ));
    assert.ok(result.files.has("quiesced.marker"));
    assert.deepEqual(events, ["runner", "quiescer"]);
    assert.deepEqual(result.attestation, {
      ephemeralWorkspace: true,
      nonRoot: true,
      readOnlyBaseFilesystem: true,
      noSecrets: true,
      noDockerSocket: true,
      boundedResources: true,
      network: "disabled",
    });
  } finally {
    await rm(workspaceRoot, { recursive: true, force: true });
  }
});

test("agent_builder_v1 quiesces detached processes after runner failure", async () => {
  const workspaceRoot = resolve(".agent-builder-failed-quiesce-test");
  const events = [];
  const adapter = new OuterDockerSandboxAdapter({
    jreRegistry: { rootForToken: () => resolve(".agent-builder-outer-jre") },
    workspaceRoot,
    prlimitPath: resolve("prlimit"),
    installerRewriter: (value) => value,
    runner: async () => {
      events.push("runner");
      throw new CompilerFailure("installer_process_failed");
    },
    quiescer: async () => {
      events.push("quiescer");
    },
  });
  const artifacts = [
    {
      coordinate: "net.neoforged:neoforge:21.1.115:installer",
      bytes: encoder.encode("exact-installer"),
    },
    {
      coordinate: "com.mojang:minecraft-server:1.21.1:server",
      bytes: encoder.encode("exact-server"),
    },
    {
      coordinate: "net.neoforged:neoforge:21.1.115:universal",
      url: "https://maven.neoforged.net/releases/net/neoforged/neoforge/21.1.115/neoforge-21.1.115-universal.jar",
      bytes: encoder.encode("exact-dependency"),
    },
  ];
  try {
    await assert.rejects(() => adapter.assemble({
      schemaVersion: 1,
      sandbox: {
        ephemeralWorkspace: true,
        nonRoot: true,
        readOnlyBaseFilesystem: true,
        noSecrets: true,
        noDockerSocket: true,
        boundedResources: true,
      },
      jre: { rootToken: "jre-token" },
      plan: {
        family: "neoforge",
        minecraftVersion: "1.21.1",
        loader: { version: "21.1.115" },
      },
      artifacts,
    }), /installer_process_failed/);
    assert.deepEqual(events, ["runner", "quiescer"]);
    assert.deepEqual(await readdir(workspaceRoot), []);
  } finally {
    await rm(workspaceRoot, { recursive: true, force: true });
  }
});

test("static NeoForge 1.21.1 agent_builder_v1 contract matches the reviewed lock", () => {
  const { buildSpec, expected } = neoforgeContract;
  const source = zip([]);
  assert.equal(sha(source), buildSpec.source.sha256);

  const catalog = new ReviewedToolchainCatalog(lockedCatalog, {
    supportedFamilies: ["neoforge"],
  });
  assert.deepEqual(buildSpec.compatibility, expected.compatibility);
  assert.equal(catalog.catalogRevision, expected.catalogRevision);
  assert.equal(catalog.runtimeCatalogRevision, expected.runtimeCatalogRevision);
  const toolchain = catalog.resolve({
    runtimeCatalogRevision: buildSpec.runtime.runtimeCatalogRevision,
    ...buildSpec.compatibility,
  });
  const lockedIdentities = toolchain.artifacts
    .map(({ coordinate, role, sha256, sizeBytes }) => ({
      coordinate,
      role,
      sha256,
      sizeBytes,
    }))
    .sort((left, right) => compareUtf8(left.coordinate, right.coordinate));
  assert.equal(expected.requiredArtifacts.length, 73);
  assert.equal(expected.requiredArtifacts.every((artifact) =>
    Object.keys(artifact).sort().join(",") === "coordinate,role,sha256,sizeBytes"), true);
  assert.deepEqual(lockedIdentities, expected.requiredArtifacts);
  assert.deepEqual(toolchain.jre, expected.jre);
  assert.equal(toolchain.launchTemplate, expected.launchTemplate);

  const dependencies = expected.requiredArtifacts
    .map((artifact) => ({
      id: artifact.coordinate,
      kind: dependencyKindFor(artifact),
      sha256: artifact.sha256,
      sizeBytes: artifact.sizeBytes,
      targetPath: `toolchain/${artifact.sha256}.artifact`,
    }))
    .sort((left, right) => compareUtf8(
      `${left.kind}\0${left.id}\0${left.targetPath}`,
      `${right.kind}\0${right.id}\0${right.targetPath}`,
    ));
  const spec = validateBuildSpecV1({ ...buildSpec, dependencies });
  assert.deepEqual(spec.dependencies, dependencies);
  assert.equal(prepareSourceBundle(spec, source).entries.size, 0);
  assert.throws(() => validateBuildSpecV1({
    ...buildSpec,
    dependencies: [{
      ...dependencies[0],
      targetPath: `world/${dependencies[0].sha256}.artifact`,
    }],
  }), /invalid_build_spec/);
  assert.equal(catalog.toolchains.every(({ loader }) => loader.kind === "neoforge"), true);
});

test("zip and mrpack inputs are strict and bind all local and dependency identities", () => {
  const config = encoder.encode("online-mode=true\n");
  const plain = zip([{ path: "server.properties", bytes: config }]);
  const plainSpec = specFor(plain, [{
    path: "server.properties",
    targetPath: "server.properties",
    sha256: sha(config),
    sizeBytes: config.byteLength,
  }]);
  assert.equal(prepareSourceBundle(plainSpec, plain).entries.size, 1);
  const corrupted = plain.slice();
  corrupted[30] ^= 1;
  assert.throws(() => prepareSourceBundle({
    ...plainSpec,
    source: { ...plainSpec.source, sha256: sha(corrupted) },
  }, corrupted), /invalid_bundle/);

  const dependencyBytes = encoder.encode("mounted mod");
  const dependency = {
    id: "modrinth:project:version",
    kind: "mod",
    targetPath: "mods/example.jar",
    sha256: sha(dependencyBytes),
    sizeBytes: dependencyBytes.byteLength,
  };
  const index = encoder.encode(JSON.stringify({
    formatVersion: 1,
    game: "minecraft",
    versionId: "pack-1",
    name: "Pack",
    files: [{
      path: dependency.targetPath,
      hashes: { sha256: dependency.sha256 },
      downloads: ["https://cdn.modrinth.com/ignored-by-builder"],
      fileSize: dependency.sizeBytes,
      env: { client: "required", server: "required" },
    }],
    dependencies: {
      minecraft: "1.21.1",
      "fabric-loader": "0.16.10",
    },
  }));
  const mrpack = zip([
    { path: "modrinth.index.json", bytes: index },
    { path: "overrides/server.properties", bytes: config },
  ]);
  const mrpackSpec = specFor(mrpack, [{
    ...plainSpec.files[0],
    path: "overrides/server.properties",
  }], [dependency], "mrpack");
  assert.equal(prepareSourceBundle(mrpackSpec, mrpack).entries.size, 1);
  const wrongIdentity = structuredClone(mrpackSpec);
  wrongIdentity.dependencies[0].sizeBytes += 1;
  assert.throws(() => prepareSourceBundle(wrongIdentity, mrpack),
    /invalid_mrpack|mrpack_dependency_mismatch/);
});

test("normal ZIP descriptors and explicit directories are accepted", () => {
  const config = encoder.encode("online-mode=true\n");
  const index = encoder.encode(JSON.stringify({
    formatVersion: 1,
    game: "minecraft",
    versionId: "pack-1",
    name: "Pack",
    files: [],
    dependencies: {
      minecraft: "1.21.1",
      "fabric-loader": "0.16.10",
    },
  }));
  const source = zip([
    {
      path: "overrides/",
      directory: true,
      descriptor: true,
      deflate: true,
    },
    { path: "modrinth.index.json", bytes: index, descriptor: true, deflate: true },
    {
      path: "overrides/server.properties",
      bytes: config,
      descriptor: true,
      descriptorSignature: false,
      deflate: true,
    },
  ]);
  const spec = specFor(source, [{
    path: "overrides/server.properties",
    targetPath: "server.properties",
    sha256: sha(config),
    sizeBytes: config.byteLength,
  }], [], "mrpack");
  const prepared = prepareSourceBundle(spec, source);
  assert.equal(prepared.entries.size, 1);
  assert.deepEqual(
    [...prepared.entries.get("instance/server.properties").bytes],
    [...config],
  );
});

test("strict ZIP parsing rejects unsafe paths, attributes, collisions, bombs, and malformed headers", () => {
  const one = encoder.encode("one");
  const cases = [
    zip([{ path: "../escape.txt", bytes: one }]),
    zip([{
      path: "link",
      bytes: one,
      externalAttributes: 0xa1ff0000,
      versionMadeBy: 0x0314,
    }]),
    zip([
      { path: "Config.txt", bytes: one },
      { path: "config.txt", bytes: one },
    ]),
    zip([
      { path: "same.txt", bytes: one },
      { path: "same.txt", bytes: one },
    ]),
    zip([
      { path: "config/\u212a.txt", bytes: one },
      { path: "config/k.txt", bytes: one },
    ]),
    zip([
      { path: "config/", directory: true },
      { path: "CONFIG", bytes: one },
    ]),
    zip([{ path: "file.txt", bytes: one, method: 12 }]),
    zip([{ path: "bomb.txt", bytes: new Uint8Array(10_000), deflate: true }]),
  ];
  for (const archive of cases) {
    assert.throws(() => readStrictZip(archive), /invalid_bundle/);
  }

  const malformedLocal = zip([{ path: "file.txt", bytes: one }]);
  malformedLocal[8] = 8;
  assert.throws(() => readStrictZip(malformedLocal), /invalid_bundle/);

  const malformedDescriptor = zip([{
    path: "file.txt",
    bytes: one,
    descriptor: true,
  }]);
  malformedDescriptor[30 + "file.txt".length + one.length + 4] ^= 1;
  assert.throws(() => readStrictZip(malformedDescriptor), /invalid_bundle/);

  const tooMany = zip(Array.from({ length: MAX_BUNDLE_ENTRIES + 1 }, (_, index) => ({
    path: `directory-${index}/`,
    directory: true,
  })));
  assert.throws(() => readStrictZip(tooMany), /invalid_bundle/);
});

test("mounted dependency reader enforces identity, digest, size, and regular files", async () => {
  const root = await mkdtemp(resolve(".agent-builder-mount-test-"));
  try {
    const bytes = encoder.encode("exact dependency");
    const digest = sha(bytes);
    const dependency = {
      id: "modrinth:project:version",
      kind: "mod",
      targetPath: "mods/example.jar",
      sha256: digest,
      sizeBytes: bytes.byteLength,
    };
    const name = `000-${digest}`;
    await writeFile(resolve(root, name), bytes);
    const reader = new MountedArtifactDownloader({
      root,
      dependencies: [dependency],
      mounts: [{ id: dependency.id, path: name, sha256: digest, sizeBytes: bytes.byteLength }],
    });
    await reader.verifyDependencies();
    assert.deepEqual(await reader.readDependency(dependency), bytes);
    await assert.rejects(() => reader.readDependency({
      ...dependency,
      id: "modrinth:project:other-version",
    }), /mounted_dependency_identity_mismatch/);
    await writeFile(resolve(root, name), encoder.encode("tampered"));
    await assert.rejects(() => reader.verifyDependencies(), /mounted_dependency_mismatch/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("agent_builder_v1 produces deterministic reviewed content without a network adapter", async () => {
  const root = await mkdtemp(resolve(".agent-builder-output-test-"));
  try {
    const sourceFile = encoder.encode("motd=hello\n");
    const source = zip([{ path: "server.properties", bytes: sourceFile }]);
    const artifactBytes = encoder.encode("reviewed loader");
    const artifactDigest = sha(artifactBytes);
    const artifactName = `000-${artifactDigest}`;
    await writeFile(resolve(root, artifactName), artifactBytes);
    const jreBytes = encoder.encode("reviewed jre");
    const jre = {
      id: "jre-21",
      sha256: sha(jreBytes),
      component: "java-runtime-delta",
      major: 21,
      runtimeCatalogRevision: runtimeRevision,
    };
    const catalog = new ReviewedToolchainCatalog({
      schemaVersion: 1,
      catalogVersion: "test-1",
      runtimeCatalogRevision: runtimeRevision,
      approvedArtifactHosts: ["toolchain.example"],
      toolchains: [{
        minecraftVersion: "1.21.1",
        loader: { kind: "fabric", version: "0.16.10" },
        java: { component: "java-runtime-delta", major: 21 },
        jre,
        artifacts: [{
          role: "primary",
          coordinate: "net.fabricmc:fabric-loader:0.16.10",
          url: "https://toolchain.example/loader.jar",
          sha256: artifactDigest,
          sizeBytes: artifactBytes.byteLength,
        }],
        launchTemplate: "fabric-server-jar-v1",
      }],
    });
    catalog.catalogRevision = toolchainRevision;
    const toolchainDependency = {
      id: "net.fabricmc:fabric-loader:0.16.10",
      kind: "toolchain",
      targetPath: "toolchain/fabric-loader.jar",
      sha256: artifactDigest,
      sizeBytes: artifactBytes.byteLength,
    };
    const dependencies = new MountedArtifactDownloader({
      root,
      dependencies: [toolchainDependency],
      mounts: [{
        id: toolchainDependency.id,
        path: artifactName,
        sha256: artifactDigest,
        sizeBytes: artifactBytes.byteLength,
      }],
    });
    const builder = new AgentBuilderV1({
      catalog,
      verifiedJres: new DeterministicFakeJreRegistry([jre]),
      sandboxRunner: new DeterministicFakeSandboxRunner(),
      dependencies,
    });
    const spec = specFor(source, [{
      path: "server.properties",
      targetPath: "server.properties",
      sha256: sha(sourceFile),
      sizeBytes: sourceFile.byteLength,
    }], [toolchainDependency]);
    const first = await builder.build({ buildSpec: spec, source });
    const second = await builder.build({ buildSpec: spec, source });
    assert.deepEqual(first, second);
    assert.equal(first.result.schemaVersion, 1);
    assert.equal(first.result.buildKey, buildKeyFor(spec));
    assert.equal(first.result.content.sha256, sha(first.archive));
    assert.deepEqual(Object.keys(first.result).sort(), ["buildKey", "content", "schemaVersion"]);
    assert.deepEqual(Object.keys(first.result.content).sort(),
      ["compressedSize", "logicalSize", "paths", "sha256"]);
    const output = resolve(root, "output");
    await mkdir(output);
    await writeBuilderOutputs(output, first);
    assert.deepEqual((await readdir(output)).sort(), ["content.tar.zst", "result.json"]);
    assert.deepEqual(new Uint8Array(await readFile(resolve(output, "content.tar.zst"))),
      first.archive);
    assert.deepEqual(JSON.parse(await readFile(resolve(output, "result.json"), "utf8")),
      first.result);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

function zip(entries) {
  const out = [];
  const central = [];
  for (const entry of entries) {
    const path = encoder.encode(entry.path);
    const bytes = entry.bytes ?? new Uint8Array();
    const compressed = entry.deflate ? deflateRawSync(bytes) : bytes;
    const method = entry.method ?? (entry.deflate ? 8 : 0);
    const flags = 0x800 | (entry.descriptor ? 0x8 : 0);
    const crc = crc32(bytes);
    const offset = out.length;
    u32(out, 0x04034b50); u16(out, 20); u16(out, flags); u16(out, method);
    u16(out, 0); u16(out, 0); u32(out, entry.descriptor ? 0 : crc);
    u32(out, entry.descriptor ? 0 : compressed.length);
    u32(out, entry.descriptor ? 0 : bytes.length); u16(out, path.length); u16(out, 0);
    out.push(...path, ...compressed);
    if (entry.descriptor) {
      if (entry.descriptorSignature !== false) u32(out, 0x08074b50);
      u32(out, crc); u32(out, compressed.length);
      u32(out, bytes.length);
    }
    u32(central, 0x02014b50); u16(central, entry.versionMadeBy ?? 20);
    u16(central, 20); u16(central, flags);
    u16(central, method); u16(central, 0); u16(central, 0); u32(central, crc);
    u32(central, compressed.length); u32(central, bytes.length); u16(central, path.length);
    u16(central, 0); u16(central, 0); u16(central, 0); u16(central, 0);
    u32(central, entry.externalAttributes ??
      (entry.directory ? 0x00000010 : 0));
    u32(central, offset); central.push(...path);
  }
  const centralOffset = out.length;
  for (const byte of central) out.push(byte);
  u32(out, 0x06054b50); u16(out, 0); u16(out, 0); u16(out, entries.length);
  u16(out, entries.length); u32(out, central.length); u32(out, centralOffset); u16(out, 0);
  return Uint8Array.from(out);
}

function u16(out, value) {
  out.push(value & 255, (value >>> 8) & 255);
}

function u32(out, value) {
  u16(out, value & 0xffff);
  u16(out, value >>> 16);
}

function crc32(bytes) {
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc & 1) ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
    }
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function dependencyKindFor(artifact) {
  if (artifact.role === "primary") return "toolchain";
  if (artifact.coordinate.startsWith("com.mojang:minecraft-server:")) return "minecraft";
  if (artifact.coordinate.startsWith("net.minecraft:server:") &&
    artifact.coordinate.endsWith(":mappings")) return "mapping";
  return "library";
}

function compareUtf8(left, right) {
  const leftBytes = encoder.encode(left);
  const rightBytes = encoder.encode(right);
  const length = Math.min(leftBytes.byteLength, rightBytes.byteLength);
  for (let index = 0; index < length; index += 1) {
    if (leftBytes[index] !== rightBytes[index]) {
      return leftBytes[index] < rightBytes[index] ? -1 : 1;
    }
  }
  return leftBytes.byteLength < rightBytes.byteLength ? -1 :
    leftBytes.byteLength > rightBytes.byteLength ? 1 : 0;
}
