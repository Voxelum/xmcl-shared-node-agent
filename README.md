# XMCL Shared Node Agent

The shared-node agent is the privileged, Go-based execution component for one
Vultr shared-hosting compute node. It restores verified Minecraft workspaces
through short-lived command-scoped S3 SigV4 URL grants, runs a hardened Docker
container, and syncs immutable revisions before acknowledging a stop.

## Safety properties

- Commands and active assignments are durably recorded in `commands.json`.
  Replayed command IDs return their original result without starting another
  container.
- Per-service file locks prevent concurrent agents from operating on the same
  local workspace.
- Restore accepts only granted HTTPS URLs for the configured Vultr storage host.
  It rejects redirects, foreign URLs, traversal, duplicate archive entries,
  symlinks, decompression bombs, invalid path mappings, and hash/size failures
  before replacing the active workspace.
- Sync creates deterministic streaming `.tar.zst` mutation layers, hashes while
  streaming, obtains PUT URLs only for validated exact objects, and publishes
  `manifest.json` last with an immutable-write precondition.
- The agent has no S3 access key, secret key, List, Delete, bucket-stat, or
  arbitrary-key operation. It never stores URL grants.
- Containers run as UID/GID `1000`, have a read-only root filesystem, one
  writable `/data` bind mount, no capabilities, no-new-privileges, a PID
  limit, a memory hard limit, disabled swap, and CPU controls.
- `XMCL_CONTAINER_IMAGE` must be an immutable
  `ghcr.io/voxelum/xmcl-shared-minecraft-runtime@sha256:...` digest. Mutable
  images, vanilla fallbacks, user commands, user environment variables, and
  user-selected Docker options are rejected.
- Before creating a modded container the agent validates the compiler-owned
  `.xmcl/runtime.json`, its selected content hash, Java 8/17/21 choice, loader,
  and fixed generated launcher. It never downloads loader/server/mod artifacts.

## Configuration

Create `/etc/xmcl-shared-node-agent.env` with mode `0600`, owned by
`xmcl-agent`. The required variables are:

```text
XMCL_SHARED_NODE_ID
XMCL_SHARED_NODE_REGION=sgp
XMCL_CONTROL_PLANE_URL
XMCL_CONTROL_PLANE_CREDENTIAL
XMCL_SHARED_NODE_INGRESS_HOST
XMCL_VULTR_OBJECT_STORAGE_ENDPOINT=https://sgp1.vultrobjects.com
XMCL_VULTR_OBJECT_STORAGE_REGION=sgp
XMCL_VULTR_OBJECT_STORAGE_BUCKET
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

`XMCL_CONTAINER_IMAGE` is the generic multi-JRE image only. It contains the
trusted Java 8, 17, and 21 assets and its health check probes local port 25565.
The image writes `eula=true` only when the control plane sends the
server-side policy-approved `eulaAccepted` command field; missing approval is a
launch failure. See [`deploy/runtime/Dockerfile`](deploy/runtime/Dockerfile).
The release pipeline must materialize and hash-verify the JRE assets described
by `deploy/runtime/runtime-assets.example.json` before building and publishing
the digest.

`XMCL_VULTR_OBJECT_STORAGE_ENDPOINT` must be the Vultr HTTPS origin and
`XMCL_VULTR_OBJECT_STORAGE_BUCKET` is used only to verify a grant's path-style
bucket/key association. The control plane gives the agent no storage
credentials. For each active v2 command lease it signs a control-plane request
for one of `restore`, `sync`, or `publish`; the response contains only exact
short-lived GET or PUT URLs. The agent rejects
`XMCL_VULTR_OBJECT_STORAGE_ACCESS_KEY`,
`XMCL_VULTR_OBJECT_STORAGE_SECRET_KEY`, and
`XMCL_VULTR_OBJECT_STORAGE_CREDENTIAL_URL` if configured, including in
development.

`XMCL_SHARED_NODE_INGRESS_HOST` is the provisioner-owned, reachable public DNS
name or IPv4 address used for service connections. It must be supplied in the
root-owned node environment by the trusted provisioning workflow (for example,
from the assigned Vultr instance address). The agent does not derive it from a
public-IP discovery service, DNS guess, or control-plane URL; startup fails if
it is absent or malformed.

`XMCL_SHARED_NODE_REGION` is written by the trusted cloud-init configuration
from the control plane's `VULTR_SHARED_NODE_REGION_ID`; operators do not set it
manually. The current shared pool is Singapore (`sgp`). A future multi-region
product needs explicit region selection and a cross-region data policy; it is
out of scope.

`XMCL_QUOTA_MOUNT_PATH` must be an XFS filesystem mounted with project quotas.
Provision the root-owned `/usr/local/libexec/xmcl-quota-helper` with mode
`4750`, group `xmcl-agent`, and a root-owned mode-`0600`
`/etc/xmcl-shared-node-agent/quota-helper.json` based on
`deploy/systemd/quota-helper.json.example`. The agent can request a quota only
for a direct workspace child; it fails closed when the helper or hard quota is
unavailable.

## Workspace storage operations

See [`deploy/vultr/workspace-storage.md`](deploy/vultr/workspace-storage.md)
for the v2 layout, signer-only bucket policy, retention, and the required
staging validation sequence. The metrics listener must remain bound to loopback
or another private monitoring network.

## Control-plane transport

The binary uses an outbound HTTPS long-poll client. Bootstrap registration
exchanges `XMCL_CONTROL_PLANE_CREDENTIAL` for an atomically persisted,
mode-`0600` short-lived node credential. On restart it reuses that credential
instead of enrolling again. Only a control-plane `401` or `403` invalidates it;
the agent removes the rejected credential and enrolls again. Every request has the required
timestamp, nonce, body hash, and HMAC signature; no control-plane port is
opened on a compute node. It sends a heartbeat immediately after registration
and on a fixed cadence, retrying transient transport failures with bounded
backoff.

The existing heartbeat remains the signed v1 JSON contract containing ready/draining state,
remaining allocatable memory/CPU/workspace capacity derived from the managed
containers, agent version, and the configured ingress host. Empty heartbeat
bodies are not supported.

Restore commands must include `connection.host` and numeric
`connection.hostPort`. The control plane reserves that host port durably before
dispatch; the agent rejects commands without it and never derives a port from
the assignment ID. After a healthy start it reports only
`endpoint: {host, port}` to confirm the assigned public endpoint.

## Install

```sh
go build -o xmcl-shared-node-agent ./cmd/xmcl-shared-node-agent
install -m 0644 deploy/systemd/xmcl-shared-node-agent.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now xmcl-shared-node-agent
```
