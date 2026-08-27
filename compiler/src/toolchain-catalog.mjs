import { createHash } from "node:crypto";
import { inflateRawSync } from "node:zlib";

const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const ZERO_SHA256 = "0".repeat(64);
const MAX_METADATA_BYTES = 4 * 1024 * 1024;
const MAX_ARTIFACT_BYTES = 512 * 1024 * 1024;

export const APPROVED_METADATA_HOSTS = Object.freeze([
  "maven.minecraftforge.net",
  "meta.fabricmc.net",
  "maven.neoforged.net",
  "meta.quiltmc.org",
  "piston-meta.mojang.com",
]);

export const APPROVED_ARTIFACT_HOSTS = Object.freeze([
  "maven.fabricmc.net",
  "maven.minecraftforge.net",
  "maven.neoforged.net",
  "maven.quiltmc.org",
  "libraries.minecraft.net",
  "piston-data.mojang.com",
]);

export const SUPPORTED_TOOLCHAIN_CANDIDATES = Object.freeze([
  {
    minecraftVersion: "1.12.2",
    loader: { kind: "forge", version: "14.23.5.2859" },
    java: { component: "jre-legacy", major: 8 },
  },
  {
    minecraftVersion: "1.17.1",
    loader: { kind: "fabric", version: "0.12.12" },
    java: { component: "java-runtime-alpha", major: 16 },
  },
  {
    minecraftVersion: "1.20.1",
    loader: { kind: "fabric", version: "0.15.11" },
    java: { component: "java-runtime-gamma", major: 17 },
  },
  {
    minecraftVersion: "1.21.1",
    loader: { kind: "neoforge", version: "21.1.115" },
    java: { component: "java-runtime-delta", major: 21 },
  },
  {
    minecraftVersion: "26.2",
    loader: { kind: "fabric", version: "0.19.3" },
    java: { component: "java-runtime-epsilon", major: 25 },
  },
]);

const loaderTemplates = Object.freeze({
  forge: "forge-unix-args-v1",
  fabric: "fabric-server-jar-v1",
  neoforge: "neoforge-unix-args-v1",
  quilt: "quilt-server-jar-v1",
});

export class ToolchainCatalogFailure extends Error {
  constructor(code) {
    super(code);
    this.code = code;
    this.name = "ToolchainCatalogFailure";
  }
}

export class StrictCatalogFetcher {
  constructor({
    fetchImpl = fetch,
    timeoutMs = 30_000,
    maxMetadataBytes = MAX_METADATA_BYTES,
    maxArtifactBytes = MAX_ARTIFACT_BYTES,
  } = {}) {
    if (typeof fetchImpl !== "function" || !validPositiveInteger(timeoutMs, 120_000) ||
      !validPositiveInteger(maxMetadataBytes, MAX_METADATA_BYTES) ||
      !validPositiveInteger(maxArtifactBytes, MAX_ARTIFACT_BYTES)) {
      throw new TypeError("invalid catalog fetcher configuration");
    }
    this.fetchImpl = fetchImpl;
    this.timeoutMs = timeoutMs;
    this.maxMetadataBytes = maxMetadataBytes;
    this.maxArtifactBytes = maxArtifactBytes;
  }

  async metadata(url) {
    return await this.#download(url, APPROVED_METADATA_HOSTS, this.maxMetadataBytes, false);
  }

  async artifact(url) {
    return await this.#download(url, APPROVED_ARTIFACT_HOSTS, this.maxArtifactBytes, true);
  }

  async checksum(url) {
    return await this.#download(url, APPROVED_ARTIFACT_HOSTS, this.maxMetadataBytes, false);
  }

  async #download(url, hosts, maxBytes, requireContentLength) {
    for (let attempt = 0; attempt < 3; attempt += 1) {
      try {
        return await this.#downloadOnce(url, hosts, maxBytes, requireContentLength);
      } catch (error) {
        if (!(error instanceof ToolchainCatalogFailure) ||
          !["upstream_fetch_failed", "upstream_fetch_timeout"].includes(error.code) || attempt === 2) {
          if (error instanceof ToolchainCatalogFailure) error.url ??= url;
          throw error;
        }
        await new Promise((resolve) => setTimeout(resolve, 500 * (attempt + 1)));
      }
    }
    throw new ToolchainCatalogFailure("upstream_fetch_failed");
  }

  async #downloadOnce(url, hosts, maxBytes, requireContentLength) {
    assertTrustedUrl(url, hosts, "upstream_url_rejected");
    const controller = new AbortController();
    let timeout;
    try {
      const response = await Promise.race([
        this.fetchImpl(url, {
          method: "GET",
          redirect: "error",
          credentials: "omit",
          referrerPolicy: "no-referrer",
          signal: controller.signal,
        }),
        new Promise((_, reject) => {
          timeout = setTimeout(() => {
            controller.abort();
            reject(new ToolchainCatalogFailure("upstream_fetch_timeout"));
          }, this.timeoutMs);
        }),
      ]);
      if (!response || response.status !== 200 || response.redirected) {
        throw new ToolchainCatalogFailure("upstream_fetch_failed");
      }
      if (response.url && new URL(response.url).href !== new URL(url).href) {
        throw new ToolchainCatalogFailure("upstream_redirect_rejected");
      }
      const contentLength = response.headers?.get("content-length");
      const contentEncoding = response.headers?.get("content-encoding");
      const encoded = contentEncoding && contentEncoding.toLowerCase() !== "identity";
      if (requireContentLength && encoded) throw new ToolchainCatalogFailure("upstream_size_mismatch");
      if (contentLength !== null && contentLength !== undefined) {
        if (!/^(?:0|[1-9]\d*)$/.test(contentLength) || Number(contentLength) > maxBytes) {
          throw new ToolchainCatalogFailure("upstream_size_mismatch");
        }
      } else if (requireContentLength) {
        throw new ToolchainCatalogFailure("upstream_size_mismatch");
      }
      const bytes = await readBoundedResponse(response, maxBytes);
      if (!encoded && contentLength !== null && contentLength !== undefined &&
        bytes.byteLength !== Number(contentLength)) {
        throw new ToolchainCatalogFailure("upstream_size_mismatch");
      }
      return bytes;
    } catch (error) {
      if (error instanceof ToolchainCatalogFailure) throw error;
      const failure = new ToolchainCatalogFailure("upstream_fetch_failed");
      failure.cause = error;
      throw failure;
    } finally {
      clearTimeout(timeout);
    }
  }
}

export async function generateToolchainCatalog({
  runtimeCatalogBytes,
  fetchImpl = fetch,
  candidates = SUPPORTED_TOOLCHAIN_CANDIDATES,
  timeoutMs,
} = {}) {
  const runtime = parseReviewedRuntimeCatalog(runtimeCatalogBytes);
  validateCandidates(candidates);
  const fetcher = new StrictCatalogFetcher({ fetchImpl, timeoutMs });
  const minecraft = await loadMinecraftMetadata(fetcher, candidates);
  const toolchains = [];
  for (const candidate of candidates) {
    const jre = jreFor(runtime, candidate.java);
    const primary = await resolvePrimaryArtifact(fetcher, candidate);
    const server = await resolveMinecraftServer(fetcher, minecraft.get(candidate.minecraftVersion), candidate);
    const dependencies = candidate.loader.kind === "neoforge"
      ? await resolveNeoForgeDependencies(fetcher, primary.artifactBytes,
        minecraft.get(candidate.minecraftVersion), candidate)
      : [];
    toolchains.push({
      minecraftVersion: candidate.minecraftVersion,
      loader: { ...candidate.loader },
      java: { ...candidate.java },
      jre,
      artifacts: sortArtifacts([primary, server, ...dependencies]),
      launchTemplate: loaderTemplates[candidate.loader.kind],
    });
  }
  const lock = {
    schemaVersion: 1,
    catalogRevision: ZERO_SHA256,
    runtimeCatalogRevision: runtime.revision,
    approvedArtifactHosts: [...APPROVED_ARTIFACT_HOSTS],
    toolchains: toolchains.sort(compareToolchains),
  };
  lock.catalogRevision = catalogRevisionFor(lock);
  validateToolchainCatalog(lock, runtimeCatalogBytes, {
    expectedCandidates: candidates,
    requireCanonical: false,
  });
  return canonicalize(lock);
}

export function validateToolchainCatalog(document, runtimeCatalogBytes, {
  expectedCandidates,
  requireCanonical = false,
  rawLockText,
} = {}) {
  const catalog = parseCatalogDocument(document);
  const runtime = parseReviewedRuntimeCatalog(runtimeCatalogBytes);
  if (catalog.runtimeCatalogRevision !== runtime.revision ||
    catalog.catalogRevision !== catalogRevisionFor(catalog)) {
    throw new ToolchainCatalogFailure("catalog_revision_mismatch");
  }
  if (requireCanonical && rawLockText !== formatCatalog(catalog)) {
    throw new ToolchainCatalogFailure("catalog_not_canonical");
  }
  if (!sameStringList(catalog.approvedArtifactHosts, APPROVED_ARTIFACT_HOSTS)) {
    throw new ToolchainCatalogFailure("catalog_hosts_invalid");
  }
  const tupleKeys = new Set();
  for (const toolchain of catalog.toolchains) {
    validateToolchain(toolchain, catalog, runtime, tupleKeys);
  }
  if (expectedCandidates) {
    const expected = new Set(expectedCandidates.map(candidateKey));
    const actual = new Set(catalog.toolchains.map(candidateKey));
    if (expected.size !== actual.size || [...expected].some((key) => !actual.has(key))) {
      throw new ToolchainCatalogFailure("catalog_coverage_mismatch");
    }
  }
  return canonicalize(catalog);
}

export function parseCatalogText(bytes) {
  return parseJson(bytes, "catalog_malformed");
}

export function formatCatalog(document) {
  return `${JSON.stringify(canonicalize(document), null, 2)}\n`;
}

export function catalogRevisionFor(document) {
  if (!plainObject(document)) throw new ToolchainCatalogFailure("catalog_malformed");
  return sha256(canonicalJsonBytes({ ...document, catalogRevision: ZERO_SHA256 }));
}

export function canonicalJsonBytes(value) {
  return encoder.encode(JSON.stringify(canonicalize(value)));
}

export function primaryArtifactUrl(candidate) {
  validateCandidate(candidate);
  const { minecraftVersion, loader } = candidate;
  switch (loader.kind) {
    case "forge":
      return `https://maven.minecraftforge.net/net/minecraftforge/forge/${minecraftVersion}-${loader.version}/` +
        `forge-${minecraftVersion}-${loader.version}-installer.jar`;
    case "fabric":
      return `https://maven.fabricmc.net/net/fabricmc/fabric-loader/${loader.version}/` +
        `fabric-loader-${loader.version}.jar`;
    case "neoforge":
      return `https://maven.neoforged.net/releases/net/neoforged/neoforge/${loader.version}/` +
        `neoforge-${loader.version}-installer.jar`;
    case "quilt":
      return `https://maven.quiltmc.org/repository/release/org/quiltmc/quilt-loader/${loader.version}/` +
        `quilt-loader-${loader.version}.jar`;
    default:
      throw new ToolchainCatalogFailure("candidate_invalid");
  }
}

function parseCatalogDocument(document) {
  if (!plainObject(document) ||
    !sameKeys(document, [
      "approvedArtifactHosts",
      "catalogRevision",
      "runtimeCatalogRevision",
      "schemaVersion",
      "toolchains",
    ]) ||
    document.schemaVersion !== 1 || !validSha256(document.catalogRevision) ||
    !validSha256(document.runtimeCatalogRevision) ||
    !Array.isArray(document.approvedArtifactHosts) ||
    !Array.isArray(document.toolchains) || document.toolchains.length < 1) {
    throw new ToolchainCatalogFailure("catalog_malformed");
  }
  return canonicalize(document);
}

function parseReviewedRuntimeCatalog(bytes) {
  if (!(bytes instanceof Uint8Array)) throw new ToolchainCatalogFailure("runtime_catalog_malformed");
  const document = parseJson(bytes, "runtime_catalog_malformed");
  if (document.schemaVersion !== 1 || !plainObject(document.platform) ||
    document.platform.os !== "linux" || document.platform.architecture !== "x64" ||
    !Array.isArray(document.requirements) || !Array.isArray(document.runtimes) ||
    !Array.isArray(document.resolutions)) {
    throw new ToolchainCatalogFailure("runtime_catalog_malformed");
  }
  const requirements = new Set();
  for (const entry of document.requirements) {
    if (!validJava(entry)) throw new ToolchainCatalogFailure("runtime_catalog_malformed");
    const key = javaKey(entry);
    if (requirements.has(key)) throw new ToolchainCatalogFailure("runtime_catalog_malformed");
    requirements.add(key);
  }
  const resolutions = new Map();
  for (const resolution of document.resolutions) {
    if (!plainObject(resolution) || !validJava(resolution)) {
      throw new ToolchainCatalogFailure("runtime_catalog_malformed");
    }
    const key = javaKey(resolution);
    if (resolutions.has(key)) throw new ToolchainCatalogFailure("runtime_catalog_malformed");
    resolutions.set(key, canonicalize(resolution));
  }
  const selected = new Map();
  for (const entry of document.runtimes) {
    if (!validJava(entry)) throw new ToolchainCatalogFailure("runtime_catalog_malformed");
    const key = javaKey(entry);
    if (!requirements.has(key) || !resolutions.has(key) || selected.has(key)) {
      throw new ToolchainCatalogFailure("runtime_catalog_malformed");
    }
    selected.set(key, canonicalize(entry));
  }
  return {
    revision: sha256(bytes),
    selected,
    resolutions,
  };
}

function jreFor(runtime, java) {
  const key = javaKey(java);
  if (!runtime.selected.has(key)) throw new ToolchainCatalogFailure("runtime_java_unavailable");
  const resolution = runtime.resolutions.get(key);
  return {
    id: `${java.component}-${java.major}`,
    sha256: sha256(canonicalJsonBytes(resolution)),
    component: java.component,
    major: java.major,
    runtimeCatalogRevision: runtime.revision,
  };
}

async function loadMinecraftMetadata(fetcher, candidates) {
  const bytes = await fetcher.metadata("https://piston-meta.mojang.com/mc/game/version_manifest_v2.json");
  const manifest = parseJson(bytes, "minecraft_metadata_malformed");
  if (!Array.isArray(manifest.versions)) throw new ToolchainCatalogFailure("minecraft_metadata_malformed");
  const selected = new Map();
  for (const candidate of candidates) {
    if (selected.has(candidate.minecraftVersion)) continue;
    const matches = manifest.versions.filter((entry) => entry?.id === candidate.minecraftVersion);
    if (matches.length !== 1 || !plainObject(matches[0]) ||
      !validSha1(matches[0].sha1) || typeof matches[0].url !== "string") {
      throw new ToolchainCatalogFailure("minecraft_metadata_malformed");
    }
    assertTrustedUrl(matches[0].url, ["piston-meta.mojang.com"], "upstream_url_rejected");
    const metadataBytes = await fetcher.metadata(matches[0].url);
    verifySha1(metadataBytes, matches[0].sha1, "upstream_checksum_mismatch");
    const metadata = parseJson(metadataBytes, "minecraft_metadata_malformed");
    verifyMinecraftJava(metadata, candidate);
    selected.set(candidate.minecraftVersion, metadata);
  }
  return selected;
}

function verifyMinecraftJava(metadata, candidate) {
  const java = metadata.javaVersion;
  if (java === undefined) {
    if (candidate.java.component === "jre-legacy" && candidate.java.major === 8) return;
    throw new ToolchainCatalogFailure("minecraft_java_mismatch");
  }
  if (!plainObject(java) || java.component !== candidate.java.component ||
    java.majorVersion !== candidate.java.major) {
    throw new ToolchainCatalogFailure("minecraft_java_mismatch");
  }
}

async function resolvePrimaryArtifact(fetcher, candidate) {
  const url = primaryArtifactUrl(candidate);
  const coordinate = primaryCoordinate(candidate);
  let expectedSha1;
  let expectedSize;
  if (candidate.loader.kind === "fabric") {
    const metadata = await loadFabricMetadata(fetcher, candidate);
    expectedSha1 = await fetchMavenSha1(fetcher, url);
    if (metadata.loader.version !== candidate.loader.version) {
      throw new ToolchainCatalogFailure("loader_metadata_malformed");
    }
  } else if (candidate.loader.kind === "quilt") {
    const metadata = await loadQuiltMetadata(fetcher, candidate);
    expectedSha1 = metadata.loader.hashes.sha1;
    expectedSize = metadata.loader.file_size;
  } else {
    await verifyMavenArtifactVersion(fetcher, candidate);
    expectedSha1 = await fetchMavenSha1(fetcher, url);
  }
  return await downloadArtifact(fetcher, {
    role: "primary",
    coordinate,
    url,
    expectedSha1,
    expectedSize,
  });
}

async function loadFabricMetadata(fetcher, candidate) {
  const { minecraftVersion, loader } = candidate;
  const url = `https://meta.fabricmc.net/v2/versions/loader/${minecraftVersion}/${loader.version}`;
  const metadata = parseJson(await fetcher.metadata(url), "loader_metadata_malformed");
  if (!plainObject(metadata.loader) || metadata.loader.version !== loader.version ||
    metadata.loader.maven !== `net.fabricmc:fabric-loader:${loader.version}` ||
    !plainObject(metadata.intermediary) ||
    ![minecraftVersion, "0.0.0"].includes(metadata.intermediary.version)) {
    throw new ToolchainCatalogFailure("loader_metadata_malformed");
  }
  return metadata;
}

async function loadQuiltMetadata(fetcher, candidate) {
  const { minecraftVersion, loader } = candidate;
  const url = `https://meta.quiltmc.org/v3/versions/loader/${minecraftVersion}/${loader.version}`;
  const metadata = parseJson(await fetcher.metadata(url), "loader_metadata_malformed");
  if (!plainObject(metadata.loader) || metadata.loader.version !== loader.version ||
    metadata.loader.maven !== `org.quiltmc:quilt-loader:${loader.version}` ||
    !validPositiveInteger(metadata.loader.file_size, MAX_ARTIFACT_BYTES) ||
    !plainObject(metadata.loader.hashes) || !validSha1(metadata.loader.hashes.sha1) ||
    (metadata.intermediary !== undefined &&
      (!plainObject(metadata.intermediary) || metadata.intermediary.version !== minecraftVersion))) {
    throw new ToolchainCatalogFailure("loader_metadata_malformed");
  }
  return metadata;
}

async function verifyMavenArtifactVersion(fetcher, candidate) {
  const forge = candidate.loader.kind === "forge";
  const groupPath = forge ? "net/minecraftforge/forge" : "releases/net/neoforged/neoforge";
  const host = forge ? "maven.minecraftforge.net" : "maven.neoforged.net";
  const groupId = forge ? "net.minecraftforge" : "net.neoforged";
  const artifactId = forge ? "forge" : "neoforge";
  const expectedVersion = forge
    ? `${candidate.minecraftVersion}-${candidate.loader.version}`
    : candidate.loader.version;
  const xml = await fetcher.metadata(`https://${host}/${groupPath}/maven-metadata.xml`);
  const versions = parseMavenMetadata(xml, groupId, artifactId);
  if (!versions.includes(expectedVersion)) {
    throw new ToolchainCatalogFailure("loader_version_unavailable");
  }
}

function parseMavenMetadata(bytes, expectedGroupId, expectedArtifactId) {
  let xml;
  try {
    xml = decoder.decode(bytes);
  } catch {
    throw new ToolchainCatalogFailure("maven_metadata_malformed");
  }
  if (xml.includes("<!") || /&(?:#\d+|#x[a-f0-9]+|[a-z]+);/i.test(xml)) {
    throw new ToolchainCatalogFailure("maven_metadata_malformed");
  }
  xml = xml.replace(/^\s*<\?xml\s+version=["']1\.0["'](?:\s+encoding=["'][A-Za-z0-9._-]+["'])?\s*\?>\s*/, "");
  const metadata = /^<metadata>([\s\S]*)<\/metadata>\s*$/.exec(xml);
  if (!metadata || textTag(metadata[1], "groupId") !== expectedGroupId ||
    textTag(metadata[1], "artifactId") !== expectedArtifactId) {
    throw new ToolchainCatalogFailure("maven_metadata_malformed");
  }
  const versions = singleTag(metadata[1], "versioning");
  const list = versions && singleTag(versions, "versions");
  if (!list) throw new ToolchainCatalogFailure("maven_metadata_malformed");
  const values = [...list.matchAll(/<version>([0-9A-Za-z._+-]+)<\/version>/g)].map((match) => match[1]);
  if (!values.length || list.replace(/<version>[0-9A-Za-z._+-]+<\/version>/g, "").replace(/\s/g, "") !== "") {
    throw new ToolchainCatalogFailure("maven_metadata_malformed");
  }
  return values;
}

function singleTag(value, name) {
  const matches = [...value.matchAll(new RegExp(`<${name}>([\\s\\S]*?)<\\/${name}>`, "g"))];
  return matches.length === 1 ? matches[0][1] : undefined;
}

function textTag(value, name) {
  const text = singleTag(value, name);
  return text !== undefined && /^[A-Za-z0-9_.-]+$/.test(text) ? text : undefined;
}

async function fetchMavenSha1(fetcher, artifactUrl) {
  const sidecar = await fetcher.checksum(`${artifactUrl}.sha1`);
  let text;
  try {
    text = decoder.decode(sidecar).trim();
  } catch {
    throw new ToolchainCatalogFailure("upstream_checksum_missing");
  }
  const match = /^([a-f0-9]{40})(?:\s+\*?[^\s]+)?$/i.exec(text);
  if (!match) throw new ToolchainCatalogFailure("upstream_checksum_missing");
  return match[1].toLowerCase();
}

async function resolveMinecraftServer(fetcher, metadata, candidate) {
  const server = metadata.downloads?.server;
  if (!plainObject(server) || typeof server.url !== "string" ||
    !validSha1(server.sha1) || !validPositiveInteger(server.size, MAX_ARTIFACT_BYTES)) {
    throw new ToolchainCatalogFailure("minecraft_metadata_malformed");
  }
  assertTrustedUrl(server.url, ["piston-data.mojang.com"], "upstream_url_rejected");
  return await downloadArtifact(fetcher, {
    role: "dependency",
    coordinate: `com.mojang:minecraft-server:${candidate.minecraftVersion}:server`,
    url: server.url,
    expectedSha1: server.sha1,
    expectedSize: server.size,
  });
}

async function resolveNeoForgeDependencies(fetcher, installerBytes, metadata, candidate) {
    const profile = parseJson(readZipEntry(installerBytes, "install_profile.json"),
      "loader_metadata_malformed");
    const versionPath = typeof profile.json === "string" ? profile.json.replace(/^\//, "") : "";
    const version = parseJson(readZipEntry(installerBytes, versionPath),
      "loader_metadata_malformed");
    if (profile.minecraft !== candidate.minecraftVersion || !Array.isArray(profile.libraries) ||
      version.id !== `neoforge-${candidate.loader.version}` || !Array.isArray(version.libraries)) {
      throw new ToolchainCatalogFailure("loader_metadata_malformed");
    }
    const artifacts = [];
    const keys = new Set();
    for (const library of [...profile.libraries, ...version.libraries]) {
      const download = library?.downloads?.artifact;
      const coordinate = normalizeMavenCoordinate(library?.name);
      if (!coordinate || !plainObject(download) || typeof download.url !== "string" ||
        !validSha1(download.sha1) || !validPositiveInteger(download.size, MAX_ARTIFACT_BYTES)) {
        throw new ToolchainCatalogFailure("loader_metadata_malformed");
      }
      const key = `${coordinate}\0${download.url}`;
      if (keys.has(key)) continue;
      keys.add(key);
      artifacts.push(await downloadArtifact(fetcher, {
        role: "dependency",
        coordinate,
        url: download.url,
        expectedSha1: download.sha1,
        expectedSize: download.size,
      }));
    }
    const mappings = metadata.downloads?.server_mappings;
    const mappingsCoordinate = normalizeMavenCoordinate(
      profile.data?.MOJMAPS?.server?.slice(1, -1),
    );
    if (!mappingsCoordinate || !plainObject(mappings) || typeof mappings.url !== "string" ||
      !validSha1(mappings.sha1) || !validPositiveInteger(mappings.size, MAX_ARTIFACT_BYTES)) {
      throw new ToolchainCatalogFailure("minecraft_metadata_malformed");
    }
    artifacts.push(await downloadArtifact(fetcher, {
      role: "dependency",
      coordinate: mappingsCoordinate,
      url: mappings.url,
      expectedSha1: mappings.sha1,
      expectedSize: mappings.size,
    }));
    return artifacts;
}

async function downloadArtifact(fetcher, {
  role,
  coordinate,
  url,
  expectedSha1,
  expectedSize,
}) {
  if (!validSha1(expectedSha1)) throw new ToolchainCatalogFailure("upstream_checksum_missing");
  const bytes = await fetcher.artifact(url);
  if (expectedSize !== undefined && bytes.byteLength !== expectedSize) {
    throw new ToolchainCatalogFailure("upstream_size_mismatch");
  }
  verifySha1(bytes, expectedSha1, "upstream_checksum_mismatch");
  const artifact = {
    role,
    coordinate,
    url,
    sizeBytes: bytes.byteLength,
    sha256: sha256(bytes),
  };
  Object.defineProperty(artifact, "artifactBytes", { value: bytes });
  return artifact;
}

function validateToolchain(toolchain, catalog, runtime, tupleKeys) {
  if (!plainObject(toolchain) ||
    !sameKeys(toolchain, ["artifacts", "java", "jre", "launchTemplate", "loader", "minecraftVersion"]) ||
    !validMinecraftVersion(toolchain.minecraftVersion) || !validLoader(toolchain.loader) ||
    !validJava(toolchain.java) || !validJre(toolchain.jre) ||
    toolchain.launchTemplate !== loaderTemplates[toolchain.loader.kind] ||
    !Array.isArray(toolchain.artifacts) || toolchain.artifacts.length < 2 ||
    toolchain.artifacts.length > 256) {
    throw new ToolchainCatalogFailure("toolchain_malformed");
  }
  const tuple = candidateKey(toolchain);
  if (tupleKeys.has(tuple)) throw new ToolchainCatalogFailure("duplicate_toolchain");
  tupleKeys.add(tuple);
  const expectedJre = jreFor(runtime, toolchain.java);
  if (JSON.stringify(canonicalize(toolchain.jre)) !== JSON.stringify(canonicalize(expectedJre)) ||
    toolchain.jre.runtimeCatalogRevision !== catalog.runtimeCatalogRevision) {
    throw new ToolchainCatalogFailure("runtime_java_mismatch");
  }
  const expectedPrimary = {
    minecraftVersion: toolchain.minecraftVersion,
    loader: toolchain.loader,
    java: toolchain.java,
  };
  const primary = toolchain.artifacts.filter((artifact) => artifact?.role === "primary");
  const server = toolchain.artifacts.filter((artifact) =>
    artifact?.coordinate === `com.mojang:minecraft-server:${toolchain.minecraftVersion}:server`);
  if (primary.length !== 1 || server.length !== 1 ||
    primary[0].coordinate !== primaryCoordinate(expectedPrimary) ||
    primary[0].url !== primaryArtifactUrl(expectedPrimary) ||
    server[0].coordinate !== `com.mojang:minecraft-server:${toolchain.minecraftVersion}:server`) {
    throw new ToolchainCatalogFailure("required_artifact_missing");
  }
  const ordered = sortArtifacts(toolchain.artifacts);
  if (JSON.stringify(ordered) !== JSON.stringify(toolchain.artifacts)) {
    throw new ToolchainCatalogFailure("toolchain_not_canonical");
  }
  for (const artifact of toolchain.artifacts) validateArtifact(artifact, catalog.approvedArtifactHosts);
  validateMinecraftServerUrl(server[0].url);
  if (toolchain.loader.kind === "neoforge" && toolchain.artifacts.length < 3) {
    throw new ToolchainCatalogFailure("required_artifact_missing");
  }
}

function validateArtifact(artifact, hosts) {
  if (!plainObject(artifact) ||
    !sameKeys(artifact, ["coordinate", "role", "sha256", "sizeBytes", "url"]) ||
    !["primary", "dependency"].includes(artifact.role) || !validCoordinate(artifact.coordinate) ||
    !validSha256(artifact.sha256) || !validPositiveInteger(artifact.sizeBytes, MAX_ARTIFACT_BYTES)) {
    throw new ToolchainCatalogFailure("artifact_malformed");
  }
  assertTrustedUrl(artifact.url, hosts, "artifact_url_rejected");
}

function validateMinecraftServerUrl(value) {
  const url = new URL(value);
  if (url.hostname !== "piston-data.mojang.com" ||
    !/^\/v1\/objects\/[a-f0-9]{40}\/server\.jar$/.test(url.pathname)) {
    throw new ToolchainCatalogFailure("artifact_url_rejected");
  }
}

function primaryCoordinate(candidate) {
  const { minecraftVersion, loader } = candidate;
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
      throw new ToolchainCatalogFailure("candidate_invalid");
  }
}

function validateCandidates(candidates) {
  if (!Array.isArray(candidates) || candidates.length < 1 || candidates.length > 32) {
    throw new ToolchainCatalogFailure("candidate_invalid");
  }
  const keys = new Set();
  for (const candidate of candidates) {
    validateCandidate(candidate);
    const key = candidateKey(candidate);
    if (keys.has(key)) throw new ToolchainCatalogFailure("duplicate_toolchain");
    keys.add(key);
  }
}

function validateCandidate(candidate) {
  if (!plainObject(candidate) ||
    !sameKeys(candidate, ["java", "loader", "minecraftVersion"]) ||
    !validMinecraftVersion(candidate.minecraftVersion) ||
    !validLoader(candidate.loader) || !validJava(candidate.java)) {
    throw new ToolchainCatalogFailure("candidate_invalid");
  }
}

function validJre(value) {
  return plainObject(value) &&
    sameKeys(value, ["component", "id", "major", "runtimeCatalogRevision", "sha256"]) &&
    validIdentifier(value.id) && validIdentifier(value.component) &&
    Number.isSafeInteger(value.major) && value.major > 0 &&
    validSha256(value.sha256) && validSha256(value.runtimeCatalogRevision);
}

function validLoader(value) {
  return plainObject(value) && sameKeys(value, ["kind", "version"]) &&
    Object.hasOwn(loaderTemplates, value.kind) && validVersion(value.version);
}

function validJava(value) {
  return plainObject(value) && validIdentifier(value.component) &&
    Number.isSafeInteger(value.major) && value.major > 0 && value.major <= 255;
}

function validMinecraftVersion(value) {
  return typeof value === "string" &&
    /^(?:1\.(?:0|[1-9]\d{0,2})\.(?:0|[1-9]\d{0,2})|[1-9]\d{1,3}\.(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2}))?)$/.test(value);
}

function validVersion(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128 &&
    /^[0-9A-Za-z][0-9A-Za-z._+-]*$/.test(value);
}

function validCoordinate(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 256 &&
    /^[0-9A-Za-z_.-]+:[0-9A-Za-z_.-]+:[0-9A-Za-z._+-]+(?::[0-9A-Za-z_.-]+)?$/.test(value);
}

function normalizeMavenCoordinate(value) {
  if (typeof value !== "string") return undefined;
  const coordinate = value.replace(/@(jar|zip|txt)$/, "");
  return validCoordinate(coordinate) ? coordinate : undefined;
}

function readZipEntry(bytes, expectedPath) {
  if (!(bytes instanceof Uint8Array) || bytes.byteLength < 22) {
    throw new ToolchainCatalogFailure("loader_metadata_malformed");
  }
  for (let offset = bytes.byteLength - 22; offset >= Math.max(0, bytes.byteLength - 65_557);
    offset -= 1) {
    if (u32(bytes, offset) !== 0x06054b50) continue;
    const entries = u16(bytes, offset + 10);
    let central = u32(bytes, offset + 16);
    for (let index = 0; index < entries; index += 1) {
      if (u32(bytes, central) !== 0x02014b50) break;
      const method = u16(bytes, central + 10);
      const compressedSize = u32(bytes, central + 20);
      const size = u32(bytes, central + 24);
      const nameLength = u16(bytes, central + 28);
      const extraLength = u16(bytes, central + 30);
      const commentLength = u16(bytes, central + 32);
      const local = u32(bytes, central + 42);
      const name = decoder.decode(bytes.subarray(central + 46, central + 46 + nameLength));
      if (name === expectedPath) {
        if (u32(bytes, local) !== 0x04034b50) break;
        const localName = u16(bytes, local + 26);
        const localExtra = u16(bytes, local + 28);
        const start = local + 30 + localName + localExtra;
        const compressed = bytes.subarray(start, start + compressedSize);
        const result = method === 0 ? compressed : method === 8 ? inflateRawSync(compressed) : undefined;
        if (!result || result.byteLength !== size) break;
        return new Uint8Array(result);
      }
      central += 46 + nameLength + extraLength + commentLength;
    }
  }
  throw new ToolchainCatalogFailure("loader_metadata_malformed");
}

function u16(bytes, offset) {
  return bytes[offset] | (bytes[offset + 1] << 8);
}

function u32(bytes, offset) {
  return (u16(bytes, offset) | (u16(bytes, offset + 2) << 16)) >>> 0;
}

function validIdentifier(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128 &&
    /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(value);
}

function validSha1(value) {
  return typeof value === "string" && /^[a-f0-9]{40}$/i.test(value);
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value);
}

function validPositiveInteger(value, maximum) {
  return Number.isSafeInteger(value) && value > 0 && value <= maximum;
}

function assertTrustedUrl(value, hosts, code) {
  if (typeof value !== "string") throw new ToolchainCatalogFailure(code);
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new ToolchainCatalogFailure(code);
  }
  if (url.protocol !== "https:" || url.username || url.password || url.port || url.hash ||
    url.search || !hosts.includes(url.hostname.toLowerCase()) ||
    !safeUrlPath(url.pathname)) {
    throw new ToolchainCatalogFailure(code);
  }
}

function safeUrlPath(pathname) {
  return pathname.startsWith("/") && pathname.length > 1 && !pathname.includes("\\") &&
    !pathname.includes("%") && pathname.split("/").every((segment, index) =>
      index === 0 || (segment.length > 0 && segment !== "." && segment !== ".."),
    );
}

async function readBoundedResponse(response, maxBytes) {
  if (response.body?.getReader) {
    const reader = response.body.getReader();
    const chunks = [];
    let total = 0;
    try {
      while (true) {
        const next = await reader.read();
        if (next.done) break;
        if (!(next.value instanceof Uint8Array)) throw new ToolchainCatalogFailure("upstream_fetch_failed");
        total += next.value.byteLength;
        if (total > maxBytes) throw new ToolchainCatalogFailure("upstream_size_mismatch");
        chunks.push(next.value);
      }
    } finally {
      reader.releaseLock?.();
    }
    return concatenate(chunks, total);
  }
  if (typeof response.arrayBuffer !== "function") throw new ToolchainCatalogFailure("upstream_fetch_failed");
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength > maxBytes) throw new ToolchainCatalogFailure("upstream_size_mismatch");
  return bytes;
}

function parseJson(bytes, code) {
  try {
    const value = JSON.parse(decoder.decode(bytes));
    if (!plainObject(value)) throw new Error();
    return value;
  } catch {
    throw new ToolchainCatalogFailure(code);
  }
}

function verifySha1(bytes, expected, code) {
  if (!validSha1(expected) || createHash("sha1").update(bytes).digest("hex") !== expected.toLowerCase()) {
    throw new ToolchainCatalogFailure(code);
  }
}

function sortArtifacts(artifacts) {
  return [...artifacts].sort((left, right) =>
    compareString(left.coordinate, right.coordinate) || compareString(left.role, right.role),
  );
}

function compareToolchains(left, right) {
  return compareString(left.minecraftVersion, right.minecraftVersion) ||
    compareString(left.loader.kind, right.loader.kind) ||
    compareString(left.loader.version, right.loader.version) ||
    compareString(left.java.component, right.java.component) ||
    left.java.major - right.java.major;
}

function candidateKey(value) {
  return `${value.minecraftVersion}\0${value.loader.kind}\0${value.loader.version}\0` +
    `${value.java.component}\0${value.java.major}`;
}

function javaKey(value) {
  return `${value.component}\0${value.major}`;
}

function sameStringList(actual, expected) {
  return Array.isArray(actual) && actual.length === expected.length &&
    actual.every((value, index) => value === expected[index]);
}

function sameKeys(value, expected) {
  if (!plainObject(value)) return false;
  const keys = Object.keys(value).sort();
  const wanted = [...expected].sort();
  return keys.length === wanted.length && keys.every((key, index) => key === wanted[index]);
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function canonicalize(value) {
  if (value === null || typeof value === "string" || typeof value === "boolean") return value;
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw new ToolchainCatalogFailure("catalog_malformed");
    return value;
  }
  if (Array.isArray(value)) return value.map(canonicalize);
  if (!plainObject(value)) throw new ToolchainCatalogFailure("catalog_malformed");
  return Object.fromEntries(Object.keys(value).sort().map((key) => [key, canonicalize(value[key])]));
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function concatenate(chunks, total) {
  const result = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return result;
}

function compareString(left, right) {
  return left < right ? -1 : left > right ? 1 : 0;
}
