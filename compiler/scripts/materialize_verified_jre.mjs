#!/usr/bin/env node
import { createHash, randomBytes } from "node:crypto";
import {
  chmod,
  lstat,
  mkdir,
  readFile,
  rename,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { dirname, resolve, sep } from "node:path";
import { pathToFileURL } from "node:url";
import {
  ReviewedToolchainCatalog,
} from "../src/reviewed-builder.mjs";
import { canonicalJsonBytes } from "../src/toolchain-catalog.mjs";

const MAX_FILE_BYTES = 512 * 1024 * 1024;
const MAX_TOTAL_BYTES = 2 * 1024 * 1024 * 1024;
const decoder = new TextDecoder("utf-8", { fatal: true });

export async function materializeVerifiedJre({
  runtimeCatalogPath,
  toolchainCatalogPath,
  outputRoot,
  jreId = "java-runtime-delta-21",
  fetchImpl = fetch,
} = {}) {
  if (!absolutePath(runtimeCatalogPath) || !absolutePath(toolchainCatalogPath) ||
    !absolutePath(outputRoot) || jreId !== "java-runtime-delta-21" ||
    typeof fetchImpl !== "function") {
    throw new MaterializerFailure("invalid_arguments");
  }
  const runtimeBytes = new Uint8Array(await readFile(runtimeCatalogPath));
  const runtime = parseJson(runtimeBytes, "runtime_catalog_malformed");
  const catalogDocument = parseJson(new Uint8Array(await readFile(toolchainCatalogPath)),
    "toolchain_catalog_malformed");
  const catalog = new ReviewedToolchainCatalog(catalogDocument, {
    supportedFamilies: ["neoforge"],
  });
  if (sha256(runtimeBytes) !== catalog.runtimeCatalogRevision ||
    runtime.platform?.os !== "linux" || runtime.platform?.architecture !== "x64" ||
    !Array.isArray(runtime.resolutions)) {
    throw new MaterializerFailure("runtime_catalog_mismatch");
  }
  const toolchains = catalog.toolchains.filter((toolchain) => toolchain.jre.id === jreId);
  if (toolchains.length !== 1) throw new MaterializerFailure("jre_not_supported");
  const expected = toolchains[0].jre;
  const resolutions = runtime.resolutions.filter((resolution) =>
    resolution?.component === expected.component && resolution?.major === expected.major);
  if (resolutions.length !== 1) throw new MaterializerFailure("jre_resolution_missing");
  const resolution = resolutions[0];
  if (sha256(canonicalJsonBytes(resolution)) !== expected.sha256) {
    throw new MaterializerFailure("jre_resolution_digest_mismatch");
  }
  const entries = validateResolution(resolution);
  await assertMissing(outputRoot);
  await mkdir(dirname(outputRoot), { recursive: true, mode: 0o755 });
  const staging = `${outputRoot}.partial-${process.pid}-${randomBytes(8).toString("hex")}`;
  await mkdir(staging, { mode: 0o700 });
  try {
    for (const entry of entries.filter((candidate) => candidate.type === "directory")) {
      await mkdir(targetPath(staging, entry.path), { recursive: true, mode: 0o755 });
    }
    let total = 0;
    for (const entry of entries.filter((candidate) => candidate.type === "file")) {
      total += entry.size;
      if (total > MAX_TOTAL_BYTES) throw new MaterializerFailure("jre_too_large");
      const bytes = await downloadExact(fetchImpl, entry);
      const target = targetPath(staging, entry.path);
      await mkdir(dirname(target), { recursive: true, mode: 0o755 });
      await writeFile(target, bytes, { flag: "wx", mode: entry.executable ? 0o755 : 0o644 });
      await chmod(target, entry.executable ? 0o755 : 0o644);
    }
    for (const entry of entries.filter((candidate) => candidate.type === "link")) {
      const target = targetPath(staging, entry.path);
      await mkdir(dirname(target), { recursive: true, mode: 0o755 });
      if (!linkStaysWithinRoot(staging, target, entry.target)) {
        throw new MaterializerFailure("jre_link_rejected");
      }
      await symlink(entry.target, target);
    }
    const manifest = `${JSON.stringify(canonicalize(resolution), null, 2)}\n`;
    await writeFile(`${staging}${sep}.xmcl-runtime-resolution.json`, manifest, {
      flag: "wx",
      mode: 0o644,
    });
    if (sha256(canonicalJsonBytes(JSON.parse(manifest))) !== expected.sha256) {
      throw new MaterializerFailure("jre_resolution_digest_mismatch");
    }
    await chmod(staging, 0o755);
    await rename(staging, outputRoot);
    return {
      id: expected.id,
      component: expected.component,
      major: expected.major,
      sha256: expected.sha256,
      runtimeCatalogRevision: expected.runtimeCatalogRevision,
      files: entries.filter((entry) => entry.type === "file").length,
      sizeBytes: total,
      outputRoot,
    };
  } catch (error) {
    await rm(staging, { recursive: true, force: true }).catch(() => undefined);
    if (error instanceof MaterializerFailure) throw error;
    const failure = new MaterializerFailure("jre_materialization_failed");
    failure.cause = error;
    throw failure;
  }
}

class MaterializerFailure extends Error {
  constructor(code) {
    super(code);
    this.name = "MaterializerFailure";
    this.code = code;
  }
}

function validateResolution(resolution) {
  if (!plainObject(resolution) || !Array.isArray(resolution.files) ||
    resolution.files.length < 1 || resolution.files.length > 16_384) {
    throw new MaterializerFailure("jre_resolution_malformed");
  }
  const paths = new Map();
  for (const entry of resolution.files) {
    if (!plainObject(entry) || !safePath(entry.path) || paths.has(entry.path) ||
      !["directory", "file", "link"].includes(entry.type)) {
      throw new MaterializerFailure("jre_resolution_malformed");
    }
    if (entry.type === "directory" && !sameKeys(entry, ["path", "type"])) {
      throw new MaterializerFailure("jre_resolution_malformed");
    }
    if (entry.type === "link" &&
      (!sameKeys(entry, ["path", "target", "type"]) || !safeLinkTarget(entry.target))) {
      throw new MaterializerFailure("jre_resolution_malformed");
    }
    if (entry.type === "file" &&
      (!sameKeys(entry, ["executable", "hashes", "path", "size", "type", "url"]) ||
        typeof entry.executable !== "boolean" || !plainObject(entry.hashes) ||
        !sameKeys(entry.hashes, ["sha1"]) || !/^[a-f0-9]{40}$/.test(entry.hashes.sha1) ||
        !Number.isSafeInteger(entry.size) || entry.size < 1 || entry.size > MAX_FILE_BYTES ||
        !exactMojangObjectUrl(entry.url, entry.hashes.sha1))) {
      throw new MaterializerFailure("jre_resolution_malformed");
    }
    paths.set(entry.path, entry.type);
  }
  for (const entry of resolution.files) {
    const parts = entry.path.split("/");
    for (let index = 1; index < parts.length; index += 1) {
      if (paths.get(parts.slice(0, index).join("/")) !== "directory") {
        throw new MaterializerFailure("jre_resolution_malformed");
      }
    }
  }
  return resolution.files;
}

async function downloadExact(fetchImpl, entry) {
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 60_000);
  try {
    const response = await fetchImpl(entry.url, {
      method: "GET",
      redirect: "error",
      credentials: "omit",
      referrerPolicy: "no-referrer",
      headers: { "accept-encoding": "identity" },
      signal: controller.signal,
    });
    if (!response || response.status !== 200 || response.redirected ||
      response.url && new URL(response.url).href !== entry.url ||
      response.headers?.get("content-encoding") &&
        response.headers.get("content-encoding").toLowerCase() !== "identity" ||
      response.headers?.get("content-length") !== String(entry.size)) {
      throw new MaterializerFailure("jre_download_failed");
    }
    const bytes = await readExactBody(response, entry.size);
    if (createHash("sha1").update(bytes).digest("hex") !== entry.hashes.sha1) {
      throw new MaterializerFailure("jre_artifact_mismatch");
    }
    return bytes;
  } catch (error) {
    if (error instanceof MaterializerFailure) throw error;
    throw new MaterializerFailure("jre_download_failed");
  } finally {
    clearTimeout(timeout);
  }
}

async function readExactBody(response, expectedSize) {
  if (!response.body?.getReader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength !== expectedSize) {
      throw new MaterializerFailure("jre_artifact_mismatch");
    }
    return bytes;
  }
  const reader = response.body.getReader();
  const bytes = new Uint8Array(expectedSize);
  let offset = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      if (!(next.value instanceof Uint8Array) ||
        offset + next.value.byteLength > expectedSize) {
        throw new MaterializerFailure("jre_artifact_mismatch");
      }
      bytes.set(next.value, offset);
      offset += next.value.byteLength;
    }
  } finally {
    reader.releaseLock?.();
  }
  if (offset !== expectedSize) throw new MaterializerFailure("jre_artifact_mismatch");
  return bytes;
}

async function assertMissing(path) {
  try {
    await lstat(path);
  } catch (error) {
    if (error?.code === "ENOENT") return;
    throw error;
  }
  throw new MaterializerFailure("output_already_exists");
}

function targetPath(root, relative) {
  const target = resolve(root, ...relative.split("/"));
  if (!target.startsWith(`${resolve(root)}${sep}`)) {
    throw new MaterializerFailure("jre_path_rejected");
  }
  return target;
}

function linkStaysWithinRoot(root, path, target) {
  return resolve(dirname(path), target).startsWith(`${resolve(root)}${sep}`);
}

function exactMojangObjectUrl(value, sha1) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password &&
      !url.port && !url.search && !url.hash &&
      url.hostname === "piston-data.mojang.com" &&
      url.pathname.startsWith(`/v1/objects/${sha1}/`) &&
      /^\/v1\/objects\/[a-f0-9]{40}\/[^/]+$/.test(url.pathname);
  } catch {
    return false;
  }
}

function safePath(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 512 &&
    !value.includes("\\") && !value.startsWith("/") &&
    value.split("/").every((part) => part && part !== "." && part !== "..");
}

function safeLinkTarget(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 512 &&
    !value.includes("\\") && !value.startsWith("/") && !/[\x00-\x1f\x7f]/.test(value);
}

function absolutePath(value) {
  return typeof value === "string" && value.length > 1 && value.length <= 1_024 &&
    resolve(value) === value;
}

function parseJson(bytes, code) {
  try {
    const value = JSON.parse(decoder.decode(bytes));
    if (!plainObject(value)) throw new Error();
    return value;
  } catch {
    throw new MaterializerFailure(code);
  }
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (!plainObject(value)) return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) =>
    [key, canonicalize(value[key])]));
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
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

function parseArguments(arguments_) {
  const values = {};
  for (let index = 0; index < arguments_.length; index += 2) {
    const name = arguments_[index];
    const value = arguments_[index + 1];
    if (!value || Object.hasOwn(values, name) ||
      !["--runtime-catalog-lock", "--toolchain-catalog-lock",
        "--output-root", "--jre-id"].includes(name)) {
      throw new MaterializerFailure("invalid_arguments");
    }
    values[name] = value;
  }
  if (!values["--runtime-catalog-lock"] || !values["--toolchain-catalog-lock"] ||
    !values["--output-root"]) {
    throw new MaterializerFailure("invalid_arguments");
  }
  return {
    runtimeCatalogPath: resolve(values["--runtime-catalog-lock"] ?? ""),
    toolchainCatalogPath: resolve(values["--toolchain-catalog-lock"] ?? ""),
    outputRoot: resolve(values["--output-root"] ?? ""),
    jreId: values["--jre-id"] ?? "java-runtime-delta-21",
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  materializeVerifiedJre(parseArguments(process.argv.slice(2))).then((result) => {
    process.stdout.write(`${JSON.stringify(result)}\n`);
  }).catch((error) => {
    process.stderr.write(`verified JRE materialization failed: ${error.code ?? error.message}\n`);
    process.exitCode = 1;
  });
}
