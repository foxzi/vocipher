# Architecture / Архитектура

## English

### Overview

Vocipher is a self-hosted voice chat server built as a single Go binary. It combines an HTTP server, WebSocket signaling, WebRTC SFU (Selective Forwarding Unit) and an optional embedded TURN server in one process.

### Project Structure

```
vocipher/
├── cmd/server/main.go           # Entry point, HTTP routing, middleware
├── internal/
│   ├── auth/auth.go             # User registration, login, sessions
│   ├── channel/channel.go       # Channel CRUD, in-memory presence tracking
│   ├── database/database.go     # SQLite init and schema migrations
│   ├── signaling/hub.go         # WebSocket hub, message routing
│   ├── turn/server.go           # Embedded TURN server (pion/turn)
│   └── webrtc/sfu.go            # SFU: peer connections, RTP forwarding
├── web/
│   ├── templates/               # Go html/template files
│   │   ├── layout.html          # Base HTML shell (Tailwind, HTMX)
│   │   ├── login.html           # Login page
│   │   ├── register.html        # Registration page
│   │   └── app.html             # Main app with channel list
│   └── static/js/app.js         # Client-side logic (vanilla JS)
├── Dockerfile                   # Multi-stage Docker build
├── docker-compose.yaml          # Docker Compose configuration
└── Makefile                     # Build shortcuts
```

### Request Flow

```
Browser ──HTTPS──> Nginx ──HTTP──> Vocipher HTTP Server (:8090)
   │                                    │
   ├── GET /login, /register            ├── Template rendering
   ├── GET / (requires auth)            ├── requireAuth middleware
   ├── POST /channels (CSRF)            ├── HTMX partial responses
   ├── GET /ws                          ├── WebSocket upgrade
   │                                    │
   └── UDP ──────────────────────> TURN Server (:3478)
```

### WebRTC SFU Architecture

Vocipher uses a Selective Forwarding Unit (SFU) model. Unlike peer-to-peer mesh (where each client sends media to every other client), the SFU receives media from each client and re-broadcasts it to all others. This is significantly more efficient for groups larger than 2-3 people.

```
User A (audio) ──RTP──> SFU ──RTP──> User B (playback)
                             └─RTP──> User C (playback)

User B (audio) ──RTP──> SFU ──RTP──> User A (playback)
                             └─RTP──> User C (playback)
```

**Key properties:**
- No transcoding -- raw RTP packet relay
- Opus codec for audio, VP8/VP9/H.264 for video (screen share)
- PLI (Picture Loss Indication) for keyframe requests on video
- Dynamic renegotiation when peers join/leave or start/stop screen sharing

### WebSocket Signaling Protocol

All real-time communication uses a WebSocket connection to `/ws`. Messages are JSON objects with a `type` field.

**Client to Server:**

| Type | Payload | Description |
|------|---------|-------------|
| `join_channel` | `{channel_id: number}` | Join a voice channel |
| `leave_channel` | -- | Leave current channel |
| `mute` | `{muted: boolean}` | Update mute state |
| `speaking` | `{speaking: boolean}` | Update speaking state (from VAD) |
| `webrtc_offer` | `{sdp: string}` | SDP offer for WebRTC |
| `webrtc_answer` | `{sdp: string}` | SDP answer (during renegotiation) |
| `ice_candidate` | `{candidate: object}` | ICE candidate exchange |
| `screen_preview` | `{image: string}` | Base64 JPEG thumbnail of screen share |

**Server to Client:**

| Type | Content | Description |
|------|---------|-------------|
| `channel_users` | `{channel_id, users[]}` | User list for a channel |
| `presence` | `{channels: {id: users[]}}` | Full presence snapshot |
| `webrtc_offer` | `{payload: {sdp}}` | Server-initiated renegotiation |
| `webrtc_answer` | `{payload: {sdp}}` | SDP answer from SFU |
| `ice_candidate` | `{payload: {candidate}}` | ICE candidate from SFU |
| `screen_preview` | `{user_id, username, payload: {image}}` | Screen share thumbnail |
| `screen_preview_clear` | -- | Screen share ended |

### Database Schema

SQLite with WAL mode and foreign keys enabled.

```sql
users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
)

channels (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT UNIQUE NOT NULL,
    created_by INTEGER REFERENCES users(id),
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)

sessions (
    token      TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL DEFAULT (datetime('now', '+30 days'))
)
```

### TURN Server

The embedded TURN server uses [pion/turn](https://github.com/pion/turn) and runs inside the same process. It is activated by setting the `VOCIPHER_TURN_IP` environment variable to the server's public IP address.

```
Client behind NAT
    │
    ├── Try direct P2P (STUN) ──> Fails (symmetric NAT)
    │
    └── Fallback to TURN relay
            │
            └── UDP :3478 ──> Vocipher TURN ──> Other peer
```

TURN credentials are generated automatically at startup and injected into the ICE configuration sent to clients via the HTML template.

### Frontend

- **No build step** -- Vanilla JavaScript, Tailwind CSS via CDN, HTMX for server interactions
- **HTMX** -- Used for channel creation/deletion (POST with partial HTML swap)
- **WebSocket** -- Raw JS `WebSocket` for real-time signaling
- **WebRTC** -- Browser `RTCPeerConnection` API for audio/video
- **VAD** -- Web Audio API `AnalyserNode` for voice activity detection
- **Screen Share** -- `getDisplayMedia()` with periodic JPEG thumbnail preview

---

## Русский

### Обзор

Vocipher -- self-hosted голосовой чат-сервер, собранный в один бинарный файл Go. Объединяет HTTP-сервер, WebSocket-сигнализацию, WebRTC SFU и опциональный встроенный TURN-сервер в одном процессе.

### Структура проекта

```
vocipher/
├── cmd/server/main.go           # Точка входа, HTTP роутинг, middleware
├── internal/
│   ├── auth/auth.go             # Регистрация, логин, сессии
│   ├── channel/channel.go       # CRUD каналов, in-memory отслеживание присутствия
│   ├── database/database.go     # Инициализация SQLite и миграции схемы
│   ├── signaling/hub.go         # WebSocket хаб, маршрутизация сообщений
│   ├── turn/server.go           # Встроенный TURN-сервер (pion/turn)
│   └── webrtc/sfu.go            # SFU: пир-соединения, пересылка RTP
├── web/
│   ├── templates/               # Go html/template файлы
│   └── static/js/app.js         # Клиентская логика (vanilla JS)
├── Dockerfile                   # Многоэтапная Docker-сборка
├── docker-compose.yaml          # Конфигурация Docker Compose
└── Makefile                     # Команды сборки
```

### Архитектура WebRTC SFU

Vocipher использует модель SFU (Selective Forwarding Unit). В отличие от P2P mesh (где каждый клиент отправляет медиа каждому), SFU получает медиа от каждого клиента и переправляет остальным. Это значительно эффективнее для групп больше 2-3 человек.

**Ключевые свойства:**
- Без транскодирования -- пересылка сырых RTP-пакетов
- Opus кодек для аудио, VP8/VP9/H.264 для видео (демонстрация экрана)
- Динамическая ренеготиация при подключении/отключении участников

### Протокол WebSocket-сигнализации

Вся real-time коммуникация идёт через WebSocket на `/ws`. Сообщения -- JSON-объекты с полем `type`. Полный список типов сообщений см. в английской версии выше.

### Схема базы данных

SQLite с WAL-режимом и включёнными foreign keys. Три таблицы: `users`, `channels`, `sessions`. Сессии имеют автоматическое истечение через 30 дней.

### TURN-сервер

Встроенный TURN-сервер на базе [pion/turn](https://github.com/pion/turn) запускается в том же процессе. Активируется переменной `VOCIPHER_TURN_IP`. Credentials генерируются автоматически при запуске.

### Фронтенд

- Без шага сборки -- vanilla JS, Tailwind CSS через CDN, HTMX
- WebSocket для real-time сигнализации
- WebRTC `RTCPeerConnection` для аудио/видео
- VAD через Web Audio API
- Демонстрация экрана через `getDisplayMedia()`
