# UserrTM Gateway

Tek TLS domaini üzerinden WebSocket path'lerine göre birden fazla Marzban/Xray backend'ine yönlendirme yapan hafif Nginx gateway paneli.

## Özellikler

- Backend ekleme, düzenleme, açma/kapatma ve silme
- Toplu JSON import/export
- Nginx önizleme, doğrulama, uygulama ve geri alma
- Domain + TLS modu
- SQLite veri tabanı
- Mobil uyumlu panel
- Otomatik Nginx ve Certbot kurulumu
- Mevcut `/etc/gatewayui/gatewayui.db` verisini taşıma

## Tek komutla kurulum

Önce domain A kaydını gateway VPS IP adresine yönlendir. 80 ve 443 portları açık olmalı.

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/UserrTM/userrtm-gateway/main/install.sh)
```

Kurulum sırasıyla domaini, Let's Encrypt e-postasını, panel kullanıcı adını ve şifreyi sorar. Ardından sertifikayı alır, uygulamayı derler ve systemd servisini başlatır.

## Panel

Varsayılan port:

```text
http://SUNUCU_IP:3389
```

Güvenlik için 3389 portunu sadece yönetici IP adresine aç:

```bash
ufw delete allow 3389/tcp
ufw allow from SENIN_IP_ADRESIN to any port 3389 proto tcp
```

## Kullanım

1. Panelde backend ekle.
2. Backend path'i ile Marzban Core Settings içindeki `wsSettings.path` değerini aynı yap.
3. Nginx Preview ekranını kontrol et.
4. Apply Configuration düğmesine bas.
5. İstemcide domain, TLS 443 ve aynı WebSocket path'i kullan.

## Güncelleme

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/UserrTM/userrtm-gateway/main/upgrade.sh)
```

Veritabanı korunur.

## Servis komutları

```bash
systemctl status userrtm-gateway
journalctl -u userrtm-gateway -f
systemctl restart userrtm-gateway
nginx -t
```

## Güvenlik

- Backend portlarını yalnızca gateway VPS IP'sine aç.
- Panel portunu herkese açık bırakma.
- Gerçek şifreleri, UUID'leri ve sertifika anahtarlarını GitHub'a yükleme.

## Lisans

MIT
