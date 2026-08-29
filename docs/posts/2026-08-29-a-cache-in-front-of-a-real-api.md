# A cache in front of a real API

M1 measured that cosine similarity is not a safe cache key. M2 built the verifier that fixes it. M3 made invalidation atomic in Postgres. This one puts all of it behind `/v1/chat/completions` and runs it against the actual OpenAI API, because a benchmark harness can be right about everything a proxy still gets wrong.

The first live run paid for itself immediately. With the answer to *"How do I enable two-factor authentication?"* cached, `gpt-4o-mini`, judge as the second stage:

| Request | Outcome | Cosine | Latency |
|---|---|---|---|
| the same question again | `exact` | 1.0000 | 0.16 s |
| "How can I turn on 2FA for my account?" | `verified` | 0.8437 | 0.92 s |
| "How do I **disable** two-factor authentication?" | `reject` | **0.8724** | 1.72 s |
| cold, nothing cached | `miss` | — | 1.74 s |

The question with the opposite meaning scored higher than the legitimate paraphrase. This is the M1 result again, but it is worth seeing it happen in traffic rather than in a labeled set: there is no threshold that serves row two and refuses row three, because the ranking itself puts the wrong answer first. Every gateway that ships a `similarity_threshold` knob is asking operators to tune a parameter that cannot express the thing they want.

Note what the latencies say about cost. An exact hit is 10× faster than the provider, and its floor is one embedding round trip — a local embedder would cut it further. A verified hit is only about 2× faster, because the judge is itself a model call. The saving there is tokens on the expensive model, not milliseconds; M2 measured it as 5.3% of the spend it avoids. A cache that promises latency and delivers token savings is still worth having, but it should say which one it is.

## The payload is the provider's response, verbatim

The cache stores the entire response body and a hit returns those bytes unchanged. The alternative — keeping the answer text and rebuilding a response around it — means the proxy owns a copy of someone else's schema, and every field it does not know about is silently dropped: `system_fingerprint`, `logprobs`, refusals, annotations, whatever ships next quarter.

Storing the whole body has one consequence worth being explicit about: a hit returns the original `id` and `created`, so two identical requests return the same completion id. That is honest — it *is* the same completion — but a client that assumes ids are unique per request will be surprised.

Streaming needs one exception. A hit for a `stream: true` request is replayed as a single SSE frame plus `[DONE]`, because a client that asked for a stream must get a stream; without that, the cache silently does nothing for most real clients. A *miss* with `stream: true` is not cached at all, since what comes back over the wire is a sequence of frames rather than a response.

## What must not be cached

The bypass rules are the part where a cache stops being a performance feature and becomes a correctness problem:

- `n > 1` asks for several different samples. A cache would return one.
- `tools` means the answer depends on tool definitions and on their results. A single text key does not describe that.
- Non-text content (images, audio) cannot be keyed by the text of the conversation.
- `X-Semcache: bypass` from the client.

Each one sets `X-Semcache-Bypass` with the reason and increments a counter, because a cache that quietly declines to work is indistinguishable from a cache that is broken.

Temperature is deliberately *not* on that list. Every reasonable default temperature is nonzero, so refusing to cache non-deterministic requests refuses to cache almost everything; a repeat answer is what a cache is for, and clients that need variety have `n` and the bypass header.

## One namespace per model

The key includes the whole conversation — system prompt and prior turns, not just the last question — since the answer depends on all of it. But the key is not enough on its own: an answer from `gpt-4o` is not a valid answer to a request for `gpt-4o-mini`, no matter how interchangeable the prompts are. Serving it is not a cache hit, it is a substitution, and the response would even carry the other model's name in its `model` field.

So entries carry a namespace, and lookups only ever see their own. The proxy uses the model name. This is a filter on the ANN scan, which means it inherits exactly the overfiltering behaviour measured in [M3](2026-08-29-eager-invalidation-buys-recall.md): with a handful of namespaces of comparable size it costs a few points of recall, and under a strong skew it needs pgvector's iterative scan. Cheaper than being wrong.

The model also goes in as a `model:<name>` tag, so retiring a model version can drop its answers in one call. Namespaces isolate; tags invalidate.

## Writes are not on the request path

A cache write costs an embedding call. Doing it inline would add that latency to the response the client is already waiting for, so writes go to a bounded queue with a fixed number of workers.

Bounded, and it drops when full. A dropped write is a future cache miss; a blocked handler is an outage. The same reasoning makes cache errors fail open by default: the lookup failed, the metric and the log record it, and the request still reaches the provider. A cache is an optimization, and an optimization that can take the service down is a liability.

## The bug that only a live run finds

The first live attempt cached nothing at all. Every request was a miss, invalidation removed zero rows, and the log said:

```
ERROR cache put failed error="upsert entry: ERROR: expected 8 dimensions, not 1536"
```

The database still had an `entries` table left over from an early test run, built for 8-dimensional vectors. `create table if not exists` looked at it, saw a table named `entries`, and did nothing. Migration reported success. The proxy started, answered requests correctly, and quietly failed to cache a single one — visible only in an async write log, because writes are off the request path.

Two fixes, both of which should have been there from the start. Migration now reads the actual column type and refuses to start on a mismatch:

```
table entries stores vector(8), but this process embeds 1536 dimensions;
point it at another schema or migrate the existing rows
```

And the schema got explicit `alter table ... add column if not exists` for every column added after the first version, since `create table if not exists` will never add one to a live table. A single-file schema is fine until the file changes; then it needs to say what to do about tables that already exist.

The dimension count is no longer taken from a flag either. The proxy asks the embedding model for a vector at startup and measures it. A flag for a number that the model already knows is just a way to get it wrong.

## What is measured and what is not

Everything above is either measured or a design decision with a stated reason. Two things are neither:

The `lang` column from M3 is now wired into the retrieval loop: a candidate is skipped when the incoming prompt is confidently *not* in the language of the stored answer. The asymmetry matters — the language of a full answer is reliable, the language of a two-word query is not — and this is the direction that works. But its effect is not measured, because v1 has prompts and labels, not answers. Measuring it needs a dataset with answers in it, which is a dataset change, not a code change.

Cost savings in production are also unmeasured here. The 5.3% overhead figure comes from the v1 run; what an actual traffic mix does to it depends on that traffic's repeat rate, which is exactly the number nobody publishes.

## Next

The library is importable now (`store`, `verify`, `embed`, and the `Cache` facade), the proxy runs, and `make bench` regenerates every number in the README. What is still missing is a gateway plugin — the original M4 — and a Grafana dashboard for the metrics the proxy already exposes. Both are packaging; neither would change a measurement.
