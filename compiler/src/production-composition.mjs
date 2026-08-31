import { readFile, realpath, stat } from "node:fs/promises";
import { resolve, sep } from "node:path";
import { CompilerHttpWorker } from "./http-worker.mjs";
import {
  ReviewedToolchainCatalog,
  StrictArtifactDownloader,
} from "./reviewed-builder.mjs";
import { VerifiedReadOnlyJreRegistry } from "./verified-jre-registry.mjs";
import { BubblewrapSandboxAdapter } from "./bubblewrap-sandbox.mjs";
import {
  FilesystemReplayStore,
  HmacServiceIdentity,
} from "./service-identity.mjs";
import { PRODUCTION_OUTPUT_LIMITS } from "./production-limits.mjs";

const decoder = new TextDecoder("utf-8", { fatal: true });

export const PRODUCTION_PATHS = Object.freeze({
  configuration: "/etc/xmcl-compiler/worker.json",
  catalog: "/opt/xmcl-compiler/app/toolchain-catalog.lock.json",
  jreRegistry: "/etc/xmcl-compiler/jre-registry.json",
  jreRoot: "/opt/xmcl/jres",
  workspaceRoot: "/run/xmcl-compiler/workspaces",
  replayRoot: "/var/lib/xmcl-compiler/replay",
  secretsRoot: "/run/credentials/xmcl-compiler.service",
  bubblewrap: "/usr/bin/bwrap",
  prlimit: "/usr/bin/prlimit",
});

export async function createProductionCompilerWorker({
  paths = PRODUCTION_PATHS,
  fetchImpl = fetch,
  mountInspector,
  sandboxRunner,
} = {}) {
  validatePathSet(paths);
  const config = await loadJson(paths.configuration);
  validateConfiguration(config, paths);
  const catalogDocument = await loadJson(paths.catalog);
  const catalog = new ReviewedToolchainCatalog(catalogDocument, {
    supportedFamilies: ["neoforge"],
  });
  const registry = await VerifiedReadOnlyJreRegistry.load({
    path: paths.jreRegistry,
    catalog,
    rootDirectory: paths.jreRoot,
    mountInspector,
  });
  await registry.initialize();
  const replayStore = new FilesystemReplayStore({
    directory: paths.replayRoot,
    maxEntries: config.identity.maxReplayEntries,
  });
  await replayStore.initialize();
  const secret = await readSecret(config.identity.secretPath, paths.secretsRoot);
  const identity = new HmacServiceIdentity({
    keyId: config.identity.keyId,
    secret,
    nonceStore: replayStore,
    maxAgeMs: config.identity.maxAgeMs,
    requireDurableReplay: true,
  });
  const sandbox = new BubblewrapSandboxAdapter({
    jreRegistry: registry,
    workspaceRoot: paths.workspaceRoot,
    bubblewrapPath: paths.bubblewrap,
    prlimitPath: paths.prlimit,
    timeoutMs: config.sandbox.timeoutMs,
    addressSpaceBytes: config.sandbox.addressSpaceBytes,
    fileSizeBytes: config.sandbox.fileSizeBytes,
    processLimit: config.sandbox.processLimit,
    cpuSeconds: config.sandbox.cpuSeconds,
    runner: sandboxRunner,
  });
  await sandbox.initialize();
  return new CompilerHttpWorker({
    toolchainCatalog: catalog,
    verifiedJres: registry,
    sandboxAdapter: sandbox,
    artifactDownloader: new StrictArtifactDownloader({
      fetchImpl,
      timeoutMs: config.artifactDownloadTimeoutMs,
    }),
    objectRequestTimeoutMs: config.objectRequestTimeoutMs,
    requestAuthenticator: identity,
    callback: {
      controlPlaneOrigin: config.callback.controlPlaneOrigin,
      authenticator: identity,
      fetchImpl,
      timeoutMs: config.callback.timeoutMs,
    },
    fetchImpl,
    maxConcurrentJobs: config.maxConcurrentJobs,
  });
}

async function loadJson(path) {
  try {
    return JSON.parse(decoder.decode(await readFile(path)));
  } catch {
    throw new TypeError(`invalid production configuration: ${path}`);
  }
}

async function readSecret(path, root) {
  try {
    const actual = await realpath(path);
    if (!within(root, actual)) throw new Error("secret escaped configured root");
    const metadata = await stat(actual);
    if (!safeSecretMetadata(metadata)) {
      throw new Error("unsafe secret permissions");
    }
    return await readFile(actual);
  } catch {
    throw new TypeError("invalid production service identity");
  }
}

export function safeSecretMetadata(metadata) {
  if (!metadata?.isFile?.() || metadata.size > 64 * 1024) return false;
  const permissions = metadata.mode & 0o777;
  return permissions === 0o400 ||
    permissions === 0o440 && metadata.uid === 0 && metadata.gid === 0;
}

function validateConfiguration(value, paths) {
  if (!plainObject(value) || !sameKeys(value, [
    "schemaVersion", "callback", "identity", "sandbox",
    "artifactDownloadTimeoutMs", "objectRequestTimeoutMs", "maxConcurrentJobs",
  ]) || value.schemaVersion !== 1 ||
    !plainObject(value.callback) || !sameKeys(value.callback, [
      "controlPlaneOrigin", "timeoutMs",
    ]) || !exactHttpsOrigin(value.callback.controlPlaneOrigin) ||
    !bounded(value.callback.timeoutMs, 1_000, 60_000) ||
    !plainObject(value.identity) || !sameKeys(value.identity, [
      "keyId", "secretPath", "maxAgeMs", "maxReplayEntries",
    ]) || !validIdentifier(value.identity.keyId) ||
    !within(paths.secretsRoot, value.identity.secretPath) ||
    !bounded(value.identity.maxAgeMs, 1_000, 300_000) ||
    !bounded(value.identity.maxReplayEntries, 1, 10_000_000) ||
    !plainObject(value.sandbox) || !sameKeys(value.sandbox, [
      "timeoutMs", "addressSpaceBytes", "fileSizeBytes", "processLimit", "cpuSeconds",
    ]) || !bounded(value.sandbox.timeoutMs, 10_000, 15 * 60 * 1000) ||
    !bounded(value.sandbox.addressSpaceBytes, 512 * 1024 * 1024, 8 * 1024 * 1024 * 1024) ||
    !bounded(value.sandbox.fileSizeBytes, 64 * 1024 * 1024,
      PRODUCTION_OUTPUT_LIMITS.maxFileBytes) ||
    !bounded(value.sandbox.processLimit, 16, 1_024) ||
    !bounded(value.sandbox.cpuSeconds, 10, 600) ||
    !bounded(value.artifactDownloadTimeoutMs, 1, 300_000) ||
    !bounded(value.objectRequestTimeoutMs, 1_000, 900_000) ||
    !bounded(value.maxConcurrentJobs, 1, 16)) {
    throw new TypeError("invalid production configuration");
  }
}

function validatePathSet(paths) {
  if (!plainObject(paths) || !sameKeys(paths, Object.keys(PRODUCTION_PATHS)) ||
    Object.values(paths).some((path) => !absoluteNormalizedPath(path)) ||
    !within(paths.jreRoot, `${paths.jreRoot}${sep}probe`) ||
    !within(paths.secretsRoot, `${paths.secretsRoot}${sep}probe`)) {
    throw new TypeError("invalid production paths");
  }
}

function within(root, value) {
  return absoluteNormalizedPath(root) && absoluteNormalizedPath(value) &&
    resolve(value).startsWith(`${resolve(root)}${sep}`);
}

function exactHttpsOrigin(value) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password && !url.hash &&
      url.pathname === "/" && !url.search;
  } catch {
    return false;
  }
}

function absoluteNormalizedPath(value) {
  return typeof value === "string" && value.length > 1 && value.length <= 1_024 &&
    resolve(value) === value;
}

function validIdentifier(value) {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);
}

function bounded(value, minimum, maximum) {
  return Number.isSafeInteger(value) && value >= minimum && value <= maximum;
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function sameKeys(value, expected) {
  const actual = Object.keys(value).sort();
  const keys = [...expected].sort();
  return actual.length === keys.length &&
    actual.every((key, index) => key === keys[index]);
}
