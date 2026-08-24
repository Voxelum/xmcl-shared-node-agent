#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != Linux ]]; then
  echo "mock acceptance must run inside Linux or WSL2" >&2
  exit 1
fi
for command in docker go java ldd mkfs.xfs xfs_quota sudo; do
  command -v "$command" >/dev/null || {
    echo "missing prerequisite: $command" >&2
    exit 1
  }
done
if ! java --list-modules | grep -q '^jdk.compiler@'; then
  echo "the WSL Java runtime must provide the jdk.compiler module" >&2
  exit 1
fi

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
state_root="${XMCL_MOCK_LIGHTNODE_STATE_ROOT:-/var/lib/xmcl-mock-lightnode}"
mount_path="${XMCL_MOCK_LIGHTNODE_MOUNT:-/mnt/xmcl-mock-lightnode}"
workspace_root="$mount_path/workspaces"
image_file="$state_root/workspace.xfs"
image_size_gib="${XMCL_MOCK_LIGHTNODE_DISK_GIB:-4}"
runtime_image="${XMCL_MOCK_LIGHTNODE_RUNTIME_IMAGE:-xmcl/mock-lightnode-runtime:local}"
base_image="${XMCL_MOCK_LIGHTNODE_BASE_IMAGE:-}"
agent_user="${XMCL_MOCK_LIGHTNODE_AGENT_USER:-}"
if [[ -z "$agent_user" ]]; then
  agent_user="$(getent passwd 1000 | cut -d: -f1)"
fi
if [[ -z "$agent_user" || "$agent_user" == root ]]; then
  echo "set XMCL_MOCK_LIGHTNODE_AGENT_USER to the non-root node-agent account" >&2
  exit 1
fi

if ! [[ "$image_size_gib" =~ ^[1-9][0-9]?$ ]]; then
  echo "XMCL_MOCK_LIGHTNODE_DISK_GIB must be an integer from 1 through 99" >&2
  exit 1
fi

temporary="$(mktemp -d)"
cleanup() {
  rm -rf "$temporary"
}
trap cleanup EXIT

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -buildvcs=false -trimpath -ldflags "-s -w -X main.version=mock-lightnode" \
  -o "$temporary/xmcl-shared-minecraft-runtime" \
  "$repository/cmd/xmcl-shared-minecraft-runtime"

if [[ -n "$base_image" ]]; then
  cat >"$temporary/Dockerfile" <<EOF
FROM $base_image
USER 0:0
COPY xmcl-shared-minecraft-runtime /usr/local/bin/xmcl-shared-minecraft-runtime
USER 1000:1000
EOF
else
  catalog_sha="$(sed -n 's/.*"sha256": "\([a-f0-9]\{64\}\)".*/\1/p' \
    "$repository/internal/runtime/catalog.json")"
  if [[ -z "$catalog_sha" ]]; then
    echo "could not read the reviewed runtime catalog identity" >&2
    exit 1
  fi
  java_home="$(dirname "$(dirname "$(readlink -f "$(command -v java)")")")"
  if ! java -version 2>&1 | head -n 1 | grep -Eq '"21(\.|")'; then
    echo "the local acceptance image requires the WSL Java 21 runtime" >&2
    exit 1
  fi
  install -d "$temporary/rootfs/opt/xmcl/jre/21" "$temporary/rootfs/bin"
  cp -a "$java_home/." "$temporary/rootfs/opt/xmcl/jre/21/"
  cp -a --parents /etc/java-21-openjdk "$temporary/rootfs"
  cp "$(readlink -f /bin/sh)" "$temporary/rootfs/bin/sh"
  while IFS= read -r dependency; do
    cp --parents "$dependency" "$temporary/rootfs"
  done < <(
    {
      find "$java_home" -type f \
        \( -perm /111 -o -name '*.so' -o -name '*.so.*' \) -exec ldd {} \; 2>/dev/null
      ldd "$(readlink -f /bin/sh)"
    } | awk '/=> \// { print $3 } $1 ~ /^\// { print $1 }' | sort -u
  )
  cat >"$temporary/Dockerfile" <<EOF
FROM scratch
LABEL io.xmcl.runtime-catalog-sha256="$catalog_sha"
COPY rootfs /
COPY xmcl-shared-minecraft-runtime /usr/local/bin/xmcl-shared-minecraft-runtime
USER 1000:1000
WORKDIR /data
EXPOSE 25565/tcp
HEALTHCHECK --interval=2s --timeout=2s --start-period=5s --retries=10 CMD ["/usr/local/bin/xmcl-shared-minecraft-runtime", "health"]
ENTRYPOINT ["/usr/local/bin/xmcl-shared-minecraft-runtime"]
EOF
fi
docker build --quiet --tag "$runtime_image" "$temporary" >/dev/null

sudo install -d -m 0755 "$state_root" "$mount_path" \
  /usr/local/libexec /etc/xmcl-shared-node-agent
if [[ ! -f "$image_file" ]]; then
  sudo truncate -s "${image_size_gib}G" "$image_file"
  sudo mkfs.xfs -f "$image_file" >/dev/null
fi
if ! mountpoint -q "$mount_path"; then
  sudo mount -o loop,prjquota "$image_file" "$mount_path"
fi
if [[ "$(findmnt -no FSTYPE "$mount_path")" != xfs ]] ||
  ! findmnt -no OPTIONS "$mount_path" | grep -Eq '(^|,)(prjquota|pquota)(,|$)'; then
  echo "$mount_path is not an XFS project-quota mount" >&2
  exit 1
fi

agent_group="$(id -gn "$agent_user")"
sudo install -d -m 0750 -o "$agent_user" -g "$agent_group" "$workspace_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -buildvcs=false -trimpath -ldflags "-s -w" \
  -o "$temporary/xmcl-quota-helper" \
  "$repository/cmd/xmcl-quota-helper"
sudo install -o root -g root -m 4755 "$temporary/xmcl-quota-helper" \
  /usr/local/libexec/xmcl-quota-helper

cat >"$temporary/quota-helper.json" <<EOF
{"workspaceRoot":"$workspace_root","mountPath":"$mount_path","projectBase":200000,"agentUser":"$agent_user"}
EOF
sudo install -o root -g root -m 0644 "$temporary/quota-helper.json" \
  /etc/xmcl-shared-node-agent/quota-helper.json

(
  cd "$repository"
  XMCL_ACCEPTANCE_RUNTIME_IMAGE="$runtime_image" \
  XMCL_ACCEPTANCE_WORKSPACE_ROOT="$workspace_root" \
    go test -buildvcs=false -tags=integration ./internal/docker \
      -run '^TestMockLightNodeDockerLifecycle$' -count=1 -v
)
