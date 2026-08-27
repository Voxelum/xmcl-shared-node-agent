import { createHash } from "node:crypto";
import { CompilerFailure, MAX_BUNDLE_BYTES, validateBundle } from "./bundle.mjs";
import { ReviewedRuntimeBuilder, validateImmutableContent } from "./reviewed-builder.mjs";

const failureCodes = new Set([
  "unsupported_compatibility",
  "compiler_unavailable",
  "compiler_failed",
]);

/**
 * Control-plane interface. Its implementation uses a workload identity (mTLS
 * or equivalent) outside this repository; it must never accept an object-store
 * master credential, node grant, or browser credential.
 */
export class CompilerWorker {
  constructor({
    controlPlane,
    builder = new FailClosedRuntimeBuilder(),
    fetchImpl = fetch,
    requestTimeoutMs = 30_000,
  }) {
    if (!Number.isSafeInteger(requestTimeoutMs) || requestTimeoutMs < 1_000 ||
      requestTimeoutMs > 120_000) {
      throw new TypeError("invalid compiler request timeout");
    }
    this.controlPlane = controlPlane;
    this.builder = builder;
    this.fetch = fetchImpl;
    this.requestTimeoutMs = requestTimeoutMs;
  }

  async run(job) {
    let publication;
    let publicationStarted = false;
    let uploadPreparationStarted = false;
    try {
      validateCompilerJob(job);
      const grants = await this.controlPlane.getGrants(job.deploymentId);
      const { input, output } = verifyGrantSet(grants, job);
      const archive = await downloadExact(this.fetch, input, job.frozenManifest.archive, {
        timeoutMs: this.requestTimeoutMs,
      });
      const bundle = await validateBundle(archive, job.frozenManifest);
      const built = await this.builder.build({
        bundle,
        frozenManifest: job.frozenManifest,
        expectedContentKey: job.expectedContentKey,
      });
      verifyBuiltContent(built, job);
      publication = publicationForControlPlane(job, built);
      // The control plane persists this exact binding before a PUT can begin.
      // If this request or the PUT response is lost, a redelivery can only
      // reconcile the same digest and descriptor through its GET grant.
      uploadPreparationStarted = true;
      const prepared = await this.controlPlane.prepareUpload(publication);
      const boundPublication = verifyPreparedUpload(prepared, job);
      if (prepared.status === "upload_prepared") {
        try {
          await uploadExact(this.fetch, output, built.archive, {
            timeoutMs: this.requestTimeoutMs,
          });
        } catch {
          await reconcileExactUpload(this.fetch, prepared.reconciliation, boundPublication, {
            timeoutMs: this.requestTimeoutMs,
          });
        }
      } else {
        await reconcileExactUpload(this.fetch, prepared.reconciliation, boundPublication, {
          timeoutMs: this.requestTimeoutMs,
        });
      }
      publication = boundPublication;
      publicationStarted = true;
      await this.controlPlane.publish(publication);
      return { status: "published", deploymentId: job.deploymentId };
    } catch (error) {
      if (publicationStarted) {
        return {
          status: "published_callback_uncertain",
          deploymentId: job.deploymentId,
          manifestSha256: job.manifestSha256,
          content: publication.content,
          descriptor: publication.descriptor,
        };
      }
      if (uploadPreparationStarted && publication) {
        return {
          status: "upload_reconciliation_uncertain",
          deploymentId: job.deploymentId,
          manifestSha256: job.manifestSha256,
          content: publication.content,
          descriptor: publication.descriptor,
        };
      }
      const code = classifyFailure(error);
      await this.controlPlane.failed({
        deploymentId: job?.deploymentId,
        manifestSha256: job?.manifestSha256,
        code,
      }).catch(() => undefined);
      return { status: "failed", code };
    }
  }
}

/**
 * Explicitly prevents a deployment from pretending that local Java, a Docker
 * daemon, or arbitrary Internet downloads can build a customer runtime.
 */
export class FailClosedRuntimeBuilder {
  async build() {
    throw new CompilerFailure("compiler_unavailable");
  }
}

/**
 * Production code must inject all reviewed dependencies explicitly. Omitting
 * any one of them leaves ReviewedRuntimeBuilder fail-closed at build time.
 */
export function createReviewedCompilerWorker({
  controlPlane,
  fetchImpl = fetch,
  toolchainCatalog,
  verifiedJres,
  sandboxRunner,
  artifactDownloader,
} = {}) {
  return new CompilerWorker({
    controlPlane,
    fetchImpl,
    builder: new ReviewedRuntimeBuilder({
      toolchainCatalog,
      verifiedJres,
      sandboxRunner,
      artifactDownloader,
    }),
  });
}

export function verifyGrantSet(grants, job, { now = Date.now(), requireUnexpired = false } = {}) {
  if (!grants || !hasExactKeys(grants, [
    "accountId", "serviceId", "deploymentId", "compilerRequestId",
    "manifestSha256", "grants",
  ]) || grants.accountId !== job.accountId ||
    grants.serviceId !== job.serviceId || grants.deploymentId !== job.deploymentId ||
    grants.compilerRequestId !== job.compilerRequestId ||
    grants.manifestSha256 !== job.manifestSha256 || !Array.isArray(grants.grants)) {
    throw new CompilerFailure("invalid_grants");
  }
  const input = grants.grants.find((grant) => grant.method === "GET");
  const output = grants.grants.find((grant) => grant.method === "PUT");
  if (
    grants.grants.length !== 2 || !input || !output ||
    input.key !== job.frozenManifest.archive.key ||
    output.key !== job.expectedContentKey ||
    !isExactSignedGrant(input, "GET") || !isExactSignedGrant(output, "PUT") ||
    !hasExactHeaders(input.headers, {}) ||
    !hasExactOutputHeaders(output.headers) ||
    (requireUnexpired && (!validNow(now) || grantExpiresAt(input) <= now || grantExpiresAt(output) <= now))
  ) throw new CompilerFailure("invalid_grants");
  return { input, output };
}

export async function downloadExact(fetchImpl, grant, expected, { timeoutMs = 30_000 } = {}) {
  return await exactRequest(fetchImpl, grant.url, {
    method: "GET",
    headers: grant.headers,
    redirect: "error",
  }, timeoutMs, "input_download_failed", async (response) => {
    if (!response.ok || response.redirected ||
      (response.url && new URL(response.url).href !== new URL(grant.url).href)) {
      throw new CompilerFailure("input_download_failed");
    }
    const length = Number(response.headers.get("content-length") ?? 0);
    if (!Number.isSafeInteger(length) || length < 1 || length > MAX_BUNDLE_BYTES ||
      length !== expected.sizeBytes) {
      throw new CompilerFailure("input_size_mismatch");
    }
    const bytes = await readExactBody(response, expected.sizeBytes);
    if (bytes.byteLength !== expected.sizeBytes || sha256(bytes) !== expected.sha256) {
      throw new CompilerFailure("input_hash_mismatch");
    }
    return bytes;
  });
}

async function uploadExact(fetchImpl, grant, archive, { timeoutMs = 30_000 } = {}) {
  await exactRequest(fetchImpl, grant.url, {
    method: "PUT",
    headers: grant.headers,
    body: archive,
    redirect: "error",
  }, timeoutMs, "content_upload_failed", async (response) => {
    if (!response.ok || response.redirected ||
      (response.url && new URL(response.url).href !== new URL(grant.url).href)) {
      throw new CompilerFailure("content_upload_failed");
    }
  });
}

/**
 * A successful immutable PUT response is not required to reconcile: it is
 * published normally. Every non-successful/unknown result is treated as
 * ambiguous instead of as a compile failure, because a proxy can lose a
 * response after S3 commits the object. The reconciliation GET is issued only
 * after the control plane durably bound this exact output.
 */
async function reconcileExactUpload(fetchImpl, grant, publication, { timeoutMs = 30_000 } = {}) {
  if (!isExactReconciliationGrant(grant, publication.content.key)) {
    throw new CompilerFailure("invalid_reconciliation_grant");
  }
  await exactRequest(fetchImpl, grant.url, {
    method: "GET",
    headers: grant.headers,
    redirect: "error",
  }, timeoutMs, "content_reconciliation_failed", async (response) => {
    if (!response.ok || response.redirected ||
      (response.url && new URL(response.url).href !== new URL(grant.url).href)) {
      throw new CompilerFailure("content_reconciliation_failed");
    }
    const length = Number(response.headers.get("content-length") ?? 0);
    if (!Number.isSafeInteger(length) || length !== publication.content.compressedSize) {
      throw new CompilerFailure("content_reconciliation_failed");
    }
    const digest = await hashExactBody(response, publication.content.compressedSize);
    if (digest !== publication.content.sha256) {
      throw new CompilerFailure("content_reconciliation_failed");
    }
  });
}

function publicationForControlPlane(job, built) {
  return {
    deploymentId: job.deploymentId,
    manifestSha256: job.manifestSha256,
    content: {
      key: built.content.key,
      sha256: built.content.sha256,
      compressedSize: built.content.sizeBytes,
      logicalSize: built.content.entries.reduce((total, entry) => total + entry.sizeBytes, 0),
      paths: built.content.entries.map((entry) => entry.path),
    },
    descriptor: {
      schemaVersion: 1,
      minecraftVersion: job.frozenManifest.compatibility.minecraftVersion,
      java: job.frozenManifest.compatibility.java,
      runtimeCatalog: {
        sha256: job.frozenManifest.compatibility.runtimeCatalog.sha256,
      },
      loader: {
        kind: job.frozenManifest.compatibility.loader,
        version: job.frozenManifest.compatibility.loaderVersion,
      },
      launch: {
        kind: built.descriptor.launch.kind,
        path: built.descriptor.launch.path,
        arguments: [...built.descriptor.launch.arguments],
      },
      contentSha256: built.content.sha256,
    },
  };
}

function verifyPreparedUpload(value, job) {
  if (!value || !["upload_prepared", "upload_existing"].includes(value.status) ||
    value.publication?.deploymentId !== job.deploymentId ||
    value.publication?.manifestSha256 !== job.manifestSha256 ||
    value.publication?.content?.key !== job.expectedContentKey ||
    !validSha256(value.publication.content.sha256) ||
    !Number.isSafeInteger(value.publication.content.compressedSize) ||
    value.publication.content.compressedSize < 1 ||
    !Number.isSafeInteger(value.publication.content.logicalSize) ||
    value.publication.content.logicalSize < 1 ||
    !Array.isArray(value.publication.content.paths) ||
    !value.publication.descriptor || typeof value.publication.descriptor !== "object") {
    throw new CompilerFailure("invalid_upload_preparation");
  }
  return value.publication;
}

export function validateCompilerJob(job) {
  if (!job || typeof job !== "object" ||
    !hasExactKeys(job, [
      "accountId", "serviceId", "deploymentId", "compilerRequestId",
      "manifestSha256", "expectedContentKey", "frozenManifest",
    ]) ||
    !validKeySegment(job.accountId) || !validKeySegment(job.serviceId) ||
    !validKeySegment(job.deploymentId) || !validSha256(job.manifestSha256) ||
    !validKeySegment(job.compilerRequestId) ||
    typeof job.expectedContentKey !== "string" ||
    job.frozenManifest?.sourceFormat !== "xmcl_server_bundle") {
    throw new CompilerFailure("invalid_job");
  }
  const archive = job.frozenManifest?.archive;
  if (!archive || !validSha256(archive.sha256) ||
    !Number.isSafeInteger(archive.sizeBytes) || archive.sizeBytes < 1 ||
    archive.sizeBytes > MAX_BUNDLE_BYTES || typeof archive.key !== "string" ||
    !validKeySegment(job.frozenManifest.importId)) {
    throw new CompilerFailure("invalid_job");
  }
  const prefix = `shared-hosting/${job.accountId}/${job.serviceId}/`;
  if (
    archive.key !== `${prefix}compiler-inputs/${job.frozenManifest.importId}.xmcl-server-bundle` ||
    job.expectedContentKey !== `${prefix}compiler-content/${job.manifestSha256}.tar.zst`
  ) {
    throw new CompilerFailure("invalid_job");
  }
}

function verifyBuiltContent(built, job) {
  if (!built || !(built.archive instanceof Uint8Array) ||
    !built.content || built.content.key !== job.expectedContentKey ||
    built.descriptor?.launch?.path !== ".xmcl/launch.sh" ||
    built.descriptor?.launch?.kind !== "generated-server-launcher" ||
    !Array.isArray(built.descriptor?.launch?.arguments) ||
    built.descriptor.launch.arguments.length !== 0) {
    throw new CompilerFailure("invalid_builder_output");
  }
  validateImmutableContent(built);
}

function isExactSignedGrant(grant, method) {
  const keys = method === "PUT"
    ? ["key", "method", "url", "expiresAt", "headers"]
    : ["key", "method", "url", "expiresAt"];
  const validKeys = hasExactKeys(grant, keys) ||
    method === "GET" && hasExactKeys(grant, [...keys, "headers"]);
  return validKeys && grant.method === method && typeof grant.key === "string" &&
    exactHttpsUrl(grant.url) && Number.isSafeInteger(grantExpiresAt(grant));
}

function isExactReconciliationGrant(grant, expectedKey) {
  return grant && grant.method === "GET" && grant.key === expectedKey &&
    isExactSignedGrant(grant, "GET") && hasExactHeaders(grant.headers, {});
}

function classifyFailure(error) {
  if (error instanceof CompilerFailure && error.code === "unsupported_compatibility") {
    return "unsupported_compatibility";
  }
  if (error instanceof CompilerFailure && error.code === "compiler_unavailable") {
    return "compiler_unavailable";
  }
  return failureCodes.has(error?.code) ? error.code : "compiler_failed";
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value);
}

function validKeySegment(value) {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$/.test(value);
}

function hasExactHeaders(value, expected) {
  if (value === undefined) return Object.keys(expected).length === 0;
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const keys = Object.keys(expected).sort();
  return actual.length === keys.length &&
    actual.every((key, index) => key === keys[index] && value[key] === expected[key]);
}

function hasExactKeys(value, expected) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const actual = Object.keys(value).sort();
  const keys = [...expected].sort();
  return actual.length === keys.length &&
    actual.every((key, index) => key === keys[index]);
}

function hasExactOutputHeaders(value) {
  return hasExactHeaders(value, { "if-none-match": "*" }) ||
    hasExactHeaders(value, {
      "if-none-match": "*",
      "x-ms-blob-type": "BlockBlob",
    });
}

function exactHttpsUrl(value) {
  try {
    const url = new URL(value);
    return url.protocol === "https:" && !url.username && !url.password && !url.hash;
  } catch {
    return false;
  }
}

function grantExpiresAt(grant) {
  const timestamp = Date.parse(grant?.expiresAt);
  return Number.isSafeInteger(timestamp) ? timestamp : NaN;
}

function validNow(value) {
  return Number.isSafeInteger(value) && value > 0;
}

async function exactRequest(fetchImpl, url, options, timeoutMs, failureCode, validateResponse) {
  if (typeof fetchImpl !== "function" || !Number.isSafeInteger(timeoutMs) ||
    timeoutMs < 1_000 || timeoutMs > 120_000) {
    throw new CompilerFailure(failureCode);
  }
  const controller = new AbortController();
  let timeout;
  try {
    const request = async () => {
      const response = await fetchImpl(url, { ...options, signal: controller.signal });
      return await validateResponse(response);
    };
    return await Promise.race([
      request(),
      new Promise((_, reject) => {
        timeout = setTimeout(() => {
          controller.abort();
          reject(new CompilerFailure(failureCode));
        }, timeoutMs);
      }),
    ]);
  } catch (error) {
    if (error instanceof CompilerFailure) throw error;
    throw new CompilerFailure(failureCode);
  } finally {
    clearTimeout(timeout);
  }
}

async function readExactBody(response, expectedSize) {
  if (!response.body?.getReader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength !== expectedSize) throw new CompilerFailure("input_size_mismatch");
    return bytes;
  }

  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      if (!(next.value instanceof Uint8Array)) throw new CompilerFailure("input_download_failed");
      total += next.value.byteLength;
      if (total > expectedSize || total > MAX_BUNDLE_BYTES) {
        throw new CompilerFailure("input_size_mismatch");
      }
      chunks.push(next.value);
    }
  } finally {
    reader.releaseLock?.();
  }
  if (total !== expectedSize) throw new CompilerFailure("input_size_mismatch");
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

async function hashExactBody(response, expectedSize) {
  if (!response.body?.getReader) {
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength !== expectedSize) throw new CompilerFailure("content_reconciliation_failed");
    return sha256(bytes);
  }
  const hash = createHash("sha256");
  const reader = response.body.getReader();
  let total = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      if (!(next.value instanceof Uint8Array)) {
        throw new CompilerFailure("content_reconciliation_failed");
      }
      total += next.value.byteLength;
      if (total > expectedSize) throw new CompilerFailure("content_reconciliation_failed");
      hash.update(next.value);
    }
  } finally {
    reader.releaseLock?.();
  }
  if (total !== expectedSize) throw new CompilerFailure("content_reconciliation_failed");
  return hash.digest("hex");
}
