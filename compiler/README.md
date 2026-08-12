# XMCL shared Minecraft compiler

This package lives under `compiler/` in the
`Voxelum/xmcl-shared-node-agent` monorepo. It remains an independently built
and deployed service.

This repository owns the egress-isolated compiler worker boundary. It accepts a
deployment identity, obtains exactly one compiler input GET grant and one
immutable content PUT grant from the control plane, then revalidates the
`.xmcl-server-bundle` before any builder can see it.

The bundle is a content-and-metadata handoff from an already-working modded
client instance, not a local dedicated-server export. It carries selected
instance artifacts and their hashes plus loader/version metadata; a reviewed
builder must assemble the dedicated-server runtime independently.

`FailClosedRuntimeBuilder` remains the default. A deployment can explicitly
compose `ReviewedRuntimeBuilder` (or `createReviewedCompilerWorker`) only with:

- a versioned `ReviewedToolchainCatalog` bound to the exact raw runtime-catalog
  SHA, exact loader coordinate, approved HTTPS host, artifact URL, size, and
  SHA-256;
- a verified, read-only JRE registry whose selected root matches the catalog's
  component, major, digest, and runtime-catalog revision;
- a sandbox adapter that attests to an ephemeral non-root workspace, read-only
  base filesystem, no secrets or Docker socket, bounded resources, and disabled
  installer network; and
- an exact-artifact downloader. `StrictArtifactDownloader` rejects non-HTTPS or
  unapproved hosts, redirects, absent/wrong sizes, timeout, and hash mismatch.

The catalog supports deterministic Forge, Fabric, NeoForge, and Quilt assembly
plans. The compiler owns the plan and launcher arguments; it never executes
`server.sh`, local Java paths, Docker options, launcher-provided JVM arguments,
or a catalog-provided command.

Successful reviewed builds copy only revalidated server-relevant local content,
generate `.xmcl/runtime.json` and `.xmcl/launch.sh`, and emit a deterministic
USTAR stream in a valid raw Zstandard frame (`.tar.zst`). Before the immutable
PUT, the worker rechecks every packaged output path, mode, size, SHA-256,
runtime descriptor, and generated launcher.

`DeterministicFakeArtifactDownloader`, `DeterministicFakeJreRegistry`, and
`DeterministicFakeSandboxRunner` are test doubles only. No production sandbox,
JRE root, reviewed catalog, or artifact mirror is bundled here; missing or
invalid injected dependencies return `compiler_unavailable`.

Local-world migration is a separate `.xmcl-world-seed` path. `WorldSeedWorker`
accepts exactly one control-plane-issued GET grant bound to an account, service,
seed ID, archive key, size, and SHA-256; it has no list/delete grant and the
default `FailClosedWorldSeedHandler` cannot unpack or restore anything. A
production handler must validate the archive again, restore only the selected
initial world supplied on a first-start command, and atomically refuse existing
or completed runtime worlds.

## Deployment prerequisites

- Run the image as the non-root user already declared in the Dockerfile with a
  read-only root filesystem, an ephemeral writable workspace, no Docker socket,
  no host mounts, and PID/CPU/memory limits.
- Use mTLS or an equivalent workload identity for the four internal control
  plane callbacks: grant retrieval, durable upload preparation, immutable
  publish, and durable failure
  reporting. Do not inject browser, node, billing, or object-store master
  credentials.
- Permit HTTPS egress only to reviewed artifact origins while a future reviewed
  builder acquires exact catalog artifacts; disable egress before server-loader
  assembly.
- The API owns durable deployment state. Retries use the same deployment ID,
  manifest SHA, exact input key, and immutable `If-None-Match: *` output grant.
- Install real reviewed Forge/Fabric/NeoForge/Quilt artifacts, approved mirror
  allowlists, matching verified JRE roots, and a sandbox adapter that enforces
  the requested attestations before composing the reviewed builder. Until then,
  retain the default fail-closed worker.

Run `npm test` and `npm run check` locally. This package intentionally does not
claim live loader compilation until those external reviewed adapters exist.

## Reviewed toolchain catalog

`toolchain-catalog.lock.json` is generated only from the reviewed
`runtime-image/runtime-catalog.lock.json` in the repository root and official
loader metadata. The catalog revision is the SHA-256 of its canonical JSON projection
with `catalogRevision` zeroed, avoiding a self-referential digest while keeping
the tracked lock deterministic and reviewable.

```text
node scripts/update_toolchain_catalog.mjs \
  --runtime-catalog-lock ../runtime-image/runtime-catalog.lock.json
node scripts/validate_toolchain_catalog.mjs \
  --runtime-catalog-lock ../runtime-image/runtime-catalog.lock.json
```

Generation permits only the explicit compatibility candidates in
`src/toolchain-catalog.mjs`. It downloads official metadata and the exact
approved artifacts with HTTPS-only, no-redirect, bounded-size requests; it
verifies published SHA-1 checksums where available, then records each artifact's
computed SHA-256 and byte size. Validation rejects an unbound runtime revision,
unselected Java component/major, unsupported URL/host/path, duplicate tuple,
incorrect template, or missing primary/Mojang server artifact.

The weekly workflow only validates and refreshes this lock, then opens a review
PR if it changed. It does not compose a compiler worker, run installers, build
or publish an image, upload content, or provision infrastructure.

## HTTP worker and queue consumer

`src/http-worker.mjs` provides `CompilerHttpWorker` and
`createCompilerHttpServer`. The server exposes:

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | unauthenticated liveness only; returns 200 even when compilation is fail-closed |
| `GET /readyz` | returns 200 only after every reviewed dependency and identity adapter is configured |
| `POST /v1/compiler-jobs` | authenticated synchronous queue-message consumption |

The HTTP response acknowledges only after the worker has attempted the
immutable upload and the authenticated callback. Before the PUT, the worker
posts its reviewed content digest and descriptor to `uploadPreparationUrl`.
The control plane durably binds that exact payload and returns a single-key GET
reconciliation grant. A 200 response with
`{"status":"failed"}` means the control-plane failure callback was attempted;
the control plane owns retry policy and must treat
`compilerRequestId` as an idempotency key. A busy worker returns 429 before
starting a sandbox. The default concurrency is one and is capped at sixteen.
Once the immutable upload succeeds and published-callback delivery begins, any
callback timeout, transport error, or non-2xx response returns
`published_callback_uncertain` instead. That result includes the exact
publication payload for an authenticated control plane to retry as the same
published event; the worker never sends a contradictory failed callback after
publication has begun.

After upload preparation begins, a PUT timeout, transport error, non-success
response (including `412 If-None-Match: *`), or failed reconciliation returns
`upload_reconciliation_uncertain`, never `failed`. The worker reconciles only
through that control-plane-issued GET grant, checks the exact object length and
SHA-256 against the durable binding, and then publishes it. A redelivery that
receives 412 follows the same path; it never publishes an arbitrary existing
key. If reconciliation cannot prove the object, retry the same request.

The endpoint accepts only this schema-versioned envelope (additional fields are
rejected):

```json
{
  "schemaVersion": 1,
  "requestId": "exact-compiler-request-id",
  "issuedAt": "2026-07-25T08:00:00.000Z",
  "expiresAt": "2026-07-25T08:05:00.000Z",
  "job": { "compilerRequestId": "exact-compiler-request-id", "...": "existing CompilerWorker job fields" },
  "grants": {
    "compilerRequestId": "exact-compiler-request-id",
    "accountId": "…",
    "serviceId": "…",
    "deploymentId": "…",
    "manifestSha256": "…",
    "grants": ["the one exact GET grant and one immutable PUT grant"]
  }
}
```

`requestId`, `job.compilerRequestId`, and `grants.compilerRequestId` must be
identical. The envelope lifetime is at most five minutes. Before any download
or sandbox call, the worker reuses `validateCompilerJob` and `verifyGrantSet`
to bind account, service, deployment, manifest SHA, input archive key, output
key, GET/PUT methods, HTTPS grant URLs, unexpired grant timestamps, no input
headers, and exactly `If-None-Match: *` on the output grant. It then reuses the
bundle and reviewed-toolchain validation described above. Neither the envelope
nor its grants can select callbacks, commands, Java paths, JVM arguments,
Docker options, or sandbox settings.

Object-grant GET/PUT operations are bounded to 30 seconds and stream the input
with an exact byte cap; callbacks are bounded to 10 seconds. Timeouts are
reported through the fixed `compiler_failed` callback path only before upload
preparation begins. Published-callback delivery is instead reported as
`published_callback_uncertain`; upload uncertainty is reported as
`upload_reconciliation_uncertain` so it can be reconciled safely.

Completion and failure go only to the callback URL configured when the worker
is composed. Upload preparation uses a separately configured immutable
`uploadPreparationUrl`; it is HTTPS in production, cannot contain credentials
or a fragment, does not follow redirects, and is never read from a job. A
completion includes the validated content and runtime descriptor; a failure
includes only the fixed failure code. Callback receivers must authenticate and
deduplicate the same `compilerRequestId`.

## Service identity and replay protection

Every queue delivery and callback uses the `ServiceIdentity` adapter boundary:

```text
verifyIncoming({ method, target, headers, body, transport })
signOutgoing({ method, target, body })
```

Adapters must provide signed service identity plus timestamp and nonce replay
checks. `HmacServiceIdentity` is supplied for a private HMAC key of at least
32 bytes for local/integration testing only. It signs:

```text
METHOD \n PATH_AND_QUERY \n UNIX_MILLISECONDS \n NONCE \n SHA256(BODY)
```

in `Authorization: HMAC <key-id>:<base64url-signature>`, with
`X-Xmcl-Timestamp` and `X-Xmcl-Nonce`. It accepts timestamps within 60 seconds
and rejects reused nonces. Its in-memory nonce cache is **local test/single
process only**, cannot be marked durable, and `HmacServiceIdentity` is always
ineligible for production worker composition. A production HMAC deployment must
implement a distinct `ServiceIdentity` adapter backed by an atomic shared
replay store (for example, control-plane/Redis conditional insert); each
callback receiver must enforce the same policy.

JWT and mTLS deployments use the same adapter boundary. A JWT adapter must bind
method, target, body digest, issuance time, expiry, and unique token ID. An
mTLS adapter must validate the peer workload identity through trusted transport
state and still enforce a signed/timestamped nonce; an outbound mTLS adapter
must use a trusted client-certificate transport. Do not terminate identity
checks in untrusted job code or accept a browser/node/object-store credential.

## Trusted composition and sandbox boundary

`CompilerHttpWorker` remains unavailable unless code owned by the deployment
injects all of the following:

1. a valid `ReviewedToolchainCatalog`;
2. a verified JRE registry that returns a matching `verified`, read-only root;
3. an isolated `sandboxAdapter` with the required ephemeral/non-root/read-only,
   no-secrets/no-Docker, bounded-resource, approved-artifact-only capabilities;
4. `StrictArtifactDownloader` (or the explicitly test-only fake path);
5. replay-protected inbound and callback service-identity adapters; and
6. an immutable configured callback URL and authenticated callback transport.

The adapter receives only the compiler-owned assembly plan, exact reviewed
artifact bytes, and exact JRE root token. It has one narrow
`assemble(request)` operation. This package does not spawn a shell, execute a
bundle file, select a JVM, invoke Docker, or load an adapter module from an
environment variable. Never make a job field into adapter configuration.

No approved production sandbox, read-only JRE roots, or installer execution
adapter is included in this repository. The included container consequently
starts liveness only and returns 503 from `/readyz` and job delivery until a
trusted deployment image composes the reviewed adapters in code. This is
intentional: Forge, Fabric, NeoForge, and Quilt installers are **not claimed
production-ready** here.

## Container, environment, and image publication

The image runs `src/worker-server.mjs` as UID/GID `10001`, exposes port 8080,
and has a liveness health check against `/healthz`.

| Environment variable | Default | Allowed values | Meaning |
| --- | --- | --- | --- |
| `HOST` | `0.0.0.0` | `0.0.0.0`, `127.0.0.1`, `::` | HTTP bind address |
| `PORT` | `8080` | `1`–`65535` | HTTP bind port |
| `NODE_ENV` | `production` | platform value | Node runtime mode; it does not enable compilation |

There are deliberately no environment variables for catalog URLs, installer
commands, Java paths, Docker options, sandbox commands, callback URLs, or
identity secrets. A trusted deployment integration supplies reviewed objects
and secret-backed identity adapters without making them user-controlled.

Run the container with a read-only root filesystem, no host mounts or Docker
socket, no added capabilities, an ephemeral writable workspace owned by the
non-root UID if the reviewed sandbox needs one, and CPU/memory/PID/network
limits. A default image being live is not evidence that it is ready to compile.

`.github/workflows/publish-compiler-image.yml` at the repository root validates
tests and publishes only a unique
`sha-<full-git-sha>` GHCR tag, and attaches BuildKit provenance and SBOM
attestations. Tags are convenience references; deploy the resulting immutable
`ghcr.io/voxelum/xmcl-shared-node-compiler@sha256:...` digest after
verifying the provenance and SBOM. The workflow never publishes `latest`.
