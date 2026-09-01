#!/usr/bin/env node
import { spawn } from "node:child_process";
import { mkdtemp, readFile, readlink, rm } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const DEFAULT_JRE_ROOT = "/opt/xmcl/jres/java-runtime-delta-21";

export function bubblewrapProbeArguments({
  workspace,
  parentNetworkNamespace,
  jreRoot = DEFAULT_JRE_ROOT,
} = {}) {
  return [
    "--die-with-parent",
    "--new-session",
    "--unshare-user",
    "--unshare-ipc",
    "--unshare-pid",
    "--unshare-net",
    "--unshare-uts",
    "--unshare-cgroup-try",
    "--uid", "10001",
    "--gid", "10001",
    "--clearenv",
    "--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
    "--setenv", "XMCL_PROBE_PARENT_NETNS", parentNetworkNamespace,
    "--tmpfs", "/",
    "--proc", "/proc",
    "--dev", "/dev",
    "--dir", "/opt",
    "--dir", "/work",
    "--dir", "/jre",
    "--ro-bind", "/bin", "/bin",
    "--ro-bind", "/lib", "/lib",
    "--ro-bind", "/lib64", "/lib64",
    "--ro-bind", "/usr", "/usr",
    "--ro-bind", "/usr/local", "/usr/local",
    "--ro-bind", "/opt/xmcl-compiler", "/opt/xmcl-compiler",
    "--ro-bind", jreRoot, "/jre",
    "--bind", workspace, "/work",
    "--chdir", "/work",
    "/usr/local/bin/node",
    "/opt/xmcl-compiler/app/scripts/probe_bubblewrap.mjs",
    "--inside",
  ];
}

export async function runBubblewrapProbe({
  bubblewrapPath = "/usr/bin/bwrap",
  jreRoot = DEFAULT_JRE_ROOT,
  workspaceRoot = "/run/xmcl-compiler",
  runner = runProcess,
} = {}) {
  const status = await readFile("/proc/self/status", "utf8");
  if (!/^CapEff:\s+0+$/m.test(status) || !/^NoNewPrivs:\s+1$/m.test(status)) {
    throw new Error("outer container must have zero effective capabilities and no-new-privileges");
  }
  const workspace = await mkdtemp(join(workspaceRoot, "bwrap-probe-"));
  try {
    const parentNetworkNamespace = await readlink("/proc/self/ns/net");
    const result = await runner(
      bubblewrapPath,
      bubblewrapProbeArguments({ workspace, parentNetworkNamespace, jreRoot }),
      { env: Object.freeze({}), timeoutMs: 30_000 },
    );
    const report = JSON.parse(result.stdout);
    if (report?.status !== "compatible" || report.networkNamespaceIsolated !== true ||
      report.readOnlyJre !== true || report.readOnlySystem !== true ||
      report.writableWorkspace !== true || report.pidNamespaceIsolated !== true ||
      report.nestedBubblewrap !== true) {
      throw new Error("bubblewrap probe returned an invalid report");
    }
    return report;
  } finally {
    await rm(workspace, { recursive: true, force: true });
  }
}

async function runInsideProbe() {
  const networkNamespace = await readlink("/proc/self/ns/net");
  const noNewPrivileges = await readNoNewPrivileges();
  const parentNetworkNamespace = process.env.XMCL_PROBE_PARENT_NETNS;
  if (!parentNetworkNamespace || networkNamespace === parentNetworkNamespace ||
    process.pid !== 2 || noNewPrivileges !== true) {
    throw new Error("namespace or no-new-privileges check failed");
  }
  await runProcess("/jre/bin/java", ["-version"], {
    env: Object.freeze({}),
    timeoutMs: 10_000,
  });
  const nested = await runProcess("/usr/bin/bwrap", [
    "--die-with-parent",
    "--new-session",
    "--unshare-user",
    "--unshare-pid",
    "--unshare-net",
    "--tmpfs", "/",
    "--proc", "/proc",
    "--dev", "/dev",
    "--ro-bind", "/bin", "/bin",
    "--ro-bind", "/lib", "/lib",
    "--ro-bind", "/lib64", "/lib64",
    "/bin/true",
  ], { env: Object.freeze({}), timeoutMs: 10_000 });
  if (nested.code !== 0) throw new Error("nested bubblewrap failed");

  const report = {
    status: "compatible",
    networkNamespaceIsolated: true,
    pidNamespaceIsolated: process.pid === 2,
    readOnlyJre: await pathIsReadOnlyMount("/jre/bin/java") &&
      await writeRejected("/jre/bin/java"),
    readOnlySystem: await pathIsReadOnlyMount("/usr/bin/bwrap") &&
      await writeRejected("/usr/bin/bwrap"),
    writableWorkspace: await writeAccepted("/work/probe"),
    nestedBubblewrap: true,
    noNewPrivileges,
  };
  process.stdout.write(`${JSON.stringify(report)}\n`);
}

async function readNoNewPrivileges() {
  const status = await readFile("/proc/self/status", "utf8");
  return /^NoNewPrivs:\s+1$/m.test(status);
}

async function pathIsReadOnlyMount(path) {
  const mountInfo = await readFile("/proc/self/mountinfo", "utf8");
  let selected;
  for (const line of mountInfo.split("\n")) {
    const fields = line.split(" ");
    const separator = fields.indexOf("-");
    if (separator < 6) continue;
    const mountPoint = fields[4].replace(/\\([0-7]{3})/g, (_, octal) =>
      String.fromCharCode(Number.parseInt(octal, 8)));
    if ((path === mountPoint || path.startsWith(`${mountPoint}/`)) &&
      (!selected || mountPoint.length > selected.mountPoint.length)) {
      selected = { mountPoint, options: fields[5].split(",") };
    }
  }
  return selected?.options.includes("ro") === true;
}

async function writeRejected(path) {
  const { open } = await import("node:fs/promises");
  try {
    const handle = await open(path, "a");
    await handle.close();
    return false;
  } catch (error) {
    return ["EACCES", "EROFS", "EPERM"].includes(error?.code);
  }
}

async function writeAccepted(path) {
  const { writeFile } = await import("node:fs/promises");
  try {
    await writeFile(path, "probe", { flag: "wx", mode: 0o600 });
    return true;
  } catch {
    return false;
  }
}

async function runProcess(command, args, { env, timeoutMs }) {
  return await new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      if (stdout.length < 16_384) stdout += chunk.slice(0, 16_384 - stdout.length);
    });
    child.stderr.on("data", (chunk) => {
      if (stderr.length < 16_384) stderr += chunk.slice(0, 16_384 - stderr.length);
    });
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error("bubblewrap probe timed out"));
    }, timeoutMs);
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      clearTimeout(timer);
      if (code === 0 && !signal) resolve({ code, stdout, stderr });
      else reject(new Error(`bubblewrap probe failed (${code ?? signal}): ${stderr.trim()}`));
    });
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const action = process.argv[2] === "--inside"
    ? runInsideProbe()
    : runBubblewrapProbe().then((report) => {
      process.stdout.write(`${JSON.stringify(report)}\n`);
    });
  action.catch((error) => {
    process.stderr.write(`bubblewrap compatibility probe failed: ${error.message}\n`);
    process.exitCode = 1;
  });
}
