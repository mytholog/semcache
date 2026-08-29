# semcache

Measurement study: **cosine similarity is not answer interchangeability.** Semantic caches in LLM gateways expose a threshold and do not publish false-hit rate.

- Dataset and labeling: [`bench/dataset/LABELING.md`](bench/dataset/LABELING.md)
- Write-up: [`docs/posts/2026-08-27-cosine-is-not-interchangeability.md`](docs/posts/2026-08-27-cosine-is-not-interchangeability.md)
- Plot: [`bench/out/frontier.svg`](bench/out/frontier.svg)

```bash
make genv1          # rebuild v1.jsonl from the gold set + templates
make study          # OpenAI text-embedding-3-small
make study-local    # + bge-m3 + multilingual-e5-large (downloads models)
make test
```

Requires Go 1.26, `OPENAI_API_KEY` for the hosted model, and `uv` for local embeddings.
