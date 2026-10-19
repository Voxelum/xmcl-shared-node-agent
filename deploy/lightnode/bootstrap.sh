#!/usr/bin/env bash
set -euo pipefail

readonly config_file="${1:-/run/xmcl-lightnode-bootstrap.env}"
readonly service_file="${2:-/run/xmcl-shared-node-agent.service}"
readonly managed_public_file="${3:-}"
readonly managed_public_fingerprint="${4:-}"
readonly state_dir=/var/lib/xmcl-bootstrap
readonly data_root=/var/lib/xmcl-shared
readonly workspace_image=/var/lib/xmcl-workspace.xfs
readonly workspace_image_staging=/var/lib/xmcl-workspace.xfs.new
readonly agent_env=/etc/xmcl-shared-node-agent.env
readonly ready_marker="$data_root/state/bootstrap-ready"
readonly enrollment_rejected_marker="$data_root/state/bootstrap-enrollment-rejected"

fail() {
  printf 'xmcl LightNode bootstrap: %s\n' "$*" >&2
  exit 1
}

reject_enrollment() {
  sed -i '/^XMCL_CONTROL_PLANE_CREDENTIAL=/d' "$agent_env" 2>/dev/null || true
  shred -u "$config_file" 2>/dev/null || rm -f "$config_file"
  exit 77
}

managed_public_key_matches() (
  set -euo pipefail
  local file="$1" expected="$2" kind blob comment actual bits
  [[ "$expected" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] || return 1
  [[ -f "$file" && ! -L "$file" ]] || return 1
  mapfile -t lines < "$file" || return 1
  [[ "${#lines[@]}" == 1 ]] || return 1
  read -r kind blob comment <<<"${lines[0]}" || return 1
  [[ "$kind" == ssh-rsa && "$blob" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || return 1
  actual=$(ssh-keygen -lf "$file" -E sha256) || return 1
  read -r bits actual comment <<<"$actual" || return 1
  [[ "$bits" =~ ^[0-9]+$ && "$bits" -ge 3072 && "$actual" == "$expected" ]]
)

# Only standard unoptioned public-key records are managed by this candidate.
# Option-prefixed keys, comments and unrelated records are preserved verbatim.
remove_managed_authorized_key() (
  set -euo pipefail
  local target="$1" public_file="$2" expected_fingerprint="$3"
  local key_type key_blob comment actual temporary parent
  managed_public_key_matches "$public_file" "$expected_fingerprint" || return 1
  [[ "$expected_fingerprint" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] || return 1
  [[ -f "$public_file" && ! -L "$public_file" ]] || return 1
  mapfile -t public_lines < "$public_file" || return 1
  [[ "${#public_lines[@]}" == 1 ]] || return 1
  read -r key_type key_blob comment <<<"${public_lines[0]}" || return 1
  [[ "$key_type" == ssh-rsa && "$key_blob" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || return 1
  actual=$(ssh-keygen -lf "$public_file" -E sha256 | awk 'NR == 1 {print $2}') || return 1
  [[ "$actual" == "$expected_fingerprint" ]] || return 1
  # Missing target is an idempotent no-op, but a dangling symlink is rejected.
  [[ ! -L "$target" ]] || return 1
  [[ -e "$target" ]] || return 0
  [[ -f "$target" ]] || return 1
  parent=$(dirname -- "$target") || return 1
  [[ -d "$parent" && ! -L "$parent" ]] || return 1
  [[ "$(stat -c '%u' "$parent")" == 0 && "$(stat -c '%u' "$target")" == 0 ]] || return 1
  [[ "$(stat -c '%a' "$parent")" == 700 ]] || return 1
  case "$(stat -c '%a' "$target")" in 600|644) ;; *) return 1;; esac
  # The root-owned 0700 directory makes the lock and replace non-user-writable.
  [[ ! -L "$parent/.xmcl-key-cleanup.lock" ]] || return 1
  umask 077
  exec 9>"$parent/.xmcl-key-cleanup.lock" || return 1
  flock -x 9 || return 1
  # Refuse an option-prefixed instance of this same key rather than silently
  # claiming it was revoked. Comments are not authorization records.
  awk -v kind="$key_type" -v blob="$key_blob" '
    /^[[:space:]]*#/ { next }
    { for (i=2; i<NF; i++) if ($i == kind && $(i+1) == blob) exit 1 }
  ' "$target" || return 1
  temporary=$(mktemp "$parent/.authorized_keys.xmcl.XXXXXX") || return 1
  trap 'rm -f "$temporary"' EXIT
  awk -v kind="$key_type" -v blob="$key_blob" \
    '!($1 == kind && $2 == blob)' "$target" >"$temporary" || return 1
  chmod --reference="$target" "$temporary" || return 1
  chown --reference="$target" "$temporary" || return 1
  mv -f -- "$temporary" "$target" || return 1
)

validate_object_storage_region() {
  [[ "$1" == azure ]]
}

if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
  return 0
fi

[[ "$(id -u)" == 0 ]] || fail "must run as root"
[[ "${SUDO_USER:-root}" == root ]] || fail "this fixed-key candidate requires root login"
[[ -n "$managed_public_file" && -n "$managed_public_fingerprint" ]] || fail "explicit managed public key and fingerprint required"
managed_public_key_matches "$managed_public_file" "$managed_public_fingerprint" || fail "invalid managed public key"
[[ -f "$config_file" && ! -L "$config_file" ]] || fail "configuration must be a regular file"
[[ "$(stat -c '%u:%a' "$config_file")" == "0:600" ]] ||
  fail "configuration must be owned by root with mode 0600"
[[ -f "$service_file" && ! -L "$service_file" ]] ||
  fail "systemd service must be a regular file"
[[ "$(stat -c '%u:%a' "$service_file")" == "0:644" ]] ||
  fail "systemd service must be owned by root with mode 0644"
# Parse the runner's single-quoted records as data, never execute config text.
# None of the approved field grammars needs a literal single quote.
declare -A seen_config=()
while IFS= read -r record || [[ -n "$record" ]]; do
  [[ "$record" =~ ^([A-Z][A-Z0-9_]*)=\'([^\']*)\'$ ]] || fail "invalid configuration record"
  name="${BASH_REMATCH[1]}"; value="${BASH_REMATCH[2]}"
  case "$name" in
    XMCL_NODE_ID|XMCL_INSTANCE_ID|XMCL_REGION|XMCL_CONTROL_PLANE_URL|XMCL_ENROLLMENT_TOKEN|XMCL_INGRESS_HOST|XMCL_OBJECT_STORAGE_ENDPOINT|XMCL_OBJECT_STORAGE_REGION|XMCL_OBJECT_STORAGE_BUCKET|XMCL_RELEASE_MANIFEST_URL|XMCL_RELEASE_MANIFEST_SHA256|XMCL_DOCKER_PACKAGE_VERSION|XMCL_TOTAL_MEMORY_MIB|XMCL_TOTAL_SHARED_CPU|XMCL_TOTAL_WORKSPACE_GIB|XMCL_WORKSPACE_VOLUME_GIB|XMCL_BOOTSTRAP_TIMEOUT_SECONDS|OTEL_EXPORTER_OTLP_ENDPOINT|OTEL_EXPORTER_OTLP_HEADERS) ;;
    *) fail "unknown configuration field" ;;
  esac
  [[ -z "${seen_config[$name]:-}" ]] || fail "duplicate configuration field"
  [[ "$value" != *$'\r'* && "$value" != *$'\n'* ]] || fail "invalid configuration value"
  seen_config[$name]=1
  printf -v "$name" '%s' "$value"
done < "$config_file"

required=(
  XMCL_NODE_ID XMCL_INSTANCE_ID XMCL_REGION XMCL_CONTROL_PLANE_URL XMCL_ENROLLMENT_TOKEN
  XMCL_INGRESS_HOST XMCL_OBJECT_STORAGE_ENDPOINT XMCL_OBJECT_STORAGE_REGION
  XMCL_OBJECT_STORAGE_BUCKET
  XMCL_RELEASE_MANIFEST_URL
  XMCL_RELEASE_MANIFEST_SHA256 XMCL_DOCKER_PACKAGE_VERSION XMCL_TOTAL_MEMORY_MIB
  XMCL_TOTAL_SHARED_CPU XMCL_TOTAL_WORKSPACE_GIB XMCL_WORKSPACE_VOLUME_GIB
  XMCL_BOOTSTRAP_TIMEOUT_SECONDS
)
for name in "${required[@]}"; do
  [[ -n "${seen_config[$name]:-}" && -n "${!name:-}" ]] || fail "$name is required"
done

[[ "$XMCL_NODE_ID" =~ ^[A-Za-z0-9][A-Za-z0-9_.:-]{0,95}$ ]] ||
  fail "XMCL_NODE_ID is invalid"
[[ "$XMCL_INSTANCE_ID" =~ ^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$ ]] ||
  fail "XMCL_INSTANCE_ID is invalid"
[[ "$XMCL_REGION" == tpe ]] ||
  fail "new LightNode deployment is supported only in Taipei (tpe/cn-taiwan-2)"
validate_object_storage_region "$XMCL_OBJECT_STORAGE_REGION" ||
  fail "Azure Blob storage region must be azure"
[[ "$XMCL_ENROLLMENT_TOKEN" =~ ^[A-Za-z0-9_-]{32,512}$ ]] ||
  fail "XMCL_ENROLLMENT_TOKEN is invalid"
[[ "$XMCL_CONTROL_PLANE_URL" =~ ^https://[^/?#]+(:[0-9]+)?(/[^?#]*)?$ ]] ||
  fail "XMCL_CONTROL_PLANE_URL must be HTTPS without query or fragment"
[[ "$XMCL_INGRESS_HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.-]{0,252}[A-Za-z0-9]$ ||
   "$XMCL_INGRESS_HOST" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] ||
  fail "XMCL_INGRESS_HOST is invalid"
[[ "$XMCL_OBJECT_STORAGE_ENDPOINT" =~ ^https://[a-z0-9]{3,24}\.blob\.core\.windows\.net/?$ ]] ||
  fail "XMCL_OBJECT_STORAGE_ENDPOINT must be an Azure Blob account HTTPS origin"
[[ "$XMCL_OBJECT_STORAGE_BUCKET" =~ ^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$ &&
   "$XMCL_OBJECT_STORAGE_BUCKET" != *--* ]] ||
  fail "XMCL_OBJECT_STORAGE_BUCKET must be an Azure Blob container name"
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
  [[ "${!name}" =~ ^[1-9][0-9]{0,6}$ ]] || fail "$name must be a bounded positive integer"
done
(( XMCL_TOTAL_MEMORY_MIB <= 1048576 && XMCL_TOTAL_SHARED_CPU <= 4096 &&
   XMCL_TOTAL_WORKSPACE_GIB <= 16384 && XMCL_WORKSPACE_VOLUME_GIB <= 16384 )) ||
  fail "capacity exceeds supported preflight bounds"
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

# Fail closed before apt/users/systemd/persistent destinations. No tool bootstrap.
for tool in python3 timeout mktemp rm; do
  command -v "$tool" >/dev/null || fail "preflight prerequisite missing: $tool"
done
umask 077
preflight_dir=$(mktemp -d /tmp/xmcl-release-preflight.XXXXXXXXXX)
trap 'rm -rf -- "$preflight_dir"' EXIT
timeout --signal=TERM --kill-after=5s 300s python3 -I - "$preflight_dir" "$XMCL_RELEASE_MANIFEST_URL" "$XMCL_RELEASE_MANIFEST_SHA256" <<'XMCL_PREFLIGHT_PY'
# Embedded verbatim in bootstrap.sh; stdlib only, no downloaded code execution.
import hashlib, json, os, re, sys, urllib.request, urllib.parse
from pathlib import Path
root, manifest_url, manifest_sha = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
HEX = r'[a-f0-9]{64}'
class Rejected(ValueError): pass
def require(ok, reason):
    if not ok: raise Rejected(reason)
def match(pattern, value):
    return isinstance(value, str) and re.fullmatch(pattern, value) is not None
def digest(data): return hashlib.sha256(data).hexdigest()
def unique(pairs):
    obj = {}
    for k, v in pairs:
        require(k not in obj, 'duplicate JSON key')
        obj[k] = v
    return obj
def parse(data): return json.loads(data, object_pairs_hook=unique)
def url_ok(url):
    p = urllib.parse.urlsplit(url)
    require(p.scheme == 'https' and not p.username and not p.password and p.port in (None,443) and not p.fragment, 'unsafe download URL')
    require(p.hostname in {'github.com','release-assets.githubusercontent.com','objects.githubusercontent.com','raw.githubusercontent.com','ghcr.io','pkg-containers.githubusercontent.com'}, 'unapproved download host')
class Redirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        url_ok(newurl)
        result = super().redirect_request(req, fp, code, msg, headers, newurl)
        if urllib.parse.urlsplit(req.full_url).hostname != urllib.parse.urlsplit(newurl).hostname:
            result.remove_header('Authorization')
        return result
opener = urllib.request.build_opener(Redirect())
def fetch(url, limit=8*1024*1024, token=None, accept=None):
    url_ok(url)
    headers = {'User-Agent':'xmcl-bootstrap-preflight/1'}
    if token: headers['Authorization'] = 'Bearer '+token
    if accept: headers['Accept'] = accept
    with opener.open(urllib.request.Request(url, headers=headers), timeout=30) as response:
        url_ok(response.url)
        data = response.read(limit+1)
    require(len(data) <= limit, 'download exceeds bound')
    return data
def checked(url, expected, limit=8*1024*1024, **kw):
    require(match(HEX, expected), 'invalid expected digest')
    data = fetch(url, limit, **kw)
    require(digest(data) == expected, 'download digest mismatch')
    return data
def artifact(record, filename, base, download=False):
    expected={'filename','sha256','sizeBytes'}
    if filename=='xmcl-shared-node-agent-linux-amd64': expected.add('version')
    require(isinstance(record,dict) and set(record)==expected, 'artifact schema mismatch')
    if 'version' in record: require(record['version']==m['gitCommit'],'agent version/commit mismatch')
    require(record['filename']==filename and match(HEX,record['sha256']), 'artifact identity mismatch')
    size=record['sizeBytes']
    require(type(size) is int and 0 < size <= 128*1024*1024, 'artifact size invalid')
    if not download: return
    data=checked(base+'/'+filename,record['sha256'],size)
    require(len(data)==size,'artifact size mismatch')
    (root/filename).write_bytes(data)
def image(record, labels):
    repo=record['name'].removeprefix('ghcr.io/')
    auth=parse(fetch('https://ghcr.io/token?service=ghcr.io&scope=repository:'+repo+':pull'))
    token=auth.get('token')
    require(isinstance(token,str) and 0 < len(token) < 16384 and not any(c.isspace() for c in token), 'registry token invalid')
    base='https://ghcr.io/v2/'+repo
    accept=','.join(['application/vnd.oci.image.index.v1+json','application/vnd.oci.image.manifest.v1+json','application/vnd.docker.distribution.manifest.list.v2+json','application/vnd.docker.distribution.manifest.v2+json'])
    def document(d, expected_size=None):
        require(match('sha256:'+HEX,d),'registry digest invalid')
        raw=checked(base+'/manifests/'+d,d[7:],token=token,accept=accept)
        if expected_size is not None: require(len(raw)==expected_size,'image manifest descriptor size mismatch')
        return parse(raw)
    doc=document(record['digest'])
    require(type(doc.get('schemaVersion')) is int and doc['schemaVersion']==2,'image manifest version invalid')
    if 'manifests' in doc:
        candidates=[d for d in doc['manifests'] if d.get('platform',{}).get('os')=='linux' and d.get('platform',{}).get('architecture')=='amd64' and d.get('platform',{}).get('variant','') in ('','v1')]
        require(len(candidates)==1,'exactly one linux/amd64 image required')
        selected=candidates[0]
        require(type(selected.get('size')) is int and 0<selected['size']<=8*1024*1024 and selected.get('mediaType') in {'application/vnd.oci.image.manifest.v1+json','application/vnd.docker.distribution.manifest.v2+json'} and 'urls' not in selected,'unverifiable image descriptor')
        doc=document(selected['digest'],selected['size'])
    require(type(doc.get('schemaVersion')) is int and doc['schemaVersion']==2 and doc.get('mediaType') in {'application/vnd.oci.image.manifest.v1+json','application/vnd.docker.distribution.manifest.v2+json'} and isinstance(doc.get('layers'),list) and len(doc['layers'])>0,'image manifest invalid')
    for layer in doc['layers']:
        require(isinstance(layer,dict) and match('sha256:'+HEX,layer.get('digest')) and type(layer.get('size')) is int and 0<layer['size']<2**63 and layer.get('mediaType') in {'application/vnd.oci.image.layer.v1.tar','application/vnd.oci.image.layer.v1.tar+gzip','application/vnd.oci.image.layer.v1.tar+zstd','application/vnd.docker.image.rootfs.diff.tar.gzip'} and 'urls' not in layer,'unverifiable layer descriptor')
    config=doc['config']
    require(isinstance(config,dict) and match('sha256:'+HEX,config.get('digest')) and type(config.get('size')) is int and 0<config['size']<=8*1024*1024 and config.get('mediaType') in {'application/vnd.oci.image.config.v1+json','application/vnd.docker.container.image.v1+json'} and 'urls' not in config,'image config descriptor invalid')
    raw=checked(base+'/blobs/'+config['digest'], config['digest'][7:], token=token)
    require(len(raw)==config.get('size'),'image config size mismatch')
    config=parse(raw)
    require(config.get('os')=='linux' and config.get('architecture')=='amd64','image platform mismatch')
    actual=config.get('config',{}).get('Labels',{})
    require(all(actual.get(k)==v for k,v in labels.items()),'image catalog/identity mismatch')
try:
    require(match(r'https://github\.com/Voxelum/xmcl-shared-node-agent/releases/download/v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?/release-manifest\.json',manifest_url),'unapproved manifest URL')
    raw=checked(manifest_url,manifest_sha)
    m=parse(raw)
    require(isinstance(m,dict) and set(m)=={'schemaVersion','gitCommit','tag','runtimeCatalogSha256','runtimeImage','builderImage','artifacts'},'manifest schema mismatch')
    require(type(m['schemaVersion']) is int and m['schemaVersion']==2,'manifest version mismatch')
    require(match(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?',m['tag']) and match(r'[a-f0-9]{40}',m['gitCommit']),'release version invalid')
    base='https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/'+m['tag']
    require(manifest_url==base+'/release-manifest.json','manifest tag mismatch')
    require(match(HEX,m['runtimeCatalogSha256']),'catalog digest invalid')
    runtime,builder=m['runtimeImage'],m['builderImage']
    require(set(runtime)=={'name','digest'} and runtime['name']=='ghcr.io/voxelum/xmcl-shared-minecraft-runtime' and match('sha256:'+HEX,runtime['digest']),'runtime identity invalid')
    require(set(builder)=={'name','digest','schema','toolchainCatalogRevision','runtimeCatalogRevision','target'} and builder['name']=='ghcr.io/voxelum/xmcl-shared-node-builder' and match('sha256:'+HEX,builder['digest']),'builder identity invalid')
    require(builder['schema']=='agent_builder_v1' and builder['target']=='linux-amd64' and match(HEX,builder['toolchainCatalogRevision']) and builder['runtimeCatalogRevision']==m['runtimeCatalogSha256'],'builder catalog binding invalid')
    # Commit-addressed source catalogs; never use a branch, moving tag or old candidate.
    source='https://raw.githubusercontent.com/Voxelum/xmcl-shared-node-agent/'+m['gitCommit']
    catalog=checked(source+'/runtime-image/runtime-catalog.lock.json',m['runtimeCatalogSha256'])
    require(isinstance(parse(catalog),dict),'runtime catalog is not an object')
    toolchain=parse(fetch(source+'/compiler/toolchain-catalog.lock.json'))
    require(toolchain.get('catalogRevision')==builder['toolchainCatalogRevision'] and toolchain.get('runtimeCatalogRevision')==m['runtimeCatalogSha256'],'toolchain catalog mismatch')
    def canonical_safe(v):
        if isinstance(v,dict):
            require(all(isinstance(k,str) and k.isascii() and not k.isdecimal() for k in v),'unsupported catalog key')
            for value in v.values(): canonical_safe(value)
        elif isinstance(v,list):
            for value in v: canonical_safe(value)
        else:
            require(v is None or isinstance(v,(str,bool)) or (type(v) is int and abs(v)<2**53),'unsupported catalog scalar')
    canonical_safe(toolchain)
    canonical=dict(toolchain, catalogRevision='0'*64)
    require(digest(json.dumps(canonical,sort_keys=True,separators=(',',':'),ensure_ascii=False).encode())==builder['toolchainCatalogRevision'],'toolchain content digest mismatch')
    artifacts=m['artifacts']
    require(set(artifacts)=={'nodeAgent','quotaHelper','runtimeEntrypoint'},'artifact set invalid')
    for key,name in [('nodeAgent','xmcl-shared-node-agent-linux-amd64'),('quotaHelper','xmcl-quota-helper-linux-amd64'),('runtimeEntrypoint','xmcl-shared-minecraft-runtime-linux-amd64')]:
        artifact(artifacts[key],name,base)
    image(runtime,{'io.xmcl.runtime-catalog-sha256':m['runtimeCatalogSha256']})
    image(builder,{'io.xmcl.builder.schema':builder['schema'],'io.xmcl.builder.toolchain-catalog-revision':builder['toolchainCatalogRevision'],'io.xmcl.builder.runtime-catalog-revision':m['runtimeCatalogSha256'],'io.xmcl.builder.target':'linux-amd64'})
    # Metadata identity first; binary payloads only after both image identities pass.
    for key,name in [('nodeAgent','xmcl-shared-node-agent-linux-amd64'),('quotaHelper','xmcl-quota-helper-linux-amd64'),('runtimeEntrypoint','xmcl-shared-minecraft-runtime-linux-amd64')]:
        artifact(artifacts[key],name,base,download=True)
    (root/'release-manifest.json').write_bytes(raw)
    values=[m['tag'],m['gitCommit'],m['runtimeCatalogSha256'],runtime['name']+'@'+runtime['digest'],builder['name']+'@'+builder['digest'],builder['schema'],builder['toolchainCatalogRevision']]
    (root/'values').write_text('\n'.join(values)+'\n')
except Exception as error:
    # No URLs, headers, tokens or raw remote content in errors.
    print('xmcl release preflight rejected: '+(str(error) if isinstance(error,Rejected) else type(error).__name__),file=sys.stderr)
    sys.exit(1)
XMCL_PREFLIGHT_PY
mapfile -t release_values < "$preflight_dir/values"
release_tag="${release_values[0]}"
release_commit="${release_values[1]}"
runtime_catalog_sha256="${release_values[2]}"
runtime_image="${release_values[3]}"
builder_image="${release_values[4]}"
builder_schema="${release_values[5]}"
builder_catalog_revision="${release_values[6]}"
agent_filename=xmcl-shared-node-agent-linux-amd64
quota_filename=xmcl-quota-helper-linux-amd64
# MUTATION BOUNDARY: everything above must succeed before any host setup.
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends \
  ca-certificates curl jq "docker.io=$XMCL_DOCKER_PACKAGE_VERSION" xfsprogs
apt-mark hold docker.io >/dev/null

if getent group xmcl-agent >/dev/null; then
  [[ "$(getent group xmcl-agent | cut -d: -f3)" == 10001 ]] ||
    fail "xmcl-agent must use the reviewed Builder GID 10001"
else
  ! getent group 10001 >/dev/null ||
    fail "reviewed Builder GID 10001 is already assigned"
  groupadd --system --gid 10001 xmcl-agent
fi
id xmcl-agent >/dev/null 2>&1 ||
  useradd --system --uid 10001 --gid xmcl-agent --groups docker --home-dir "$data_root" \
    --no-create-home --shell /usr/sbin/nologin xmcl-agent
[[ "$(id -u xmcl-agent)" == 10001 ]] ||
  fail "xmcl-agent must use the reviewed Builder UID 10001"
[[ "$(id -g xmcl-agent)" == 10001 ]] ||
  fail "xmcl-agent must use the reviewed Builder GID 10001"
usermod -aG docker xmcl-agent

install -d -o root -g root -m 0750 \
  "$state_dir" /etc/xmcl-shared-node-agent
install -d -o root -g root -m 0755 /usr/local/libexec
install -d -o root -g xmcl-agent -m 0750 "$data_root"

release_manifest="$state_dir/release-manifest.json"
install -o root -g root -m 0600 "$preflight_dir/release-manifest.json" "$release_manifest"

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
  "$data_root/workspaces" "$data_root/state" "$data_root/builds"

# Install the exact bytes already verified in the private temporary directory.
install -o root -g root -m 0755 "$preflight_dir/$agent_filename" /usr/local/bin/xmcl-shared-node-agent
install -o root -g xmcl-agent -m 4750 "$preflight_dir/$quota_filename" /usr/local/libexec/xmcl-quota-helper

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
XMCL_SHARED_NODE_INSTANCE_ID=$XMCL_INSTANCE_ID
XMCL_SHARED_NODE_REGION=$XMCL_REGION
XMCL_CONTROL_PLANE_URL=$XMCL_CONTROL_PLANE_URL
XMCL_CONTROL_PLANE_CREDENTIAL=$XMCL_ENROLLMENT_TOKEN
XMCL_SHARED_NODE_INGRESS_HOST=$XMCL_INGRESS_HOST
XMCL_AZURE_BLOB_ENDPOINT=$XMCL_OBJECT_STORAGE_ENDPOINT
XMCL_AZURE_BLOB_CONTAINER=$XMCL_OBJECT_STORAGE_BUCKET
XMCL_WORKSPACE_ROOT=$data_root/workspaces
XMCL_STATE_ROOT=$data_root/state
XMCL_CONTAINER_IMAGE=$runtime_image
XMCL_BUILDER_IMAGE=$builder_image
XMCL_BUILDER_TOOLCHAIN_CATALOG_REVISION=$builder_catalog_revision
XMCL_BUILDER_ROOT=$data_root/builds
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
docker pull "$builder_image" >/dev/null
image_catalog_sha256=$(docker image inspect "$runtime_image" \
  --format '{{ index .Config.Labels "io.xmcl.runtime-catalog-sha256" }}')
[[ "$image_catalog_sha256" == "$runtime_catalog_sha256" ]] ||
  fail "runtime image catalog does not match the release manifest"
image_builder_schema=$(docker image inspect "$builder_image" \
  --format '{{ index .Config.Labels "io.xmcl.builder.schema" }}')
image_builder_catalog=$(docker image inspect "$builder_image" \
  --format '{{ index .Config.Labels "io.xmcl.builder.toolchain-catalog-revision" }}')
image_builder_runtime_catalog=$(docker image inspect "$builder_image" \
  --format '{{ index .Config.Labels "io.xmcl.builder.runtime-catalog-revision" }}')
image_builder_target=$(docker image inspect "$builder_image" \
  --format '{{ index .Config.Labels "io.xmcl.builder.target" }}')
[[ "$image_builder_schema" == "$builder_schema" &&
   "$image_builder_catalog" == "$builder_catalog_revision" &&
   "$image_builder_runtime_catalog" == "$runtime_catalog_sha256" &&
   "$image_builder_target" == linux-amd64 ]] ||
  fail "builder image identity does not match the release manifest"
systemctl daemon-reload
rm -f "$ready_marker" "$enrollment_rejected_marker"
systemctl enable xmcl-shared-node-agent
systemctl restart xmcl-shared-node-agent

deadline=$((SECONDS + XMCL_BOOTSTRAP_TIMEOUT_SECONDS))
until [[ -f "$ready_marker" ]]; do
  [[ ! -f "$enrollment_rejected_marker" ]] || reject_enrollment
  (( SECONDS < deadline )) ||
    fail "agent did not confirm enrollment and heartbeat before timeout"
  if ! systemctl is-active --quiet xmcl-shared-node-agent; then
    [[ ! -f "$enrollment_rejected_marker" ]] || reject_enrollment
    fail "agent stopped before confirming readiness"
  fi
  sleep 2
done

# The persisted rotating node credential is now authoritative. Remove the
# consumed enrollment token before any restart and erase the runner secret.
sed -i '/^XMCL_CONTROL_PLANE_CREDENTIAL=/d' "$agent_env"
shred -u "$config_file" 2>/dev/null || rm -f "$config_file"

# Exact managed-key cleanup only. The provider /32 rule is reconciled separately.
# Never empty authorized_keys and never disable sshd in this candidate.
remove_managed_authorized_key /root/.ssh/authorized_keys \
  "$managed_public_file" "$managed_public_fingerprint" ||
  fail "managed SSH public-key cleanup rejected; control-plane diagnosis required, never reinstall"
rm -f -- "$managed_public_file"
printf 'xmcl LightNode bootstrap complete for node %s\n' "$XMCL_NODE_ID"