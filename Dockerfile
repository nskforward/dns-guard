# syntax=docker/dockerfile:1

# --- Сборка ---
# Билд-стадия выполняется на нативной платформе раннера (amd64), а целевой
# бинарь собирается кросс-компиляцией через GOARCH — без QEMU-эмуляции.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Кросс-компиляция статического бинаря. По умолчанию — linux/arm64 (hAP ax³).
ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/dns-guard .

# --- Рантайм ---
# NOTE: слушаем привилегированный порт 53, поэтому образ работает от root.
# Если нужен non-root (USER nonroot:nonroot), контейнеру требуется
# capability CAP_NET_BIND_SERVICE, иначе bind на :53 не удастся.
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/dns-guard /usr/local/bin/dns-guard

EXPOSE 53/udp
EXPOSE 53/tcp
EXPOSE 80

ENTRYPOINT ["/usr/local/bin/dns-guard"]
CMD ["-config", "/etc/dns-guard/config.yaml"]
