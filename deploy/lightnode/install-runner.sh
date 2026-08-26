#!/usr/bin/env bash
set -euo pipefail

readonly release_url="https://github.com/Voxelum/xmcl-shared-node-agent/releases/download/v0.3.7"
readonly bundle_name="xmcl-lightnode-bootstrap-runner-linux-amd64.tar.gz"
readonly bundle_sha256="cf01632dec0b277b74b0f62467c1979e91b45e6cf14d25dba15825e3ad6f8d05"
readonly state_root="/var/lib/xmcl-lightnode-runner"
readonly install_root="/opt/xmcl-lightnode-runner"

if [[ ${EUID} -ne 0 || $# -ne 1 ]]; then
  echo "usage: sudo $0 <public-ip>" >&2
  exit 2
fi
if [[ ! $1 =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
  echo "public-ip must be an IPv4 address" >&2
  exit 2
fi

readonly public_ip="$1"
readonly public_name="${public_ip}.sslip.io"

read -r -s -p "Runner secret: " runner_secret
echo
read -r -s -p "Approval secret: " approval_secret
echo
if (( ${#runner_secret} < 32 || ${#approval_secret} < 32 )); then
  echo "runner secrets must contain at least 32 characters" >&2
  exit 2
fi

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates certbot curl nginx openssh-client

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"; unset runner_secret approval_secret' EXIT
curl --fail --location --proto '=https' --tlsv1.2 \
  "${release_url}/${bundle_name}" -o "${tmp_dir}/${bundle_name}"
echo "${bundle_sha256}  ${tmp_dir}/${bundle_name}" | sha256sum --check -
tar -xzf "${tmp_dir}/${bundle_name}" -C "$tmp_dir"
test -x "${tmp_dir}/xmcl-lightnode-bootstrap-runner"
test -x "${tmp_dir}/runner.sh"
test -x "${tmp_dir}/bootstrap.sh"

id xmcl-lightnode-runner >/dev/null 2>&1 ||
  useradd --system --home-dir "$state_root" --shell /usr/sbin/nologin \
    xmcl-lightnode-runner
install -d -m 0700 -o xmcl-lightnode-runner -g xmcl-lightnode-runner \
  "$state_root"
install -d -m 0755 -o root -g root "$install_root"
install -m 0755 "${tmp_dir}/xmcl-lightnode-bootstrap-runner" \
  /usr/local/bin/xmcl-lightnode-bootstrap-runner
install -m 0755 "${tmp_dir}/runner.sh" "$install_root/runner.sh"
install -m 0755 "${tmp_dir}/bootstrap.sh" "$install_root/bootstrap.sh"
install -m 0644 "${tmp_dir}/xmcl-shared-node-agent.service" \
  "$install_root/xmcl-shared-node-agent.service"
install -m 0644 "${tmp_dir}/xmcl-lightnode-bootstrap-runner.service" \
  /etc/systemd/system/xmcl-lightnode-bootstrap-runner.service

if [[ -n ${XMCL_LIGHTNODE_BOOTSTRAP_KEY_FILE:-} ]]; then
  test -f "$XMCL_LIGHTNODE_BOOTSTRAP_KEY_FILE"
  install -m 0600 -o xmcl-lightnode-runner -g xmcl-lightnode-runner \
    "$XMCL_LIGHTNODE_BOOTSTRAP_KEY_FILE" "$state_root/bootstrap-key"
  ssh-keygen -y -f "$state_root/bootstrap-key" \
    >"$state_root/bootstrap-key.pub"
elif [[ ! -f "$state_root/bootstrap-key" ]]; then
  ssh-keygen -q -t rsa -b 4096 -N "" \
    -C "xmcl-lightnode-staging-bootstrap" \
    -f "$state_root/bootstrap-key"
fi
chown xmcl-lightnode-runner:xmcl-lightnode-runner \
  "$state_root/bootstrap-key" "$state_root/bootstrap-key.pub"
chmod 0600 "$state_root/bootstrap-key"
chmod 0644 "$state_root/bootstrap-key.pub"

install -m 0600 /dev/null /etc/xmcl-lightnode-bootstrap-runner.env
cat >/etc/xmcl-lightnode-bootstrap-runner.env <<EOF
XMCL_LIGHTNODE_RUNNER_ADDR=127.0.0.1:8088
XMCL_LIGHTNODE_RUNNER_STATE_ROOT=${state_root}
XMCL_LIGHTNODE_RUNNER_SECRET=${runner_secret}
XMCL_LIGHTNODE_RUNNER_APPROVAL_SECRET=${approval_secret}
XMCL_LIGHTNODE_RUNNER_SCRIPT=${install_root}/runner.sh
XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT=${install_root}/bootstrap.sh
XMCL_LIGHTNODE_AGENT_SERVICE=${install_root}/xmcl-shared-node-agent.service
XMCL_LIGHTNODE_PRIVATE_KEY=${state_root}/bootstrap-key
XMCL_LIGHTNODE_SSH_USER=root
XMCL_LIGHTNODE_SSH_PORT=22
EOF

systemctl stop nginx
certbot certonly --standalone --non-interactive --agree-tos \
  --register-unsafely-without-email -d "$public_name"

cat >/etc/nginx/sites-available/xmcl-lightnode-runner <<EOF
server {
    listen 80;
    listen [::]:80;
    server_name ${public_name};
    return 301 https://\$host\$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name ${public_name};

    ssl_certificate /etc/letsencrypt/live/${public_name}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${public_name}/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    client_max_body_size 32k;

    location = /v1/bootstrap-jobs {
        limit_except POST { deny all; }
        proxy_pass http://127.0.0.1:8088;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_connect_timeout 5s;
        proxy_send_timeout 30s;
        proxy_read_timeout 1900s;
    }

    location ~ "^/v1/bootstrap-jobs/[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}/(status|approve)\$" {
        allow 127.0.0.1;
        allow ::1;
        deny all;
        limit_except POST { deny all; }
        proxy_pass http://127.0.0.1:8088;
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_connect_timeout 5s;
        proxy_send_timeout 30s;
        proxy_read_timeout 30s;
    }

    location / {
        return 404;
    }
}
EOF
ln -sfn /etc/nginx/sites-available/xmcl-lightnode-runner \
  /etc/nginx/sites-enabled/xmcl-lightnode-runner
rm -f /etc/nginx/sites-enabled/default

nginx -t
systemctl start nginx
systemctl daemon-reload
systemctl enable xmcl-lightnode-bootstrap-runner
systemctl restart xmcl-lightnode-bootstrap-runner
systemctl is-active --quiet xmcl-lightnode-bootstrap-runner
test "$(curl --silent --show-error --output /dev/null \
  --write-out '%{http_code}' --request POST \
  "https://${public_name}/v1/bootstrap-jobs")" = "401"

echo "Bootstrap public key:"
cat "$state_root/bootstrap-key.pub"
