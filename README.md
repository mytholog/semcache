# semcache

Two-stage semantic cache: retrieve by cosine, then **verify interchangeability**. Cosine similarity is not a safe cache key, and no gateway publishes its false-hit rate.

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
| 75% | 0.914 recall, 5.00 candidates | 0.812, 4.95 | 0.815, 5.00 |
| 90% | 0.952, 5.00 | **0.561**, 3.33 | 0.722, 4.97 |
| 99% | 0.982, 5.00 | **0.073**, 0.39 | 0.761, 4.93 |

Recall@5 is against exact search over the same live entries. TTL does not serve stale answers here — the store filters those unconditionally — it stops answering: at 99% dead, a cache holding 200 healthy entries returns 0.39 candidates per lookup, because the ANN query walks a graph that is 99% corpses. pgvector 0.8's iterative scan recovers most of the recall at 2.7× the latency, on every lookup forever, for a problem one `DELETE` solves once.

What eager invalidation does **not** buy is disk: the HNSW index measures 172 MB at 20,000 rows and 172 MB at 200 rows after `VACUUM`. Details, plus three ways to silently lose the vector index and never notice, in the [M3 write-up](docs/posts/2026-08-29-eager-invalidation-buys-recall.md).

## Read more

- M1 write-up: [`docs/posts/2026-08-27-cosine-is-not-interchangeability.md`](docs/posts/2026-08-27-cosine-is-not-interchangeability.md)
- M2 write-up: [`docs/posts/2026-08-29-verification-frontier.md`](docs/posts/2026-08-29-verification-frontier.md)
- M3 write-up: [`docs/posts/2026-08-29-eager-invalidation-buys-recall.md`](docs/posts/2026-08-29-eager-invalidation-buys-recall.md)
- Plots: [`bench/out/frontier.svg`](bench/out/frontier.svg), [`bench/out/verify-frontier.svg`](bench/out/verify-frontier.svg)
- Labels: [`bench/dataset/LABELING.md`](bench/dataset/LABELING.md)

```bash
make genv1             # rebuild v1.jsonl from the gold set + templates
make study             # cosine sweep (OpenAI text-embedding-3-small)
make study-local       # + bge-m3 + multilingual-e5-large
make verify-study      # two-stage: Noop vs CrossEncoder vs LLM judge
make pg-up             # pgvector 0.8 on Postgres 17, port 5434
make pg-test           # store integration tests against it
make invalidate-study  # eager tagged DELETE vs TTL
make verify            # gofmt + go vet + go test -race
```

Requires Go 1.26, `uv` for the Python sidecar, Docker for the Postgres store, and `OPENAI_API_KEY` for hosted embeddings and the judge. Embeddings, rerank scores and judge decisions cache under `bench/.cache/`; a cold judge run over v1 costs about $0.02. Cost is always reported for a cold run, so re-running does not print free verification. Store tests skip themselves unless `SEMCACHE_TEST_DSN` is set.
