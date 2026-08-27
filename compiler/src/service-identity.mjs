import {
  createHash,
  createHmac,
  createPrivateKey,
  createPublicKey,
  sign,
  verify,
  randomBytes,
  timingSafeEqual,
} from "node:crypto";
import { mkdir, open, readdir, rm, stat } from "node:fs/promises";
import { dirname, resolve, sep } from "node:path";

const noncePattern = /^[A-Za-z0-9_-]{16,128}$/;

/**
 * Adapter boundary for workload identities. JWT and mTLS deployments implement
 * the same two operations; each verifier must validate timestamp and nonce
 * replay protection before returning successfully.
 */
export class ServiceIdentity {
  constructor({ verifyIncoming, signOutgoing, replayProtected = false } = {}) {
    if (typeof verifyIncoming !== "function" || typeof signOutgoing !== "function" ||
      replayProtected !== true) {
      throw new TypeError("invalid service identity configuration");
    }

    this.verify = verifyIncoming;
    this.sign = signOutgoing;
    this.replayProtected = true;
  }

  async verifyIncoming(request) {
    const accepted = await this.verify(request);
    if (accepted === false) throw new ServiceIdentityError("request_identity_rejected");
  }

  async signOutgoing(request) {
    const headers = await this.sign(request);
    if (!plainStringRecord(headers)) {
      throw new ServiceIdentityError("callback_identity_rejected");
    }
    return headers;
  }
}

/**
 * Ed25519 workload identity with an atomic, shared filesystem replay store.
 * The replay directory must be a persistent volume shared by every replica.
 */
export class Ed25519ServiceIdentity {
  constructor({
    keyId,
    verificationKeyId = keyId,
    signingKeyId = keyId,
    verificationKey,
    signingKey,
    nonceStore,
    clock = Date.now,
    maxAgeMs = 60_000,
  } = {}) {
    if (!validIdentifier(verificationKeyId) || !validIdentifier(signingKeyId) || !nonceStore ||
      nonceStore.durable !== true || typeof nonceStore.consume !== "function" ||
      typeof clock !== "function" || !Number.isSafeInteger(maxAgeMs) ||
      maxAgeMs < 1_000 || maxAgeMs > 300_000) {
      throw new TypeError("invalid Ed25519 service identity configuration");
    }
    try {
      this.verificationKey = verificationKey?.type === "public"
        ? verificationKey
        : createPublicKey(verificationKey);
      this.signingKey = signingKey?.type === "private"
        ? signingKey
        : createPrivateKey(signingKey);
    } catch {
      throw new TypeError("invalid Ed25519 service identity configuration");
    }
    if (this.verificationKey.asymmetricKeyType !== "ed25519" ||
      this.signingKey.asymmetricKeyType !== "ed25519") {
      throw new TypeError("invalid Ed25519 service identity configuration");
    }
    this.verificationKeyId = verificationKeyId;
    this.signingKeyId = signingKeyId;
    this.nonceStore = nonceStore;
    this.clock = clock;
    this.maxAgeMs = maxAgeMs;
    Object.defineProperty(this, "replayProtected", {
      value: true,
      enumerable: true,
      writable: false,
      configurable: false,
    });
  }

  async verifyIncoming({ method, target, headers, body } = {}) {
    const timestamp = parseTimestamp(header(headers, "x-xmcl-timestamp"));
    const nonce = header(headers, "x-xmcl-nonce");
    const authorization = header(headers, "authorization");
    const now = this.clock();
    validateSignedRequest({ method, target, body, timestamp, nonce, now, maxAgeMs: this.maxAgeMs });
    const actual = parseEd25519Authorization(authorization, this.verificationKeyId);
    if (!actual || !verify(null, signedPayload(method, target, timestamp, nonce, body),
      this.verificationKey, actual)) {
      throw new ServiceIdentityError("request_identity_rejected");
    }
    const accepted = await this.nonceStore.consume({
      key: `${this.verificationKeyId}:${nonce}`,
      expiresAt: timestamp + this.maxAgeMs,
      now,
    });
    if (accepted !== true) throw new ServiceIdentityError("request_replayed");
  }

  async signOutgoing({ method, target, body } = {}) {
    const timestamp = this.clock();
    const nonce = randomBytes(24).toString("base64url");
    validateSignedRequest({ method, target, body, timestamp, nonce, now: timestamp, maxAgeMs: 0 });
    const signature = sign(null, signedPayload(method, target, timestamp, nonce, body),
      this.signingKey);
    return {
      authorization: `Signature Ed25519 ${this.signingKeyId}:${signature.toString("base64url")}`,
      "x-xmcl-timestamp": String(timestamp),
      "x-xmcl-nonce": nonce,
    };
  }
}

export class FilesystemReplayStore {
  constructor({ directory, maxEntries = 1_000_000 } = {}) {
    if (!absoluteNormalizedPath(directory) || !Number.isSafeInteger(maxEntries) ||
      maxEntries < 1 || maxEntries > 10_000_000) {
      throw new TypeError("invalid filesystem replay store configuration");
    }
    this.directory = directory;
    this.maxEntries = maxEntries;
    Object.defineProperty(this, "durable", {
      value: true,
      enumerable: true,
      writable: false,
      configurable: false,
    });
  }

  async initialize() {
    await mkdir(this.directory, { recursive: true, mode: 0o700 });
    const metadata = await stat(this.directory);
    if (!metadata.isDirectory() ||
      (process.platform !== "win32" && (metadata.mode & 0o077) !== 0)) {
      throw new ServiceIdentityError("replay_store_rejected");
    }
  }

  async consume({ key, expiresAt, now } = {}) {
    if (typeof key !== "string" || key.length > 512 ||
      !Number.isSafeInteger(expiresAt) || !Number.isSafeInteger(now) || expiresAt <= now) {
      throw new ServiceIdentityError("replay_store_rejected");
    }
    await this.initialize();
    const entries = await readdir(this.directory);
    if (entries.length >= this.maxEntries) {
      await this.#removeExpired(entries, now);
      if ((await readdir(this.directory)).length >= this.maxEntries) return false;
    }
    const name = createHash("sha256").update(key).digest("hex");
    const path = `${this.directory}${sep}${name}`;
    let handle;
    try {
      handle = await open(path, "wx", 0o600);
      await handle.writeFile(`${expiresAt}\n`, { encoding: "ascii" });
      await handle.sync();
      await handle.close();
      handle = undefined;
      if (process.platform !== "win32") {
        const directory = await open(this.directory, "r");
        try {
          await directory.sync();
        } finally {
          await directory.close();
        }
      }
      return true;
    } catch (error) {
      if (error?.code === "EEXIST") return false;
      await rm(path, { force: true }).catch(() => undefined);
      throw new ServiceIdentityError("replay_store_rejected");
    } finally {
      await handle?.close().catch(() => undefined);
    }
  }

  async #removeExpired(entries, now) {
    for (const name of entries.slice(0, 1_024)) {
      if (!/^[a-f0-9]{64}$/.test(name)) continue;
      const path = `${this.directory}${sep}${name}`;
      try {
        const handle = await open(path, "r");
        const text = await handle.readFile({ encoding: "ascii" });
        await handle.close();
        if (/^[1-9]\d{0,15}\n$/.test(text) && Number(text.trim()) <= now) {
          await rm(path, { force: true });
        }
      } catch {
        // Concurrent cleanup or a malformed entry is safely ignored.
      }
    }
  }
}

/**
 * HMAC workload identity. It is production-eligible only when backed by a
 * replay store that explicitly attests durable atomic consume semantics.
 */
export class HmacServiceIdentity {
  constructor({
    keyId,
    secret,
    nonceStore = new InMemoryReplayCache(),
    clock = Date.now,
    maxAgeMs = 60_000,
    requireDurableReplay = false,
  } = {}) {
    const key = toSecret(secret);
    if (!validIdentifier(keyId) || !key || key.byteLength < 32 ||
      !nonceStore || typeof nonceStore.consume !== "function" ||
      typeof clock !== "function" || !Number.isSafeInteger(maxAgeMs) ||
      maxAgeMs < 1_000 || maxAgeMs > 300_000 ||
      typeof requireDurableReplay !== "boolean" ||
      requireDurableReplay && nonceStore.durable !== true) {
      throw new TypeError("invalid HMAC service identity configuration");
    }
    this.keyId = keyId;
    this.key = key;
    this.nonceStore = nonceStore;
    this.clock = clock;
    this.maxAgeMs = maxAgeMs;
    Object.defineProperty(this, "replayProtected", {
      value: nonceStore.durable === true,
      enumerable: true,
      writable: false,
      configurable: false,
    });
  }

  async verifyIncoming({ method, target, headers, body } = {}) {
    const timestamp = parseTimestamp(header(headers, "x-xmcl-timestamp"));
    const nonce = header(headers, "x-xmcl-nonce");
    const authorization = header(headers, "authorization");
    const now = this.clock();
    validateSignedRequest({ method, target, body, timestamp, nonce, now, maxAgeMs: this.maxAgeMs });
    const expected = this.signature(method, target, timestamp, nonce, body);
    const actual = parseAuthorization(authorization, this.keyId);
    if (!actual || actual.byteLength !== expected.byteLength ||
      !timingSafeEqual(actual, expected)) {
      throw new ServiceIdentityError("request_identity_rejected");
    }
    const accepted = await this.nonceStore.consume({
      key: `${this.keyId}:${nonce}`,
      expiresAt: timestamp + this.maxAgeMs,
      now,
    });
    if (accepted !== true) throw new ServiceIdentityError("request_replayed");
  }

  async signOutgoing({ method, target, body } = {}) {
    const timestamp = this.clock();
    const nonce = randomBytes(24).toString("base64url");
    validateSignedRequest({ method, target, body, timestamp, nonce, now: timestamp, maxAgeMs: 0 });
    return {
      authorization: `HMAC ${this.keyId}:${this.signature(method, target, timestamp, nonce, body).toString("base64url")}`,
      "x-xmcl-timestamp": String(timestamp),
      "x-xmcl-nonce": nonce,
    };
  }

  signature(method, target, timestamp, nonce, body) {
    return createHmac("sha256", this.key)
      .update(`${method}\n${target}\n${timestamp}\n${nonce}\n${bodyDigest(body)}`)
      .digest();
  }
}

/**
 * In-memory replay protection is suitable only for a single local process and
 * tests. It can never be marked durable.
 */
export class InMemoryReplayCache {
  constructor(options = {}) {
    if (!plainObject(options) || Object.keys(options).some((key) => key !== "maxEntries")) {
      throw new TypeError("invalid replay cache configuration");
    }
    const { maxEntries = 100_000 } = options;
    if (!Number.isSafeInteger(maxEntries) || maxEntries < 1 || maxEntries > 1_000_000) {
      throw new TypeError("invalid replay cache configuration");
    }
    this.maxEntries = maxEntries;
    this.entries = new Map();
  }

  async consume({ key, expiresAt, now } = {}) {
    if (typeof key !== "string" || !Number.isSafeInteger(expiresAt) ||
      !Number.isSafeInteger(now)) {
      throw new ServiceIdentityError("replay_store_rejected");
    }
    for (const [existingKey, expiry] of this.entries) {
      if (expiry <= now) this.entries.delete(existingKey);
    }
    if (this.entries.has(key) || this.entries.size >= this.maxEntries) return false;
    this.entries.set(key, expiresAt);
    return true;
  }
}

export class ServiceIdentityError extends Error {
  constructor(code) {
    super(code);
    this.name = "ServiceIdentityError";
    this.code = code;
  }
}

function validateSignedRequest({ method, target, body, timestamp, nonce, now, maxAgeMs }) {
  if (typeof method !== "string" || !/^[A-Z]+$/.test(method) ||
    typeof target !== "string" || !target.startsWith("/") || target.length > 2_048 ||
    !(body instanceof Uint8Array) || !Number.isSafeInteger(timestamp) ||
    !noncePattern.test(nonce ?? "") || !Number.isSafeInteger(now) ||
    (maxAgeMs > 0 && Math.abs(now - timestamp) > maxAgeMs)) {
    throw new ServiceIdentityError("request_identity_rejected");
  }
}

function parseAuthorization(value, keyId) {
  if (typeof value !== "string") return undefined;
  const prefix = `HMAC ${keyId}:`;
  if (!value.startsWith(prefix)) return undefined;
  try {
    const signature = Buffer.from(value.slice(prefix.length), "base64url");
    return signature.byteLength === 32 ? signature : undefined;
  } catch {
    return undefined;
  }
}

function parseEd25519Authorization(value, keyId) {
  if (typeof value !== "string") return undefined;
  const prefix = `Signature Ed25519 ${keyId}:`;
  if (!value.startsWith(prefix)) return undefined;
  try {
    const signature = Buffer.from(value.slice(prefix.length), "base64url");
    return signature.byteLength === 64 ? signature : undefined;
  } catch {
    return undefined;
  }
}

function signedPayload(method, target, timestamp, nonce, body) {
  return Buffer.from(`${method}\n${target}\n${timestamp}\n${nonce}\n${bodyDigest(body)}`, "utf8");
}

function parseTimestamp(value) {
  if (typeof value !== "string" || !/^[1-9]\d{0,15}$/.test(value)) return NaN;
  const timestamp = Number(value);
  return Number.isSafeInteger(timestamp) ? timestamp : NaN;
}

function header(headers, name) {
  if (headers?.get instanceof Function) return headers.get(name);
  const value = headers?.[name] ?? headers?.[name.toLowerCase()];
  return Array.isArray(value) ? undefined : value;
}

function bodyDigest(body) {
  return createHash("sha256").update(body).digest("hex");
}

function toSecret(value) {
  if (value instanceof Uint8Array) return Buffer.from(value);
  return typeof value === "string" ? Buffer.from(value, "utf8") : undefined;
}

function validIdentifier(value) {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);
}

function absoluteNormalizedPath(value) {
  return typeof value === "string" && value.length > 1 && value.length <= 1_024 &&
    resolve(value) === value && dirname(value) !== value;
}

function plainStringRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    Object.entries(value).every(([key, item]) =>
      /^[a-z0-9-]+$/.test(key) && typeof item === "string" && !/[\r\n]/.test(item),
    );
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}
