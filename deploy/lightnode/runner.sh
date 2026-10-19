#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'xmcl LightNode runner: %s\n' "$*" >&2
  exit 1
}

required() {
  local name="$1"
  [[ -n "${!name:-}" ]] || fail "$name is required"
}

required XMCL_LIGHTNODE_HOST
required XMCL_LIGHTNODE_PORT
[[ "$XMCL_LIGHTNODE_HOST" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] ||
  fail "XMCL_LIGHTNODE_HOST must be the expected literal public IPv4"
[[ "$XMCL_LIGHTNODE_PORT" =~ ^[1-9][0-9]{0,4}$ ]] &&
  (( XMCL_LIGHTNODE_PORT <= 65535 )) ||
  fail "XMCL_LIGHTNODE_PORT is invalid"

mode="${1:-}"
if [[ "$XMCL_LIGHTNODE_PORT" == 22 ]]; then
  host_pattern="$XMCL_LIGHTNODE_HOST"
else
  host_pattern="[$XMCL_LIGHTNODE_HOST]:$XMCL_LIGHTNODE_PORT"
fi
case "$mode" in
  probe)
    # This phase sends no key, token, config, or command.
    scanned_key=$(ssh-keyscan -T 10 -p "$XMCL_LIGHTNODE_PORT" \
      -t ed25519 "$XMCL_LIGHTNODE_HOST" 2>/dev/null) ||
      fail "could not read the SSH host key"
    mapfile -t scanned_lines <<<"$scanned_key"
    [[ "${#scanned_lines[@]}" == 1 ]] ||
      fail "expected exactly one ED25519 host key"
    read -r scanned_host scanned_type scanned_blob extra <<<"${scanned_lines[0]}"
    [[ -z "${extra:-}" && "$scanned_host" == "$host_pattern" &&
      "$scanned_type" == ssh-ed25519 &&
      "$scanned_blob" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] ||
      fail "SSH host key does not match the exact endpoint"
    known_host="$host_pattern ssh-ed25519 $scanned_blob"
    fingerprint_line=$(printf '%s\n' "$known_host" | ssh-keygen -lf - -E sha256) ||
      fail "could not fingerprint the SSH host key"
    read -r bits host_fingerprint comment algorithm extra <<<"$fingerprint_line"
    [[ -z "${extra:-}" && "$bits" == 256 &&
      "$host_fingerprint" =~ ^SHA256:[A-Za-z0-9+/]{43}$ &&
      "$algorithm" == "(ED25519)" ]] ||
      fail "SSH host fingerprint is invalid"
    printf '%s\n%s\n' "$known_host" "$fingerprint_line"
    ;;
  apply|check)
    required_names=(XMCL_LIGHTNODE_USER XMCL_LIGHTNODE_PRIVATE_KEY
      XMCL_LIGHTNODE_KNOWN_HOSTS XMCL_LIGHTNODE_HOST_KEY_SHA256)
    if [[ "$mode" == apply ]]; then
      required_names+=(XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256
        XMCL_LIGHTNODE_BOOTSTRAP_ENV XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT
        XMCL_LIGHTNODE_AGENT_SERVICE)
    fi
    for name in "${required_names[@]}"; do required "$name"; done
    [[ "$XMCL_LIGHTNODE_USER" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] ||
      fail "XMCL_LIGHTNODE_USER is invalid"
    [[ "$XMCL_LIGHTNODE_HOST_KEY_SHA256" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] ||
      fail "XMCL_LIGHTNODE_HOST_KEY_SHA256 is invalid"
    input_files=("$XMCL_LIGHTNODE_PRIVATE_KEY" "$XMCL_LIGHTNODE_KNOWN_HOSTS")
    if [[ "$mode" == apply ]]; then
      input_files+=("$XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY" "$XMCL_LIGHTNODE_BOOTSTRAP_ENV" "$XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT"
        "$XMCL_LIGHTNODE_AGENT_SERVICE")
    fi
    for file in "${input_files[@]}"; do
      [[ -f "$file" && ! -L "$file" ]] || fail "$file must be a regular file"
    done
    [[ "$(stat -c '%a' "$XMCL_LIGHTNODE_PRIVATE_KEY")" == 600 ]] ||
      fail "bootstrap private key must have mode 0600"
    [[ "$(stat -c '%a' "$XMCL_LIGHTNODE_KNOWN_HOSTS")" == 600 ]] ||
      fail "known-hosts file must have mode 0600"
    if [[ "$mode" == apply ]]; then
      [[ "$(stat -c '%a' "$XMCL_LIGHTNODE_BOOTSTRAP_ENV")" == 600 ]] ||
        fail "bootstrap environment must have mode 0600"
    fi

    if [[ "$mode" == apply ]]; then
      [[ "$XMCL_LIGHTNODE_USER" == root ]] || fail "fixed-key apply requires root login"
      [[ "$XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] ||
        fail "managed public-key fingerprint is invalid"
      actual_managed_fingerprint=$(ssh-keygen -lf "$XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY" -E sha256 | awk 'NR == 1 {print $2}')
      [[ "$actual_managed_fingerprint" == "$XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256" ]] ||
        fail "managed public-key fingerprint mismatch"
    fi

    mapfile -t known_host_lines < "$XMCL_LIGHTNODE_KNOWN_HOSTS"
    [[ "${#known_host_lines[@]}" == 1 ]] ||
      fail "known-hosts must contain exactly one key for the expected endpoint"
    read -r known_host known_type known_blob extra <<<"${known_host_lines[0]}"
    [[ -z "${extra:-}" && "$known_host" == "$host_pattern" &&
      "$known_type" == ssh-ed25519 &&
      "$known_blob" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] ||
      fail "known-hosts entry does not match the exact endpoint"
    actual_fingerprint=$(ssh-keygen -lf "$XMCL_LIGHTNODE_KNOWN_HOSTS" \
      -E sha256 | awk 'NF >= 2 { print $2 }')
    [[ "$actual_fingerprint" == "$XMCL_LIGHTNODE_HOST_KEY_SHA256" ]] ||
      fail "persisted SSH host key fingerprint does not match the job"

    ssh_options=(
      -4
      -F /dev/null
      -p "$XMCL_LIGHTNODE_PORT"
      -i "$XMCL_LIGHTNODE_PRIVATE_KEY"
      -o BatchMode=yes
      -o ConnectTimeout=15
      -o ConnectionAttempts=1
      -o ServerAliveInterval=10
      -o ServerAliveCountMax=2
      -o ProxyCommand=none
      -o ProxyJump=none
      -o IdentityAgent=none
      -o IdentitiesOnly=yes
      -o PubkeyAuthentication=yes
      -o PasswordAuthentication=no
      -o KbdInteractiveAuthentication=no
      -o GSSAPIAuthentication=no
      -o HostKeyAlgorithms=ssh-ed25519
      -o StrictHostKeyChecking=yes
      -o UpdateHostKeys=no
      -o VerifyHostKeyDNS=no
      -o GlobalKnownHostsFile=/dev/null
      -o "UserKnownHostsFile=$XMCL_LIGHTNODE_KNOWN_HOSTS"
      -o LogLevel=ERROR
    )
    target="$XMCL_LIGHTNODE_USER@$XMCL_LIGHTNODE_HOST"
    # root is the provider's Linux administrator; do not assume sudo is installed.
    root_prefix="sudo -n "
    [[ "$XMCL_LIGHTNODE_USER" != root ]] || root_prefix=""

    # Verify host identity and authentication before streaming any enrollment data.
    # This performs no setup and emits only a bounded diagnostic category.
    umask 077
    preflight_error=$(mktemp)
    trap 'rm -f "$preflight_error"' EXIT
    set +e
    LC_ALL=C timeout --foreground --signal=TERM --kill-after=5s 25s \
      ssh "${ssh_options[@]}" "$target" "${root_prefix}true" \
      </dev/null >/dev/null 2>"$preflight_error"
    preflight_status=$?
    set -e
    if [[ "$preflight_status" != 0 ]]; then
      if grep -Eqi 'REMOTE HOST IDENTIFICATION HAS CHANGED|Host key verification failed|No .* host key is known|no matching host key type found' "$preflight_error"; then
        printf 'ssh_preflight=host_identity_rejected\n' >&2
        exit 70
      elif grep -Eqi 'Permission denied|no mutual signature|sign_and_send_pubkey: signing failed' "$preflight_error"; then
        printf 'ssh_preflight=authentication_rejected\n' >&2
        exit 70
      elif [[ "$preflight_status" == 124 || "$preflight_status" == 137 ]] ||
        grep -Eqi 'Connection timed out|Connection refused|No route to host|Network is unreachable|Connection reset|Connection closed|kex_exchange_identification' "$preflight_error"; then
        printf 'ssh_preflight=transport_unavailable\n' >&2
        exit 75
      else
        printf 'ssh_preflight=unclassified_failure\n' >&2
        exit 70
      fi
    fi
    rm -f "$preflight_error"
    trap - EXIT
    [[ "$mode" != check ]] || { printf 'ssh_preflight=authenticated\n'; exit 0; }

    classify_ssh_exit() {
      local status="$1" phase="$2"
      case "$status" in
        255)
          if [[ "$phase" == apply ]]; then
            exit 76 # bootstrap may still be running remotely; reconcile first
          fi
          exit 75 # no bootstrap side effect started; the transfer may retry
          ;;
        77)
          [[ "$phase" == apply ]] && exit 77
          exit 70
          ;;
        *) exit 70 ;;
      esac
    }

    stream_root_file() {
      local source="$1" destination="$2" mode="$3"
      local status
      set +e
      ssh "${ssh_options[@]}" "$target" \
        "${root_prefix}sh -ceu 'umask 077; cat > \"$destination\"; chmod \"$mode\" \"$destination\"; chown root:root \"$destination\"'" \
        < "$source"
      status=$?
      set -e
      [[ "$status" == 0 ]] || classify_ssh_exit "$status" transfer
    }

    stream_root_file "$XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY" /run/xmcl-lightnode-bootstrap.pub 0644
    stream_root_file "$XMCL_LIGHTNODE_BOOTSTRAP_ENV" \
      /run/xmcl-lightnode-bootstrap.env 0600
    stream_root_file "$XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT" \
      /run/xmcl-lightnode-bootstrap 0700
    stream_root_file "$XMCL_LIGHTNODE_AGENT_SERVICE" \
      /run/xmcl-shared-node-agent.service 0644

    set +e
    ssh "${ssh_options[@]}" "$target" \
      "${root_prefix}/bin/bash /run/xmcl-lightnode-bootstrap /run/xmcl-lightnode-bootstrap.env /run/xmcl-shared-node-agent.service /run/xmcl-lightnode-bootstrap.pub $XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256"
    status=$?
    set -e
    [[ "$status" == 0 ]] || classify_ssh_exit "$status" apply
    ;;
  *)
    fail "usage: runner.sh probe|check|apply"
    ;;
esac
