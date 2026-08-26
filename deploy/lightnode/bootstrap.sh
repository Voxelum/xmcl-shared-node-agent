#!/usr/bin/env bash
set -euo pipefail

readonly config_file="${1:-/run/xmcl-lightnode-bootstrap.env}"
readonly service_file="${2:-/run/xmcl-shared-node-agent.service}"
readonly state_dir=/var/lib/xmcl-bootstrap
readonly data_root=/var/lib/xmcl-shared
readonly workspace_image=/var/lib/xmcl-workspace.xfs
readonly workspace_image_staging=/var/lib/xmcl-workspace.xfs.new
readonly agent_env=/etc/xmcl-shared-node-agent.env
readonly ready_marker="$data_root/state/bootstrap-ready"

fail() {
  printf 'xmcl LightNode bootstrap: %s\n' "$*" >&2
  exit 1
}

[[ "$(id -u)" == 0 ]] || fail "must run as root"
[[ -f "$config_file" && ! -L "$config_file" ]] || fail "configuration must be a regular file"
[[ "$(stat -c '%u:%a' "$config_file")" == "0:600" ]] ||
  fail "configuration must be owned by root with mode 0600"
[[ -f "$service_file" && ! -L "$service_file" ]] ||
  fail "systemd service must be a regular file"
[[ "$(stat -c '%u:%a' "$service_file")" == "0:644" ]] ||
  fail "systemd service must be owned by root with mode 0644"

# The file is a trusted, runner-generated root secret. Ownership and mode are
# checked before sourcing so another local user cannot inject shell code.
# shellcheck disable=SC1090
source "$config_file"

required=(
  XMCL_NODE_ID XMCL_REGION XMCL_CONTROL_PLANE_URL XMCL_ENROLLMENT_TOKEN
  XMCL_INGRESS_HOST XMCL_OBJECT_STORAGE_ENDPOINT XMCL_OBJECT_STORAGE_REGION
  XMCL_OBJECT_STORAGE_BUCKET XMCL_RELEASE_MANIFEST_URL
  XMCL_RELEASE_MANIFEST_SHA256 XMCL_DOCKER_PACKAGE_VERSION XMCL_TOTAL_MEMORY_MIB
  XMCL_TOTAL_SHARED_CPU XMCL_TOTAL_WORKSPACE_GIB XMCL_WORKSPACE_VOLUME_GIB
  XMCL_BOOTSTRAP_TIMEOUT_SECONDS
)
for name in "${required[@]}"; do
  [[ -n "${!name:-}" ]] || fail "$name is required"
done

[[ "$XMCL_NODE_ID" =~ ^[A-Za-z0-9][A-Za-z0-9_.:-]{0,95}$ ]] ||
  fail "XMCL_NODE_ID is invalid"
[[ "$XMCL_REGION" == mow || "$XMCL_REGION" == tpe ]] ||
  fail "XMCL_REGION must be mow or tpe"
[[ "$XMCL_ENROLLMENT_TOKEN" =~ ^[A-Za-z0-9_-]{32,512}$ ]] ||
  fail "XMCL_ENROLLMENT_TOKEN is invalid"
[[ "$XMCL_CONTROL_PLANE_URL" =~ ^https://[^/?#]+(:[0-9]+)?(/[^?#]*)?$ ]] ||
  fail "XMCL_CONTROL_PLANE_URL must be HTTPS without query or fragment"
[[ "$XMCL_INGRESS_HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.-]{0,252}[A-Za-z0-9]$ ||
   "$XMCL_INGRESS_HOST" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] ||
  fail "XMCL_INGRESS_HOST is invalid"
[[ "$XMCL_OBJECT_STORAGE_ENDPOINT" =~ ^https://[^/?#]+/?$ ]] ||
  fail "XMCL_OBJECT_STORAGE_ENDPOINT must be an HTTPS origin"
[[ "$XMCL_OBJECT_STORAGE_REGION" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$ ]] ||
  fail "XMCL_OBJECT_STORAGE_REGION is invalid"
[[ "$XMCL_OBJECT_STORAGE_BUCKET" =~ ^[A-Za-z0-9][A-Za-z0-9.-]{1,62}[A-Za-z0-9]$ ]] ||
  fail "XMCL_OBJECT_STORAGE_BUCKET is invalid"
[[ "$XMCL_RELEASE_MANIFEST_URL" =~ ^https://github\.com/Voxelum/xmcl-shared-node-agent/releases/download/[^/]+/release-manifest\.json$ ]] ||
  fail "XMCL_RELEASE_MANIFEST_URL is not an approved release URL"
[[ "$XMCL_RELEASE_MANIFEST_SHA256" =~ ^[a-f0-9]{64}$ ]] ||
  fail "XMCL_RELEASE_MANIFEST_SHA256 is invalid"
[[ "$XMCL_DOCKER_PACKAGE_VERSION" =~ ^[A-Za-z0-9.+:~_-]+$ ]] ||
  fail "XMCL_DOCKER_PACKAGE_VERSION is invalid"
if [[ -n ${OTEL_EXPORTER_OTLP_ENDPOINT:-} ]]; then
  [[ "$OTEL_EXPORTER_OTLP_ENDPOINT" =~ ^https://[^/?#]+(/[^?#]*)?$ ||
    "$OTEL_EXPORTER_OTLP_ENDPOINT" =~ ^http://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]+)?(/[^?#]*)?$ ]] ||
    fail "OTEL_EXPORTER_OTLP_ENDPOINT must use HTTPS or loopback HTTP"
  otlp_headers_value=${OTEL_EXPORTER_OTLP_HEADERS:-}
  [[ ${#otlp_headers_value} -le 4096 &&
    "$otlp_headers_value" != *$'\n'* &&
    "$otlp_headers_value" != *$'\r'* ]] ||
    fail "OTEL_EXPORTER_OTLP_HEADERS is invalid"
  if [[ -n $otlp_headers_value ]]; then
    IFS=',' read -r -a otlp_headers <<<"$otlp_headers_value"
    for header in "${otlp_headers[@]}"; do
      [[ "$header" =~ ^[A-Za-z0-9._~-]+=[A-Za-z0-9%._~:/+=-]*$ ]] ||
        fail "OTEL_EXPORTER_OTLP_HEADERS is invalid"
      without_escapes=$(sed -E 's/%[0-9A-Fa-f]{2}//g' <<<"$header")
      [[ "$without_escapes" != *%* ]] ||
        fail "OTEL_EXPORTER_OTLP_HEADERS has invalid percent encoding"
    done
  fi
elif [[ -n ${OTEL_EXPORTER_OTLP_HEADERS:-} ]]; then
  fail "OTEL_EXPORTER_OTLP_HEADERS requires OTEL_EXPORTER_OTLP_ENDPOINT"
fi

for name in XMCL_TOTAL_MEMORY_MIB XMCL_TOTAL_SHARED_CPU \
  XMCL_TOTAL_WORKSPACE_GIB XMCL_WORKSPACE_VOLUME_GIB \
  XMCL_BOOTSTRAP_TIMEOUT_SECONDS; do
  [[ "${!name}" =~ ^[1-9][0-9]*$ ]] || fail "$name must be a positive integer"
done
(( XMCL_WORKSPACE_VOLUME_GIB >= XMCL_TOTAL_WORKSPACE_GIB )) ||
  fail "workspace volume must cover allocatable workspace"
(( XMCL_BOOTSTRAP_TIMEOUT_SECONDS >= 60 && XMCL_BOOTSTRAP_TIMEOUT_SECONDS <= 1800 )) ||
  fail "bootstrap timeout must be between 60 and 1800 seconds"

[[ -r /etc/os-release ]] || fail "cannot identify operating system"
# shellcheck disable=SC1091
source /etc/os-release
[[ "${ID:-}" == ubuntu && "${VERSION_ID:-}" == 24.04 ]] ||
  fail "the approved base image is Ubuntu 24.04 LTS"
[[ "$(dpkg --print-architecture)" == amd64 ]] ||
  fail "the approved architecture is amd64"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
  ca-certificates curl jq "docker.io=$XMCL_DOCKER_PACKAGE_VERSION" xfsprogs
apt-mark hold docker.io >/dev/null

getent group xmcl-agent >/dev/null || groupadd --system xmcl-agent
id xmcl-agent >/dev/null 2>&1 ||
  useradd --system --gid xmcl-agent --groups docker --home-dir "$data_root" \
    --no-create-home --shell /usr/sbin/nologin xmcl-agent
usermod -aG docker xmcl-agent

install -d -o root -g root -m 0750 \
  "$state_dir" /etc/xmcl-shared-node-agent
install -d -o root -g root -m 0755 /usr/local/libexec
install -d -o root -g xmcl-agent -m 0750 "$data_root"

release_manifest="$state_dir/release-manifest.json"
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --max-time 120 --output "$release_manifest.download" \
  "$XMCL_RELEASE_MANIFEST_URL"
printf '%s  %s\n' "$XMCL_RELEASE_MANIFEST_SHA256" \
  "$release_manifest.download" | sha256sum --check --status ||
  fail "release manifest digest verification failed"
install -o root -g root -m 0600 "$release_manifest.download" "$release_manifest"
rm -f "$release_manifest.download"

jq -e '
  .schemaVersion == 1 and
  (.gitCommit | type == "string") and
  (.tag | type == "string") and
  (.runtimeCatalogSha256 | type == "string") and
  .runtimeImage.name == "ghcr.io/voxelum/xmcl-shared-minecraft-runtime" and
  (.runtimeImage.digest | type == "string") and
  (.artifacts.nodeAgent.filename | type == "string") and
  (.artifacts.nodeAgent.sha256 | type == "string") and
  (.artifacts.quotaHelper.filename | type == "string") and
  (.artifacts.quotaHelper.sha256 | type == "string")
' "$release_manifest" >/dev/null || fail "release manifest schema is invalid"

release_tag=$(jq -er '.tag' "$release_manifest")
release_commit=$(jq -er '.gitCommit' "$release_manifest")
runtime_catalog_sha256=$(jq -er '.runtimeCatalogSha256' "$release_manifest")
runtime_image_name=$(jq -er '.runtimeImage.name' "$release_manifest")
runtime_image_digest=$(jq -er '.runtimeImage.digest' "$release_manifest")
agent_filename=$(jq -er '.artifacts.nodeAgent.filename' "$release_manifest")
agent_sha256=$(jq -er '.artifacts.nodeAgent.sha256' "$release_manifest")
quota_filename=$(jq -er '.artifacts.quotaHelper.filename' "$release_manifest")
quota_sha256=$(jq -er '.artifacts.quotaHelper.sha256' "$release_manifest")

[[ "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] ||
  fail "release manifest tag is invalid"
[[ "$release_commit" =~ ^[a-f0-9]{40}$ ]] ||
  fail "release manifest commit is invalid"
[[ "$runtime_catalog_sha256" =~ ^[a-f0-9]{64}$ ]] ||
  fail "release manifest catalog digest is invalid"
[[ "$runtime_image_digest" =~ ^sha256:[a-f0-9]{64}$ ]] ||
  fail "release manifest image digest is invalid"
[[ "$agent_filename" == xmcl-shared-node-agent-linux-amd64 &&
   "$quota_filename" == xmcl-quota-helper-linux-amd64 ]] ||
  fail "release manifest artifact names are invalid"
[[ "$agent_sha256" =~ ^[a-f0-9]{64}$ &&
   "$quota_sha256" =~ ^[a-f0-9]{64}$ ]] ||
  fail "release manifest artifact digests are invalid"
expected_manifest_url="https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/$release_tag/release-manifest.json"
[[ "$XMCL_RELEASE_MANIFEST_URL" == "$expected_manifest_url" ]] ||
  fail "release manifest URL does not match its immutable tag"
release_base="https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/$release_tag"
runtime_image="$runtime_image_name@$runtime_image_digest"

if [[ ! -e "$workspace_image" ]]; then
  if [[ -e "$workspace_image_staging" ]]; then
    [[ -f "$workspace_image_staging" && ! -L "$workspace_image_staging" &&
      "$(stat -c '%u:%a' "$workspace_image_staging")" == "0:600" ]] ||
      fail "incomplete workspace image is unsafe"
    rm -f "$workspace_image_staging"
  fi
  required_bytes=$(( (XMCL_WORKSPACE_VOLUME_GIB + 8) * 1024 * 1024 * 1024 ))
  available_bytes=$(df --output=avail -B1 /var/lib | tail -n 1 | tr -d ' ')
  (( available_bytes >= required_bytes )) ||
    fail "system disk lacks workspace volume plus 8 GiB host headroom"
  install -o root -g root -m 0600 /dev/null "$workspace_image_staging"
  fallocate -l "${XMCL_WORKSPACE_VOLUME_GIB}G" "$workspace_image_staging"
  mkfs.xfs -q "$workspace_image_staging"
  mv -T "$workspace_image_staging" "$workspace_image"
elif [[ ! -f "$workspace_image" || -L "$workspace_image" ||
  "$(stat -c '%u:%a' "$workspace_image")" != "0:600" ]]; then
  fail "existing workspace image is not a private root-owned regular file"
fi
[[ "$(blkid -s TYPE -o value "$workspace_image" 2>/dev/null)" == xfs ]] ||
  fail "workspace image is not XFS"

fstab_line="$workspace_image $data_root xfs loop,pquota,nodev,nosuid 0 2"
if grep -Eq "[[:space:]]$data_root[[:space:]]" /etc/fstab &&
  ! grep -Fqx "$fstab_line" /etc/fstab; then
  fail "workspace mount has an unrecognized fstab entry"
fi
grep -Fqx "$fstab_line" /etc/fstab || printf '%s\n' "$fstab_line" >> /etc/fstab
mountpoint -q "$data_root" || mount "$data_root"
[[ "$(findmnt -n -o FSTYPE --target "$data_root")" == xfs ]] ||
  fail "workspace mount is not XFS"
findmnt -n -o OPTIONS --target "$data_root" | tr ',' '\n' |
  grep -Eq '^(pquota|prjquota)$' ||
  fail "workspace mount does not have project quotas enabled"

volume_marker="$data_root/.xmcl-lightnode-workspace"
if [[ -e "$volume_marker" ]]; then
  [[ -f "$volume_marker" && ! -L "$volume_marker" &&
     "$(stat -c '%u:%a' "$volume_marker")" == "0:600" ]] ||
    fail "workspace marker is unsafe"
  grep -Fqx "node_id=$XMCL_NODE_ID" "$volume_marker" ||
    fail "workspace belongs to another node"
else
  install -o root -g root -m 0600 /dev/null "$volume_marker"
  printf 'node_id=%s\n' "$XMCL_NODE_ID" > "$volume_marker"
fi
install -d -o xmcl-agent -g xmcl-agent -m 0700 \
  "$data_root/workspaces" "$data_root/state"

download_install() {
  local url="$1" sha256="$2" destination="$3" owner="$4" group="$5" mode="$6"
  local temporary="$state_dir/$(basename "$destination").download"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
    --max-time 120 --output "$temporary" "$url"
  printf '%s  %s\n' "$sha256" "$temporary" | sha256sum --check --status ||
    fail "release digest verification failed for $destination"
  install -o "$owner" -g "$group" -m "$mode" "$temporary" "$destination"
  rm -f "$temporary"
}

download_install "$release_base/$agent_filename" "$agent_sha256" \
  /usr/local/bin/xmcl-shared-node-agent root root 0755
download_install "$release_base/$quota_filename" "$quota_sha256" \
  /usr/local/libexec/xmcl-quota-helper root xmcl-agent 4750

cat > /etc/xmcl-shared-node-agent/quota-helper.json <<EOF
{
  "workspaceRoot": "$data_root/workspaces",
  "mountPath": "$data_root",
  "projectBase": 100000,
  "agentUser": "xmcl-agent"
}
EOF
chmod 0600 /etc/xmcl-shared-node-agent/quota-helper.json

cat > "$agent_env" <<EOF
XMCL_SHARED_NODE_ID=$XMCL_NODE_ID
XMCL_SHARED_NODE_REGION=$XMCL_REGION
XMCL_CONTROL_PLANE_URL=$XMCL_CONTROL_PLANE_URL
XMCL_CONTROL_PLANE_CREDENTIAL=$XMCL_ENROLLMENT_TOKEN
XMCL_SHARED_NODE_INGRESS_HOST=$XMCL_INGRESS_HOST
XMCL_VULTR_OBJECT_STORAGE_ENDPOINT=$XMCL_OBJECT_STORAGE_ENDPOINT
XMCL_VULTR_OBJECT_STORAGE_REGION=$XMCL_OBJECT_STORAGE_REGION
XMCL_VULTR_OBJECT_STORAGE_BUCKET=$XMCL_OBJECT_STORAGE_BUCKET
XMCL_WORKSPACE_ROOT=$data_root/workspaces
XMCL_STATE_ROOT=$data_root/state
XMCL_CONTAINER_IMAGE=$runtime_image
XMCL_RCON_STOP_TIMEOUT_SECONDS=60
XMCL_TOTAL_MEMORY_MIB=$XMCL_TOTAL_MEMORY_MIB
XMCL_TOTAL_SHARED_CPU=$XMCL_TOTAL_SHARED_CPU
XMCL_TOTAL_WORKSPACE_GIB=$XMCL_TOTAL_WORKSPACE_GIB
XMCL_QUOTA_MOUNT_PATH=$data_root
XMCL_QUOTA_PROJECT_BASE=100000
XMCL_METRICS_ADDR=127.0.0.1:9464
EOF
if [[ -n ${OTEL_EXPORTER_OTLP_ENDPOINT:-} ]]; then
  printf 'OTEL_EXPORTER_OTLP_ENDPOINT=%s\n' "$OTEL_EXPORTER_OTLP_ENDPOINT" \
    >>"$agent_env"
  if [[ -n ${OTEL_EXPORTER_OTLP_HEADERS:-} ]]; then
    printf 'OTEL_EXPORTER_OTLP_HEADERS=%s\n' "$OTEL_EXPORTER_OTLP_HEADERS" \
      >>"$agent_env"
  fi
fi
chmod 0600 "$agent_env"

install -o root -g root -m 0644 \
  "$service_file" \
  /etc/systemd/system/xmcl-shared-node-agent.service

systemctl enable --now docker
docker pull "$runtime_image" >/dev/null
image_catalog_sha256=$(docker image inspect "$runtime_image" \
  --format '{{ index .Config.Labels "io.xmcl.runtime-catalog-sha256" }}')
[[ "$image_catalog_sha256" == "$runtime_catalog_sha256" ]] ||
  fail "runtime image catalog does not match the release manifest"
systemctl daemon-reload
rm -f "$ready_marker"
systemctl enable xmcl-shared-node-agent
systemctl restart xmcl-shared-node-agent

deadline=$((SECONDS + XMCL_BOOTSTRAP_TIMEOUT_SECONDS))
until [[ -f "$ready_marker" ]]; do
  (( SECONDS < deadline )) ||
    fail "agent did not confirm enrollment and heartbeat before timeout"
  systemctl is-active --quiet xmcl-shared-node-agent ||
    fail "agent stopped before confirming readiness"
  sleep 2
done

# The persisted rotating node credential is now authoritative. Remove the
# consumed enrollment token before any restart and erase the runner secret.
sed -i '/^XMCL_CONTROL_PLANE_CREDENTIAL=/d' "$agent_env"
shred -u "$config_file" 2>/dev/null || rm -f "$config_file"

# The provider firewall must remove TCP 22 separately after the runner sees a
# successful exit. Locally remove the bootstrap key and stop accepting SSH.
if [[ -f /root/.ssh/authorized_keys ]]; then
  install -o root -g root -m 0600 /dev/null /root/.ssh/authorized_keys
fi
if [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != root ]]; then
  bootstrap_home=$(getent passwd "$SUDO_USER" | cut -d: -f6)
  if [[ -n "$bootstrap_home" && -f "$bootstrap_home/.ssh/authorized_keys" ]]; then
    install -o "$SUDO_USER" -g "$(id -gn "$SUDO_USER")" -m 0600 /dev/null \
      "$bootstrap_home/.ssh/authorized_keys"
  fi
fi
systemctl disable --now ssh.service ssh.socket 2>/dev/null || true
printf 'xmcl LightNode bootstrap complete for node %s\n' "$XMCL_NODE_ID"
