# dns-guard — план реализации

DNS-сервис для MikroTik RouterOS (ARM64 контейнер): принимает запросы по UDP/53
(и TCP/53), маршрутизирует их по правилам зон на группы вышестоящих серверов
(UDP / DoT / DoH) с primary+fallback, применяет MIN-TTL, кэширует ответы
в памяти и показывает статистику (top-100 доменов с пиковым RPM) в веб-UI.

> **Актуальное состояние:** сессии 1–6 завершены — код (этапы 0–6), live-деплой контейнера на роутере (сессия 2), собственный DoH-сервер на VPS + клиентская опция `hosts` (сессия 3), **этап C** — VPS-DoH как primary (сессия 4), **сессия 5** — зональный резолвинг `.ru` через головной роутер `192.168.1.1` с публичным DoH-fallback (и смешанные протоколы в группе через префикс адреса), **сессия 6** — топология «роутер → контейнер»: `/ip dns` роутера переведён на `192.168.89.3`, DNS от ISP (`dynamic-servers=192.168.1.1`) убран, `to_vpn_list` наполняется через контейнер. Открытых вопросов не осталось. Свежие разделы: «Сессия 6 — сделано», «Сессия 5 — сделано», «Сессия 4 — сделано».

## Контекст: результаты разведки роутера (обновлено в сессии 2)

- **hAP ax³**, RouterOS **7.24.5** (stable), ARM64, 4×864 МГц, 1 ГБ RAM (~638 МБ свободно), uptime на момент аудита 35 мин.
- Пакет `container` установлен, **device-mode container=yes** уже включён.
- Registry `https://registry-1.docker.io` настроен, **username пустой** (анонимный pull публичных образов), tmpdir `/usb1/container-tmp`.
- USB: SanDisk Extreme (SLOT `usb1`, смонтирован). Внутренняя флеш 128 МБ, **8.5% бэд-блоков**, free-hdd всего 77.9 MiB — использовать только внешний диск (`root-dir=/usb1/images/...`).
- Работает контейнер **`awg-converter`** (`remote-image=nskforward/mikwg:latest`, os=linux arch=arm64, `interface=veth-awg`, `root-dir=/usb1/images/awg-converter`, `dns=1.1.1.1`, `start-on-boot=yes`, memory-current ~12 MiB). Инфраструктура контейнеров обкатана; namespace аккаунта — **`nskforward`**.
- **veth:** `veth-awg 10.99.0.2/30` (для awg-converter), `vless 192.168.89.2/24 gateway=192.168.89.1` (порт в бридже `container`, флаг I — inactive). Шаблон для нас: `veth-dnsguard 192.168.89.3/24 gateway=192.168.89.1` + порт в бридж `container`.
- Контейнерный бридж `container` = **192.168.89.1/24**. Firewall forward **не блокирует** LAN→container (есть `fasttrack` + `drop all from WAN`, это не мешает routed-трафику из бриджа).
- `/ip dns`: `servers:` пусто, `dynamic-servers=192.168.1.1` (от DHCP на ether1), `use-doh-server:` пусто, `allow-remote-requests=yes`, `cache-size=2048KiB`, **`cache-max-ttl=1w`**, cache-used 319KiB. DHCP раздаёт клиентам `dns-server=192.168.88.1`.
- `/ip dns static` — 69 записей; `to_vpn_list` на момент аудита = **23** (база для замера).
- **SSH `192.168.88.1:22` теперь доступен** (в сессии 1 был `No route to host`) — deploy выполняется автономно, `admin` / `admin1` (пароль вводится через `expect`, т.к. `sshpass` не установлен). Также открыты 8728 (API) и 8291 (winbox).
- **Критично (VPN-DNS-связка):** на роутере DNS управляет маршрутизацией в VPN: статические `FWD`-записи DNS с `address-list=to_vpn_list` наполняют список, mangle уводит эти IP в AmneziaWG. Поэтому резолвинг VPN-доменов обязан проходить через DNS роутера. Полное внедрение (внешние DoH) — по схеме **«роутер → контейнер»** (`/ip dns set servers=192.168.89.3`); в сессии 2 клиентов и `/ip dns` не трогаем (см. топологию ниже).

### Находки сессии 2 (живой деплой)

- **Внешний UDP/53 заблокирован провайдером.** Запросы контейнера на `8.8.8.8:53`, `77.88.8.8:53`, `192.168.1.1:53` уходят (видно сниффером, conntrack `REPL-PACKETS=0`), ответов нет. При этом WAN роутера исправен: `/ping 1.1.1.1` и `:resolve google.com` работают, `to_vpn_list` растёт. Вывод: **нужен DoH/DoT** — прямой UDP/53 к внешнему миру не проходит.
- **Роутер не отвечает контейнеру на UDP/53** (запрос Mac → контейнер → роутер `192.168.88.1:53`/`192.168.89.1:53` виден, ответа нет; кэш роутера запрос контейнера не обрабатывает). Проверено: добавление input‑accept для `192.168.89.0/24` (счётчик срабатывает) и внесение бриджа `container` в список `LAN` **не помогло**. Похоже на отказ резолвера RouterOS отвечать контейнерной подсети. **Отсюда: план «cache-only, апстрим = роутер» нерабочий** — идём сразу на DoH.
- **DoH/DoT работают из контейнера:** `https://9.9.9.9/dns-query` (Quad9) и `https://94.140.14.14/dns-query` (AdGuard) — стабильно `NOERROR`; DoT `1.1.1.1:853` — `NOERROR`; Cloudflare `1.1.1.1`/Google `8.8.8.8` DoH — нестабильны (часто `SERVFAIL`). Bootstrap недоступен (UDP/53), поэтому DoH-URL только по IP.
- **MIN-TTL подтверждён:** у `example.com` апстрим отдаёт малый TTL, контейнер возвращает **3600**.
- **«Пропажа сети» — это Mac, не роутер.** WAN (`ether1`) стабилен, DHCP‑аренда `192.168.1.11` не менялась. `/ping 192.168.1.1` в timeout — норма (ISP‑шлюз не отвечает на ICMP). А вот Wi‑Fi Mac (en0, `c0:c7:db:03:ff:18`) **роумится** между сетью роутера и сетью провайдера (то `192.168.88.251`, то `192.168.1.13`), аренда многократно deassign/assign — отсюда `No route to host`. Плюс на `ether5` флапает линк (100M‑устройство).
- В 14:59 через WebFig с Mac применялся `/ip dns set ...` — в частности очищен `mdns-repeat-ifaces` (был `awg`). Стоит уточнить, намеренно ли.

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
| Кэш сервиса на деплое (сессия 2) | **Выключен** (`cache.enabled: false`), код кэша сохраняется как опция. Роль кэша исполняет кэш RouterOS; MIN-TTL применяется к ответам независимо от кэша |
| Топология сессии 2 | **Cache-only, без переключения клиентов.** Но апстрим = **DoH наружу** (не роутер): внешний UDP/53 провайдер блокирует, а роутер не отвечает контейнеру на UDP/53 (см. находки). DHCP-DNS и `/ip dns` **не меняем** — петля невозможна, контейнер ходит напрямую в DoH |
| Апстримы (сессия 2) | **DoH по IP** (хостнеймы без bootstrap не резолвятся): primary `9.9.9.9` (Quad9) + `94.140.14.14` (AdGuard), fallback `1.1.1.1`. DoT `1.1.1.1:853` тоже работает |
| Топология полной фазы | **Решено в сессии 6:** «роутер → контейнер» — `/ip dns set servers=192.168.89.3`, FWD-записи роутера наполняют `to_vpn_list` |
| Апстрим (целевой) | **Свой DoH-сервер на VPS** как primary для контейнера (свой IP/443 — не зависит от блокировок и капризов публичных DoH), публичные DoH (Quad9/AdGuard) — fallback |
| Сборка/публикация образа | **GitHub Actions** (buildx, `linux/arm64`) → Docker Hub `nskforward/dns-guard`; локальный Docker-демон не требуется |
| DoH-сервер на VPS (сессия 3) | **nginx (TLS/LE + секретный путь) → `doh-proxy` (plain HTTP) → `unbound` (рекурсор, DNSSEC)**. Порт **8443** (443 занят `mtg`) |
| Авторизация DoH | **Секретный путь** в URL (стиль NextDNS/AdGuard), проверяет nginx; плюс `limit_req` от перебора |
| TLS-точка VPS | Существующий vhost **`stream2.sla9.ru:8443`** (LE-серт, http2) — новых доменов/сертов не заводили |
| Резолв хостнейма апстрима | Групповая опция **`hosts`** (статический хостнейм→IP) — bootstrap plain-DNS из контейнера недоступен (UDP/53 закрыт), URL по IP ломает проверку серта |
| Backend DoH на VPS | `unbound` как **свой рекурсор** (не форвардер на публичные DNS) — независимость от чужих резолверов |
| Этап C (подключение контейнера) | **Отложено** по решению пользователя на отдельную сессию (см. «Следующий шаг (сессия 4)») |

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

Важно: шаг 4 (`applyTTL`) выполняется **независимо от кэша** (`server.go:156`), а `minTTL` берётся из `cfg.Cache.MinTTL` даже при `cache.enabled: false` (`server.go:98`). Поэтому сервис работает и как «TTL-реврайтер» перед кэшем роутера.

Слои:

- `internal/config` — YAML-конфиг, load/save/validate
- `internal/rules` — матчинг зон
- `internal/upstream` — Resolver (UDP/DoT/DoH) + Group (primary/fallback, ротация, bootstrap)
- `internal/cache` — LRU-кэш с TTL-границами
- `internal/stats` — по-доменные счётчики + peak RPM, top-N
- `internal/server` — DNS-пайплайн (UDP+TCP), hot-reload
- `internal/web` — dashboard + REST API + YAML-редактор (embed)
- `main.go` — флаги, сигналы

### Внешний DoH-апстрим на VPS (сессия 3)

Помимо внутреннего пайплайна dns-guard, в сессии 3 развёрнут собственный DoH-сервер на VPS `157.228.153.163` (Ubuntu 24.04.5):

```
контейнер dns-guard
  primary: https://stream2.sla9.ru:8443/<TOKEN>/dns-query
     │  hosts: stream2.sla9.ru → 157.228.153.163   (plain-DNS bootstrap из контейнера недоступен)
     ▼  TLS (LE-серт) + ALPN h2
  nginx :8443  (существующий vhost stream2.sla9.ru)
     location = /<TOKEN>/dns-query  →  proxy_pass http://127.0.0.1:8053/dns-query
     (limit_req; чужой путь → 404; корень сайта не тронут)
     ▼  plain HTTP/1.1
  doh-proxy 127.0.0.1:8053  (jedisct1/doh-server 0.10.0, systemd, DynamicUser)
     ▼  plain DNS (UDP/TCP)
  unbound 127.0.0.1:53  (рекурсор от корня, DNSSEC, qname-minimisation, кэши под 1 ГБ)
```

Почему `doh-proxy`, а не DoH самого unbound: unbound DoH — **только HTTP/2 (h2c)**; distro-nginx 1.24 не умеет HTTP/2 к апстриму (`proxy_http_version 2` появился лишь в nginx 1.29.4/mainline). `doh-proxy` — тонкий HTTP/1.1→DNS-фронтенд, специально сделанный для работы за reverse-proxy, поэтому рабочий nginx и `mtg`/443 не трогали.

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
  vps:                        # свой DoH на VPS: соединение по IP, хостнейм — для TLS
    protocol: doh
    primary:  ["https://stream2.sla9.ru:8443/<TOKEN>/dns-query"]
    fallback: ["https://9.9.9.9/dns-query", "https://94.140.14.14/dns-query"]
    hosts: {"stream2.sla9.ru": "157.228.153.163"}
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
- [x] `internal/upstream/dialer.go` — bootstrap-диалер (резолв DoH/DoT хостнеймов через plain DNS) + статический маппинг `hosts` (сессия 3)
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
- [x] `.github/workflows/docker.yml` — сборка и push `nskforward/dns-guard:latest` (arm64) через Actions (сделано в сессии 2)

### Этап 7. Тесты и приёмка — ✅ сделано (включая живой прогон в сессиях 2 и 4)

- [x] юнит-тесты: `rules.Match` (longest-suffix, `*.ru`), `cache` (MIN-TTL/декей/негатив), `stats` (peak RPM, rollover), `config`, `server` (fake-апстрим)
- [x] `go vet ./...`, `go build ./...`, `go test ./...`
- [x] локальный smoke: конфиг + `dig @127.0.0.1` по UDP и TCP
- [x] проверка MIN-TTL: апстрим с малым TTL (5s) → ответ ≥ min_ttl (60s)
- [x] проверка fallback: недоступный primary → ответ через fallback
- [x] сборка arm64-образа через GitHub Actions, публикация в Docker Hub (`nskforward/dns-guard:latest`)
- [x] живая приёмка на роутере: `dig @192.168.89.3` (UDP/TCP), TTL ≥ 3600, `/api/stats`, RAM (`/container print` memory-current) — сессия 4; рост `to_vpn_list` зависит от выбора топологии (открытый вопрос).

---

## Известные риски / решения

1. **VPN-DNS-связка** — точку внедрения выбирать так, чтобы запросы всё равно проходили через DNS роутера (он наполняет `to_vpn_list`). В режиме «только кэш» апстрим контейнера = DNS роутера, поэтому `/ip dns servers` роутера на контейнер НЕ переключаем (иначе петля) — см. вопросы сессии 2.
2. **Egress DoH из контейнера** идёт напрямую через WAN (как сейчас у роутера); при необходимости VPN для DNS — отдельные mangle-правила (вне текущего объёма).
3. **Двойной кэш** — в сессии 2 кэш сервиса выключен, кэширует только RouterOS. При переходе на топологию «контейнер впереди» кэш контейнера включается: он нужен для MIN-TTL на повторах и точной статистики; кэш роутера тогда работает как дедуп позади (не дублирование).
4. **Пустой `ads_block`** — правило в forward ссылается на пустой список; вне объёма.
5. **Секретный путь DoH** попадает в `access.log` nginx (читает только root) и в конфиг контейнера. При утечке — ротация токена: правка `location` в nginx + `hosts`/`primary` в конфиге контейнера (две точки). Токен хранить только в `/root/doh-token` на VPS и в конфиге контейнера, **не коммитить в git**.
6. **Порт 8443** нестандартный — если ISP начнёт фильтровать, сменить порт (nginx `listen` + UFW + URL в конфиге контейнера — 3 строки). Опционально позже: SNI-разделение 443 через `nginx stream`/`ssl_preread` (свой домен → DoH, остальное → `mtg`), чтобы DoH шёл именно по 443.
7. **`doh-proxy` — сторонний бинарник** (не из apt), обновляется вручную из GitHub Releases. Альтернатива при нежелании — апгрейд nginx до mainline (≥1.29.4) и удаление `doh-proxy`.

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

## Следующий шаг (сессия 2) — согласованный план

### Этап 2.1. CI: сборка и публикация образа

- [x] `.github/workflows/docker.yml`: триггеры `push` в `main` + `workflow_dispatch`; `docker/setup-buildx-action` (+ QEMU), `docker/build-push-action` с `platforms: linux/arm64`, `push: true`; теги `latest` и `sha-<short>`; кэш слоёв через `type=gha`; логин по секретам **`DOCKER_USERNAME` / `DOCKER_PASSWORD`**.
- [x] Первичный коммит проекта и `git push -u origin main` (коммит `f8b975e`).
- [x] **Блокер снят:** первый ран `38893687113` упал на шаге **Log in to Docker Hub** — в репозитории `dns-guard` не было секретов (они были только у `mikwg`). Добавлены `DOCKER_USERNAME`/`DOCKER_PASSWORD`; ран `da3a934` и последующие — `success`.
- [x] Репозиторий Docker Hub **публичный** (`curl https://hub.docker.com/v2/repositories/nskforward/dns-guard/` → 200, тег `latest`, `linux/arm64`).

### Этап 2.2. Деплой на роутер (SSH `admin@192.168.88.1`, через `expect`)

- [x] `veth-dnsguard 192.168.89.3/24 gateway=192.168.89.1` + `/interface bridge port add bridge=container interface=veth-dnsguard`.
- [x] Конфиг залит по SFTP в `/usb1/dns-guard/conf/config.yaml` (актуальный — DoH, см. ниже):
  ```yaml
  listen:  {udp: ":53", tcp: ":53"}
  web:     {enabled: true, listen: ":8080"}
  cache:   {enabled: false, max_entries: 10000, min_ttl: 3600, max_ttl: 0}
  upstream:{timeout: 3s}
  groups:
    a: {protocol: doh, primary: ["https://9.9.9.9/dns-query", "https://94.140.14.14/dns-query"], fallback: ["https://1.1.1.1/dns-query"]}
  default_group: a
  rules: []
  ```
  (апстрим — DoH по IP; UDP/53 наружу блокируется. Актуальный файл — в `tmp/router-config.yaml` и в `/usb1/dns-guard/conf/config.yaml`.)
- [x] `/container/mounts add list=dns-guard-conf src=/usb1/dns-guard/conf dst=/etc/dns-guard mode=rw` (у `mounts` список задаётся через `list=`; у контейнера свойство — `mountlists=`).
- [x] `/container/add remote-image=nskforward/dns-guard:latest interface=veth-dnsguard mountlists=dns-guard-conf root-dir=/usb1/images/dns-guard dns=192.168.88.1 start-on-boot=yes logging=yes` → запущен (у add нет параметров `os`/`arch` — определяются автоматически).
- [x] Диагностика: `/container print`, `/log print where topics~"container"`.

### Этап 2.3. Приёмка

- [x] `dig @192.168.89.3 <домен>` (UDP) — ответы есть (`NOERROR`, через DoH).
- [x] MIN-TTL: апстрим отдаёт малый TTL → контейнер возвращает **3600**.
- [x] `curl http://192.168.89.3:8080/api/stats` — `total` растёт, top-100/группы видны (`cache_len=0` — ожидаемо, кэш выключен).
- [x] `dig` TCP от клиента — дозаполнено в сессии 4 (после пересоздания контейнера): `NOERROR`, TTL 3600.
- [x] RAM: `/container print` memory-current — дозаполнено в сессии 4: ~10.6–11.9 MiB, стабильно.
- [x] `PUT /api/config` → hot-reload без рестарта (использован многократно).
- [ ] `to_vpn_list` — наполняется роутером от клиентских запросов (26+ записей); отдельная проверка после выбора топологии.
- [ ] **Откат:** `/container stop` + `/container remove` + veth + mount. Клиенты и `/ip dns` не затронуты.

### Прогресс сессии 2 (хроника)

- SSH к роутеру работает (Mac периодически теряет LAN из-за роуминга Wi‑Fi — на деплой не влияет).
- Каталог `usb1/volumes/` отсутствует; конфиг размещён в **`/usb1/dns-guard/conf/config.yaml`**.
- Создано: `veth-dnsguard 192.168.89.3/24 gateway=192.168.89.1` + порт в бридж `container`; mount `list=dns-guard-conf src=/usb1/dns-guard/conf dst=/etc/dns-guard mode=rw`.
- Образ собран и опубликован: **`nskforward/dns-guard:latest`** (linux/arm64, публичный) через GitHub Actions. Секреты `DOCKER_USERNAME`/`DOCKER_PASSWORD` добавлены в репозиторий — первый ран падал на логине. Пуш образа на роутер сначала падал по `resolving error`/`timeout connecting` (флап апстрима), со второй попытки зашёл.
- Исправлен баг: `config.Load/Parse` домешивал дефолтную группу к группам из YAML (`groups=2`) — коммит `6f4a6b3`, деплой подтвердил `groups=1`.
- Контейнер `dns-guard` добавлен (`remote-image=nskforward/dns-guard:latest`, `mountlists=dns-guard-conf`, `root-dir=/usb1/images/dns-guard`, `dns=192.168.88.1`, `start-on-boot=yes`, `logging=yes`) и запущен. Слушает UDP/TCP 53 и 8080.
- **Апстрим переопределён на DoH** (см. «Находки»): план «cache-only с апстримом = роутер» нереален — роутер не отвечает контейнеру на UDP/53, а внешний UDP/53 блокируется. Контейнер работает через DoH (Quad9/AdGuard), TTL 3600.
- Временные правки firewall (input‑accept для контейнера, членство `container` в `LAN`) **откачены** — не потребовались.

## Сессия 3 — сделано: собственный DoH-сервер на VPS

**Цель была:** поднять свой DoH-сервер на VPS `157.228.153.163` и подготовить клиент dns-guard к использованию его как primary. Достигнуто (этапы A и B); подключение в контейнере (этап C) отложено по решению пользователя.

### Разведка VPS

- **Ubuntu 24.04.5 LTS** (не 26), x86_64, 1 ГБ RAM (~600 МБ свободно), диск 8.8 ГБ (3.7 свободно).
- **443 занят `mtg`** (MTProto-прокси, raw TCP) — поделить нельзя. **80** и **8443** — nginx; на 8443 существующий vhost **`stream2.sla9.ru`** с LE-сертом (действует до 2027-01-06, `certbot.timer` активен, `webroot`).
- UFW: открыты 22/80/443; **8443 пришлось открыть**. Egress свободный (UDP/TCP 53, HTTPS наружу).
- `unbound`/`docker` не были установлены; `dig`, `curl`, `openssl`, `python3` есть.

### Этап A (VPS DoH-сервер) — ✅

- [x] `apt install unbound` (1.19.2). Конфиг `/etc/unbound/unbound.conf.d/doh-guard.conf`: `interface: 127.0.0.1` (только localhost, plain DNS на :53), `access-control` localhost-only, `do-ip6: no`, `hide-identity/version`, `qname-minimisation`, `prefetch`, `harden-*`, кэши `msg 32m / rrset 64m / key 16m / neg 4m`. DNSSEC — из штатного `root-auto-trust-anchor-file.conf`. RAM ~19 МБ.
- [x] `doh-proxy` (jedisct1/doh-server **0.10.0**, из `.deb` amd64) → `/usr/bin/doh-proxy`, свой systemd-юнит `/etc/systemd/system/doh-proxy.service`: `doh-proxy -H stream2.sla9.ru -u 127.0.0.1:53 -l 127.0.0.1:8053`, `DynamicUser`, `ProtectSystem=strict`, без capabilities. RAM ~10 МБ.
- [x] **Токен** (секретный путь): `openssl rand -base64 32` (43 символа, base64url); хранится на VPS в **`/root/doh-token`** (chmod 600). В git **не коммитить** — в плане плейсхолдер `<TOKEN>`.
- [x] nginx: в vhost `stream2.sla9.ru` добавлены `limit_req_zone ... zone=doh:10m rate=100r/s;` и локация
      `location = /<TOKEN>/dns-query { limit_req zone=doh burst=200 nodelay; proxy_pass http://127.0.0.1:8053/dns-query; proxy_http_version 1.1; proxy_set_header Connection ""; ... }`.
      Бэкап оригинала: **`/root/stream2.sla9.ru.orig-20261016`**.
- [x] `ufw allow 8443/tcp comment "DoH nginx"`.
- [x] certbot: deploy-хук `/etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh` (`systemctl reload nginx`), чтобы после автопродления серта nginx подхватил новый.
- [x] `mtg`/443, сайт на 80/8443, fail2ban — не тронуты.

### Ключевая находка (отступление от первоначального плана)

DoH в unbound — **только HTTP/2 (h2c)**, а distro-nginx 1.24 не умеет HTTP/2 к апстриму (`proxy_http_version 2` появился в nginx **1.29.4**, mainline). Поэтому `unbound` отдаёт обычный DNS на `127.0.0.1:53`, а DoH-фронтендом за nginx стал **`doh-proxy`** (HTTP/1.1). Вариант «обновить nginx до mainline» отвергнут ради ненарушения рабочего nginx/`mtg`.

### Этап B (клиент dns-guard) — ✅

- [x] `config.Group.Hosts map[string]string` (`hosts:`) — статический маппинг **хостнейм→IP**, приоритетнее bootstrap; валидация (значения — IP) и нормализация (lowercase/trim). Коммит `682287d`.
- [x] `dialer.go` — `staticHost()`; `doh.go` — обёртка `DialContext` (адрес соединения → IP, хостнейм остаётся в URL для TLS-SNI/проверки серта); `dot.go` — подстановка адреса соединения при сохранении `ServerName`.
- [x] `New(...)`/`NewGroup(...)` прокидывают `hosts`; `config.example.yaml` + `README.md` обновлены.
- [x] Тесты: `config` (нормализация/валидация `hosts`), `upstream` (интеграционный — подстановка на локальный DoH-сервер по неразрешимому имени). `go build/vet/test` — зелёные.
- [x] CI `success` (run `38901514093`, коммит `682287d`) → образ **`nskforward/dns-guard:latest`** (arm64) обновлён.

### Живая проверка VPS (внешняя, через тот же ISP-путь)

- `https://stream2.sla9.ru:8443/<TOKEN>/dns-query` → **200**, валидный `application/dns-message`, TLS-верификация `ssl_verify_result=0` (нормальный LE-серт).
- Чужой путь → **404**; корень сайта → **200** (не сломан).
- DNSSEC: подписанный домен → флаг `ad`; bogus (`sigfail.verteiltesysteme.net`) → `SERVFAIL`. Рекурсия идёт через unbound (`total.num.queries` растёт).
- Порт **8443 провайдером не блокируется**.

### Откат (если понадобится)

- nginx: `cp /root/stream2.sla9.ru.orig-20261016 /etc/nginx/sites-available/stream2.sla9.ru && nginx -t && systemctl reload nginx`; `ufw delete allow 8443/tcp`.
- Сервисы: `systemctl disable --now doh-proxy unbound`; `apt remove unbound`; `rm /usr/bin/doh-proxy /etc/systemd/system/doh-proxy.service`.
- Клиентов и `/ip dns` роутера всё это **не касается** (этап C не выполнялся).

## Сессия 4 — сделано: Этап C (VPS-DoH как primary в контейнере)

**Цель:** перевести контейнер `dns-guard` на роутере на свой DoH как primary; публичные Quad9/AdGuard — fallback. Выполнено.

### Доступы и факты

- VPS: `ssh root@157.228.153.163` (key-auth, BatchMode). Токен: `/root/doh-token` на VPS. DoH-URL: `https://stream2.sla9.ru:8443/<TOKEN>/dns-query`.
- Роутер: SSH `admin@192.168.88.1` (пароль `admin1`, вводится через `expect`; `sshpass` нет). Контейнер `dns-guard`: `interface=veth-dnsguard` (`192.168.89.3`), `mountlists=dns-guard-conf` (`/usb1/dns-guard/conf/config.yaml` → `/etc/dns-guard/config.yaml`), web `http://192.168.89.3:8080`.
- **Важно (снято):** образ в контейнере был обновлён (сборка с поддержкой `hosts`) *до* применения конфига с `hosts`.

### Итог (хроника исполнения)

1. [x] **Образ обновлён.** Контейнер на роутере остановлен/удалён (`/container stop` + `/container remove`; `usb1/images/dns-guard` RouterOS удалил сам вместе с container store), затем пересоздан:
      `/container/add remote-image=nskforward/dns-guard:latest interface=veth-dnsguard mountlists=dns-guard-conf root-dir=/usb1/images/dns-guard dns=192.168.88.1 start-on-boot=yes logging=yes name=dns-guard`.
      Первый pull упал (`check registry failed: resolving error` — флап апстрима, как в сессии 2), со второй попытки образ скачался; после `download/extract done` контейнер оказался в статусе STOPPED — запущен вручную `/container/start`. Новый `image-id=a73c9486…` (старый был `d7fafdd6…`), старт чистый: `groups=1 rules=0 cache=false`.
2. [x] **Токен** считан на VPS из `/root/doh-token` (в git/план не попал — плейсхолдер `<TOKEN>`).
3. [x] **Конфиг залит по SFTP ДО старта** в `/usb1/dns-guard/conf/config.yaml` (актуальный — `tmp/router-config.yaml`, gitignored):
   ```yaml
   listen:  {udp: ":53", tcp: ":53"}
   web:     {enabled: true, listen: ":8080"}
   cache:   {enabled: false, max_entries: 10000, min_ttl: 3600, max_ttl: 0}
   upstream:{timeout: 3s}
   groups:
     a:
       protocol: doh
       primary:  ["https://stream2.sla9.ru:8443/<TOKEN>/dns-query"]
       fallback: ["https://9.9.9.9/dns-query", "https://94.140.14.14/dns-query", "https://1.1.1.1/dns-query"]
       hosts: {stream2.sla9.ru: 157.228.153.163}
   default_group: a
   rules: []
   ```
4. [x] **Приёмка** (см. ниже) — все пункты пройдены.
5. [x] **Дозаполнена приёмка сессии 2:** TCP-`dig` и RAM (см. чек-лист этапа 2.3).
6. [x] **Откат** — не потребовался. Резерв: `PUT /api/config` со старым конфигом (публичные DoH) или пересоздание контейнера с `remote-image=nskforward/dns-guard:sha-6f4a6b3` (образ сессии 2).

### Результаты приёмки (живой прогон)

- **UDP и TCP/53:** `dig @192.168.89.3 example.com` и `dig +tcp …` → `NOERROR`, `ANSWER: 2`, **TTL 3600** (MIN-TTL), флаг `ad` (DNSSEC от unbound), EDNS `udp:1232` (признак ответа VPS, а не fallback).
- **Статистика:** `GET /api/stats` → `groups:{a:N}`, `cache_len:0` (кэш выключен, как задумано), `total` растёт. Из 38 запросов — `servfail:2` (только первые два сразу после старта контейнера; далее без ошибок).
- **Primary реально используется:** серия из 12 запросов на уникальные имена → на VPS `unbound-control stats` `total.num.queries` вырос ровно на 12 (25→37).
- **Failover:** `systemctl stop doh-proxy` на VPS → `dig @192.168.89.3` по-прежнему отвечает (`NOERROR` через fallback), счётчик unbound не растёт; `systemctl start doh-proxy` → следующий запрос снова primary (37→38, EDNS 1232). Вернули как было.
- **Ресурсы:** RAM контейнера `~10.6–11.9 MiB`; VPS `free -h` — 616 MiB available, `nginx`/`doh-proxy`/`unbound` — `active`.
- **Замечание:** первые 1–2 запроса к контейнеру сразу после пересоздания вернули `SERVFAIL` (ошибки обоих апстримов), затем всё стабильно. Похоже на короткий прогрев сетевого пути/`conntrack` после рестарта контейнера — не блокер; при желании можно добавить ретрай на старте.

### Открытый вопрос — ✅ решён в сессии 6

- **`to_vpn_list`:** выбран вариант топологии «роутер → контейнер» (`/ip dns set servers=192.168.89.3`), `dynamic-servers` от ISP убран; `to_vpn_list` наполняют статические FWD-записи роутера (резолвятся через контейнер). Подробности — «Сессия 6 — сделано».

### Definition of Done этапа C — ✅ выполнено

- [x] `dig` UDP и TCP через контейнер отдают ответы (TTL ≥ 3600), статистика растёт.
- [x] Трафик резолва идёт через VPS-рекурсор (подтверждено `unbound-control stats`), fallback проверен отключением `doh-proxy`.
- [x] Клиенты и `/ip dns` роутера при этом не затронуты; петли нет.

### Следующий шаг

Единственный оставшийся пункт — **топология `to_vpn_list`** — решён в сессии 6. Всё остальное по изначальному запросу закрыто: контейнер на роутере резолвит через собственный DoH на VPS, MIN-TTL применяется, статистика и веб-UI работают.

## Сессия 5 — сделано: зона .RU через головной роутер 192.168.1.1

**Цель:** резолвинг доменов `.ru` через сервер головного роутера `192.168.1.1` (провайдерский), все остальные запросы — на VPS-DoH; при отказе `192.168.1.1` — публичный DoH-fallback.

### Находки

- **Находка сессии 2 не воспроизвелась:** контейнер → `192.168.1.1:53` по UDP **работает**. Дифф-проба с Mac в LAN MikroTik (тот же masquerade, что у контейнера) → `NOERROR`; затем то же подтвердил сам контейнер. Головной роутер отвечает NAT-ед запросам, `192.168.1.1` пригоден как апстрим.
- **Reply-TTL у ISP маленькие** (напр. `yandex.ru` = 25s), поэтому MIN-TTL 3600 поднимает их до часа — ожидаемое (глобальное) поведение.
- **Баг бюджета запроса (исправлен):** `upstream.timeout` работал как *общий* бюджет запроса, поэтому «висящий» primary (мёртвый UDP-адрес) съедал всё время, и fallback падал с `context deadline exceeded`. В тесте сессии 4 это не проявлялось, т.к. там primary падал *быстро* (остановленный `doh-proxy`).

### Реализация (коммиты)

- `e6e1287` — **префикс протокола в адресе апстрима** (`<udp|dot|doh>+`): `config.SplitProtocol` + разбор в `upstream.New`. Позволяет смешивать протоколы в одной группе (UDP primary + DoH fallback). Валидация, юнит-тесты (`config`, `upstream`), README и `config.example.yaml`.
- `7b24e80` — **бюджет запроса** = `upstream.timeout × число попыток группы` (`internal/server`), чтобы fallback успевал сработать даже при таймауте primary. Per-attempt timeout остаётся у резолверов. Тест `TestBuildStateRequestBudget`.

Оба образа собраны CI (GitHub Actions, arm64) и опубликованы в Docker Hub (`latest` + `sha-*`).

### Деплой и целевой конфиг

Контейнер пересоздан на `nskforward/dns-guard:latest` (образ `sha-7b24e80`). Первый `pull` упал (`fetch layer failed: timeout connecting` — флап апстрима Docker Hub), со второй попытки — успех (как в сессиях 2/4). Целевой конфиг — `tmp/router-config.yaml`:

```yaml
cache: {enabled: false, min_ttl: 3600, max_ttl: 0}
groups:
  a:  # всё остальное → VPS DoH
    protocol: doh
    primary:  ["https://stream2.sla9.ru:8443/<TOKEN>/dns-query"]
    fallback: ["https://9.9.9.9/dns-query", "https://94.140.14.14/dns-query", "https://1.1.1.1/dns-query"]
    hosts: {stream2.sla9.ru: 157.228.153.163}
  ru: # зона .ru → головной роутер; при отказе → публичный DoH
    protocol: udp
    primary:  ["192.168.1.1:53"]
    fallback: ["doh+https://9.9.9.9/dns-query", "doh+https://94.140.14.14/dns-query"]
default_group: a
rules:
  - zone: ru
    group: ru
```

### Приёмка (живой прогон)

- `.ru` по **UDP и TCP** → `NOERROR`, TTL **3600**, **без флага `ad`** (ISP-резолвер, не валидирует DNSSEC); адреса совпадают с прямым `dig @192.168.1.1`.
- Остальное → VPS-DoH: флаг `ad`, EDNS `udp:1232` (признак unbound на VPS).
- Статистика: `groups: {a: N, ru: M}` — обе группы реально используются; на серии из 12 запросов `servfail` не вырос.
- **Fallback:** primary заменён на мёртвый `192.168.1.250:53` (`PUT /api/config`) → `.ru` всё равно отвечает `NOERROR` через публичный DoH (после фикса бюджета). Возврат рабочего конфига — один PUT.
- RAM контейнера `~0.5 MiB` сразу после старта, стабилизируется до ~10–12 MiB.

### Замечания / ограничения

- `.ru` через ISP: нет DNSSEC-валидации (`ad`), MIN-TTL 3600 глобален (per-group не переопределяется без доработки).
- `to_vpn_list` этим **не решается** (контейнер по-прежнему не проходит через резолвер MikroTik) — на тот момент открытый вопрос; **решён в сессии 6**.
- Счётчик `unbound-control stats` на VPS как доказательство не использовался: SSH к VPS с Mac периодически недоступен (нестабильный путь); основной маркер — флаг `ad`.

### Откат

- `PUT /api/config` со старым конфигом (`tmp/router-config-rollback-s4.yaml`) — вернуть один VPS-DoH без зоны `.ru`.
- При необходимости — пересоздание контейнера с образом предыдущего `sha` (напр. `sha-f71cc45`).

## Сессия 6 — сделано: топология «роутер → контейнер» (закрыт вопрос `to_vpn_list`)

**Цель:** перевести DNS-резолвер роутера с DNS от ISP на контейнер `dns-guard`, чтобы весь клиентский DNS шёл через него (зона `.ru`, MIN-TTL, статистика), и чтобы `to_vpn_list` по-прежнему наполнялся (через статические FWD-записи роутера). Это выбранный вариант топологии «роутер → контейнер».

### Изменения на роутере (2 команды)

1. `/ip dhcp-client set [find interface=ether1] use-peer-dns=no` — убран `dynamic-servers=192.168.1.1` от ISP (иначе RouterOS использовал бы оба сервера параллельно, часть запросов шла бы мимо контейнера).
2. `/ip dns set servers=192.168.89.3` — апстрим роутера = контейнер.

Не трогали: DHCP-сервер (`dns-server=192.168.88.1` — клиенты по-прежнему шлют на роутер), `allow-remote-requests`, кэш роутера, static-записи, mangle.

### Целевая цепочка

```
LAN-клиенты → 192.168.88.1 (резолвер RouterOS, как и раньше)
            → 192.168.89.3 (контейнер dns-guard)
            → группа a: VPS-DoH      (не-.ru)
            → группа ru: 192.168.1.1 (зона .ru) + публичный DoH-fallback
```

### Приёмка (живой прогон)

- **Препроверка направления роутер→контейнер:** `:put [:resolve server=192.168.89.3 example.com]` → `104.20.23.154` (в сессии 2 не работало обратное направление; это — рабочее).
- `/ip dns print`: `servers=192.168.89.3`, `dynamic-servers` пуст сразу после команды (renew не понадобился).
- `:resolve example.com` → `104.20.23.154`; `:resolve yandex.ru` → `5.255.255.77` — обе ветки отвечают.
- С Mac (`dig @192.168.88.1`): не-.ru → TTL ≈3600; `.ru` → TTL ≈3600 (ISP-группа). Напрямую к контейнеру (`dig @192.168.89.3`) ответ VPS несёт флаг `ad` + EDNS `udp:1232`; через резолвер роутера AD-бит снимается (норма RouterOS), TTL декеится — ожидаемо.
- Сверка: `example.com` через контейнер совпал с публичным DoH Cloudflare (`104.20.23.154`, `172.66.147.243`) — anomaly нет.
- UDP и TCP/53 через роутер — `NOERROR`.
- **Кэш роутера дедуплицирует:** 4 повтора `www.iana.org` → дельта контейнера 0; `/ip dns cache` наполняется.
- **`to_vpn_list` наполняется через контейнер:** резолв `chatgpt.com`/`openai.com` через роутер создал dynamic-записи `created for chatgpt.com/openai.com` с Cloudflare-IP (`104.18.32.47`, `172.64.155.209`, `172.64.154.211`). FWD-записей для `to_vpn_list` = 35, ни одна не имеет собственного `server=` (все идут через апстрим роутера = контейнер).
- **Статистика:** `/api/stats` — обе группы растут (`a`, `ru`), `servfail` в спокойном режиме не увеличивается (31 запрос подряд → дельта 0).

### Наблюдение (не блокер)

- В момент всплеска клиентского фона (браузеры, ~90 запросов/мин) зафиксирован кратковременный рост `servfail` (1→14): падали цепочки и группы `a` (VPS), и `ru` (в т.ч. primary `192.168.1.1` и публичные DoH-fallback — `94.140.14.14`, `1.1.1.1` таймаутили). Через минуту всё восстановилось; далее `servfail` стабилен. Наиболее вероятная причина — транзиентный сетевой сбой/троттлинг ISP-резолвера и публичных DoH под burst (публичные DoH отмечены как нестабильные ещё в сессии 2). Кэш контейнера выключен намеренно (кэширует роутер), поэтому первый проход burst идёт в апстрим. Наблюдать при нагрузке.

### Ограничения / соображения

- У роутера теперь **единственный** апстрим (контейнер). Если контейнер остановлен — DNS у клиентов пропадёт (у контейнера `start-on-boot=yes` и свои primary/fallback). Осознанный выбор: второй сервер (ISP) вернул бы недетерминизм.
- `.ru` через ISP: нет DNSSEC-валидации (`ad`); MIN-TTL 3600 глобален.
- DNSSEC-флаг `ad` от VPS через резолвер RouterOS до клиента не доходит (снимается MikroTik).

### Откат

`/ip dns set servers=""` + `/ip dhcp-client set [find interface=ether1] use-peer-dns=yes` (+ при необходимости `/ip dhcp-client renew ether1`) — вернёт `dynamic-servers=192.168.1.1`. Клиенты и контейнер не затрагиваются.

## Состояние на начало сессии 2

- Код (этапы 0–6) и локальная часть этапа 7 — зелёные (`go build/vet/test`), см. «Состояние на конец сессии 1».
- Роутер доступен по SSH; аудит выполнен (см. «Контекст»), все блокеры сессии 1 по доступу сняты.
- Закрытые вопросы сессии 1:

  1. **Петля.** В режиме cache-only апстрим контейнера = DNS роутера; `/ip dns servers` роутера на контейнер не указывает → петли нет. Кто обращается к `192.168.89.3` в сессии 2 — только диагностика (`dig` с Mac); DHCP-DNS клиентов не трогаем. `to_vpn_list` наполняется, т.к. запросы приходят в DNS роутера от контейнера.
  2. **Доступ.** SSH к роутеру работает (`admin`/`admin1`, `expect`); образ публикуется через GitHub Actions → доступен роутеру из Docker Hub. Локальный Docker-демон не нужен.
  3. **Двойной кэш.** В сессии 2 кэш сервиса выключен — кэширует только RouterOS; пересечения нет. Вопрос повторно рассматривается при переходе на топологию «контейнер впереди» (тогда кэш нужен для MIN-TTL на повторах и точной статистики).

## Состояние на конец сессии 1 (архив)

Код готов и проверен локально: этапы 0–6 полностью, этап 7 — вся локальная часть.

- `go build ./...`, `go vet ./...`, `go test ./...` — зелёные.
- Юнит-тесты: `rules`, `cache`, `config`, `stats`, `server` (fake-апстрим).
- Локальный smoke (через фейковый апстрим `127.0.0.1:5355`; временные файлы — в gitignored `tmp/`): UDP и TCP отдают ответы; MIN-TTL (5→60); кэш miss→hit (`cache_len:1`); `PUT /api/config` применяется и сохраняется в файл; невалидный конфиг → 400; `POST /api/cache/flush` → ok.
- Отступление: Docker-образ работает от root (не nonroot) — nonroot не займёт порт 53 без `CAP_NET_BIND_SERVICE` (см. комментарий в `Dockerfile`).
