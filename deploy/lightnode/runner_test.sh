#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
runner="$repo_root/deploy/lightnode/runner.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

cat >"$tmp/bin/ssh-keyscan" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$MOCK_SSH_KEYSCAN"
EOF
cat >"$tmp/bin/ssh-keygen" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == -lf && "$2" == - ]]; then
  cat >/dev/null
fi
printf '%s\n' '256 SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA host (ED25519)'
EOF
cat >"$tmp/bin/scp" <<'EOF'
#!/usr/bin/env bash
printf 'scp %s\n' "$*" >>"$MOCK_SSH_LOG"
EOF
cat >"$tmp/bin/ssh" <<'EOF'
#!/usr/bin/env bash
cat >/dev/null || true
printf 'ssh %s\n' "$*" >>"$MOCK_SSH_LOG"
count=1
if [[ -n "${MOCK_SSH_COUNT_FILE:-}" && -f "$MOCK_SSH_COUNT_FILE" ]]; then
  count=$(( $(cat "$MOCK_SSH_COUNT_FILE") + 1 ))
fi
if [[ -n "${MOCK_SSH_COUNT_FILE:-}" ]]; then
  printf '%s\n' "$count" >"$MOCK_SSH_COUNT_FILE"
fi
if [[ -z "${MOCK_SSH_FAIL_ON_CALL:-}" || "$MOCK_SSH_FAIL_ON_CALL" == "$count" ]]; then
  printf '%s\n' "${MOCK_SSH_ERROR:-}" >&2
  exit "${MOCK_SSH_EXIT:-0}"
fi
EOF
# Windows Git Bash cannot exercise Unix mode bits; opt-in shim is tests only.
# No real key material or network: all ssh/ssh-keyscan/ssh-keygen executables mocked.
if [[ "${TEST_WINDOWS_MODE_SHIM:-0}" == 1 ]]; then
  cat >"$tmp/bin/stat" <<'EOF'
#!/usr/bin/env bash
printf '600\n'
EOF
fi
chmod 0755 "$tmp/bin/"*

export PATH="$tmp/bin:$PATH"
export XMCL_LIGHTNODE_HOST=8.8.8.8
export XMCL_LIGHTNODE_PORT=22
export MOCK_SSH_KEYSCAN='8.8.8.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey'
expected_fingerprint=SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA

probe_output=$("$runner" probe)
[[ "$probe_output" == "$MOCK_SSH_KEYSCAN"$'\n'"256 $expected_fingerprint host (ED25519)" ]]

if MOCK_SSH_KEYSCAN='8.8.4.4 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey' \
  "$runner" probe >/dev/null 2>&1; then
  echo "probe accepted a key for a different address" >&2
  exit 1
fi
if MOCK_SSH_KEYSCAN=$'8.8.8.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITestKey\n8.8.8.8 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIConflict' \
  "$runner" probe >/dev/null 2>&1; then
  echo "probe accepted conflicting ED25519 keys" >&2
  exit 1
fi
if MOCK_SSH_KEYSCAN='8.8.8.8 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQTest' \
  "$runner" probe >/dev/null 2>&1; then
  echo "probe accepted an unsupported host key" >&2
  exit 1
fi

known_hosts="$tmp/known_hosts"
private_key="$tmp/id_ed25519"
bootstrap_env="$tmp/bootstrap.env"
bootstrap_script="$tmp/bootstrap.sh"
agent_service="$tmp/agent.service"
printf '%s\n' "$MOCK_SSH_KEYSCAN" >"$known_hosts"
printf '%s\n' private-key-placeholder >"$private_key"
printf '%s\n' XMCL_SHARED_NODE_ID=node-test >"$bootstrap_env"
printf '%s\n' '#!/usr/bin/env bash' >"$bootstrap_script"
printf '%s\n' '[Service]' >"$agent_service"
chmod 0600 "$private_key" "$known_hosts" "$bootstrap_env"

export XMCL_LIGHTNODE_USER=root
export XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY="$known_hosts"
export XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256="$expected_fingerprint"
export XMCL_LIGHTNODE_PRIVATE_KEY="$private_key"
export XMCL_LIGHTNODE_KNOWN_HOSTS="$known_hosts"
export XMCL_LIGHTNODE_HOST_KEY_SHA256="$expected_fingerprint"
export XMCL_LIGHTNODE_BOOTSTRAP_ENV="$bootstrap_env"
export XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT="$bootstrap_script"
export XMCL_LIGHTNODE_AGENT_SERVICE="$agent_service"
export MOCK_SSH_LOG="$tmp/ssh.log"

"$runner" apply
grep -q -- 'StrictHostKeyChecking=yes' "$MOCK_SSH_LOG"
grep -q -- 'PasswordAuthentication=no' "$MOCK_SSH_LOG"
grep -q -- 'KbdInteractiveAuthentication=no' "$MOCK_SSH_LOG"
grep -q -- 'HostKeyAlgorithms=ssh-ed25519' "$MOCK_SSH_LOG"
if grep -q -- 'StrictHostKeyChecking=no' "$MOCK_SSH_LOG"; then
  echo "apply disabled strict host-key checking" >&2
  exit 1
fi

if XMCL_LIGHTNODE_HOST_KEY_SHA256=SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB \
  "$runner" apply >/dev/null 2>&1; then
  echo "apply accepted a changed host-key fingerprint" >&2
  exit 1
fi

export MOCK_SSH_COUNT_FILE="$tmp/ssh-count"
for pair in 255:75 77:70 1:70; do
  remote_status=${pair%%:*}
  expected_status=${pair##*:}
  set +e
  rm -f "$MOCK_SSH_COUNT_FILE"
  MOCK_SSH_EXIT="$remote_status" MOCK_SSH_FAIL_ON_CALL=2 "$runner" apply >/dev/null 2>&1
  actual_status=$?
  set -e
  [[ "$actual_status" == "$expected_status" ]] || {
    echo "SSH exit $remote_status mapped to $actual_status, expected $expected_status" >&2
    exit 1
  }
done

export MOCK_SSH_COUNT_FILE="$tmp/ssh-count"
for pair in 255:76 77:77 1:70; do
  remote_status=${pair%%:*}
  expected_status=${pair##*:}
  rm -f "$MOCK_SSH_COUNT_FILE"
  set +e
  MOCK_SSH_EXIT="$remote_status" MOCK_SSH_FAIL_ON_CALL=6 \
    "$runner" apply >/dev/null 2>&1
  actual_status=$?
  set -e
  [[ "$actual_status" == "$expected_status" ]] || {
    echo "apply SSH exit $remote_status mapped to $actual_status, expected $expected_status" >&2
    exit 1
  }
done

# Added fixed-key transport preflight tests (all SSH commands remain mocked).
for entry in \
  '255|Permission denied (publickey).|70|authentication_rejected' \
  '255|Host key verification failed.|70|host_identity_rejected' \
  '255|REMOTE HOST IDENTIFICATION HAS CHANGED!|70|host_identity_rejected' \
  '255|No ED25519 host key is known for host.|70|host_identity_rejected' \
  '255|Connection timed out|75|transport_unavailable' \
  '255|Connection refused|75|transport_unavailable' \
  '124||75|transport_unavailable' \
  '255|unrecognized SSH problem|70|unclassified_failure'; do
  IFS='|' read -r code message expected category <<<"$entry"
  rm -f "$MOCK_SSH_COUNT_FILE" "$MOCK_SSH_LOG"
  set +e
  result=$(MOCK_SSH_EXIT="$code" MOCK_SSH_ERROR="$message" MOCK_SSH_FAIL_ON_CALL=1 \
    "$runner" apply 2>&1)
  status=$?
  set -e
  [[ "$status" == "$expected" && "$result" == "ssh_preflight=$category" ]]
  [[ "$(cat "$MOCK_SSH_COUNT_FILE")" == 1 ]]
  ! grep -q 'cat >' "$MOCK_SSH_LOG"
done

rm -f "$MOCK_SSH_COUNT_FILE" "$MOCK_SSH_LOG"
XMCL_LIGHTNODE_USER=root "$runner" apply
[[ "$(cat "$MOCK_SSH_COUNT_FILE")" == 6 ]]
! grep -q 'sudo' "$MOCK_SSH_LOG"
for required_option in '-4 -F /dev/null' 'IdentityAgent=none' 'ProxyCommand=none' \
  'ProxyJump=none' 'ConnectionAttempts=1' 'ServerAliveInterval=10' \
  'ServerAliveCountMax=2' 'StrictHostKeyChecking=yes'; do
  grep -q -- "$required_option" "$MOCK_SSH_LOG"
done

rm -f "$MOCK_SSH_COUNT_FILE" "$MOCK_SSH_LOG"
unset XMCL_LIGHTNODE_BOOTSTRAP_ENV XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT XMCL_LIGHTNODE_AGENT_SERVICE
[[ "$(XMCL_LIGHTNODE_USER=root "$runner" check)" == 'ssh_preflight=authenticated' ]]
[[ "$(cat "$MOCK_SSH_COUNT_FILE")" == 1 ]]
! grep -q 'cat >' "$MOCK_SSH_LOG"
printf 'PASS: runner shell contract + fixed-key preflight cases; mocked SSH only; Windows mode shim=%s\n' "${TEST_WINDOWS_MODE_SHIM:-0}"
