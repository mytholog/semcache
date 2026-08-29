# Cosine similarity is not answer interchangeability

Date: 2026-08-27
Dataset: `bench/dataset/v1.jsonl` — 616 pairs, 80 hand-written. See `bench/dataset/LABELING.md`.
Plot: `bench/out/frontier.svg` (filled circle = θ 0.95).

Every LLM gateway now ships a semantic cache with a cosine threshold. None of them publish the only number that decides whether that feature is safe:

**How often does a cache "hit" return an answer that is wrong for the incoming request?**

## Setup

Pairs are labeled for a production question, not a search question: *can the cached answer for prompt B be served for prompt A?* Categories force the label. Near-miss families (negation, entity swap, numeric, temporal, scope, language switch) are `interchangeable: false`. Paraphrase and format-only are `true`.

Three embedding models, same pairs, same sweep 0.50–0.99:

| Alias | Actual model | Where |
|---|---|---|
| `text-embedding-3-small` | OpenAI | hosted |
| `bge-m3` | `BAAI/bge-m3` | local, sentence-transformers |
| `e5-large` | `intfloat/multilingual-e5-large` | local, fastembed ONNX |

English `e5-large-v2` is not in the ONNX catalog; the multilingual checkpoint is the honest local stand-in on a dataset that includes `language_switch`.

```bash
make genv1
make study-local    # all three; embeddings cache under bench/.cache/
```

## Result

At a **70% recall floor** on interchangeable pairs:

| Model | θ | Hit rate | False-hit rate |
|---|---|---|---|
| text-embedding-3-small | 0.90 | 73% | **37%** (161/432) |
| bge-m3 | 0.95 | 73% | **16%** (69/432) |
| e5-large | 0.97 | 72% | **31%** (136/432) |

At the typical gateway default **θ = 0.95**:

| Model | Hit rate | False-hit rate |
|---|---|---|
| text-embedding-3-small | 28% | **16%** |
| bge-m3 | 73% | **16%** |
| e5-large | 89% | **51%** |

A better embedding model moves you along the same ugly curve. It does not invent a safe threshold. `bge-m3` is the least bad of the three and still serves a wrong answer on **one in six** near-misses at the point where it finally catches 70% of paraphrases.

## Failure is concentrated

Medians on `text-embedding-3-small`:

| Category | Should hit? | n | median | ≥ 0.90 | ≥ 0.95 |
|---|---|---|---|---|---|
| negation | no | 100 | **0.950** | 99/100 | 49/100 |
| temporal | no | 68 | 0.905 | 37/68 | 14/68 |
| numeric | no | 57 | 0.878 | 15/57 | 5/57 |
| format_only | yes | 98 | 0.941 | 86/98 | 32/98 |
| paraphrase | yes | 86 | 0.914 | 49/86 | 20/86 |
| scope | no | 50 | 0.824 | 5/50 | 0/50 |
| entity_swap | no | 84 | 0.792 | 5/84 | 0/84 |
| language_switch | no | 73 | 0.653 | 0/73 | 0/73 |

**Negation is unseparated.** Half of polarity flips still clear 0.95 on OpenAI; `e5-large` puts **100/100** of them above 0.95. Versions and numbers leak too. Entity swaps and language switches — the Slack examples — are the ones a monolingual English model actually handles. Switch to multilingual e5 and language_switch **collapses** (median 0.91): the model is doing its job, which is exactly the wrong job for a cache.

Format-only (please / casing / emoji) is what cosine is good at. That traffic could have been hashed.

## Gold vs templates

The 80 hand-written pairs told a sharper story on paraphrases: median cosine **0.72** on OpenAI, and *none* of them cleared 0.90. v1 paraphrase templates are "How do I X?" / "What is the process for X?" — closer than real rephrasings, which *raises* hit rate. Even with that tailwind, no model in this set has a threshold that is both useful and safe.

If you only remember one comparison: **negation median 0.95, gold paraphrase median 0.72.** The classes can invert.

## What this is not

- Not a production trace. Synthetic SaaS/infra support. Template pairs are not individually copy-edited; the gold 80 are.
- Not an argument against caching. Exact-match plus singleflight still collapse bursts. Semantic cache without a verifier is the problem.

## Next

A two-stage cache: retrieve at a low threshold, then verify interchangeability (cross-encoder or LLM judge) on the candidates. The cost of the judge has to be a small fraction of the provider spend it saves. That is M2.


Date: 2026-08-27
Status: v1 study. OpenAI (`text-embedding-3-small`) numbers below. Local: `bge-m3` via sentence-transformers; `e5-large` mapped to `intfloat/multilingual-e5-large` (fastembed ONNX; English `e5-large-v2` is not in the ONNX catalog). Run `make study-local` for the other two curves.
Dataset: `bench/dataset/v1.jsonl` — 616 pairs, of which 80 are hand-written. See `bench/dataset/LABELING.md`.

Every LLM gateway now ships a semantic cache with a cosine threshold. None of them publish the only number that decides whether that feature is safe:

**How often does a cache "hit" return an answer that is wrong for the incoming request?**

## Setup

Pairs are labeled for a production question, not a search question: *can the cached answer for prompt B be served for prompt A?* Categories force the label. Near-miss families (negation, entity swap, numeric, temporal, scope, language switch) are `interchangeable: false`. Paraphrase and format-only are `true`.

The harness embeds both sides, sweeps cosine from 0.50 to 0.99, and reports hit rate on interchangeable pairs against false-hit rate on near-misses. Reproduce:

```bash
make genv1
make study          # OpenAI
make study-local    # OpenAI + bge-m3 + e5-large
```

`make study` regenerates `bench/out/frontier.svg`.

## Result

On `text-embedding-3-small`, v1, 2026-08-27:

| Operating point | Hit rate | False-hit rate |
|---|---|---|
| Typical gateway default **0.95** | 28% | **16%** (68 / 432) |
| Recall floor 70% (best in sweep) | 73% at θ=0.90 | **37%** (161 / 432) |
| Best F1 (θ=0.87) | 90% | **47%** |

A threshold cannot buy recall without eating near-misses. That is the product-shaped finding: verification, not a better default θ.

Per-category medians (same model):

| Category | Should hit? | n | median cosine | ≥ 0.90 | ≥ 0.95 |
|---|---|---|---|---|---|
| negation | no | 100 | **0.950** | 99/100 | 49/100 |
| temporal | no | 68 | 0.905 | 37/68 | 14/68 |
| numeric | no | 57 | 0.878 | 15/57 | 5/57 |
| format_only | yes | 98 | 0.941 | 86/98 | 32/98 |
| paraphrase | yes | 86 | 0.914 | 49/86 | 20/86 |
| scope | no | 50 | 0.824 | 5/50 | 0/50 |
| entity_swap | no | 84 | 0.792 | 5/84 | 0/84 |
| language_switch | no | 73 | 0.653 | 0/73 | 0/73 |

The failure is concentrated. **Negation is essentially unseparated** from a cache's point of view: half of polarity flips still clear 0.95. Versions and numbers leak too. Entity swaps and language switches, the examples everyone quotes in Slack, are the ones cosine actually handles.

Format-only (please / casing / emoji) is what cosine is good at. That is also the traffic you could have hashed.

## Gold vs templates

The 80 hand-written pairs told a sharper story on paraphrases: median cosine **0.72**, and *none* of them cleared 0.90. v1 paraphrase templates are "How do I X?" / "What is the process for X?" — closer than real rephrasings, which *raises* hit rate. Even with that tailwind, θ=0.95 still only catches 28% of interchangeable pairs and still serves a wrong answer on 16% of near-misses.

If you only remember one comparison: **negation median 0.95, gold paraphrase median 0.72.** The classes can invert.

## What this is not

- Not a claim about every embedding model. `bge-m3` and `e5-large` are in the catalog for that reason.
- Not a production trace. The domain is synthetic SaaS/infra support. Template pairs are not individually copy-edited; the gold 80 are.
- Not an argument against caching. Exact-match plus singleflight still collapse bursts. Semantic cache without a verifier is the problem.

## Next

A two-stage cache: retrieve at a low threshold, then verify interchangeability (cross-encoder or LLM judge) on the candidates. The cost of the judge has to be a small fraction of the provider spend it saves. That is M2.
