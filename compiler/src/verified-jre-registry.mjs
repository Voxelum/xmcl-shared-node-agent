import { access, lstat, readFile, readlink, realpath, stat } from "node:fs/promises";
import { constants } from "node:fs";
import { createReadStream } from "node:fs";
import { createHash } from "node:crypto";
import { resolve, sep } from "node:path";
import { CompilerFailure } from "./bundle.mjs";

const decoder = new TextDecoder("utf-8", { fatal: true });

export class VerifiedReadOnlyJreRegistry {
  constructor({ document, catalog, rootDirectory, mountInspector = readOnlyMount } = {}) {
    if (!catalog || typeof catalog !== "object" || !absoluteNormalizedPath(rootDirectory) ||
      typeof mountInspector !== "function") {
      throw new TypeError("invalid verified JRE registry configuration");
    }
    const parsed = parseRegistry(document, catalog, rootDirectory);
    this.catalog = catalog;
    this.rootDirectory = rootDirectory;
    this.mountInspector = mountInspector;
    this.roots = parsed;
    this.tokens = new Map();
  }

  static async load({ path, catalog, rootDirectory, mountInspector } = {}) {
    let document;
    try {
      document = JSON.parse(decoder.decode(await readFile(path)));
    } catch {
      throw new TypeError("invalid verified JRE registry configuration");
    }
    return new VerifiedReadOnlyJreRegistry({
      document, catalog, rootDirectory, mountInspector,
    });
  }

  async resolve(expected) {
    const root = this.roots.get(expected?.id);
    if (!root || root.sha256 !== expected.sha256 ||
      root.component !== expected.component || root.major !== expected.major ||
      root.runtimeCatalogRevision !== expected.runtimeCatalogRevision) {
      throw new CompilerFailure("compiler_unavailable");
    }
    let actualRoot;
    try {
      actualRoot = await realpath(root.path);
      if (!within(this.rootDirectory, actualRoot) ||
        await this.mountInspector(actualRoot) !== true) {
        throw new Error("JRE root is not a read-only mount");
      }
      await verifyRuntimeRoot(actualRoot, root);
      const java = `${actualRoot}${sep}bin${sep}java`;
      const metadata = await stat(java);
      await access(java, constants.R_OK | constants.X_OK);
      if (!metadata.isFile() || (metadata.mode & 0o022) !== 0) throw new Error("unsafe java");
    } catch {
      throw new CompilerFailure("compiler_unavailable");
    }

    async function verifyRuntimeRoot(root, expected) {
      const raw = await readFile(`${root}${sep}.xmcl-runtime-resolution.json`);
      const resolution = JSON.parse(decoder.decode(raw));
      if (!plainObject(resolution) || resolution.component !== expected.component ||
        resolution.major !== expected.major || !Array.isArray(resolution.files) ||
        sha256(canonicalJsonBytes(resolution)) !== expected.sha256) {
        throw new Error("JRE resolution does not match reviewed digest");
      }
      const paths = new Map();
      for (const entry of resolution.files) {
        if (!plainObject(entry) || !safeRelativePath(entry.path) || paths.has(entry.path) ||
          !["directory", "file", "link"].includes(entry.type)) {
          throw new Error("invalid JRE resolution entry");
        }
        paths.set(entry.path, entry.type);
      }
      for (const entry of resolution.files) {
        const parts = entry.path.split("/");
        for (let index = 1; index < parts.length; index += 1) {
          if (paths.get(parts.slice(0, index).join("/")) !== "directory") {
            throw new Error("JRE entry has undeclared parent");
          }
        }
        const path = `${root}${sep}${entry.path.split("/").join(sep)}`;
        const metadata = await lstat(path);
        if (!within(root, await realpath(path))) throw new Error("JRE entry escaped root");
        if (entry.type === "directory") {
          if (!metadata.isDirectory()) throw new Error("JRE directory mismatch");
        } else if (entry.type === "link") {
          if (!metadata.isSymbolicLink() || typeof entry.target !== "string" ||
            await readlink(path) !== entry.target) {
            throw new Error("JRE link mismatch");
          }
        } else {
          if (!metadata.isFile() || metadata.size !== entry.size ||
            !plainObject(entry.hashes) || !/^[a-f0-9]{40}$/.test(entry.hashes.sha1) ||
            await hashFile(path, "sha1") !== entry.hashes.sha1 ||
            process.platform !== "win32" && entry.executable === true &&
              (metadata.mode & 0o111) === 0) {
            throw new Error("JRE file mismatch");
          }
        }
      }
    }

    async function hashFile(path, algorithm) {
      const hash = createHash(algorithm);
      for await (const chunk of createReadStream(path)) hash.update(chunk);
      return hash.digest("hex");
    }

    function canonicalJsonBytes(value) {
      return new TextEncoder().encode(JSON.stringify(canonicalize(value)));
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

    function safeRelativePath(value) {
      return typeof value === "string" && value.length > 0 && value.length <= 512 &&
        !value.includes("\\") && !value.startsWith("/") &&
        value.split("/").every((part) => part && part !== "." && part !== "..");
    }
    const token = `jre-${createHash("sha256")
      .update(`${root.id}\0${root.sha256}\0${actualRoot}`)
      .digest("hex")}`;
    this.tokens.set(token, actualRoot);
    return {
      id: root.id,
      sha256: root.sha256,
      component: root.component,
      major: root.major,
      runtimeCatalogRevision: root.runtimeCatalogRevision,
      verified: true,
      readOnly: true,
      rootToken: token,
    };
  }

  async initialize() {
    for (const root of this.roots.values()) await this.resolve(root);
  }

  rootForToken(token) {
    const root = this.tokens.get(token);
    if (!root) throw new CompilerFailure("compiler_unavailable");
    return root;
  }
}

function parseRegistry(value, catalog, rootDirectory) {
  if (!plainObject(value) || !sameKeys(value, [
    "schemaVersion", "catalogRevision", "runtimeCatalogRevision", "roots",
  ]) || value.schemaVersion !== 1 ||
    value.catalogRevision !== catalog.catalogRevision ||
    value.runtimeCatalogRevision !== catalog.runtimeCatalogRevision ||
    !Array.isArray(value.roots) || value.roots.length < 1 || value.roots.length > 32) {
    throw new TypeError("invalid verified JRE registry configuration");
  }
  const expected = new Map();
  for (const toolchain of catalog.toolchains) expected.set(toolchain.jre.id, toolchain.jre);
  const roots = new Map();
  for (const entry of value.roots) {
    if (!plainObject(entry) || !sameKeys(entry, [
      "id", "sha256", "component", "major", "runtimeCatalogRevision", "path",
    ]) || roots.has(entry.id) || !expected.has(entry.id) ||
      typeof entry.path !== "string" || !absoluteNormalizedPath(entry.path) ||
      !within(rootDirectory, entry.path)) {
      throw new TypeError("invalid verified JRE registry configuration");
    }
    const reviewed = expected.get(entry.id);
    if (entry.sha256 !== reviewed.sha256 || entry.component !== reviewed.component ||
      entry.major !== reviewed.major ||
      entry.runtimeCatalogRevision !== reviewed.runtimeCatalogRevision) {
      throw new TypeError("invalid verified JRE registry configuration");
    }
    roots.set(entry.id, Object.freeze({ ...entry }));
  }
  if ([...expected].some(([id]) => !roots.has(id))) {
    throw new TypeError("invalid verified JRE registry configuration");
  }
  return roots;
}

async function readOnlyMount(path) {
  const mountInfo = await readFile("/proc/self/mountinfo", "utf8");
  let selected;
  for (const line of mountInfo.split("\n")) {
    const fields = line.split(" ");
    const separator = fields.indexOf("-");
    if (separator < 6) continue;
    const mountPoint = unescapeMount(fields[4]);
    if ((path === mountPoint || path.startsWith(`${mountPoint}/`)) &&
      (!selected || mountPoint.length > selected.mountPoint.length)) {
      selected = { mountPoint, options: fields[5].split(",") };
    }
  }
  return selected?.options.includes("ro") === true;
}

function unescapeMount(value) {
  return value.replace(/\\([0-7]{3})/g, (_, octal) =>
    String.fromCharCode(Number.parseInt(octal, 8)));
}

function within(root, value) {
  const normalizedRoot = resolve(root);
  const normalized = resolve(value);
  return normalized.startsWith(`${normalizedRoot}${sep}`);
}

function absoluteNormalizedPath(value) {
  return typeof value === "string" && value.length > 1 && value.length <= 1_024 &&
    resolve(value) === value;
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
