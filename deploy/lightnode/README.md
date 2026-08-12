# LightNode bootstrap

Use the plain **Ubuntu 24.04 LTS amd64** image returned by LightNode's
documented image-list API. Persist its exact `imageResourceUUID` in the
provider offer. Do not use the Docker, Minecraft Panel, aaPanel, CloudPanel, or
other application images: their preinstalled packages and remote-management
surfaces are outside the reviewed node baseline.

LightNode does not currently provide the cloud-init path used by the Vultr
adapter. A private runner therefore uses `runner.sh` in two phases. First,
`probe` performs only an SSH handshake and prints the ED25519 host key and
SHA-256 fingerprint; it sends no secret or command:

```sh
XMCL_LIGHTNODE_HOST=203.0.113.10 XMCL_LIGHTNODE_PORT=22 \
  deploy/lightnode/runner.sh probe
```

Persist the exact known-hosts line and fingerprint against the already
reconciled LightNode instance. A later durable job uses `apply` with that
pinned identity:

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

The uploaded bootstrap environment pins one
`XMCL_RELEASE_MANIFEST_URL` and its exact
`XMCL_RELEASE_MANIFEST_SHA256`. The manifest supplies the only accepted agent
hash, quota-helper hash, runtime image digest, and raw runtime-catalog hash.
Bootstrap derives artifact URLs from the manifest's immutable GitHub release
tag and rejects an image whose embedded catalog label differs.

The runner must pin and verify the expected LightNode instance id, public IPv4,
image id, region, zone, package resources, and the persisted SSH host key before
the `apply` job is eligible. TCP 22 must be limited to the runner's stable
egress address. A
successful script exit means the one-time enrollment was consumed, the agent
persisted its rotating credential, and the control plane accepted its first
heartbeat. The runner must then remove TCP 22 from the provider firewall and
report bootstrap completion. A timeout or disconnect remains quarantined and
must be reconciled against the same instance and node id.

The bootstrap creates a fixed-size XFS loopback filesystem with project quotas
on the system disk. This avoids guessing a LightNode block-device name or
formatting an undocumented device. The selected package must have enough
system-disk capacity for the configured workspace volume plus at least 8 GiB
of host headroom; otherwise bootstrap fails before allocating the file. If a
future documented data-disk identity/lifecycle is accepted, replace this with a
stable by-id mount rather than device-order probing.

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
