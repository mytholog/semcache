# semcache — architecture

Two-stage semantic cache. Stage 1 retrieves by cosine with a deliberately low floor (0.70);
stage 2 verifies that two prompts are interchangeable as cache keys.

Package layout
- `semcache.go` — the `Cache` facade: embeds the prompt, hashes it, delegates to `TwoStage`.
- `twostage.go` — retrieval plus verification plus the language gate over stored `Entry.Lang`.
- `store/` — `Store` interface, `memory.go`, `postgres.go` (pgvector, HNSW), `schema.sql`.
  One store owns entries, vectors and tags together, so tag invalidation deletes all three
  in a single transaction.
- `verify/` — `Verifier` interface, `judge.go` (LLM judge), `cross.go` (cross-encoder via a
  Python sidecar), `language.go` (deterministic gate), `lingua/` (detector adapter).
- `embed/openai.go` — hosted embeddings.
- `cmd/semcached/` — OpenAI-compatible proxy for `/v1/chat/completions`, hand-rolled
  Prometheus counters in `metrics.go`, background cache writers in `writer.go`.
- `bench/` — every number published in the README; `make bench` regenerates them.
