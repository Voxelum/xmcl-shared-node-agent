#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
source "$root/deploy/lightnode/bootstrap.sh"
mkdir -p "$tmp/bin" "$tmp/keys"
chmod 0700 "$tmp/keys"
if [[ "${TEST_WINDOWS_CLEANUP_SHIM:-0}" == 1 ]]; then
  cat >"$tmp/bin/stat" <<'EOF'
#!/usr/bin/env bash
if [[ "$2" == '%u' ]]; then printf '0\n'; elif [[ -d "$3" ]]; then printf '700\n'; else printf '600\n'; fi
EOF
  for tool in chown flock; do
    printf '#!/usr/bin/env bash\nexit 0\n' >"$tmp/bin/$tool"
  done
  chmod 0755 "$tmp/bin/"*
  export PATH="$tmp/bin:$PATH"
elif [[ "$(id -u)" != 0 ]] || ! command -v flock >/dev/null; then
  printf 'UNSUPPORTED: real cleanup permission/lock tests require Linux root + flock; no implicit shim\n' >&2
  exit 78
fi
# Synthetic PUBLIC RSA wire record, not a real generated keypair. No private key.
blob=$(node -e 'const B=Buffer; const s=x=>{const b=B.isBuffer(x)?x:B.from(x);const n=B.alloc(4);n.writeUInt32BE(b.length);return B.concat([n,b])};const n=B.alloc(385,133);n[0]=0;console.log(B.concat([s("ssh-rsa"),s(B.from([1,0,1])),s(n)]).toString("base64"))')
printf 'ssh-rsa %s platform-managed\n' "$blob" >"$tmp/managed.pub"
fp=$(ssh-keygen -lf "$tmp/managed.pub" -E sha256 | awk '{print $2}')
target="$tmp/keys/authorized_keys"
printf '# existing comment\nssh-ed25519 OTHER unchanged\nssh-rsa %s platform-managed\nssh-rsa %s duplicate\ncommand="echo hello world" ssh-ed25519 OTHER2 options-preserved\n' "$blob" "$blob" >"$target"
chmod 0600 "$target"
printf '# existing comment\nssh-ed25519 OTHER unchanged\ncommand="echo hello world" ssh-ed25519 OTHER2 options-preserved\n' >"$tmp/expected"
remove_managed_authorized_key "$target" "$tmp/managed.pub" "$fp"
cmp "$target" "$tmp/expected"
remove_managed_authorized_key "$target" "$tmp/managed.pub" "$fp"
cmp "$target" "$tmp/expected"
# Wrong fingerprint and absent key blob must not change unrelated records.
if remove_managed_authorized_key "$target" "$tmp/managed.pub" SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA; then exit 1; fi
cmp "$target" "$tmp/expected"
remove_managed_authorized_key "$tmp/keys/missing" "$tmp/managed.pub" "$fp"
# The same key with options is unsupported: refuse, do not falsely claim revocation.
printf 'restrict ssh-rsa %s option-prefixed\n' "$blob" >"$target"
cp "$target" "$tmp/before"
if remove_managed_authorized_key "$target" "$tmp/managed.pub" "$fp"; then exit 1; fi
cmp "$target" "$tmp/before"
# Invalid public key / multiline record cannot authorize removal.
printf 'ssh-rsa BAD\nssh-rsa BAD\n' >"$tmp/invalid.pub"
if remove_managed_authorized_key "$target" "$tmp/invalid.pub" "$fp"; then exit 1; fi
cmp "$target" "$tmp/before"
# Fault injection must preserve the original even when caller uses `if`/`||`.
export REAL_AWK=$(command -v awk)
export REAL_FLOCK=$(command -v flock)
cat >"$tmp/bin/awk-fault" <<'EOF'
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "${MOCK_AWK_PARTIAL:-0}" == 1 && "$arg" == '!($1 == kind && $2 == blob)' ]]; then
    printf 'partial-output-must-not-replace-original\n'
    exit 42
  fi
done
exec "$REAL_AWK" "$@"
EOF
# Preserve the prior (real or explicitly shimmed) flock implementation.
cp "$REAL_FLOCK" "$tmp/bin/flock-original"
cat >"$tmp/bin/flock" <<'EOF'
#!/usr/bin/env bash
[[ "${MOCK_FLOCK_FAIL:-0}" != 1 ]] || exit 42
exec "$(dirname "$0")/flock-original" "$@"
EOF
mv "$tmp/bin/awk-fault" "$tmp/bin/awk"
chmod 0755 "$tmp/bin/awk" "$tmp/bin/flock" "$tmp/bin/flock-original"
export PATH="$tmp/bin:$PATH"
printf 'ssh-ed25519 OTHER preserve\nssh-rsa %s managed\n' "$blob" >"$target"
cp "$target" "$tmp/before"
if MOCK_FLOCK_FAIL=1 remove_managed_authorized_key "$target" "$tmp/managed.pub" "$fp"; then exit 1; fi
cmp "$target" "$tmp/before"
if MOCK_AWK_PARTIAL=1 remove_managed_authorized_key "$target" "$tmp/managed.pub" "$fp"; then exit 1; fi
cmp "$target" "$tmp/before"
# Candidate contains no service-disabling command at all.
! grep -Eq 'systemctl (disable|stop|mask).*ssh|/dev/null /root/.ssh/authorized_keys' "$root/deploy/lightnode/bootstrap.sh"
printf 'PASS: exact public-key cleanup algorithm, duplicate/no-match/bad-fingerprint/unsupported-options/invalid-record; Windows ownership+flock shim=%s\n' "${TEST_WINDOWS_CLEANUP_SHIM:-0}"
