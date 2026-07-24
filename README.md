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
XMCL_VULTR_OBJECT_STORAGE_ENDPOINT
XMCL_VULTR_OBJECT_STORAGE_REGION
XMCL_VULTR_OBJECT_STORAGE_BUCKET
XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY
XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY
XMCL_WORKSPACE_ROOT
XMCL_STATE_ROOT
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
Provision the root-owned `/usr/local/libexec/xmcl-quota-helper` with mode
`4750`, group `xmcl-agent`, and a root-owned mode-`0600`
`/etc/xmcl-shared-node-agent/quota-helper.json` based on
`deploy/systemd/quota-helper.json.example`. The agent can request a quota only
for a direct workspace child; it fails closed when the helper or hard quota is
unavailable.

## Workspace storage operations

See [`deploy/vultr/workspace-storage.md`](deploy/vultr/workspace-storage.md)
for private-bucket provisioning, least-privilege credential rotation,
incomplete-upload cleanup, historical retention, and billing metrics. The
metrics listener must remain bound to loopback or another private monitoring
network.

## Control-plane transport

The binary uses an outbound HTTPS long-poll client. Bootstrap registration
exchanges `XMCL_CONTROL_PLANE_CREDENTIAL` for an atomically persisted,
mode-`0600` short-lived node credential. Every request has the required
timestamp, nonce, body hash, and HMAC signature; no control-plane port is
opened on a compute node. It sends a heartbeat immediately after registration
and on a fixed cadence, retrying transient transport failures with bounded
backoff.

## Install

```sh
go build -o xmcl-shared-node-agent ./cmd/xmcl-shared-node-agent
install -m 0644 deploy/systemd/xmcl-shared-node-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now xmcl-shared-node-agent
```
