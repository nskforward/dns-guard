# dns-guard

Небольшой DNS-сервис для MikroTik RouterOS (ARM64-контейнер). Принимает запросы
по **UDP/53** и **TCP/53**, маршрутизирует их по правилам зон на группы
вышестоящих серверов (**UDP / DoT / DoH**) с primary+fallback, применяет
MIN/MAX-TTL, кэширует ответы в памяти и показывает статистику (top-100 доменов
с пиковым RPM) в веб-интерфейсе.

## Возможности

- Приём DNS по UDP и TCP на порту 53.
- Маршрутизация по зонам (longest-suffix match): `ru`, `*.ru`, `example.com`.
- Группы апстримов с протоколом на группу: `udp` / `dot` / `doh`.
- Primary + fallback: резервные серверы используются **только при ошибке
  запроса** к основным (фоновых health-check нет); основные — с ротацией.
- Bootstrap-резолв хостнеймов DoH/DoT через заданный plain-DNS.
- In-memory LRU-кэш с границами TTL (MIN-TTL пол, MAX-TTL потолок),
  кэширование негативных ответов (по SOA minttl). Сброс при рестарте.
- Горячая перезагрузка конфигурации (SIGHUP или через веб-UI), без простоя.
- Веб-UI и REST API: статистика, top-100 доменов с пиковым RPM, правка конфига.
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
upstream:
  timeout: 3s
groups:
  a:
    protocol: udp            # udp | dot | doh
    primary:  ["77.88.8.8", "8.8.8.8"]
    fallback: ["1.1.1.1"]
  b:
    protocol: doh
    primary:  ["https://dns.google/dns-query"]
    fallback: ["https://cloudflare-dns.com/dns-query"]
    bootstrap: ["77.88.8.8"] # plain-DNS для резолва хостнеймов
default_group: a
rules:
  - zone: "ru"
    group: b
```

- `zone` нормализуется: `ru`, `*.ru`, `.ru` и `RU.` эквивалентны.
- Для `dot`/`doh` указывайте хостнейм (`dns.google`) или URL
  (`https://dns.google/dns-query`). Порт по умолчанию: 53 (udp), 853 (dot).
- Конфигурация применяется целиком: при изменении крайне рекомендуется
  оставить `default_group` существующим.

## REST API

| Метод | Путь | Описание |
|---|---|---|
| GET | `/api/health` | `{"status":"ok"}` |
| GET | `/api/stats` | счётчики, uptime, размер кэша, `top` (до 100 доменов) |
| GET | `/api/config` | `{"yaml":"..."}` текущая эффективная конфигурация |
| PUT | `/api/config` | тело — YAML; валидируется, применяется на лету и сохраняется в файл |
| POST | `/api/cache/flush` | очистить кэш |

Пример:

```sh
curl -s localhost:8080/api/stats | jq
curl -s -X PUT localhost:8080/api/config \
  --data-binary @config.yaml -H 'Content-Type: application/x-yaml'
```

## Деплой на MikroTik RouterOS (hAP ax³, arm64)

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
   2. cache lookup (key: qname|qtype|qclass|DO)
        hit  -> TTL-декей -> ответ (+stats)
   3. miss -> группа: primary (ротация) -> при ошибке fallback
   4. Apply MIN-TTL/MAX-TTL ко всем секциям
   5. cache.Put, stats.Record -> ответ клиенту
```

Слои: `internal/config`, `internal/rules`, `internal/upstream`, `internal/cache`,
`internal/stats`, `internal/server`, `internal/web`.

## Тесты

```sh
go vet ./...
go test ./...
```
