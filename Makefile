.PHONY: run bench pilot study study-local verify-study langprobe genv1 csv verify fmt vet test race tidy \
	pg-up pg-down pg-test invalidate-study vuln plugin-test plugin-demo

# Прокси с кэшем в памяти процесса: чтобы посмотреть, как он себя ведёт,
# ничего кроме ключа не нужно.
run:
	go run ./cmd/semcached

# Все числа из README за один прогон. Порядок тот же, что у этапов: порог
# косинуса, вторая стадия, инвалидация.
bench: study verify-study invalidate-study

pilot:
	go run ./bench -dataset bench/dataset/pilot.jsonl -models text-embedding-3-small

study:
	go run ./bench -dataset bench/dataset/v1.jsonl -models text-embedding-3-small

study-local:
	go run ./bench -dataset bench/dataset/v1.jsonl -models text-embedding-3-small,bge-m3,e5-large

genv1:
	go run ./tools/genv1

genv2:
	go run ./tools/genv2

csv:
	go run ./bench -dataset bench/dataset/v1.jsonl -models text-embedding-3-small

verify-study:
	go run ./bench -mode verify -dataset bench/dataset/v1.jsonl -models text-embedding-3-small

# Печатает язык, отрыв и перекрёстные вероятности по строкам: этим подбираются
# пороги языкового гейта.
langprobe:
	go run ./bench/langprobe

PG_DSN ?= postgres://semcache:semcache@localhost:5434/semcache?sslmode=disable

# Postgres с pgvector для интеграционных тестов и демо инвалидации.
pg-up:
	docker start semcache-pg 2>/dev/null || docker run -d --name semcache-pg \
		-e POSTGRES_USER=semcache -e POSTGRES_PASSWORD=semcache -e POSTGRES_DB=semcache \
		-p 5434:5432 pgvector/pgvector:pg17
	@until docker exec semcache-pg pg_isready -U semcache -q; do sleep 1; done

pg-down:
	docker stop semcache-pg

pg-test: pg-up
	SEMCACHE_TEST_DSN='$(PG_DSN)' go test -race -count=1 ./store/...

# Демо инвалидации: eager DELETE по тегу против TTL-подхода.
invalidate-study: pg-up
	go run ./bench -mode invalidate -dataset bench/dataset/v1.jsonl \
		-models text-embedding-3-small -pg-dsn '$(PG_DSN)'

verify: fmt vet vuln race plugin-test

# Плагин к Bifrost — отдельный модуль: bifrost/core тянет sonic, fasthttp и
# zerolog, и в библиотеке этим зависимостям места нет.
plugin-test:
	cd plugin/bifrost && go vet ./... && go tool govulncheck ./... && go test -race ./...

# Четыре вопроса через гейтвей с плагином: miss, exact, verified, reject.
plugin-demo:
	cd plugin/bifrost && go run ./cmd/semcache-bifrost

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

vuln:
	go tool govulncheck ./...

tidy:
	go mod tidy
