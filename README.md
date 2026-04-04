# Vocala (Fork)

> This is a fork of [kidandcat/vocala](https://github.com/kidandcat/vocala) -- a self-hosted voice chat server.
> The original project provides a lightweight, single-binary alternative to Discord focused on voice communication.
> This fork adds security hardening, admin panel, embedded TURN server, Docker + Nginx HTTPS support, mobile layout and documentation.

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
- Activated by setting `VOCALA_TURN_IP` environment variable

### WebRTC Improvements

- **Interceptor registry** -- Proper RTP extension handling for Chrome simulcast support
- **Automatic PLI** -- Periodic keyframe requests via `intervalpli` interceptor for reliable video
- **NAT 1:1 mapping** -- `VOCALA_NAT_IP` for Docker deployments (ICE candidates advertise host IP)
- **Serialized renegotiation** -- Per-peer mutex prevents concurrent offer/answer conflicts
- **Screen share fixes** -- Video container survives renegotiation, stream updated in place

### Voice Activity Detection

- **GainNode-based VAD** -- Audio routed through Web Audio API GainNode instead of track.enabled, fixing mobile audio
- **Threshold marker** -- Visual marker on level meter showing the activation threshold
- **Speaking indicators** -- Green ring + animated bars on your avatar when speaking; green glow + "Speaking" label on other users' cards
- **Sensitivity slider** -- Adjustable VAD threshold (1-60), lower = more sensitive

### Infrastructure

- **Docker + Nginx HTTPS** -- Multi-stage Dockerfile, Nginx reverse proxy with self-signed certificate
- **Self-signed cert generator** -- `nginx/generate-cert.sh` auto-detects local IP for SAN
- **`.env` configuration** -- `VOCALA_NAT_IP` via `.env` file for Docker deployments
- **UDP port range** -- Ports 50000-50100 exposed for WebRTC media in Docker
- **Graceful shutdown** -- Signal handling (SIGINT/SIGTERM) with 10-second timeout
- **Server timeouts** -- Read (15s), Write (30s), Idle (120s) on HTTP server

### Mobile UI

- **Responsive sidebar** -- Collapsible sidebar on mobile with hamburger menu
- **Compact toolbar** -- Icon-only buttons on mobile, sensitivity slider on second row
- **Auto-close** -- Sidebar closes when joining a channel on mobile
- **Mobile header** -- Shows current channel name

### Code Quality

- Removed duplicate `sdpHasVideoSending` function (was in both signaling and webrtc packages)
- `cleanupWebRTC` no longer creates unnecessary SFU instances
- User passed via `context.Context` instead of double DB queries
- Cookie helper function for consistent session cookie creation

## Features

- **WebRTC SFU** -- Low-latency voice powered by [Pion WebRTC](https://github.com/pion/webrtc)
- **Built-in TURN server** -- Embedded [Pion TURN](https://github.com/pion/turn) for NAT traversal
- **Voice Activity Detection** -- GainNode-based VAD with visual level meter and threshold marker
- **Push-to-Talk** -- Optional PTT mode activated with spacebar
- **Screen Sharing** -- Share your screen with live preview thumbnails (Firefox + Chrome)
- **Channels** -- Voice channels with real-time presence and user counts
- **Admin Panel** -- User management with manual activation
- **Authentication** -- bcrypt + session cookies + CSRF protection
- **Single Binary** -- SQLite database, embedded TURN, one process to run
- **Mobile Responsive** -- Collapsible sidebar, compact controls on small screens
- **Modern UI** -- Dark-themed interface built with HTMX and Tailwind CSS

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

# Generate self-signed certificate
./nginx/generate-cert.sh ./nginx/certs

# Configure host IP for WebRTC
cp .env.example .env
# Edit .env and set VOCALA_NAT_IP to your machine's IP

# Start
docker compose up -d
```

Access at `https://<your-ip>`. The first registered user becomes the admin.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `VOCALA_ADDR` | `:8090` | HTTP listen address |
| `VOCALA_DB_PATH` | `vocala.db` | Path to SQLite database file |
| `VOCALA_TURN_IP` | *(disabled)* | Public IP for built-in TURN server |
| `VOCALA_NAT_IP` | *(disabled)* | Host IP for WebRTC ICE candidates (required in Docker) |

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

Original project by [kidandcat](https://github.com/kidandcat/vocala).
