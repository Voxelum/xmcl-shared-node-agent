import { readdir, readFile } from "node:fs/promises";
import { setTimeout as delay } from "node:timers/promises";
import { performance } from "node:perf_hooks";
import { CompilerFailure } from "./bundle.mjs";

const PROC_ROOT = "/proc";
const QUIESCE_TIMEOUT_MS = 5_000;
const SCAN_DELAY_MS = 10;
const REQUIRED_EMPTY_SCANS = 3;

export async function quiesceContainerProcesses() {
  if (process.pid !== 1) {
    throw new CompilerFailure("installer_process_failed");
  }
  await quiesceProcfs({
    procRoot: PROC_ROOT,
    selfPid: process.pid,
    timeoutMs: QUIESCE_TIMEOUT_MS,
    scanDelayMs: SCAN_DELAY_MS,
    requiredEmptyScans: REQUIRED_EMPTY_SCANS,
  });
}

export async function quiesceProcfs({
  procRoot,
  selfPid,
  timeoutMs,
  scanDelayMs,
  requiredEmptyScans,
  kill = process.kill,
  now = () => performance.now(),
  wait = delay,
} = {}) {
  if (typeof procRoot !== "string" || !procRoot ||
    !Number.isSafeInteger(selfPid) || selfPid < 1 ||
    !Number.isSafeInteger(timeoutMs) || timeoutMs < 1 ||
    !Number.isSafeInteger(scanDelayMs) || scanDelayMs < 0 ||
    !Number.isSafeInteger(requiredEmptyScans) || requiredEmptyScans < 2 ||
    typeof kill !== "function" || typeof now !== "function" || typeof wait !== "function") {
    throw new CompilerFailure("installer_process_failed");
  }
  const deadline = now() + timeoutMs;
  let emptyScans = 0;
  while (now() <= deadline) {
    const live = await liveProcessIds(procRoot, selfPid);
    if (live.length === 0) {
      emptyScans += 1;
      if (emptyScans >= requiredEmptyScans) return;
    } else {
      emptyScans = 0;
      for (const pid of live) {
        try {
          kill(pid, "SIGKILL");
        } catch (error) {
          if (error?.code !== "ESRCH") {
            throw new CompilerFailure("installer_process_failed");
          }
        }
      }
    }
    await wait(scanDelayMs);
  }
  throw new CompilerFailure("installer_process_failed");
}

export async function liveProcessIds(procRoot = PROC_ROOT, selfPid = process.pid) {
  let entries;
  try {
    entries = await readdir(procRoot, { withFileTypes: true });
  } catch {
    throw new CompilerFailure("installer_process_failed");
  }
  const result = [];
  for (const entry of entries) {
    if (!entry.isDirectory() || !/^[1-9][0-9]*$/.test(entry.name)) continue;
    const pid = Number(entry.name);
    if (!Number.isSafeInteger(pid) || pid === selfPid) continue;
    if (await processHasLiveTask(procRoot, pid)) result.push(pid);
  }
  return result.sort((left, right) => left - right);
}

async function processHasLiveTask(procRoot, pid) {
  let tasks;
  try {
    tasks = await readdir(`${procRoot}/${pid}/task`, { withFileTypes: true });
  } catch (error) {
    if (vanished(error)) return false;
    throw new CompilerFailure("installer_process_failed");
  }
  for (const task of tasks) {
    if (!task.isDirectory() || !/^[1-9][0-9]*$/.test(task.name)) continue;
    let stat;
    try {
      stat = await readFile(`${procRoot}/${pid}/task/${task.name}/stat`, "utf8");
    } catch (error) {
      if (vanished(error)) continue;
      throw new CompilerFailure("installer_process_failed");
    }
    const state = taskState(stat);
    if (state !== "Z" && state !== "X") return true;
  }
  return false;
}

function taskState(stat) {
  const commandEnd = stat.lastIndexOf(")");
  if (commandEnd < 2 || stat[commandEnd + 1] !== " " ||
    !stat[commandEnd + 2] || stat[commandEnd + 3] !== " ") {
    throw new CompilerFailure("installer_process_failed");
  }
  return stat[commandEnd + 2];
}

function vanished(error) {
  return error?.code === "ENOENT" || error?.code === "ESRCH";
}
