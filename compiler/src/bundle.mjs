import { createHash } from "node:crypto";
import { inflateRawSync } from "node:zlib";
import { PRODUCTION_OUTPUT_LIMITS } from "./production-limits.mjs";

export const MAX_BUNDLE_BYTES = PRODUCTION_OUTPUT_LIMITS.maxBundleArchiveBytes;
export const MAX_BUNDLE_ENTRIES = 4096;
export const MAX_BUNDLE_LOGICAL_BYTES = PRODUCTION_OUTPUT_LIMITS.maxPackageInputBytes;
const MAX_ENTRY_BYTES = 64 * 1024 * 1024;
const decoder = new TextDecoder("utf-8", { fatal: true });
const requiredResolvedFiles = new Set([
  "resolved/version.json",
  "resolved/loader.json",
  "resolved/artifacts.json",
  "resolved/mods.json",
]);

/**
 * Re-validates an input archive without trusting the launcher's manifest or
 * control-plane validation. It does not extract to disk and never invokes a
 * user supplied executable.
 */
export async function validateBundle(archive, frozenManifest) {
  if (!(archive instanceof Uint8Array) || archive.byteLength > MAX_BUNDLE_BYTES) {
    throw new CompilerFailure("bundle_too_large");
  }
  const entries = readZip(archive);
  const manifestEntry = entries.get("bundle.json");
  if (!manifestEntry || entries.size > MAX_BUNDLE_ENTRIES) {
    throw new CompilerFailure("invalid_bundle");
  }
  const manifest = parseManifest(manifestEntry.bytes);
  verifyCompatibility(manifest, frozenManifest);
  const listed = new Map(manifest.files.map((file) => [file.path, file]));
  for (const [path, entry] of entries) {
    if (path === "bundle.json") continue;
    const expected = listed.get(path);
    if (!expected || !allowedPath(path) || expected.sizeBytes !== entry.bytes.byteLength ||
      expected.sha256 !== sha256(entry.bytes)) {
      throw new CompilerFailure("bundle_hash_or_path_mismatch");
    }
  }
  if (listed.size !== entries.size - 1) throw new CompilerFailure("bundle_manifest_mismatch");
  for (const required of requiredResolvedFiles) {
    if (!entries.has(required)) throw new CompilerFailure("missing_resolved_metadata");
  }
  verifyResolvedMetadata(entries, manifest);
  verifyResolvedMods(entries, manifest, frozenManifest);
  return { manifest, entries };
}

export class CompilerFailure extends Error {
  constructor(code) {
    super(code);
    this.code = code;
    this.name = "CompilerFailure";
  }
}

function parseManifest(bytes) {
  let value;
  try {
    value = JSON.parse(decoder.decode(bytes));
  } catch {
    throw new CompilerFailure("invalid_bundle_manifest");
  }
  if (!plainObject(value) || value.schemaVersion !== 1 ||
    typeof value.instanceName !== "string" || value.instanceName.length < 1 ||
    !validMinecraftVersion(value.minecraftVersion) ||
    !plainObject(value.loader) || !plainObject(value.javaRequirement) ||
    !plainObject(value.runtimeCatalog) || !Array.isArray(value.files) ||
    Object.keys(value).some((key) => ![
      "schemaVersion", "instanceName", "minecraftVersion", "loader",
      "javaRequirement", "runtimeCatalog", "files",
    ].includes(key)) ||
    !["forge", "fabric", "neoforge", "quilt"].includes(value.loader.kind) ||
    !validLoaderVersion(value.loader.version) ||
    typeof value.javaRequirement.component !== "string" ||
    !Number.isSafeInteger(value.javaRequirement.major) ||
    !validSha256(value.runtimeCatalog.sha256)
  ) throw new CompilerFailure("invalid_bundle_manifest");

  const files = [];
  let previous = "";
  for (const file of value.files) {
    if (!plainObject(file) || Object.keys(file).some((key) =>
      !["path", "sha256", "sizeBytes"].includes(key)
    ) || !allowedPath(file.path) || !validSha256(file.sha256) ||
      !Number.isSafeInteger(file.sizeBytes) || file.sizeBytes < 0 ||
      (previous && comparePath(previous, file.path) >= 0)) {
      throw new CompilerFailure("invalid_bundle_manifest");
    }
    previous = file.path;
    files.push({ path: file.path, sha256: file.sha256.toLowerCase(), sizeBytes: file.sizeBytes });
  }
  return {
    schemaVersion: 1,
    instanceName: value.instanceName,
    minecraftVersion: value.minecraftVersion,
    loader: { kind: value.loader.kind, version: value.loader.version },
    javaRequirement: {
      component: value.javaRequirement.component,
      major: value.javaRequirement.major,
    },
    runtimeCatalog: { sha256: value.runtimeCatalog.sha256.toLowerCase() },
    files,
  };
}

function verifyCompatibility(bundle, frozen) {
  const compatibility = frozen?.compatibility;
  if (!plainObject(frozen) || frozen.schemaVersion !== 1 || !plainObject(compatibility) ||
    bundle.minecraftVersion !== compatibility.minecraftVersion ||
    bundle.loader.kind !== compatibility.loader ||
    bundle.loader.version !== compatibility.loaderVersion ||
    bundle.runtimeCatalog.sha256 !== compatibility.runtimeCatalog?.sha256 ||
    (compatibility.java && (
      bundle.javaRequirement.component !== compatibility.java.component ||
      bundle.javaRequirement.major !== compatibility.java.major
    ))
  ) {
    throw new CompilerFailure("frozen_compatibility_mismatch");
  }
}

function verifyResolvedMetadata(entries, manifest) {
  try {
    const loader = JSON.parse(decoder.decode(entries.get("resolved/loader.json").bytes));
    const version = JSON.parse(decoder.decode(entries.get("resolved/version.json").bytes));
    const artifacts = JSON.parse(decoder.decode(entries.get("resolved/artifacts.json").bytes));
    if (
      loader.minecraftVersion !== manifest.minecraftVersion ||
      loader.loader?.kind !== manifest.loader.kind ||
      loader.loader?.version !== manifest.loader.version ||
      loader.javaRequirement?.component !== manifest.javaRequirement.component ||
      loader.javaRequirement?.major !== manifest.javaRequirement.major ||
      loader.runtimeCatalog?.sha256 !== manifest.runtimeCatalog.sha256 ||
      version.minecraftVersion !== manifest.minecraftVersion ||
      version.javaVersion?.component !== manifest.javaRequirement.component ||
      version.javaVersion?.majorVersion !== manifest.javaRequirement.major ||
      !validArtifactMetadata(artifacts, manifest)
    ) throw new Error();
  } catch {
    throw new CompilerFailure("resolved_metadata_mismatch");
  }
}

function verifyResolvedMods(entries, manifest, frozenManifest) {
    let declarations;
    try {
      declarations = JSON.parse(decoder.decode(entries.get("resolved/mods.json").bytes));
    } catch {
      throw new CompilerFailure("resolved_mods_mismatch");
    }
    if (!Array.isArray(declarations) || !Array.isArray(frozenManifest?.mods)) {
      throw new CompilerFailure("resolved_mods_mismatch");
    }
    const local = new Map(manifest.files
      .filter((file) => file.path.startsWith("instance/mods/"))
      .map((file) => [file.path, file]));
    const remote = new Map();
    for (const mod of frozenManifest.mods) {
      validateFrozenRemoteMod(mod);
      const path = `instance/mods/${mod.filename}`;
      if (remote.has(path) || local.has(path)) {
        throw new CompilerFailure("remote_mod_collision");
      }
      remote.set(path, mod);
    }
    const seen = new Set();
    for (const declaration of declarations) {
      if (!plainObject(declaration) || typeof declaration.path !== "string" ||
        seen.has(declaration.path) || !validSha256(declaration.sha256) ||
        !Number.isSafeInteger(declaration.sizeBytes) || declaration.sizeBytes < 1) {
        throw new CompilerFailure("resolved_mods_mismatch");
      }
      seen.add(declaration.path);
      if (declaration.source === undefined) {
        if (!sameKeys(declaration, ["path", "sha256", "sizeBytes"])) {
          throw new CompilerFailure("resolved_mods_mismatch");
        }
        const expected = local.get(declaration.path);
        if (!expected || expected.sha256 !== declaration.sha256.toLowerCase() ||
          expected.sizeBytes !== declaration.sizeBytes) {
          throw new CompilerFailure("resolved_mods_mismatch");
        }
        continue;
      }
      if (!sameKeys(declaration, ["filename", "path", "sha256", "sizeBytes", "source"]) ||
        declaration.path !== `instance/mods/${declaration.filename}` ||
        !safeModFilename(declaration.filename) || !plainObject(declaration.source)) {
        throw new CompilerFailure("resolved_mods_mismatch");
      }
      const expected = remote.get(declaration.path);
      const source = declaration.source;
      const matchesSource = expected?.provider === "modrinth"
        ? sameKeys(source, ["projectId", "provider", "versionId"]) &&
          source.provider === "modrinth" &&
          source.projectId === expected.projectId &&
          source.versionId === expected.fileId
        : expected?.provider === "curseforge"
          ? sameKeys(source, ["fileId", "projectId", "provider"]) &&
            source.provider === "curseforge" &&
            String(source.projectId) === expected.projectId &&
            String(source.fileId) === expected.fileId
          : false;
      if (!expected || !matchesSource || expected.filename !== declaration.filename ||
        expected.sha256 !== declaration.sha256.toLowerCase() ||
        expected.sizeBytes !== declaration.sizeBytes) {
        throw new CompilerFailure("resolved_mods_mismatch");
      }
    }
    if (seen.size !== local.size + remote.size ||
      [...local.keys(), ...remote.keys()].some((path) => !seen.has(path))) {
      throw new CompilerFailure("resolved_mods_mismatch");
    }
  }

function validateFrozenRemoteMod(mod) {
    if (!plainObject(mod) ||
      !sameKeys(mod, [
        "downloadUrl", "fileId", "filename", "projectId", "provider", "sha256",
        "sizeBytes",
      ]) ||
      !["modrinth", "curseforge"].includes(mod.provider) ||
      typeof mod.projectId !== "string" || !mod.projectId ||
      typeof mod.fileId !== "string" || !mod.fileId ||
      !safeModFilename(mod.filename) || !validSha256(mod.sha256) ||
      !Number.isSafeInteger(mod.sizeBytes) || mod.sizeBytes < 1 ||
      typeof mod.downloadUrl !== "string") {
      throw new CompilerFailure("invalid_remote_mod");
    }
  }

function safeModFilename(value) {
    return typeof value === "string" && value.length > 0 && value.length <= 255 &&
      !value.includes("/") && !value.includes("\\") && !value.includes("\0") &&
      value.toLowerCase().endsWith(".jar");
}

function validArtifactMetadata(value, manifest) {
  if (!plainObject(value) || value.schemaVersion !== 1 ||
    Object.keys(value).some((key) => key !== "schemaVersion" && key !== "artifacts") ||
    !Array.isArray(value.artifacts)
  ) return false;
  const expected = manifest.files.filter((file) => file.path.startsWith("instance/"));
  return expected.length === value.artifacts.length && expected.every((file, index) => {
    const artifact = value.artifacts[index];
    return plainObject(artifact) &&
      Object.keys(artifact).every((key) =>
        ["intent", "path", "sha256", "sizeBytes"].includes(key)
      ) &&
      artifact.path === file.path &&
      artifact.sha256 === file.sha256 &&
      artifact.sizeBytes === file.sizeBytes &&
      artifact.intent === artifactIntent(file.path);
  });
}

function artifactIntent(path) {
  if (path.startsWith("instance/mods/")) return "mod";
  if (path.startsWith("instance/config/") || path.startsWith("instance/defaultconfigs/")) return "config";
  if (path.startsWith("instance/kubejs/")) return "kubejs";
  if (path.startsWith("instance/scripts/")) return "script";
  if (path.startsWith("instance/datapacks/")) return "datapack";
  if (path.startsWith("instance/global_packs/")) return "global-pack";
  if (path.startsWith("instance/openloader/")) return "openloader";
  if (path.startsWith("instance/paxi/")) return "paxi";
  if (path.startsWith("instance/resourcepacks/")) return "resourcepack";
  return "data";
}

function readZip(archive) {
  if (archive.byteLength < 22) throw new CompilerFailure("invalid_bundle");
  const view = new DataView(archive.buffer, archive.byteOffset, archive.byteLength);
  let eocd = -1;
  for (let offset = archive.byteLength - 22; offset >= Math.max(0, archive.byteLength - 65_557); offset -= 1) {
    if (u32(view, offset) === 0x06054b50) {
      eocd = offset;
      break;
    }
  }
  if (eocd < 0 || u16(view, eocd + 4) !== 0 || u16(view, eocd + 6) !== 0) {
    throw new CompilerFailure("invalid_bundle");
  }
  const count = u16(view, eocd + 10);
  const centralSize = u32(view, eocd + 12);
  const centralOffset = u32(view, eocd + 16);
  if (count > MAX_BUNDLE_ENTRIES || centralOffset + centralSize > eocd) {
    throw new CompilerFailure("invalid_bundle");
  }
  const entries = new Map();
  let cursor = centralOffset;
  let logicalBytes = 0;
  for (let index = 0; index < count; index += 1) {
    if (cursor + 46 > archive.byteLength || u32(view, cursor) !== 0x02014b50) {
      throw new CompilerFailure("invalid_bundle");
    }
    const flags = u16(view, cursor + 8);
    const method = u16(view, cursor + 10);
    const expectedCrc = u32(view, cursor + 16);
    const compressedSize = u32(view, cursor + 20);
    const size = u32(view, cursor + 24);
    const nameLength = u16(view, cursor + 28);
    const extraLength = u16(view, cursor + 30);
    const commentLength = u16(view, cursor + 32);
    const localOffset = u32(view, cursor + 42);
    const end = cursor + 46 + nameLength + extraLength + commentLength;
    logicalBytes += size;
    if (
      end > archive.byteLength || (flags & 1) || (method !== 0 && method !== 8) ||
      size > MAX_ENTRY_BYTES || logicalBytes > MAX_BUNDLE_LOGICAL_BYTES ||
      (size > 0 && (compressedSize === 0 || size / compressedSize > 100))
    ) {
      throw new CompilerFailure("invalid_bundle");
    }
    let path;
    try {
      path = decoder.decode(archive.subarray(cursor + 46, cursor + 46 + nameLength));
    } catch {
      throw new CompilerFailure("invalid_bundle");
    }
    if (!safeZipPath(path) || entries.has(path) || localOffset + 30 > centralOffset ||
      u32(view, localOffset) !== 0x04034b50) {
      throw new CompilerFailure("invalid_bundle");
    }
    const localNameLength = u16(view, localOffset + 26);
    const localExtraLength = u16(view, localOffset + 28);
    const dataOffset = localOffset + 30 + localNameLength + localExtraLength;
    if (dataOffset + compressedSize > centralOffset) throw new CompilerFailure("invalid_bundle");
    const compressed = archive.subarray(dataOffset, dataOffset + compressedSize);
    let bytes;
    try {
      bytes = method === 0 ? compressed.slice() : inflateRawSync(compressed);
    } catch {
      throw new CompilerFailure("invalid_bundle");
    }
    if (bytes.byteLength !== size || crc32(bytes) !== expectedCrc) throw new CompilerFailure("invalid_bundle");
    entries.set(path, { bytes });
    cursor = end;
  }
  if (cursor !== centralOffset + centralSize) throw new CompilerFailure("invalid_bundle");
  return entries;
}

function allowedPath(path) {
  if (!safeZipPath(path) || /(?:^|\/)(?:server|start)\.(?:sh|bat|cmd)$/i.test(path)) return false;
  return path === "resolved/version.json" || path === "resolved/loader.json" ||
    path === "resolved/artifacts.json" || path === "resolved/mods.json" ||
    path === "instance/server.properties" || path === "instance/pack.toml" ||
    path === "instance/pack.mcmeta" || path === "instance/server-icon.png" ||
    [
      "instance/mods/", "instance/config/", "instance/defaultconfigs/",
      "instance/kubejs/", "instance/scripts/", "instance/datapacks/",
      "instance/global_packs/", "instance/openloader/", "instance/paxi/",
      "instance/resourcepacks/",
    ].some((prefix) => path.startsWith(prefix));
}

function safeZipPath(path) {
  return typeof path === "string" && path.length > 0 && path.length <= 1024 &&
    !path.includes("\\") && !path.startsWith("/") && !/^[A-Za-z]:/.test(path) &&
    path.split("/").every((part) => part && part !== "." && part !== "..");
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value);
}

function validLoaderVersion(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 128 &&
    /^[0-9A-Za-z][0-9A-Za-z._+-]*$/.test(value);
}

function validMinecraftVersion(value) {
  return typeof value === "string" &&
    /^(?:1\.(?:0|[1-9]\d{0,2})\.(?:0|[1-9]\d{0,2})|[1-9]\d{1,3}\.(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2}))?)$/.test(value);
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function sameKeys(value, expected) {
  const keys = Object.keys(value).sort();
  const sorted = [...expected].sort();
  return keys.length === sorted.length &&
    keys.every((key, index) => key === sorted[index]);
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function u16(view, offset) {
  return view.getUint16(offset, true);
}

function u32(view, offset) {
  return view.getUint32(offset, true);
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

function comparePath(left, right) {
  return left < right ? -1 : left > right ? 1 : 0;
}
