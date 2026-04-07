# Vocala

Self-hosted voice chat server with WebRTC SFU, text chat, webcam support, and admin panel. A lightweight, single-binary alternative to Discord for teams and communities who want full control.

## Features

- **WebRTC SFU** -- Low-latency voice powered by [Pion WebRTC](https://github.com/pion/webrtc)
- **Webcam support** -- Camera grid with per-user video streams
- **Text chat** -- Persistent messages with emoji reactions in voice channels
- **Screen sharing** -- Share your screen with live preview thumbnails
- **Private channels** -- Invite-only channels with member management and invite links
- **Built-in TURN/TURNS** -- Embedded [Pion TURN](https://github.com/pion/turn) with TLS support
- **Voice Activity Detection** -- GainNode-based VAD with visual level meter
- **Push-to-Talk** -- Optional PTT mode with spacebar
- **Admin Panel** -- User management, activation, password reset
- **Authentication** -- bcrypt + session cookies + CSRF protection
- **Single Binary** -- SQLite database, embedded TURN, one process
- **Mobile Responsive** -- Collapsible sidebar, compact controls
- **Modern UI** -- Dark-themed interface with HTMX and Tailwind CSS
- **deb/rpm packaging** -- systemd service, auto-restart on upgrade

## Quick Start

### Binary

```bash
git clone https://github.com/foxzi/vocala.git
cd vocala
make build
./vocala
```

### Docker with HTTPS

```bash
git clone https://github.com/foxzi/vocala.git
cd vocala
./nginx/generate-cert.sh ./nginx/certs
cp .env.example .env
# Edit .env: set VOCALA_NAT_IP to your machine's IP
docker compose up -d
```

Access at `https://<your-ip>`. The first registered user becomes the admin.

### deb package

```bash
make deb
sudo dpkg -i dist/vocala_*.deb
sudo vim /etc/vocala/config.yaml
sudo systemctl start vocala
```

## Configuration

Vocala is configured via `config.yaml` and/or environment variables (env vars override YAML).

| Variable | Default | Description |
|----------|---------|-------------|
| `VOCALA_ADDR` | `:8090` | HTTP listen address |
| `VOCALA_DB_PATH` | `vocala.db` | SQLite database path |
| `VOCALA_TURN_IP` | *(disabled)* | Public IP for built-in TURN server |
| `VOCALA_NAT_IP` | *(disabled)* | Host IP for WebRTC ICE candidates (required in Docker) |
| `VOCALA_COOKIE_SECURE` | `false` | Set `true` for HTTPS |

See [docs/configuration.md](docs/configuration.md) for full YAML reference.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/architecture.md) | System design, SFU, WebSocket protocol, database schema |
| [Configuration](docs/configuration.md) | YAML config, environment variables |
| [Deployment](docs/deployment.md) | Docker, Nginx, HTTPS, TURN, systemd |
| [Security](docs/security.md) | Authentication, CSRF, private channels, hardening |
| [OAuth](docs/oauth.md) | Google, GitHub, Keycloak, Authentik setup guides |

## Stack

| Component | Technology |
|-----------|------------|
| Backend | Go |
| WebRTC | [Pion WebRTC](https://github.com/pion/webrtc) |
| TURN | [Pion TURN](https://github.com/pion/turn) (embedded) |
| Database | SQLite (WAL mode) |
| WebSocket | [Gorilla WebSocket](https://github.com/gorilla/websocket) |
| Frontend | HTMX + Tailwind CSS + Vanilla JS |
| Auth | bcrypt + session cookies + OAuth2/OIDC |

## Development

```bash
make run    # Compile and run
make build  # Build binary
make clean  # Remove binary and database files
make deb    # Build deb package
make rpm    # Build rpm package
```

## License

MIT

## Acknowledgments

This project is based on [Vocipher](https://github.com/kidandcat/vocipher) by [kidandcat](https://github.com/kidandcat), which provided the original WebRTC SFU voice chat foundation. Thank you for the great starting point.
