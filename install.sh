#!/usr/bin/env bash
set -Eeuo pipefail

APP="userrtm-gateway"
REPO="${USERRTM_REPOSITORY:-UserrTM/userrtm-gateway}"
BRANCH="${USERRTM_BRANCH:-main}"
APP_DIR="/opt/${APP}"
DATA_DIR="/etc/${APP}"
ENV_FILE="${DATA_DIR}/${APP}.env"
SERVICE_FILE="/etc/systemd/system/${APP}.service"
NGINX_CONFIG="/etc/nginx/conf.d/${APP}.conf"
PANEL_PORT="${USERRTM_PANEL_PORT:-3389}"

ok(){ printf '\033[0;32m%s\033[0m\n' "$*"; }
warn(){ printf '\033[1;33m%s\033[0m\n' "$*"; }
die(){ printf '\033[0;31mHata: %s\033[0m\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "root olarak çalıştır: sudo bash install.sh"
[[ -r /etc/os-release ]] || die "İşletim sistemi algılanamadı."
# shellcheck disable=SC1091
source /etc/os-release
case "${ID:-}" in ubuntu|debian) ;; *) die "Bu sürüm Ubuntu ve Debian içindir." ;; esac

printf '\nUserrTM Gateway kurulumu\n\n'
read -r -p "Domain (örnek: back.example.com): " DOMAIN
DOMAIN="${DOMAIN,,}"; DOMAIN="${DOMAIN#http://}"; DOMAIN="${DOMAIN#https://}"; DOMAIN="${DOMAIN%%/*}"
[[ "$DOMAIN" =~ ^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,63}$ ]] || die "Geçersiz domain."

read -r -p "Let's Encrypt e-posta adresi: " EMAIL
[[ "$EMAIL" =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]] || die "Geçersiz e-posta."

read -r -p "Panel kullanıcı adı [obi]: " ADMIN_USER
ADMIN_USER="${ADMIN_USER:-obi}"
while true; do
  read -r -s -p "Panel şifresi (boş bırakırsan güçlü şifre üretilecek): " ADMIN_PASS; printf '\n'
  if [[ -z "$ADMIN_PASS" ]]; then
    ADMIN_PASS="$(LC_ALL=C tr -dc 'A-Za-z0-9_@#%+=-' </dev/urandom | head -c 20)"
    break
  fi
  [[ ${#ADMIN_PASS} -ge 10 ]] && break
  warn "Şifre en az 10 karakter olmalı."
done

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y nginx certbot curl ca-certificates dnsutils openssl tar gzip git golang-go build-essential
systemctl enable --now nginx

PUBLIC_IP="$(curl -4fsS --max-time 10 https://api.ipify.org || true)"
DNS_IPS="$(getent ahostsv4 "$DOMAIN" | awk '{print $1}' | sort -u | tr '\n' ' ' || true)"
if [[ -n "$PUBLIC_IP" && " $DNS_IPS " != *" $PUBLIC_IP "* ]]; then
  warn "DNS şu adreslere gidiyor: ${DNS_IPS:-sonuç yok}"
  warn "Bu VPS'nin IPv4 adresi: $PUBLIC_IP"
  read -r -p "Yine de devam edilsin mi? [y/N]: " ANSWER
  [[ "$ANSWER" =~ ^[Yy]$ ]] || die "DNS'i düzeltip tekrar çalıştır."
else
  ok "DNS kontrolü başarılı."
fi

mkdir -p /var/www/certbot
cat > "$NGINX_CONFIG" <<CONF
server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};

    location ^~ /.well-known/acme-challenge/ {
        root /var/www/certbot;
    }

    location / {
        return 200 "UserrTM Gateway SSL bootstrap\\n";
        add_header Content-Type text/plain;
    }
}
CONF
rm -f /etc/nginx/sites-enabled/default
nginx -t
systemctl reload nginx

ok "Certbot sertifikası alınıyor..."
certbot certonly --webroot -w /var/www/certbot -d "$DOMAIN" \
  --email "$EMAIL" --agree-tos --non-interactive --keep-until-expiring

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
ok "Kaynak kod indiriliyor..."
curl -fL --retry 3 "https://github.com/${REPO}/archive/refs/heads/${BRANCH}.tar.gz" -o "$TMP/source.tar.gz"
tar -xzf "$TMP/source.tar.gz" -C "$TMP"
SRC="$(find "$TMP" -mindepth 1 -maxdepth 1 -type d | head -n1)"
[[ -f "$SRC/main.go" ]] || die "GitHub kaynağında main.go bulunamadı."

install -d -m 0755 "$APP_DIR/templates" "$APP_DIR/static"
install -d -m 0750 "$DATA_DIR/backups"
cp "$SRC/main.go" "$SRC/go.mod" "$APP_DIR/"
[[ -f "$SRC/go.sum" ]] && cp "$SRC/go.sum" "$APP_DIR/"
cp "$SRC/templates/"*.html "$APP_DIR/templates/"
cp "$SRC/static/"* "$APP_DIR/static/"
cp "$SRC/userrtm-gateway.service" "$SERVICE_FILE"

cd "$APP_DIR"
unset GOFLAGS || true
go mod tidy
go build -mod=mod -trimpath -ldflags="-s -w" -o "$APP" .
chmod 0755 "$APP"

# Eski GatewayUI verisi varsa ilk kurulumda koru.
if [[ ! -f "$DATA_DIR/userrtm-gateway.db" && -f /etc/gatewayui/gatewayui.db ]]; then
  cp /etc/gatewayui/gatewayui.db "$DATA_DIR/userrtm-gateway.db"
  ok "Eski GatewayUI veritabanı taşındı."
fi

cat > "$ENV_FILE" <<ENV
USERRTM_LISTEN=:${PANEL_PORT}
USERRTM_ADMIN_USER=${ADMIN_USER}
USERRTM_ADMIN_PASS=${ADMIN_PASS}
USERRTM_DOMAIN=${DOMAIN}
USERRTM_EMAIL=${EMAIL}
ENV
chmod 0600 "$ENV_FILE"

systemctl daemon-reload
systemctl enable --now "$APP"
sleep 1
systemctl is-active --quiet "$APP" || { journalctl -u "$APP" -n 60 --no-pager; die "Servis başlamadı."; }

# Uygulama domain ayarını kullandığı için ilk Nginx konfigürasyonunu üretmesini beklemeden
# güvenli TLS iskeleti kurulur. Backendler panelden Apply ile eklenecektir.
cat > "$NGINX_CONFIG" <<CONF
map \$http_upgrade \$connection_upgrade {
    default upgrade;
    '' close;
}

server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};

    location ^~ /.well-known/acme-challenge/ {
        root /var/www/certbot;
    }

    location / {
        return 301 https://\$host\$request_uri;
    }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name ${DOMAIN};

    ssl_certificate /etc/letsencrypt/live/${DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${DOMAIN}/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;

    location / {
        return 404;
    }
}
CONF
nginx -t
systemctl reload nginx

printf '\nKurulum tamamlandı.\n'
printf 'Gateway domain : https://%s\n' "$DOMAIN"
printf 'Panel          : http://%s:%s\n' "${PUBLIC_IP:-SUNUCU_IP}" "$PANEL_PORT"
printf 'Kullanıcı      : %s\n' "$ADMIN_USER"
printf 'Şifre          : %s\n\n' "$ADMIN_PASS"
warn "3389 portunu yalnızca kendi IP adresine açman önerilir."
printf 'Panelden backendleri ekle, Preview yap ve Apply Configuration düğmesine bas.\n'
