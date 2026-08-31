import { spawn } from "node:child_process";
import { access, mkdtemp, mkdir, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import { constants } from "node:fs";
import { resolve, sep } from "node:path";
import { inflateRawSync } from "node:zlib";
import { CompilerFailure } from "./bundle.mjs";
import { PRODUCTION_OUTPUT_LIMITS } from "./production-limits.mjs";

const MAX_FILES = PRODUCTION_OUTPUT_LIMITS.maxFiles;

export class BubblewrapSandboxAdapter {
  constructor({
    jreRegistry,
    workspaceRoot,
    bubblewrapPath = "/usr/bin/bwrap",
    prlimitPath = "/usr/bin/prlimit",
    timeoutMs = 5 * 60 * 1000,
    addressSpaceBytes = 3 * 1024 * 1024 * 1024,
    fileSizeBytes = PRODUCTION_OUTPUT_LIMITS.maxFileBytes,
    processLimit = 256,
    cpuSeconds = 240,
    runner = runProcess,
    installerRewriter = rewriteNeoForgeInstaller,
  } = {}) {
    if (!jreRegistry || typeof jreRegistry.rootForToken !== "function" ||
      !absoluteNormalizedPath(workspaceRoot) || !absoluteNormalizedPath(bubblewrapPath) ||
      !absoluteNormalizedPath(prlimitPath) || typeof runner !== "function" ||
      !bounded(timeoutMs, 10_000, 15 * 60 * 1000) ||
      !bounded(addressSpaceBytes, 512 * 1024 * 1024, 8 * 1024 * 1024 * 1024) ||
      !bounded(fileSizeBytes, 64 * 1024 * 1024, PRODUCTION_OUTPUT_LIMITS.maxFileBytes) ||
      !bounded(processLimit, 16, 1_024) || !bounded(cpuSeconds, 10, 600) ||
      typeof installerRewriter !== "function") {
      throw new TypeError("invalid bubblewrap sandbox configuration");
    }
    this.jreRegistry = jreRegistry;
    this.workspaceRoot = workspaceRoot;
    this.bubblewrapPath = bubblewrapPath;
    this.prlimitPath = prlimitPath;
    this.timeoutMs = timeoutMs;
    this.addressSpaceBytes = addressSpaceBytes;
    this.fileSizeBytes = fileSizeBytes;
    this.processLimit = processLimit;
    this.cpuSeconds = cpuSeconds;
    this.runner = runner;
    this.installerRewriter = installerRewriter;
    this.capabilities = Object.freeze({
      ephemeralWorkspace: true,
      nonRoot: true,
      readOnlyBaseFilesystem: true,
      noSecrets: true,
      noDockerSocket: true,
      boundedResources: true,
      egress: "approved-artifacts-only",
    });
  }

  async initialize() {
    if (typeof process.getuid !== "function" || process.getuid() !== 10001 ||
      typeof process.getgid !== "function" || process.getgid() !== 10001) {
      throw new TypeError("sandbox worker must run as uid/gid 10001");
    }
    await Promise.all([
      access(this.bubblewrapPath, constants.X_OK),
      access(this.prlimitPath, constants.X_OK),
      mkdir(this.workspaceRoot, { recursive: true, mode: 0o700 }),
    ]);
    await this.runner(this.bubblewrapPath, [
      "--die-with-parent",
      "--new-session",
      "--unshare-all",
      "--uid", String(process.getuid()),
      "--gid", String(process.getgid()),
      "--clearenv",
      "--tmpfs", "/",
      "--dir", "/bin",
      "--dir", "/usr",
      "--proc", "/proc",
      "--dev", "/dev",
      "--ro-bind", "/bin", "/bin",
      "--ro-bind", "/lib", "/lib",
      "--ro-bind", "/lib64", "/lib64",
      "--ro-bind", "/usr/lib", "/usr/lib",
      "/bin/true",
    ], { timeoutMs: 10_000, cwd: "/", env: Object.freeze({}) });
  }

  async assemble(request) {
    validateRequest(request);
    if (request.plan.family !== "neoforge") {
      throw new CompilerFailure("unsupported_compatibility");
    }
    const jreRoot = this.jreRegistry.rootForToken(request.jre.rootToken);
    await mkdir(this.workspaceRoot, { recursive: true, mode: 0o700 });
    const workspace = await mkdtemp(`${this.workspaceRoot}${sep}job-`);
    const inputs = `${workspace}${sep}inputs`;
    const output = `${workspace}${sep}output`;
    try {
      await mkdir(inputs, { mode: 0o700 });
      await mkdir(output, { mode: 0o700 });
      const { installer, server } = selectNeoForgeArtifacts(request);
      const offlineInstaller = this.installerRewriter(installer.bytes);
      await writeFile(`${inputs}${sep}installer.jar`, offlineInstaller, { mode: 0o400, flag: "wx" });
      const serverDirectory = `${output}${sep}libraries${sep}net${sep}minecraft${sep}server${sep}` +
        request.plan.minecraftVersion;
      await mkdir(serverDirectory, { recursive: true, mode: 0o700 });
      await writeFile(`${serverDirectory}${sep}server-${request.plan.minecraftVersion}.jar`,
        server.bytes, { mode: 0o600, flag: "wx" });
      for (const artifact of request.artifacts) {
        if (artifact === installer || artifact === server) continue;
        const relative = mavenPath(artifact);
        const target = `${output}${sep}libraries${sep}${relative.split("/").join(sep)}`;
        await mkdir(resolve(target, ".."), { recursive: true, mode: 0o700 });
        await writeFile(target, artifact.bytes, { mode: 0o600, flag: "wx" });
      }
      const args = this.#command(jreRoot, inputs, output);
      await this.runner(this.prlimitPath, args, {
        timeoutMs: this.timeoutMs,
        cwd: "/",
        env: Object.freeze({}),
      });
      await Promise.all([
        "run.sh",
        "run.bat",
        "user_jvm_args.txt",
        "installer.log",
        "installer.jar.log",
      ].map((name) => rm(`${output}${sep}${name}`, { force: true })));
      const files = await collectFiles(output);
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
    } catch (error) {
      if (error instanceof CompilerFailure) throw error;
      throw new CompilerFailure("installer_failed");
    } finally {
      await rm(workspace, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  #command(jreRoot, inputs, output) {
    return [
      `--as=${this.addressSpaceBytes}`,
      `--fsize=${this.fileSizeBytes}`,
      `--nproc=${this.processLimit}`,
      `--cpu=${this.cpuSeconds}`,
      "--",
      this.bubblewrapPath,
      "--die-with-parent",
      "--new-session",
      "--unshare-all",
      "--uid", "10001",
      "--gid", "10001",
      "--clearenv",
      "--tmpfs", "/",
      "--dir", "/usr",
      "--proc", "/proc",
      "--dev", "/dev",
      "--dir", "/work",
      "--dir", "/inputs",
      "--dir", "/jre",
      "--ro-bind", "/lib", "/lib",
      "--ro-bind", "/lib64", "/lib64",
      "--ro-bind", "/usr/lib", "/usr/lib",
      "--ro-bind", jreRoot, "/jre",
      "--ro-bind", inputs, "/inputs",
      "--bind", output, "/work",
      "--chdir", "/work",
      "/jre/bin/java",
      "-Djava.awt.headless=true",
      "-Duser.home=/work",
      "-Djava.io.tmpdir=/work",
      "-Xms128m",
      "-Xmx1024m",
      "-Xss512k",
      "-XX:MaxMetaspaceSize=512m",
      "-XX:CompressedClassSpaceSize=256m",
      "-XX:ReservedCodeCacheSize=128m",
      "-XX:MaxDirectMemorySize=256m",
      "-XX:ActiveProcessorCount=2",
      "-jar", "/inputs/installer.jar",
      "--offline",
      "--installServer", "/work",
    ];
  }
}

function selectNeoForgeArtifacts(request) {
  const installerCoordinate =
    `net.neoforged:neoforge:${request.plan.loader.version}:installer`;
  const serverCoordinate =
    `com.mojang:minecraft-server:${request.plan.minecraftVersion}:server`;
  const installer = request.artifacts.find((entry) => entry.coordinate === installerCoordinate);
  const server = request.artifacts.find((entry) => entry.coordinate === serverCoordinate);
  if (!installer || !server || request.artifacts.length < 3) {
    throw new CompilerFailure("installer_failed");
  }
  return { installer, server };
}

function mavenPath(artifact) {
  if (typeof artifact?.url !== "string" || typeof artifact.coordinate !== "string") {
    throw new CompilerFailure("installer_failed");
  }
  const parts = artifact.coordinate.split(":");
  if (parts.length < 3 || parts.length > 4) throw new CompilerFailure("installer_failed");
  const [group, name, version, classifier] = parts;
  let extension;
  try {
    const filename = new URL(artifact.url).pathname.split("/").at(-1);
    extension = filename?.split(".").at(-1);
  } catch {
    throw new CompilerFailure("installer_failed");
  }
  if (!/^(?:jar|zip|txt)$/.test(extension ?? "")) {
    throw new CompilerFailure("installer_failed");
  }
  const filename = `${name}-${version}${classifier ? `-${classifier}` : ""}.${extension}`;
  const path = `${group.replaceAll(".", "/")}/${name}/${version}/${filename}`;
  if (!/^[0-9A-Za-z._+/-]+$/.test(path) || path.includes("..")) {
    throw new CompilerFailure("installer_failed");
  }
  return path;
}

export function rewriteNeoForgeInstaller(bytes) {
    const entries = readZip(bytes);
    const profile = entries.find((entry) => entry.name === "install_profile.json");
    if (!profile) throw new CompilerFailure("installer_failed");
    let document;
    try {
      document = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(profile.bytes));
    } catch {
      throw new CompilerFailure("installer_failed");
    }
    const before = document.processors;
    if (!Array.isArray(before)) throw new CompilerFailure("installer_failed");
    const processors = before.filter((processor) =>
      !Array.isArray(processor?.args) || !processor.args.includes("DOWNLOAD_MOJMAPS"));
    if (processors.length !== before.length - 1) throw new CompilerFailure("installer_failed");
    document.processors = processors;
    profile.bytes = new TextEncoder().encode(JSON.stringify(document));
    return writeStoredZip(entries);
  }

  function readZip(bytes) {
    if (!(bytes instanceof Uint8Array) || bytes.byteLength < 22) {
      throw new CompilerFailure("installer_failed");
    }
    for (let end = bytes.byteLength - 22; end >= Math.max(0, bytes.byteLength - 65_557); end -= 1) {
      if (readU32(bytes, end) !== 0x06054b50) continue;
      const count = readU16(bytes, end + 10);
      let central = readU32(bytes, end + 16);
      const entries = [];
      for (let index = 0; index < count; index += 1) {
        if (readU32(bytes, central) !== 0x02014b50) throw new CompilerFailure("installer_failed");
        const method = readU16(bytes, central + 10);
        const compressedSize = readU32(bytes, central + 20);
        const size = readU32(bytes, central + 24);
        const nameLength = readU16(bytes, central + 28);
        const extraLength = readU16(bytes, central + 30);
        const commentLength = readU16(bytes, central + 32);
        const local = readU32(bytes, central + 42);
        const nameBytes = bytes.subarray(central + 46, central + 46 + nameLength);
        const name = new TextDecoder("utf-8", { fatal: true }).decode(nameBytes);
        if (readU32(bytes, local) !== 0x04034b50 || name.includes("\\") ||
          name.startsWith("/") || name.split("/").includes("..")) {
          throw new CompilerFailure("installer_failed");
        }
        const localNameLength = readU16(bytes, local + 26);
        const localExtraLength = readU16(bytes, local + 28);
        const start = local + 30 + localNameLength + localExtraLength;
        const compressed = bytes.subarray(start, start + compressedSize);
        const content = method === 0 ? compressed : method === 8 ? inflateRawSync(compressed) : undefined;
        if (!content || content.byteLength !== size) throw new CompilerFailure("installer_failed");
        entries.push({ name, bytes: new Uint8Array(content) });
        central += 46 + nameLength + extraLength + commentLength;
      }
      return entries;
    }
    throw new CompilerFailure("installer_failed");
  }

  function writeStoredZip(entries) {
    const local = [];
    const central = [];
    let offset = 0;
    for (const entry of entries) {
      const name = Buffer.from(entry.name, "utf8");
      const content = Buffer.from(entry.bytes);
      const checksum = crc32(content);
      const header = Buffer.alloc(30);
      header.writeUInt32LE(0x04034b50, 0);
      header.writeUInt16LE(20, 4);
      header.writeUInt16LE(0x800, 6);
      header.writeUInt32LE(checksum, 14);
      header.writeUInt32LE(content.byteLength, 18);
      header.writeUInt32LE(content.byteLength, 22);
      header.writeUInt16LE(name.byteLength, 26);
      local.push(header, name, content);
      const directory = Buffer.alloc(46);
      directory.writeUInt32LE(0x02014b50, 0);
      directory.writeUInt16LE(20, 4);
      directory.writeUInt16LE(20, 6);
      directory.writeUInt16LE(0x800, 8);
      directory.writeUInt32LE(checksum, 16);
      directory.writeUInt32LE(content.byteLength, 20);
      directory.writeUInt32LE(content.byteLength, 24);
      directory.writeUInt16LE(name.byteLength, 28);
      directory.writeUInt32LE(offset, 42);
      central.push(directory, name);
      offset += header.byteLength + name.byteLength + content.byteLength;
    }
    const centralBytes = Buffer.concat(central);
    const end = Buffer.alloc(22);
    end.writeUInt32LE(0x06054b50, 0);
    end.writeUInt16LE(entries.length, 8);
    end.writeUInt16LE(entries.length, 10);
    end.writeUInt32LE(centralBytes.byteLength, 12);
    end.writeUInt32LE(offset, 16);
    return new Uint8Array(Buffer.concat([...local, centralBytes, end]));
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

  function readU16(bytes, offset) {
    return bytes[offset] | (bytes[offset + 1] << 8);
  }

  function readU32(bytes, offset) {
    return (readU16(bytes, offset) | (readU16(bytes, offset + 2) << 16)) >>> 0;
  }
function validateRequest(value) {
  if (!value || value.schemaVersion !== 1 || !value.plan || !value.jre ||
    !Array.isArray(value.artifacts) || value.sandbox?.ephemeralWorkspace !== true ||
    value.sandbox?.nonRoot !== true || value.sandbox?.readOnlyBaseFilesystem !== true ||
    value.sandbox?.noSecrets !== true || value.sandbox?.noDockerSocket !== true ||
    value.sandbox?.boundedResources !== true) {
    throw new CompilerFailure("installer_failed");
  }
}

async function collectFiles(root) {
  const files = new Map();
  let total = 0;
  const visit = async (directory, prefix = "") => {
    for (const entry of await readdir(directory, { withFileTypes: true })) {
      const relative = prefix ? `${prefix}/${entry.name}` : entry.name;
      const path = `${directory}${sep}${entry.name}`;
      if (entry.isSymbolicLink() || (!entry.isDirectory() && !entry.isFile())) {
        throw new CompilerFailure("invalid_installer_output");
      }
      if (entry.isDirectory()) {
        await visit(path, relative);
        continue;
      }
      const metadata = await stat(path);
      total += metadata.size;
      if (metadata.size > PRODUCTION_OUTPUT_LIMITS.maxFileBytes ||
        files.size >= MAX_FILES || total > PRODUCTION_OUTPUT_LIMITS.maxSandboxBytes) {
        throw new CompilerFailure("invalid_installer_output");
      }
      files.set(relative, {
        bytes: new Uint8Array(await readFile(path)),
        mode: metadata.mode & 0o111 ? 0o755 : 0o644,
      });
    }
  };
  await visit(root);
  return files;
}

async function runProcess(command, args, { timeoutMs, cwd, env }) {
  await new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, {
      cwd,
      env,
      stdio: ["ignore", "ignore", "pipe"],
    });
    let stderr = "";
    child.stderr.setEncoding("utf8");
    child.stderr.on("data", (chunk) => {
      if (stderr.length < 16_384) stderr += chunk.slice(0, 16_384 - stderr.length);
    });
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new CompilerFailure("installer_timeout"));
    }, timeoutMs);
    child.once("error", () => reject(new CompilerFailure("installer_spawn_failed")));
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      if (code === 0 && !signal) resolvePromise();
      else reject(new CompilerFailure("installer_process_failed"));
    });
  });
}

function absoluteNormalizedPath(value) {
  return typeof value === "string" && value.length > 1 && value.length <= 1_024 &&
    resolve(value) === value;
}

function bounded(value, minimum, maximum) {
  return Number.isSafeInteger(value) && value >= minimum && value <= maximum;
}
