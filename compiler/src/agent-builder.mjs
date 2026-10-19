import { createHash } from "node:crypto";
import { constants } from "node:fs";
import {
  access,
  lstat,
  mkdir,
  readFile,
  readdir,
  realpath,
  rename,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { basename, resolve, sep } from "node:path";
import {
  CompilerFailure,
  MAX_BUNDLE_BYTES,
  readStrictZip,
} from "./bundle.mjs";
import {
  buildKeyFor,
  validateBuildSpecV1,
} from "./build-spec.mjs";
import {
  ReviewedRuntimeBuilder,
  ReviewedToolchainCatalog,
} from "./reviewed-builder.mjs";
import { VerifiedReadOnlyJreRegistry } from "./verified-jre-registry.mjs";
import { OuterDockerSandboxAdapter } from "./outer-docker-sandbox.mjs";
import { quiesceContainerProcesses } from "./process-quiescer.mjs";
import { PRODUCTION_OUTPUT_LIMITS } from "./production-limits.mjs";

const decoder = new TextDecoder("utf-8", { fatal: true });
const encoder = new TextEncoder();
const MAX_SPEC_BYTES = 4 * 1024 * 1024;
const MAX_ARTIFACT_BYTES = 512 * 1024 * 1024;

export const AGENT_BUILDER_PATHS = Object.freeze({
  buildSpec: "/input/build-spec.json",
  source: "/input/source.pack",
  dependencyIndex: "/input/dependencies.json",
  dependencies: "/input/dependencies",
  output: "/output",
  catalog: "/opt/xmcl-builder/app/toolchain-catalog.lock.json",
  jreRegistry: "/opt/xmcl-builder/config/jre-registry.json",
  jreRoot: "/opt/xmcl/jres",
  workspaceRoot: "/output",
  prlimit: "/usr/bin/prlimit",
});

export function createAgentBuilderSandbox(options) {
  return new OuterDockerSandboxAdapter({
    ...options,
    quiescer: quiesceContainerProcesses,
  });
}

export class MountedArtifactDownloader {
  constructor({ root, dependencies = [], mounts = [] } = {}) {
    if (typeof root !== "string" || !resolve(root) || !Array.isArray(dependencies) ||
      !Array.isArray(mounts) || mounts.length !== dependencies.length) {
      throw new TypeError("invalid mounted dependency configuration");
    }
    this.root = resolve(root);
    this.dependencies = new Map();
    for (let index = 0; index < dependencies.length; index += 1) {
      const dependency = dependencies[index];
      const mount = mounts[index];
      if (!plainObject(mount) || !sameKeys(mount, ["id", "path", "sha256", "sizeBytes"]) ||
        mount.id !== dependency.id || mount.sha256 !== dependency.sha256 ||
        mount.sizeBytes !== dependency.sizeBytes ||
        mount.path !== `${String(index).padStart(3, "0")}-${dependency.sha256}`) {
        throw new CompilerFailure("mounted_dependency_identity_mismatch");
      }
      this.dependencies.set(dependency.id, { dependency, mount });
    }
    if (this.dependencies.size !== dependencies.length) {
      throw new CompilerFailure("invalid_build_spec");
    }
  }

  async verifyDependencies() {
    let names;
    try {
      names = (await readdir(this.root, { withFileTypes: true }))
        .map((entry) => entry.isFile() ? entry.name : "");
    } catch {
      throw new CompilerFailure("mounted_dependency_mismatch");
    }
    const expected = [...this.dependencies.values()].map(({ mount }) => mount.path).sort();
    if (names.some((name) => !name) || names.sort().join("\0") !== expected.join("\0")) {
      throw new CompilerFailure("mounted_dependency_mismatch");
    }
    for (const { dependency, mount } of this.dependencies.values()) {
      await this.#readMount(mount.path, dependency.sha256, dependency.sizeBytes);
    }
  }

  async download(artifact) {
    if (!artifact || !validSha256(artifact.sha256) ||
      !Number.isSafeInteger(artifact.sizeBytes) || artifact.sizeBytes < 1) {
      throw new CompilerFailure("invalid_reviewed_artifact");
    }
    const mounted = this.dependencies.get(artifact.coordinate);
    if (!mounted || mounted.dependency.sha256 !== artifact.sha256 ||
      mounted.dependency.sizeBytes !== artifact.sizeBytes ||
      !["toolchain", "minecraft", "mapping", "library"].includes(mounted.dependency.kind)) {
      throw new CompilerFailure("mounted_dependency_identity_mismatch");
    }
    return await this.#readMount(
      mounted.mount.path,
      artifact.sha256,
      artifact.sizeBytes,
    );
  }

  async readDependency(dependency) {
    const mounted = this.dependencies.get(dependency?.id);
    if (!mounted || mounted.dependency.kind !== dependency?.kind ||
      mounted.dependency.targetPath !== dependency.targetPath ||
      mounted.dependency.sha256 !== dependency.sha256 ||
      mounted.dependency.sizeBytes !== dependency.sizeBytes) {
      throw new CompilerFailure("mounted_dependency_identity_mismatch");
    }
    return await this.#readMount(
      mounted.mount.path,
      dependency.sha256,
      dependency.sizeBytes,
    );
  }

  async #readMount(name, digest, expectedSize) {
    const path = resolve(this.root, name);
    if (!path.startsWith(`${this.root}${sep}`) || basename(path) !== name) {
      throw new CompilerFailure("mounted_dependency_invalid");
    }
    try {
      const [root, metadata] = await Promise.all([realpath(this.root), lstat(path)]);
      if (!metadata.isFile() || metadata.isSymbolicLink() || metadata.size !== expectedSize ||
        expectedSize > MAX_ARTIFACT_BYTES ||
        resolve(await realpath(path)) !== resolve(root, name)) {
        throw new Error("invalid mounted dependency");
      }
      const bytes = new Uint8Array(await readFile(path));
      if (bytes.byteLength !== expectedSize || sha256(bytes) !== digest) {
        throw new Error("mounted dependency digest mismatch");
      }
      return bytes;
    } catch {
      throw new CompilerFailure("mounted_dependency_mismatch");
    }
  }
}

export class AgentBuilderV1 {
  constructor({
    catalog,
    verifiedJres,
    sandboxRunner,
    dependencies,
  } = {}) {
    if (!(catalog instanceof ReviewedToolchainCatalog) ||
      !verifiedJres || !sandboxRunner || !(dependencies instanceof MountedArtifactDownloader)) {
      throw new TypeError("invalid agent builder configuration");
    }
    this.catalog = catalog;
    this.dependencies = dependencies;
    this.runtimeBuilder = new ReviewedRuntimeBuilder({
      toolchainCatalog: catalog,
      verifiedJres,
      sandboxRunner,
      artifactDownloader: dependencies,
    });
  }

  async build({ buildSpec, source } = {}) {
    const spec = validateBuildSpecV1(buildSpec);
    verifyCatalogBinding(spec, this.catalog);
    await this.dependencies.verifyDependencies();
    const bundle = prepareSourceBundle(spec, source);
    for (const dependency of spec.dependencies) {
      if (dependency.kind !== "mod") continue;
      const bytes = await this.dependencies.readDependency(dependency);
      bundle.manifest.files.push({
        path: `instance/${dependency.targetPath}`,
        sha256: dependency.sha256,
        sizeBytes: dependency.sizeBytes,
      });
      bundle.entries.set(`instance/${dependency.targetPath}`, { bytes });
    }
    bundle.manifest.files.sort((left, right) =>
      left.path < right.path ? -1 : left.path > right.path ? 1 : 0);
    const buildKey = buildKeyFor(spec);
    const built = await this.runtimeBuilder.build({
      bundle,
      frozenManifest: {
        compatibility: {
          runtimeCatalog: { sha256: spec.runtime.runtimeCatalogRevision },
        },
        mods: [],
      },
      expectedContentKey:
        `shared-runtime/prepared/v1/linux-amd64/${buildKey}/content.tar.zst`,
    });
    return {
      archive: built.archive,
      result: resultDocument(buildKey, built),
    };
  }
}

export function prepareSourceBundle(buildSpec, source) {
  const spec = validateBuildSpecV1(buildSpec);
  if (!(source instanceof Uint8Array) || source.byteLength < 22 ||
    source.byteLength > MAX_BUNDLE_BYTES || sha256(source) !== spec.source.sha256) {
    throw new CompilerFailure("source_digest_mismatch");
  }
  const archive = readStrictZip(source);
  const content = spec.source.format === "mrpack"
    ? validateMrpack(archive, spec)
    : validatePlainZip(archive, spec);
  const files = spec.files.map((file) => ({
    path: `instance/${file.targetPath}`,
    sha256: file.sha256,
    sizeBytes: file.sizeBytes,
  }));
  return {
    manifest: {
      minecraftVersion: spec.compatibility.minecraftVersion,
      loader: { ...spec.compatibility.loader },
      javaRequirement: { ...spec.compatibility.java },
      runtimeCatalog: { sha256: spec.runtime.runtimeCatalogRevision },
      files,
    },
    entries: new Map(spec.files.map((file) => [
      `instance/${file.targetPath}`,
      { bytes: content.get(file.path).bytes.slice() },
    ])),
  };
}

export async function runAgentBuilderV1({
  paths = AGENT_BUILDER_PATHS,
  mountInspector,
  sandboxProcessRunner,
} = {}) {
  validatePaths(paths);
  const [rawSpec, source, dependencyIndex, catalogDocument] = await Promise.all([
    readBounded(paths.buildSpec, MAX_SPEC_BYTES),
    readBounded(paths.source, MAX_BUNDLE_BYTES),
    readJson(paths.dependencyIndex, MAX_SPEC_BYTES),
    readJson(paths.catalog, MAX_SPEC_BYTES),
  ]);
  let spec;
  try {
    spec = validateBuildSpecV1(JSON.parse(decoder.decode(rawSpec)));
  } catch (error) {
    if (error instanceof CompilerFailure) throw error;
    throw new CompilerFailure("invalid_build_spec");
  }
  const catalog = new ReviewedToolchainCatalog(catalogDocument, {
    supportedFamilies: ["neoforge"],
  });
  verifyCatalogBinding(spec, catalog);
  const registry = await VerifiedReadOnlyJreRegistry.load({
    path: paths.jreRegistry,
    catalog,
    rootDirectory: paths.jreRoot,
    mountInspector,
  });
  await registry.initialize();
  const sandbox = createAgentBuilderSandbox({
    jreRegistry: registry,
    workspaceRoot: paths.workspaceRoot,
    prlimitPath: paths.prlimit,
    runner: sandboxProcessRunner,
  });
  await sandbox.initialize();
  const dependencies = new MountedArtifactDownloader({
    root: paths.dependencies,
    dependencies: spec.dependencies,
    mounts: parseDependencyIndex(dependencyIndex, spec.dependencies.length),
  });
  const builder = new AgentBuilderV1({
    catalog,
    verifiedJres: registry,
    sandboxRunner: sandbox,
    dependencies,
  });
  const built = await builder.build({ buildSpec: spec, source });
  await writeBuilderOutputs(paths.output, built);
  return built.result;
}

function validatePlainZip(archive, spec) {
  if (archive.size !== spec.files.length) {
    throw new CompilerFailure("source_file_mismatch");
  }
  verifyFileSet(archive, spec.files);
  return archive;
}

function validateMrpack(archive, spec) {
  const indexEntry = archive.get("modrinth.index.json");
  if (!indexEntry) throw new CompilerFailure("invalid_mrpack");
  let index;
  try {
    index = JSON.parse(decoder.decode(indexEntry.bytes));
  } catch {
    throw new CompilerFailure("invalid_mrpack");
  }
  if (!plainObject(index) || Object.keys(index).some((key) => ![
    "formatVersion", "game", "versionId", "name", "summary", "files", "dependencies",
  ].includes(key)) || index.formatVersion !== 1 || index.game !== "minecraft" ||
    typeof index.versionId !== "string" || !index.versionId ||
    typeof index.name !== "string" || !index.name ||
    index.summary !== undefined && typeof index.summary !== "string" ||
    !Array.isArray(index.files) || !plainObject(index.dependencies)) {
    throw new CompilerFailure("invalid_mrpack");
  }
  verifyMrpackCompatibility(index.dependencies, spec.compatibility);
  const mods = spec.dependencies.filter((dependency) => dependency.kind === "mod");
  const modsByPath = new Map(mods.map((dependency) => [
    dependency.targetPath,
    dependency,
  ]));
  if (index.files.length !== mods.length) {
    throw new CompilerFailure("mrpack_dependency_mismatch");
  }
  let previous = "";
  for (let position = 0; position < index.files.length; position += 1) {
    const file = index.files[position];
    const expected = modsByPath.get(file?.path);
    if (!plainObject(file) || Object.keys(file).some((key) => ![
      "path", "hashes", "env", "downloads", "fileSize",
    ].includes(key)) || typeof file.path !== "string" ||
      previous && previous >= file.path || !plainObject(file.hashes) ||
      Object.keys(file.hashes).some((key) => !["sha1", "sha512", "sha256"].includes(key)) ||
      !Object.values(file.hashes).every((hash) =>
        typeof hash === "string" && /^[a-f0-9]+$/.test(hash)) ||
      !Array.isArray(file.downloads) ||
      file.downloads.some((url) => typeof url !== "string") ||
      !Number.isSafeInteger(file.fileSize) || file.fileSize < 1 ||
      !expected || file.fileSize !== expected.sizeBytes ||
      file.hashes.sha256 !== undefined && file.hashes.sha256 !== expected.sha256 ||
      file.env !== undefined && (!plainObject(file.env) ||
        Object.keys(file.env).some((key) => !["client", "server"].includes(key)) ||
        file.env.server === "unsupported")) {
      throw new CompilerFailure("invalid_mrpack");
    }
    previous = file.path;
  }

  const content = new Map();
  for (const [path, entry] of archive) {
    if (path === "modrinth.index.json") continue;
    const prefix = path.startsWith("server-overrides/") ? "server-overrides/" :
      path.startsWith("overrides/") ? "overrides/" : undefined;
    if (!prefix) throw new CompilerFailure("invalid_mrpack");
    const file = spec.files.find((candidate) => candidate.path === path);
    if (!file || file.targetPath !== path.slice(prefix.length) || content.has(path)) {
      throw new CompilerFailure("invalid_mrpack");
    }
    content.set(path, entry);
  }
  verifyFileSet(content, spec.files);
  return content;
}

function verifyMrpackCompatibility(dependencies, compatibility) {
  const loaderKey = {
    fabric: "fabric-loader",
    forge: "forge",
    neoforge: "neoforge",
    quilt: "quilt-loader",
  }[compatibility.loader.kind];
  if (!sameKeys(dependencies, ["minecraft", loaderKey]) ||
    dependencies.minecraft !== compatibility.minecraftVersion ||
    dependencies[loaderKey] !== compatibility.loader.version) {
    throw new CompilerFailure("mrpack_compatibility_mismatch");
  }
}

function verifyFileSet(entries, expected) {
  if (entries.size !== expected.length) throw new CompilerFailure("source_file_mismatch");
  for (const file of expected) {
    const actual = entries.get(file.path)?.bytes;
    if (!(actual instanceof Uint8Array) || actual.byteLength !== file.sizeBytes ||
      sha256(actual) !== file.sha256) {
      throw new CompilerFailure("source_file_mismatch");
    }
  }
}

function resultDocument(buildKey, built) {
  return canonicalObject({
    schemaVersion: 1,
    buildKey,
    content: {
      sha256: built.content.sha256,
      compressedSize: built.content.sizeBytes,
      logicalSize: built.content.entries.reduce((total, entry) =>
        total + entry.sizeBytes, 0),
      paths: built.content.entries.map((entry) => entry.path),
    },
  });
}

export async function writeBuilderOutputs(output, built) {
  await mkdir(output, { recursive: true, mode: 0o700 });
  const metadata = await stat(output);
  if (!metadata.isDirectory()) throw new CompilerFailure("output_write_failed");
  const archivePath = resolve(output, "content.tar.zst");
  const resultPath = resolve(output, "result.json");
  const archiveTemporary = resolve(output, ".content.tar.zst.partial");
  const resultTemporary = resolve(output, ".result.json.partial");
  try {
    await access(archivePath, constants.F_OK).then(
      () => { throw new CompilerFailure("output_not_empty"); },
      () => undefined,
    );
    await access(resultPath, constants.F_OK).then(
      () => { throw new CompilerFailure("output_not_empty"); },
      () => undefined,
    );
    await writeFile(archiveTemporary, built.archive, { flag: "wx", mode: 0o600 });
    await rename(archiveTemporary, archivePath);
    await writeFile(resultTemporary,
      encoder.encode(`${JSON.stringify(built.result)}\n`), { flag: "wx", mode: 0o600 });
    await rename(resultTemporary, resultPath);
  } catch (error) {
    await Promise.all([
      rm(archiveTemporary, { force: true }),
      rm(resultTemporary, { force: true }),
    ]);
    if (error instanceof CompilerFailure) throw error;
    throw new CompilerFailure("output_write_failed");
  }
}

function verifyCatalogBinding(spec, catalog) {
  if (spec.builder.toolchainCatalogRevision !== catalog.catalogRevision ||
    spec.runtime.runtimeCatalogRevision !== catalog.runtimeCatalogRevision) {
    throw new CompilerFailure("catalog_revision_mismatch");
  }
}

async function readJson(path, maximum) {
  try {
    return JSON.parse(decoder.decode(await readBounded(path, maximum)));
  } catch {
    throw new CompilerFailure("invalid_builder_configuration");
  }
}

async function readBounded(path, maximum) {
  try {
    const metadata = await lstat(path);
    if (!metadata.isFile() || metadata.isSymbolicLink() ||
      metadata.size < 1 || metadata.size > maximum) {
      throw new Error("invalid input");
    }
    return new Uint8Array(await readFile(path));
  } catch {
    throw new CompilerFailure("invalid_builder_input");
  }
}

function validatePaths(paths) {
  if (!plainObject(paths) || !sameKeys(paths, Object.keys(AGENT_BUILDER_PATHS)) ||
    Object.values(paths).some((path) => typeof path !== "string" || resolve(path) !== path)) {
    throw new TypeError("invalid agent builder paths");
  }
}

function parseDependencyIndex(value, count) {
  if (!plainObject(value) || !sameKeys(value, ["schemaVersion", "dependencies"]) ||
    value.schemaVersion !== 1 || !Array.isArray(value.dependencies) ||
    value.dependencies.length !== count) {
    throw new CompilerFailure("mounted_dependency_identity_mismatch");
  }
  return value.dependencies;
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function canonicalObject(value) {
  if (Array.isArray(value)) return value.map(canonicalObject);
  if (!plainObject(value)) return value;
  return Object.fromEntries(Object.keys(value).sort().map((key) =>
    [key, canonicalObject(value[key])]));
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
