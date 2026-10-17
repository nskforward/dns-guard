# dns-guard

Небольшой DNS-сервис для MikroTik RouterOS (ARM64-контейнер). Принимает запросы
по **UDP/53** и **TCP/53**, маршрутизирует их по правилам зон на группы
вышестоящих серверов (**UDP / DoT / DoH**) с primary+fallback, применяет
MIN/MAX-TTL, кэширует ответы в памяти и показывает статистику (полные доменные
имена с пиковым RPM, нагрузка по группам и апстримам, поиск и пагинация) в
веб-интерфейсе.

## Возможности

- Приём DNS по UDP и TCP на порту 53.
- Маршрутизация по зонам (longest-suffix match): `ru`, `*.ru`, `example.com`.
- Группы апстримов с протоколом на группу: `udp` / `dot` / `doh`.
- Primary + fallback: резервные серверы используются при ошибке запроса к
  основным, а также когда основной ответил NODATA (NOERROR без записей) на
  **A/AAAA** — такой ответ проверяется резервными, и первый положительный ответ
  выигрывает. Для остальных типов (HTTPS/SVCB и т.п.) NODATA штатен и резерв не
  привлекается. Фоновых health-check нет; основные — с ротацией.
- Bootstrap-резолв хостнеймов DoH/DoT через заданный plain-DNS либо
  статический маппинг `hosts` (хостнейм→IP).
- In-memory LRU-кэш с границами TTL (MIN-TTL пол, MAX-TTL потолок),
  кэширование негативных ответов (по SOA minttl) с отдельным потолком
  `neg_ttl`. Сброс при рестарте.
- Дедупликация одинаковых одновременных запросов (singleflight): параллельные
  промахи обслуживаются одним обращением к апстриму.
- Автоматический повтор усечённого (TC) UDP-ответа апстрима по TCP.
- Горячая перезагрузка конфигурации (SIGHUP или через веб-UI), без простоя.
- Веб-UI (на английском) и REST API: статистика по полным доменам с пиковым
  RPM, поиск, пагинация и сводные сведения по домену — все известные записи,
  склеенные из успешных ответов по всем типам запросов (A, AAAA, HTTPS, ...),
  отдельная вкладка Upstreams с нагрузкой по группам и серверам (успешные и
  неудачные попытки), правка конфига.
- Один статический бинарь, без CGO, работает в distroless.

## Сборка и запуск (локально)

```sh
go build ./...
go test ./...

cp config.example.yaml config.yaml
./dns-guard -config config.yaml
```

Флаги:

| Флаг | По умолчанию | Описание |
|---|---|---|
| `-config` | `config.yaml` | путь к файлу конфигурации |
| `-log-level` | `info` | `debug` / `info` / `warn` / `error` |

Проверка:

```sh
dig @127.0.0.1 example.com A     # UDP
dig @127.0.0.1 example.com A +tcp # TCP
curl -s localhost:8080/api/health
```

## Конфигурация

См. `config.example.yaml`. Ключевые секции:

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
  min_ttl: 60      # пол TTL (секунды)
  max_ttl: 0       # 0 = без верхней границы
  neg_ttl: 300     # потолок TTL негативных ответов, 0 = без потолка
stats:
  max_domains: 50000  # лимит уникальных доменов в таблице статистики
upstream:
  timeout: 3s      # таймаут одной попытки; общий бюджет = timeout × число попыток
groups:
  a:
    protocol: udp            # udp | dot | doh
    primary:  ["77.88.8.8", "8.8.8.8"]
    fallback: ["1.1.1.1"]
  b:
    protocol: udp            # UDP primary + DoH fallback через префикс
    primary:  ["192.168.1.1:53"]
    fallback: ["doh+https://9.9.9.9/dns-query"]
  c:
    protocol: doh
    primary:  ["https://doh.example.com:8443/secret/dns-query"]
    fallback: ["https://9.9.9.9/dns-query"]
    hosts: {"doh.example.com": "203.0.113.10"} # статический хостнейм→IP
default_group: a
rules:
  - zone: "ru"
    group: b
```

- `zone` нормализуется: `ru`, `*.ru`, `.ru` и `RU.` эквивалентны.
- Для `dot`/`doh` указывайте хостнейм (`dns.google`) или URL
  (`https://dns.google/dns-query`). Порт по умолчанию: 53 (udp), 853 (dot).
- Любой адрес апстрима может нести префикс `<udp|dot|doh>+`, который
  перекрывает протокол группы. Это позволяет смешивать протоколы внутри одной
  группы — например, UDP primary и DoH fallback:
  ```yaml
  b:
    protocol: udp
    primary:  ["192.168.1.1:53"]
    fallback: ["doh+https://9.9.9.9/dns-query"]
  ```
  Префиксом считается только ведущий токен без `/`, `:`, `?`, поэтому знак `+`
  внутри URL (`...?a=1+2`) префиксом не воспринимается. Неизвестный префикс
  (`tcp+…`) — ошибка валидации.
- `bootstrap` — plain-DNS для резолва хостнеймов апстримов. `hosts` —
  статический маппинг хостнейм→IP (приоритетнее bootstrap): полезен, когда
  plain-DNS до резолвера недоступен. Хостнейм остаётся в URL для TLS-SNI и
  проверки сертификата, а соединение идёт на указанный IP. `hosts` действует на
  всю группу, включая fallback другого протокола.
- `cache.neg_ttl` — потолок TTL негативных ответов (NXDOMAIN и NODATA по SOA):
  ошибочный негативный ответ апстрима не «консервируется» в кэше дольше
  указанного времени. `0` — не ограничивать (действуют только `max_ttl` и
  SOA minttl).
- `upstream.timeout` — таймаут **одной** попытки. Общий бюджет запроса —
  `timeout` × число серверов в выбранной группе (primary+fallback), поэтому
  fallback успевает сработать даже при «висящем» primary, а не только при
  быстром отказе.
- Конфигурация применяется целиком: при изменении крайне рекомендуется
  оставить `default_group` существующим.

## REST API

| Метод | Путь | Описание |
|---|---|---|
| GET | `/api/health` | `{"status":"ok"}` |
| GET | `/api/stats` | общие счётчики, uptime, размер кэша, распределение по группам |
| GET | `/api/domains` | страница доменов: `?page=1&limit=50&search=подстрока` |
| GET | `/api/domain` | статистика домена и сводные сведения: все известные записи по домену (склейка успешных ответов по qtype): `?name=example.com` |
| GET | `/api/upstreams` | статистика по группам и апстримам: запросы, кэш-хиты, успешные и неудачные попытки |
| GET | `/api/servfail` | детали SERVFAIL: домены с числом ошибок и последней ошибкой (`?page=1&limit=50`), журнал последних 100 событий |
| GET | `/api/config` | `{"yaml":"..."}` текущая эффективная конфигурация |
| PUT | `/api/config` | тело — YAML; валидируется, применяется на лету и сохраняется в файл |
| POST | `/api/cache/flush` | очистить кэш |

Веб-интерфейс показывает полные доменные имена (без сокращения до домена
второго уровня), позволяет искать по подстроке без учёта регистра,
переключать страницы (50 записей на страницу) и по клику на домен
просматривать сводные сведения о нём: все известные записи (Answer и
Authority), склеенные из последних успешных ответов по всем типам запросов
(A, AAAA, HTTPS, ...), с TTL, а также атрибуты самого свежего ответа —
`Status` (OK), источник, апстрим (включая ответы из кэша — сохраняется сервер,
выдавший исходный ответ) и время. Негативные ответы (NXDOMAIN, NODATA) в
сводке не отображаются и не затирают ранее полученные записи; ошибки
SERVFAIL — на отдельной вкладке. Вкладка Upstreams
показывает для
каждой группы число запросов, кэш-хитов, успешных ответов и ошибок, а по
клику на группу — разбивку по конкретным серверам с их долей нагрузки.
Счётчик SERVFAIL на вкладке Stats кликабелен и открывает вкладку SERVFAIL:
домены с числом ошибок, долей от общего числа SERVFAIL и последней ошибкой
(по клику на домен — его сводные сведения), а также журнал последних 100
событий с временем, типом, группой, апстримом, источником (кэш/апстрим) и
текстом ошибки.

Пример:

```sh
curl -s localhost:8080/api/stats | jq
curl -s 'localhost:8080/api/domains?page=1&limit=50&search=example' | jq
curl -s 'localhost:8080/api/domain?name=example.com' | jq
curl -s localhost:8080/api/upstreams | jq
curl -s -X PUT localhost:8080/api/config \
  --data-binary @config.yaml -H 'Content-Type: application/x-yaml'
```

## Деплой на MikroTik RouterOS (hAP ax³, arm64)

> Обновление уже установленного контейнера до новой версии — см.
> [`deploy.md`](deploy.md) (включая постоянное решение проблемы с DNS
> при `update`).

Схема — **«роутер → контейнер»**: сам роутер остаётся основным DNS для LAN,
а его upstream-сервером становится dns-guard. Это критично, если DNS управляет
VPN-маршрутизацией через `address-list` (см. предупреждение ниже).

### 1. Подготовка образа

```sh
docker buildx build --platform linux/arm64 -t <user>/dns-guard:latest --push .
```

### 2. Сеть контейнера

Бридж `container` уже имеет адрес `192.168.89.1/24` и маскарад. Добавьте veth
контейнеру:

```rsc
/interface/veth/add name=veth-dnsguard address=192.168.89.3/24 gateway=192.168.89.1
/interface/bridge/port/add bridge=container interface=veth-dnsguard
```

### 3. Тома и конфиг

На USB-накопителе (`usb1/volumes/...`) создайте каталог и положите конфиг:

```rsc
/container/mounts/add name=dnsguard-conf src=usb1/volumes/usb1-something/dns-guard list=dnsguard-conf
```

Скопируйте в этот каталог `config.yaml` (по образцу `config.example.yaml`).
Внутри контейнера конфиг должен быть доступен как `/etc/dns-guard/config.yaml`.

### 4. Запуск контейнера

```rsc
/container/add \
  remote-image=user/dns-guard:latest \
  interface=veth-dnsguard \
  root-dir=usb1/volumes/usb1-something/dns-guard-root \
  mounts=dnsguard-conf \
  envlist=... \
  start-on-boot=yes \
  comment="dns-guard"
```

> `usb1/volumes/...` и `remote-image` замените на свои значения. Если образ
> требует указания архитектуры — RouterOS подтянет arm64 автоматически.

Проверьте, что сервис поднялся:

```rsc
/container/print
/log/print where topics~"container"
```

### 5. Переключение DNS роутера на контейнер

```rsc
/ip/dns/set servers=192.168.89.3 use-doh-server=""
```

Откат — вернуть прежние серверы:

```rsc
/ip/dns/set servers=77.88.8.8,8.8.8.8
```

Веб-UI будет доступен по адресу `http://192.168.89.3:8080`.

### 6. Проверка VPN-маршрутизации

Трафик DNS, идущий **транзитом через роутер** (LAN-клиенты → роутер → dns-guard),
должен по-прежнему наполнять `to_vpn_list` через статические `FWD`-записи DNS.
Проверьте рост списка:

```rsc
/ip/firewall/address-list/print where list=to_vpn_list
```

## Предупреждение: DNS и VPN

На этом роутере DNS участвует в маршрутизации в VPN: статические `FWD`-записи
DNS с `address-list=to_vpn_list` наполняют список, а mangle уводит эти IP в
AmneziaWG. Поэтому внедрять dns-guard нужно **только** по схеме
«роутер → контейнер» (через `/ip dns set servers=...`), **не** меняя DNS в DHCP
для клиентов напрямую на контейнер. Иначе `to_vpn_list` перестанет наполняться
и часть маршрутизации в VPN отвалится.

## Архитектура

```
клиент --UDP/TCP 53--> dns-guard
   1. матчинг правила по зоне (longest-suffix) -> группа
   2. cache lookup (key: qname|qtype|qclass|DO клиента)
        hit  -> TTL-декей -> ответ (+stats)
   3. miss -> singleflight: группа primary (ротация) -> fallback
        TC по UDP -> повтор по TCP; NODATA primary (A/AAAA) -> проверка fallback
        ошибка запроса ко всем -> SERVFAIL
   4. Apply MIN-TTL/MAX-TTL ко всем секциям, neg_ttl для негативных ответов
   5. cache.Put (ключ по вопросу клиента), stats.Record + stats.RecordUpstream
   6. каждому ожидающему запросу — своя копия ответа с его Id/вопросом
```

Слои: `internal/config`, `internal/rules`, `internal/upstream`, `internal/cache`,
`internal/stats`, `internal/server`, `internal/web`.

## Тесты

```sh
go vet ./...
go test ./...
```
