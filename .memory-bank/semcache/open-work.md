# semcache — open work

Done: M0-M3 plus a runnable proxy. Remaining, in the order proposed on 2026-09-11.

1. Streaming misses are not cached. `forward` in `cmd/semcached/proxy.go` only counts
   `bypassed_total{reason="stream_miss"}`; hits can be replayed as SSE but the cache is never
   filled from a streamed response, so a client that always sends `stream: true` never
   populates it. Fix: assemble SSE chunks into a whole body before the cache write.
2. Latency histograms. `metrics.go` exports counters and `_seconds_total` only, so p95 cannot
   be computed in Grafana. Add manual buckets, still without `client_golang`.
3. Docker-compose stack (semcached, pgvector, Prometheus, Grafana) and a Dockerfile; there is
   no Dockerfile in the repo. Then a dashboard JSON. Do not plot a "false-hit rate" in
   production: there is no ground truth there; the honest proxy signal is the reject rate.
4. Overhead benchmark (the k6 item from M4). Fake upstream with a fixed delay plus a
   deterministic embedder, so it runs in CI without `OPENAI_API_KEY`. Report p50/p95/p99 per
   outcome and the load at which `writeQueue` starts dropping.
5. Dataset v2 with answers, to measure the effect of `Entry.Lang` end to end. v1 has prompts
   and labels only, so the stored-language gate has never been measured. Schema change in
   `internal/dataset`, generator in `tools/`, a `bench -mode lang` run.
6. Cross-encoder is unavailable in production: `newVerifier` in `cmd/semcached/main.go` knows
   only judge and noop, while the README sells the cross-encoder as 2.8% false-hit for $0.
7. Packaging: gateway plugin, upstream issue or PR, screencast, final post.

Smaller debt: no Redis store (so "one store owns entries, vectors and tags" is demonstrated,
not compared); CI has no golangci-lint and no image build; cost savings are modelled from v1
only, never from a real traffic mix.
