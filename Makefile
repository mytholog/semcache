.PHONY: pilot study study-local verify-study langprobe genv1 csv verify fmt vet test race tidy

pilot:
	go run ./bench -dataset bench/dataset/pilot.jsonl -models text-embedding-3-small

study:
	go run ./bench -dataset bench/dataset/v1.jsonl -models text-embedding-3-small

study-local:
	go run ./bench -dataset bench/dataset/v1.jsonl -models text-embedding-3-small,bge-m3,e5-large

genv1:
	go run ./tools/genv1

csv:
	go run ./bench -dataset bench/dataset/v1.jsonl -models text-embedding-3-small

verify-study:
	go run ./bench -mode verify -dataset bench/dataset/v1.jsonl -models text-embedding-3-small

# Печатает язык, отрыв и перекрёстные вероятности по строкам: этим подбираются
# пороги языкового гейта.
langprobe:
	go run ./bench/langprobe

verify: fmt vet race

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test ./...

race:
	go test -race ./...

tidy:
	go mod tidy
