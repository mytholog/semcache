.PHONY: pilot study study-local genv1 csv verify fmt vet test tidy

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

verify: fmt vet test

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy
