# Vocipher (Fork)

> This is a fork of [kidandcat/vocipher](https://github.com/kidandcat/vocipher) -- a self-hosted voice chat server.
> The original project provides a lightweight, single-binary alternative to Discord focused on voice communication.
> This fork adds security hardening, admin panel, embedded TURN server, Docker support and documentation.

## What's Changed in This Fork

### Security Fixes

- **CSWSH protection** -- WebSocket Origin header validation against request Host
- **XSS prevention** -- All user-controlled strings escaped in client-side JavaScript
- **CSRF protection** -- Token-based CSRF on all POST endpoints (login, register, channels, admin)
- **Session expiry** -- Sessions expire after 30 days with automatic hourly cleanup
- **Rate limiting** -- IP-based rate limiter (10 req/s, burst 20) on all HTTP endpoints
- **WebSocket hardening** -- 512 KB message size limit, duplicate connection handling
- **HTTP security headers** -- X-Content-Type-Options, X-Frame-Options, Referrer-Policy, Permissions-Policy
- **Method checking** -- All routes enforce correct HTTP methods
- **Error handling** -- Errors from DB queries and template execution are logged, not silently ignored
- **Channel authorization** -- Only the creator (or admin) can delete a channel
- **Password policy** -- Minimum password length increased to 8 characters

### Admin Panel

- **User activation** -- New users are created with Pending status and must be activated by an admin
- **First user auto-admin** -- The first registered user is automatically admin and active
- **Admin UI** at `/admin` -- Table of all users with activate, deactivate, make-admin, revoke-admin, delete actions
- **Deactivation** -- Deactivating a user removes all their sessions (immediate logout)
- **Self-protection** -- Admins cannot modify their own account
- **Gear icon** -- Admin panel link visible only to admin users in the sidebar

### Embedded TURN Server

- **Built-in TURN** using [Pion TURN](https://github.com/pion/turn), runs in the same process
- **Auto-generated credentials** -- Random secret generated at startup, no manual configuration
- **Server-to-client config** -- ICE servers (STUN + TURN) injected into the page via template
- Activated by setting `VOCIPHER_TURN_IP` environment variable

### Infrastructure

- **Docker** -- Multi-stage Dockerfile, docker-compose.yaml with named volume for DB persistence
- **Graceful shutdown** -- Signal handling (SIGINT/SIGTERM) with 10-second timeout
- **Configurable** -- `VOCIPHER_ADDR`, `VOCIPHER_DB_PATH`, `VOCIPHER_TURN_IP` environment variables
- **Server timeouts** -- Read (15s), Write (30s), Idle (120s) timeouts on HTTP server

### Code Quality

- Removed duplicate `sdpHasVideoSending` function (was in both signaling and webrtc packages)
- `cleanupWebRTC` no longer creates unnecessary SFU instances
- User passed via `context.Context` instead of double DB queries
- Cookie helper function for consistent session cookie creation

## Features

- **WebRTC SFU** -- Low-latency voice powered by [Pion WebRTC](https://github.com/pion/webrtc)
- **Built-in TURN server** -- Embedded [Pion TURN](https://github.com/pion/turn) for NAT traversal
- **Voice Activity Detection** -- Real-time VAD with visual audio level meter
- **Push-to-Talk** -- Optional PTT mode activated with spacebar
- **Screen Sharing** -- Share your screen with live preview thumbnails
- **Channels** -- Voice channels with real-time presence and user counts
- **Admin Panel** -- User management with manual activation
- **Authentication** -- bcrypt + session cookies + CSRF protection
- **Single Binary** -- SQLite database, embedded TURN, one process to run
- **Modern UI** -- Dark-themed interface built with HTMX and Tailwind CSS

## Quick Start

### Binary

```bash
git clone https://github.com/foxzi/vocipher.git
cd vocipher
make build
./vocipher
```

### Docker

```bash
docker compose up -d
```

The server starts at `http://localhost:8090`. The first registered user becomes the admin.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `VOCIPHER_ADDR` | `:8090` | HTTP listen address |
| `VOCIPHER_DB_PATH` | `vocipher.db` | Path to SQLite database file |
| `VOCIPHER_TURN_IP` | *(disabled)* | Public IP for built-in TURN server |

See [docs/configuration.md](docs/configuration.md) for details.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/architecture.md) | System design, SFU, WebSocket protocol, database schema |
| [Configuration](docs/configuration.md) | Environment variables and tuning |
| [Deployment](docs/deployment.md) | Docker, Nginx, HTTPS, TURN setup |
| [Security](docs/security.md) | Authentication, CSRF, rate limiting, hardening |

## Stack

| Component | Technology |
|-----------|------------|
| Backend | Go |
| WebRTC | [Pion WebRTC](https://github.com/pion/webrtc) |
| TURN | [Pion TURN](https://github.com/pion/turn) (embedded) |
| Database | SQLite (WAL mode) |
| WebSocket | [Gorilla WebSocket](https://github.com/gorilla/websocket) |
| Frontend | HTMX + Tailwind CSS + Vanilla JS |
| Auth | bcrypt + session cookies |

## Development

```bash
make run    # Compile and run
make build  # Build binary
make clean  # Remove binary and database files
```

## License

MIT

## Credits

Original project by [kidandcat](https://github.com/kidandcat/vocipher).
