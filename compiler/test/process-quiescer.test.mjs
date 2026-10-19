import assert from "node:assert/strict";
import { mkdir, rm, writeFile } from "node:fs/promises";
import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import test from "node:test";
import {
  liveProcessIds,
  quiesceProcfs,
} from "../src/process-quiescer.mjs";

const root = resolve(".process-quiescer-test");

test.afterEach(async () => {
  await rm(root, { recursive: true, force: true });
});

test("procfs quiescer kills fork-racing live processes and permits zombies", async () => {
  await processEntry(1, "S", "trusted node");
  await processEntry(2, "S", "installer (worker)");
  await processEntry(3, "Z", "zombie");
  const killed = [];
  let waits = 0;
  await quiesceProcfs({
    procRoot: root,
    selfPid: 1,
    timeoutMs: 100,
    scanDelayMs: 0,
    requiredEmptyScans: 3,
    kill: (pid, signal) => {
      assert.equal(signal, "SIGKILL");
      killed.push(pid);
      writeFileSync(resolve(root, String(pid), "stat"), statLine(pid, "Z", "dead"));
      writeFileSync(resolve(root, String(pid), "task", String(pid), "stat"),
        statLine(pid, "Z", "dead"));
    },
    now: () => waits,
    wait: async () => {
      waits += 1;
      if (waits === 1) await processEntry(4, "S", "double fork");
    },
  });
  assert.deepEqual(killed, [2, 4]);
  assert.deepEqual(await liveProcessIds(root, 1), []);
  assert.ok(waits >= 4);
});

test("procfs quiescer kills a process with a zombie leader and live worker thread", async () => {
  await processEntry(1, "S", "trusted node");
  await processEntry(2, "Z", "zombie leader");
  await taskEntry(2, 7, "S", "live worker");
  const killed = [];
  await quiesceProcfs({
    procRoot: root,
    selfPid: 1,
    timeoutMs: 100,
    scanDelayMs: 0,
    requiredEmptyScans: 2,
    kill: (pid) => {
      killed.push(pid);
      writeFileSync(resolve(root, String(pid), "task", "7", "stat"),
        statLine(7, "X", "dead worker"));
    },
    wait: async () => {},
  });
  assert.deepEqual(killed, [2]);
  assert.deepEqual(await liveProcessIds(root, 1), []);
});

test("procfs quiescer fails closed when a live process cannot be eliminated", async () => {
  await processEntry(1, "S", "trusted node");
  await processEntry(2, "S", "escaped child");
  let clock = 0;
  await assert.rejects(() => quiesceProcfs({
    procRoot: root,
    selfPid: 1,
    timeoutMs: 3,
    scanDelayMs: 0,
    requiredEmptyScans: 2,
    kill: () => {},
    now: () => clock++,
    wait: async () => {},
  }), /installer_process_failed/);
});

async function processEntry(pid, state, command) {
  const directory = resolve(root, String(pid));
  await mkdir(resolve(directory, "task", String(pid)), { recursive: true });
  await writeFile(resolve(directory, "stat"), statLine(pid, state, command));
  await writeFile(resolve(directory, "task", String(pid), "stat"),
    statLine(pid, state, command));
}

async function taskEntry(pid, tid, state, command) {
  const directory = resolve(root, String(pid), "task", String(tid));
  await mkdir(directory, { recursive: true });
  await writeFile(resolve(directory, "stat"), statLine(tid, state, command));
}

function statLine(pid, state, command) {
  return `${pid} (${command}) ${state} 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 0 0 0\n`;
}
