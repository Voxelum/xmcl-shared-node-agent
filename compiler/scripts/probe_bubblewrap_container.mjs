#!/usr/bin/env node
import { spawn } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";
import { join, resolve } from "node:path";

const DEFAULT_IMAGE = "xmcl-compiler-aca-probe:local";
const REPOSITORY_ROOT = resolve(fileURLToPath(new URL("../..", import.meta.url)));

export function dockerBuildArguments({ image = DEFAULT_IMAGE } = {}) {
  return [
    "build",
    "--file", join(REPOSITORY_ROOT, "compiler", "Dockerfile.aca"),
    "--tag", image,
    REPOSITORY_ROOT,
  ];
}

export function dockerRunArguments({ image = DEFAULT_IMAGE } = {}) {
  return [
    "run",
    "--rm",
    "--read-only",
    "--user", "10001:10001",
    "--cap-drop", "ALL",
    "--security-opt", "no-new-privileges",
    "--network", "none",
    "--tmpfs", "/run/xmcl-compiler:rw,noexec,nosuid,nodev,size=64m,uid=10001,gid=10001,mode=0700",
    "--tmpfs", "/var/lib/xmcl-compiler:rw,noexec,nosuid,nodev,size=512m,uid=10001,gid=10001,mode=0700",
    image,
    "probe",
  ];
}

export async function probeBubblewrapContainer({
  image = DEFAULT_IMAGE,
  runner = runDocker,
} = {}) {
  await runner(dockerBuildArguments({ image }));
  await runner(dockerRunArguments({ image }));
}

async function runDocker(args) {
  await new Promise((resolve, reject) => {
    const child = spawn("docker", args, { stdio: "inherit" });
    child.once("error", reject);
    child.once("exit", (code, signal) => {
      if (code === 0 && !signal) resolve();
      else reject(new Error(`docker ${args[0]} failed (${code ?? signal})`));
    });
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  probeBubblewrapContainer().catch((error) => {
    process.stderr.write(`container bubblewrap probe failed: ${error.message}\n`);
    process.exitCode = 1;
  });
}
