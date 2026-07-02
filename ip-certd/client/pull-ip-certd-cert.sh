#!/usr/bin/env bash
set -Eeuo pipefail

IP_CERTD_TMP_DIR=""

cleanup() {
  if [[ -n "${IP_CERTD_TMP_DIR:-}" ]]; then
    rm -rf "$IP_CERTD_TMP_DIR"
  fi
}

usage() {
  cat >&2 <<'EOF'
Usage:
  pull-ip-certd-cert.sh <api-base-url>

Example:
  pull-ip-certd-cert.sh https://example.com/api

Environment overrides:
  IP_CERTD_IP             Use this IPv4 instead of auto-detection.
  IP_CERTD_INSTALL_ROOT   Certificate install root. Default: /etc/nginx/ssl
  IP_CERTD_RELOAD_NGINX   Reload nginx after install. Default: 1
EOF
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

normalize_api_base() {
  local value="${1%/}"
  if [[ -z "$value" ]]; then
    fail "api-base-url must not be empty"
  fi
  if [[ "$value" != http://* && "$value" != https://* ]]; then
    value="https://$value"
  fi
  printf '%s\n' "$value"
}

host_from_url() {
  local value="${1#*://}"
  value="${value%%/*}"
  value="${value%%:*}"
  [[ -n "$value" ]] || fail "failed to parse domain from api-base-url"
  printf '%s\n' "$value"
}

detect_public_ipv4() {
  if [[ -n "${IP_CERTD_IP:-}" ]]; then
    printf '%s\n' "$IP_CERTD_IP"
    return
  fi

  local service ip
  for service in \
    "https://api.ipify.org" \
    "https://ifconfig.me/ip" \
    "https://icanhazip.com"
  do
    ip="$(curl -4fsS --max-time 15 "$service" 2>/dev/null | tr -d '[:space:]' || true)"
    if [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
      printf '%s\n' "$ip"
      return
    fi
  done

  fail "failed to detect public IPv4; set IP_CERTD_IP and retry"
}

json_error_message() {
  local file="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$file" <<'PY' 2>/dev/null || true
import json, sys
try:
    data = json.load(open(sys.argv[1], "rb"))
    if isinstance(data, dict):
        print(data.get("error") or data.get("message") or "")
except Exception:
    pass
PY
  fi
}

header_value() {
  local file="$1"
  local name="$2"
  awk -v name="$name" 'BEGIN{IGNORECASE=1} index($0, name ":")==1 {sub("^[^:]+:[[:space:]]*", ""); gsub("\r", ""); print; exit}' "$file"
}

nginx_binary() {
  if [[ -x /www/server/nginx/sbin/nginx ]]; then
    printf '%s\n' /www/server/nginx/sbin/nginx
  elif command -v nginx >/dev/null 2>&1; then
    command -v nginx
  fi
}

reload_nginx_if_available() {
  [[ "${IP_CERTD_RELOAD_NGINX:-1}" != "0" ]] || return 0

  local nginx_bin
  nginx_bin="$(nginx_binary || true)"
  if [[ -z "$nginx_bin" ]]; then
    printf 'nginx: not found, skip reload\n' >&2
    return 0
  fi

  "$nginx_bin" -t >/dev/null || fail "nginx configuration test failed after certificate install"

  if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet nginx 2>/dev/null; then
    systemctl reload nginx || fail "failed to reload nginx via systemctl"
  elif [[ -x /etc/init.d/nginx ]]; then
    /etc/init.d/nginx reload || fail "failed to reload nginx via init script"
  else
    "$nginx_bin" -s reload || fail "failed to reload nginx"
  fi
  printf 'nginx: reloaded\n'
}

verify_certificate_files() {
  local dir="$1"
  local hostname="$2"
  local required file

  for required in fullchain.pem privkey.pem cert.pem chain.pem metadata.json; do
    file="$dir/$required"
    [[ -s "$file" ]] || fail "bundle missing or empty file: $required"
  done

  if command -v openssl >/dev/null 2>&1; then
    openssl x509 -in "$dir/cert.pem" -noout >/dev/null 2>&1 || fail "cert.pem is not a valid certificate"
    openssl pkey -in "$dir/privkey.pem" -noout >/dev/null 2>&1 || fail "privkey.pem is not a valid private key"
    openssl x509 -in "$dir/cert.pem" -noout -text | grep -Fq "DNS:$hostname" \
      || fail "certificate SAN does not contain DNS:$hostname"

    local cert_pub key_pub
    cert_pub="$(mktemp)"
    key_pub="$(mktemp)"
    trap 'rm -f "$cert_pub" "$key_pub"' RETURN
    openssl x509 -in "$dir/cert.pem" -pubkey -noout >"$cert_pub"
    openssl pkey -in "$dir/privkey.pem" -pubout >"$key_pub"
    cmp -s "$cert_pub" "$key_pub" || fail "certificate and private key do not match"
    rm -f "$cert_pub" "$key_pub"
    trap - RETURN
  fi
}

main() {
  if [[ "$#" -ne 1 ]]; then
    usage
    exit 2
  fi

  need_cmd curl
  need_cmd tar
  need_cmd mktemp

  local api_base domain ip hostname url install_root install_dir tmp_dir bundle headers body status
  api_base="$(normalize_api_base "$1")"
  domain="$(host_from_url "$api_base")"
  ip="$(detect_public_ipv4)"
  hostname="$ip.$domain"
  url="$api_base/v1/certificates/$ip/bundle"
  install_root="${IP_CERTD_INSTALL_ROOT:-/etc/nginx/ssl}"
  install_dir="$install_root/$hostname"

  tmp_dir="$(mktemp -d)"
  IP_CERTD_TMP_DIR="$tmp_dir"
  bundle="$tmp_dir/bundle.tar.gz"
  headers="$tmp_dir/headers.txt"
  body="$tmp_dir/error-body.txt"
  trap cleanup EXIT

  printf 'request: %s\n' "$url"
  status="$(
    curl -sS -L -X POST \
      -H 'Accept: application/gzip' \
      -D "$headers" \
      -o "$bundle" \
      -w '%{http_code}' \
      --max-time 480 \
      "$url" || true
  )"

  if [[ "$status" != "200" ]]; then
    mv "$bundle" "$body" 2>/dev/null || true
    local message
    message="$(json_error_message "$body")"
    if [[ -z "$message" && -s "$body" ]]; then
      message="$(head -c 300 "$body" | tr '\r\n' ' ')"
    fi
    [[ -n "$message" ]] || message="request failed"
    fail "certificate request failed: HTTP $status: $message"
  fi

  mkdir -p "$install_dir" || fail "failed to create install directory: $install_dir"
  tar -xzf "$bundle" -C "$install_dir" || fail "failed to extract certificate bundle"
  chmod 600 "$install_dir/privkey.pem" || fail "failed to chmod privkey.pem"
  chmod 600 "$install_dir/metadata.json" 2>/dev/null || true
  chmod 644 "$install_dir/fullchain.pem" "$install_dir/cert.pem" "$install_dir/chain.pem" 2>/dev/null || true

  verify_certificate_files "$install_dir" "$hostname"

  local response_hostname not_after
  response_hostname="$(header_value "$headers" "X-Certificate-Hostname" || true)"
  not_after="$(header_value "$headers" "X-Certificate-Not-After" || true)"
  [[ -z "$response_hostname" || "$response_hostname" == "$hostname" ]] \
    || fail "server returned unexpected hostname: $response_hostname"

  reload_nginx_if_available

  printf 'certificate: installed\n'
  printf 'hostname: %s\n' "$hostname"
  printf 'directory: %s\n' "$install_dir"
  if [[ -n "$not_after" ]]; then
    printf 'not_after: %s\n' "$not_after"
  fi
}

main "$@"
