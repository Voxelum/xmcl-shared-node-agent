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
host_pattern="[$XMCL_LIGHTNODE_HOST]:$XMCL_LIGHTNODE_PORT"
case "$mode" in
  probe)
    # This phase sends no key, token, config, or command. Persist the complete
    # output against the expected provider instance before allowing apply.
    scanned_key=$(ssh-keyscan -T 10 -p "$XMCL_LIGHTNODE_PORT" \
      -t ed25519 "$XMCL_LIGHTNODE_HOST" 2>/dev/null) ||
      fail "could not read the SSH host key"
    key_fields=$(printf '%s\n' "$scanned_key" |
      awk '$2 == "ssh-ed25519" && NF == 3 { print $2 " " $3 }')
    [[ -n "$key_fields" && "$(printf '%s\n' "$key_fields" | wc -l)" == 1 ]] ||
      fail "expected exactly one ED25519 host key"
    scan="$host_pattern $key_fields"
    printf '%s\n' "$scan"
    printf '%s\n' "$scan" | ssh-keygen -lf - -E sha256
    ;;
  apply)
    for name in XMCL_LIGHTNODE_USER XMCL_LIGHTNODE_PRIVATE_KEY \
      XMCL_LIGHTNODE_KNOWN_HOSTS XMCL_LIGHTNODE_HOST_KEY_SHA256 \
      XMCL_LIGHTNODE_BOOTSTRAP_ENV XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT \
      XMCL_LIGHTNODE_AGENT_SERVICE; do
      required "$name"
    done
    [[ "$XMCL_LIGHTNODE_USER" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] ||
      fail "XMCL_LIGHTNODE_USER is invalid"
    [[ "$XMCL_LIGHTNODE_HOST_KEY_SHA256" =~ ^SHA256:[A-Za-z0-9+/]{43}$ ]] ||
      fail "XMCL_LIGHTNODE_HOST_KEY_SHA256 is invalid"
    for file in "$XMCL_LIGHTNODE_PRIVATE_KEY" "$XMCL_LIGHTNODE_KNOWN_HOSTS" \
      "$XMCL_LIGHTNODE_BOOTSTRAP_ENV" "$XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT" \
      "$XMCL_LIGHTNODE_AGENT_SERVICE"; do
      [[ -f "$file" && ! -L "$file" ]] || fail "$file must be a regular file"
    done
    [[ "$(stat -c '%a' "$XMCL_LIGHTNODE_PRIVATE_KEY")" == 600 ]] ||
      fail "bootstrap private key must have mode 0600"
    [[ "$(stat -c '%a' "$XMCL_LIGHTNODE_KNOWN_HOSTS")" == 600 ]] ||
      fail "known-hosts file must have mode 0600"
    [[ "$(stat -c '%a' "$XMCL_LIGHTNODE_BOOTSTRAP_ENV")" == 600 ]] ||
      fail "bootstrap environment must have mode 0600"

    matching_key=$(ssh-keygen -F "$host_pattern" \
      -f "$XMCL_LIGHTNODE_KNOWN_HOSTS" 2>/dev/null |
      grep -v '^#' || true)
    [[ -n "$matching_key" && "$(printf '%s\n' "$matching_key" | wc -l)" == 1 ]] ||
      fail "known-hosts must contain exactly one key for the expected endpoint"
    actual_fingerprint=$(printf '%s\n' "$matching_key" |
      ssh-keygen -lf - -E sha256 | awk '{print $2}')
    [[ "$actual_fingerprint" == "$XMCL_LIGHTNODE_HOST_KEY_SHA256" ]] ||
      fail "persisted SSH host key fingerprint does not match the job"

    ssh_options=(
      -p "$XMCL_LIGHTNODE_PORT"
      -i "$XMCL_LIGHTNODE_PRIVATE_KEY"
      -o BatchMode=yes
      -o ConnectTimeout=15
      -o IdentitiesOnly=yes
      -o PasswordAuthentication=no
      -o KbdInteractiveAuthentication=no
      -o StrictHostKeyChecking=yes
      -o "UserKnownHostsFile=$XMCL_LIGHTNODE_KNOWN_HOSTS"
    )
    target="$XMCL_LIGHTNODE_USER@$XMCL_LIGHTNODE_HOST"

    stream_root_file() {
      local source="$1" destination="$2" mode="$3"
      ssh "${ssh_options[@]}" "$target" \
        "sudo -n sh -ceu 'umask 077; cat > \"$destination\"; chmod \"$mode\" \"$destination\"; chown root:root \"$destination\"'" \
        < "$source"
    }

    stream_root_file "$XMCL_LIGHTNODE_BOOTSTRAP_ENV" \
      /run/xmcl-lightnode-bootstrap.env 0600
    stream_root_file "$XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT" \
      /run/xmcl-lightnode-bootstrap 0700
    stream_root_file "$XMCL_LIGHTNODE_AGENT_SERVICE" \
      /run/xmcl-shared-node-agent.service 0644

    ssh "${ssh_options[@]}" "$target" \
      "sudo -n /run/xmcl-lightnode-bootstrap /run/xmcl-lightnode-bootstrap.env /run/xmcl-shared-node-agent.service"
    ;;
  *)
    fail "usage: runner.sh probe|apply"
    ;;
esac
