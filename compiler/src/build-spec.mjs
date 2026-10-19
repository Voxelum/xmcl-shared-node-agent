import { createHash } from "node:crypto";
import { CompilerFailure, safeArchivePath } from "./bundle.mjs";
import { PRODUCTION_OUTPUT_LIMITS } from "./production-limits.mjs";

export const AGENT_BUILDER_SCHEMA = "agent_builder_v1";
export const BUILD_KEY_DOMAIN = "xmcl-prepared-content-build-key-v1\n";
export const BUILD_TARGET = Object.freeze({ architecture: "amd64", os: "linux" });

const dependencyKinds = new Set(["toolchain", "minecraft", "mapping", "library", "mod"]);
const MAX_DEPENDENCY_BYTES = 512 * 1024 * 1024;
const encoder = new TextEncoder();

export function canonicalizeBuildSpec(value) {
  const spec = validateBuildSpecV1(value);
  return JSON.stringify(canonicalObject(spec))
    .replaceAll("\u2028", "\\u2028")
    .replaceAll("\u2029", "\\u2029");
}

export function buildKeyFor(value) {
  return createHash("sha256")
    .update(BUILD_KEY_DOMAIN)
    .update(canonicalizeBuildSpec(value))
    .digest("hex");
}

export function validateBuildSpecV1(value) {
  if (!plainObject(value) || !sameKeys(value, [
    "schemaVersion", "source", "compatibility", "files", "dependencies",
    "builder", "runtime", "target",
  ]) || value.schemaVersion !== 1 ||
    !plainObject(value.source) || !sameKeys(value.source, ["format", "sha256"]) ||
    !["mrpack", "zip"].includes(value.source.format) || !validSha256(value.source.sha256) ||
    !plainObject(value.compatibility) || !sameKeys(value.compatibility, [
      "minecraftVersion", "loader", "java",
    ]) || !validMinecraftVersion(value.compatibility.minecraftVersion) ||
    !validLoader(value.compatibility.loader) || !validJava(value.compatibility.java) ||
    !Array.isArray(value.files) || value.files.length > PRODUCTION_OUTPUT_LIMITS.maxFiles ||
    !Array.isArray(value.dependencies) ||
    value.dependencies.length > PRODUCTION_OUTPUT_LIMITS.maxFiles ||
    !validBuilder(value.builder) || !validRuntime(value.runtime) ||
    !plainObject(value.target) ||
    !sameKeys(value.target, ["architecture", "os"]) ||
    value.target.architecture !== BUILD_TARGET.architecture ||
    value.target.os !== BUILD_TARGET.os) {
    throw new CompilerFailure("invalid_build_spec");
  }

  const files = value.files.map(parseFileIdentity);
  requireSortedUnique(files, fileSortKey);
  const dependencies = value.dependencies.map(parseDependencyIdentity);
  requireSortedUnique(dependencies, dependencySortKey);
  const occupied = new Set(files.map((file) => collisionKey(file.targetPath)));
  const dependencyIds = new Set();
  for (const dependency of dependencies) {
    const key = collisionKey(dependency.targetPath);
    if (occupied.has(key) || dependencyIds.has(dependency.id)) {
      throw new CompilerFailure("invalid_build_spec");
    }
    occupied.add(key);
    dependencyIds.add(dependency.id);
  }
  return deepFreeze({
    schemaVersion: 1,
    source: {
      format: value.source.format,
      sha256: value.source.sha256,
    },
    compatibility: {
      minecraftVersion: value.compatibility.minecraftVersion,
      loader: { ...value.compatibility.loader },
      java: { ...value.compatibility.java },
    },
    files,
    dependencies,
    builder: {
      schema: value.builder.schema,
      image: value.builder.image,
      toolchainCatalogRevision: value.builder.toolchainCatalogRevision,
    },
    runtime: {
      image: value.runtime.image,
      runtimeCatalogRevision: value.runtime.runtimeCatalogRevision,
    },
    target: { ...BUILD_TARGET },
  });
}

function parseFileIdentity(value) {
  if (!plainObject(value) || !sameKeys(value, [
    "path", "sha256", "sizeBytes", "targetPath",
  ]) || !safeArchivePath(value.path) || !safeBuildTargetPath(value.targetPath) ||
    !validSha256(value.sha256) || !validSize(value.sizeBytes, true)) {
    throw new CompilerFailure("invalid_build_spec");
  }
  return {
    path: value.path,
    sha256: value.sha256,
    sizeBytes: value.sizeBytes,
    targetPath: value.targetPath,
  };
}

function parseDependencyIdentity(value) {
  if (!plainObject(value) || !sameKeys(value, [
    "id", "kind", "sha256", "sizeBytes", "targetPath",
  ]) || !dependencyKinds.has(value.kind) || !validDependencyId(value.id, value.kind) ||
    !safeBuildTargetPath(value.targetPath) ||
    !validSha256(value.sha256) || !validSize(value.sizeBytes, false)) {
    throw new CompilerFailure("invalid_build_spec");
  }
  return {
    id: value.id,
    kind: value.kind,
    sha256: value.sha256,
    sizeBytes: value.sizeBytes,
    targetPath: value.targetPath,
  };
}

function validBuilder(value) {
  return plainObject(value) && sameKeys(value, [
    "image", "schema", "toolchainCatalogRevision",
  ]) && value.schema === AGENT_BUILDER_SCHEMA &&
    validImage(value.image, "ghcr.io/voxelum/xmcl-shared-node-builder") &&
    validSha256(value.toolchainCatalogRevision);
}

function validRuntime(value) {
  return plainObject(value) && sameKeys(value, [
    "image", "runtimeCatalogRevision",
  ]) && validImage(value.image, "ghcr.io/voxelum/xmcl-shared-minecraft-runtime") &&
    validSha256(value.runtimeCatalogRevision);
}

function validLoader(value) {
  return plainObject(value) && sameKeys(value, ["kind", "version"]) &&
    ["fabric", "forge", "neoforge", "quilt"].includes(value.kind) &&
    validVersion(value.version);
}

function validJava(value) {
  return plainObject(value) && sameKeys(value, ["component", "major"]) &&
    validVersion(value.component) && Number.isSafeInteger(value.major) &&
    value.major >= 8 && value.major <= 99;
}

function validMinecraftVersion(value) {
  return typeof value === "string" &&
    /^(?:1\.(?:0|[1-9]\d{0,2})\.(?:0|[1-9]\d{0,2})|[1-9]\d{1,3}\.(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2}))?)$/.test(value);
}

function safeBuildTargetPath(value) {
  if (!safeArchivePath(value) || value.length > 512) return false;
  const lower = value.toLowerCase();
  return !lower.startsWith(".xmcl/") && !lower.startsWith("world/") &&
    !/\.(?:sh|bat|cmd)$/.test(lower);
}

function validVersion(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128 &&
    /^[0-9A-Za-z][0-9A-Za-z._+-]*$/.test(value);
}

function validDependencyId(value, kind) {
  if (typeof value !== "string" || value.length === 0 || value.length > 512 ||
    !/^[0-9A-Za-z][0-9A-Za-z._:+@/-]*$/.test(value)) return false;
  if (kind === "mod") {
    return value.startsWith("modrinth:") || value.startsWith("curseforge:");
  }
  return !value.includes("/") && value.split(":").length >= 3;
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
}

function validImage(value, repository) {
  return typeof value === "string" &&
    new RegExp(`^${repository.replaceAll(".", "\\.")}@sha256:[a-f0-9]{64}$`).test(value);
}

function validSize(value, allowEmpty) {
  return Number.isSafeInteger(value) && value >= (allowEmpty ? 0 : 1) &&
    value <= MAX_DEPENDENCY_BYTES;
}

function requireSortedUnique(values, keyFor) {
  let previous;
  for (const value of values) {
    const key = keyFor(value);
    if (previous !== undefined && compareUtf8(previous, key) >= 0) {
      throw new CompilerFailure("invalid_build_spec");
    }
    previous = key;
  }
}

export function compareUtf8(left, right) {
  return compareBytes(encoder.encode(left), encoder.encode(right));
}

function compareBytes(left, right) {
  const length = Math.min(left.byteLength, right.byteLength);
  for (let index = 0; index < length; index += 1) {
    if (left[index] !== right[index]) return left[index] < right[index] ? -1 : 1;
  }
  return left.byteLength < right.byteLength ? -1 :
    left.byteLength > right.byteLength ? 1 : 0;
}

function fileSortKey(value) {
  return `${value.targetPath}\0${value.path}`;
}

function dependencySortKey(value) {
  return `${value.kind}\0${value.id}\0${value.targetPath}`;
}

function collisionKey(value) {
  return value.normalize("NFKC").toLowerCase();
}

function canonicalObject(value) {
  if (Array.isArray(value)) return value.map(canonicalObject);
  if (!plainObject(value)) return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) =>
    [key, canonicalObject(value[key])]));
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.getPrototypeOf(value) === Object.prototype;
}

function sameKeys(value, expected) {
  const actual = Object.keys(value).sort();
  const keys = [...expected].sort();
  return actual.length === keys.length &&
    actual.every((key, index) => key === keys[index]);
}

function deepFreeze(value) {
  Object.freeze(value);
  for (const child of Object.values(value)) {
    if (child && typeof child === "object" && !Object.isFrozen(child)) deepFreeze(child);
  }
  return value;
}
