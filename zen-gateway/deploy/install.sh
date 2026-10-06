#!/usr/bin/env bash
# Install / refresh zen-gateway on the server.
# Run as a user with sudo. Idempotent: safe to re-run after a code update.
#
#   sudo bash install.sh /path/to/zen-gateway-source
#
# It does NOT touch /opt/wb2api, /etc/wb2api or the wb2api service.
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
APP_DIR=/opt/zen-gateway
ETC_DIR=/etc/zen-gateway
LIB_DIR=/var/lib/zen-gateway
NODE_BIN=/opt/skilldesk-platform/runtime/node/bin/node
UNIT=/etc/systemd/system/zen-gateway.service

[[ $EUID -eq 0 ]] || { echo "run with sudo" >&2; exit 1; }
[[ -f "$SRC/src/server.mjs" ]] || { echo "source not found at $SRC" >&2; exit 1; }

echo "== user + dirs =="
id -u zen-gateway >/dev/null 2>&1 || useradd --system --home "$LIB_DIR" --shell /usr/sbin/nologin zen-gateway
install -d -m 0755 "$APP_DIR" "$ETC_DIR"
install -d -m 0700 -o zen-gateway -g zen-gateway "$LIB_DIR"

echo "== code -> $APP_DIR/current =="
install -d -m 0755 "$APP_DIR/current"
cp -r "$SRC/src" "$APP_DIR/current/"
cp "$SRC/package.json" "$APP_DIR/current/"
cp -r "$SRC/deploy" "$APP_DIR/current/" 2>/dev/null || true
chown -R root:root "$APP_DIR/current"
chmod -R a-w "$APP_DIR/current"

echo "== node runtime =="
if [[ ! -x "$NODE_BIN" ]]; then
  echo "WARN: $NODE_BIN not found; falling back to /usr/bin/node" >&2
fi

echo "== config =="
if [[ ! -f "$ETC_DIR/config.json" ]]; then
  PANEL_KEY="$(python3 -c 'import json;print(json.load(open("/etc/wb2api/config.json"))["api_key"])')"
  GW_KEY="zen-gw-$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 32)"
  cat > "$ETC_DIR/config.json" <<JSON
{
  "listen": "127.0.0.1:7864",
  "api_keys": ["$GW_KEY"],
  "wb2api": { "base": "http://127.0.0.1:7863", "api_key": "$PANEL_KEY", "models_ttl_ms": 600000 },
  "zen": {
    "base": "https://opencode.ai/zen",
    "catalog_ttl_ms": 600000,
    "metadata_ttl_ms": 86400000,
    "metadata_cache": "$LIB_DIR/models.dev.json"
  },
  "limits": {
    "first_byte_ms": 30000,
    "body_idle_ms": 120000,
    "responses_body_idle_ms": 300000,
    "max_in_flight_per_model": 3,
    "max_in_flight_global": 8,
    "upstream_timeout_ms": 120000,
    "pre_content_retries": 1
  },
  "logging": {
    "request_log": "$LIB_DIR/requests-%Y-%m-%d.jsonl",
    "log_prompts": false,
    "retention_days": 7
  }
}
JSON
  chown root:zen-gateway "$ETC_DIR/config.json"
  chmod 0640 "$ETC_DIR/config.json"
  echo "generated $ETC_DIR/config.json (gateway key below)"
  echo "GATEWAY_KEY=$GW_KEY"
else
  echo "keeping existing $ETC_DIR/config.json"
fi

echo "== systemd =="
install -m 0644 "$SRC/deploy/zen-gateway.service" "$UNIT"
systemctl daemon-reload
systemctl enable zen-gateway >/dev/null
systemctl restart zen-gateway
sleep 2
systemctl is-active zen-gateway
echo "== local check =="
curl -sS -m 10 http://127.0.0.1:7864/healthz; echo
echo "done. Remember the Caddy route (deploy/caddy.snippet)."
