# Контекст сборки — корень репозитория: плагин отдельный модуль, но его go.mod
# ссылается на соседний кэш через replace.
FROM golang:1.27 AS build
WORKDIR /src
COPY . .
WORKDIR /src/plugin/bifrost
RUN CGO_ENABLED=0 go build -o /out/semcache-bifrost ./cmd/semcache-bifrost

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/semcache-bifrost /semcache-bifrost
ENTRYPOINT ["/semcache-bifrost"]
