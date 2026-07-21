#!/usr/bin/env bash
set -Eeuo pipefail
[[ $EUID -eq 0 ]] || { echo "root olarak çalıştır."; exit 1; }
read -r -p "UserrTM Gateway kaldırılsın mı? [y/N]: " A
[[ "$A" =~ ^[Yy]$ ]] || exit 0
systemctl disable --now userrtm-gateway 2>/dev/null || true
rm -f /etc/systemd/system/userrtm-gateway.service
rm -f /etc/nginx/conf.d/userrtm-gateway.conf
rm -rf /opt/userrtm-gateway
systemctl daemon-reload
read -r -p "Veritabanı ve ayarlar da silinsin mi? [y/N]: " B
[[ "$B" =~ ^[Yy]$ ]] && rm -rf /etc/userrtm-gateway
nginx -t && systemctl reload nginx || true
echo "Kaldırıldı. Let's Encrypt sertifikasına dokunulmadı."
