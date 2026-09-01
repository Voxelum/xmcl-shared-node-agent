import { createHash, randomUUID } from "node:crypto";
import {
  mkdir,
  open,
  readFile,
  rename,
  rm,
  stat,
} from "node:fs/promises";
import { isAbsolute } from "node:path";
import { pathToFileURL } from "node:url";
import {
  PRODUCTION_PATHS,
  createProductionCompilerWorker,
} from "./production-composition.mjs";

const DELIVERY_PATH = "/run/xmcl-compiler/config/delivery.json";
const MAX_DELIVERY_BYTES = 90 * 1024;
const MAX_BODY_BYTES = 60 * 1024;

const LEASE_ROOT = "/var/lib/xmcl-compiler/leases";
const LEASE_MS = 25 * 60 * 1000;

export async function runAcaCompilerJob({
  deliveryPath = DELIVERY_PATH,
  workerFactory = createProductionCompilerWorker,
  acquireLease = acquireExecutionLease,
  pathsFactory = acaPathsFor,
} = {}) {
  const delivery = parseAcaDelivery(await readFile(deliveryPath));
  const paths = pathsFactory(delivery.requestId);
  const lease = await acquireLease(delivery.requestId);
  try {
    const worker = await workerFactory({
      paths,
      startWorker: false,
    });
    await worker.initialize();
    const accepted = await worker.consume(delivery);
    const outcome = await worker.runOne(delivery.requestId);
    if (outcome.terminal !== true) {
      throw new AcaJobFailure(outcome.result?.status ?? "job_not_terminal");
    }
    return { accepted, outcome: outcome.result };
  } finally {
    await lease.release();
  }
}

export function parseAcaDelivery(bytes) {
  if (!(bytes instanceof Uint8Array) || bytes.byteLength < 2 ||
    bytes.byteLength > MAX_DELIVERY_BYTES) {
    throw new AcaJobFailure("invalid_delivery");
  }
  let value;
  try {
    value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch {
    throw new AcaJobFailure("invalid_delivery");
  }
  if (!plainObject(value) || !sameKeys(value, [
    "schemaVersion", "method", "target", "headers", "bodyBase64",
  ]) || value.schemaVersion !== 1 || value.method !== "POST" ||
    value.target !== "/v1/compiler-jobs" ||
    !plainObject(value.headers) || !sameKeys(value.headers, [
      "authorization", "x-xmcl-nonce", "x-xmcl-timestamp",
    ]) || !Object.values(value.headers).every((entry) =>
      typeof entry === "string" && entry.length > 0 && entry.length <= 1024) ||
    typeof value.bodyBase64 !== "string" ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/
      .test(value.bodyBase64)) {
    throw new AcaJobFailure("invalid_delivery");
  }
  const body = Uint8Array.from(Buffer.from(value.bodyBase64, "base64"));
  if (body.byteLength < 2 || body.byteLength > MAX_BODY_BYTES ||
    Buffer.from(body).toString("base64") !== value.bodyBase64) {
    throw new AcaJobFailure("invalid_delivery");
  }
  let envelope;
  try {
    envelope = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body));
  } catch {
    throw new AcaJobFailure("invalid_delivery");
  }
  if (!plainObject(envelope) || typeof envelope.requestId !== "string" ||
    envelope.requestId.length < 1 || envelope.requestId.length > 256 ||
    /[\x00-\x1f\x7f]/.test(envelope.requestId)) {
    throw new AcaJobFailure("invalid_delivery");
  }
  return {
    method: value.method,
    target: value.target,
    headers: value.headers,
    body,
    requestId: envelope.requestId,
  };
}

export function acaPathsFor(requestId) {
  if (typeof requestId !== "string" || !requestId) {
    throw new AcaJobFailure("invalid_delivery");
  }
  const key = createHash("sha256").update(requestId).digest("hex");
  return Object.freeze({
    ...PRODUCTION_PATHS,
    configuration: "/run/xmcl-compiler/config/worker.json",
    jreRegistry: "/opt/xmcl-compiler/config/jre-registry.json",
    queueRoot: `/var/lib/xmcl-compiler/jobs/${key}`,
    secretsRoot: "/run/xmcl-compiler/config",
  });
}

export async function acquireExecutionLease(requestId, {
  root = LEASE_ROOT,
  now = Date.now,
  leaseMs = LEASE_MS,
} = {}) {
  if (typeof requestId !== "string" || !requestId || !isAbsolute(root)) {
    throw new AcaJobFailure("invalid_execution_lease");
  }
  await mkdir(root, { recursive: true, mode: 0o700 });
  const key = createHash("sha256").update(requestId).digest("hex");
  const path = `${root}/${key}`;
  const owner = randomUUID();
  for (let attempt = 0; attempt < 3; attempt += 1) {
    try {
      await mkdir(path, { mode: 0o700 });
      await durableLeaseWrite(`${path}/lease.json`, {
        schemaVersion: 1,
        owner,
        expiresAt: now() + leaseMs,
      });
      return {
        async release() {
          let current;
          try {
            current = JSON.parse(await readFile(`${path}/lease.json`, "utf8"));
          } catch {
            return;
          }
          if (current.owner === owner) await rm(path, { recursive: true, force: true });
        },
      };
    } catch (error) {
      if (error?.code !== "EEXIST") {
        await rm(path, { recursive: true, force: true }).catch(() => undefined);
        throw new AcaJobFailure("execution_lease_failed");
      }
      const expiresAt = await leaseExpiry(path, leaseMs);
      if (expiresAt > now()) throw new AcaJobFailure("execution_already_active");
      const expired = `${path}.expired-${randomUUID()}`;
      try {
        await rename(path, expired);
        await rm(expired, { recursive: true, force: true });
      } catch (renameError) {
        if (renameError?.code !== "ENOENT") {
          throw new AcaJobFailure("execution_lease_failed");
        }
      }
    }
  }
  throw new AcaJobFailure("execution_lease_failed");
}

async function durableLeaseWrite(path, value) {
  const handle = await open(path, "wx", 0o600);
  try {
    await handle.writeFile(JSON.stringify(value));
    await handle.sync();
  } finally {
    await handle.close();
  }
}

async function leaseExpiry(path, leaseMs) {
  try {
    const value = JSON.parse(await readFile(`${path}/lease.json`, "utf8"));
    if (value?.schemaVersion === 1 && Number.isSafeInteger(value.expiresAt)) {
      return value.expiresAt;
    }
  } catch {
    // A creator killed between mkdir and durable write retains a bounded lease.
  }
  const metadata = await stat(path);
  return Math.trunc(metadata.mtimeMs) + leaseMs;
}

export class AcaJobFailure extends Error {
  constructor(code) {
    super(code);
    this.name = "AcaJobFailure";
    this.code = code;
  }
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

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  runAcaCompilerJob().then((result) => {
    process.stdout.write(`${JSON.stringify({
      status: "completed",
      deploymentId: result.accepted.deploymentId,
      result: result.outcome.status,
    })}\n`);
  }).catch((error) => {
    process.stderr.write(`xmcl compiler ACA job failed: ${error.code ?? error.message}\n`);
    process.exitCode = 1;
  });
}
