# Deployment / Развёртывание

## English

### Local Development

```bash
# Build and run
make run

# Or build separately
make build
./vocipher
```

Server starts at `http://localhost:8090`.

### Docker

```bash
# Start
docker compose up -d

# View logs
docker compose logs -f

# Stop
docker compose down
```

The database is stored on a Docker named volume (`vocipher-data`), so data survives container restarts.

### Docker with TURN

Edit `docker-compose.yaml` and uncomment the `VOCIPHER_TURN_IP` line with your server's public IP:

```yaml
environment:
  - VOCIPHER_DB_PATH=/app/data/vocipher.db
  - VOCIPHER_ADDR=:8090
  - VOCIPHER_TURN_IP=203.0.113.1  # your public IP
```

Make sure UDP port 3478 is open in your firewall.

### Production with Nginx + HTTPS

For production, Nginx handles TLS termination and proxies to Vocipher.

**Network diagram:**

```
Internet
    │
    ├── TCP 443 (HTTPS) ──> Nginx ──> Vocipher :8090
    ├── TCP 80  (HTTP)  ──> Nginx (redirect to HTTPS)
    │
    └── UDP 3478 (TURN) ──> Vocipher TURN (direct, bypasses Nginx)
```

**Important:** Nginx does NOT proxy TURN/UDP traffic. TURN works directly between clients and the Vocipher process.

**Nginx configuration example:**

```nginx
server {
    listen 80;
    server_name voice.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name voice.example.com;

    ssl_certificate     /etc/letsencrypt/live/voice.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/voice.example.com/privkey.pem;

    # Security headers
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

    # Proxy to Vocipher
    location / {
        proxy_pass http://127.0.0.1:8090;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # WebSocket
    location /ws {
        proxy_pass http://127.0.0.1:8090/ws;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }

    # Static files (optional: serve directly for performance)
    location /static/ {
        proxy_pass http://127.0.0.1:8090/static/;
        expires 7d;
        add_header Cache-Control "public, immutable";
    }
}
```

**Let's Encrypt setup:**

```bash
sudo apt install certbot python3-certbot-nginx
sudo certbot --nginx -d voice.example.com
```

**Vocipher configuration for Nginx:**

```bash
VOCIPHER_ADDR=127.0.0.1:8090 \
VOCIPHER_TURN_IP=203.0.113.1 \
./vocipher
```

Bind to `127.0.0.1` so Vocipher is only accessible through Nginx, not directly from the internet.

### Firewall Rules

| Port | Protocol | Service | Access |
|------|----------|---------|--------|
| 80 | TCP | HTTP (Nginx) | Public (redirects to HTTPS) |
| 443 | TCP | HTTPS (Nginx) | Public |
| 3478 | UDP | TURN | Public |
| 8090 | TCP | Vocipher HTTP | Localhost only (behind Nginx) |

```bash
# UFW example
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 3478/udp
```

### Docker Compose with Nginx

If you want Nginx in the same Docker Compose, you can add it as a service. However, the simpler approach is to run Nginx on the host and proxy to the container.

### Ports Summary

| Component | Default Port | Protocol | Configurable |
|-----------|-------------|----------|--------------|
| HTTP server | 8090 | TCP | `VOCIPHER_ADDR` |
| TURN server | 3478 | UDP | Not yet (hardcoded) |

### Health Check

Vocipher does not have a dedicated health endpoint. You can check the login page:

```bash
curl -o /dev/null -w "%{http_code}" http://localhost:8090/login
# Expected: 200
```

---

## Русский

### Локальная разработка

```bash
make run    # Сборка и запуск
make build  # Только сборка
./vocipher  # Запуск
```

Сервер стартует на `http://localhost:8090`.

### Docker

```bash
docker compose up -d     # Запуск
docker compose logs -f   # Логи
docker compose down      # Остановка
```

База данных хранится в Docker named volume, данные сохраняются при перезапуске контейнера.

### Docker с TURN

В `docker-compose.yaml` раскомментируйте строку `VOCIPHER_TURN_IP`, указав публичный IP сервера. Убедитесь, что UDP порт 3478 открыт в файрволе.

### Продакшен с Nginx + HTTPS

В продакшене Nginx выполняет TLS-терминацию и проксирует трафик к Vocipher.

**Схема сети:**

```
Интернет
    │
    ├── TCP 443 (HTTPS) ──> Nginx ──> Vocipher :8090
    ├── TCP 80  (HTTP)  ──> Nginx (редирект на HTTPS)
    │
    └── UDP 3478 (TURN) ──> Vocipher TURN (напрямую, минуя Nginx)
```

**Важно:** Nginx НЕ проксирует TURN/UDP трафик. TURN работает напрямую между клиентами и Vocipher.

Пример конфигурации Nginx и настройки Let's Encrypt см. в английской версии выше.

**Конфигурация Vocipher за Nginx:**

```bash
VOCIPHER_ADDR=127.0.0.1:8090 \
VOCIPHER_TURN_IP=203.0.113.1 \
./vocipher
```

Привязка к `127.0.0.1` делает Vocipher доступным только через Nginx.

### Правила файрвола

| Порт | Протокол | Сервис | Доступ |
|------|----------|--------|--------|
| 80 | TCP | HTTP (Nginx) | Публичный (редирект на HTTPS) |
| 443 | TCP | HTTPS (Nginx) | Публичный |
| 3478 | UDP | TURN | Публичный |
| 8090 | TCP | Vocipher HTTP | Только localhost (за Nginx) |

### Проверка работоспособности

```bash
curl -o /dev/null -w "%{http_code}" http://localhost:8090/login
# Ожидается: 200
```
