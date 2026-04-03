# Security / Безопасность

## English

### Authentication

- **Password hashing:** bcrypt with default cost (10 rounds)
- **Minimum password length:** 8 characters (server-enforced)
- **Minimum username length:** 2 characters
- **Session tokens:** 64-character hex strings (32 bytes of `crypto/rand`)
- **Session expiry:** 30 days, with automatic cleanup every hour
- **Cookies:** `HttpOnly`, `SameSite=Lax`, `Path=/`

### CSRF Protection

All state-changing POST requests require a CSRF token. The token is:
- Stored in a separate `csrf_token` cookie (`SameSite=Strict`, 24-hour expiry)
- Embedded in HTML forms as a hidden `<input>` field
- Validated server-side by comparing the cookie value with the form field value

Protected endpoints: `/login`, `/register`, `/channels`, `/channels/delete`, `/channels/members/*`, `/channels/invite`, `/admin/users/*`.

### WebSocket Security

- **Origin validation:** The WebSocket upgrader checks the `Origin` header against the request `Host`. Cross-origin WebSocket connections are rejected (prevents CSWSH attacks)
- **Authentication:** WebSocket connections require a valid session cookie
- **Message size limit:** 512 KB maximum per message (prevents memory exhaustion)
- **Duplicate connection handling:** When a user opens a second WebSocket, the previous connection is closed

### Rate Limiting

An IP-based rate limiter protects all HTTP endpoints:
- 10 requests per second per IP, with a burst of 20
- Returns `429 Too Many Requests` when exceeded

### HTTP Security Headers

All responses include:
- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy: microphone=(self), camera=(self)`

### Authorization

- **User activation:** New users must be activated by an admin before they can log in
- **First user auto-admin:** The first registered user is automatically admin and active
- **Admin panel:** Only users with `is_admin=1` can access `/admin`
- **Channel deletion:** Only the creator or an admin can delete a channel
- **Channel creation:** Any active authenticated user can create channels
- **Public voice channels:** Any active authenticated user can join
- **Private channels:** Only members, the creator, or server admins can join
- **Private channel management:** Only the creator or admins can add/remove members and generate invite links
- **Invite links:** 7-day expiry, token-based, auto-add member on accept
- **Chat messages:** Limited to 2000 characters, auto-cleaned by retention policy
- **Self-protection:** Admins cannot modify their own account (prevents self-deactivation)

### XSS Prevention

- **Server-side:** Go `html/template` auto-escapes all template variables
- **Client-side:** All user-controlled strings (usernames, channel names) are escaped via `escapeHTML()` before insertion into `innerHTML`
- **Screen previews:** Only `data:image/` URIs are accepted for screen share thumbnails

### SQL Injection

All database queries use parameterized statements (`?` placeholders). No string concatenation is used in SQL queries.

### TURN Server

- TURN credentials use a randomly generated 64-character hex secret
- Long-term credentials authentication via MD5 key (TURN standard)
- The secret is regenerated on each server restart

### Known Limitations

- No `Content-Security-Policy` header (CDN scripts for Tailwind/HTMX)
- No Subresource Integrity (SRI) hashes on CDN scripts
- No account lockout after failed login attempts
- WebSocket sessions are not re-validated after initial authentication
- Cookie `Secure` flag is hardcoded to `false` (must be changed for HTTPS)

### Recommendations for Production

1. **Use HTTPS** -- Deploy behind Nginx with TLS (see [deployment.md](deployment.md))
2. **Set `Secure` cookie flag** -- Change `Secure: false` to `true` in `cmd/server/main.go` when using HTTPS
3. **Enable TURN** -- Set `VOCIPHER_TURN_IP` for reliable NAT traversal
4. **Restrict listen address** -- Use `VOCIPHER_ADDR=127.0.0.1:8090` when behind a reverse proxy
5. **Firewall** -- Only expose ports 80, 443 (TCP) and 3478 (UDP) to the internet

---

## Русский

### Аутентификация

- **Хеширование паролей:** bcrypt с дефолтной стоимостью (10 раундов)
- **Минимальная длина пароля:** 8 символов (проверка на сервере)
- **Минимальная длина имени:** 2 символа
- **Токены сессий:** 64-символьные hex-строки (32 байта `crypto/rand`)
- **Срок жизни сессий:** 30 дней, автоматическая очистка каждый час
- **Куки:** `HttpOnly`, `SameSite=Lax`, `Path=/`

### CSRF-защита

Все POST-запросы, изменяющие состояние, требуют CSRF-токен. Токен хранится в отдельной куке `csrf_token` и дублируется в скрытом поле формы. Сервер сравнивает оба значения.

Защищённые эндпоинты: `/login`, `/register`, `/channels`, `/channels/delete`, `/channels/members/*`, `/channels/invite`, `/admin/users/*`.

### Безопасность WebSocket

- **Проверка Origin:** WebSocket апгрейдер проверяет заголовок `Origin` против `Host`. Кросс-доменные подключения отклоняются
- **Аутентификация:** Для WebSocket-подключения нужна валидная сессионная кука
- **Лимит размера сообщений:** 512 КБ максимум (защита от исчерпания памяти)
- **Дубликаты подключений:** При повторном подключении того же пользователя старое соединение закрывается

### Rate Limiting

IP-based ограничение скорости на всех HTTP-эндпоинтах: 10 запросов/сек, burst 20. При превышении возвращает `429 Too Many Requests`.

### HTTP-заголовки безопасности

- `X-Content-Type-Options: nosniff`
- `X-Frame-Options: DENY`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Permissions-Policy: microphone=(self), camera=(self)`

### Авторизация

- **Активация пользователей:** Новые пользователи должны быть активированы админом
- **Первый пользователь:** Автоматически становится админом и активируется
- **Админ-панель:** Доступна только пользователям с `is_admin=1`
- **Удаление каналов:** Может создатель канала или админ
- **Создание каналов:** Любой активный аутентифицированный пользователь
- **Публичные каналы:** Любой активный аутентифицированный пользователь
- **Приватные каналы:** Только участники, создатель или администраторы
- **Управление приватными каналами:** Только создатель или администраторы могут добавлять/удалять участников и генерировать invite-ссылки
- **Invite-ссылки:** Срок действия 7 дней, токен-based, автоматическое добавление при переходе
- **Чат:** Сообщения ограничены 2000 символами, автоочистка по retention policy
- **Самозащита:** Админ не может деактивировать/удалить свой аккаунт

### Предотвращение XSS

- **Сервер:** Go `html/template` автоматически экранирует переменные
- **Клиент:** Все пользовательские строки экранируются через `escapeHTML()` перед вставкой в `innerHTML`
- **Превью экрана:** Принимаются только `data:image/` URI

### SQL-инъекции

Все запросы к базе используют параметризованные выражения (`?`). Конкатенация строк в SQL не используется.

### Известные ограничения

- Нет заголовка `Content-Security-Policy` (CDN-скрипты Tailwind/HTMX)
- Нет SRI-хешей на CDN-скриптах
- Нет блокировки аккаунта после неудачных попыток входа
- WebSocket-сессии не перепроверяются после начальной аутентификации
- Флаг `Secure` на куках отключён (нужно включить при HTTPS)

### Рекомендации для продакшена

1. **Используйте HTTPS** -- разверните за Nginx с TLS (см. [deployment.md](deployment.md))
2. **Включите `Secure` флаг кук** -- измените `Secure: false` на `true` при использовании HTTPS
3. **Включите TURN** -- установите `VOCIPHER_TURN_IP` для надёжного NAT traversal
4. **Ограничьте адрес прослушивания** -- `VOCIPHER_ADDR=127.0.0.1:8090` за реверс-прокси
5. **Файрвол** -- откройте только порты 80, 443 (TCP) и 3478 (UDP)
