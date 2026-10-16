# dns-guard — план реализации

DNS-сервис для MikroTik RouterOS (ARM64 контейнер): принимает запросы по UDP/53
(и TCP/53), маршрутизирует их по правилам зон на группы вышестоящих серверов
(UDP / DoT / DoH) с primary+fallback, применяет MIN-TTL, кэширует ответы
в памяти и показывает статистику (top-100 доменов с пиковым RPM) в веб-UI.

## Контекст: результаты разведки роутера

- **hAP ax³**, RouterOS **7.24.5** (stable), ARM64, 4×864 МГц, 1 ГБ RAM (~635 МБ свободно).
- Пакет `container` установлен, **device-mode container=yes** уже включён.
- Registry `https://registry-1.docker.io` настроен, tmpdir на USB.
- USB: SanDisk Extreme 62.7 ГБ ext4 (свободно 61.4 ГБ). Внутренняя флеш 128 МБ, **8.5% бэд-блоков** — использовать только внешний диск.
- Уже работает контейнер `nskforward/mikwg:latest` (arm64) — инфраструктура обкатана.
- Контейнерный бридж `container` = **192.168.89.1/24**, маскарад `192.168.89.0/24` уже есть.
- **Критично:** на роутере DNS управляет маршрутизацией в VPN: статические `FWD`-записи DNS с `address-list=to_vpn_list` наполняют список, mangle уводит эти IP в AmneziaWG. Поэтому внедрение — по схеме **«роутер → контейнер»** (`/ip dns set servers=192.168.89.3`), а не «клиенты → контейнер». Иначе `to_vpn_list` перестанет наполняться.

## Решения (согласованы)

| Вопрос | Решение |
|---|---|
| Своё решение vs готовое | Своё на **Go 1.26+** (AdGuard Home закрывает ~80% и не даёт peak RPM + per-group fallback) |
| Протокол апстрима | **На группу** (`udp` / `dot` / `doh`) |
| Fallback | **Только по ошибке запроса** (без фоновых health-check) |
| Входящие протоколы | **UDP/53 + TCP/53** |
| Web UI | **Без авторизации** (LAN) |
| Статистика | Сброс при рестарте, top-100 по числу запросов + пиковый RPM на домен |
| Кэш | In-memory, сброс при рестарте, MIN-TTL пол |

## Архитектура

```
клиент --UDP/TCP 53--> dns-guard
   1. матчинг правила по зоне (longest-suffix)  -> группа
   2. cache lookup (key: qname|qtype|qclass|DO)
        hit  -> TTL-декей -> ответ (+stats)
   3. miss -> группа: primary (ротация) -> при ошибке fallback
   4. Apply MIN-TTL/MAX-TTL ко всем секциям
   5. cache.Put, stats.Record -> ответ клиенту
```

Слои:

- `internal/config` — YAML-конфиг, load/save/validate
- `internal/rules` — матчинг зон
- `internal/upstream` — Resolver (UDP/DoT/DoH) + Group (primary/fallback, ротация, bootstrap)
- `internal/cache` — LRU-кэш с TTL-границами
- `internal/stats` — по-доменные счётчики + peak RPM, top-N
- `internal/server` — DNS-пайплайн (UDP+TCP), hot-reload
- `internal/web` — dashboard + REST API + YAML-редактор (embed)
- `main.go` — флаги, сигналы

## Схема конфига (config.example.yaml)

```yaml
listen:
  udp: ":53"
  tcp: ":53"
web:
  enabled: true
  listen: ":8080"
cache:
  enabled: true
  max_entries: 10000
  min_ttl: 60      # MIN-TTL: пол TTL ответа
  max_ttl: 0       # 0 = без верхней границы
upstream:
  timeout: 3s
groups:
  a:                          # дефолтная группа — UDP
    protocol: udp
    primary:  ["77.88.8.8", "8.8.8.8"]
    fallback: ["1.1.1.1"]
  b:                          # зона .ru — DoH
    protocol: doh
    primary:  ["https://dns.google/dns-query"]
    fallback: ["https://cloudflare-dns.com/dns-query"]
    bootstrap: ["77.88.8.8"]
default_group: a
rules:
  - zone: "ru"
    group: b
```

---

## Шаги реализации

### Этап 0. Каркас и конфиг — ✅ сделано

- [x] `go.mod` (Go 1.26, `miekg/dns`, `yaml.v3`)
- [x] `internal/config/config.go` — типы, Default/Load/Parse/Save/Validate, normalize, `Timeout()`
- [x] `internal/rules/rules.go` — `Matcher`, longest-suffix match, нормализация зон (`ru`, `*.ru`, `example.com`)

### Этап 1. Апстримы — ✅ сделано (нужен ревью/тесты)

- [x] `internal/upstream/upstream.go` — `Resolver`, `New(...)`, `Group` (ротация primary → fallback по ошибке)
- [x] `internal/upstream/udp.go` — UDP-клиент + `normalizeHostPort`
- [x] `internal/upstream/dialer.go` — bootstrap-диалер (резолв DoH/DoT хостнеймов через plain DNS)
- [x] `internal/upstream/dot.go` — DoT-клиент (tcp-tls, TLS 1.2+, SNI)
- [x] `internal/upstream/doh.go` — DoH-клиент (RFC 8484, POST application/dns-message, HTTP/2)

### Этап 2. Кэш — ✅ сделано

- [x] `internal/cache/cache.go` — LRU, `Apply` (MIN/MAX TTL), `Get` (TTL-декей), `Put`, `Flush`, `Len`
- [x] негативные ответы (SOA min), пропуск TC/неуспешных rcode

### Этап 3. Статистика — ✅ сделано

- [x] `internal/stats/stats.go`
  - [x] счётчики: `total`, `cache_hits`, `cache_misses`, `servfail`, по группам
  - [x] на домен: `total`, `currentMinute`, `peakRPM` (O(1) памяти на домен)
  - [x] rollover минуты (ленивый, без горутины): `peakRPM = max(peakRPM, currentMinute)`, сброс `currentMinute`
  - [x] `TopN(n)` — top-100 по `total` с `peakRPM`
  - [x] `Snapshot()` с `Uptime` — с момента старта; агрегация по домену 2-го уровня

### Этап 4. DNS-сервер и пайплайн — ✅ сделано

- [x] `internal/server/server.go`
  - [x] `Server` с атомарным состоянием (matcher + группы), `Reload(cfg)`
  - [x] `ServeDNS` (общий для UDP/TCP): матчинг → кэш → апстрим → TTL → ответ
  - [x] `RecursionDesired=true`, сохранение `Id`, обработка EDNS/OPT
  - [x] UDP: `Truncate()` по буферу клиента (512/EDNS) с TC=1
  - [x] SERVFAIL при отказе всех апстримов группы
  - [x] `stats.Record` на каждом пути (hit/miss/группа/rcode)
- [x] `main.go`
  - [x] флаг `-config` (default `./config.yaml`; в контейнере `/etc/dns-guard/config.yaml`)
  - [x] запуск UDP и TCP слушателей (`dns.Server`)
  - [x] graceful shutdown по SIGTERM/SIGINT, reload по SIGHUP
  - [x] логирование (slog)

### Этап 5. Веб-UI и API — ✅ сделано

- [x] `internal/web/web.go` + `internal/web/index.html` (embed)
  - [x] dashboard: top-100 (домен, запросы, peak RPM), счётчики, uptime, размер кэша
  - [x] кнопка «Flush cache»
  - [x] вкладка конфига: YAML-редактор (GET/PUT), сохранение в файл + применение на лету
  - [x] API: `GET /api/stats`, `GET /api/config`, `PUT /api/config`, `POST /api/cache/flush`, `GET /api/health`
  - [x] автообновление дашборда (fetch + таймер)

### Этап 6. Упаковка и деплой — ✅ сделано

- [x] `.gitignore`
- [x] `config.example.yaml`
- [x] `Dockerfile` — multi-stage, `CGO_ENABLED=0 GOOS=linux GOARCH=arm64`, distroless/static (ca-certs), root (bind :53)
- [x] `README.md` — конфиг, API, деплой на RouterOS
  - [x] `usb1/volumes/dns-guard/{conf}` mount
  - [x] `veth-dnsguard 192.168.89.3/24` + bridge `container` (маскарад уже есть)
  - [x] `/container/add remote-image=... arm64`
  - [x] переключение `/ip dns set use-doh-server="" servers=192.168.89.3` (+ откат)
  - [x] предупреждение про `to_vpn_list` / VPN-маршрутизацию

### Этап 7. Тесты и приёмка — ⏳ частично (осталось на роутере)

- [x] юнит-тесты: `rules.Match` (longest-suffix, `*.ru`), `cache` (MIN-TTL/декей/негатив), `stats` (peak RPM, rollover), `config`, `server` (fake-апстрим)
- [x] `go vet ./...`, `go build ./...`, `go test ./...`
- [x] локальный smoke: конфиг + `dig @127.0.0.1` по UDP и TCP
- [x] проверка MIN-TTL: апстрим с малым TTL (5s) → ответ ≥ min_ttl (60s)
- [x] проверка fallback: недоступный primary → ответ через fallback
- [ ] сборка arm64-образа, загрузка на роутер, `dig @192.168.88.1 youtube.com` → рост `to_vpn_list`
- [ ] контроль RAM (`/container print` memory-current)

---

## Известные риски / решения

1. **VPN-DNS-связка** — точку внедрения выбирать так, чтобы запросы всё равно проходили через DNS роутера (он наполняет `to_vpn_list`). В режиме «только кэш» апстрим контейнера = DNS роутера, поэтому `/ip dns servers` роутера на контейнер НЕ переключаем (иначе петля) — см. вопросы сессии 2.
2. **Egress DoH из контейнера** идёт напрямую через WAN (как сейчас у роутера); при необходимости VPN для DNS — отдельные mangle-правила (вне текущего объёма).
3. **Двойной кэш** — временно снизить `cache-max-ttl` роутера для чистоты статистики (не обязательно).
4. **Пустой `ads_block`** — правило в forward ссылается на пустой список; вне объёма.

## Definition of Done

- `go test ./...` и `go build ./...` зелёные.
- Бинарь в arm64-образе запускается в контейнере на hAP ax³, слушает UDP+TCP/53, отдаёт ответы.
- Применяется MIN-TTL, работает primary/fallback, работает кэш (сброс при рестарте).
- Web UI показывает top-100 с peak RPM и позволяет править конфиг.
- DNS-трафик, идущий транзитом через роутер, по-прежнему наполняет `to_vpn_list`.

---

## Состояние на конец сессии 1

Код готов и проверен локально: этапы 0–6 полностью, этап 7 — вся локальная часть.

- `go build ./...`, `go vet ./...`, `go test ./...` — зелёные.
- Юнит-тесты: `rules`, `cache`, `config`, `stats`, `server` (fake-апстрим).
- Локальный smoke (через фейковый апстрим `127.0.0.1:5355`; временные файлы — в gitignored `tmp/`): UDP и TCP отдают ответы; MIN-TTL (5→60); кэш miss→hit (`cache_len:1`); `PUT /api/config` применяется и сохраняется в файл; невалидный конфиг → 400; `POST /api/cache/flush` → ok.
- Отступление: Docker-образ работает от root (не nonroot) — nonroot не займёт порт 53 без `CAP_NET_BIND_SERVICE` (см. комментарий в `Dockerfile`).

Не проверено: живое развёртывание на роутере.

## Следующий шаг (сессия 2)

Первый деплой — **режим «только кэш»**:

- `cache.min_ttl` = **3600** (1 час) — минимизировать обращения к апстриму.
- Группа по умолчанию `a` — `udp`, **primary = DNS роутера** (`192.168.88.1`), без внешних DoH. Роутер пока единственный резолвер и продолжает наполнять `to_vpn_list`.
- Внешние DoH-апстримы добавляем **отдельным этапом позже**.

Подготовить к сессии 2:

- [ ] конфиг для роутера: `groups.a.primary: ["192.168.88.1"]`, `min_ttl: 3600`, `web.listen: ":8080"`.
- [ ] `deploy.rsc` / пошаговые команды RouterOS: veth `192.168.89.3/24` + bridge `container`, mount конфига, `/container/add`, старт, диагностика.

Вопросы, которые нужно закрыть до правок на роутере:

1. **Петля.** Апстрим контейнера = DNS роутера ⇒ `/ip dns servers` роутера **нельзя** направлять на контейнер. Определить, кто и как обращается к `192.168.89.3` в режиме «только кэш» (временно DHCP-DNS на клиентах; на роутере сначала `dig @192.168.89.3`), и подтвердить, что `to_vpn_list` при этом наполняется (запрос приходит в DNS роутера от контейнера — ожидаемо да, но проверить).
2. **Доступ к роутеру из сессии.** `192.168.88.1:22` недоступен (`No route to host`), Docker daemon в окружении не запущен. Нужен рабочий путь: SSH/джамп-хост к роутеру и registry для arm64-образа — либо пользователь выполняет команды на роутере вручную, а мы готовим конфиг и скрипты.
3. **Двойной кэш.** При MIN-TTL 1 час кэш роутера также активен — решить, снижать ли `cache-max-ttl` роутера (не обязательно).
