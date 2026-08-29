# Retrieve, then verify

Date: 2026-08-29
Dataset: `bench/dataset/v1.jsonl` — 616 pairs, same labels as M1.
Plot: [`bench/out/verify-frontier.svg`](../../bench/out/verify-frontier.svg)
Reproduce: `make verify-study`

M1 showed that a cosine threshold cannot buy recall without eating near-misses: 73% hit at **37%** false-hit. M2 asks whether a second stage — a verifier that sees both prompts — moves that frontier, and what it costs.

Stage 1 retrieves at a deliberately low cosine floor (0.70) on `text-embedding-3-small`. Stage 2 decides interchangeability:

| Verifier | What it is |
|---|---|
| `Noop` | today's product: accept whatever cleared retrieval |
| `CrossEncoder` | `BAAI/bge-reranker-base` via a Python sidecar, sigmoid scores, disk cache |
| `LLMJudge` | `gpt-4o-mini`, strict rubric, JSON output, exact-pair disk cache |

`Noop` at retrieve-min 0.70 is 98% hit / **85%** false-hit. That is what a low retrieval floor is supposed to look like, and why stage 2 exists.

## Result

| Stage | Hit rate | False-hit rate | Verify cost |
|---|---|---|---|
| Cosine only, θ = 0.90 (M1) | 73% | **37%** (161/432) | — |
| Cross-encoder, τ = 0.99 | 87% | 6.0% (26/432) | $0 (local) |
| Cross-encoder, τ = 0.999 | 72% | 2.8% (12/432) | $0 (local) |
| **LLM judge** | **97%** | 4.9% (21/432) | **5.3%** of savings |
| LLM judge + language gate | 97% | **1.4%** (6/432) | same |
| Cross-encoder τ = 0.999 + gate | 72% | **0.7%** (3/432) | $0 (local) |

The judge wins outright: it keeps almost every interchangeable pair (97% vs 73%) and cuts false hits from 37% to 4.9% — **7× fewer silent wrong answers at 24 points more recall**. It costs 105,625 tokens, **$0.021** at `gpt-4o-mini` pricing, against $0.40 of avoided completions. That is **5.3%** of the spend it saves, which is the single-digit share the design targeted.

The cross-encoder is free and gets the false-hit rate lower still (2.8%), but only by throwing away recall down to the 70% floor. At the recall the judge sustains, the reranker is not competitive.

The last two rows are the judge and the reranker with a deterministic language gate in front, which costs nothing and no model call. Together with the judge that is **1.4% false-hit at 97% hit rate** — 26× better than a cosine threshold, at the same recall the threshold could never reach.

## The residual errors are all one category

At their respective operating points, **every remaining false hit in both verifiers is `language_switch`**:

| Category | Should hit? | n | CE accepts (τ=0.999) | Judge accepts |
|---|---|---|---|---|
| negation | no | 100 | 0 | 0 |
| entity_swap | no | 84 | 0 | 0 |
| numeric | no | 57 | 0 | 0 |
| temporal | no | 68 | 0 | 0 |
| scope | no | 50 | 0 | 0 |
| **language_switch** | no | 73 | **12** | **21** |
| paraphrase | yes | 86 | 67 | 81 |
| format_only | yes | 98 | 65 | 98 |

**Excluding `language_switch`, false-hit is 0.0% (0/359) for both.** Negation — the category cosine could not touch at all, median 0.95 in M1 — is fully solved by either verifier.

The leak is a language bug, not a similarity bug. Only 24 of 73 language-switch pairs even clear retrieve-min 0.70, so this is not a large slice of traffic, but both verifiers wave through most of what reaches them. `bge-reranker-base` is a relevance model: a Russian question about the same topic *is* relevant, so it scores 0.999 (median). The judge's rubric names "same language" explicitly and it still accepts 21 of 24 — an instruction the model does not follow. The fix is a cheap deterministic check before stage 2, not a better prompt.

## The language gate and what it cannot do

An answer in the wrong language is never interchangeable, so this needs no model. `LanguageGate` sits in front of stage 2 and rejects a pair outright when it can show the two prompts are in different languages. It only ever rejects — a rejection costs a miss, which is money, while a wrong accept costs a wrong answer, so the gate abstains whenever it is unsure and lets stage 2 decide.

Two comparers run in order, cheapest first. `ScriptComparer` compares writing systems with `unicode` range tables: free, exact, and decisive for Russian or Japanese against English. `lingua.Comparer` wraps [lingua-go](https://github.com/pemistahl/lingua-go) restricted to the ten languages present in the dataset, for pairs inside one script.

| Gate | Caught (near-miss) | Lost (interchangeable) | No opinion |
|---|---|---|---|
| script only | 10 | 0 | 538 |
| lingua only | 14 | 0 | 69 |
| **script + lingua** | **17** | **0** | 66 |

17 of the 24 language-switch pairs that reach stage 2, at **zero cost in recall**, which is why the gate is on by default. Judge false-hit drops 4.9% → **1.4%** and precision rises 89.5% → 96.8%, with the hit rate untouched at 97.3%.

The 7 that get through are the interesting part. Every one is **English against a Latin-script European language**, and the reason is that short English text has no confident identity. `lingua` gives "How do I reset my password?" English with a margin of only 0.04 over the runner-up, while "Wie aktiviere ich 2FA?" gets German at 0.86 — so a rule requiring both sides to be identified abstains on exactly the pairs that matter most.

The obvious repair is to trust the one confident side: if the cached prompt is definitely German and the incoming one is definitely not German, reject. That rule catches all 24 — and also rejects `("api key", "How do I create an API key?")`, a legitimate hit. Keyword fragments carry no function words, so the model reads "api key" as Turkish and "reset password" as Italian, and the confident English side truthfully reports it is not Turkish. The two cases are not separable by any threshold on these features: on `min(cross-confidence)`, the keyword traps sit at 0.061 and 0.142 while genuine switches span 0.000–0.097. So the gate keeps the strict rule and the leak stays measured at 1.4% rather than hidden behind a heuristic that silently taxes keyword traffic.

The real fix is not a better guess at the language of a five-word query. It is to stop guessing: store the language alongside the cached entry, detected once at write time from the full answer, where there is enough text to be sure — or take it from the caller's locale when the gateway knows it. That is a store concern, so it lands with the schema in M3.

## Score spread explains the shape

Cross-encoder sigmoid scores on retrieved pairs:

| Category | Label | n | p10 | median | p90 |
|---|---|---|---|---|---|
| negation | no | 100 | 0.494 | 0.873 | 0.970 |
| entity_swap | no | 71 | 0.014 | 0.141 | 0.751 |
| numeric | no | 57 | 0.006 | 0.039 | 0.428 |
| temporal | no | 68 | 0.006 | 0.209 | 0.805 |
| scope | no | 48 | 0.164 | 0.692 | 0.983 |
| language_switch | no | 24 | 0.972 | **0.999** | 1.000 |
| paraphrase | yes | 82 | 0.998 | **1.000** | 1.000 |
| format_only | yes | 98 | 0.978 | 1.000 | 1.000 |

The positive class sits at 0.9997+, so **the entire useful threshold range is above 0.99**. A sweep on a 0.05 grid cannot see it: the first version of this study reported "6.0% at τ = 0.99" purely because the grid stopped there. The harness now adds a 0.995 / 0.998 / 0.999 / 0.9999 grid at the top, and `sweepVerifier` quantizes to 1e-4 instead of 1e-2.

## Caveats

- **The judge is not deterministic** at `temperature: 0`. Two cold runs gave 19 and 21 false hits (0.5pp apart), both entirely inside `language_switch`. The disk cache freezes a run once made; the cost model reports cold-run tokens so a re-run does not print free verification.
- Not a latency bench. The sidecar batches all 616 pairs into one Python process so the reranker loads once; per-request use needs a resident sidecar, not `exec`.
- Not ONNX in Go. The spec's fallback shipped; ONNX can replace the sidecar behind the same `PairScorer`.
- 1.4% false-hit is 26× better than a threshold and still too high for a billing or auth answer. The category table says where the remaining risk is, which is the point of publishing it.
- **The gate's recall cost is unmeasurable on this dataset.** Every interchangeable pair in v1 is English, so "0 lost" is a weak claim — it says the gate does not reject monolingual paraphrases, not that it is safe on multilingual positive traffic. The keyword-fragment cases in `lingua_test.go` are hand-written for exactly this reason; v2 should carry same-language positives in each language.
- `lingua-go` embeds all 75 language models unconditionally, adding ~130 MB to any binary that links it, regardless of the subset selected. That is why it lives in `internal/verify/lingua` behind the `LangComparer` interface: the cache core links `ScriptComparer` and stays dependency-free.

## Next

M3: eager tagged invalidation in Postgres, atomic with vector removal — and a language column on the entry, which is what actually closes the residual `language_switch` leak.
