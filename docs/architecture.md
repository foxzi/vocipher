# Architecture / Архитектура

## English

### Overview

Vocala is a self-hosted voice chat server built as a single Go binary. It combines an HTTP server, WebSocket signaling, WebRTC SFU (Selective Forwarding Unit) and an optional embedded TURN server in one process.

### Project Structure

```
vocala/
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
Browser ──HTTPS──> Nginx ──HTTP──> Vocala HTTP Server (:8090)
   │                                    │
   ├── GET /login, /register            ├── Template rendering
   ├── GET / (requires auth)            ├── requireAuth middleware
   ├── POST /channels (CSRF)            ├── HTMX partial responses
   ├── GET /ws                          ├── WebSocket upgrade
   │                                    │
   └── UDP ──────────────────────> TURN Server (:3478)
```

### WebRTC SFU Architecture

Vocala uses a Selective Forwarding Unit (SFU) model. Unlike peer-to-peer mesh (where each client sends media to every other client), the SFU receives media from each client and re-broadcasts it to all others. This is significantly more efficient for groups larger than 2-3 people.

```
User A (audio) ──RTP──> SFU ──RTP──> User B (playback)
                             └─RTP──> User C (playback)

User B (audio) ──RTP──> SFU ──RTP──> User A (playback)
                             └─RTP──> User C (playback)
```

**Key properties:**
- No transcoding -- raw RTP packet relay
- Opus codec for audio, VP8/VP9/H.264 for video (screen share)
- Interceptor registry for Chrome simulcast RTP extension support
- Automatic PLI (Picture Loss Indication) via `intervalpli` interceptor
- Serialized per-peer renegotiation with stable state waiting
- NAT 1:1 IP mapping for Docker deployments (`VOCALA_NAT_IP`)
- Ephemeral UDP port range 50000-50100 for Docker port mapping

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
| `camera_on` / `camera_off` | -- | Toggle camera track expectation |
| `chat_message` | `{text: string}` | Send text chat message |
| `chat_reaction` | `{message_id, emoji}` | Add emoji reaction to a message |
| `screen_preview` | `{image: string}` | Base64 JPEG thumbnail of screen share |

**Server to Client:**

| Type | Content | Description |
|------|---------|-------------|
| `channel_users` | `{channel_id, users[]}` | User list for a channel |
| `presence` | `{channels: {id: users[]}}` | Full presence snapshot |
| `webrtc_offer` | `{payload: {sdp}}` | Server-initiated renegotiation |
| `webrtc_answer` | `{payload: {sdp}}` | SDP answer from SFU |
| `ice_candidate` | `{payload: {candidate}}` | ICE candidate from SFU |
| `chat_message` | `{id, user_id, username, text, timestamp}` | Chat message broadcast |
| `chat_history` | `{messages[]}` | Last 50 messages on channel join |
| `chat_reaction` | `{message_id, user_id, username, emoji}` | Reaction broadcast |
| `error` | `{error, text}` | Error (e.g. `access_denied`) |
| `screen_preview` | `{user_id, username, payload: {image}}` | Screen share thumbnail |
| `screen_preview_clear` | -- | Screen share ended |

### Database Schema

SQLite with WAL mode and foreign keys enabled.

```sql
users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    is_active     INTEGER NOT NULL DEFAULT 0,
    created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
)

channels (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT UNIQUE NOT NULL,
    created_by INTEGER REFERENCES users(id),
    is_private INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
)

channel_members (
    channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (channel_id, user_id)
)

channel_invites (
    token      TEXT PRIMARY KEY,
    channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    created_by INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    max_uses   INTEGER NOT NULL DEFAULT 0,
    uses       INTEGER NOT NULL DEFAULT 0
)

chat_messages (
    id         TEXT PRIMARY KEY,
    channel_id INTEGER NOT NULL,
    user_id    INTEGER NOT NULL,
    username   TEXT NOT NULL,
    text       TEXT NOT NULL,
    created_at INTEGER NOT NULL
)
-- INDEX idx_chat_messages_channel ON chat_messages(channel_id, created_at)

sessions (
    token      TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    expires_at DATETIME NOT NULL DEFAULT (datetime('now', '+30 days'))
)
```

### Text Chat

Each voice channel has an integrated text chat panel. Messages are relayed via WebSocket and persisted in SQLite (`chat_messages` table).

- On channel join, server sends `chat_history` with the last 50 messages
- New messages are broadcast to all channel participants in real-time
- Emoji reactions (ephemeral, not persisted) can be added to messages
- Old messages are auto-deleted based on `chat_retention_days` config (default 30 days, runs every 6 hours)
- Message length is limited to 2000 characters

### Camera (Webcam)

The SFU supports camera video tracks separate from screen share. Each user can toggle their camera on/off.

- Camera tracks use `streamID = "camera-{userID}"` for stable deduplication during renegotiation
- Camera grid is displayed above user avatars in the channel view
- Client handles `onnegotiationneeded` with debouncing (500ms) to coalesce multiple track additions

### Private Channels

Channels can be created as private (locked). All users see private channels in the sidebar (with a lock icon), but only authorized users can join.

**Access rules:**
- Public channels: any authenticated user can join
- Private channels: only members, the creator, or server admins can join
- Attempting to join without access returns a `{type: "error", error: "access_denied"}` message

**Member management:**
- Creator and server admins can add/remove members
- Members are stored in `channel_members` table
- Creator is automatically added as a member on channel creation

**Invite links:**
- Creator/admins can generate invite links (`/invite/{token}`)
- Links expire after 7 days
- Accepting an invite adds the user as a channel member
- If not logged in, user is redirected to login and then back to the invite

**API endpoints:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/channels/members?id={id}` | GET | List members (JSON) |
| `/channels/members/add` | POST | Add member by username |
| `/channels/members/remove` | POST | Remove member by user_id |
| `/channels/invite` | POST | Generate invite link |
| `/invite/{token}` | GET | Accept invite link |
| `/api/users` | GET | List active users (for member picker) |

### OAuth2 / OpenID Connect

Vocala supports external authentication via OAuth2/OIDC. Configurable providers (Google, GitHub, Keycloak, Authentik, GitLab, etc.) appear as buttons on the login page.

- OAuth flow: redirect to provider -> callback with code -> exchange for token -> fetch userinfo
- Users auto-created on first OAuth login, linked by email to existing accounts
- `auto_activate` option to skip admin approval for trusted providers
- State verification via cookie to prevent CSRF
- See [docs/oauth.md](oauth.md) for detailed setup instructions

### TURN Server

The embedded TURN server uses [pion/turn](https://github.com/pion/turn) and runs inside the same process. It is activated by setting the `VOCALA_TURN_IP` environment variable to the server's public IP address.

```
Client behind NAT
    │
    ├── Try direct P2P (STUN) ──> Fails (symmetric NAT)
    │
    └── Fallback to TURN relay
            │
            └── UDP :3478 ──> Vocala TURN ──> Other peer
```

TURN credentials are generated automatically at startup and injected into the ICE configuration sent to clients via the HTML template.

### Frontend

- **No build step** -- Vanilla JavaScript, Tailwind CSS via CDN, HTMX for server interactions
- **HTMX** -- Used for channel creation/deletion (POST with partial HTML swap)
- **WebSocket** -- Raw JS `WebSocket` for real-time signaling
- **WebRTC** -- Browser `RTCPeerConnection` API for audio/video
- **VAD** -- Web Audio API `AnalyserNode` + `GainNode` for voice activity detection. Audio is routed through GainNode (gain=0 when silent, gain=1 when speaking) instead of disabling the track, which fixes mobile browser compatibility
- **Screen Share** -- `getDisplayMedia()` with periodic JPEG thumbnail preview
- **Mobile responsive** -- Collapsible sidebar, compact icon-only toolbar, auto-close on channel join
- **Speaking indicators** -- Green ring on avatar + animated bars when speaking (self and others)

---

## Русский

### Обзор

Vocala -- self-hosted голосовой чат-сервер, собранный в один бинарный файл Go. Объединяет HTTP-сервер, WebSocket-сигнализацию, WebRTC SFU и опциональный встроенный TURN-сервер в одном процессе.

### Структура проекта

```
vocala/
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

Vocala использует модель SFU (Selective Forwarding Unit). В отличие от P2P mesh (где каждый клиент отправляет медиа каждому), SFU получает медиа от каждого клиента и переправляет остальным. Это значительно эффективнее для групп больше 2-3 человек.

**Ключевые свойства:**
- Без транскодирования -- пересылка сырых RTP-пакетов
- Opus кодек для аудио, VP8/VP9/H.264 для видео (демонстрация экрана)
- Динамическая ренеготиация при подключении/отключении участников

### Протокол WebSocket-сигнализации

Вся real-time коммуникация идёт через WebSocket на `/ws`. Сообщения -- JSON-объекты с полем `type`. Полный список типов сообщений см. в английской версии выше.

### Схема базы данных

SQLite с WAL-режимом и включёнными foreign keys. Три таблицы: `users`, `channels`, `sessions`. Сессии имеют автоматическое истечение через 30 дней.

### TURN-сервер

Встроенный TURN-сервер на базе [pion/turn](https://github.com/pion/turn) запускается в том же процессе. Активируется переменной `VOCALA_TURN_IP`. Credentials генерируются автоматически при запуске.

### Фронтенд

- Без шага сборки -- vanilla JS, Tailwind CSS через CDN, HTMX
- WebSocket для real-time сигнализации
- WebRTC `RTCPeerConnection` для аудио/видео
- VAD через Web Audio API (`AnalyserNode` + `GainNode`) -- управление громкостью вместо отключения трека (фикс мобильных браузеров)
- Демонстрация экрана через `getDisplayMedia()`
- Адаптивная мобильная вёрстка -- сворачиваемый сайдбар, компактная панель управления
- Индикаторы говорящих -- зелёное кольцо + анимированные полоски

### Текстовый чат

Каждый голосовой канал имеет встроенную панель текстового чата. Сообщения пересылаются через WebSocket и сохраняются в SQLite.

- При входе в канал сервер отправляет последние 50 сообщений
- Новые сообщения транслируются всем участникам канала в реальном времени
- Emoji-реакции на сообщения (эфемерные, не сохраняются)
- Автоматическое удаление старых сообщений по настройке `chat_retention_days` (по умолчанию 30 дней)
- Максимальная длина сообщения -- 2000 символов

### Камера (вебкамера)

SFU поддерживает отдельные видеотреки камеры и демонстрации экрана. Каждый пользователь может включать/выключать камеру.

- Треки камеры используют `streamID = "camera-{userID}"` для стабильной дедупликации при ренеготиации
- Сетка камер отображается над аватарами пользователей

### OAuth2 / OpenID Connect

Vocala поддерживает внешнюю авторизацию через OAuth2/OIDC. Настраиваемые провайдеры (Google, GitHub, Keycloak, Authentik, GitLab и др.) отображаются кнопками на странице логина.

- Пользователи создаются автоматически при первом OAuth входе
- Привязка по email к существующим аккаунтам
- `auto_activate` для автоматической активации без ожидания админа
- Подробные инструкции: [docs/oauth.md](oauth.md)

### Приватные каналы

Каналы можно создавать как приватные (закрытые). Все пользователи видят приватные каналы в сайдбаре (с иконкой замка), но войти могут только авторизованные.

**Правила доступа:**
- Публичные каналы -- любой аутентифицированный пользователь
- Приватные каналы -- только участники, создатель или администраторы сервера

**Управление участниками:**
- Создатель и администраторы могут добавлять/удалять участников
- Выбор из списка зарегистрированных пользователей (dropdown)
- Создатель автоматически становится участником

**Invite-ссылки:**
- Создатель/админ может сгенерировать ссылку-приглашение (`/invite/{token}`)
- Ссылка действует 7 дней
- При переходе по ссылке пользователь добавляется как участник канала
- Если не залогинен -- редирект на логин и обратно на invite

### Админ-панель

- `/admin` -- таблица пользователей с управлением
- Активация/деактивация, назначение/снятие админа, удаление, сброс пароля
- Первый зарегистрированный пользователь автоматически становится админом
- Новые пользователи создаются в статусе Pending
