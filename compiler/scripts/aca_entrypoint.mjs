#!/usr/bin/env node
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { chmod, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { BubblewrapSandboxAdapter } from "../src/bubblewrap-sandbox.mjs";
import { FilesystemCompilerJobQueue } from "../src/job-queue.mjs";
import { ReviewedToolchainCatalog } from "../src/reviewed-builder.mjs";
import { FilesystemReplayStore } from "../src/service-identity.mjs";
import { VerifiedReadOnlyJreRegistry } from "../src/verified-jre-registry.mjs";
import { runBubblewrapProbe } from "./probe_bubblewrap.mjs";
import { acquireExecutionLease } from "../src/aca-job-runner.mjs";

const CONFIG_ROOT = "/run/xmcl-compiler/config";
const sensitiveAzureVariables = [
  "AZURE_STORAGE_ACCOUNT",
  "AZURE_STORAGE_KEY",
  "AZURE_STORAGE_CONNECTION_STRING",
  "AZURE_CLIENT_SECRET",
];

export async function runAcaEntrypoint({
  mode = process.argv[2],
  env = process.env,
  runner = runProcess,
} = {}) {
  if (mode === "probe") return await runAcaCanary();
  if (mode === "probe-inner") return await runProductionSandboxCanary();
  if (mode !== "job") throw new Error("entrypoint mode must be probe or job");
  for (const name of sensitiveAzureVariables) {
    if (env[name]) throw new Error(`forbidden credential environment variable: ${name}`);
  }

  const workerConfig = decodeBase64(env.XMCL_COMPILER_WORKER_CONFIG_B64, 64 * 1024);
  const serviceSecret = decodeBase64(env.XMCL_COMPILER_HMAC_SECRET_B64, 64 * 1024);
  const delivery = decodeBase64(env.XMCL_COMPILER_DELIVERY_B64, 90 * 1024);
  if (serviceSecret.byteLength < 32) throw new Error("service identity secret is too short");
  JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(workerConfig));
  JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(delivery));

  await prepareDirectories();
  await Promise.all([
    writePrivate(`${CONFIG_ROOT}/worker.json`, workerConfig),
    writePrivate(`${CONFIG_ROOT}/service-hmac-secret`, serviceSecret),
    writePrivate(`${CONFIG_ROOT}/delivery.json`, delivery),
  ]);

  const result = await runner("/usr/bin/bwrap", outerSandboxArguments(), {
    env: Object.freeze({}),
  });
  if (result.code !== 0) throw new Error(`compiler job exited with status ${result.code}`);
  return result;
}

export async function runAcaCanary() {
  await prepareDirectories();
  const root = `/var/lib/xmcl-compiler/canary-${randomUUID()}`;
  try {
    const replay = new FilesystemReplayStore({
      directory: `${root}/replay`,
      maxEntries: 16,
    });
    await replay.initialize();
    const now = Date.now();
    const first = await replay.consume({
      key: "aca-canary-replay-key",
      expiresAt: now + 60_000,
      now,
    });
    const duplicate = await replay.consume({
      key: "aca-canary-replay-key",
      expiresAt: now + 60_000,
      now,
    });
    const queue = new FilesystemCompilerJobQueue({ directory: `${root}/jobs` });
    await queue.initialize();
    await queue.enqueue({
      id: "aca-canary",
      deploymentId: "aca-canary",
      jobFingerprint: "a".repeat(64),
      body: new TextEncoder().encode("{}"),
    });
    const claimed = await queue.claim();
    await queue.finish(claimed.id, {
      kind: "terminal",
      result: { status: "canary" },
    });
    if (first !== true || duplicate !== false ||
      (await queue.counts()).archived !== 1) {
      throw new Error("durable replay/queue state canary failed");
    }
    const lease = await acquireExecutionLease("aca-canary", {
      root: `${root}/leases`,
      leaseMs: 60_000,
    });
    let duplicateLeaseRejected = false;
    try {
      await acquireExecutionLease("aca-canary", {
        root: `${root}/leases`,
        leaseMs: 60_000,
      });
    } catch (error) {
      duplicateLeaseRejected = error?.code === "execution_already_active";
    }
    await lease.release();
    if (!duplicateLeaseRejected) throw new Error("durable execution lease canary failed");
    const outer = await runProcess("/usr/bin/bwrap", outerSandboxArguments([
      "/usr/local/bin/node",
      "/opt/xmcl-compiler/app/scripts/aca_entrypoint.mjs",
      "probe-inner",
    ]), { env: Object.freeze({}) });
    if (outer.code !== 0) throw new Error("production outer sandbox canary failed");
    return {
      status: "compatible",
      productionOuterSandbox: true,
      durableExecutionLease: true,
      durableReplayState: true,
      durableQueueState: true,
    };
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

export async function runProductionSandboxCanary() {
  const catalog = new ReviewedToolchainCatalog(JSON.parse(await readFile(
    "/opt/xmcl-compiler/app/toolchain-catalog.lock.json",
    "utf8",
  )), { supportedFamilies: ["neoforge"] });
  const registry = await VerifiedReadOnlyJreRegistry.load({
    path: "/opt/xmcl-compiler/config/jre-registry.json",
    catalog,
    rootDirectory: "/opt/xmcl/jres",
  });
  await registry.initialize();
  const adapter = new BubblewrapSandboxAdapter({
    jreRegistry: registry,
    workspaceRoot: "/run/xmcl-compiler/workspaces",
  });
  await adapter.initialize();
  return {
    ...await runBubblewrapProbe(),
    productionJreRegistry: true,
    productionSandboxAdapter: true,
  };
}

export function outerSandboxArguments(command = [
  "/usr/local/bin/node",
  "/opt/xmcl-compiler/app/src/aca-job-runner.mjs",
]) {
  return [
    "--die-with-parent",
    "--new-session",
    "--unshare-user",
    "--unshare-ipc",
    "--unshare-pid",
    "--unshare-uts",
    "--unshare-cgroup-try",
    "--uid", "10001",
    "--gid", "10001",
    "--clearenv",
    "--setenv", "HOME", "/nonexistent",
    "--setenv", "NODE_ENV", "production",
    "--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
    "--tmpfs", "/",
    "--proc", "/proc",
    "--dev", "/dev",
    "--dir", "/opt",
    "--dir", "/etc",
    "--dir", "/run",
    "--dir", "/run/xmcl-compiler",
    "--dir", "/var",
    "--dir", "/var/lib",
    "--dir", "/var/lib/xmcl-compiler",
    "--ro-bind", "/bin", "/bin",
    "--ro-bind", "/lib", "/lib",
    "--ro-bind", "/lib64", "/lib64",
    "--ro-bind", "/usr", "/usr",
    "--ro-bind", "/usr/local", "/usr/local",
    "--ro-bind", "/etc/hosts", "/etc/hosts",
    "--ro-bind", "/etc/nsswitch.conf", "/etc/nsswitch.conf",
    "--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
    "--ro-bind", "/etc/ssl", "/etc/ssl",
    "--ro-bind", "/opt/xmcl-compiler", "/opt/xmcl-compiler",
    "--ro-bind", "/opt/xmcl/jres", "/opt/xmcl/jres",
    "--ro-bind", CONFIG_ROOT, CONFIG_ROOT,
    "--ro-bind", "/var/lib/xmcl-compiler/artifacts", "/var/lib/xmcl-compiler/artifacts",
    "--bind", "/run/xmcl-compiler/workspaces", "/run/xmcl-compiler/workspaces",
    "--bind", "/var/lib/xmcl-compiler/replay", "/var/lib/xmcl-compiler/replay",
    "--bind", "/var/lib/xmcl-compiler/jobs", "/var/lib/xmcl-compiler/jobs",
    "--bind", "/var/lib/xmcl-compiler/leases", "/var/lib/xmcl-compiler/leases",
    "--chdir", "/opt/xmcl-compiler/app",
    ...command,
  ];
}

export function decodeBase64(value, maximumBytes) {
  if (typeof value !== "string" || value.length === 0 ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) {
    throw new Error("invalid base64 configuration");
  }
  const bytes = Uint8Array.from(Buffer.from(value, "base64"));
  if (bytes.byteLength === 0 || bytes.byteLength > maximumBytes ||
    Buffer.from(bytes).toString("base64") !== value) {
    throw new Error("invalid base64 configuration");
  }
  return bytes;
}

async function prepareDirectories() {
  for (const path of [
    CONFIG_ROOT,
    "/run/xmcl-compiler/workspaces",
    "/var/lib/xmcl-compiler/replay",
    "/var/lib/xmcl-compiler/jobs",
    "/var/lib/xmcl-compiler/leases",
    "/var/lib/xmcl-compiler/artifacts",
  ]) {
    await mkdir(path, { recursive: true, mode: 0o700 });
    await chmod(path, 0o700);
  }
}

async function writePrivate(path, bytes) {
  await writeFile(path, bytes, { flag: "wx", mode: 0o400 });
  await chmod(path, 0o400);
}

async function runProcess(command, args, { env }) {
  return await new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      env,
      stdio: "inherit",
    });
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (signal) reject(new Error(`compiler job terminated by ${signal}`));
      else resolve({ code });
    });
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  runAcaEntrypoint().then((result) => {
    if (result?.status === "compatible") {
      process.stdout.write(`${JSON.stringify(result)}\n`);
    }
  }).catch((error) => {
    process.stderr.write(`xmcl compiler ACA entrypoint failed: ${error.message}\n`);
    process.exitCode = 1;
  });
}
