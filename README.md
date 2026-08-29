# semcache

Two-stage semantic cache: retrieve by cosine, then **verify interchangeability**. Cosine similarity is not a safe cache key, and no gateway publishes its false-hit rate.

Measured on 616 labeled pairs, embeddings from `text-embedding-3-small`, stage-1 floor 0.70:

| Stage | Hit rate | False-hit rate | Verify cost |
|---|---|---|---|
| Cosine only, θ = 0.90 | 73% | **37%** (161/432) | — |
| Cross-encoder `bge-reranker-base`, τ = 0.99 | 87% | 6.0% (26/432) | $0, local |
| **LLM judge `gpt-4o-mini`** | **97%** | **4.9%** (21/432) | **5.3%** of savings |

The judge gives 24 points more recall and ~8× fewer silent wrong answers for 5.3% of the provider spend it avoids. **Excluding `language_switch`, false-hit is 0.0% (0/359)** — negation, entity swaps, numbers, dates and scope are fully separated; wrong-language answers are the one leak left, in both verifiers.

- M1 write-up: [`docs/posts/2026-08-27-cosine-is-not-interchangeability.md`](docs/posts/2026-08-27-cosine-is-not-interchangeability.md)
- M2 write-up: [`docs/posts/2026-08-29-verification-frontier.md`](docs/posts/2026-08-29-verification-frontier.md)
- Plots: [`bench/out/frontier.svg`](bench/out/frontier.svg), [`bench/out/verify-frontier.svg`](bench/out/verify-frontier.svg)
- Labels: [`bench/dataset/LABELING.md`](bench/dataset/LABELING.md)

```bash
make genv1          # rebuild v1.jsonl from the gold set + templates
make study          # cosine sweep (OpenAI text-embedding-3-small)
make study-local    # + bge-m3 + multilingual-e5-large
make verify-study   # two-stage: Noop vs CrossEncoder vs LLM judge
make verify         # gofmt + go vet + go test -race
```

Requires Go 1.26, `uv` for the Python sidecar, and `OPENAI_API_KEY` for hosted embeddings and the judge. Embeddings, rerank scores and judge decisions cache under `bench/.cache/`; a cold judge run over v1 costs about $0.02. Cost is always reported for a cold run, so re-running does not print free verification.
