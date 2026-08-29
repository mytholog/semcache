# semcache — design spec

Date: 2026-08-25
Status: draft, not implemented
Repo: `github.com/mytholog/semcache` (planned)

## 1. Why this exists

Semantic caching is now a commodity feature of every LLM gateway: Bifrost, Ferro Labs
AI Gateway, LiteLLM, Kong AI Gateway, GPTCache. All of them expose a cosine
similarity threshold. None of them publish the only number that decides whether the
feature is safe to enable in production:

> **How often does a cache "hit" return an answer that is wrong for the incoming
> request?**

Cosine similarity between prompt embeddings measures topical closeness, not
interchangeability of answers. Prompt pairs that routinely land above 0.95:

| Category | Example A | Example B |
|---|---|---|
| Negation | "Can I deploy without a license key?" | "Can I deploy with a license key?" |
| Entity swap | "refund policy for EU customers" | "refund policy for US customers" |
| Numeric / unit | "rate limit on the 100k plan" | "rate limit on the 10k plan" |
| Temporal | "what changed in v3.1" | "what changed in v3.2" |
| Scope | "does this apply to trial accounts" | "does this apply to enterprise accounts" |

There are two distinct failure modes, and they need different fixes:

1. **False hit (precision failure).** A wrong answer is returned and, because it is
   cached, returned again and again. Operationally indistinguishable from a
   hallucination, except it is deterministic and invisible in provider logs — the
   request never reached the provider.
2. **Stale hit.** The cached answer was correct when produced, but the underlying
   source of truth changed (document updated, price changed, feature shipped). TTL is
   the only invalidation primitive the existing gateways offer, and TTL cannot express
   "this answer depends on document 47".

`semcache` is a focused attack on both, delivered as a measurement study plus a
deployable plugin for an existing gateway. It does **not** try to be another gateway.

## 2. Scope

In scope:

- A labeled benchmark dataset of prompt pairs and a harness that computes the
  precision/recall of the cache-hit decision across embedding models and thresholds.
- A two-stage cache (retrieve candidates → verify interchangeability) that moves the
  precision/cost frontier.
- Eager, atomic dependency-tagged invalidation.
- A plugin for an existing OSS gateway, with an overhead benchmark.

Out of scope (already solved well by the incumbent gateways, no reason to rebuild):

- Multi-provider routing, failover, load balancing.
- Per-tenant budgets and rate limiting.
- Provider capability translation, guardrails, PII detection.
- A dashboard.

Dependencies are kept minimal on purpose: the plugin is meant to be upstreamable to the
target gateway, and every third-party module in it is a review objection.

## 3. Deliverables

### D1. Benchmark dataset (`bench/dataset/`)

JSONL, ~600–1000 prompt pairs, each labeled `interchangeable: true|false` with a
`category` from the taxonomy above plus `paraphrase`, `format_only`
(politeness/formatting differences that *should* hit) and `language_switch`.

Construction: LLM-assisted generation from seed templates per category, then **manual
review of every pair**. Provenance and the review process are documented, and a
hand-written subset is marked `human_authored: true` so the study does not rest
entirely on synthetic data. Honest limitations section is mandatory — the credibility
of the whole project depends on it.

### D2. Measurement harness (`bench/`)

For each (embedding model × threshold) pair, compute precision, recall, F1 of the hit
decision, and derive the operational curve: **hit rate vs. false-hit rate vs. dollars
saved**. Embedding models to cover: `text-embedding-3-small` (OpenAI), `bge-m3`,
`e5-large` — one hosted, two local, so the comparison is not vendor-specific.

Headline artifact: a plot and a sentence of the form "at the threshold recommended by
default in <gateway>, N% of hits on near-miss traffic return the wrong answer".

### D3. Two-stage cache (`verify/`)

- Stage 1: vector retrieval of top-k candidates at a deliberately *low* threshold,
  tuned for recall.
- Stage 2: a verifier decides interchangeability. Implementations:
  `NoopVerifier` (baseline, equals today's state of the art), `CrossEncoder`
  (bge-reranker-base via ONNX), `LLMJudge` (structured output, strict rubric, its own
  exact-match cache so the judge is not called twice for the same pair).

Target result: false-hit rate down by roughly an order of magnitude at equal hit rate,
with verification cost a small single-digit percentage of the provider spend saved.
The cost model is part of the deliverable, not an afterthought.

### D4. Dependency-tagged invalidation (`store/`)

Cache entries are tagged with everything the answer depends on: retrieved document
IDs and versions (RAG citations), tool-call results, model version, prompt template
version. When a document changes, exactly the affected entries die.

**The invalidation must be eager and atomic with vector removal.** This is the central
design constraint of the whole component, and it is why one backend owns entries,
vectors and tags together instead of composing a vector index with a separate cache:

- A key-value cache can afford lazy invalidation, because staleness is discovered on
  read of a known key. A vector cache cannot: the lookup step is an ANN query for
  nearest neighbours, which happily returns invalidated entries. They consume slots in
  the top-k, so recall over live entries degrades. Over-fetching k trades latency for a
  bound that still is not a bound.
- Vectors are the expensive part — HNSW memory and build cost. Lazy invalidation makes
  the index carry every dead vector until TTL, and the realistic worst case is a doc
  re-publish invalidating thousands of entries at once.
- Two stores means two sources of truth, and the drift shows up as dangling candidates
  (vector present, payload gone) and orphan vectors (payload gone, vector present).

Postgres implementation: `entries(id, prompt_hash, embedding vector, payload, ...)` plus
`entry_tags(entry_id, tag)`, and invalidation is a single transactional
`DELETE FROM entries WHERE id IN (SELECT entry_id FROM entry_tags WHERE tag = ANY($1))`
that removes payload, vector and tag rows together. Redis path uses eager `DEL` of both
the entry and its vector-set member, driven by a reverse index.

Demo: mutate one document in a small corpus, show precisely which cached answers die
and that the index shrank accordingly, and contrast with TTL-only behaviour (serve
staleness for the rest of the TTL, or throw the whole cache away).

This is the capability none of the incumbents have — they all ship TTL only.

**Rejected alternative:** building this on `github.com/mytholog/tagcache`. Its Redis
store invalidates lazily by `INCR`-ing a tag version counter and comparing snapshots on
read, leaving entries in place until TTL — pre-TTL cleanup is explicitly out of scope
there. That is the right trade-off for a key-value cache and the wrong one here, for
every reason above. Also, a dependency on a personal module weakens the case for
upstreaming the plugin. The idea carries over; the code does not.

### D5. Gateway plugin (`plugin/`)

Implement against the Ferro Labs AI Gateway plugin framework (public and documented)
as the primary target, Bifrost as the secondary. Ship `docker-compose.yaml` bringing
up gateway + plugin + Prometheus/Grafana/Jaeger, driven by generated traffic.

Overhead benchmark with k6: added p50/p99 at 1k and 5k RPS, allocations per request,
RSS. Reported against the incumbents' published numbers, using their methodology so
the comparison is fair.

## 4. Architecture

Go 1.26. Small interfaces, everything swappable, no framework.

```
type Embedder interface { Embed(ctx, []string) ([][]float32, error) }
type Verifier interface { Interchangeable(ctx, incoming, cached string) (Decision, error) }

// Store owns entries, vectors and tags together — see D4 on why they cannot be split.
type Store interface {
    Lookup(ctx, promptHash string, vec []float32, k int) ([]Candidate, error)
    Put(ctx, Entry) error
    InvalidateTags(ctx, tags []string) (removed int, err error)
}
```

- `Store`: two implementations — `memory` (exact brute-force search, for reproducible
  benchmarks) and `postgres` (pgvector HNSW, production path). `Lookup` serves both
  cache layers in one round trip: exact `prompt_hash` match first, ANN candidates
  otherwise.
- Request coalescing via `golang.org/x/sync/singleflight` on the exact-match key, so a
  burst of identical prompts collapses into one provider call. Worth measuring in the
  benchmark as a saving distinct from cache hits.
- Streaming: a cached response must be replayable as SSE to be a true drop-in
  replacement. Store the assembled response and re-chunk on replay; document the
  behaviour difference (no real token timing) explicitly.
- Observability: OTel spans following `gen_ai.*` semantic conventions plus counters
  for hit / miss / verify-reject / invalidated, a cost-saved counter, and latency
  histograms split by stage.

## 5. Milestones

Part-time, roughly 2.5 weeks of evenings. Each milestone ends with something
publishable.

- **M0 — premise test (1–2 evenings).** The riskiest assumption of the project is that
  false hits are frequent at the thresholds people actually run. Test it before
  building anything: 60–80 hand-written pairs, one hosted embedding model, a brute-force
  cosine sweep from 0.80 to 0.99. No interfaces, no store, no index — a single `main.go`
  under `bench/`. Per-category breakdown, not just the aggregate.

  Kill criterion, recorded before the numbers arrive: the thesis holds if, in the
  threshold range that keeps a sensible hit rate on paraphrases (~70%+), the false-hit
  rate on near-miss categories is double-digit. If it is a few percent, the project
  pivots — dependency-tagged invalidation becomes the headline and verification becomes
  a footnote. If the threshold separates the classes cleanly, the project is dead and
  two evenings were the entire cost.
- **M1 — the study (4 d).** Dataset v1 (full 600–1000 pairs), P/R/F1 across 3 embedding
  models × threshold sweep, plots. Standalone blog post lives here.
- **M2 — cache and verification (4 d).** `Store` interface, `memory` implementation, CI
  with `go test`/`go vet`/race; cross-encoder and LLM-judge verifiers, frontier
  comparison, cost model.
- **M3 — invalidation (2 d).** Postgres store with tags, eager transactional
  invalidation, document-mutation demo, TTL contrast, index-size proof.
- **M4 — plugin (3 d).** Gateway plugin, docker-compose stack, k6 overhead bench,
  Grafana dashboard.
- **M5 — packaging (2 d).** README with the numbers up front, 3-minute screencast,
  blog post, upstream issue or PR to the target gateway.

## 6. Risks

- **ONNX cross-encoder in Go** is the riskiest technical dependency. Fallback: a small
  Python sidecar, which doubles as a polyglot signal for clients wary of a Go-only
  contractor.
- **Dataset credibility.** Mitigation: hand-authored subset, published labeling
  guidelines, per-category breakdown so a reader can discount categories they consider
  unrealistic.
- **Embedding API cost.** Expected $5–20 total; the sweep is over a cached embedding
  matrix computed once.
- **Scope creep into "yet another gateway".** The out-of-scope list in §2 is binding.

## 7. Success criteria

- README leads with measured numbers, not features.
- One plot that makes the precision/cost frontier improvement obvious at a glance.
- The plugin runs inside a real gateway, verified by the compose stack.
- Reproducible: `make bench` regenerates every number in the README.

## 8. Relation to the rest of the portfolio

- The RAG service (portfolio project 2) reuses the evaluation methodology from D2 and
  the citation-based tagging from D4 directly.
- The agent runner (project 3) reuses the cost model from D3 for its budget caps.
