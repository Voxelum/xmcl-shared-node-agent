import { createHash } from "node:crypto";
import { CompilerFailure } from "./bundle.mjs";
import { catalogRevisionFor } from "./toolchain-catalog.mjs";
import {
  PRODUCTION_OUTPUT_LIMITS,
  outputSizeWithinLimit,
} from "./production-limits.mjs";

const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const MAX_ARTIFACT_BYTES = 512 * 1024 * 1024;
const MAX_OUTPUT_FILES = PRODUCTION_OUTPUT_LIMITS.maxFiles;
const RAW_ZSTD_BLOCK_BYTES = 128 * 1024;

const loaderTemplates = Object.freeze({
  forge: "forge-unix-args-v1",
  fabric: "fabric-server-jar-v1",
  neoforge: "neoforge-unix-args-v1",
  quilt: "quilt-server-jar-v1",
});

const requiredSandboxCapabilities = Object.freeze({
  ephemeralWorkspace: true,
  nonRoot: true,
  readOnlyBaseFilesystem: true,
  noSecrets: true,
  noDockerSocket: true,
  boundedResources: true,
  egress: "approved-artifacts-only",
});

/**
 * A reviewed, versioned catalog. It is deliberately separate from every
 * launcher-provided bundle and contains only exact artifact identities.
 */
export class ReviewedToolchainCatalog {
  constructor(document, { supportedFamilies } = {}) {
    const catalog = parseCatalog(document);
    if (supportedFamilies !== undefined &&
      (!Array.isArray(supportedFamilies) || supportedFamilies.length < 1 ||
        new Set(supportedFamilies).size !== supportedFamilies.length ||
        supportedFamilies.some((family) => !Object.hasOwn(loaderTemplates, family)))) {
      throw new CompilerFailure("invalid_reviewed_catalog");
    }
    this.catalogVersion = catalog.catalogVersion;
    this.catalogRevision = catalog.catalogRevision;
    this.runtimeCatalogRevision = catalog.runtimeCatalogRevision;
    this.approvedArtifactHosts = catalog.approvedArtifactHosts;
    this.toolchains = supportedFamilies === undefined
      ? catalog.toolchains
      : catalog.toolchains.filter((toolchain) => supportedFamilies.includes(toolchain.loader.kind));
    if (this.toolchains.length < 1) throw new CompilerFailure("invalid_reviewed_catalog");
  }

  resolve(input) {
    if (!plainObject(input) ||
      input.runtimeCatalogRevision !== this.runtimeCatalogRevision ||
      !validMinecraftVersion(input.minecraftVersion) ||
      !plainObject(input.loader) || !plainObject(input.java)) {
      throw new CompilerFailure("unsupported_compatibility");
    }
    const toolchain = this.toolchains.find((candidate) =>
      candidate.minecraftVersion === input.minecraftVersion &&
      candidate.loader.kind === input.loader.kind &&
      candidate.loader.version === input.loader.version &&
      candidate.java.component === input.java.component &&
      candidate.java.major === input.java.major,
    );
    if (!toolchain) throw new CompilerFailure("unsupported_compatibility");
    return toolchain;
  }
}

/**
 * Resolves only an exact reviewed artifact. This API intentionally accepts an
 * artifact object rather than an arbitrary URL.
 */
export class StrictArtifactDownloader {
  constructor({ fetchImpl, timeoutMs = 30_000, maxArtifactBytes = MAX_ARTIFACT_BYTES } = {}) {
    if (typeof fetchImpl !== "function" ||
      !Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 300_000 ||
      !Number.isSafeInteger(maxArtifactBytes) || maxArtifactBytes < 1 ||
      maxArtifactBytes > MAX_ARTIFACT_BYTES) {
      throw new TypeError("invalid strict artifact downloader configuration");
    }
    this.fetchImpl = fetchImpl;
    this.timeoutMs = timeoutMs;
    this.maxArtifactBytes = maxArtifactBytes;
  }

  async download(artifact, { approvedHosts } = {}) {
    validateApprovedArtifact(artifact, approvedHosts, this.maxArtifactBytes);
    const controller = new AbortController();
    let timeout;
    try {
      const download = async () => {
        const response = await this.fetchImpl(artifact.url, {
          method: "GET",
          headers: { "accept-encoding": "identity" },
          redirect: "error",
          credentials: "omit",
          referrerPolicy: "no-referrer",
          signal: controller.signal,
        });
        if (!response || response.status !== 200 || response.redirected) {
          throw new CompilerFailure("artifact_download_failed");
        }
        if (response.url && new URL(response.url).href !== new URL(artifact.url).href) {
          throw new CompilerFailure("artifact_redirect_rejected");
        }
        const contentEncoding = response.headers?.get("content-encoding");
        if (contentEncoding && contentEncoding.toLowerCase() !== "identity") {
          throw new CompilerFailure("artifact_download_failed");
        }
        const contentLength = response.headers?.get("content-length");
        if (!/^(?:0|[1-9]\d*)$/.test(contentLength ?? "")) {
          throw new CompilerFailure("artifact_size_mismatch");
        }
        const expectedSize = Number(contentLength);
        if (!Number.isSafeInteger(expectedSize) || expectedSize !== artifact.sizeBytes ||
          expectedSize > this.maxArtifactBytes) {
          throw new CompilerFailure("artifact_size_mismatch");
        }
        const bytes = await readExactResponse(response, expectedSize, this.maxArtifactBytes);
        if (sha256(bytes) !== artifact.sha256) {
          throw new CompilerFailure("artifact_hash_mismatch");
        }
        return bytes;
      };
      return await Promise.race([
        download(),
        new Promise((_, reject) => {
          timeout = setTimeout(() => {
            controller.abort();
            reject(new CompilerFailure("artifact_download_timeout"));
          }, this.timeoutMs);
        }),
      ]);
    } catch (error) {
      const failure = error instanceof CompilerFailure
        ? error
        : new CompilerFailure("artifact_download_failed");
      failure.artifactCoordinate = artifact.coordinate;
      throw failure;
    } finally {
      clearTimeout(timeout);
    }
  }
}

/**
 * Production composition is intentionally not supplied here. Every dependency
 * below must be injected by deployment code after it has been independently
 * reviewed and installed.
 */
export class ReviewedRuntimeBuilder {
  constructor({ toolchainCatalog, verifiedJres, sandboxRunner, artifactDownloader } = {}) {
    try {
      this.toolchainCatalog = toolchainCatalog instanceof ReviewedToolchainCatalog
        ? toolchainCatalog
        : new ReviewedToolchainCatalog(toolchainCatalog);
    } catch {
      this.toolchainCatalog = undefined;
    }
    this.verifiedJres = verifiedJres;
    this.sandboxRunner = sandboxRunner;
    this.artifactDownloader = artifactDownloader;
  }

  async build({ bundle, frozenManifest, expectedContentKey } = {}) {
    if (!this.toolchainCatalog || !hasReviewedDependencies(this)) {
      throw new CompilerFailure("compiler_unavailable");
    }
    const manifest = bundle?.manifest;
    if (!plainObject(manifest) || !(bundle?.entries instanceof Map) ||
      !validContentKey(expectedContentKey)) {
      throw new CompilerFailure("invalid_builder_input");
    }

    const toolchain = this.toolchainCatalog.resolve({
      runtimeCatalogRevision: manifest.runtimeCatalog?.sha256,
      minecraftVersion: manifest.minecraftVersion,
      loader: manifest.loader,
      java: manifest.javaRequirement,
    });
    if (frozenManifest?.compatibility?.runtimeCatalog?.sha256 !==
      this.toolchainCatalog.runtimeCatalogRevision) {
      throw new CompilerFailure("unsupported_compatibility");
    }

    const jre = await resolveVerifiedJre(this.verifiedJres, toolchain.jre);
    const plan = createAssemblyPlan(toolchain);
    const artifacts = [];
    for (const artifact of toolchain.artifacts) {
      const bytes = await this.artifactDownloader.download(artifact, {
        approvedHosts: this.toolchainCatalog.approvedArtifactHosts,
      });
      if (!(bytes instanceof Uint8Array) || bytes.byteLength !== artifact.sizeBytes ||
        sha256(bytes) !== artifact.sha256) {
        throw new CompilerFailure("artifact_hash_mismatch");
      }
      artifacts.push({
        coordinate: artifact.coordinate,
        url: artifact.url,
        bytes,
      });
    }

    let installed;
    try {
      installed = await this.sandboxRunner.assemble({
        schemaVersion: 1,
        sandbox: requiredSandboxCapabilities,
        jre,
        plan,
        artifacts,
      });
    } catch (error) {
      if (error instanceof CompilerFailure && error.code === "installer_failed") throw error;
      throw new CompilerFailure("installer_failed");
    }
    const files = normalizeSandboxOutput(installed);
    copyValidatedBundleContent(files, bundle);

    const runtime = runtimeDescriptor(toolchain, plan);
    addGeneratedFile(files, ".xmcl/runtime.json", encodeJson(runtime), 0o644);
    addGeneratedFile(files, ".xmcl/launch.sh", generatedLauncher(plan.launch.arguments), 0o755);

    const entries = validateOutputFiles(files);
    const archive = packageDeterministicTarZst(files);
    const content = {
      key: expectedContentKey,
      format: "tar.zst",
      sha256: sha256(archive),
      sizeBytes: archive.byteLength,
      entries,
    };
    const descriptor = {
      schemaVersion: 1,
      runtime: {
        path: ".xmcl/runtime.json",
        sha256: entryHash(entries, ".xmcl/runtime.json"),
      },
      launch: {
        path: ".xmcl/launch.sh",
        kind: "generated-server-launcher",
        arguments: [],
      },
    };
    validateImmutableContent({ archive, content, descriptor });
    return { archive, content, descriptor };
  }
}

/**
 * Creates deterministic, hermetic test doubles. They are deliberately named
 * fakes and are not selected by CompilerWorker or any production constructor.
 */
export class DeterministicFakeArtifactDownloader {
  constructor(artifacts = new Map()) {
    this.artifacts = artifacts instanceof Map ? artifacts : new Map(Object.entries(artifacts));
    this.calls = [];
  }

  async download(artifact, { approvedHosts } = {}) {
    validateApprovedArtifact(artifact, approvedHosts, MAX_ARTIFACT_BYTES);
    this.calls.push(artifact.coordinate);
    const bytes = this.artifacts.get(artifact.coordinate);
    if (!(bytes instanceof Uint8Array)) throw new CompilerFailure("artifact_download_failed");
    return bytes.slice();
  }
}

export class DeterministicFakeJreRegistry {
  constructor(roots = []) {
    this.roots = new Map(roots.map((root) => [root.id, { ...root }]));
    this.calls = [];
  }

  async resolve(request) {
    this.calls.push(request.id);
    const root = this.roots.get(request.id);
    if (!root) throw new CompilerFailure("compiler_unavailable");
    return {
      ...root,
      verified: true,
      readOnly: true,
      rootToken: root.rootToken ?? `fake-jre-${root.id}`,
    };
  }
}

export class DeterministicFakeSandboxRunner {
  constructor({ files, fail = false } = {}) {
    this.files = files;
    this.fail = fail;
    this.calls = [];
    this.capabilities = { ...requiredSandboxCapabilities };
  }

  async assemble(request) {
    this.calls.push(request);
    if (this.fail) throw new CompilerFailure("installer_failed");
    const files = this.files ? cloneFiles(this.files) : fakeServerFiles(request.plan);
    return {
      files,
      attestation: {
        ephemeralWorkspace: true,
        nonRoot: true,
        readOnlyBaseFilesystem: true,
        noSecrets: true,
        noDockerSocket: true,
        boundedResources: true,
        network: "disabled",
      },
    };
  }
}

export function createAssemblyPlan(toolchain) {
  if (!toolchain || !loaderTemplates[toolchain.loader?.kind]) {
    throw new CompilerFailure("unsupported_compatibility");
  }
  const { kind, version } = toolchain.loader;
  const minecraftVersion = toolchain.minecraftVersion;
  let launchArguments;
  if (kind === "forge") {
    launchArguments = [
      `@libraries/net/minecraftforge/forge/${minecraftVersion}-${version}/unix_args.txt`,
      "nogui",
    ];
  } else if (kind === "neoforge") {
    launchArguments = [
      `@libraries/net/neoforged/neoforge/${version}/unix_args.txt`,
      "nogui",
    ];
  } else {
    launchArguments = ["-jar", "server.jar", "nogui"];
  }
  return deepFreeze({
    schemaVersion: 1,
    family: kind,
    minecraftVersion,
    loader: { kind, version },
    java: { ...toolchain.java },
    jre: {
      id: toolchain.jre.id,
      sha256: toolchain.jre.sha256,
      component: toolchain.jre.component,
      major: toolchain.jre.major,
      runtimeCatalogRevision: toolchain.jre.runtimeCatalogRevision,
    },
    artifacts: toolchain.artifacts.map((artifact) => ({
      coordinate: artifact.coordinate,
      sha256: artifact.sha256,
      sizeBytes: artifact.sizeBytes,
    })),
    launch: {
      template: loaderTemplates[kind],
      arguments: launchArguments,
    },
  });
}

/**
 * Verifies the complete content description, its paths, hashes, and the
 * deterministic raw-Zstandard tar payload before the worker uploads anything.
 */
export function validateImmutableContent({ archive, content, descriptor }) {
  if (!(archive instanceof Uint8Array) || !plainObject(content) ||
    content.format !== "tar.zst" || !validContentKey(content.key) ||
    !validSha256(content.sha256) || content.sha256 !== sha256(archive) ||
    !Number.isSafeInteger(content.sizeBytes) || content.sizeBytes !== archive.byteLength ||
    !Array.isArray(content.entries) || !validGeneratedDescriptor(descriptor)) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const expected = validateEntryMetadata(content.entries);
  const unpacked = unpackDeterministicTarZst(archive);
  if (unpacked.size !== expected.length) throw new CompilerFailure("invalid_builder_output");
  for (const entry of expected) {
    const actual = unpacked.get(entry.path);
    if (!actual || actual.sizeBytes !== entry.sizeBytes || actual.sha256 !== entry.sha256 ||
      actual.mode !== entry.mode) {
      throw new CompilerFailure("invalid_builder_output");
    }
  }
  if (!unpacked.has(".xmcl/runtime.json") || !unpacked.has(".xmcl/launch.sh")) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const runtime = unpacked.get(".xmcl/runtime.json");
  const launcher = unpacked.get(".xmcl/launch.sh");
  if (descriptor.runtime.sha256 !== runtime.sha256) {
    throw new CompilerFailure("invalid_builder_output");
  }
  validateGeneratedRuntimeAndLauncher(unpacked, runtime, launcher);
}

function parseCatalog(value) {
  const legacy = plainObject(value) && sameKeys(value, ["schemaVersion", "catalogVersion",
    "runtimeCatalogRevision", "approvedArtifactHosts", "toolchains"]);
  const generated = plainObject(value) && sameKeys(value, ["schemaVersion", "catalogRevision",
    "runtimeCatalogRevision", "approvedArtifactHosts", "toolchains"]);
  if (!plainObject(value) || (!legacy && !generated) || value.schemaVersion !== 1 ||
    (legacy && !validIdentifier(value.catalogVersion)) ||
    (generated && (!validSha256(value.catalogRevision) ||
      catalogRevisionFor(value) !== value.catalogRevision.toLowerCase())) ||
    !validSha256(value.runtimeCatalogRevision) ||
    !Array.isArray(value.approvedArtifactHosts) || value.approvedArtifactHosts.length < 1 ||
    !Array.isArray(value.toolchains) || value.toolchains.length < 1 ||
    value.toolchains.length > 256) {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  const approvedArtifactHosts = value.approvedArtifactHosts.map((host) => {
    if (typeof host !== "string" || host !== host.toLowerCase() ||
      !/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(host)) {
      throw new CompilerFailure("invalid_reviewed_catalog");
    }
    return host;
  });
  if (new Set(approvedArtifactHosts).size !== approvedArtifactHosts.length) {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  const coordinates = new Set();
  const toolchainKeys = new Set();
  const toolchains = value.toolchains.map((toolchain) => {
    const parsed = parseToolchain(toolchain, value.runtimeCatalogRevision, approvedArtifactHosts);
    const key = `${parsed.minecraftVersion}\0${parsed.loader.kind}\0${parsed.loader.version}\0` +
      `${parsed.java.component}\0${parsed.java.major}`;
    if (toolchainKeys.has(key)) throw new CompilerFailure("invalid_reviewed_catalog");
    toolchainKeys.add(key);
    for (const artifact of parsed.artifacts) {
      const coordinateKey = `${key}\0${artifact.coordinate}`;
      if (coordinates.has(coordinateKey)) throw new CompilerFailure("invalid_reviewed_catalog");
      coordinates.add(coordinateKey);
    }
    return deepFreeze(parsed);
  });
  return deepFreeze({
    catalogVersion: legacy ? value.catalogVersion : value.catalogRevision.toLowerCase(),
    catalogRevision: generated ? value.catalogRevision.toLowerCase() : undefined,
    runtimeCatalogRevision: value.runtimeCatalogRevision.toLowerCase(),
    approvedArtifactHosts,
    toolchains,
  });
}

function parseToolchain(value, revision, approvedHosts) {
  if (!plainObject(value) ||
    !sameKeys(value, ["minecraftVersion", "loader", "java", "jre", "artifacts", "launchTemplate"]) ||
    !validMinecraftVersion(value.minecraftVersion) || !validLoader(value.loader) ||
    !validJava(value.java) || !plainObject(value.jre) ||
    !Array.isArray(value.artifacts) || value.artifacts.length < 1 || value.artifacts.length > 256 ||
    value.launchTemplate !== loaderTemplates[value.loader.kind]) {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  const jre = parseJre(value.jre, value.java, revision);
  const artifacts = value.artifacts.map((artifact) => parseArtifact(artifact, approvedHosts));
  const primary = artifacts.filter((artifact) => artifact.role === "primary");
  if (primary.length !== 1 ||
    primary[0].coordinate !== expectedPrimaryCoordinate(value.minecraftVersion, value.loader)) {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  const artifactCoordinates = new Set(artifacts.map((artifact) => artifact.coordinate));
  if (artifactCoordinates.size !== artifacts.length) throw new CompilerFailure("invalid_reviewed_catalog");
  return {
    minecraftVersion: value.minecraftVersion,
    loader: { kind: value.loader.kind, version: value.loader.version },
    java: { component: value.java.component, major: value.java.major },
    jre,
    artifacts,
    launchTemplate: value.launchTemplate,
  };
}

function parseJre(value, java, revision) {
  if (!sameKeys(value, ["id", "sha256", "component", "major", "runtimeCatalogRevision"]) ||
    !validIdentifier(value.id) || !validSha256(value.sha256) ||
    value.component !== java.component || value.major !== java.major ||
    value.runtimeCatalogRevision !== revision) {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  return {
    id: value.id,
    sha256: value.sha256.toLowerCase(),
    component: value.component,
    major: value.major,
    runtimeCatalogRevision: value.runtimeCatalogRevision.toLowerCase(),
  };
}

function parseArtifact(value, approvedHosts) {
  if (!plainObject(value) ||
    !sameKeys(value, ["role", "coordinate", "url", "sha256", "sizeBytes"]) ||
    !["primary", "dependency"].includes(value.role) ||
    !validCoordinate(value.coordinate) || typeof value.url !== "string" ||
    !validSha256(value.sha256) || !Number.isSafeInteger(value.sizeBytes) ||
    value.sizeBytes < 1 || value.sizeBytes > MAX_ARTIFACT_BYTES) {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  try {
    const url = new URL(value.url);
    if (url.protocol !== "https:" || url.username || url.password || url.hash ||
      url.port || !approvedHosts.includes(url.hostname.toLowerCase())) {
      throw new Error();
    }
  } catch {
    throw new CompilerFailure("invalid_reviewed_catalog");
  }
  return {
    role: value.role,
    coordinate: value.coordinate,
    url: value.url,
    sha256: value.sha256.toLowerCase(),
    sizeBytes: value.sizeBytes,
  };
}

function expectedPrimaryCoordinate(minecraftVersion, loader) {
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
      throw new CompilerFailure("invalid_reviewed_catalog");
  }
}

function hasReviewedDependencies(builder) {
  return builder.verifiedJres && typeof builder.verifiedJres.resolve === "function" &&
    builder.artifactDownloader && typeof builder.artifactDownloader.download === "function" &&
    builder.sandboxRunner && typeof builder.sandboxRunner.assemble === "function" &&
    hasCapabilities(builder.sandboxRunner.capabilities);
}

function hasCapabilities(capabilities) {
  return plainObject(capabilities) && Object.entries(requiredSandboxCapabilities)
    .every(([key, value]) => capabilities[key] === value);
}

async function resolveVerifiedJre(registry, expected) {
  let actual;
  try {
    actual = await registry.resolve({ ...expected });
  } catch {
    throw new CompilerFailure("compiler_unavailable");
  }
  if (!plainObject(actual) || actual.verified !== true || actual.readOnly !== true ||
    actual.id !== expected.id || actual.sha256?.toLowerCase() !== expected.sha256 ||
    actual.component !== expected.component || actual.major !== expected.major ||
    actual.runtimeCatalogRevision?.toLowerCase() !== expected.runtimeCatalogRevision ||
    !validIdentifier(actual.rootToken)) {
    throw new CompilerFailure("compiler_unavailable");
  }
  return {
    id: actual.id,
    sha256: actual.sha256.toLowerCase(),
    component: actual.component,
    major: actual.major,
    runtimeCatalogRevision: actual.runtimeCatalogRevision.toLowerCase(),
    rootToken: actual.rootToken,
  };
}

function validateApprovedArtifact(artifact, approvedHosts, maxArtifactBytes) {
  if (!Array.isArray(approvedHosts) || !Number.isSafeInteger(maxArtifactBytes) ||
    maxArtifactBytes < 1 || !plainObject(artifact) || !validCoordinate(artifact.coordinate) ||
    typeof artifact.url !== "string" || !validSha256(artifact.sha256) ||
    !Number.isSafeInteger(artifact.sizeBytes) || artifact.sizeBytes < 1 ||
    artifact.sizeBytes > maxArtifactBytes) {
    throw new CompilerFailure("artifact_not_approved");
  }
  let url;
  try {
    url = new URL(artifact.url);
  } catch {
    throw new CompilerFailure("artifact_not_approved");
  }
  if (url.protocol !== "https:" || url.username || url.password || url.hash || url.port ||
    !approvedHosts.includes(url.hostname.toLowerCase())) {
    throw new CompilerFailure("artifact_host_rejected");
  }
}

async function readExactResponse(response, expectedSize, maxArtifactBytes) {
  if (response.body?.getReader) {
    const reader = response.body.getReader();
    const chunks = [];
    let total = 0;
    try {
      while (true) {
        const next = await reader.read();
        if (next.done) break;
        if (!(next.value instanceof Uint8Array)) throw new CompilerFailure("artifact_download_failed");
        total += next.value.byteLength;
        if (total > expectedSize || total > maxArtifactBytes) {
          throw new CompilerFailure("artifact_size_mismatch");
        }
        chunks.push(next.value);
      }
    } finally {
      reader.releaseLock?.();
    }
    if (total !== expectedSize) throw new CompilerFailure("artifact_size_mismatch");
    return concatenate(chunks, total);
  }
  if (typeof response.arrayBuffer !== "function") throw new CompilerFailure("artifact_download_failed");
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength !== expectedSize || bytes.byteLength > maxArtifactBytes) {
    throw new CompilerFailure("artifact_size_mismatch");
  }
  return bytes;
}

function normalizeSandboxOutput(result) {
  if (!plainObject(result) || !validSandboxAttestation(result.attestation)) {
    throw new CompilerFailure("installer_failed");
  }
  if (!(result.files instanceof Map) || result.files.size > MAX_OUTPUT_FILES ||
    !outputSizeWithinLimit(
      [...result.files.values()].map((file) => file?.bytes?.byteLength),
      PRODUCTION_OUTPUT_LIMITS.maxSandboxBytes,
    )) {
    throw new CompilerFailure("invalid_installer_output");
  }
  const files = new Map();
  for (const [path, file] of result.files) {
    const pathIsSafe = safeOutputPath(path);
    const reason = !pathIsSafe
      ? "unsafe_path"
      : forbiddenOutputPath(path)
        ? "forbidden_path"
        : path.startsWith(".xmcl/")
          ? "reserved_path"
          : !(file.bytes instanceof Uint8Array) || !Number.isSafeInteger(file.mode)
            ? "invalid_metadata"
            : undefined;
    if (reason) {
      const failure = new CompilerFailure("invalid_installer_output");
      failure.installerOutputReason = reason;
      failure.installerOutputPath = pathIsSafe ? path : undefined;
      throw failure;
    }
    files.set(path, { bytes: file.bytes, mode: file.mode });
  }
  return files;
}

function validSandboxAttestation(value) {
  return plainObject(value) && value.ephemeralWorkspace === true && value.nonRoot === true &&
    value.readOnlyBaseFilesystem === true && value.noSecrets === true &&
    value.noDockerSocket === true && value.boundedResources === true &&
    value.network === "disabled";
}

function copyValidatedBundleContent(files, bundle) {
  for (const file of bundle.manifest.files) {
    if (!file.path.startsWith("instance/")) continue;
    const outputPath = file.path.slice("instance/".length);
    const source = bundle.entries.get(file.path)?.bytes;
    if (!(source instanceof Uint8Array) || source.byteLength !== file.sizeBytes ||
      sha256(source) !== file.sha256 || !safeOutputPath(outputPath) ||
      forbiddenOutputPath(outputPath) || files.has(outputPath)) {
      throw new CompilerFailure("invalid_local_content");
    }
    files.set(outputPath, { bytes: source, mode: 0o644 });
  }
}

function addGeneratedFile(files, path, bytes, mode) {
  if (files.has(path)) throw new CompilerFailure("invalid_installer_output");
  files.set(path, { bytes, mode });
}

function runtimeDescriptor(toolchain, plan) {
  return {
    schemaVersion: 1,
    runtimeCatalogRevision: toolchain.jre.runtimeCatalogRevision,
    minecraftVersion: toolchain.minecraftVersion,
    loader: { ...toolchain.loader },
    java: {
      component: toolchain.java.component,
      major: toolchain.java.major,
      jreId: toolchain.jre.id,
    },
    launch: {
      path: ".xmcl/launch.sh",
      kind: "generated-server-launcher",
      arguments: [...plan.launch.arguments],
    },
  };
}

function generatedLauncher(arguments_) {
  if (!Array.isArray(arguments_) || arguments_.some((argument) =>
    typeof argument !== "string" || !/^[A-Za-z0-9_@./:+-]+$/.test(argument))) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const command = arguments_.map(shellQuote).join(" ");
  return encoder.encode(
    "#!/bin/sh\nset -eu\n: \"${XMCL_JAVA:?XMCL_JAVA is required}\"\n" +
    `exec "$XMCL_JAVA" ${command}\n`,
  );
}

function shellQuote(value) {
  return value;
}

function validateOutputFiles(files) {
  if (!(files instanceof Map) || files.size < 2 || files.size > MAX_OUTPUT_FILES) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const sizes = [];
  const entries = [];
  for (const [path, file] of [...files.entries()].sort(([left], [right]) => comparePath(left, right))) {
    if (!safeOutputPath(path) ||
      (path !== ".xmcl/runtime.json" && path !== ".xmcl/launch.sh" && forbiddenOutputPath(path)) ||
      !(file?.bytes instanceof Uint8Array) || !validOutputMode(file.mode)) {
      throw new CompilerFailure("invalid_builder_output");
    }
    sizes.push(file.bytes.byteLength);
    entries.push({
      path,
      sha256: sha256(file.bytes),
      sizeBytes: file.bytes.byteLength,
      mode: file.mode,
    });
  }
  if (!files.has(".xmcl/runtime.json") || !files.has(".xmcl/launch.sh") ||
    files.get(".xmcl/launch.sh").mode !== 0o755 ||
    !outputSizeWithinLimit(sizes, PRODUCTION_OUTPUT_LIMITS.maxPackageInputBytes)) {
    throw new CompilerFailure("invalid_builder_output");
  }
  return entries;
}

function packageDeterministicTarZst(files) {
  const tar = packageTar(files);
  return rawZstd(tar);
}

function packageTar(files) {
  const chunks = [];
  let total = 0;
  for (const [path, file] of [...files.entries()].sort(([left], [right]) => comparePath(left, right))) {
    const header = new Uint8Array(512);
    const { name, prefix } = splitTarPath(path);
    writeTarText(header, 0, 100, name);
    writeTarOctal(header, 100, 8, file.mode);
    writeTarOctal(header, 108, 8, 0);
    writeTarOctal(header, 116, 8, 0);
    writeTarOctal(header, 124, 12, file.bytes.byteLength);
    writeTarOctal(header, 136, 12, 0);
    header.fill(0x20, 148, 156);
    header[156] = 0x30;
    writeTarText(header, 257, 6, "ustar");
    writeTarText(header, 263, 2, "00");
    writeTarText(header, 265, 32, "root");
    writeTarText(header, 297, 32, "root");
    writeTarText(header, 345, 155, prefix);
    const checksum = header.reduce((sum, byte) => sum + byte, 0);
    writeTarChecksum(header, checksum);
    const padding = (512 - (file.bytes.byteLength % 512)) % 512;
    chunks.push(header, file.bytes, new Uint8Array(padding));
    total += 512 + file.bytes.byteLength + padding;
    if (total > PRODUCTION_OUTPUT_LIMITS.maxPackageTarBytes) {
      throw new CompilerFailure("invalid_builder_output");
    }
  }
  chunks.push(new Uint8Array(1024));
  if (total + 1024 > PRODUCTION_OUTPUT_LIMITS.maxPackageTarBytes) {
    throw new CompilerFailure("invalid_builder_output");
  }
  return concatenate(chunks, total + 1024);
}

function rawZstd(input) {
  if (input.byteLength > PRODUCTION_OUTPUT_LIMITS.maxPackageTarBytes) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const chunks = [Uint8Array.of(0x28, 0xb5, 0x2f, 0xfd, 0xa0, ...littleEndian32(input.byteLength))];
  let total = chunks[0].byteLength;
  for (let offset = 0; offset < input.byteLength; offset += RAW_ZSTD_BLOCK_BYTES) {
    const size = Math.min(RAW_ZSTD_BLOCK_BYTES, input.byteLength - offset);
    const last = offset + size === input.byteLength ? 1 : 0;
    const header = (size << 3) | last;
    chunks.push(Uint8Array.of(header & 0xff, (header >>> 8) & 0xff, (header >>> 16) & 0xff));
    chunks.push(input.subarray(offset, offset + size));
    total += 3 + size;
  }
  if (total > PRODUCTION_OUTPUT_LIMITS.maxPackageArchiveBytes) {
    throw new CompilerFailure("invalid_builder_output");
  }
  return concatenate(chunks, total);
}

function unpackDeterministicTarZst(archive) {
  if (!(archive instanceof Uint8Array) || archive.byteLength < 12 ||
    archive.byteLength > PRODUCTION_OUTPUT_LIMITS.maxPackageArchiveBytes ||
    archive[0] !== 0x28 || archive[1] !== 0xb5 || archive[2] !== 0x2f || archive[3] !== 0xfd ||
    archive[4] !== 0xa0) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const expectedSize = readLittleEndian32(archive, 5);
  const chunks = [];
  let total = 0;
  let offset = 9;
  let last = false;
  while (!last) {
    if (offset + 3 > archive.byteLength) throw new CompilerFailure("invalid_builder_output");
    const header = archive[offset] | (archive[offset + 1] << 8) | (archive[offset + 2] << 16);
    offset += 3;
    last = (header & 1) === 1;
    const type = (header >>> 1) & 3;
    const size = header >>> 3;
    if (type !== 0 || size > RAW_ZSTD_BLOCK_BYTES || offset + size > archive.byteLength) {
      throw new CompilerFailure("invalid_builder_output");
    }
    chunks.push(archive.subarray(offset, offset + size));
    offset += size;
    total += size;
  }
  if (offset !== archive.byteLength || total !== expectedSize) {
    throw new CompilerFailure("invalid_builder_output");
  }
  return unpackTar(concatenate(chunks, total));
}

function unpackTar(tar) {
  const files = new Map();
  let offset = 0;
  while (offset + 512 <= tar.byteLength) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every((byte) => byte === 0)) {
      if (offset + 1024 !== tar.byteLength ||
        !tar.subarray(offset + 512, offset + 1024).every((byte) => byte === 0)) {
        throw new CompilerFailure("invalid_builder_output");
      }
      return files;
    }
    if (tarText(header, 257, 6) !== "ustar" ||
      (header[156] !== 0 && header[156] !== 0x30)) {
      throw new CompilerFailure("invalid_builder_output");
    }
    const name = tarText(header, 0, 100);
    const prefix = tarText(header, 345, 155);
    const path = prefix ? `${prefix}/${name}` : name;
    const sizeBytes = parseTarOctal(header, 124, 12);
    const mode = parseTarOctal(header, 100, 8);
    const dataOffset = offset + 512;
    if (!safeOutputPath(path) || files.has(path) || !Number.isSafeInteger(sizeBytes) ||
      dataOffset + sizeBytes > tar.byteLength || !validOutputMode(mode)) {
      throw new CompilerFailure("invalid_builder_output");
    }
    const bytes = tar.subarray(dataOffset, dataOffset + sizeBytes);
    files.set(path, { bytes, sha256: sha256(bytes), sizeBytes, mode });
    offset = dataOffset + sizeBytes + ((512 - (sizeBytes % 512)) % 512);
  }
  throw new CompilerFailure("invalid_builder_output");
}

function validateEntryMetadata(entries) {
  if (entries.length < 2 || entries.length > MAX_OUTPUT_FILES) {
    throw new CompilerFailure("invalid_builder_output");
  }
  let previous = "";
  const sizes = [];
  const normalized = entries.map((entry) => {
    if (!plainObject(entry) || !sameKeys(entry, ["path", "sha256", "sizeBytes", "mode"]) ||
      !safeOutputPath(entry.path) ||
      (entry.path !== ".xmcl/runtime.json" && entry.path !== ".xmcl/launch.sh" &&
        forbiddenOutputPath(entry.path)) ||
      !validSha256(entry.sha256) ||
      !Number.isSafeInteger(entry.sizeBytes) || entry.sizeBytes < 0 ||
      !validOutputMode(entry.mode) || (previous && comparePath(previous, entry.path) >= 0)) {
      throw new CompilerFailure("invalid_builder_output");
    }
    previous = entry.path;
    sizes.push(entry.sizeBytes);
    return {
      path: entry.path,
      sha256: entry.sha256.toLowerCase(),
      sizeBytes: entry.sizeBytes,
      mode: entry.mode,
    };
  });
  if (!outputSizeWithinLimit(sizes, PRODUCTION_OUTPUT_LIMITS.maxPackageInputBytes)) {
    throw new CompilerFailure("invalid_builder_output");
  }
  return normalized;
}

function validGeneratedDescriptor(value) {
  return plainObject(value) && value.schemaVersion === 1 && plainObject(value.runtime) &&
    value.runtime.path === ".xmcl/runtime.json" && validSha256(value.runtime.sha256) &&
    plainObject(value.launch) && value.launch.path === ".xmcl/launch.sh" &&
    value.launch.kind === "generated-server-launcher" &&
    Array.isArray(value.launch.arguments) && value.launch.arguments.length === 0;
}

function entryHash(entries, path) {
  const entry = entries.find((candidate) => candidate.path === path);
  if (!entry) throw new CompilerFailure("invalid_builder_output");
  return entry.sha256;
}

function validateGeneratedRuntimeAndLauncher(unpacked, runtime, launcher) {
  let value;
  try {
    value = JSON.parse(decoder.decode(unpacked.get(".xmcl/runtime.json").bytes));
  } catch {
    throw new CompilerFailure("invalid_builder_output");
  }
  if (!plainObject(value) ||
    !sameKeys(value, ["schemaVersion", "runtimeCatalogRevision", "minecraftVersion", "loader",
      "java", "launch"]) ||
    value.schemaVersion !== 1 || !validSha256(value.runtimeCatalogRevision) ||
    !validMinecraftVersion(value.minecraftVersion) || !validLoader(value.loader) ||
    !plainObject(value.java) ||
    !sameKeys(value.java, ["component", "major", "jreId"]) ||
    !validIdentifier(value.java.component) || !Number.isSafeInteger(value.java.major) ||
    !validIdentifier(value.java.jreId) || !plainObject(value.launch) ||
    !sameKeys(value.launch, ["path", "kind", "arguments"]) ||
    value.launch.path !== ".xmcl/launch.sh" ||
    value.launch.kind !== "generated-server-launcher" ||
    !Array.isArray(value.launch.arguments)) {
    throw new CompilerFailure("invalid_builder_output");
  }
  const expected = createAssemblyPlan({
    minecraftVersion: value.minecraftVersion,
    loader: value.loader,
    java: { component: value.java.component, major: value.java.major },
    jre: {
      id: value.java.jreId,
      sha256: "0".repeat(64),
      component: value.java.component,
      major: value.java.major,
      runtimeCatalogRevision: value.runtimeCatalogRevision,
    },
    artifacts: [],
  }).launch.arguments;
  if (JSON.stringify(value.launch.arguments) !== JSON.stringify(expected) ||
    decoder.decode(unpacked.get(".xmcl/launch.sh").bytes) !==
      decoder.decode(generatedLauncher(expected))) {
    throw new CompilerFailure("invalid_builder_output");
  }
}

function fakeServerFiles(plan) {
  const seed = encoder.encode(JSON.stringify(plan));
  const files = new Map([
    ["server.jar", { bytes: new Uint8Array(createHash("sha256").update(seed).digest()), mode: 0o644 }],
  ]);
  const argsFile = plan.launch.arguments.find((argument) => argument.startsWith("@"));
  if (argsFile) {
    files.set(argsFile.slice(1), {
      bytes: encoder.encode("-jar server.jar\n"),
      mode: 0o644,
    });
  }
  return files;
}

function cloneFiles(value) {
  const entries = value instanceof Map ? [...value.entries()] : Array.isArray(value) ? value : undefined;
  if (!entries || entries.length > MAX_OUTPUT_FILES) throw new CompilerFailure("installer_failed");
  const files = new Map();
  for (const entry of entries) {
    if (!Array.isArray(entry) || entry.length !== 2 || typeof entry[0] !== "string") {
      throw new CompilerFailure("installer_failed");
    }
    const file = entry[1];
    const bytes = file instanceof Uint8Array ? file : file?.bytes;
    const mode = file instanceof Uint8Array ? 0o644 : file?.mode ?? 0o644;
    if (!(bytes instanceof Uint8Array) || files.has(entry[0])) throw new CompilerFailure("installer_failed");
    files.set(entry[0], { bytes: bytes.slice(), mode });
  }
  return files;
}

function safeOutputPath(path) {
  return typeof path === "string" && path.length > 0 && path.length <= 255 &&
    !path.includes("\\") && !path.startsWith("/") && !/^[A-Za-z]:/.test(path) &&
    path.split("/").every((part) => part && part !== "." && part !== "..");
}

function forbiddenOutputPath(path) {
  const lower = path.toLowerCase();
  return /(?:^|\/)(?:eula\.txt|java(?:w)?(?:\.exe)?)$/.test(lower) ||
    /(?:^|\/)(?:jre|jdk)(?:\/|$)/.test(lower) ||
    /\.(?:sh|bat|cmd|ps1|exe)$/i.test(path);
}

function validOutputMode(mode) {
  return mode === 0o644 || mode === 0o755;
}

function validContentKey(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 1024 &&
    !/[\x00-\x1f\x7f]/.test(value);
}

function validLoader(value) {
  return plainObject(value) && sameKeys(value, ["kind", "version"]) &&
    Object.hasOwn(loaderTemplates, value.kind) && validLoaderVersion(value.version);
}

function validJava(value) {
  return plainObject(value) && sameKeys(value, ["component", "major"]) &&
    validIdentifier(value.component) && Number.isSafeInteger(value.major) &&
    value.major >= 1 && value.major <= 255;
}

function validMinecraftVersion(value) {
  return typeof value === "string" &&
    /^(?:1\.(?:0|[1-9]\d{0,2})\.(?:0|[1-9]\d{0,2})|[1-9]\d{1,3}\.(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2}))?)$/.test(value);
}

function validLoaderVersion(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128 &&
    /^[0-9A-Za-z][0-9A-Za-z._+-]*$/.test(value);
}

function validCoordinate(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 256 &&
    /^[0-9A-Za-z_.-]+:[0-9A-Za-z_.-]+:[0-9A-Za-z._+-]+(?::[0-9A-Za-z_.-]+)?$/.test(value);
}

function validIdentifier(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128 &&
    /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(value);
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value);
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function sameKeys(value, expected) {
  const keys = Object.keys(value).sort();
  return keys.length === expected.length && keys.every((key, index) => key === [...expected].sort()[index]);
}

function encodeJson(value) {
  return encoder.encode(JSON.stringify(value));
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function concatenate(chunks, total) {
  const output = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    output.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return output;
}

function comparePath(left, right) {
  return left < right ? -1 : left > right ? 1 : 0;
}

function deepFreeze(value) {
  if (value && typeof value === "object" && !Object.isFrozen(value)) {
    Object.freeze(value);
    for (const child of Object.values(value)) deepFreeze(child);
  }
  return value;
}

function splitTarPath(path) {
  const bytes = encoder.encode(path);
  if (bytes.byteLength <= 100) return { name: path, prefix: "" };
  const parts = path.split("/");
  for (let index = 1; index < parts.length; index += 1) {
    const prefix = parts.slice(0, index).join("/");
    const name = parts.slice(index).join("/");
    if (encoder.encode(prefix).byteLength <= 155 && encoder.encode(name).byteLength <= 100) {
      return { name, prefix };
    }
  }
  throw new CompilerFailure("invalid_builder_output");
}

function writeTarText(target, offset, length, value) {
  const bytes = encoder.encode(value);
  if (bytes.byteLength > length) throw new CompilerFailure("invalid_builder_output");
  target.set(bytes, offset);
}

function writeTarOctal(target, offset, length, value) {
  const text = value.toString(8).padStart(length - 1, "0");
  if (text.length !== length - 1) throw new CompilerFailure("invalid_builder_output");
  writeTarText(target, offset, length, text);
}

function writeTarChecksum(target, value) {
  const text = value.toString(8).padStart(6, "0");
  if (text.length !== 6) throw new CompilerFailure("invalid_builder_output");
  target.set(encoder.encode(`${text}\0 `), 148);
}

function tarText(target, offset, length) {
  const end = target.indexOf(0, offset);
  const boundedEnd = end === -1 || end > offset + length ? offset + length : end;
  try {
    return decoder.decode(target.subarray(offset, boundedEnd));
  } catch {
    throw new CompilerFailure("invalid_builder_output");
  }
}

function parseTarOctal(target, offset, length) {
  const text = tarText(target, offset, length).trim();
  if (!/^[0-7]+$/.test(text)) throw new CompilerFailure("invalid_builder_output");
  const value = Number.parseInt(text, 8);
  if (!Number.isSafeInteger(value)) throw new CompilerFailure("invalid_builder_output");
  return value;
}

function littleEndian32(value) {
  return [value & 0xff, (value >>> 8) & 0xff, (value >>> 16) & 0xff, (value >>> 24) & 0xff];
}

function readLittleEndian32(bytes, offset) {
  return (bytes[offset] | (bytes[offset + 1] << 8) | (bytes[offset + 2] << 16) |
    (bytes[offset + 3] << 24)) >>> 0;
}
