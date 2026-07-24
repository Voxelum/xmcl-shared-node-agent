# Shared Workspace Object Storage

## Provisioning

Create one dedicated private Vultr Object Storage bucket per environment, such
as `xmcl-shared-workspaces-prod`. Do not enable static website hosting or any
public bucket/object ACL. Configure the agent with the regional HTTPS endpoint,
the bucket name, and a distinct access key for each node or node group.

The canonical layout is:

```text
shared-hosting/<accountId>/<serviceId>/
  revisions/<revision>/files/<relative workspace path>
  revisions/<revision>/manifest.json
```

The agent uploads all `files/` objects before it writes `manifest.json`. The
manifest is the only completion marker; revisions with no manifest are never
restorable.

## Credential policy and rotation

Use a dedicated shared-workspace credential, not a root or backup credential.
Grant only `ListBucket`, `GetObject`, `PutObject`, `DeleteObject`, and multipart
upload operations for the dedicated bucket and the `shared-hosting/*` prefix.
Do not grant bucket-policy, ACL, encryption-key, or account administration
permissions. If the provider cannot enforce prefix restrictions on an access
key, isolate this workload in its own bucket and restrict the credential to
that bucket.

Rotate node credentials with overlap: issue a new key, update the systemd
environment file with mode `0600`, restart one drained node to validate it,
roll the remaining nodes, then revoke the old key. Never place access keys in
images, Git, logs, or command-line arguments.

Require TLS for the S3 endpoint and enable Vultr provider encryption at rest.
The agent always uses an HTTPS MinIO client.

## Retention and cleanup

Keep the revision referenced by the scheduler indefinitely. The control-plane
retention job may expire approved historical *complete* revisions only after
checking that no service references them. It must never delete a current
`manifest.json` or its `files/` objects.

Configure the provider to abort incomplete multipart uploads after one day.
Run `Manager.CleanupIncomplete` as a maintenance operation: it deletes only
revisions older than 24 hours that have no `manifest.json`, and explicitly
skips the current revision. This cleanup is safe because a manifest is
published only after every data object is durable.

## Metrics and billing handoff

The agent exposes loopback-only Prometheus text metrics on
`XMCL_METRICS_ADDR` (default `127.0.0.1:9464`):

- `xmcl_shared_workspace_logical_bytes` is the current canonical manifest size
  and is the value to send to billing/operations.
- `xmcl_shared_workspace_object_bytes` is the retained physical object total
  after a refresh.
- Restore download bytes, sync upload bytes, restore failures, and sync
  failures are emitted as counters.

The agent does not charge users or delete data based on plan quotas. Billing
uses the reported logical canonical bytes against the 32GiB, 48GiB, and 64GiB
persistent-data quotas until an overage policy exists.
