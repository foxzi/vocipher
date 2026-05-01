# Deployment / Развёртывание

## English

### Quick Start: deb Package + Nginx

The fastest way to deploy Vocala on a Debian/Ubuntu server.

**1. Install the package:**

```bash
sudo dpkg -i vocala_0.1.0_amd64.deb
```

This installs:
- `/usr/bin/vocala` -- server binary
- `/etc/vocala/config.yaml` -- server config (created from default on first install)
- `/etc/vocala/config.yaml.example` -- full documented config example
- `/etc/vocala/nginx.conf.example` -- Nginx reverse proxy config
- `/usr/lib/systemd/system/vocala.service` -- systemd unit
- `/usr/share/vocala/web/` -- templates and static files
- `/usr/share/doc/vocala/` -- documentation

**2. Edit config:**

```bash
sudo vim /etc/vocala/config.yaml
```

Key settings to change:

```yaml
server:
  addr: "127.0.0.1:8090"    # bind to localhost (Nginx will proxy)

auth:
  cookie_secure: true        # required for HTTPS

turn:
  enabled: true
  ip: "YOUR_PUBLIC_IP"       # your server's public IP
  port: 3478
  tls_port: 5349             # TURNS for mobile clients (optional)
  tls_host: "YOUR_DOMAIN"   # must match TLS certificate
  cert_file: "/etc/letsencrypt/live/YOUR_DOMAIN/fullchain.pem"
  key_file: "/etc/letsencrypt/live/YOUR_DOMAIN/privkey.pem"
```

**3. Set up Nginx:**

```bash
# Install Nginx
sudo apt install nginx

# Copy example config
sudo cp /etc/vocala/nginx.conf.example /etc/nginx/sites-available/vocala.conf

# Edit: replace YOUR_DOMAIN with your domain
sudo sed -i 's/YOUR_DOMAIN/voice.example.com/g' /etc/nginx/sites-available/vocala.conf

# Enable site
sudo ln -s /etc/nginx/sites-available/vocala.conf /etc/nginx/sites-enabled/

# Get TLS certificate
sudo apt install certbot python3-certbot-nginx
sudo certbot --nginx -d voice.example.com

# Test and reload
sudo nginx -t && sudo systemctl reload nginx
```

**4. Open firewall:**

```bash
sudo ufw allow 80/tcp       # HTTP (Let's Encrypt + redirect)
sudo ufw allow 443/tcp      # HTTPS
sudo ufw allow 3478/udp     # TURN
sudo ufw allow 5349/tcp     # TURNS (optional, for mobile)
sudo ufw allow 40000:40200/udp  # WebRTC media
```

**5. Start Vocala:**

```bash
sudo systemctl start vocala
sudo systemctl status vocala

# Check version
vocala -version
```

**6. Register first user:**

Open `https://voice.example.com` in your browser. The first registered user automatically becomes the admin. If public registration is not desired, set `registration_enabled: false` in `config.yaml` or `VOCALA_REGISTRATION=false` after bootstrapping the initial admin account.

### Network Diagram

```
Internet
    |
    +-- TCP 443 (HTTPS) --> Nginx --> Vocala :8090
    +-- TCP 80  (HTTP)  --> Nginx (redirect to HTTPS)
    |
    +-- UDP 3478 (TURN)        --> Vocala TURN (direct)
    +-- TCP 5349 (TURNS/TLS)   --> Vocala TURNS (direct)
    +-- UDP 40000-40200 (RTP)  --> Vocala SFU (direct)
```

Nginx proxies HTTP/WebSocket only. TURN, TURNS, and WebRTC media traffic go directly to the Vocala process.

### Package Contents

| Path | Description |
|------|-------------|
| `/usr/bin/vocala` | Server binary |
| `/etc/vocala/config.yaml` | Active config (not overwritten on upgrade) |
| `/etc/vocala/config.yaml.default` | Default config template |
| `/etc/vocala/config.yaml.example` | Full documented example |
| `/etc/vocala/nginx.conf.example` | Nginx reverse proxy config |
| `/usr/lib/systemd/system/vocala.service` | Systemd service unit |
| `/usr/share/vocala/web/` | Templates and static files |
| `/usr/share/doc/vocala/` | Documentation (architecture, config, deployment, security) |
| `/var/lib/vocala/` | Data directory (SQLite database) |

### Upgrading

```bash
sudo dpkg -i vocala_NEW_VERSION_amd64.deb
# Service restarts automatically
```

Config at `/etc/vocala/config.yaml` is preserved on upgrade.

### Local Development

```bash
make run                    # build and run
make build                  # build binary only
./vocala -version           # check version
./vocala -config my.yaml    # custom config
```

### Docker

```bash
cp .env.example .env
# Edit .env: set VOCALA_NAT_IP to your host IP

# With self-signed cert:
./nginx/generate-cert.sh ./nginx/certs

docker compose up -d
# Access at https://<your-ip>
```

### Firewall Rules

| Port | Protocol | Service | Access |
|------|----------|---------|--------|
| 80 | TCP | HTTP (Nginx) | Public (redirect to HTTPS) |
| 443 | TCP | HTTPS (Nginx) | Public |
| 3478 | UDP | TURN | Public |
| 5349 | TCP | TURNS (TLS) | Public (optional, for mobile) |
| 40000-40200 | UDP | WebRTC media (RTP) | Public |
| 8090 | TCP | Vocala HTTP | Localhost only (behind Nginx) |

### Health Check

```bash
curl -o /dev/null -w "%{http_code}" http://localhost:8090/login
# Expected: 200
```

---

## Русский

### Быстрый старт: deb-пакет + Nginx

Самый быстрый способ развернуть Vocala на Debian/Ubuntu сервере.

**1. Установка пакета:**

```bash
sudo dpkg -i vocala_0.1.0_amd64.deb
```

Устанавливается:
- `/usr/bin/vocala` -- бинарник сервера
- `/etc/vocala/config.yaml` -- конфиг (создаётся при первой установке)
- `/etc/vocala/nginx.conf.example` -- пример конфига Nginx
- `/usr/lib/systemd/system/vocala.service` -- systemd юнит
- `/usr/share/vocala/web/` -- шаблоны и статика
- `/usr/share/doc/vocala/` -- документация

**2. Настройка конфига:**

```bash
sudo vim /etc/vocala/config.yaml
```

Ключевые настройки:

```yaml
server:
  addr: "127.0.0.1:8090"    # только localhost (Nginx проксирует)

auth:
  cookie_secure: true        # обязательно для HTTPS

turn:
  enabled: true
  ip: "ВАШ_ПУБЛИЧНЫЙ_IP"
  tls_host: "ВАШ_ДОМЕН"     # должен совпадать с TLS-сертификатом
  cert_file: "/etc/letsencrypt/live/ВАШ_ДОМЕН/fullchain.pem"
  key_file: "/etc/letsencrypt/live/ВАШ_ДОМЕН/privkey.pem"
```

**3. Настройка Nginx:**

```bash
# Установка Nginx
sudo apt install nginx

# Копирование примера конфига
sudo cp /etc/vocala/nginx.conf.example /etc/nginx/sites-available/vocala.conf

# Замена YOUR_DOMAIN на ваш домен
sudo sed -i 's/YOUR_DOMAIN/voice.example.com/g' /etc/nginx/sites-available/vocala.conf

# Включение сайта
sudo ln -s /etc/nginx/sites-available/vocala.conf /etc/nginx/sites-enabled/

# Получение TLS-сертификата
sudo apt install certbot python3-certbot-nginx
sudo certbot --nginx -d voice.example.com

# Проверка и перезагрузка
sudo nginx -t && sudo systemctl reload nginx
```

**4. Файрвол:**

```bash
sudo ufw allow 80/tcp       # HTTP
sudo ufw allow 443/tcp      # HTTPS
sudo ufw allow 3478/udp     # TURN
sudo ufw allow 5349/tcp     # TURNS (опционально, для мобильных)
sudo ufw allow 40000:40200/udp  # WebRTC media
```

**5. Запуск:**

```bash
sudo systemctl start vocala
sudo systemctl status vocala
vocala -version
```

**6. Регистрация:** откройте `https://voice.example.com` в браузере. Первый пользователь автоматически становится админом. Если публичная регистрация не нужна, после создания первого администратора установите `registration_enabled: false` в `config.yaml` или `VOCALA_REGISTRATION=false`.

### Обновление

```bash
sudo dpkg -i vocala_НОВАЯ_ВЕРСИЯ_amd64.deb
# Сервис перезапускается автоматически
```

Конфиг `/etc/vocala/config.yaml` сохраняется при обновлении.

### Схема сети

```
Интернет
    |
    +-- TCP 443 (HTTPS) --> Nginx --> Vocala :8090
    +-- TCP 80  (HTTP)  --> Nginx (редирект на HTTPS)
    |
    +-- UDP 3478 (TURN)        --> Vocala TURN (напрямую)
    +-- TCP 5349 (TURNS/TLS)   --> Vocala TURNS (напрямую)
    +-- UDP 40000-40200 (RTP)  --> Vocala SFU (напрямую)
```

Nginx проксирует только HTTP/WebSocket. TURN, TURNS и WebRTC media идут напрямую к процессу Vocala.

### Содержимое пакета

| Путь | Описание |
|------|----------|
| `/usr/bin/vocala` | Бинарник сервера |
| `/etc/vocala/config.yaml` | Активный конфиг (не перезаписывается при обновлении) |
| `/etc/vocala/nginx.conf.example` | Пример конфига Nginx |
| `/usr/lib/systemd/system/vocala.service` | Systemd юнит |
| `/usr/share/vocala/web/` | Шаблоны и статика |
| `/usr/share/doc/vocala/` | Документация |
| `/var/lib/vocala/` | Директория данных (SQLite) |
