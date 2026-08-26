# LightNode bootstrap

Use the plain **Ubuntu 24.04 LTS amd64** image returned by LightNode's
documented image-list API. Persist its exact `imageResourceUUID` in the provider
offer. Do not use the Docker, Minecraft Panel, aaPanel, CloudPanel, or other
application images: their preinstalled packages and remote-management surfaces
are outside the reviewed node baseline.

LightNode does not currently provide the cloud-init path used by the Vultr
adapter. A private runner therefore uses `runner.sh` in two phases. First,
`probe` performs only an SSH handshake and prints the ED25519 host key and
SHA-256 fingerprint; it sends no secret or command:

```sh
XMCL_LIGHTNODE_HOST=203.0.113.10 XMCL_LIGHTNODE_PORT=22 \
  deploy/lightnode/runner.sh probe
```

Persist the exact known-hosts line and fingerprint against the already
reconciled LightNode instance. A later durable job uses `apply` with that pinned
identity:

```sh
export XMCL_LIGHTNODE_HOST=203.0.113.10
export XMCL_LIGHTNODE_PORT=22
export XMCL_LIGHTNODE_USER=ubuntu
export XMCL_LIGHTNODE_PRIVATE_KEY=/run/secrets/lightnode-bootstrap-key
export XMCL_LIGHTNODE_KNOWN_HOSTS=/var/lib/xmcl-runner/known-hosts/node-id
export XMCL_LIGHTNODE_HOST_KEY_SHA256=SHA256:...
export XMCL_LIGHTNODE_BOOTSTRAP_ENV=/run/jobs/node-id/bootstrap.env
export XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT=deploy/lightnode/bootstrap.sh
export XMCL_LIGHTNODE_AGENT_SERVICE=deploy/systemd/xmcl-shared-node-agent.service
deploy/lightnode/runner.sh apply
```

`xmcl-lightnode-bootstrap-runner` exposes this flow to the control plane
without auto-approving trust on first use. `POST /v1/bootstrap-jobs` persists
the exact request and performs only `probe`; it returns
`awaiting_host_key_approval`. An operator reads the fingerprint from
`POST /v1/bootstrap-jobs/<jobId>/status`, compares its node, instance, address,
instance name, package, region, zone, image, firewall, and SSH key fields with
the LightNode control plane, and approves that exact fingerprint through
`POST /v1/bootstrap-jobs/<jobId>/approve` using the separate approval secret.
The next control-plane retry performs `apply`. Changed payloads for an existing
job are rejected, and completed jobs return success without another SSH
connection. Deploy it with
`deploy/systemd/xmcl-lightnode-bootstrap-runner.service` and the settings
documented in `runner.env.example`; terminate public TLS in front of its
loopback listener. `nginx-runner.conf.example` exposes only the exact
control-plane bootstrap endpoint publicly. It restricts status and approval to
loopback, so operators must invoke them on the runner host or through an
authenticated SSH/VPN tunnel. Replace the example hostname and certificate
paths before enabling the site.

The uploaded bootstrap environment pins one `XMCL_RELEASE_MANIFEST_URL` and its
exact `XMCL_RELEASE_MANIFEST_SHA256`. The manifest supplies the only accepted
agent hash, quota-helper hash, runtime image digest, and raw runtime-catalog
hash. Bootstrap derives artifact URLs from the manifest's immutable GitHub
release tag and rejects an image whose embedded catalog label differs.

The control plane validates the provider response and sends the expected
LightNode instance id, public IPv4, name, image, region, zone, package,
firewall, and SSH key identifiers in the immutable runner job. The operator
compares those persisted fields with LightNode together with the probed host
key before approval makes `apply` eligible. TCP 22 must be limited to the
runner's stable egress address. A successful script exit means the one-time
enrollment was consumed, the agent persisted its rotating credential, and the
control plane accepted its first heartbeat. Deployment automation or an
operator must then remove TCP 22 from the provider firewall. A timeout or
disconnect remains quarantined and must be reconciled against the same
instance and node id.

The bootstrap creates a fixed-size XFS loopback filesystem with project quotas
on the system disk. This avoids guessing a LightNode block-device name or
formatting an undocumented device. The selected package must have enough
system-disk capacity for the configured workspace volume plus at least 8 GiB of
host headroom; otherwise bootstrap fails before allocating the file. If a future
documented data-disk identity/lifecycle is accepted, replace this with a stable
by-id mount rather than device-order probing.

The script is intentionally fail-closed:

- only Ubuntu 24.04 amd64 is accepted;
- SSH apply rejects unknown or changed host keys and password authentication;
- one SHA-256-pinned release manifest binds all executable artifacts;
- Docker is installed at an exact apt version and held;
- both Go binaries are fetched over HTTPS and SHA-256 verified;
- the runtime image must use its immutable GHCR digest;
- the enrollment token exists only in root-mode files and is erased after the
  first confirmed heartbeat;
- SSH is disabled locally only after readiness; provider firewall cleanup
  remains the runner's responsibility.

## Local mock acceptance

The local acceptance profile treats WSL2 Ubuntu 24.04 as a disposable LightNode
VM while preserving the production runtime boundaries. It uses the real Docker
runtime, a loopback XFS project-quota mount, the privileged quota helper,
compiler-shaped immutable content, and in-memory exact object grants. Only the
cloud provider, control plane, and object store are mocked.

From Windows PowerShell:

```powershell
wsl.exe -d Ubuntu -u root -- bash -lc `
  'cd /mnt/c/Users/ci010-4090/Workspace/xmcl-shared-node-agent && bash deploy/lightnode/mock-acceptance.sh'
```

The default profile builds a local Java 21 test image from the WSL runtime,
without contacting a container registry. It uses the current Go runtime,
reviewed catalog identity, production entrypoint, and health contract; it does
not claim to validate every JRE bundled by the published image. Set
`XMCL_MOCK_LIGHTNODE_BASE_IMAGE` to the published immutable runtime digest to
replace its runtime binary and run the same lifecycle against the complete
image.

The first run creates a sparse 4 GiB XFS image at
`/var/lib/xmcl-mock-lightnode/workspace.xfs`. Subsequent runs reuse it. The test
completes restore, quota application, container health, start reporting,
graceful stop, immutable workspace sync, manifest publication, and local
workspace release. It never contacts LightNode or deployed XMCL APIs.
