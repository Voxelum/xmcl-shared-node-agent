import { createHash, randomUUID } from "node:crypto";
import { mkdir, open, readFile, readdir, rename, rm } from "node:fs/promises";
import { dirname, join } from "node:path";
import { platform } from "node:process";

const decoder = new TextDecoder("utf-8", { fatal: true });

export class FilesystemCompilerJobQueue {
  constructor({ directory, now = Date.now, retryDelayMs = 1_000 } = {}) {
    if (typeof directory !== "string" || !directory ||
      typeof now !== "function" || !Number.isSafeInteger(retryDelayMs) ||
      retryDelayMs < 100 || retryDelayMs > 300_000) {
      throw new TypeError("invalid compiler job queue configuration");
    }
    this.directory = directory;
    this.pendingDirectory = join(directory, "pending");
    this.archiveDirectory = join(directory, "archive");
    this.now = now;
    this.retryDelayMs = retryDelayMs;
    this.tail = Promise.resolve();
  }

  async initialize() {
    await mkdir(this.pendingDirectory, { recursive: true });
    await mkdir(this.archiveDirectory, { recursive: true });
    await syncDirectory(this.directory);
    await syncDirectory(this.pendingDirectory);
    await syncDirectory(this.archiveDirectory);
    await this.lock(async () => {
      for (const name of await jsonFiles(this.pendingDirectory)) {
        const path = join(this.pendingDirectory, name);
        const record = await readRecord(path);
        const archived = await readRecordIfPresent(join(this.archiveDirectory, name));
        if (archived) {
          assertSameIdentity(archived, record);
          if (archived.state !== "terminal") {
            throw new Error("invalid compiler queue archive state");
          }
          await durableRemove(path);
          continue;
        }
        if (record.state === "running") {
          await atomicWrite(path, { ...record, state: "pending", updatedAt: this.now() });
        }
      }
    });
  }

  async enqueue(input) {
    validateEnqueue(input);
    return await this.lock(async () => {
      const name = `${queueKey(input.id)}.json`;
      const pendingPath = join(this.pendingDirectory, name);
      const archivePath = join(this.archiveDirectory, name);
      const archived = await readRecordIfPresent(archivePath);
      if (archived) {
        assertSameIdentity(archived, input);
        return { duplicate: true };
      }
      const existing = await readRecordIfPresent(pendingPath);
      if (existing) {
        assertSameIdentity(existing, input);
        if (existing.state === "running") {
          await atomicWrite(pendingPath, {
            ...existing,
            redeliveryBody: Buffer.from(input.body).toString("base64"),
            redeliveryBodySha256: sha256(input.body),
            updatedAt: this.now(),
          });
        } else {
          await atomicWrite(pendingPath, {
            ...existing,
            body: Buffer.from(input.body).toString("base64"),
            bodySha256: sha256(input.body),
            state: "pending",
            nextAttemptAt: this.now(),
            updatedAt: this.now(),
          });
        }
        return { duplicate: true };
      }
      const now = this.now();
      await atomicWrite(pendingPath, {
        schemaVersion: 1,
        id: input.id,
        deploymentId: input.deploymentId,
        jobFingerprint: input.jobFingerprint,
        body: Buffer.from(input.body).toString("base64"),
        bodySha256: sha256(input.body),
        state: "pending",
        attempts: 0,
        nextAttemptAt: now,
        createdAt: now,
        updatedAt: now,
      });
      return { duplicate: false };
    });
  }

  async claim() {
    return await this.lock(async () => {
      const now = this.now();
      for (const name of await jsonFiles(this.pendingDirectory)) {
        const path = join(this.pendingDirectory, name);
        const record = await readRecord(path);
        if (record.state === "running" || record.nextAttemptAt > now) continue;
        const running = {
          ...record,
          state: "running",
          attempts: record.attempts + 1,
          updatedAt: now,
        };
        await atomicWrite(path, running);
        return {
          id: running.id,
          deploymentId: running.deploymentId,
          body: decodeBody(running),
          attempts: running.attempts,
        };
      }
      return undefined;
    });
  }

  async finish(id, outcome) {
    if (typeof id !== "string" || !id ||
      !outcome || !["terminal", "retry"].includes(outcome.kind)) {
      throw new TypeError("invalid compiler queue outcome");
    }
    await this.lock(async () => {
      const name = `${queueKey(id)}.json`;
      const pendingPath = join(this.pendingDirectory, name);
      const record = await readRecord(pendingPath);
      if (record.id !== id || record.state !== "running") {
        throw new Error("compiler queue state conflict");
      }
      if (outcome.kind === "terminal") {
        await atomicWrite(join(this.archiveDirectory, name), {
          ...record,
          state: "terminal",
          terminalResult: outcome.result,
          updatedAt: this.now(),
        });
        await durableRemove(pendingPath);
      } else {
        const body = record.redeliveryBody ?? record.body;
        const bodySha256 = record.redeliveryBodySha256 ?? record.bodySha256;
        await atomicWrite(pendingPath, {
          ...record,
          body,
          bodySha256,
          redeliveryBody: undefined,
          redeliveryBodySha256: undefined,
          state: "retry",
          nextAttemptAt: this.now() + retryDelay(this.retryDelayMs, record.attempts),
          lastResult: outcome.result,
          updatedAt: this.now(),
        });
      }
    });
  }

  async counts() {
    const pending = await jsonFiles(this.pendingDirectory);
    const archive = await jsonFiles(this.archiveDirectory);
    return { pending: pending.length, archived: archive.length };
  }

  lock(action) {
    const result = this.tail.then(action, action);
    this.tail = result.then(() => undefined, () => undefined);
    return result;
  }
}

export class MemoryCompilerJobQueue {
  constructor({ now = Date.now, retryDelayMs = 10 } = {}) {
    this.now = now;
    this.retryDelayMs = retryDelayMs;
    this.pending = new Map();
    this.archive = new Map();
  }

  async initialize() {
    for (const record of this.pending.values()) {
      if (record.state === "running") record.state = "pending";
    }
  }

  async enqueue(input) {
    validateEnqueue(input);
    const archived = this.archive.get(input.id);
    if (archived) {
      assertSameIdentity(archived, input);
      return { duplicate: true };
    }
    const existing = this.pending.get(input.id);
    if (existing) {
      assertSameIdentity(existing, input);
      if (existing.state === "running") {
        existing.redeliveryBody = input.body.slice();
      } else {
        existing.body = input.body.slice();
        existing.state = "pending";
        existing.nextAttemptAt = this.now();
      }
      return { duplicate: true };
    }
    this.pending.set(input.id, {
      id: input.id,
      deploymentId: input.deploymentId,
      jobFingerprint: input.jobFingerprint,
      body: input.body.slice(),
      state: "pending",
      attempts: 0,
      nextAttemptAt: this.now(),
    });
    return { duplicate: false };
  }

  async claim() {
    for (const record of this.pending.values()) {
      if (record.state === "running" || record.nextAttemptAt > this.now()) continue;
      record.state = "running";
      record.attempts += 1;
      return {
        id: record.id,
        deploymentId: record.deploymentId,
        body: record.body.slice(),
        attempts: record.attempts,
      };
    }
    return undefined;
  }

  async finish(id, outcome) {
    const record = this.pending.get(id);
    if (!record || record.state !== "running") throw new Error("compiler queue state conflict");
    if (outcome.kind === "terminal") {
      this.pending.delete(id);
      this.archive.set(id, { ...record, state: "terminal", terminalResult: outcome.result });
    } else {
      if (record.redeliveryBody) record.body = record.redeliveryBody;
      delete record.redeliveryBody;
      record.state = "retry";
      record.nextAttemptAt = this.now() + retryDelay(this.retryDelayMs, record.attempts);
      record.lastResult = outcome.result;
    }
  }

  async counts() {
    return { pending: this.pending.size, archived: this.archive.size };
  }
}

function validateEnqueue(input) {
  if (!input || typeof input.id !== "string" || !input.id ||
    typeof input.deploymentId !== "string" || !input.deploymentId ||
    typeof input.jobFingerprint !== "string" || !/^[a-f0-9]{64}$/.test(input.jobFingerprint) ||
    !(input.body instanceof Uint8Array)) {
    throw new TypeError("invalid compiler queue job");
  }
}

function assertSameIdentity(record, input) {
  if (record.id !== input.id || record.deploymentId !== input.deploymentId ||
    record.jobFingerprint !== input.jobFingerprint) {
    const error = new Error("compiler queue idempotency conflict");
    error.code = "idempotency_conflict";
    throw error;
  }
}

async function jsonFiles(directory) {
  return (await readdir(directory).catch(() => []))
    .filter((name) => /^[a-f0-9]{64}\.json$/.test(name))
    .sort();
}

async function readRecordIfPresent(path) {
  try {
    return await readRecord(path);
  } catch (error) {
    if (error?.code === "ENOENT") return undefined;
    throw error;
  }
}

async function readRecord(path) {
  const value = JSON.parse(decoder.decode(await readFile(path)));
  if (!value || value.schemaVersion !== 1 || typeof value.id !== "string" ||
    typeof value.deploymentId !== "string" || typeof value.jobFingerprint !== "string" ||
    typeof value.body !== "string" || typeof value.bodySha256 !== "string" ||
    !["pending", "running", "retry", "terminal"].includes(value.state) ||
    !Number.isSafeInteger(value.attempts) || !Number.isSafeInteger(value.nextAttemptAt)) {
    throw new Error("invalid persisted compiler job");
  }
  decodeBody(value);
  if ((value.redeliveryBody === undefined) !==
      (value.redeliveryBodySha256 === undefined) ||
    (value.redeliveryBody !== undefined &&
      sha256(Uint8Array.from(Buffer.from(value.redeliveryBody, "base64"))) !==
        value.redeliveryBodySha256)) {
    throw new Error("invalid persisted compiler job");
  }
  return value;
}

function decodeBody(record) {
  const body = Uint8Array.from(Buffer.from(record.body, "base64"));
  if (sha256(body) !== record.bodySha256) throw new Error("invalid persisted compiler job");
  return body;
}

async function atomicWrite(path, value) {
  const temporary = `${path}.${randomUUID()}.partial`;
  let handle;
  try {
    handle = await open(temporary, "wx", 0o600);
    await handle.writeFile(JSON.stringify(value));
    await handle.sync();
    await handle.close();
    handle = undefined;
    await rename(temporary, path);
    await syncDirectory(dirname(path));
  } catch (error) {
    await handle?.close().catch(() => undefined);
    await rm(temporary, { force: true }).catch(() => undefined);
    throw error;
  }
}

async function durableRemove(path) {
  await rm(path, { force: true });
  await syncDirectory(dirname(path));
}

async function syncDirectory(directory) {
  let handle;
  try {
    handle = await open(directory, "r");
    await handle.sync();
  } catch (error) {
    if (platform === "win32" &&
      ["EACCES", "EBADF", "EISDIR", "EINVAL", "EPERM", "UNKNOWN"]
        .includes(error?.code)) {
      return;
    }
    throw error;
  } finally {
    await handle?.close().catch(() => undefined);
  }
}

function queueKey(id) {
  return createHash("sha256").update(id).digest("hex");
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function retryDelay(base, attempts) {
  return Math.min(base * (2 ** Math.min(Math.max(attempts - 1, 0), 8)), 300_000);
}
