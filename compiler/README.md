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

The standalone worker server now composes `ReviewedRuntimeBuilder` from fixed
production paths. Library callers still remain fail closed unless they
explicitly compose `ReviewedRuntimeBuilder` (or `createReviewedCompilerWorker`)
with:

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
`DeterministicFakeSandboxRunner` are test doubles only. The deployable server
never imports them. It uses `StrictArtifactDownloader`,
`VerifiedReadOnlyJreRegistry`, and `BubblewrapSandboxAdapter`; missing or
invalid production dependencies leave `/readyz` unavailable.

Local-world migration is a separate `.xmcl-world-seed` path. `WorldSeedWorker`
accepts exactly one control-plane-issued GET grant bound to an account, service,
seed ID, archive key, size, and SHA-256; it has no list/delete grant and the
default `FailClosedWorldSeedHandler` cannot unpack or restore anything. A
production handler must validate the archive again, restore only the selected
initial world supplied on a first-start command, and atomically refuse existing
or completed runtime worlds.

## Deployment prerequisites

- Run the supported host systemd unit as UID/GID 10001 with its read-only
  application/config/JRE paths, ephemeral workspace, persistent replay path,
  empty capability sets, and PID/CPU/memory limits.
- Use the control-plane-compatible HMAC service identity for queue submissions,
  exact grant refresh, durable upload preparation, immutable publish, and
  durable failure reporting. The initial closed envelope carries no authority
  beyond its short-lived submission; each processing attempt obtains fresh,
  deployment-bound object grants through the authenticated grants route. Do
  not inject browser, node, billing, or object-store master credentials.
- Permit worker HTTPS egress only to the callback/object grants and reviewed
  artifact origins. Bubblewrap unshares the network namespace before any
  installer code executes.
- The API owns durable deployment state. Retries use the same deployment ID,
  manifest SHA, exact input key, and immutable `If-None-Match: *` output grant.
- Materialize the exact Java 21 root listed in `jre-registry.example.json`.
  Production composition restricts its catalog view to NeoForge, so Forge,
  Fabric, and Quilt reject as `unsupported_compatibility` before any artifact
  download. Their unused Java 8/16/17/25 roots are neither accepted nor
  required.

Run `npm test` and `npm run check` locally. This package intentionally does not
claim live loader compilation from the fake-runner unit test. Before deploying
a catalog/JRE pair, run the opt-in Linux integration test against the exact
materialized JRE on a read-only mount:

```text
XMCL_RUN_NEOFORGE_INTEGRATION=1 \
XMCL_REVIEWED_JAVA_21_ROOT=/opt/xmcl/jres/java-runtime-delta-21 \
XMCL_INTEGRATION_WORKSPACE_ROOT=/var/lib/xmcl-compiler/integration \
npm run test:integration:neoforge
```

The test verifies the complete materialized JRE through
`VerifiedReadOnlyJreRegistry`, downloads all 83 reviewed NeoForge 1.21.1
artifacts through `StrictArtifactDownloader`, executes the rewritten installer
offline through the real `prlimit`/Bubblewrap adapter, checks every catalog
library and generated `unix_args.txt`, and starts the resulting server through
its bounded `--help` path in a second network-disabled Bubblewrap sandbox. It
fails unless the JRE source is an actual read-only mount. Optional
`XMCL_BWRAP_PATH` and `XMCL_PRLIMIT_PATH` values must be absolute paths.

### Reproducible reviewed Java 21 root

Do not install Java with apt and do not create the resolution manifest by hand.
From a checkout containing the pinned runtime and toolchain locks, run:

```text
cd compiler
node scripts/materialize_verified_jre.mjs \
  --runtime-catalog-lock ../runtime-image/runtime-catalog.lock.json \
  --toolchain-catalog-lock ./toolchain-catalog.lock.json \
  --jre-id java-runtime-delta-21 \
  --output-root /srv/xmcl-compiler/jres/java-runtime-delta-21
```

The materializer fails if the destination already exists. It first verifies
the raw runtime lock SHA-256 equals the reviewed toolchain
`runtimeCatalogRevision`, selects only the exact NeoForge Java component/major,
and verifies the canonical resolution digest equals:

```text
2b3bf58669d502aae932fc492019c1d33bc3439dce1d7951110802d55a8b427d
```

It permits only digest-addressed `piston-data.mojang.com` object URLs, rejects
redirects and encoded responses, verifies exact Content-Length, byte count,
and SHA-1 for every file, constrains links to the root, writes deterministic
modes, and atomically renames a sibling partial directory only after success.
It then writes the exact canonical resolution as
`.xmcl-runtime-resolution.json` and verifies its digest again. A verified run
of the current locks materializes 136 files totaling 108,928,861 bytes. The
production registry independently repeats the manifest digest and complete
filesystem verification at startup.

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

At build time, artifacts reviewed under `libraries.minecraft.net` use the
same absolute Maven path on a code-owned mirror: `com/mojang` artifacts use
Sponge's public Maven proxy, while other artifacts use Maven Central. This
avoids regional Azure Front Door failures while preserving the catalog's exact
coordinate, byte size, and SHA-256 requirements; no job-supplied mirror or path
is accepted.

Root-owned artifacts in `/var/lib/xmcl-compiler/artifacts/<sha256>` are checked
before network download. Cached bytes must still match the reviewed catalog's
exact size and SHA-256, and the compiler service has read-only access to them.

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
| `POST /v1/compiler-jobs` | authenticate, validate, durably enqueue, then return an exact 202 acknowledgement |

The HTTP response returns `202 {"status":"accepted","deploymentId":"..."}`
only after the exact envelope has been atomically persisted and synced. A
bounded background worker claims persisted jobs, resumes pending/running work
after service restart, and archives a job only after a terminal callback is
durable. Failed or uncertain attempts remain queued with bounded exponential
backoff. Before each attempt, the worker requests fresh exact GET/PUT grants;
the short-lived grants in the submission envelope are never reused after
acceptance.

Before the PUT, the worker posts its reviewed content digest and descriptor to
the code-owned `upload-prepared` deployment route.
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
or sandbox call, the worker posts the exact compiler request and deployment
identity to the authenticated grants route, then reuses `verifyGrantSet` to
bind account, service, deployment, manifest SHA, input archive key, output
key, GET/PUT methods, HTTPS grant URLs, unexpired grant timestamps, no input
headers, and exactly `If-None-Match: *` on the output grant, optionally with
Azure's exact `x-ms-blob-type: BlockBlob`. It then reuses the
bundle and reviewed-toolchain validation described above. Neither the envelope
nor its grants can select callbacks, commands, Java paths, JVM arguments,
Docker options, or sandbox settings.

Object-grant GET/PUT operations are bounded to 30 seconds and stream the input
with an exact byte cap; callbacks are bounded to 10 seconds. Timeouts are
reported through the fixed `compiler_failed` callback path only before upload
preparation begins. Published-callback delivery is instead reported as
`published_callback_uncertain`; upload uncertainty is reported as
`upload_reconciliation_uncertain` so it can be reconciled safely.

Composition accepts only an exact HTTPS control-plane origin with no path,
query, credentials, or fragment. For the already validated `deploymentId`,
compiler-owned code derives exactly these paths:

```text
/v1/internal/shared-runtime-compiler/deployments/{deploymentId}/upload-prepared
/v1/internal/shared-runtime-compiler/deployments/{deploymentId}/grants
/v1/internal/shared-runtime-compiler/deployments/{deploymentId}/published
/v1/internal/shared-runtime-compiler/deployments/{deploymentId}/failed
```

The HMAC signature covers the exact derived path and body. Upload preparation,
publication, and failure cannot share or substitute routes. Neither callback
URLs nor templates are accepted from a job or its grants. A completion includes
the validated content and runtime descriptor; a failure includes only the
fixed failure code. Callback receivers must authenticate and deduplicate the
same `compilerRequestId`.

## Service identity and replay protection

Every queue delivery and callback uses the `ServiceIdentity` adapter boundary:

```text
verifyIncoming({ method, target, headers, body, transport })
signOutgoing({ method, target, body })
```

Adapters must provide signed service identity plus timestamp and nonce replay
checks. Production composition uses `HmacServiceIdentity` with the API's
matching key ID and an exact shared secret of at least 32 bytes. It signs:

```text
METHOD \n PATH_AND_QUERY \n UNIX_MILLISECONDS \n NONCE \n SHA256(BODY)
```

in `Authorization: HMAC <key-id>:<base64url-signature>`, with
`X-Xmcl-Timestamp` and `X-Xmcl-Nonce`. It accepts timestamps within 60 seconds
and rejects reused nonces. Its in-memory nonce cache is **local test/single
process only** and cannot be marked durable. Production uses
`FilesystemReplayStore`, whose atomic create and fsync operations require a
persistent volume shared by every compiler replica. `HmacServiceIdentity`
becomes production-eligible only with a replay store explicitly marked durable.
Production composition also sets `requireDurableReplay: true`, so construction
fails rather than silently producing an unready identity if that guarantee is
absent. In-memory HMAC identities always expose `replayProtected: false`.
Each callback receiver must enforce the matching HMAC and replay policy.

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
6. one exact HTTPS control-plane origin and authenticated callback transport.

The adapter receives only the compiler-owned assembly plan, exact reviewed
artifact bytes, and exact JRE root token. It has one narrow
`assemble(request)` operation. This package does not spawn a shell, execute a
bundle file, select a JVM, invoke Docker, or load an adapter module from an
environment variable. Never make a job field into adapter configuration.

The production adapter runs the exact reviewed NeoForge installer, Mojang
server, Mojang mappings, installer libraries, and runtime libraries with
`--offline`. The catalog includes every one of these inputs by URL, size, and
SHA-256. The adapter deterministically removes only the installer's
`DOWNLOAD_MOJMAPS` processor after staging the exact catalog-pinned mappings;
this prevents the processor's hard-coded network request. Bubblewrap then
executes the derived installer in a new user/PID/network/IPC/UTS/cgroup
namespace with an empty base root. `prlimit` enforces address-space, file-size,
process, and CPU ceilings, and a wall-clock timeout is enforced by the parent.

Production output is intentionally smaller than the sandbox filesystem ceiling.
The installer may return at most 320 MiB, one file may be at most 256 MiB, and
the final installer-plus-modpack input may be at most 384 MiB. Input bundles
are capped at 400 MiB compressed and 384 MiB logical. Tar construction is
capped at 400 MiB and the raw-Zstandard archive at 401 MiB before allocation.
These limits leave room in the 3200-MiB service cgroup for the live file map,
tar/archive verification buffers, downloaded artifacts, Node, and native
overhead. Packaging reuses verified artifact, sandbox, and bundle byte arrays;
archive verification uses views rather than cloning every file.

## Runtime packaging

The original `Dockerfile` remains a fail-closed experimental HTTP-service
artifact. `Dockerfile.aca` is the finite Container Apps Job image. It
materializes the exact catalog-bound Java 21 runtime during the image build,
copies the verified resolution into the final image, removes write bits from
the application and JRE, and runs as UID/GID 10001. It never installs a generic
APT JRE.

There are deliberately no environment variables for catalog URLs, installer
commands, Java paths, Docker options, sandbox commands, callback URLs, or
identity secrets. Production composition reads these fixed paths only:

- `/opt/xmcl-compiler/app/toolchain-catalog.lock.json`;
- `/etc/xmcl-compiler/worker.json` (schema shown by `worker.example.json`);
- `/etc/xmcl-compiler/jre-registry.json` (schema and exact current digests shown
  by `jre-registry.example.json`);
- read-only JRE bind mounts below `/opt/xmcl/jres`;
- one systemd credential at
  `/run/credentials/xmcl-compiler.service/service-hmac-secret`;
- an ephemeral writable directory at `/run/xmcl-compiler/workspaces`; and
- an atomic shared persistent replay volume at
  `/var/lib/xmcl-compiler/replay`.

The Java 21 JRE root must be on a read-only mount and must contain
`.xmcl-runtime-resolution.json`, copied from the matching `resolutions` entry
of the exact runtime catalog. Startup hashes that canonical resolution against
the catalog JRE digest, then verifies every declared file, size, SHA-1,
executable bit, directory, and symlink without permitting a link to escape the
root. `/readyz` becomes 200 only after those checks, the replay store, key
permissions, and an actual Bubblewrap namespace probe succeed.

### Azure Container Apps Job

`deploy/aca-job.bicep` defines one manual, scale-to-zero **canary-only** job
resource. Its internal module hard-codes `probe`, has no environment variables
or secrets, and cannot process a deployment. The repository intentionally
contains no independently deployable enabled-job template: a parameter named
`approved`, a Bicep assertion, or a human-supplied digest is not proof that an
ACA execution succeeded.

The dormant one-shot entrypoint is packaged for a future protected promoter.
One execution handles one signed deployment delivery and exits; it does not
start the HTTP server. The intended execution shape has one replica, one
required completion, no platform retry, a 1200-second replica timeout, 2 vCPU,
and 4 GiB memory. A failed or uncertain compiler attempt exits nonzero and
remains in the durable queue; the control plane must reconcile it and start a
new execution with a fresh signature. ACA replica retry must remain zero
because retrying the same execution would replay the same inbound HMAC nonce.

The Bicep template references an existing managed environment and an existing
Azure Files environment storage definition. It creates no storage account,
network, role assignment, managed identity, or registry credential. The job
identity is explicitly `None`. The storage definition must present the share
as UID/GID 10001 with `0700` directories and `0600` files. The share backs
`/var/lib/xmcl-compiler`; `/run/xmcl-compiler` is a replica-scoped `EmptyDir`.
Replay, pending jobs, and terminal archives are persistent. Workspaces,
delivery material, and the copied HMAC credential disappear with the replica.
The artifact-cache subdirectory is mounted read-only into the inner worker.
Each request ID has a separate queue directory and a durable 25-minute
execution lease. The worker claims only the delivered request ID, so concurrent
manual executions cannot consume one another's jobs. A duplicate active
request fails closed; after a crash its lease must expire before a replacement
execution can recover the request. Recovery cannot reuse the expired delivery
file: the control plane must create a new outer envelope with fresh
`issuedAt`/`expiresAt`, fresh exact grants, and a fresh HMAC timestamp/nonce,
while preserving the inner `compilerRequestId`, deployment identity, job, and
therefore job fingerprint. After authenticating the fresh envelope, enqueue
replaces the stale persisted outer body and `runOne` claims only that stable
request ID. An expired or replayed signature is rejected before recovery.

The image defaults to `probe`, and `deploy/aca-job.bicep` hard-codes the probe
entrypoint. In this mode it processes no deployment. It requires:

- UID/GID 10001, zero effective outer-container capabilities, and
  `NoNewPrivs: 1`;
- an explicit unprivileged user namespace plus mount, PID, IPC, UTS, and
  network namespaces;
- a tmpfs Bubblewrap root, read-only `/usr` and JRE mounts, a writable
  ephemeral workspace, and a runnable exact Java 21 binary;
- a second nested Bubblewrap invocation, matching the production outer-worker
  plus inner-installer namespace shape; and
- atomic replay create/deduplication, file and directory `fsync`, queue claim,
  and terminal archive operations on the mounted persistent share.

Any failed check exits nonzero. Passing it does not unlock a template in this
repository. Never make the probe optional and never work around it with
privileged mode, `SYS_ADMIN`, a Docker socket, `seccomp=unconfined`, or by
removing Bubblewrap.

The dormant `job` mode in `scripts/aca_entrypoint.mjs` copies the protected
worker config, HMAC secret, and one signed delivery into the ephemeral volume
with mode `0400`. It then enters an outer Bubblewrap filesystem/PID/user
sandbox. That sandbox supplies the worker with a tmpfs root and only these
writable paths:

```text
/run/xmcl-compiler/workspaces
/var/lib/xmcl-compiler/replay
/var/lib/xmcl-compiler/jobs
/var/lib/xmcl-compiler/leases
```

The outer sandbox retains the ACA network namespace so trusted compiler code
can call the exact control-plane and reviewed artifact origins. The existing
inner `BubblewrapSandboxAdapter` creates a separate network namespace for the
untrusted installer. Its environment remains empty and no credential,
callback configuration, replay state, or Docker socket is mounted inside it.
The JRE baked into the OCI image is not assumed read-only merely because the
image is immutable: the outer `--ro-bind` creates the read-only mount that
`VerifiedReadOnlyJreRegistry` independently observes in
`/proc/self/mountinfo`.

The per-execution `XMCL_COMPILER_DELIVERY_B64` decodes to this exact wrapper:

```json
{
  "schemaVersion": 1,
  "method": "POST",
  "target": "/v1/compiler-jobs",
  "headers": {
    "authorization": "HMAC <key-id>:<signature>",
    "x-xmcl-nonce": "<fresh nonce>",
    "x-xmcl-timestamp": "<unix milliseconds>"
  },
  "bodyBase64": "<base64 of the existing schema-versioned compiler envelope>"
}
```

The ACA body is capped at 60 KiB (the HTTP service remains capped at 256 KiB)
and passes through the same HMAC,
timestamp, nonce, envelope, grant, catalog, download, immutable PUT, output
revalidation, and callback code as the HTTP boundary. The start caller must
submit the complete execution template override, preserving the immutable
image digest, resource settings, volume mounts, `job` argument, and the two
secret references while replacing only the empty delivery value. Microsoft
documents that a start override replaces the whole template and that a
principal able to start a job can access its configured secrets. Therefore the
launcher is a highly trusted control-plane principal. Grant it only the exact
job read/start/execution-read actions; do not expose that principal or a
generic Container Apps Contributor credential to tenant or node code.
The delivery wrapper is base64-encoded once more for the execution environment
override; the 60-KiB body and 90-KiB wrapper limits keep the final Linux
environment string below 128 KiB. ACA does not document a separate environment
value limit, so the real canary must also cover the largest expected envelope.

#### Evidence and open compatibility question

As reviewed on 2026-09-02:

| Level | What is established |
| --- | --- |
| Microsoft official documentation | [ACA Jobs](https://learn.microsoft.com/azure/container-apps/jobs) are finite tasks; manual executions can be started through ARM, accept a full-template override, use exit status, timeout, retry, parallelism, and completion settings, and can have no active replica between executions. The same page states that job starters gain access to configured secrets. |
| Microsoft official documentation | [ACA storage](https://learn.microsoft.com/azure/container-apps/storage-mounts) says the container filesystem is writable ephemeral storage, `EmptyDir` lasts for a replica, and Azure Files is persistent. This design does **not** call ACA's ordinary container root read-only. |
| Microsoft official schema | The stable [`Microsoft.App/jobs@2025-01-01`](https://learn.microsoft.com/azure/templates/microsoft.app/jobs) container shape exposes command, args, env, image, probes, resources, and volume mounts. It exposes no customer security context for capabilities, seccomp, AppArmor, privileged mode, user namespaces, or `readOnlyRootFilesystem`. Absence of that control surface does not prove which host syscalls ACA permits. |
| Bubblewrap official documentation | [Bubblewrap](https://github.com/containers/bubblewrap) requires a user namespace when non-root, always creates a mount namespace, uses a tmpfs root, and can create PID/network/IPC/UTS namespaces and read-only bind mounts. Its maintainers also state that the caller's arguments define the security policy. |
| Local container probe only | `npm run probe:bwrap:container` builds the exact ACA image and runs it non-root with read-only OCI root, all capabilities dropped, no-new-privileges, no Docker socket, no network, and only explicit tmpfs state/work paths. Passing proves that local runtime/profile only, not ACA. |
| Must be proved by real ACA canary | ACA host seccomp/AppArmor, unprivileged and nested user namespaces, namespace-local mount operations, outer no-new-privileges/capability state, exact workload-profile kernel behavior, and Azure Files atomic-create/`fsync` behavior. Microsoft documentation does not promise these Bubblewrap requirements. |

The local probe is reusable and intentionally uses Docker's standard seccomp
profile; it does not install a permissive profile. On a host where Docker is
unavailable or the standard profile rejects Bubblewrap, the result is
inconclusive for ACA but the deployment remains disabled. A passing local
probe is necessary evidence for the image, not sufficient evidence for ACA.
No passing local or ACA probe result is asserted by the repository itself.

#### Build, canary, enable, and rollback

Build from the repository root so the verified JRE materializer can read both
locks:

```text
docker build --file compiler/Dockerfile.aca \
  --tag xmcl-compiler-aca:local .
cd compiler
npm run probe:bwrap:container
```

The two lock files are forced to LF by the repository `.gitattributes` because
the reviewed `runtimeCatalogRevision` hashes the exact raw runtime lock bytes.
The build and standard validation command do not perform hidden normalization.

The deployment pipeline must publish by digest and pass only
the 64-character lowercase digest as `compilerImageDigest`; the Bicep template
constructs the fixed
`ghcr.io/voxelum/xmcl-shared-node-compiler@sha256:<digest>` reference. Deploy
`deploy/aca-job.bicep`, the
existing VNet-integrated managed-environment resource ID, exact workload
profile, and the existing durable storage name. The canary form contains no
compiler HMAC secret or worker configuration.

Start the probe without an execution override. Require a successful execution
and both canary JSON records. The inner production-sandbox record must contain
these `true` fields:
`networkNamespaceIsolated`, `pidNamespaceIsolated`, `readOnlyJre`,
`readOnlySystem`, `writableWorkspace`, `nestedBubblewrap`,
`noNewPrivileges`, `productionJreRegistry`, and
`productionSandboxAdapter`. The outer record must contain
`productionOuterSandbox`, `durableExecutionLease`, `durableReplayState`, and
`durableQueueState`.
Confirm the execution used the expected image digest and workload profile.
Re-run after every image, ACA environment/workload-profile, kernel, or storage
change.

There is currently no promotion command. Before adding one, implement a
protected CI workflow or trusted control-plane operation that uses an
Azure-federated deployment identity to query the real ACA execution and logs,
requires a successful canary execution, validates every exact probe field, and
binds the evidence to the immutable image digest, managed-environment resource
ID, storage definition, and workload profile. The same protected operation
must construct the enabled deployment itself; it must not emit a reusable
user-editable approval file or accept those bindings back as ordinary
parameters. Repository/environment protection and human approval are still
required, but neither is a cryptographic runtime attestation by itself.

After such a promoter exists, run a disposable signed deployment canary and
require an exact terminal callback and immutable object reconciliation before
routing production deployments. Supply the base64 of
`deploy/worker.aca.example.json` after replacing its invalid example
origin/key ID and a protected base64 HMAC secret only inside that protected
operation. Do not put either value in source control or CLI history. Network
controls belong to the existing
VNet/UDR/firewall: permit HTTPS only to the exact control-plane/object-grant
origins and reviewed catalog hosts, plus Azure Files transport required by the
mounted share. ACA Jobs have no ingress and this template enables none.

Until that promoter exists, keep the VM/systemd worker as production and use
the ACA resource only for canaries. A future rollback must first stop new ACA
starts or redeploy the canary-only template, let active executions reach their
terminal callback or timeout, and retain the replay/queue share before resuming
the Ubuntu systemd worker against the same idempotency contract. If ACA cannot
pass Bubblewrap, keep VM/systemd, or move to a dedicated AKS node pool where a
narrowly reviewed runtime seccomp/AppArmor and user-namespace configuration
can be controlled. Removing the inner network namespace or running the
compiler directly in a standard ACA container is not an accepted fallback.

### Ubuntu 24.04 staging deployment contract

The exact supported staging composition is the host systemd service in
`deploy/xmcl-compiler.service`, not Docker. It runs directly on Ubuntu 24.04 as
the dedicated numeric UID/GID 10001, listening only on host loopback. Install
reviewed/pinned Node 22 at `/usr/bin/node` plus Ubuntu's `bubblewrap`,
`util-linux`, and `ca-certificates` packages. The distro Bubblewrap AppArmor
policy and `kernel.unprivileged_userns_clone=1` must permit an unprivileged
user/network/PID/mount namespace; do not disable AppArmor globally.

The 2-vCPU/4-GiB shape supports one concurrent compiler job.
Use the limits in `worker.example.json`: a 3-GiB installer address-space
limit, fixed 128-MiB/1-GiB Java heap, 512-MiB metaspace, 256 processes,
256-MiB compressed-class space, 128-MiB code cache, 256-MiB direct-memory
ceiling, 512-KiB thread stacks, two visible JVM processors, 240 CPU-seconds,
300-second wall timeout, and 256-MiB per-file limit. The unit enforces two CPUs,
3200 MiB memory, no swap, and 512 tasks. Do not increase `maxConcurrentJobs`.

Only these environment variables are read:

```text
NODE_ENV=production
HOST=127.0.0.1
PORT=8080
```

Prepare these exact host paths:

| Path | Access/ownership |
| --- | --- |
| `/opt/xmcl-compiler/app` | root-owned, immutable application tree |
| `/etc/xmcl-compiler/worker.json` | root-owned mode 0444 |
| `/etc/xmcl-compiler/jre-registry.json` | root-owned mode 0444 |
| `/etc/xmcl-compiler/service-hmac-secret` | root-owned mode 0400; exact API-matching bytes, at least 32 bytes |
| `/opt/xmcl/jres` | exact JRE roots on a read-only `nosuid,nodev` mount |
| `/run/xmcl-compiler/workspaces` | systemd-created ephemeral mode 0700, UID/GID 10001 |
| `/var/lib/xmcl-compiler/replay` | systemd-created persistent mode 0700, UID/GID 10001 |
| `/var/lib/xmcl-compiler/jobs` | systemd-created persistent queue/archive mode 0700, UID/GID 10001 |

Install the unit at `/etc/systemd/system/xmcl-compiler.service`, run
`systemd-analyze verify` on it, then `systemctl daemon-reload` and
`systemctl enable --now xmcl-compiler`. `LoadCredential` exposes the secret
only inside the service credential directory. The source remains mode 0400;
systemd may materialize the root-owned credential as mode 0440, which is the
only group-readable secret mode the worker accepts. `ProtectSystem=strict`, empty
capability sets, private devices/tmp, read-only code/config/JRE paths, and
explicit writable workspace/replay/job paths keep the host composition
fail-closed. The unit deliberately does not apply a broad syscall filter:
Bubblewrap requires namespace-local mount syscalls, while its own
`--unshare-all` disables installer networking. `/readyz` must return 200 before
nginx receives traffic.

Restrict inbound host traffic to SSH, TCP 80, and TCP 443. Restrict compiler
egress to the exact HTTPS callback/object-grant destinations and the artifact
hosts in `toolchain-catalog.lock.json`. Installer assembly itself has no
network namespace.

nginx should terminate TLS and be the only public compiler listener:

```nginx
server {
    listen 80;
    server_name 130.94.89.36.sslip.io;

    location / {
        return 404;
    }

    location = /v1/compiler-jobs {
        client_max_body_size 256k;
        proxy_http_version 1.1;
        proxy_connect_timeout 10s;
        proxy_read_timeout 10m;
        proxy_send_timeout 30s;
        proxy_pass http://127.0.0.1:8080;
    }
}
```

Obtain and renew the certificate with certbot's nginx integration for
`130.94.89.36.sslip.io`, then retain the same exact-location proxy in the
generated TLS server. Do not proxy `/healthz` or `/readyz` publicly. Deployment
must gate traffic on a local `GET http://127.0.0.1:8080/readyz` returning 200,
not merely the liveness health check.

`.github/workflows/publish-compiler-image.yml` at the repository root validates
tests and publishes the experimental image only as a unique
`sha-<full-git-sha>` GHCR tag, and attaches BuildKit provenance and SBOM
attestations. The workflow never publishes `latest`. Do not deploy that image
for staging until reviewed seccomp/AppArmor profiles make its real Bubblewrap
probe pass; image publication does not supersede the host-systemd contract.
