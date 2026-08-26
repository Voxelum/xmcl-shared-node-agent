# Shared Workspace Object Storage v2

Use one private Azure Blob Storage account and container per environment.
Disable anonymous blob access, shared-key authorization where workload identity
is available, and static website hosting. Only the control plane may mint
short-lived user-delegation SAS grants; compute nodes receive no storage
credentials.

## Canonical layout

```text
shared-hosting/<accountId>/<serviceId>/
  content/<sha256>.tar.zst
  revisions/<revision>/world/<shard-id>.tar.zst
  revisions/<revision>/config.tar.zst
  revisions/<revision>/manifest.json
```

`manifest.json` is schema version 2 and is written last. It records the
service/assignment/revision, logical size, immutable content descriptor,
optional config descriptor, ordered world shard descriptors, every safe local
path mapping, and a descriptor aggregate SHA-256. It never contains credentials,
grants, or game contents. Published manifests and immutable blobs are never
overwritten. `manifestHash` equals the ordered descriptor aggregate; the
separate assigned manifest SHA-256 covers the serialized manifest object. Only
schema version 2 is accepted. Legacy file-per-object layouts are not restored:
resync them through a v2 stop/sync command.

The agent classifies `world/`, `world_nether/`, and `world_the_end/` as world
data, grouped at directory boundaries into roughly 192 MiB compressed-target
shards. The implementation uses a conservative 192 MiB source-size budget, so
incompressible data remains near the compressed target while highly-compressible
data remains smaller; it never splits a file. `config/` and `defaultconfigs/`
are the config layer. `mods/`,
`kubejs/`, `scripts/`, `resourcepacks/`, bootstrap files, and all other files
default to the conservative immutable content layer. This avoids silently
omitting user data. Archives are deterministic streaming tar+zstd files; an
unchanged content tree has the same digest and is reused. Each compressed blob
is capped at 4 GiB, so v2 uses only one signed immutable PUT per object; larger
archives are rejected rather than falling back to general credentials or
unbounded multipart operations.

## Access model

Configure the agent only with the exact Azure Blob account endpoint and
container. Do **not** set `AZURE_STORAGE_ACCOUNT`, `AZURE_STORAGE_KEY`,
`AZURE_STORAGE_CONNECTION_STRING`, or `AZURE_CLIENT_SECRET`.

```text
XMCL_AZURE_BLOB_ENDPOINT=https://<account>.blob.core.windows.net
XMCL_AZURE_BLOB_CONTAINER=<private-container>
```

Place the storage account in the selected Camp data region and use an explicit
cross-region replication policy before enabling another compute region.

For the current authenticated command and lease only, the control plane issues
10-minute (maximum 15-minute) pre-signed URLs:

- restore: GET for its manifest, then GET only for descriptors recorded in it;
- sync: immutable PUT for validated blobs for `revision + 1`;
- publish: immutable PUT for that revision's `manifest.json`.

PUT URLs require `If-None-Match: *`, `x-ms-blob-type: BlockBlob`, and read
permission for the same exact object. If Azure reports that the immutable blob
already exists, the agent downloads that object through the same short-lived
SAS and verifies its length and SHA-256 before reuse. A mismatch fails the sync;
an arbitrary `409 BlobAlreadyExists` is never success-shaped. Agents neither
list nor delete objects; the control plane does not proxy workspace bytes.
Grant the control-plane identity only the Azure Blob Data Contributor actions
needed for this isolated container and prefer user-delegation SAS over
account-key SAS. Do not grant node identities an Azure role, key, or identity.

## Required staging validation

Do not declare production readiness until this sequence succeeds against a real
private Azure Blob container and VM:

```text
VM enroll -> restore revision -> start -> stop -> upload blobs ->
publish manifest -> report sync -> slot release -> restore on another node
```

Verify the second node restores exact hashes, that an unchanged content blob is
not uploaded again, that a changed world shard does not rewrite immutable
content, and that a rejected or expired URL cannot access another service.
