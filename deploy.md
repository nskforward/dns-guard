# Обновление dns-guard на MikroTik RouterOS

Практическая инструкция по выкатке новой версии контейнера на роутер.
Описание архитектуры и первичной установки — в `README.md` (раздел
«Деплой на MikroTik RouterOS»).

## Схема деплоя

```
git tag v1.2.3 && git push origin v1.2.3
   └─> GitHub Actions (.github/workflows/docker.yml)
         └─> Docker Hub: nskforward/dns-guard:latest + 1.2.3 + sha-<short>
               └─> роутер: /container/update dns-guard
```

Роутер форвардит DNS на контейнер (`/ip/dns servers=192.168.89.3`), а сам
контейнер ходит в апстримы по правилам из `config.yaml`.

## Реквизиты

Подключение к роутеру — из `.env` в корне проекта (файл в `.gitignore`):

```sh
MIKROTIK_HOST=192.168.88.1
MIKROTIK_USER_NAME=admin
MIKROTIK_USER_PASSWORD=...
```

Примеры ниже используют `$MIKROTIK_HOST` / `$MIKROTIK_USER_NAME`.

### Подключение через `ros.sh`

Для выполнения команд на роутере используйте `ros.sh` из корня проекта —
это expect-обёртка над `ssh`. Скрипт читает `MIKROTIK_*` из окружения и сам
подставляет пароль, поэтому `sshpass` не нужен и вводить пароль вручную не
приходится.

```sh
chmod +x ros.sh
set -a; . ./.env; set +a

./ros.sh '/container/print where name=dns-guard'
```

`ros.sh` принимает **одну** команду RouterOS как единственный аргумент и
передаёт её в `ssh`. Команды с пробелами и `[find ...]` обязательно
заключайте в кавычки, чтобы их не разобрала локальная оболочка:

```sh
./ros.sh '/container/print detail where name=dns-guard'
./ros.sh '/container/stop [find where name="dns-guard"]'
./ros.sh '/container/start [find where name="dns-guard"]'
```

Таймаут выполнения — 5 минут. Для нескольких команд вызывайте скрипт
несколько раз. Если `.env` не загружен, реквизиты будут пустыми и подключение
не пройдёт — выполните `set -a; . ./.env; set +a` в текущей сессии.

## Параметры текущего контейнера

| Параметр | Значение |
|---|---|
| Имя | `dns-guard` |
| `remote-image` | `nskforward/dns-guard:latest` |
| `root-dir` | `/usb1/images/dns-guard` |
| `interface` | `veth-dnsguard` (IP `192.168.89.3`) |
| `mountlists` | `dns-guard-conf` (конфиг в `/etc/dns-guard/config.yaml`) |
| `start-on-boot` | `yes` |
| Веб-UI | `http://192.168.89.3:8080` |
| Реестр | `https://registry-1.docker.io` |

## Проблема «курица и яйцо» с DNS — решена на роутере

DNS самого роутера указывает на контейнер (`servers=192.168.89.3`). Если
просто остановить контейнер, роутер не сможет отрезолвить
`registry-1.docker.io`, и `update` падает:

```
download/extract error: check registry failed: resolving error
```

Решение — постоянные статические FWD-записи для доменов Docker Hub через
отдельный forwarder на публичный DNS (Яндекс). Записи обрабатываются самим
роутером и не зависят от контейнера:

```rsc
/ip/dns/forwarders/add name=public-dns dns-servers=77.88.8.8
/ip/dns/static/add name=docker.io  type=FWD forward-to=public-dns match-subdomain=yes comment="container registry"
/ip/dns/static/add name=docker.com type=FWD forward-to=public-dns match-subdomain=yes comment="container registry"
```

Покрываются `registry-1.docker.io`, `auth.docker.io` (`*.docker.io`) и
`production.cloudflare.docker.com` (`*.docker.com`). Важно: `address-list`
в этих записях указывать не нужно, иначе трафик Docker Hub уйдёт в VPN.

Проверка (контейнер можно даже остановить):

```sh
dig @192.168.88.1 registry-1.docker.io A +short
```

С этими записями `update` работает при остановленном контейнере — переключать
DNS роутера вручную для обновления больше не нужно.

## Пошаговое обновление

### 0. Проверить, что образ собран

После публикации нового тега (`git push origin vX.Y.Z`) дождитесь успешного
workflow `docker` в GitHub Actions. Проверить теги в Docker Hub:

```sh
curl -s "https://hub.docker.com/v2/repositories/nskforward/dns-guard/tags?page_size=5"
```

Должны быть `latest`, `X.Y.Z` и `sha-<short>` от нужного коммита. `latest`
обновляется только при публикации нового тега.

### 1. Зафиксировать текущее состояние

```sh
./ros.sh '/container/print detail where name=dns-guard'
```

Запомните `image-id` — по нему потом проверите, что образ реально сменился.
Для отката пригодится sha-тег развёрнутой версии: его можно найти по дате
деплоя в списке тегов Docker Hub (шаг 0) либо взять `sha-<short>` коммита
предыдущего релиза (`git tag -l`, затем `git rev-parse --short vX.Y.Z`).

### 2. (Опционально) переключить DNS роутера на внешний

Нужно только чтобы LAN резолвил обычные домены, пока контейнер остановлен.
Для самого `update` это не требуется — реестр резолвится через FWD-записи.

```
/ip/dns/set servers=77.88.8.8,8.8.8.8
/ip/dns/cache/flush
```

### 3. Остановить контейнер

```
/container/stop [find where name="dns-guard"]
```

Дождитесь статуса `S` (STOPPED) — `/container/print where name=dns-guard`.
Если шаг 2 выполнен, LAN продолжает резолвить через внешний DNS; если нет —
обычные домены в LAN будут недоступны до старта контейнера (docker-домены
резолвятся всегда благодаря FWD-записям).

### 4. Обновить образ

```
/container/update [find where name="dns-guard"]
```

Команда **асинхронная**: сразу возвращает управление, загрузка и распаковка
идут в фоне. Статусы в `/container/print`:

| Флаг | Значение |
|---|---|
| `E` | DOWNLOADING/EXTRACTING — идёт загрузка |
| `S` | STOPPED — готово, можно запускать |
| `F` | FAILED — ошибка (см. `/log/print where topics~"container"`) |

Обычно занимает 1–2 минуты (на USB).

### 5. Запустить контейнер

```
/container/start [find where name="dns-guard"]
```

Дождитесь статуса `R` (RUNNING).

### 6. Вернуть DNS роутера на контейнер (если выполняли шаг 2)

Если шаг 2 пропущен — пропустите и этот шаг.

```
/ip/dns/set servers=192.168.89.3
/ip/dns/cache/flush
/ip/dns/print
```

Убедитесь, что в выводе `servers: 192.168.89.3`.

### 7. Проверить

```sh
# веб-UI и API контейнера
curl -s http://192.168.89.3:8080/api/health        # {"status":"ok"}
curl -s http://192.168.89.3:8080/api/upstreams     # статистика по группам

# DNS через роутер (UDP и TCP)
dig @192.168.88.1 example.com A +short
dig @192.168.88.1 ya.ru A +tcp +short
```

Логи контейнера:

```
/log/print where topics~"container"
```

Ожидаемо: `download/extract done`, `dns udp/tcp listening`, `dns-guard started`,
`web listening` и отсутствие `error`.

### 8. Убедиться, что образ обновился

```sh
./ros.sh '/container/print detail where name=dns-guard' | grep -A1 image-id
```

`image-id` должен отличаться от зафиксированного на шаге 1.

## Troubleshooting

| Симптом | Причина / решение |
|---|---|
| `update` упал: `check registry failed: resolving error` | Нет рабочих FWD-записей для docker-доменов. Проверьте `/ip/dns/static/print where comment="container registry"` и `/ip/dns/forwarders/print`; либо временно переключите DNS (шаг 2) и повторите `update`. |
| После `update` сразу `S`, а `image-id` не изменился | Нормально, если тег `latest` не менялся: в логе будет `skip importing same version`. Если образ должен был обновиться — повторите `update` и проверьте лог. |
| Статус `F` (FAILED) | Смотрите лог: чаще всего сеть/реестр. Проверьте FWD-записи и forwarder; при необходимости переключите DNS (шаг 2) и повторите `update`. |
| Контейнер `R`, но новых ручек API нет (404) | Образ не обновился — сравните `image-id`, повторите шаги 3–5. |
| LAN не резолвит после старта | Проверьте `/ip/dns/print` (`servers: 192.168.89.3`), затем `dig @192.168.89.3 example.com`; если контейнер отвечает, а роутер нет — сбросьте кэш `/ip/dns/cache/flush`. |
| Забыли вернуть DNS роутера (шаг 2) | `/ip/dns/set servers=192.168.89.3` + `/ip/dns/cache/flush`, иначе LAN работает мимо dns-guard (без его кэша, статистики и правил зон). |

## Откат

Переключать DNS для отката не нужно: docker-домены резолвятся через FWD.

### Вариант A: откатить образ на предыдущий sha-тег

Если контейнер уже остановлен, первую строку пропустите.

```
/container/stop [find where name="dns-guard"]
/container/set [find where name="dns-guard"] \
  remote-image=nskforward/dns-guard:sha-<short>
/container/update [find where name="dns-guard"]
/container/start [find where name="dns-guard"]
```

Здесь `update` обязателен: он скачает и распакует образ по новому
`remote-image`. Дождитесь статуса `S` после `update`, затем запускайте, после
чего выполните проверки из шага 7. Если на шаге 2 вы переключали DNS, не
забудьте вернуть его (шаг 6).

### Вариант B: аварийный обход контейнера

Если контейнер не поднимается и нужен рабочий DNS прямо сейчас:

```
/ip/dns/set servers=77.88.8.8,8.8.8.8
/ip/dns/cache/flush
```

Роутер начнёт резолвить напрямую, минуя dns-guard (его правила зон, кэш и
статистика не используются). Статические FWD-записи и `to_vpn_list` при этом
продолжают работать.

## Справочник команд

```
/container/print where name=dns-guard            # статус
/container/print detail where name=dns-guard     # image-id, параметры
/container/config/print                          # registry-url, tmpdir
/ip/dns/static/print where comment="container registry"  # FWD для Docker Hub
/ip/dns/forwarders/print                         # forwarder public-dns
/log/print where topics~"container"              # логи загрузки и запуска
/container/stop   [find where name="dns-guard"]
/container/update [find where name="dns-guard"]
/container/start  [find where name="dns-guard"]
```

Любую из команд можно выполнить через `ros.sh`, передав её одной строкой:

```sh
./ros.sh '/container/print where name=dns-guard'
./ros.sh '/log/print where topics~"container"'
```

Проверка API: `/api/health`, `/api/stats`, `/api/upstreams`, `/api/domains`,
`/api/domain?name=...`, `/api/config`.

## Что важно помнить

- `update` нужно выполнять **на остановленном** контейнере; команда
  асинхронная — дождитесь статуса `S`, не запускайте повторно.
- Реестр Docker Hub резолвится через FWD-записи `container registry` —
  переключать DNS роутера для `update` не нужно.
- Недоступность самого dns-guard ~1,5–3 минуты: остановка → скачивание
  (~15 с) → распаковка (~1 мин) → старт. Если шаг 2 не выполнялся, обычные
  домены в LAN в это время не резолвятся; docker-домены работают всегда.
- Кэш и статистика dns-guard хранятся в памяти и после обновления начинаются
  с нуля — это ожидаемо.
- Отдельной health-check у контейнера нет; готовность проверяется через
  `/api/health`.
- Конфиг (`config.yaml`) лежит в mount `dns-guard-conf` и при обновлении образа
  не затрагивается.
- Пароль от роутера не коммитить: `.env` уже в `.gitignore`.
