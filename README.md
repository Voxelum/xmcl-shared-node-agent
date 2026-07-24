# XMCL Shared Node Agent

The shared-node agent is the privileged, Go-based execution component for one
Vultr shared-hosting compute node. It restores verified Minecraft workspaces
from private S3-compatible object storage, runs a hardened Docker container,
and syncs immutable revisions before acknowledging a stop.

## Safety properties

- Commands and active assignments are durably recorded in `commands.json`.
  Replayed command IDs return their original result without starting another
  container.
- Per-service file locks prevent concurrent agents from operating on the same
  local workspace.
- Restore rejects traversal paths, mismatched service/assignment/revision,
  aggregate manifest mismatches, and per-file hash mismatches before Docker is
  called.
- Sync uploads every object before publishing `manifest.json`; a failed upload
  leaves the local workspace and assignment intact.
- Revisions use `revisions/<revision>/files/...`; only a matching
  `manifest.json` makes one restorable. Upload retries are idempotent, and
  stale revisions without a manifest can be safely cleaned after 24 hours.
- Containers run as UID/GID `1000`, have a read-only root filesystem, one
  writable `/data` bind mount, no capabilities, no-new-privileges, a PID
  limit, a memory hard limit, disabled swap, and CPU controls.

## Configuration

Create `/etc/xmcl-shared-node-agent.env` with mode `0600`, owned by
`xmcl-agent`. The required variables are:

```text
XMCL_SHARED_NODE_ID
XMCL_CONTROL_PLANE_URL
XMCL_CONTROL_PLANE_CREDENTIAL
XMCL_S3_ENDPOINT
XMCL_S3_REGION
XMCL_S3_BUCKET
XMCL_S3_ACCESS_KEY
XMCL_S3_SECRET_KEY
XMCL_WORKSPACE_ROOT=/var/lib/xmcl-shared/workspaces
XMCL_CONTAINER_IMAGE
XMCL_RCON_STOP_TIMEOUT_SECONDS=60
XMCL_TOTAL_MEMORY_MIB
XMCL_TOTAL_SHARED_CPU
XMCL_TOTAL_WORKSPACE_GIB
XMCL_QUOTA_MOUNT_PATH
XMCL_QUOTA_PROJECT_BASE
XMCL_METRICS_ADDR=127.0.0.1:9464
```

`XMCL_QUOTA_MOUNT_PATH` must be an XFS filesystem mounted with project quotas.
The agent validates `xfs_quota` at startup and fails closed when hard workspace
quotas are unavailable.

## Workspace storage operations

See [`deploy/vultr/workspace-storage.md`](deploy/vultr/workspace-storage.md)
for private-bucket provisioning, least-privilege credential rotation,
incomplete-upload cleanup, historical retention, and billing metrics. The
metrics listener must remain bound to loopback or another private monitoring
network.

## Control-plane transport

The API-side contract currently has no authenticated node-agent endpoint. The
execution core exposes `controlplane.CommandSource` and
`controlplane.Reporter`; tests use `MemoryGateway`. The binary intentionally
refuses to start command processing with an unconfigured transport rather than
inventing an unauthenticated HTTP API. A production adapter must provide mTLS,
short-lived credentials, timestamp/nonce/body-hash replay protection, and
strict node ownership checks.

## Install

```sh
go build -o xmcl-shared-node-agent ./cmd/xmcl-shared-node-agent
install -m 0644 deploy/systemd/xmcl-shared-node-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now xmcl-shared-node-agent
```
