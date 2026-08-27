import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { access, mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import { constants } from "node:fs";
import { dirname, join, resolve, sep } from "node:path";
import test from "node:test";
import {
  ReviewedToolchainCatalog,
  StrictArtifactDownloader,
  createAssemblyPlan,
} from "../src/reviewed-builder.mjs";
import { BubblewrapSandboxAdapter } from "../src/bubblewrap-sandbox.mjs";
import { VerifiedReadOnlyJreRegistry } from "../src/verified-jre-registry.mjs";

const enabled = process.env.XMCL_RUN_NEOFORGE_INTEGRATION === "1";
const catalogPath = resolve("toolchain-catalog.lock.json");

test("real reviewed NeoForge 1.21.1 installs and boots offline in bubblewrap", {
  skip: enabled ? false : "set XMCL_RUN_NEOFORGE_INTEGRATION=1",
  timeout: 15 * 60 * 1000,
}, async () => {
  const jreRoot = requiredAbsolutePath("XMCL_REVIEWED_JAVA_21_ROOT");
  const workspaceRoot = requiredAbsolutePath("XMCL_INTEGRATION_WORKSPACE_ROOT");
  const bubblewrapPath = optionalAbsolutePath("XMCL_BWRAP_PATH", "/usr/bin/bwrap");
  const prlimitPath = optionalAbsolutePath("XMCL_PRLIMIT_PATH", "/usr/bin/prlimit");
  await Promise.all([
    access(join(jreRoot, "bin", "java"), constants.R_OK | constants.X_OK),
    access(bubblewrapPath, constants.X_OK),
    access(prlimitPath, constants.X_OK),
  ]);

  const catalogDocument = JSON.parse(await readFile(catalogPath, "utf8"));
  const catalog = new ReviewedToolchainCatalog(catalogDocument, {
    supportedFamilies: ["neoforge"],
  });
  const toolchain = catalog.toolchains.find((entry) =>
    entry.minecraftVersion === "1.21.1" &&
    entry.loader.kind === "neoforge" &&
    entry.loader.version === "21.1.115");
  assert.ok(toolchain, "reviewed NeoForge 1.21.1 tuple is missing");

  const registry = new VerifiedReadOnlyJreRegistry({
    document: {
      schemaVersion: 1,
      catalogRevision: catalog.catalogRevision,
      runtimeCatalogRevision: catalog.runtimeCatalogRevision,
      roots: [{ ...toolchain.jre, path: jreRoot }],
    },
    catalog,
    rootDirectory: dirname(jreRoot),
  });
  const jre = await registry.resolve(toolchain.jre);

  const downloader = new StrictArtifactDownloader({
    fetchImpl: fetch,
    timeoutMs: 120_000,
  });
  const artifacts = [];
  for (const artifact of toolchain.artifacts) {
    let bytes;
    for (let attempt = 1; attempt <= 3; attempt += 1) {
      try {
        bytes = await downloader.download(artifact, {
          approvedHosts: catalog.approvedArtifactHosts,
        });
        break;
      } catch (error) {
        if (attempt === 3) {
          error.message = `${error.message}: ${artifact.coordinate}`;
          throw error;
        }
      }
    }
    artifacts.push({
      coordinate: artifact.coordinate,
      url: artifact.url,
      bytes,
    });
  }
  assert.equal(artifacts.length, toolchain.artifacts.length);

  await mkdir(workspaceRoot, { recursive: true, mode: 0o700 });
  const adapter = new BubblewrapSandboxAdapter({
    jreRegistry: registry,
    workspaceRoot,
    bubblewrapPath,
    prlimitPath,
    runner: runChecked,
  });
  const plan = createAssemblyPlan(toolchain);
  const installed = await adapter.assemble({
    schemaVersion: 1,
    sandbox: {
      ephemeralWorkspace: true,
      nonRoot: true,
      readOnlyBaseFilesystem: true,
      noSecrets: true,
      noDockerSocket: true,
      boundedResources: true,
    },
    jre,
    plan,
    artifacts,
  });

  const unixArgsPath =
    `libraries/net/neoforged/neoforge/${toolchain.loader.version}/unix_args.txt`;
  assert.ok(installed.files.has(unixArgsPath), "installer did not generate unix_args.txt");
  assert.ok(installed.files.has(
    `libraries/net/minecraft/server/${toolchain.minecraftVersion}/` +
    `server-${toolchain.minecraftVersion}.jar`,
  ), "installer output omitted the reviewed Mojang server");
  for (const artifact of toolchain.artifacts) {
    if (artifact.coordinate.endsWith(":installer") ||
      artifact.coordinate ===
        `com.mojang:minecraft-server:${toolchain.minecraftVersion}:server`) continue;
    assert.ok(installed.files.has(catalogArtifactPath(artifact)),
      `installer output omitted ${artifact.coordinate}`);
  }
  assert.equal(installed.attestation.network, "disabled");

  const launchRoot = join(workspaceRoot, "launch-verification");
  await rm(launchRoot, { recursive: true, force: true });
  await mkdir(launchRoot, { recursive: true, mode: 0o700 });
  try {
    for (const [relative, file] of installed.files) {
      const target = join(launchRoot, ...relative.split("/"));
      await mkdir(dirname(target), { recursive: true, mode: 0o700 });
      await writeFile(target, file.bytes, { mode: file.mode, flag: "wx" });
    }
    const result = await runBounded(prlimitPath, [
      "--as=3221225472",
      "--fsize=268435456",
      "--nproc=256",
      "--cpu=60",
      "--",
      bubblewrapPath,
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
      "--dir", "/server",
      "--dir", "/jre",
      "--ro-bind", "/lib", "/lib",
      "--ro-bind", "/lib64", "/lib64",
      "--ro-bind", "/usr/lib", "/usr/lib",
      "--ro-bind", jreRoot, "/jre",
      "--bind", launchRoot, "/server",
      "--chdir", "/server",
      "/jre/bin/java",
      "-Xms128m",
      "-Xmx1024m",
      "-Xss512k",
      "-XX:MaxMetaspaceSize=512m",
      "-XX:CompressedClassSpaceSize=256m",
      "-XX:ReservedCodeCacheSize=128m",
      "-XX:MaxDirectMemorySize=256m",
      "-XX:ActiveProcessorCount=2",
      ...plan.launch.arguments.slice(0, -1),
      "--help",
    ], 90_000);
    assert.equal(result.timedOut, false, "NeoForge help launch exceeded its bound");
    assert.equal(result.code, 0, result.output);
    assert.match(result.output, /(?:help|usage|option|minecraft|neoforge)/i,
      "NeoForge did not reach its command-line bootstrap");
  } finally {
    await rm(launchRoot, { recursive: true, force: true });
    assert.deepEqual(await entries(workspaceRoot), []);
  }
});

function catalogArtifactPath(artifact) {
  const [group, name, version, classifier] = artifact.coordinate.split(":");
  const extension = new URL(artifact.url).pathname.split("/").at(-1).split(".").at(-1);
  return `libraries/${group.replaceAll(".", "/")}/${name}/${version}/` +
    `${name}-${version}${classifier ? `-${classifier}` : ""}.${extension}`;
}

async function runBounded(command, args, timeoutMs) {
  return await new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, {
      cwd: "/",
      env: Object.freeze({}),
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    const append = (chunk) => {
      output = `${output}${chunk.toString()}`.slice(-64 * 1024);
    };
    child.stdout.on("data", append);
    child.stderr.on("data", append);
    child.once("error", reject);
    const timer = setTimeout(() => child.kill("SIGKILL"), timeoutMs);
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      resolvePromise({ code, signal, output, timedOut: signal === "SIGKILL" });
    });
  });
}

async function runChecked(command, args, { timeoutMs, cwd, env }) {
  const result = await runBounded(command, args, timeoutMs, cwd, env);
  if (result.timedOut || result.code !== 0) process.stderr.write(result.output);
  assert.equal(result.timedOut, false, `sandbox command timed out\n${result.output}`);
  assert.equal(result.code, 0, `sandbox command failed\n${result.output}`);
}

function requiredAbsolutePath(name) {
  const value = process.env[name];
  assert.ok(value, `${name} is required`);
  return absolutePath(value);
}

function optionalAbsolutePath(name, fallback) {
  return absolutePath(process.env[name] ?? fallback);
}

function absolutePath(value) {
  const normalized = resolve(value);
  assert.equal(value, normalized, "integration paths must be absolute and normalized");
  return normalized;
}

async function entries(path) {
  const metadata = await stat(path);
  assert.ok(metadata.isDirectory());
  return await (await import("node:fs/promises")).readdir(path);
}
