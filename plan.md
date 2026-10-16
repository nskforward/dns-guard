# dns-guard — план реализации

DNS-сервис для MikroTik RouterOS (ARM64 контейнер): принимает запросы по UDP/53
(и TCP/53), маршрутизирует их по правилам зон на группы вышестоящих серверов
(UDP / DoT / DoH) с primary+fallback, применяет MIN-TTL, кэширует ответы
в памяти и показывает статистику (top-100 доменов с пиковым RPM) в веб-UI.

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
| Топология сессии 2 | **Cache-only, без переключения клиентов.** Апстрим контейнера = DNS роутера (`192.168.88.1`); DHCP-DNS и `/ip dns` **не меняем** — петля исключена, `to_vpn_list` наполняется |
| Сборка/публикация образа | **GitHub Actions** (buildx, `linux/arm64`) → Docker Hub `nskforward/dns-guard`; локальный Docker-демон не требуется |

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
- [ ] `.github/workflows/docker.yml` — сборка и push `nskforward/dns-guard:latest` (arm64) через Actions

### Этап 7. Тесты и приёмка — ⏳ частично (осталось на роутере)

- [x] юнит-тесты: `rules.Match` (longest-suffix, `*.ru`), `cache` (MIN-TTL/декей/негатив), `stats` (peak RPM, rollover), `config`, `server` (fake-апстрим)
- [x] `go vet ./...`, `go build ./...`, `go test ./...`
- [x] локальный smoke: конфиг + `dig @127.0.0.1` по UDP и TCP
- [x] проверка MIN-TTL: апстрим с малым TTL (5s) → ответ ≥ min_ttl (60s)
- [x] проверка fallback: недоступный primary → ответ через fallback
- [ ] сборка arm64-образа через GitHub Actions, публикация в Docker Hub и загрузка на роутер
- [ ] живая приёмка на роутере: `dig @192.168.89.3` (UDP/TCP), TTL ≥ 3600, `/api/stats`, рост `to_vpn_list`, RAM (`/container print` memory-current)

---

## Известные риски / решения

1. **VPN-DNS-связка** — точку внедрения выбирать так, чтобы запросы всё равно проходили через DNS роутера (он наполняет `to_vpn_list`). В режиме «только кэш» апстрим контейнера = DNS роутера, поэтому `/ip dns servers` роутера на контейнер НЕ переключаем (иначе петля) — см. вопросы сессии 2.
2. **Egress DoH из контейнера** идёт напрямую через WAN (как сейчас у роутера); при необходимости VPN для DNS — отдельные mangle-правила (вне текущего объёма).
3. **Двойной кэш** — в сессии 2 кэш сервиса выключен, кэширует только RouterOS. При переходе на топологию «контейнер впереди» кэш контейнера включается: он нужен для MIN-TTL на повторах и точной статистики; кэш роутера тогда работает как дедуп позади (не дублирование).
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

## Следующий шаг (сессия 2) — согласованный план

### Этап 2.1. CI: сборка и публикация образа

- [x] `.github/workflows/docker.yml`: триггеры `push` в `main` + `workflow_dispatch`; `docker/setup-buildx-action` (+ QEMU), `docker/build-push-action` с `platforms: linux/arm64`, `push: true`; теги `latest` и `sha-<short>`; кэш слоёв через `type=gha`; логин по секретам **`DOCKER_USERNAME` / `DOCKER_PASSWORD`**.
- [x] Первичный коммит проекта и `git push -u origin main` (коммит `f8b975e`).
- [ ] **Блокер:** ран `38893687113` падает на шаге **Log in to Docker Hub** (следующие шаги пропущены). Нужно проверить секреты: `DOCKER_USERNAME` = аккаунт Docker Hub, `DOCKER_PASSWORD` = **access token** (не пароль аккаунта; парольная аутентификация на Docker Hub отключена). Также убедиться, что секреты заданы на уровне репозитория (не только organization/environment).
- [ ] Перезапустить ран после исправления секретов.
- [ ] Проверить, что репозиторий Docker Hub **публичный** (роутер тянет анонимно): `curl https://hub.docker.com/v2/repositories/nskforward/dns-guard/`. Фолбэк при приватном — прописать creds в `/container/config` роутера.

### Этап 2.2. Деплой на роутер (SSH `admin@192.168.88.1`, через `expect`)

- [ ] `veth-dnsguard 192.168.89.3/24 gateway=192.168.89.1` + `/interface bridge port add bridge=container interface=veth-dnsguard`.
- [ ] Конфиг залить по SFTP в `/usb1/dns-guard/conf/config.yaml`:
  ```yaml
  listen:  {udp: ":53", tcp: ":53"}
  web:     {enabled: true, listen: ":8080"}
  cache:   {enabled: false, max_entries: 10000, min_ttl: 3600, max_ttl: 0}
  upstream:{timeout: 3s}
  groups:
    a: {protocol: udp, primary: ["192.168.88.1"]}
  default_group: a
  rules: []
  ```
- [ ] `/container/mounts add list=dns-guard-conf src=/usb1/dns-guard/conf dst=/etc/dns-guard mode=rw` (у `mounts` список задаётся через `list=`; у контейнера свойство — `mountlists=`).
- [ ] `/container/add remote-image=nskforward/dns-guard:latest interface=veth-dnsguard mountlists=dns-guard-conf root-dir=/usb1/images/dns-guard dns=192.168.88.1 start-on-boot=yes logging=yes` → `/container/start` (у add нет параметров `os`/`arch` — определяются автоматически).
- [ ] Диагностика: `/container print`, `/log print where topics~"container"`.

### Этап 2.3. Приёмка

- [ ] `dig @192.168.89.3 <домен>` (UDP) и `+tcp` — ответы есть.
- [ ] MIN-TTL: сравнить TTL у роутера (`@192.168.88.1`, мелкий TTL) и у контейнера → **≥ 3600**.
- [ ] `curl http://192.168.89.3:8080/api/stats` — `total` растёт, домены в top-100 видны (`cache_len=0` — ожидаемо, кэш выключен).
- [ ] `to_vpn_list`: `count-only` было 23 → растёт после резолва доменов из FWD-списка.
- [ ] RAM: `/container print` memory-current (~10–15 MiB ожидаемо).
- [ ] `PUT /api/config` → hot-reload без рестарта.
- [ ] **Откат:** `/container stop` + `/container remove` + veth + mount. Клиенты и `/ip dns` не затронуты.

### Прогресс сессии 2 (хроника)

- SSH к роутеру заработал (сеть Mac периодически флапает между LAN роутера и сетью провайдера — на деплой не влияет, т.к. нужен только LAN).
- Каталог `usb1/volumes/` отсутствует; конфиг размещён в **`/usb1/dns-guard/conf/config.yaml`** (694 байта, загружен по SFTP).
- Создано: `veth-dnsguard 192.168.89.3/24` + порт в бридж `container`; mount `list=dns-guard-conf src=/usb1/dns-guard/conf dst=/etc/dns-guard mode=rw`.
- Контейнер `dns-guard` добавлен (`remote-image=nskforward/dns-guard:latest`, `mountlists=dns-guard-conf`, `root-dir=/usb1/images/dns-guard`, `dns=192.168.88.1`, `start-on-boot=yes`, `logging=yes`).
- **Блокер:** образ не скачивается — `fetch manifest failed ... auth error`; репозиторий `nskforward/dns-guard` на Docker Hub пока не существует (проверено через `hub.docker.com API` → 404). Причина — ран CI упал на логине в Docker Hub (см. этап 2.1).

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
