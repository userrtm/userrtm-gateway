#!/usr/bin/env bash
set -Eeuo pipefail
APP="userrtm-gateway"
REPO="${USERRTM_REPOSITORY:-UserrTM/userrtm-gateway}"
BRANCH="${USERRTM_BRANCH:-main}"
[[ $EUID -eq 0 ]] || { echo "root olarak çalıştır."; exit 1; }
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fL --retry 3 "https://github.com/${REPO}/archive/refs/heads/${BRANCH}.tar.gz" -o "$TMP/source.tar.gz"
tar -xzf "$TMP/source.tar.gz" -C "$TMP"
SRC="$(find "$TMP" -mindepth 1 -maxdepth 1 -type d | head -n1)"
systemctl stop "$APP"
cp "$SRC/main.go" "$SRC/go.mod" /opt/userrtm-gateway/
[[ -f "$SRC/go.sum" ]] && cp "$SRC/go.sum" /opt/userrtm-gateway/
cp "$SRC/templates/"*.html /opt/userrtm-gateway/templates/
cp "$SRC/static/"* /opt/userrtm-gateway/static/
cp "$SRC/userrtm-gateway.service" /etc/systemd/system/userrtm-gateway.service
cd /opt/userrtm-gateway
unset GOFLAGS || true
go mod tidy
go build -mod=mod -trimpath -ldflags="-s -w" -o userrtm-gateway .
chmod 0755 userrtm-gateway
systemctl daemon-reload
systemctl enable --now "$APP"
systemctl status "$APP" --no-pager
