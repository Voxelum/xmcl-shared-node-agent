import assert from "node:assert/strict";
import { createHash, createHmac } from "node:crypto";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  DeterministicFakeArtifactDownloader,
  DeterministicFakeJreRegistry,
  DeterministicFakeSandboxRunner,
  StrictArtifactDownloader,
} from "../src/reviewed-builder.mjs";
import {
  CompilerHttpWorker,
  createCompilerHttpServer,
  parseUploadPreparation,
  readBoundedCallbackBody,
} from "../src/http-worker.mjs";
import { HmacServiceIdentity, InMemoryReplayCache } from "../src/service-identity.mjs";
import { FilesystemCompilerJobQueue, MemoryCompilerJobQueue } from "../src/job-queue.mjs";
import {
  acquireExecutionLease,
  parseAcaDelivery,
  runAcaCompilerJob,
} from "../src/aca-job-runner.mjs";

const encoder = new TextEncoder();

test("authenticated HTTP worker validates grants, uses the fake isolated sandbox, and signs callbacks", async (t) => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  const callbackEvents = [];
  const callbackPaths = [];
  const callbackVerifier = identity("callback", secret);
  const callbackServer = createServer(async (request, response) => {
    const body = await readBody(request);
    try {
      await callbackVerifier.verifyIncoming({
        method: request.method,
        target: request.url,
        headers: request.headers,
        body,
      });
      const event = JSON.parse(new TextDecoder().decode(body));
      callbackPaths.push(request.url);
      if (request.url.endsWith("/grants")) {
        assert.deepEqual(Object.keys(event).sort(),
          ["compilerRequestId", "deploymentId", "schemaVersion"].sort());
        assert.equal(event.schemaVersion, 1);
        assert.equal(event.deploymentId, fixture.job.deploymentId);
        response.writeHead(200, { "content-type": "application/json" })
          .end(JSON.stringify({
            ...fixture.grants,
            compilerRequestId: event.compilerRequestId,
          }));
      } else if (event.status === "upload_prepared") {
        response.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({
          schemaVersion: 1,
          status: "upload_prepared",
          compilerRequestId: event.compilerRequestId,
          deploymentId: event.deploymentId,
          manifestSha256: event.manifestSha256,
          content: event.content,
          descriptor: event.descriptor,
          reconciliation: {
            key: event.content.key,
            method: "GET",
            url: "https://object.example/reconcile",
            expiresAt: new Date(Date.now() + 60_000).toISOString(),
          },
        }));
      } else {
        callbackEvents.push(event);
        response.writeHead(204).end();
      }
    } catch {
      response.writeHead(401).end();
    }
  });
  const callbackAddress = await listen(callbackServer);
  t.after(() => close(callbackServer));

  const sandboxAdapter = new DeterministicFakeSandboxRunner();
  const requestVerifier = identity("control-plane", secret);
  const requestSigner = identity("control-plane", secret);
  const callbackSigner = identity("callback", secret);
  const jobQueue = new MemoryCompilerJobQueue();
  const worker = new CompilerHttpWorker({
    ...reviewedDependencies(fixture, sandboxAdapter),
    requestAuthenticator: requestVerifier,
    callback: {
      controlPlaneOrigin: `http://127.0.0.1:${callbackAddress.port}`,
      authenticator: callbackSigner,
    },
    fetchImpl: objectStore(fixture),
    jobQueue,
    allowInsecureForTests: true,
  });
  t.after(() => worker.stop());
  assert.equal(worker.health().ready, true);
  const server = createCompilerHttpServer({ worker });
  const address = await listen(server);
  t.after(() => close(server));

  const envelope = messageFor(fixture);
  envelope.grants = structuredClone(envelope.grants);
  for (const grant of envelope.grants.grants) {
    grant.expiresAt = "2000-01-01T00:00:00.000Z";
  }
  const first = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: requestSigner,
    envelope,
  });
  assert.equal(first.status, 202);
  assert.deepEqual(await first.json(), { status: "accepted", deploymentId: fixture.job.deploymentId });
  await waitFor(() => callbackEvents.length === 1);
  assert.equal(sandboxAdapter.calls.length, 1);
  assert.equal(callbackEvents.length, 1);
  assert.equal(callbackEvents[0].status, "published");
  assert.equal(callbackEvents[0].compilerRequestId, fixture.job.compilerRequestId);
  assert.equal(callbackEvents[0].content.key, fixture.job.expectedContentKey);
  assert.deepEqual(callbackPaths, [
    `/v1/internal/shared-runtime-compiler/deployments/${fixture.job.deploymentId}/grants`,
    `/v1/internal/shared-runtime-compiler/deployments/${fixture.job.deploymentId}/upload-prepared`,
    `/v1/internal/shared-runtime-compiler/deployments/${fixture.job.deploymentId}/published`,
  ]);

  const duplicate = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: requestSigner,
    envelope,
  });
  assert.equal(duplicate.status, 202);
  assert.deepEqual(await duplicate.json(), {
    status: "accepted",
    deploymentId: fixture.job.deploymentId,
  });
  assert.equal(sandboxAdapter.calls.length, 1);

  const replay = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: requestSigner,
    envelope,
    reuseHeadersFrom: first.requestHeaders,
  });
  assert.equal(replay.status, 401);
  assert.deepEqual(await replay.json(), { code: "request_unauthorized" });
  assert.equal(callbackEvents.length, 1);

  const callbackInjection = structuredClone(envelope);
  callbackInjection.job.callbackUrl = "https://attacker.example/callback";
  const rejectedCallback = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: requestSigner,
    envelope: callbackInjection,
  });
  assert.equal(rejectedCallback.status, 400);
  assert.deepEqual(await rejectedCallback.json(), { code: "invalid_request" });
  assert.equal(sandboxAdapter.calls.length, 1);
  assert.equal(callbackEvents.length, 1);

  const invalidGrants = structuredClone(envelope);
  invalidGrants.requestId = "request_invalid_grants";
  invalidGrants.job.compilerRequestId = invalidGrants.requestId;
  invalidGrants.grants.compilerRequestId = invalidGrants.requestId;
  invalidGrants.grants.grants[1].key = "shared-hosting/other/output.tar.zst";
  const failed = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: requestSigner,
    envelope: invalidGrants,
  });
  assert.equal(failed.status, 202);
  assert.deepEqual(await failed.json(), {
    status: "accepted",
    deploymentId: fixture.job.deploymentId,
  });
  await waitFor(() => callbackEvents.length === 2);
  assert.equal(sandboxAdapter.calls.length, 2);
  assert.equal(callbackEvents.length, 2);
  assert.equal(callbackPaths.at(-1),
    `/v1/internal/shared-runtime-compiler/deployments/${fixture.job.deploymentId}/published`);
  assert.equal(callbackEvents[1].status, "published");
  assert.equal(callbackEvents[1].compilerRequestId, invalidGrants.requestId);
  assert.equal(callbackEvents[1].deploymentId, fixture.job.deploymentId);
  assert.equal(callbackEvents[1].manifestSha256, fixture.job.manifestSha256);
  assert.deepEqual(callbackEvents[1].content, callbackEvents[0].content);
  assert.deepEqual(callbackEvents[1].descriptor, callbackEvents[0].descriptor);
  assert.deepEqual(await jobQueue.counts(), { pending: 0, archived: 2 });
});

test("ACA recovery requires a fresh envelope for the stable queued request", async (t) => {
  const root = await mkdtemp(join(tmpdir(), "xmcl-aca-recovery-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  let now = Date.now();
  let failedCallbacks = 0;
  const callbackServer = createServer(async (request, response) => {
    await readBody(request);
    if (request.url.endsWith("/grants")) {
      const grants = structuredClone(fixture.grants);
      for (const grant of grants.grants) {
        grant.expiresAt = new Date(now + 60_000).toISOString();
      }
      response.writeHead(200, { "content-type": "application/json" })
        .end(JSON.stringify(grants));
    } else if (request.url.endsWith("/failed")) {
      failedCallbacks += 1;
      response.writeHead(204).end();
    } else {
      response.writeHead(500).end();
    }
  });
  const callbackAddress = await listen(callbackServer);
  t.after(() => close(callbackServer));

  const requestIdentity = identity("control-plane", secret, () => now);
  const callbackIdentity = identity("callback", secret, () => now);
  const sandboxAdapter = new DeterministicFakeSandboxRunner({ fail: true });
  const objectFetch = objectStore(fixture);
  const callbackOrigin = `http://127.0.0.1:${callbackAddress.port}`;
  const pathsFactory = () => ({ queueRoot: join(root, "jobs") });
  const workerFactory = async ({ paths }) => new CompilerHttpWorker({
    ...reviewedDependencies(fixture, sandboxAdapter),
    requestAuthenticator: requestIdentity,
    callback: {
      controlPlaneOrigin: callbackOrigin,
      authenticator: callbackIdentity,
    },
    fetchImpl: (url, options) =>
      url.startsWith(callbackOrigin) ? fetch(url, options) : objectFetch(url, options),
    jobQueue: new FilesystemCompilerJobQueue({ directory: paths.queueRoot }),
    now: () => now,
    allowInsecureForTests: true,
  });
  const deliveryPath = join(root, "delivery.json");
  const leaseRoot = join(root, "leases");
  const leaseFactory = (requestId) => acquireExecutionLease(requestId, {
    root: leaseRoot,
    now: () => now,
    leaseMs: 1_000,
  });

  const oldEnvelope = messageForAt(fixture, now);
  const oldDelivery = await signedAcaDelivery(oldEnvelope,
    identity("control-plane", secret, () => now));
  await writeFile(deliveryPath, JSON.stringify(oldDelivery));
  const firstWorker = await workerFactory({ paths: pathsFactory() });
  await firstWorker.initialize();
  await firstWorker.consume(parseAcaDelivery(encoder.encode(JSON.stringify(oldDelivery))));
  assert.equal((await firstWorker.queue.claim(fixture.job.compilerRequestId)).id,
    fixture.job.compilerRequestId);
  await leaseFactory(fixture.job.compilerRequestId);

  now += 2 * 60_000;
  await assert.rejects(() => runAcaCompilerJob({
    deliveryPath,
    workerFactory,
    acquireLease: leaseFactory,
    pathsFactory,
  }), (error) => error?.code === "request_unauthorized");
  assert.equal(failedCallbacks, 0);

  const freshEnvelope = messageForAt(fixture, now);
  const freshDelivery = await signedAcaDelivery(freshEnvelope,
    identity("control-plane", secret, () => now));
  await writeFile(deliveryPath, JSON.stringify(freshDelivery));
  const recovered = await runAcaCompilerJob({
    deliveryPath,
    workerFactory,
    acquireLease: leaseFactory,
    pathsFactory,
  });
  assert.equal(recovered.accepted.deploymentId, fixture.job.deploymentId);
  assert.equal(recovered.outcome.status, "failed");
  assert.equal(failedCallbacks, 1);
  const recoveredQueue = new FilesystemCompilerJobQueue({ directory: pathsFactory().queueRoot });
  await recoveredQueue.initialize();
  assert.deepEqual(await recoveredQueue.counts(), { pending: 0, archived: 1 });
});

test("default HTTP composition remains fail closed while serving liveness", async (t) => {
  const server = createCompilerHttpServer();
  const address = await listen(server);
  t.after(() => close(server));
  const health = await fetch(`http://127.0.0.1:${address.port}/healthz`);
  assert.equal(health.status, 200);
  assert.deepEqual(await health.json(), { status: "ok", ready: false, activeJobs: 0 });
  const ready = await fetch(`http://127.0.0.1:${address.port}/readyz`);
  assert.equal(ready.status, 503);
  const job = await fetch(`http://127.0.0.1:${address.port}/v1/compiler-jobs`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: "{}",
  });
  assert.equal(job.status, 503);
  assert.deepEqual(await job.json(), { code: "compiler_unavailable" });
});

test("authenticated submissions return 202 before queued compilation completes", async (t) => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  const sandbox = new DeterministicFakeSandboxRunner();
  const assemble = sandbox.assemble.bind(sandbox);
  let release;
  const gate = new Promise((resolve) => {
    release = resolve;
  });
  sandbox.assemble = async (request) => {
    await gate;
    return await assemble(request);
  };
  const worker = new CompilerHttpWorker({
    ...reviewedDependencies(fixture, sandbox),
    requestAuthenticator: identity("control-plane", secret),
    callback: {
      controlPlaneOrigin: "https://control-plane.example",
      authenticator: identity("callback", secret),
      fetchImpl: async (url) => {
        if (url.endsWith("/grants")) {
          return Response.json(fixture.grants);
        }
        throw new Error("callback unavailable");
      },
    },
    fetchImpl: objectStore(fixture),
    jobQueue: new MemoryCompilerJobQueue({ retryDelayMs: 10_000 }),
    allowInsecureForTests: true,
  });
  t.after(async () => {
    release();
    await worker.stop();
  });
  const server = createCompilerHttpServer({ worker });
  const address = await listen(server);
  t.after(() => close(server));
  const response = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: identity("control-plane", secret),
    envelope: messageFor(fixture),
  });
  assert.equal(response.status, 202);
  assert.deepEqual(await response.json(), {
    status: "accepted",
    deploymentId: fixture.job.deploymentId,
  });
  await waitFor(() => worker.health().activeJobs === 1);
  assert.equal(sandbox.calls.length, 0);
  release();
});

test("invalid authentication and envelopes are rejected before durable enqueue", async (t) => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  const queue = new MemoryCompilerJobQueue();
  const worker = new CompilerHttpWorker({
    ...reviewedDependencies(fixture, new DeterministicFakeSandboxRunner()),
    requestAuthenticator: identity("control-plane", secret),
    callback: {
      controlPlaneOrigin: "https://control-plane.example",
      authenticator: identity("callback", secret),
    },
    fetchImpl: objectStore(fixture),
    jobQueue: queue,
    allowInsecureForTests: true,
  });
  t.after(() => worker.stop());
  const server = createCompilerHttpServer({ worker });
  const address = await listen(server);
  t.after(() => close(server));
  const url = `http://127.0.0.1:${address.port}/v1/compiler-jobs`;

  const unauthorized = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: "{}",
  });
  assert.equal(unauthorized.status, 401);
  assert.deepEqual(await queue.counts(), { pending: 0, archived: 0 });

  const invalid = await postSigned({
    url,
    signer: identity("control-plane", secret),
    envelope: { schemaVersion: 1, requestId: "invalid" },
  });
  assert.equal(invalid.status, 400);
  assert.deepEqual(await queue.counts(), { pending: 0, archived: 0 });
});

test("malformed fresh grants fail closed while the durable job remains retryable", async (t) => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  const queue = new MemoryCompilerJobQueue({ retryDelayMs: 10_000 });
  let grantMode = "malformed";
  let grantRequests = 0;
  const callbackEvents = [];
  const worker = new CompilerHttpWorker({
    ...reviewedDependencies(fixture, new DeterministicFakeSandboxRunner()),
    requestAuthenticator: identity("control-plane", secret),
    callback: {
      controlPlaneOrigin: "https://control-plane.example",
      authenticator: identity("callback", secret),
      fetchImpl: async (url, options) => {
        const event = JSON.parse(new TextDecoder().decode(options.body));
        if (url.endsWith("/grants")) {
          grantRequests += 1;
          if (grantMode === "malformed") return new Response("{");
          if (grantMode === "mismatched") {
            return Response.json({
              ...fixture.grants,
              deploymentId: "deployment_other",
            });
          }
          return Response.json(fixture.grants);
        }
        if (url.endsWith("/upload-prepared")) {
          return Response.json({
            schemaVersion: 1,
            status: "upload_prepared",
            compilerRequestId: event.compilerRequestId,
            deploymentId: event.deploymentId,
            manifestSha256: event.manifestSha256,
            content: event.content,
            descriptor: event.descriptor,
            reconciliation: {
              key: event.content.key,
              method: "GET",
              url: "https://object.example/reconcile",
              expiresAt: new Date(Date.now() + 60_000).toISOString(),
            },
          });
        }
        if (url.endsWith("/published")) {
          callbackEvents.push(event);
          return new Response(null, { status: 204 });
        }
        assert.fail("grant refresh failure must not emit a terminal callback");
      },
    },
    fetchImpl: objectStore(fixture),
    jobQueue: queue,
    allowInsecureForTests: true,
  });
  t.after(() => worker.stop());
  const server = createCompilerHttpServer({ worker });
  const address = await listen(server);
  t.after(() => close(server));
  const url = `http://127.0.0.1:${address.port}/v1/compiler-jobs`;
  const signer = identity("control-plane", secret);
  const envelope = messageFor(fixture);

  for (const mode of ["malformed", "mismatched"]) {
    grantMode = mode;
    const response = await postSigned({ url, signer, envelope });
    assert.equal(response.status, 202);
    await waitFor(() => grantRequests >= (mode === "malformed" ? 1 : 2) &&
      worker.health().activeJobs === 0);
    assert.deepEqual(await queue.counts(), { pending: 1, archived: 0 });
  }

  grantMode = "valid";
  const recovered = await postSigned({ url, signer, envelope });
  assert.equal(recovered.status, 202);
  await waitFor(async () => (await queue.counts()).archived === 1);
  assert.equal(callbackEvents.length, 1);
  assert.equal(callbackEvents[0].status, "published");
});

test("chunked upload preparation is incrementally read below 4 MiB", async () => {
  const job = {
    compilerRequestId: "request-1",
    deploymentId: "deployment-1",
    manifestSha256: "a".repeat(64),
    expectedContentKey: "shared-hosting/output.tar.zst",
  };
  const raw = encoder.encode(JSON.stringify({
    schemaVersion: 1,
    status: "upload_prepared",
    compilerRequestId: job.compilerRequestId,
    deploymentId: job.deploymentId,
    manifestSha256: job.manifestSha256,
    content: {
      key: job.expectedContentKey,
      sha256: "b".repeat(64),
      compressedSize: 1,
      logicalSize: 1,
      paths: ["server.jar"],
    },
    descriptor: { schemaVersion: 1 },
    reconciliation: {
      key: job.expectedContentKey,
      method: "GET",
      url: "https://object.example/reconcile",
      expiresAt: "2030-01-01T00:00:00.000Z",
    },
  }));
  let offset = 0;
  const response = new Response(new ReadableStream({
    pull(controller) {
      if (offset === raw.byteLength) return controller.close();
      const end = Math.min(offset + 7, raw.byteLength);
      controller.enqueue(raw.subarray(offset, end));
      offset = end;
    },
  }), { headers: { "content-type": "application/json" } });
  assert.equal(response.headers.has("content-length"), false);
  const parsed = await parseUploadPreparation(response, job);
  assert.equal(parsed.status, "upload_prepared");
  assert.equal(parsed.publication.content.key, job.expectedContentKey);
});

test("chunked upload preparation cancels immediately above 4 MiB", async () => {
  const chunks = [new Uint8Array(4 * 1024 * 1024), Uint8Array.of(1)];
  let index = 0;
  let cancelled = false;
  const response = new Response(new ReadableStream({
    pull(controller) {
      controller.enqueue(chunks[index]);
      index += 1;
    },
    cancel() {
      cancelled = true;
    },
  }));
  assert.equal(response.headers.has("content-length"), false);
  await assert.rejects(() => readBoundedCallbackBody(response),
    /invalid_upload_preparation/);
  assert.equal(cancelled, true);
  assert.equal(index, 2);
});

test("HMAC identity binds the exact body and rejects expired timestamps", async () => {
  const secret = "local-integration-test-secret-must-be-32-bytes";
  const body = encoder.encode('{"job":"exact"}');
  const signer = new HmacServiceIdentity({
    keyId: "control-plane",
    secret,
    clock: () => 10_000,
  });

  const verifier = new HmacServiceIdentity({
    keyId: "control-plane",
    secret,
    clock: () => 10_000,
  });
  const headers = await signer.signOutgoing({
    method: "POST",
    target: "/v1/compiler-jobs",
    body,
  });
  await verifier.verifyIncoming({
    method: "POST",
    target: "/v1/compiler-jobs",
    headers,
    body,
  });

  const tamperVerifier = new HmacServiceIdentity({
    keyId: "control-plane",
    secret,
    clock: () => 10_000,
  });
  await assert.rejects(() => tamperVerifier.verifyIncoming({
    method: "POST",
    target: "/v1/compiler-jobs",
    headers,
    body: encoder.encode('{"job":"other"}'),
  }), /request_identity_rejected/);

  const staleVerifier = new HmacServiceIdentity({
    keyId: "control-plane",
    secret,
    clock: () => 80_001,
  });
  await assert.rejects(() => staleVerifier.verifyIncoming({
    method: "POST",
    target: "/v1/compiler-jobs",
    headers,
    body,
  }), /request_identity_rejected/);
});

test("HMAC identity interoperates with the API canonical format and rejects replay", async () => {
  const keyId = "compiler-staging";
  const secret = "api-shared-hmac-secret-at-least-32-bytes";
  const timestamp = 50_000;
  const nonce = "apiCanonicalNonce_123456789";
  const method = "POST";
  const target = "/v1/compiler-jobs";
  const body = encoder.encode('{"schemaVersion":1,"requestId":"request_1"}');
  const digest = createHash("sha256").update(body).digest("hex");
  const canonical = `${method}\n${target}\n${timestamp}\n${nonce}\n${digest}`;
  const signature = createHmac("sha256", secret).update(canonical).digest("base64url");
  const headers = {
    authorization: `HMAC ${keyId}:${signature}`,
    "x-xmcl-timestamp": String(timestamp),
    "x-xmcl-nonce": nonce,
  };
  const identity = new HmacServiceIdentity({
    keyId,
    secret,
    nonceStore: new InMemoryReplayCache(),
    clock: () => timestamp,
  });
  assert.equal(identity.replayProtected, false);
  await identity.verifyIncoming({ method, target, headers, body });
  await assert.rejects(
    () => identity.verifyIncoming({ method, target, headers, body }),
    /request_replayed/,
  );
  assert.throws(() => new HmacServiceIdentity({
    keyId,
    secret,
    nonceStore: new InMemoryReplayCache(),
    requireDurableReplay: true,
  }), /invalid HMAC service identity configuration/);
});

test("in-memory HMAC replay state cannot compose a production-ready worker", () => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  assert.throws(
    () => new InMemoryReplayCache({ durable: true }),
    /invalid replay cache configuration/,
  );
  const localHmac = identity("control-plane", secret);
  assert.equal(localHmac.replayProtected, false);
  assert.throws(() => {
    localHmac.replayProtected = true;
  }, /read only/);
  const worker = new CompilerHttpWorker({
    ...reviewedDependencies(fixture, new DeterministicFakeSandboxRunner()),
    artifactDownloader: new StrictArtifactDownloader({
      fetchImpl: async () => new Response("not used"),
    }),
    requestAuthenticator: localHmac,
    callback: {
      controlPlaneOrigin: "https://control-plane.example",
      authenticator: identity("callback", secret),
    },
  });
  assert.equal(worker.health().ready, false);
});

test("callback composition accepts only an exact control-plane origin", () => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  for (const controlPlaneOrigin of [
    "https://control-plane.example/arbitrary-path",
    "https://control-plane.example/?callback=other",
    "https://user@control-plane.example",
  ]) {
    const worker = new CompilerHttpWorker({
      ...reviewedDependencies(fixture, new DeterministicFakeSandboxRunner()),
      requestAuthenticator: identity("control-plane", secret),
      callback: {
        controlPlaneOrigin,
        authenticator: identity("callback", secret),
      },
      allowInsecureForTests: true,
    });
    assert.equal(worker.health().ready, false);
  }
});

test("a published callback with a lost response never emits a failed callback", async (t) => {
  const fixture = compilerFixture();
  const secret = "local-integration-test-secret-must-be-32-bytes";
  const callbackEvents = [];
  const callbackVerifier = identity("callback", secret);
  const callbackServer = createServer(async (request, response) => {
    const body = await readBody(request);
    await callbackVerifier.verifyIncoming({
      method: request.method,
      target: request.url,
      headers: request.headers,
      body,
    });
    const event = JSON.parse(new TextDecoder().decode(body));
    if (request.url.endsWith("/grants")) {
      response.writeHead(200, { "content-type": "application/json" })
        .end(JSON.stringify(fixture.grants));
    } else if (event.status === "upload_prepared") {
      response.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({
        schemaVersion: 1,
        status: "upload_prepared",
        compilerRequestId: event.compilerRequestId,
        deploymentId: event.deploymentId,
        manifestSha256: event.manifestSha256,
        content: event.content,
        descriptor: event.descriptor,
        reconciliation: {
          key: event.content.key,
          method: "GET",
          url: "https://object.example/reconcile",
          expiresAt: new Date(Date.now() + 60_000).toISOString(),
        },
      }));
    } else {
      callbackEvents.push(event);
      response.writeHead(204).end();
    }
  });
  const callbackAddress = await listen(callbackServer);
  t.after(() => close(callbackServer));

  const sandboxAdapter = new DeterministicFakeSandboxRunner();
  const worker = new CompilerHttpWorker({
    ...reviewedDependencies(fixture, sandboxAdapter),
    requestAuthenticator: identity("control-plane", secret),
    callback: {
      controlPlaneOrigin: `http://127.0.0.1:${callbackAddress.port}`,
      authenticator: identity("callback", secret),
      fetchImpl: async (url, options) => {
        const accepted = await fetch(url, options);
        if (url.endsWith("/published")) {
          throw new Error("simulated response loss after callback acceptance");
        }
        return accepted;
      },
    },
    fetchImpl: objectStore(fixture),
    jobQueue: new MemoryCompilerJobQueue({ retryDelayMs: 10_000 }),
    allowInsecureForTests: true,
  });
  t.after(() => worker.stop());
  const server = createCompilerHttpServer({ worker });
  const address = await listen(server);
  t.after(() => close(server));

  const response = await postSigned({
    url: `http://127.0.0.1:${address.port}/v1/compiler-jobs`,
    signer: identity("control-plane", secret),
    envelope: messageFor(fixture),
  });
  assert.equal(response.status, 202);
  assert.deepEqual(await response.json(), {
    status: "accepted",
    deploymentId: fixture.job.deploymentId,
  });
  await waitFor(() => callbackEvents.length === 1);
  assert.equal(sandboxAdapter.calls.length, 1);
  assert.deepEqual(callbackEvents.map((event) => event.status), ["published"]);
  assert.deepEqual(await worker.queue.counts(), { pending: 1, archived: 0 });
});

function reviewedDependencies(fixture, sandboxAdapter) {
  const artifact = Uint8Array.from([4, 5, 6]);
  const coordinate = "net.fabricmc:fabric-loader:0.16.10";
  const jre = Uint8Array.from([7, 8, 9]);
  const jreDefinition = {
    id: "jre-java-runtime-delta-21",
    sha256: sha(jre),
    component: "java-runtime-delta",
    major: 21,
    runtimeCatalogRevision: fixture.catalog,
  };
  return {
    toolchainCatalog: {
      schemaVersion: 1,
      catalogVersion: "local-integration-test",
      runtimeCatalogRevision: fixture.catalog,
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
    sandboxAdapter,
    artifactDownloader: new DeterministicFakeArtifactDownloader(new Map([[coordinate, artifact]])),
  };
}

function compilerFixture() {
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
      artifacts: [{ intent: "mod", path: mod.path, sha256: sha(mod.bytes), sizeBytes: mod.bytes.length }],
    }) },
    { path: "resolved/version.json", bytes: json({
      minecraftVersion: "1.21.1",
      javaVersion: { component: "java-runtime-delta", majorVersion: 21 },
    }) },
  ];
  const manifest = {
    schemaVersion: 1,
    instanceName: "pack",
    minecraftVersion: "1.21.1",
    loader: { kind: "fabric", version: "0.16.10" },
    javaRequirement: { component: "java-runtime-delta", major: 21 },
    runtimeCatalog: { sha256: catalog },
    files: files.map((file) => ({
      path: file.path, sha256: sha(file.bytes), sizeBytes: file.bytes.length,
    })).sort((left, right) => left.path.localeCompare(right.path)),
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
  const expiresAt = new Date(Date.now() + 60_000).toISOString();
  return {
    archive,
    catalog,
    job,
    grants: {
      accountId: job.accountId,
      serviceId: job.serviceId,
      deploymentId: job.deploymentId,
      manifestSha256: job.manifestSha256,
      compilerRequestId: job.compilerRequestId,
      grants: [
        { key: job.frozenManifest.archive.key, method: "GET", url: "https://object.example/input", expiresAt },
        {
          key: job.expectedContentKey,
          method: "PUT",
          url: "https://object.example/output",
          expiresAt,
          headers: { "if-none-match": "*" },
        },
      ],
    },
  };
}

function messageFor(fixture) {
  const issuedAt = new Date().toISOString();
  return {
    schemaVersion: 1,
    requestId: fixture.job.compilerRequestId,
    issuedAt,
    expiresAt: new Date(Date.now() + 60_000).toISOString(),
    job: fixture.job,
    grants: fixture.grants,
  };
}

function objectStore(fixture) {
  return async (url, options) => {
    if (url === fixture.grants.grants[0].url && options.method === "GET") {
      return new Response(fixture.archive, {
        headers: { "content-length": String(fixture.archive.byteLength) },
      });
    }
    if (url === fixture.grants.grants[1].url && options.method === "PUT") {
      return new Response(null, { status: 200 });
    }
    assert.fail(`unexpected object-store request ${options.method} ${url}`);
  };
}

function identity(keyId, secret, clock = Date.now) {
  return new HmacServiceIdentity({
    keyId,
    secret,
    nonceStore: new InMemoryReplayCache(),
    clock,
  });
}

function messageForAt(fixture, now) {
  return {
    ...messageFor(fixture),
    issuedAt: new Date(now).toISOString(),
    expiresAt: new Date(now + 60_000).toISOString(),
  };
}

async function signedAcaDelivery(envelope, signer) {
  const body = encoder.encode(JSON.stringify(envelope));
  return {
    schemaVersion: 1,
    method: "POST",
    target: "/v1/compiler-jobs",
    headers: await signer.signOutgoing({
      method: "POST",
      target: "/v1/compiler-jobs",
      body,
    }),
    bodyBase64: Buffer.from(body).toString("base64"),
  };
}

async function postSigned({ url, signer, envelope, reuseHeadersFrom }) {
  const body = encoder.encode(JSON.stringify(envelope));
  const headers = reuseHeadersFrom ?? await signer.signOutgoing({
    method: "POST",
    target: "/v1/compiler-jobs",
    body,
  });
  const response = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json", ...headers },
    body,
  });
  response.requestHeaders = headers;
  return response;
}

function zip(entries) {
  const out = [];
  const central = [];
  for (const entry of entries) {
    const path = encoder.encode(entry.path);
    const bytes = entry.bytes;
    const offset = out.length;
    const crc = crc32(bytes);
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
  const offset = out.length;
  out.push(...central);
  u32(out, 0x06054b50); u16(out, 0); u16(out, 0); u16(out, entries.length);
  u16(out, entries.length); u32(out, central.length); u32(out, offset); u16(out, 0);
  return Uint8Array.from(out);
}

function json(value) {
  return encoder.encode(JSON.stringify(value));
}

function sha(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

async function waitFor(predicate, timeoutMs = 2_000) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error("timed out waiting for compiler job");
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
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

async function listen(server) {
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  return server.address();
}

async function close(server) {
  await new Promise((resolve) => server.close(resolve));
}

async function readBody(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return Buffer.concat(chunks);
}
