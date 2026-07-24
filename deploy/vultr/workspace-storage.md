# Shared Workspace Object Storage v2

Use one private Vultr Object Storage bucket per environment. Disable public
ACLs and static website hosting. Only the Worker/control plane owns the bucket
access key and secret; compute nodes receive no storage credentials.

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

Configure the agent only with the Vultr HTTPS endpoint, region, and bucket.
Do **not** set `XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY`,
`XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY`, or
`XMCL_VULTR_OBJECT_STORAGE_CREDENTIAL_URL`.

The active shared-node pool is Singapore because the current authenticated
Vultr account has no Taipei Compute or Object Storage location. Use:

```text
VULTR_SHARED_NODE_REGION_ID=sgp
XMCL_VULTR_OBJECT_STORAGE_ENDPOINT=https://sgp1.vultrobjects.com
XMCL_VULTR_OBJECT_STORAGE_REGION=sgp
XMCL_SHARED_NODE_REGION=sgp  # cloud-init owned; operators do not set it manually
```

A future multi-region product needs explicit region selection and a
cross-region data policy; it is out of scope. Do not declare production
readiness until the Singapore staging validation below succeeds.

For the current authenticated command and lease only, the control plane issues
10-minute (maximum 15-minute) pre-signed URLs:

- restore: GET for its manifest, then GET only for descriptors recorded in it;
- sync: PUT only for validated blobs for `revision + 1`;
- publish: PUT only for that revision's `manifest.json`.

PUT URLs require `If-None-Match: *`. Agents neither list nor delete objects;
the Worker does not proxy workspace bytes. Bucket policy should permit the
Worker signing credential only the S3 object actions needed for this isolated
bucket. Do not grant node identities any S3 policy, key, or role.

## Required staging validation

Do not declare production readiness until this sequence succeeds against a real
private Vultr bucket and VM:

```text
VM enroll -> restore revision -> start -> stop -> upload blobs ->
publish manifest -> report sync -> slot release -> restore on another node
```

Verify the second node restores exact hashes, that an unchanged content blob is
not uploaded again, that a changed world shard does not rewrite immutable
content, and that a rejected or expired URL cannot access another service.
