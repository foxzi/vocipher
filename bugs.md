# Аудит стабильности WebRTC/голос/соединение

Отчёт по результатам аудита кода Vocala. Цель — выявить проблемы, ухудшающие стабильность связи. Только диагностика и однострочные подсказки по исправлению, без патчей.

---

## Критичные (приводят к разрывам и сбоям)

- [x] **Нет ping/pong и read deadline у WebSocket** — `internal/signaling/hub.go:212-264` (`readPump`) вызывает только `SetReadLimit`, но не `SetReadDeadline`/`SetPongHandler`. `writePump` (`hub.go:212-232`) не шлёт периодических пингов. Полузакрытые TCP-соединения (NAT rebinding на мобильных, переключение Wi-Fi → LTE) висят до RST от ОС. Фикс: `SetReadDeadline(60s)` со сбросом в `SetPongHandler` + тикер пинга 30s в `writePump`. **(Исправлено: `pongWait=60s`, `pingPeriod=30s`, `SetReadDeadline`+`SetPongHandler` в `readPump`, `pingTicker` в `writePump`.)**

- [x] **Нет write deadline у WS** — `hub.go:219-230`. Залипший клиент блокирует `WriteMessage` навсегда; буфер `Send` переполняется, `Broadcast` (`hub.go:103-112`) молча дропает сообщения — включая критичные `webrtc_offer`, что приводит к дедлоку ренеготиации. Фикс: `SetWriteDeadline(10s)` перед каждой записью. **(Исправлено: `writeWait=10s`, `SetWriteDeadline` перед каждой `WriteMessage` в `writePump`.)**

- [x] **Offer ренеготиации может быть тихо потерян при полном Send-буфере** — `Broadcast`/`BroadcastToChannel`/`SendTo` (`hub.go:103-138`) используют `default:` при полном канале (256). `SendMessage` SFU маршрутизируется через них. Потерянный server offer оставляет пира в glare-дедлоке; polite-rollback клиента (`app.js:1185`) не срабатывает. Фикс: переотправка pending offer, либо приоритетный канал/блокировка для сигнальных сообщений. **(Исправлено: добавлен `SendPriority` канал (буфер 64) и метод `SendToPriority`, закрывающий зависшее соединение вместо тихого дропа; SFU offer/answer/ICE маршрутизируются через него.)**

- [x] **OnICEConnectionStateChange ничего не делает** — `sfu.go:305-309` только логирует. Нет ICE restart при `disconnected`/`failed`. Короткий сетевой провал на мобильном = соединение мёртво до 60s ICE failed timeout. Фикс: при `disconnected` через ~5s запускать `CreateOffer({ICERestart:true})`. **(Исправлено: split ICEConnectionStateFailed from Closed — Failed now immediately calls iceRestart() in a goroutine; Disconnected already scheduled 5s timer calling iceRestart().)**

- [x] **Пир удаляется только при `failed`/`closed`, не при `disconnected`** — `sfu.go:328-331`. С `SetICETimeouts(15s, 60s, ...)` пир, у которого "перепрыгнул" Wi-Fi, держит слот до 60s; параллельно туда форвардятся новые треки, тратится CPU и блокируется ренеготиация остальных (`renegotiateAllExcept`). Фикс: тоже сносить при затяжном `disconnected` (>20s). **(Исправлено: iceFailoverTimer на 30s в case ICEConnectionStateDisconnected вызывает s.RemovePeer(userID), отменяется при возврате в connected/completed.)**

- [x] **Порт ICE TCP mux 40201 захардкожен** — `sfu.go:109`. Не вынесен в `internal/config/config.go`, нет фолбэка. При коллизии порта — сломан TCP-фолбэк для мобильных (где оператор режет UDP). Ошибка только логируется и тихо игнорируется (`sfu.go:113-115`). Фикс: вынести в конфиг; при ошибке привязки пробовать `:0` или громко предупреждать оператора. **(Исправлено: вынесен в config.WebRTC.ICETCPPort (default 40201, 0 = выкл), при ошибке bind пишется logger.Error с инструкцией.)**

- [x] **Нет фолбэка/предупреждения для NAT 1:1** — `sfu.go:118-119`. Если `natIP` не задан и сервер за NAT (типичный VPS, Docker), pion отдаёт только host-кандидаты. По умолчанию TURN не настроен (`buildICEServers` в `cmd/server/main.go:147` отдаёт только Google STUN). Клиенты за симметричным NAT молча падают. Фикс: в продакшен-режиме отказывать запуск без TURN/NAT IP, либо автодетект через STUN на старте. **(Исправлено: при пустом NATIP и выключенном TURN на старте сервера пишется logger.Warn с инструкцией.)**

- [x] **Глобальные `WriteTimeout`/`ReadTimeout` HTTP-сервера** — `cmd/server/main.go:535-540`. После `Hijack()` gorilla/websocket отвязывает соединение — для уже поднятого WS не страшно. Но **сам upgrade WS** проходит под этими таймаутами; на медленном мобильном апгрейд может срываться. Также режутся длинные HTTP POST (screen-preview, загрузки). Фикс: `WriteTimeout=0` для WS-роута или per-route таймауты. **(Исправлено: WriteTimeout=0 (per-handler/WS deadlines уже существуют), ReadTimeout заменён на ReadHeaderTimeout для slowloris.)**

---

## Важные (страдает воспринимаемое качество)

- [x] **Гонка в дебаунсе ренеготиации** — `sfu.go:782-800`. Флаг `negoScheduled` сбрасывается *до* запуска `doRenegotiate`; повторный вызов `renegotiate()` во время выполнения `doRenegotiate` запускает вторую горутину, ждущую на `negoMu`. Два offer'а подряд провоцируют glare. Фикс: сбрасывать флаг только после возврата из `doRenegotiate`, либо использовать `dirty`-флаг. **(Исправлено: добавлен negoDirty флаг; goroutine в renegotiate() теперь крутится в цикле пока dirty=true, гарантируя один активный worker и коалесцируя повторные запросы.)**

- [x] **Ренеготиация сдаётся через 5s, если не вышли в stable** — `sfu.go:813-825`. Логирует `Debug` и молча выходит. Пир не получает новые треки (например, кто-то включил камеру) до следующего события. Фикс: перевзводить `renegotiate`, а не сбрасывать. **(Исправлено: при таймауте ожидания stable doRenegotiate выставляет negoDirty=true, и внешний цикл renegotiate() перевзводит попытку через 500ms.)**

- [x] **Переотправка pending offer при glare идёт через тот же ненадёжный путь** — `sfu.go:213-220`. Если оригинальный offer был дропнут переполненным Send-каналом, повтор уйдёт туда же. Фикс: гарантировать недропаемость сигнальных сообщений (отдельный приоритетный канал или блокирующая отправка). **(Исправлено косвенно: SFU `SendMessage` теперь идёт через `SendToPriority` с отдельным буфером 64.)**

- [x] **Клиентский `onnegotiationneeded` поллит stable в цикле** — `web/static/js/app.js:1022-1031`. Без явного ограничения попыток; в сочетании с серверным "drop client offer on glare" даёт штормы offer'ов при реконнекте. Фикс: экспоненциальный бэкофф или единичная повторная попытка. **(Исправлено: bounded exponential backoff (4 попытки: 200/400/800/1600ms) вместо 20-итерационного цикла.)**

- [x] **Клиент полностью пересоздаёт PC при каждом WS-реконнекте** — `app.js:170-178`: на каждом `ws.onopen` идёт `cleanupWebRTC()` + `startWebRTC()`. WS-провал в 3 секунды убивает живой `RTCPeerConnection`, который пережил бы это через ICE keepalive. Каждый реконнект = аудио-просадка. Фикс: пересоздавать PC только если `connectionState` в `failed`/`closed`, иначе просто переотправить `join_channel`. **(Исправлено: при WS reconnect, если `peerConnection.connectionState` in connected/connecting/new — переотправляется только `join_channel`; PC пересоздаётся только при failed/closed/disconnected.)**

- [x] **Send-буфер 256 мал для каналов с видео/демонстрацией экрана** — `hub.go:158,193`. Broadcast camera-on, реакции, presence + ICE-кандидаты могут на пике превысить 256 для медленного мобильного → молчаливые дропы. Фикс: поднять до 1024, либо отстреливать клиента при стойком переполнении. **(Исправлено: буфер `Send` поднят с 256 до 1024 для user и guest клиентов в `internal/signaling/hub.go`.)**

- [x] **Нет явной настройки NACK / RTCP feedback кроме дефолтов** — `sfu.go:80-86` использует `RegisterDefaultCodecs` + `RegisterDefaultInterceptors`. Дефолты pion включают NACK и PLI для видео, RR для аудио, но **нет RED/FlexFEC для opus** — при потерях >5% будет заметная "битость" аудио. Фикс: регистрировать opus с явным `useinbandfec=1; usedtx=0`, опционально добавить `audio/red`. (Исправлено: opus регистрируется вручную с minptime=10;useinbandfec=1;usedtx=0; видеокодеки VP8/H264 регистрируются с явным RTCP feedback nack/pli/ccm-fir/goog-remb в internal/webrtc/sfu.go.)

- [x] **Параметры opus DTX / inband-FEC не заданы явно** — там же. Chrome включает FEC по умолчанию, но сервер не договаривает `useinbandfec=1` явно через SDP — желательно прописать. (Исправлено: см. выше — opus fmtp задан явно.)

- [x] **PLI отправляется только на подписку** — `sfu.go:760-770`. Это уже хорошо для первого кадра новому подписчику, но никаких механизмов восстановления при длительной потере keyframe (например, при просадке сети у паблишера) нет. Фикс: периодический PLI при отсутствии keyframe-метрик. (Исправлено: intervalpli.NewReceiverInterceptor сконфигурирован с GeneratorInterval(2s) — периодический PLI на приёмнике.)

- **Нет клиентских хендлеров `online`/`offline`/`visibilitychange`** — `grep` по `app.js` не нашёл ни одного. Свернутая вкладка на мобильном → аудио-контекст приостановлен, при возврате никакого восстановления. Фикс: на `visibilitychange→visible` или `online` форсить WS-проверку + ICE restart.

- **Мобильный путь `USE_WS_MEDIA` без ack/backpressure** — `app.js:2331` использует `MediaRecorder.start(60)`; при тормозящем WS `ondataavailable` продолжает срабатывать, а Send-канал (64 буфера, `hub.go:159`) молча дропает аудио-чанки. Фикс: ставить `MediaRecorder` на паузу, когда `ws.bufferedAmount` превышает порог.

- **WS rate limiter 30 msg/s, burst 60** — `hub.go:247`. Всплеск ICE-кандидатов на установке соединения легко превышает burst и молча отбрасывается (`hub.go:255`), что ломает ICE pairing. Фикс: отдельный лимитер для `webrtc_ice`, либо поднять burst до 200.

- **ID гостей из `rand.Int(1<<50)`** — `hub.go:184-186`. Birthday paradox даёт коллизию после ~33M сессий. Риск низкий, но `Hub.Register` (`hub.go:81-89`) при коллизии закрывает старое соединение → "не тот" гость окажется отключён.

---

## Желательные (закаливание)

- Сделать ICE-таймауты настраиваемыми (`sfu.go:107`). 15s disconnected приемлемо для десктопа, агрессивно для мобильных. Вынести в `config.go`.
- Захардкоженные 500ms дебаунса ренеготиации (`sfu.go:794`) и 800ms задержки форвардинга (`sfu.go:724`) — в именованные константы или конфиг.
- `RemoveSFU` (`sfu.go:174-185`) вызывается безусловно после `RemovePeer` (`hub.go:713-714`); проверка пустоты внутри, но разделение логики уничтожения комнаты создаёт гонку при join во время удаления.
- `cleanupWebRTC` (`hub.go:708-716`) не вызывает `clearPreviewIfSharer`; полагается на порядок defer'ов в `readPump`.
- Рассмотреть simulcast/SVC для видео — сейчас в `sfu.go:680-720` форвардится полный трек всем подписчикам без оглядки на их канал.
- Добавить `/healthz` со статистикой ICE/SFU для мониторинга.
- Метрики на уровне клиента: размер Send-буфера, чтобы детектить залипшего клиента вместо молчаливого дропа.
- Поднять уровень лога `failed to start ICE TCP mux` (`sfu.go:115`) с Error на WARN+ с явным выделением для оператора.

---

## Уже сделано хорошо

- Корректный perfect-negotiation: сервер impolite (drop + re-send offer, `sfu.go:196-220`), клиент polite (rollback в `app.js:1185-1187`).
- ICE keepalive 2s (`sfu.go:107`) — правильно для удержания NAT-биндингов на мобильных.
- Экспоненциальный бэкофф с потолком 30s на клиентском WS-реконнекте (`app.js:200-201`).
- ICE TCP mux включён для клиентов за carrier-grade NAT (`sfu.go:109-114`).
- TURN-учётки корректно отдаются клиентам через `buildICEServers` (`main.go:147-160`), включая TURNS для HTTPS.
- Мобильный детект форсит `iceTransportPolicy: relay`, когда есть TURNS (`app.js:955-962`) — правильно для сотовой сети.
- Новое подключение того же пользователя закрывает старое WS (`hub.go:84-86`).
- WS read limit задан (`hub.go:246`).
- Fallback на receive-only при провале захвата микрофона (`app.js:1062-1112`).
- PLI-интерсептор зарегистрирован (`sfu.go:73-78`) и PLI шлётся при подписке (`sfu.go:763-766`).
- ICE-кандидаты буферизуются до установки remoteDescription (`app.js:1208-1215`).
- `RemovePeer` чистит `outputTracks`/`cameraOutputTracks`/`screenOutputTracks` у всех подписчиков (`sfu.go:471-516`).
- WS rate limiting защищает от флуд-DoS (`hub.go:247`).
- Криптостойкие ID гостей (`hub.go:184`) — нельзя предугадать.
- Адаптивный битрейт публикации по `qualityLimitationReason` (`app.js:1219+`).
- Graceful HTTP shutdown с 10s таймаутом (`main.go:558-561`).

---

## Топ-3 для исправления в первую очередь

1. **WS ping/pong + write deadline** (`hub.go:212`) — закрывает половину "случайных дисконнектов на мобильных".
2. **ICE restart при disconnected** (`sfu.go:305`) — даёт восстановление без полного пересоздания PC.
3. **Не дропать сигнальные сообщения при полном Send-буфере** (`hub.go:103-138`) — устраняет дедлоки ренеготиации.

Эти три пункта закрывают основной массив жалоб на "случайные обрывы на мобильных".
