import { createServer } from "node:http";
import { createHash } from "node:crypto";
import { CompilerFailure } from "./bundle.mjs";
import {
  CompilerWorker,
  validateCompilerJob,
  verifyGrantSet,
} from "./compiler.mjs";
import {
  ReviewedRuntimeBuilder,
  ReviewedToolchainCatalog,
  StrictArtifactDownloader,
} from "./reviewed-builder.mjs";
import { MemoryCompilerJobQueue } from "./job-queue.mjs";

const encoder = new TextEncoder();
const decoder = new TextDecoder("utf-8", { fatal: true });
const MAX_REQUEST_BYTES = 256 * 1024;
const MAX_CALLBACK_BYTES = 4 * 1024 * 1024;
const MAX_ENVELOPE_LIFETIME_MS = 5 * 60 * 1000;
const requiredSandboxCapabilities = Object.freeze({
  ephemeralWorkspace: true,
  nonRoot: true,
  readOnlyBaseFilesystem: true,
  noSecrets: true,
  noDockerSocket: true,
  boundedResources: true,
  egress: "approved-artifacts-only",
});

/**
 * HTTP and queue-message boundary for compiler jobs. It has no default
 * executable composition: without all reviewed dependencies, it serves only
 * liveness and rejects job delivery with `compiler_unavailable`.
 */
export class CompilerHttpWorker {
  constructor(options = {}) {
    this.now = typeof options.now === "function" ? options.now : Date.now;
    this.maxConcurrentJobs = validConcurrency(options.maxConcurrentJobs)
      ? options.maxConcurrentJobs
      : 1;
    this.objectRequestTimeoutMs =
      Number.isSafeInteger(options.objectRequestTimeoutMs) &&
        options.objectRequestTimeoutMs >= 1_000 &&
        options.objectRequestTimeoutMs <= 900_000
        ? options.objectRequestTimeoutMs
        : 30_000;
    this.allowInsecureForTests = options.allowInsecureForTests === true;
    this.queue = options.jobQueue ??
      (this.allowInsecureForTests ? new MemoryCompilerJobQueue() : undefined);
    this.activeJobs = 0;
    this.started = false;
    this.startPromise = undefined;
    this.initializePromise = undefined;
    this.stopping = false;
    this.loops = [];
    this.waiters = new Set();
    this.composition = createComposition(options, this.now, this.allowInsecureForTests);
    this.ready = Boolean(this.composition && validJobQueue(this.queue));
  }

  health() {
    return { status: "ok", ready: this.ready, activeJobs: this.activeJobs };
  }

  /**
   * Consumes one authenticated queue payload. Queue adapters can call this
   * directly; the HTTP endpoint below passes the raw JSON body unchanged.
   */
  async consume({ method = "POST", target = "/v1/compiler-jobs", headers, body, transport } = {}) {
    if (!this.ready) throw new HttpWorkerError(503, "compiler_unavailable");
    if (method !== "POST" || target !== "/v1/compiler-jobs") {
      throw new HttpWorkerError(400, "invalid_request");
    }
    await verifyIncoming(this.composition.requestAuthenticator, {
      method,
      target,
      headers,
      body,
      transport,
    });
    const envelope = parseEnvelope(body, this.now());
    try {
      await this.queue.enqueue({
        id: envelope.requestId,
        deploymentId: envelope.job.deploymentId,
        jobFingerprint: createHash("sha256")
          .update(JSON.stringify(envelope.job))
          .digest("hex"),
        body,
      });
    } catch (error) {
      if (error?.code === "idempotency_conflict") {
        throw new HttpWorkerError(409, "idempotency_conflict");
      }
      throw error;
    }
    this.wake();
    return { status: "accepted", deploymentId: envelope.job.deploymentId };
  }

  async start() {
    if (!this.ready) throw new HttpWorkerError(503, "compiler_unavailable");
    if (this.started) return;
    if (!this.startPromise) {
      this.startPromise = (async () => {
        await this.initialize();
        this.started = true;
        this.stopping = false;
        this.loops = Array.from(
          { length: this.maxConcurrentJobs },
          () => this.runLoop(),
        );
      })();
    }
    await this.startPromise;
  }

  async initialize() {
    if (!this.ready) throw new HttpWorkerError(503, "compiler_unavailable");
    if (!this.initializePromise) {
      this.initializePromise = this.queue.initialize();
    }
    await this.initializePromise;
  }

  async runOne(id) {
    if (this.started) throw new Error("cannot run one job while background consumers are active");
    await this.initialize();
    const queued = await this.queue.claim(id);
    if (!queued) {
      const result = id && typeof this.queue.terminalResult === "function"
        ? await this.queue.terminalResult(id)
        : undefined;
      return result
        ? { terminal: true, result }
        : { terminal: false, result: { status: "queue_empty" } };
    }
    return await this.processQueued(queued);
  }

  async stop() {
    if (!this.started) return;
    this.stopping = true;
    this.wake();
    await Promise.allSettled(this.loops);
    this.loops = [];
    this.started = false;
    this.startPromise = undefined;
  }

  async runLoop() {
    while (!this.stopping) {
      const queued = await this.queue.claim();
      if (!queued) {
        await this.waitForWork();
        continue;
      }
      await this.processQueued(queued);
    }
  }

  async processQueued(queued) {
    this.activeJobs += 1;
    let result;
    try {
      const envelope = parseQueuedEnvelope(queued.body);
      const controlPlane = new CallbackBoundControlPlane({
        callbacks: this.composition.callbacks,
        job: envelope.job,
        now: this.now,
      });
      const worker = new CompilerWorker({
        builder: this.composition.builder,
        fetchImpl: this.composition.fetchImpl,
        controlPlane,
        requestTimeoutMs: this.objectRequestTimeoutMs,
      });
      result = await worker.run(envelope.job);
      const terminal = controlPlane.terminalCallbackDelivered === true &&
        ["published", "failed"].includes(result?.status);
      if (!terminal) {
        console.warn("xmcl compiler queued job remains retryable", {
          deploymentId: queued.deploymentId,
          status: result?.status,
        });
      }
      await this.queue.finish(queued.id, {
        kind: terminal ? "terminal" : "retry",
        result,
      });
      return { terminal, result };
    } catch (error) {
      console.error("xmcl compiler queued job retry", {
        deploymentId: queued.deploymentId,
        code: typeof error?.code === "string" ? error.code : "compiler_failed",
      });
      await this.queue.finish(queued.id, {
        kind: "retry",
        result: {
          status: "queue_retry",
          code: typeof error?.code === "string" ? error.code : "compiler_failed",
        },
      }).catch(() => undefined);
      return {
        terminal: false,
        result: {
          status: "queue_retry",
          code: typeof error?.code === "string" ? error.code : "compiler_failed",
        },
      };
    } finally {
      this.activeJobs -= 1;
    }
  }

  waitForWork() {
    return new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.waiters.delete(done);
        resolve();
      }, 250);
      timer.unref?.();
      const done = () => {
        clearTimeout(timer);
        this.waiters.delete(done);
        resolve();
      };
      this.waiters.add(done);
    });
  }

  wake() {
    for (const resolve of this.waiters) resolve();
  }

  async handleHttp(request, response) {
    const target = parseTarget(request.url);
    if (!target) return sendJson(response, 404, { code: "not_found" });
    if (request.method === "GET" && target === "/healthz") {
      return sendJson(response, 200, this.health());
    }
    if (request.method === "GET" && target === "/readyz") {
      return sendJson(response, this.ready ? 200 : 503, {
        status: this.ready ? "ready" : "compiler_unavailable",
      });
    }
    if (target !== "/v1/compiler-jobs") return sendJson(response, 404, { code: "not_found" });
    if (request.method !== "POST") {
      response.setHeader("allow", "POST");
      return sendJson(response, 405, { code: "method_not_allowed" });
    }
    if (!isJsonContentType(request.headers["content-type"])) {
      return sendJson(response, 415, { code: "content_type_required" });
    }
    let body;
    try {
      await this.start();
      body = await readRequestBody(request, MAX_REQUEST_BYTES);
      const result = await this.consume({
        method: "POST",
        target,
        headers: request.headers,
        body,
        transport: { socket: request.socket },
      });
      return sendJson(response, 202, result);
    } catch (error) {
      const normalized = normalizeHttpError(error);
      if (normalized.status === 429) response.setHeader("retry-after", "1");
      return sendJson(response, normalized.status, { code: normalized.code });
    }
  }
}

/**
 * Returns an HTTP server but never starts listening. A trusted deployment
 * integration must inject reviewed dependencies into CompilerHttpWorker first.
 */
export function createCompilerHttpServer(options = {}) {
  const worker = options.worker instanceof CompilerHttpWorker
    ? options.worker
    : new CompilerHttpWorker(options);
  return createServer((request, response) => {
    worker.handleHttp(request, response).catch(() => {
      if (!response.headersSent) sendJson(response, 500, { code: "compiler_failed" });
      else response.destroy();
    });
  });
}

function createComposition(options, now, allowInsecureForTests) {
  try {
    const catalog = options.toolchainCatalog instanceof ReviewedToolchainCatalog
      ? options.toolchainCatalog
      : new ReviewedToolchainCatalog(options.toolchainCatalog);
    if (!validJreRegistry(options.verifiedJres) ||
      !validSandboxAdapter(options.sandboxAdapter) ||
      !validArtifactDownloader(options.artifactDownloader, allowInsecureForTests) ||
      !validAuthenticator(options.requestAuthenticator, allowInsecureForTests)) {
      return undefined;
    }
    const callbacks = new AuthenticatedDeploymentCallbacks({
      ...options.callback,
      now,
      allowInsecureForTests,
    });
    return {
      builder: new ReviewedRuntimeBuilder({
        toolchainCatalog: catalog,
        verifiedJres: options.verifiedJres,
        sandboxRunner: options.sandboxAdapter,
        artifactDownloader: options.artifactDownloader,
      }),
      callbacks,
      fetchImpl: options.fetchImpl,
      requestAuthenticator: options.requestAuthenticator,
    };
  } catch {
    return undefined;
  }
}

class CallbackBoundControlPlane {
  constructor({ callbacks, job, now }) {
    if (!callbacks || typeof callbacks.post !== "function") {
      throw new TypeError("missing authenticated callbacks");
    }
    if (!plainObject(job)) throw new TypeError("missing exact compiler job");
    this.callbacks = callbacks;
    this.job = job;
    this.now = now;
    this.terminalCallbackDelivered = false;
    this.grantRefreshSucceeded = false;
  }

  async getGrants(deploymentId) {
    if (deploymentId !== this.job.deploymentId) {
      throw new CompilerFailure("invalid_grants");
    }
    const grants = await this.callbacks.grants(this.job);
    verifyGrantSet(grants, this.job, { now: this.now(), requireUnexpired: true });
    this.grantRefreshSucceeded = true;
    return grants;
  }

  async publish(publication) {
    const job = this.job;
    if (!publication || publication.deploymentId !== job.deploymentId ||
      publication.manifestSha256 !== job.manifestSha256 ||
      publication.content?.key !== job.expectedContentKey) {
      throw new CompilerFailure("invalid_publication");
    }
    await this.callbacks.post("published", job.deploymentId, {
      schemaVersion: 1,
      status: "published",
      compilerRequestId: job.compilerRequestId,
      deploymentId: job.deploymentId,
      manifestSha256: job.manifestSha256,
      content: publication.content,
      descriptor: publication.descriptor,
    });
    this.terminalCallbackDelivered = true;
  }

  async prepareUpload(publication) {
    const job = this.job;
    if (!publication || publication.deploymentId !== job.deploymentId ||
      publication.manifestSha256 !== job.manifestSha256 ||
      publication.content?.key !== job.expectedContentKey) {
      throw new CompilerFailure("invalid_upload_preparation");
    }
    const response = await this.callbacks.post("upload-prepared", job.deploymentId, {
      schemaVersion: 1,
      status: "upload_prepared",
      compilerRequestId: job.compilerRequestId,
      deploymentId: job.deploymentId,
      manifestSha256: job.manifestSha256,
      content: publication.content,
      descriptor: publication.descriptor,
    });
    return await parseUploadPreparation(response, job);
  }

  async failed(failure) {
    const job = this.job;
    if (!this.grantRefreshSucceeded) {
      throw new CompilerFailure("invalid_grants");
    }
    if (!failure || failure.deploymentId !== job.deploymentId ||
      failure.manifestSha256 !== job.manifestSha256 ||
      !["unsupported_compatibility", "compiler_unavailable", "compiler_failed"].includes(failure.code)) {
      throw new CompilerFailure("invalid_failure_callback");
    }
    await this.callbacks.post("failed", job.deploymentId, {
      schemaVersion: 1,
      status: "failed",
      compilerRequestId: job.compilerRequestId,
      deploymentId: job.deploymentId,
      manifestSha256: job.manifestSha256,
      code: failure.code,
    });
    this.terminalCallbackDelivered = true;
  }
}

class AuthenticatedDeploymentCallbacks {
  constructor({
    controlPlaneOrigin,
    authenticator,
    fetchImpl = fetch,
    now,
    timeoutMs = 10_000,
    allowInsecureForTests,
  }) {
    if (!validAuthenticator(authenticator, allowInsecureForTests) || typeof fetchImpl !== "function" ||
      !Number.isSafeInteger(timeoutMs) || timeoutMs < 1_000 || timeoutMs > 60_000) {
      throw new TypeError("invalid callback identity");
    }
    let parsed;
    try {
      parsed = new URL(controlPlaneOrigin);
    } catch {
      throw new TypeError("invalid callback URL");
    }
    if (!controlPlaneOrigin || parsed.username || parsed.password || parsed.hash ||
      parsed.pathname !== "/" || parsed.search ||
      (parsed.protocol !== "https:" && !(allowInsecureForTests && parsed.protocol === "http:"))) {
      throw new TypeError("invalid control-plane origin");
    }
    this.origin = parsed.origin;
    this.authenticator = authenticator;
    this.fetch = fetchImpl;
    this.now = now;
    this.timeoutMs = timeoutMs;
  }

  async grants(job) {
    if (!plainObject(job) || !validCallbackDeploymentId(job.deploymentId) ||
      !validId(job.compilerRequestId)) {
      throw new CompilerFailure("invalid_grants");
    }
    const response = await this.post("grants", job.deploymentId, {
      schemaVersion: 1,
      compilerRequestId: job.compilerRequestId,
      deploymentId: job.deploymentId,
    });
    if (response.status !== 200 ||
      !response.headers.get("content-type")?.toLowerCase().startsWith("application/json")) {
      void response.body?.cancel("invalid grants response").catch(() => undefined);
      throw new CompilerFailure("invalid_grants");
    }
    return await parseGrantResponse(response, job);
  }

  async post(kind, deploymentId, payload) {
    const suffixes = {
      grants: "grants",
      "upload-prepared": "upload-prepared",
      published: "published",
      failed: "failed",
    };
    if (!Object.hasOwn(suffixes, kind) || !validCallbackDeploymentId(deploymentId)) {
      throw new CompilerFailure("callback_delivery_failed");
    }

    const target = `/v1/internal/shared-runtime-compiler/deployments/` +
      `${encodeURIComponent(deploymentId)}/${suffixes[kind]}`;
    const url = `${this.origin}${target}`;
    const body = encoder.encode(JSON.stringify(payload));
    if (body.byteLength > MAX_CALLBACK_BYTES) throw new CompilerFailure("callback_payload_too_large");
    const identityHeaders = await this.authenticator.signOutgoing({
      method: "POST",
      target,
      body,
      now: this.now(),
    });
    const controller = new AbortController();
    let timeout;
    try {
      const response = await Promise.race([
        this.fetch(url, {
          method: "POST",
          headers: {
            "content-type": "application/json",
            "content-length": String(body.byteLength),
            ...identityHeaders,
          },
          body,
          redirect: "error",
          credentials: "omit",
          referrerPolicy: "no-referrer",
          signal: controller.signal,
        }),
        new Promise((_, reject) => {
          timeout = setTimeout(() => {
            controller.abort();
            reject(new CompilerFailure("callback_delivery_failed"));
          }, this.timeoutMs);
        }),
      ]);
      if (!response?.ok || response.redirected ||
        (response.url && new URL(response.url).href !== url)) {
        throw new CompilerFailure("callback_delivery_failed");
      }

      return response;
    } catch (error) {
      if (error instanceof CompilerFailure) throw error;
      throw new CompilerFailure("callback_delivery_failed");
    } finally {
      clearTimeout(timeout);
    }

  }
}

async function parseGrantResponse(response, job) {
  const bytes = await readBoundedResponse(
    response,
    MAX_REQUEST_BYTES,
    "invalid_grants",
  );
  let grants;
  try {
    grants = JSON.parse(decoder.decode(bytes));
  } catch {
    throw new CompilerFailure("invalid_grants");
  }
  try {
    verifyGrantSet(grants, job, { requireUnexpired: false });
  } catch {
    throw new CompilerFailure("invalid_grants");
  }
  return grants;
}

function validCallbackDeploymentId(value) {
  return typeof value === "string" &&
    /^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$/.test(value);
}

export async function parseUploadPreparation(response, job) {
  const bytes = await readBoundedCallbackBody(response);
  let value;
  try {
    value = JSON.parse(decoder.decode(bytes));
  } catch {
    throw new CompilerFailure("invalid_upload_preparation");
  }
  if (!plainObject(value) || !sameKeys(value, [
    "schemaVersion", "status", "compilerRequestId", "deploymentId",
    "manifestSha256", "content", "descriptor", "reconciliation",
  ]) || value.schemaVersion !== 1 ||
    !["upload_prepared", "upload_existing"].includes(value.status) ||
    value.compilerRequestId !== job.compilerRequestId ||
    value.deploymentId !== job.deploymentId ||
    value.manifestSha256 !== job.manifestSha256 ||
    !plainObject(value.content) || !plainObject(value.descriptor) ||
    !plainObject(value.reconciliation) ||
    value.content.key !== job.expectedContentKey ||
    !validSha256(value.content.sha256) ||
    !Number.isSafeInteger(value.content.compressedSize) ||
    value.content.compressedSize < 1 ||
    !Number.isSafeInteger(value.content.logicalSize) ||
    value.content.logicalSize < 1 ||
    !Array.isArray(value.content.paths) ||
    !isReconciliationGrant(value.reconciliation, job.expectedContentKey)) {
    throw new CompilerFailure("invalid_upload_preparation");
  }
  return {
    status: value.status,
    publication: {
      deploymentId: value.deploymentId,
      manifestSha256: value.manifestSha256,
      content: value.content,
      descriptor: value.descriptor,
    },
    reconciliation: value.reconciliation,
  };
}

export async function readBoundedCallbackBody(response) {
  return await readBoundedResponse(
    response,
    MAX_CALLBACK_BYTES,
    "invalid_upload_preparation",
  );
}

async function readBoundedResponse(response, maximum, failureCode) {
  const length = response.headers.get("content-length");
  if (length !== null && (!/^(?:0|[1-9]\d*)$/.test(length) ||
    Number(length) > maximum)) {
    void response.body?.cancel("callback response too large").catch(() => undefined);
    throw new CompilerFailure(failureCode);
  }
  const reader = response.body?.getReader?.();
  if (!reader) throw new CompilerFailure(failureCode);
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array)) {
        void reader.cancel("invalid callback response").catch(() => undefined);
        throw new CompilerFailure(failureCode);
      }
      total += value.byteLength;
      if (total > maximum) {
        void reader.cancel("callback response too large").catch(() => undefined);
        throw new CompilerFailure(failureCode);
      }
      chunks.push(value);
    }
  } catch {
    void reader.cancel("invalid callback response").catch(() => undefined);
    throw new CompilerFailure(failureCode);
  } finally {
    reader.releaseLock();
  }
  if (length !== null && total !== Number(length)) {
    throw new CompilerFailure(failureCode);
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

function isReconciliationGrant(value, expectedKey) {
  return value.key === expectedKey && value.method === "GET" &&
    exactHttpsUrl(value.url) && Number.isSafeInteger(Date.parse(value.expiresAt)) &&
    value.headers === undefined;
}

function parseEnvelope(body, now) {
  let envelope;
  try {
    envelope = JSON.parse(decoder.decode(body));
  } catch {
    throw new HttpWorkerError(400, "invalid_request");
  }
  if (!plainObject(envelope) || !sameKeys(envelope, [
    "schemaVersion", "requestId", "issuedAt", "expiresAt", "job", "grants",
  ]) || envelope.schemaVersion !== 1 || !validId(envelope.requestId) ||
    !plainObject(envelope.job) || !plainObject(envelope.grants) ||
    envelope.requestId !== envelope.job.compilerRequestId ||
    envelope.grants.compilerRequestId !== envelope.requestId ||
    !validMessageLifetime(envelope.issuedAt, envelope.expiresAt, now)) {
    throw new HttpWorkerError(400, "invalid_request");
  }
  try {
    validateCompilerJob(envelope.job);
  } catch {
    throw new HttpWorkerError(400, "invalid_request");
  }
  return envelope;
}

function parseQueuedEnvelope(body) {
  let value;
  try {
    value = JSON.parse(decoder.decode(body));
  } catch {
    throw new CompilerFailure("invalid_persisted_job");
  }
  const issuedAt = Date.parse(value?.issuedAt);
  if (!Number.isSafeInteger(issuedAt)) {
    throw new CompilerFailure("invalid_persisted_job");
  }
  try {
    return parseEnvelope(body, issuedAt);
  } catch {
    throw new CompilerFailure("invalid_persisted_job");
  }
}

async function verifyIncoming(authenticator, request) {
  try {
    await authenticator.verifyIncoming(request);
  } catch {
    throw new HttpWorkerError(401, "request_unauthorized");
  }
}

function validMessageLifetime(issuedAt, expiresAt, now) {
  const issued = Date.parse(issuedAt);
  const expires = Date.parse(expiresAt);
  return Number.isSafeInteger(issued) && Number.isSafeInteger(expires) &&
    issued <= now + 60_000 && expires > now && expires - issued <= MAX_ENVELOPE_LIFETIME_MS;
}

function validJreRegistry(value) {
  return value && typeof value.resolve === "function";
}

function validSandboxAdapter(value) {
  return value && typeof value.assemble === "function" &&
    plainObject(value.capabilities) &&
    Object.entries(requiredSandboxCapabilities).every(([key, expected]) =>
      value.capabilities[key] === expected,
    );
}

function validArtifactDownloader(value, allowInsecureForTests) {
  return value && typeof value.download === "function" &&
    (allowInsecureForTests || value instanceof StrictArtifactDownloader);
}

function validAuthenticator(value, allowInsecureForTests) {
  return value && typeof value.verifyIncoming === "function" &&
    typeof value.signOutgoing === "function" &&
    (allowInsecureForTests || value.replayProtected === true);
}

function validConcurrency(value) {
  return Number.isSafeInteger(value) && value >= 1 && value <= 16;
}

function validJobQueue(value) {
  return value && typeof value.initialize === "function" &&
    typeof value.enqueue === "function" && typeof value.claim === "function" &&
    typeof value.finish === "function";
}

function parseTarget(value) {
  try {
    const url = new URL(value, "http://worker.invalid");
    return url.search || url.hash ? undefined : url.pathname;
  } catch {
    return undefined;
  }
}

async function readRequestBody(request, maximum) {
  const declared = request.headers["content-length"];
  if (typeof declared === "string" && (!/^(?:0|[1-9]\d*)$/.test(declared) ||
    Number(declared) > maximum)) {
    throw new HttpWorkerError(413, "request_too_large");
  }
  const chunks = [];
  let total = 0;
  for await (const chunk of request) {
    if (!(chunk instanceof Uint8Array)) throw new HttpWorkerError(400, "invalid_request");
    total += chunk.byteLength;
    if (total > maximum) throw new HttpWorkerError(413, "request_too_large");
    chunks.push(chunk);
  }
  const body = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    body.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return body;
}

function isJsonContentType(value) {
  return typeof value === "string" && /^application\/json(?:\s*;\s*charset=utf-8)?$/i.test(value);
}

function sendJson(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
    "content-length": Buffer.byteLength(body),
  });
  response.end(body);
}

function normalizeHttpError(error) {
  if (error instanceof HttpWorkerError) return error;
  return new HttpWorkerError(500, "compiler_failed");
}

class HttpWorkerError extends Error {
  constructor(status, code) {
    super(code);
    this.status = status;
    this.code = code;
  }
}

function validId(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 255 &&
    !/[\x00-\x1f\x7f]/.test(value);
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value);
}

function exactHttpsUrl(value) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password && !url.hash;
  } catch {
    return false;
  }
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function sameKeys(value, expected) {
  const actual = Object.keys(value).sort();
  return actual.length === expected.length &&
    actual.every((key, index) => key === [...expected].sort()[index]);
}
