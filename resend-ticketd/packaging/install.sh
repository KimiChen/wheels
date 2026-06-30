#!/usr/bin/env sh
set -eu

SERVICE_USER="resend-ticketd"
CONFIG_DIR="/etc/resend-ticketd"
ACME_DIR="$CONFIG_DIR/acme"
TLS_DIR="$CONFIG_DIR/tls"
DATA_DIR="/var/lib/resend-ticketd"
LOG_DIR="/var/log/resend-ticketd"

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh must be run as root" >&2
  exit 1
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -m 0750 -o root -g "$SERVICE_USER" "$CONFIG_DIR"
install -d -m 0700 -o root -g root "$ACME_DIR"
install -d -m 2750 -o root -g "$SERVICE_USER" "$TLS_DIR"
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"
install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" "$LOG_DIR"
install -d -m 0755 /usr/local/bin

install -m 0755 resend-ticketd /usr/local/bin/resend-ticketd

if [ ! -f "$CONFIG_DIR/.env" ]; then
  install -m 0640 -o root -g "$SERVICE_USER" .env.example "$CONFIG_DIR/.env"
  echo "Created $CONFIG_DIR/.env from .env.example; edit it before starting the service."
fi

install -m 0644 packaging/resend-ticketd.service /etc/systemd/system/resend-ticketd.service
install -m 0644 packaging/resend-ticketd-cert-renew.service /etc/systemd/system/resend-ticketd-cert-renew.service
install -m 0644 packaging/resend-ticketd-cert-renew.timer /etc/systemd/system/resend-ticketd-cert-renew.timer

systemctl daemon-reload
/usr/local/bin/resend-ticketd check --config "$CONFIG_DIR/.env"

echo "Enable with: systemctl enable --now resend-ticketd"
echo "Open only the configured HTTPS port, normally 9734/tcp or 443/tcp."
