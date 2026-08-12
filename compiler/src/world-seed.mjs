import { createHash } from "node:crypto";

/**
 * Boundary for a future egress-isolated world-seed unpacker. It intentionally
 * has no default storage adapter or filesystem restore implementation.
 */
export class FailClosedWorldSeedHandler {
  async unpack() {
    throw new WorldSeedFailure("world_seed_handler_unavailable");
  }
}

export class WorldSeedFailure extends Error {
  constructor(code) {
    super(code);
    this.code = code;
    this.name = "WorldSeedFailure";
  }
}

export class WorldSeedWorker {
  constructor({ controlPlane, handler = new FailClosedWorldSeedHandler(), fetchImpl = fetch }) {
    this.controlPlane = controlPlane;
    this.handler = handler;
    this.fetch = fetchImpl;
  }

  async run(job) {
    validateJob(job);
    const grants = await this.controlPlane.getWorldSeedGrants(job.seedId);
    const input = verifyWorldSeedGrantSet(grants, job);
    const archive = await downloadExactWorldSeed(this.fetch, input, job.archive);
    return await this.handler.unpack({
      archive,
      accountId: job.accountId,
      serviceId: job.serviceId,
      seedId: job.seedId,
      worldName: job.worldName,
    });
  }
}

export function verifyWorldSeedGrantSet(grants, job) {
  if (!grants || grants.accountId !== job.accountId || grants.serviceId !== job.serviceId ||
    grants.seedId !== job.seedId || !Array.isArray(grants.grants) || grants.grants.length !== 1) {
    throw new WorldSeedFailure("invalid_world_seed_grants");
  }
  const [input] = grants.grants;
  if (!input || input.method !== "GET" || input.key !== job.archive.key ||
    !exactHttpsUrl(input.url) || typeof input.expiresAt !== "string" ||
    Object.keys(input.headers ?? {}).length !== 0) {
    throw new WorldSeedFailure("invalid_world_seed_grants");
  }
  return input;
}

export async function downloadExactWorldSeed(fetchImpl, grant, expected) {
  const response = await fetchImpl(grant.url, {
    method: "GET",
    headers: grant.headers,
    redirect: "error",
  });
  if (!response.ok) throw new WorldSeedFailure("world_seed_download_failed");
  const length = Number(response.headers.get("content-length") ?? 0);
  if (!Number.isSafeInteger(length) || length < 1 || length !== expected.sizeBytes) {
    throw new WorldSeedFailure("world_seed_size_mismatch");
  }
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength !== expected.sizeBytes || sha256(bytes) !== expected.sha256) {
    throw new WorldSeedFailure("world_seed_hash_mismatch");
  }
  return bytes;
}

function validateJob(job) {
  const prefix = `shared-hosting/${job?.accountId}/${job?.serviceId}/world-seeds/`;
  if (!job || !validId(job.accountId) || !validId(job.serviceId) || !validId(job.seedId) ||
    typeof job.worldName !== "string" || !job.worldName || !job.archive ||
    !validSha256(job.archive.sha256) || !Number.isSafeInteger(job.archive.sizeBytes) ||
    job.archive.sizeBytes < 1 || job.archive.sizeBytes > 512 * 1024 * 1024 ||
    job.archive.key !== `${prefix}${job.seedId}.xmcl-world-seed`) {
    throw new WorldSeedFailure("invalid_world_seed_job");
  }
}

function exactHttpsUrl(value) {
  return typeof value === "string" && /^https:\/\//.test(value);
}
function validId(value) {
  return typeof value === "string" && value.length > 0 && value.length <= 255 && !/[\x00-\x1f\x7f]/.test(value);
}
function validSha256(value) {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value);
}
function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}
