# semcache

Two-stage semantic cache: retrieve by cosine, then **verify interchangeability**. Cosine similarity is not a safe cache key, and no gateway publishes its false-hit rate.

Ships as a Go library and as `semcached`, a drop-in proxy for `/v1/chat/completions`. A live run through the proxy, against `gpt-4o-mini`, with the answer to *"How do I enable two-factor authentication?"* already cached:

| Request | Outcome | Cosine | Latency |
|---|---|---|---|
| the same question again | `exact` | 1.0000 | **0.16 s** |
| "How can I turn on 2FA for my account?" | `verified` | 0.8437 | 0.92 s |
| "How do I **disable** two-factor authentication?" | `reject` | **0.8724** | 1.72 s, answered by the provider |
| cold, nothing cached | `miss` | — | 1.74 s |

The third row is the whole point: the question with the opposite meaning scored **higher** than the legitimate paraphrase. Any threshold that serves the paraphrase from cache also serves the reverse instruction — the ranking itself is wrong, so no tuning fixes it. The second stage sees both prompts and refuses.

Measured on 616 labeled pairs, embeddings from `text-embedding-3-small`, stage-1 floor 0.70:

| Stage | Hit rate | False-hit rate | Verify cost |
|---|---|---|---|
| Cosine only, θ = 0.90 | 73% | **37%** (161/432) | — |
| Cross-encoder `bge-reranker-base`, τ = 0.999 | 72% | 2.8% (12/432) | $0, local |
| LLM judge `gpt-4o-mini` | **97%** | 4.9% (21/432) | 5.3% of savings |
| **+ language gate** | **97%** | **1.4%** (6/432) | same |

The judge gives 24 points more recall and 7× fewer silent wrong answers than cosine alone, for 5.3% of the provider spend it avoids. Adding a deterministic language gate in front of stage 2 cuts false hits by a further 3.5 points **without costing a single hit**, because an answer in the wrong language is never interchangeable and no model call is needed to see that.

Every false hit left in either verifier is `language_switch`: **excluding that category, false-hit is 0.0% (0/359)** — negation, entity swaps, numbers, dates and scope are fully separated. The gate closes 17 of the 24 such pairs that reach stage 2; the residual 7 are short English queries, where language identification cannot be trusted without also rejecting legitimate keyword traffic ([why](docs/posts/2026-08-29-verification-frontier.md#the-language-gate-and-what-it-cannot-do)).

## Invalidation

Entries are tagged with what the answer depends on — document ids and versions, model and template versions — and one transactional `DELETE` removes the payload, the vector and the tag rows together. Every incumbent ships TTL instead. On 20,000 entries in pgvector HNSW, as one document mutation kills a growing share of the cache:

| Dead share | Eager `DELETE` | TTL only | TTL + iterative scan |
|---|---|---|---|
| 75% | 0.955 recall, 5.00 candidates | 0.864, 4.94 | 0.866, 5.00 |
| 90% | 0.962, 5.00 | **0.601**, 3.36 | 0.767, 4.98 |
| 99% | 0.979, 4.94 | **0.076**, 0.39 | 0.728, 4.95 |

Recall@5 is against exact search over the same live entries. TTL does not serve stale answers here — the store filters those unconditionally — it stops answering: at 99% dead, a cache holding 200 healthy entries returns 0.39 candidates per lookup, because the ANN query walks a graph that is 99% corpses. pgvector 0.8's iterative scan recovers most of the recall at 2.5× the latency, on every lookup forever, for a problem one `DELETE` solves once.

What eager invalidation does **not** buy is disk: the HNSW index measures 168 MB at 20,000 rows and 168 MB at 200 rows after `VACUUM`. Details, plus three ways to silently lose the vector index and never notice, in the [M3 write-up](docs/posts/2026-08-29-eager-invalidation-buys-recall.md).

## Run it

```bash
export OPENAI_API_KEY=...
go run ./cmd/semcached                 # in-process store, nothing else needed
```

Point a client at it and use it as the provider. Tag every answer with what it depends on, and drop those answers when the dependency changes:

```bash
curl localhost:8080/v1/chat/completions \
  -H 'X-Semcache-Tags: kb:2fa, tpl:v3' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"How do I enable 2FA?"}]}'

curl localhost:8080/invalidate -d '{"tags":["kb:2fa"]}'   # {"removed":1}
```

Every response carries `X-Semcache: exact|verified|reject|miss` and, when a candidate was involved, `X-Semcache-Score`. `/metrics` exposes outcomes by kind for Prometheus. With `-dsn` the cache lives in Postgres and survives restarts; without it, in the process.

A cache hit returns the provider's response body unchanged, byte for byte, including fields the proxy knows nothing about. Requests that ask for variety (`n > 1`), carry tool definitions, use non-text content, or send `X-Semcache: bypass` skip the cache and say why in `X-Semcache-Bypass`. Answers are keyed per model — one model's answer is never served for another's request.

## Use it as a library

```go
cache := &semcache.Cache{
    Store:    store.NewMemory(),                       // or store.OpenPostgres(...)
    Embedder: embedder,                                // any Embed([]string) ([][]float32, error)
    Verifier: verify.NewJudge(completer, "cache/dir"), // or verify.Threshold{Scorer: crossEncoder}
    Lang:     lingua.New(nil),                         // rejects cross-language hits for free
}

res, err := cache.Get(ctx, semcache.Query{Prompt: prompt, Namespace: model})
if res.Hit() {
    return res.Entry.Payload, nil
}

answer, err := callProvider(ctx, prompt)
// ...
err = cache.Put(ctx, semcache.Write{
    Prompt: prompt, Payload: answer, Namespace: model,
    Tags: []string{"kb:2fa", "tpl:v3"},
})
```

## In a Bifrost gateway

[`plugin/bifrost`](plugin/bifrost) is an `schemas.LLMPlugin` that puts the same two-stage cache in front of every LLM call a [Bifrost](https://github.com/maximhq/bifrost) gateway makes:

```go
plugin, _ := semcachebifrost.New(semcachebifrost.Config{Cache: cache})
client, _ := core.Init(ctx, schemas.BifrostConfig{
    Account:    account,
    LLMPlugins: []schemas.LLMPlugin{plugin},
})
```

Bifrost ships its own semantic cache, with a cosine `threshold` defaulting to `0.8` and TTL as the only way out. That default is a decision rule, so it can be run against the same labeled set:

| Decision rule | Hit rate | False-hit rate |
|---|---|---|
| cosine ≥ 0.80 — Bifrost `semantic_cache` default | 96% | **66%** (287/432) |
| cosine ≥ 0.85 | 92% | 53% (229/432) |
| cosine ≥ 0.70, then LLM judge + language gate | **97%** | **1.4%** (6/432) |

Two thirds of the answers served at the shipped default are answers to a different question, and raising the threshold trades hit rate away faster than false hits. This says nothing about their implementation — it is what any single cosine threshold does on this data.

```bash
OPENAI_API_KEY=... docker compose -f plugin/bifrost/compose.yaml up --build
```

brings up a gateway with the plugin and asks four questions, which should produce `miss`, `exact`, `verified` and `reject`. The plugin is a separate Go module, so none of `bifrost/core`'s dependencies reach the library. Details, including the one guarantee it cannot keep that the proxy does: [`plugin/bifrost/README.md`](plugin/bifrost/README.md).

## Read more

- M1 write-up: [`docs/posts/2026-08-27-cosine-is-not-interchangeability.md`](docs/posts/2026-08-27-cosine-is-not-interchangeability.md)
- M2 write-up: [`docs/posts/2026-08-29-verification-frontier.md`](docs/posts/2026-08-29-verification-frontier.md)
- M3 write-up: [`docs/posts/2026-08-29-eager-invalidation-buys-recall.md`](docs/posts/2026-08-29-eager-invalidation-buys-recall.md)
- The proxy, and the bug only a live run found: [`docs/posts/2026-08-29-a-cache-in-front-of-a-real-api.md`](docs/posts/2026-08-29-a-cache-in-front-of-a-real-api.md)
- Plots: [`bench/out/frontier.svg`](bench/out/frontier.svg), [`bench/out/verify-frontier.svg`](bench/out/verify-frontier.svg), [`bench/out/invalidation-recall.svg`](bench/out/invalidation-recall.svg)
- Labels: [`bench/dataset/LABELING.md`](bench/dataset/LABELING.md)

```bash
make run               # semcached on :8080 with an in-process store
make bench             # every number in this file, from scratch
make verify            # gofmt + go vet + go test -race

make genv1             # rebuild v1.jsonl from the gold set + templates
make study             # cosine sweep (OpenAI text-embedding-3-small)
make study-local       # + bge-m3 + multilingual-e5-large
make verify-study      # two-stage: Noop vs CrossEncoder vs LLM judge
make pg-up             # pgvector 0.8 on Postgres 17, port 5434
make pg-test           # store integration tests against it
make invalidate-study  # eager tagged DELETE vs TTL
make plugin-test       # the Bifrost plugin (separate module)
make plugin-demo       # four questions through a Bifrost gateway
```

Requires Go 1.27, `uv` for the Python sidecar, Docker for the Postgres store, and `OPENAI_API_KEY` for hosted embeddings and the judge. Embeddings, rerank scores and judge decisions cache under `bench/.cache/`; a cold judge run over v1 costs about $0.02. Cost is always reported for a cold run, so re-running does not print free verification. Store tests skip themselves unless `SEMCACHE_TEST_DSN` is set.
