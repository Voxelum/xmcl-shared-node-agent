import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import { validateBundle } from "../src/bundle.mjs";
import { CompilerWorker, verifyGrantSet } from "../src/compiler.mjs";
import { WorldSeedWorker } from "../src/world-seed.mjs";
import {
  DeterministicFakeArtifactDownloader,
  DeterministicFakeJreRegistry,
  DeterministicFakeSandboxRunner,
  ReviewedRuntimeBuilder,
  StrictArtifactDownloader,
} from "../src/reviewed-builder.mjs";

function sha(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function json(value) {
  return new TextEncoder().encode(JSON.stringify(value));
}

function zip(entries) {
  const out = [];
  const central = [];
  for (const entry of entries) {
    const path = new TextEncoder().encode(entry.path);
    const bytes = entry.bytes;
    const crc = crc32(bytes);
    const offset = out.length;
    u32(out, 0x04034b50); u16(out, 20); u16(out, 0x800); u16(out, 0);
    u16(out, 0); u16(out, 0); u32(out, crc); u32(out, bytes.length);
    u32(out, bytes.length); u16(out, path.length); u16(out, 0);
    out.push(...path, ...bytes);
    u32(central, 0x02014b50); u16(central, 20); u16(central, 20); u16(central, 0x800);
    u16(central, 0); u16(central, 0); u16(central, 0); u32(central, crc);
    u32(central, bytes.length); u32(central, bytes.length); u16(central, path.length);
    u16(central, 0); u16(central, 0); u16(central, 0); u16(central, 0); u32(central, 0);
    u32(central, offset); central.push(...path);
  }
  const centralOffset = out.length;
  out.push(...central);
  u32(out, 0x06054b50); u16(out, 0); u16(out, 0); u16(out, entries.length);
  u16(out, entries.length); u32(out, central.length); u32(out, centralOffset); u16(out, 0);
  return Uint8Array.from(out);
}

function fixture({ includeEula = false } = {}) {
  const catalog = "a".repeat(64);
  const manifestSha256 = "b".repeat(64);
  const mod = { path: "instance/mods/example.jar", bytes: Uint8Array.from([1, 2, 3]) };
  const files = [
    mod,
    { path: "resolved/loader.json", bytes: json({
      minecraftVersion: "1.21.1",
      loader: { kind: "fabric", version: "0.16.10" },
      javaRequirement: { component: "java-runtime-delta", major: 21 },
      runtimeCatalog: { sha256: catalog },
    }) },
    { path: "resolved/mods.json", bytes: json([{
      path: mod.path,
      sha256: sha(mod.bytes),
      sizeBytes: mod.bytes.length,
    }]) },
    { path: "resolved/artifacts.json", bytes: json({
      schemaVersion: 1,
      artifacts: [{
        intent: "mod",
        path: mod.path,
        sha256: sha(mod.bytes),
        sizeBytes: mod.bytes.length,
      }],
    }) },
    { path: "resolved/version.json", bytes: json({
      minecraftVersion: "1.21.1",
      javaVersion: { component: "java-runtime-delta", majorVersion: 21 },
    }) },
  ];
  if (includeEula) {
    files.push({ path: "instance/eula.txt", bytes: json("eula=true") });
  }
  const manifest = {
    schemaVersion: 1,
    instanceName: "pack",
    minecraftVersion: "1.21.1",
    loader: { kind: "fabric", version: "0.16.10" },
    javaRequirement: { component: "java-runtime-delta", major: 21 },
    runtimeCatalog: { sha256: catalog },
    files: files.map((file) => ({
      path: file.path,
      sha256: sha(file.bytes),
      sizeBytes: file.bytes.length,
    })).sort((a, b) => a.path.localeCompare(b.path)),
  };
  const archive = zip([{ path: "bundle.json", bytes: json(manifest) }, ...files]);
  const job = {
    accountId: "account_1",
    serviceId: "service_1",
    deploymentId: "deployment_1",
    compilerRequestId: "request_1",
    manifestSha256,
    expectedContentKey: `shared-hosting/account_1/service_1/compiler-content/${manifestSha256}.tar.zst`,
    frozenManifest: {
      schemaVersion: 1,
      sourceFormat: "xmcl_server_bundle",
      importId: "import_1",
      archive: {
        key: "shared-hosting/account_1/service_1/compiler-inputs/import_1.xmcl-server-bundle",
        sha256: sha(archive),
        sizeBytes: archive.length,
      },
      compatibility: {
        minecraftVersion: "1.21.1",
        loader: "fabric",
        loaderVersion: "0.16.10",
        java: { component: "java-runtime-delta", major: 21 },
        runtimeCatalog: { sha256: catalog },
      },
      mods: [],
    },
  };
  const grants = {
    accountId: job.accountId,
    serviceId: job.serviceId,
    deploymentId: job.deploymentId,
    compilerRequestId: job.compilerRequestId,
    manifestSha256: job.manifestSha256,
    grants: [
      {
        key: job.frozenManifest.archive.key,
        method: "GET",
        url: "https://object.example/input",
        expiresAt: "2026-07-25T00:10:00.000Z",
      },
      {
        key: job.expectedContentKey,
        method: "PUT",
        url: "https://object.example/output",
        expiresAt: "2026-07-25T00:10:00.000Z",
        headers: { "if-none-match": "*" },
      },
    ],
  };
  return { archive, job, grants };
}

test("grant verification rejects substituted node or output grants", () => {
  const { job, grants } = fixture();
  assert.equal(verifyGrantSet(grants, job).output.key, job.expectedContentKey);
  const azure = structuredClone(grants);
  azure.grants[1].headers["x-ms-blob-type"] = "BlockBlob";
  assert.deepEqual(verifyGrantSet(azure, job).output.headers, {
    "if-none-match": "*",
    "x-ms-blob-type": "BlockBlob",
  });
  for (const headers of [
    { "if-none-match": "*", "x-ms-blob-type": "AppendBlob" },
    { "if-none-match": "*", "x-ms-blob-type": "blockblob" },
    { "if-none-match": "*", "x-ms-version": "2025-01-05" },
  ]) {
    const invalid = structuredClone(grants);
    invalid.grants[1].headers = headers;
    assert.throws(() => verifyGrantSet(invalid, job), /invalid_grants/);
  }
  assert.throws(
    () => verifyGrantSet({ ...grants, callbackUrl: "https://attacker.example" }, job),
    /invalid_grants/,
  );
  const grantInjection = structuredClone(grants);
  grantInjection.grants[1].callbackUrl = "https://attacker.example";
  assert.throws(() => verifyGrantSet(grantInjection, job), /invalid_grants/);
  assert.throws(
    () => verifyGrantSet({ ...grants, grants: [{ ...grants.grants[0], key: "world/revision" }, grants.grants[1]] }, job),
    /invalid_grants/,
  );
});

test("world seed handling is exact-grant-only and fails closed without a restore adapter", async () => {
  const archive = Uint8Array.from([1, 2, 3]);
  const hash = sha(archive);
  const job = {
    accountId: "account_1", serviceId: "service_1", seedId: "seed_1", worldName: "World",
    archive: {
      key: "shared-hosting/account_1/service_1/world-seeds/seed_1.xmcl-world-seed",
      sha256: hash, sizeBytes: archive.byteLength,
    },
  };
  const worker = new WorldSeedWorker({
    controlPlane: {
      getWorldSeedGrants: async () => ({
        accountId: job.accountId, serviceId: job.serviceId, seedId: job.seedId,
        grants: [{ method: "GET", key: job.archive.key, url: "https://object.example/seed", expiresAt: "2026-07-25T00:10:00.000Z" }],
      }),
    },
    fetchImpl: async () => new Response(archive, { headers: { "content-length": String(archive.byteLength) } }),
  });
  await assert.rejects(() => worker.run(job), /world_seed_handler_unavailable/);
});

test("compiler rejects a launcher-provided EULA file", async () => {
  const { archive, job } = fixture({ includeEula: true });
  await assert.rejects(
    () => validateBundle(archive, job.frozenManifest),
    /invalid_bundle_manifest|bundle_hash_or_path_mismatch/,
  );
});


test("the unavailable builder reports a durable failure and never executes the bundle launcher", async () => {
  const { archive, job, grants } = fixture();
  const failures = [];
  let requestedUrl;
  const worker = new CompilerWorker({
    controlPlane: {
      getGrants: async () => grants,
      publish: async () => assert.fail("publish must not run without reviewed builder assets"),
      failed: async (failure) => failures.push(failure),
    },
    fetchImpl: async (url) => {
      requestedUrl = url;
      return new Response(archive, {
        headers: { "content-length": String(archive.length) },
      });
    },
  });
  const result = await worker.run(job);
  assert.equal(result.status, "failed");
  assert.equal(result.code, "compiler_unavailable");
  assert.equal(requestedUrl, grants.grants[0].url);
  assert.deepEqual(failures, [{
    deploymentId: job.deploymentId,
    manifestSha256: job.manifestSha256,
    code: "compiler_unavailable",
  }]);
});

test("reviewed builder uploads only the immutable output grant and publishes validated content", async () => {
  const { archive, job, grants } = fixture();
  grants.grants[1].headers["x-ms-blob-type"] = "BlockBlob";
  const artifact = Uint8Array.from([4, 5, 6]);
  const coordinate = "net.fabricmc:fabric-loader:0.16.10";
  const builder = reviewedBuilder({
    artifact,
    coordinate,
    fail: false,
  });
  const requests = [];
  const publications = [];
  const worker = new CompilerWorker({
    controlPlane: {
      getGrants: async () => grants,
      prepareUpload: async (publication) => prepared(publication),
      publish: async (publication) => publications.push(publication),
      failed: async () => assert.fail("a reviewed successful build must not fail"),
    },
    builder,
    fetchImpl: async (url, options) => {
      requests.push({ url, options });
      if (url === grants.grants[0].url) {
        return new Response(archive, {
          headers: { "content-length": String(archive.length) },
        });
      }
      if (url === grants.grants[1].url) return new Response(null, { status: 200 });
      assert.fail(`unexpected URL ${url}`);
    },
  });
  const result = await worker.run(job);
  assert.deepEqual(result, { status: "published", deploymentId: job.deploymentId });
  assert.deepEqual(requests.map((request) => request.options.method), ["GET", "PUT"]);
  assert.deepEqual(requests[1].options.headers, {
    "if-none-match": "*",
    "x-ms-blob-type": "BlockBlob",
  });
  assert.equal(publications.length, 1);
  assert.equal(publications[0].content.key, job.expectedContentKey);
  assert.ok(publications[0].content.paths.includes(".xmcl/runtime.json"));
  assert.ok(publications[0].content.paths.includes(".xmcl/launch.sh"));
});

test("an installer failure never uploads or publishes content", async () => {
  const { archive, job, grants } = fixture();
  const requests = [];
  const publications = [];
  const failures = [];
  const worker = new CompilerWorker({
    controlPlane: {
      getGrants: async () => grants,
      prepareUpload: async (publication) => prepared(publication),
      publish: async (publication) => publications.push(publication),
      failed: async (failure) => failures.push(failure),
    },
    builder: reviewedBuilder({
      artifact: Uint8Array.from([4, 5, 6]),
      coordinate: "net.fabricmc:fabric-loader:0.16.10",
      fail: true,
    }),
    fetchImpl: async (url, options) => {
      requests.push({ url, options });
      assert.equal(url, grants.grants[0].url);
      return new Response(archive, {
        headers: { "content-length": String(archive.length) },
      });
    },
  });
  const result = await worker.run(job);
  assert.deepEqual(result, { status: "failed", code: "compiler_failed" });
  assert.deepEqual(requests.map((request) => request.options.method), ["GET"]);
  assert.deepEqual(publications, []);
  assert.deepEqual(failures, [{
    deploymentId: job.deploymentId,
    manifestSha256: job.manifestSha256,
    code: "compiler_failed",
  }]);
});

test("reviewed artifact validation failures never upload or publish content", async () => {
  const cases = [
    {
      name: "wrong host",
      transform: (artifact) => ({ ...artifact, url: "https://unreviewed.example/server.jar" }),
      fetchImpl: async () => assert.fail("wrong hosts must reject before download"),
    },
    {
      name: "redirect",
      transform: (artifact) => artifact,
      fetchImpl: async (artifact) => ({
        status: 200,
        redirected: true,
        headers: new Headers({ "content-length": String(artifact.sizeBytes) }),
      }),
    },
    {
      name: "oversize",
      transform: (artifact) => artifact,
      fetchImpl: async (artifact) => new Response(bytes("oversize"), {
        status: 200,
        headers: { "content-length": String(artifact.sizeBytes + 1) },
      }),
    },
    {
      name: "hash mismatch",
      transform: (artifact) => artifact,
      fetchImpl: async (artifact) => new Response(Uint8Array.from([0, 0, 0]), {
        status: 200,
        headers: { "content-length": String(artifact.sizeBytes) },
      }),
    },
  ];
  for (const scenario of cases) {
    const { archive, job, grants } = fixture();
    const artifact = Uint8Array.from([4, 5, 6]);
    const strict = new StrictArtifactDownloader({
      fetchImpl: () => scenario.fetchImpl({
        coordinate: "net.fabricmc:fabric-loader:0.16.10",
        url: "https://toolchain.example/fabric-server.jar",
        sha256: sha(artifact),
        sizeBytes: artifact.byteLength,
      }),
    });
    const downloader = {
      download: (reviewedArtifact, options) => strict.download(
        scenario.transform(reviewedArtifact),
        options,
      ),
    };
    const requests = [];
    const publications = [];
    const worker = new CompilerWorker({
      controlPlane: {
        getGrants: async () => grants,
        publish: async (publication) => publications.push(publication),
        failed: async () => undefined,
      },
      builder: reviewedBuilder({
        artifact,
        coordinate: "net.fabricmc:fabric-loader:0.16.10",
        fail: false,
        artifactDownloader: downloader,
      }),
      fetchImpl: async (url, options) => {
        requests.push({ url, options });
        assert.equal(url, grants.grants[0].url, `${scenario.name} must not use the output grant`);
        return new Response(archive, {
          headers: { "content-length": String(archive.length) },
        });
      },
    });
    const result = await worker.run(job);
    assert.deepEqual(result, { status: "failed", code: "compiler_failed" }, scenario.name);
    assert.deepEqual(requests.map((request) => request.options.method), ["GET"], scenario.name);
    assert.deepEqual(publications, [], scenario.name);
  }
});

test("an upload timeout reconciles the exact durable binding without a failed callback", async () => {
  const { archive, job, grants } = fixture();
  const artifact = Uint8Array.from([4, 5, 6]);
  const built = [];
  const failures = [];
  let uploads = 0;
  const worker = new CompilerWorker({
    controlPlane: {
      getGrants: async () => grants,
      prepareUpload: async (publication) => {
        built.push(publication);
        return prepared(publication);
      },
      publish: async (publication) => {
        assert.deepEqual(publication, built[0]);
      },
      failed: async (failure) => failures.push(failure),
    },
    builder: reviewedBuilder({
      artifact,
      coordinate: "net.fabricmc:fabric-loader:0.16.10",
      fail: false,
    }),
    fetchImpl: async (url, options) => {
      if (url === grants.grants[0].url) {
        return new Response(archive, {
          headers: { "content-length": String(archive.length) },
        });
      }
      if (url === grants.grants[1].url && options.method === "PUT") {
        uploads += 1;
        throw new Error("response lost after object storage accepted the PUT");
      }
      if (options.method === "GET" && url === "https://object.example/reconcile") {
        const published = built[0];
        return new Response(new Uint8Array(0), {
          headers: { "content-length": String(published.content.compressedSize) },
        });
      }
      assert.fail(`unexpected request ${options.method} ${url}`);
    },
  });
  // The GET body needs the archive hash; use the worker's deterministic
  // archive from a first build rather than treating arbitrary bytes as valid.
  const originalFetch = worker.fetch;
  let output;
  worker.fetch = async (url, options) => {
    if (url === grants.grants[1].url && options.method === "PUT") {
      uploads += 1;
      output = options.body;
      throw new Error("response lost after object storage accepted the PUT");
    }
    if (options.method === "GET" && url === "https://object.example/reconcile") {
      return new Response(output, {
        headers: { "content-length": String(output.byteLength) },
      });
    }
    return originalFetch(url, options);
  };
  const result = await worker.run(job);
  assert.deepEqual(result, { status: "published", deploymentId: job.deploymentId });
  assert.equal(uploads, 1);
  assert.deepEqual(failures, []);
});

test("an immutable 412 reconciles a prior exact binding on redelivery", async () => {
  const { archive, job, grants } = fixture();
  const artifact = Uint8Array.from([4, 5, 6]);
  let stored;
  const publications = [];
  const worker = new CompilerWorker({
    controlPlane: {
      getGrants: async () => grants,
      prepareUpload: async (publication) => prepared(publication),
      publish: async (publication) => publications.push(publication),
      failed: async () => assert.fail("an already-existing object must not fail"),
    },
    builder: reviewedBuilder({
      artifact,
      coordinate: "net.fabricmc:fabric-loader:0.16.10",
      fail: false,
    }),
    fetchImpl: async (url, options) => {
      if (url === grants.grants[0].url) {
        return new Response(archive, {
          headers: { "content-length": String(archive.length) },
        });
      }
      if (url === grants.grants[1].url && options.method === "PUT") {
        stored = options.body;
        return new Response(null, { status: 412 });
      }
      if (url === "https://object.example/reconcile" && options.method === "GET") {
        return new Response(stored, {
          headers: { "content-length": String(stored.byteLength) },
        });
      }
      assert.fail(`unexpected request ${options.method} ${url}`);
    },
  });
  assert.deepEqual(
    await worker.run(job),
    { status: "published", deploymentId: job.deploymentId },
  );
  assert.equal(publications.length, 1);
});

test("an existing binding retries its missing immutable upload", async () => {
  const { archive, job, grants } = fixture();
  const artifact = Uint8Array.from([4, 5, 6]);
  let stored;
  let reconciliations = 0;
  const publications = [];
  const worker = new CompilerWorker({
    controlPlane: {
      getGrants: async () => grants,
      prepareUpload: async (publication) => ({
        ...prepared(publication),
        status: "upload_existing",
      }),
      publish: async (publication) => publications.push(publication),
      failed: async () => assert.fail("a retryable upload must not fail"),
    },
    builder: reviewedBuilder({
      artifact,
      coordinate: "net.fabricmc:fabric-loader:0.16.10",
      fail: false,
    }),
    fetchImpl: async (url, options) => {
      if (url === grants.grants[0].url) {
        return new Response(archive, {
          headers: { "content-length": String(archive.length) },
        });
      }
      if (url === grants.grants[1].url && options.method === "PUT") {
        stored = options.body;
        return new Response(null, { status: 201 });
      }
      if (url === "https://object.example/reconcile" && options.method === "GET") {
        reconciliations += 1;
        if (!stored) return new Response(null, { status: 404 });
        return new Response(stored, {
          headers: { "content-length": String(stored.byteLength) },
        });
      }
      assert.fail(`unexpected request ${options.method} ${url}`);
    },
  });
  assert.deepEqual(
    await worker.run(job),
    { status: "published", deploymentId: job.deploymentId },
  );
  assert.equal(reconciliations, 1);
  assert.equal(publications.length, 1);
});

function prepared(publication) {
  return {
    status: "upload_prepared",
    publication,
    reconciliation: {
      key: publication.content.key,
      method: "GET",
      url: "https://object.example/reconcile",
      expiresAt: "2026-07-25T00:10:00.000Z",
    },
  };
}

function reviewedBuilder({ artifact, coordinate, fail, artifactDownloader }) {
  const jre = Uint8Array.from([7, 8, 9]);
  const jreDefinition = {
    id: "jre-java-runtime-delta-21",
    sha256: sha(jre),
    component: "java-runtime-delta",
    major: 21,
    runtimeCatalogRevision: "a".repeat(64),
  };
  return new ReviewedRuntimeBuilder({
    toolchainCatalog: {
      schemaVersion: 1,
      catalogVersion: "test-review-1",
      runtimeCatalogRevision: "a".repeat(64),
      approvedArtifactHosts: ["toolchain.example"],
      toolchains: [{
        minecraftVersion: "1.21.1",
        loader: { kind: "fabric", version: "0.16.10" },
        java: { component: "java-runtime-delta", major: 21 },
        jre: jreDefinition,
        artifacts: [{
          role: "primary",
          coordinate,
          url: "https://toolchain.example/fabric-server.jar",
          sha256: sha(artifact),
          sizeBytes: artifact.byteLength,
        }],
        launchTemplate: "fabric-server-jar-v1",
      }],
    },
    verifiedJres: new DeterministicFakeJreRegistry([jreDefinition]),
    sandboxRunner: new DeterministicFakeSandboxRunner({ fail }),
    artifactDownloader: artifactDownloader ??
      new DeterministicFakeArtifactDownloader(new Map([[coordinate, artifact]])),
  });
}

function u16(out, value) {
  out.push(value & 0xff, (value >>> 8) & 0xff);
}

function u32(out, value) {
  u16(out, value & 0xffff);
  u16(out, value >>> 16);
}

function crc32(bytes) {
  let crc = 0xffffffff;
  for (const byte of bytes) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) {
      crc = (crc & 1) ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
    }
  }
  return (crc ^ 0xffffffff) >>> 0;
}
