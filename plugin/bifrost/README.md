# semcache as a Bifrost plugin

An `schemas.LLMPlugin` that puts the two-stage cache in front of every LLM call
a [Bifrost](https://github.com/maximhq/bifrost) gateway makes.

```go
plugin, err := semcachebifrost.New(semcachebifrost.Config{
    Cache: &semcache.Cache{
        Store:    store.OpenPostgres(...),
        Embedder: embedder,
        Verifier: verify.NewJudge(completer, "cache/dir"),
        Lang:     lingua.New(nil),
    },
})

client, err := core.Init(ctx, schemas.BifrostConfig{
    Account:    account,
    LLMPlugins: []schemas.LLMPlugin{plugin},
})
```

That is the same registration path Bifrost's own `semantic_cache` uses, and the
same one it has to use: Bifrost's published binaries are statically linked, so
the officially distributed gateway cannot load a Go `.so` at all. Registering
in-process means either the Go SDK, as above, or a gateway binary you build
yourself.

## Against `semantic_cache` at its own default

Bifrost ships a semantic cache whose `threshold` defaults to `0.8`
(`DefaultCacheThreshold` in `plugins/semanticcache`), and whose only way out is
TTL. A cosine threshold is exactly what the [M1
measurement](../../docs/posts/2026-08-27-cosine-is-not-interchangeability.md) is
about, so the default can simply be run against the labeled set — 616 pairs,
432 of them non-interchangeable, `text-embedding-3-small` embeddings, which is
the model their config takes too:

| Decision rule | Hit rate | False-hit rate |
|---|---|---|
| cosine ≥ 0.80 — `semantic_cache` default | 96% | **66%** (287/432) |
| cosine ≥ 0.85 | 92% | 53% (229/432) |
| cosine ≥ 0.90 | 73% | 37% (161/432) |
| cosine ≥ 0.70, then LLM judge + language gate | **97%** | **1.4%** (6/432) |

Two thirds of the answers served at the shipped default are answers to a
different question. The number is not an argument about their implementation —
retrieval, store and TTL handling are all fine — it is what any single cosine
threshold does on this data, and the reason the second stage exists. Raising the
threshold does not fix it either: it trades hit rate away faster than false
hits, because the ranking itself is wrong. A question with the opposite meaning
scores **higher** than a legitimate paraphrase.

Numbers come from [`bench/out/text-embedding-3-small-sweep.csv`](../../bench/out/text-embedding-3-small-sweep.csv)
(`make study`) and the verifier sweep (`make verify-study`).

## What it caches, and what it refuses to

The rules match `semcached`, because two adapters over one cache that disagree
about what is cacheable are two different caches.

- The key is the whole conversation, role-prefixed. An answer depends on the
  system prompt and the prior turns, not only on the last question. Bifrost's own
  cache instead stops caching past `conversation_history_threshold` messages
  (3 by default) and can exclude the system prompt.
- Entries are keyed per `provider/model`. `openai` and `azure` serving the same
  model name are different deployments, and serving one's answer for the other
  is a substitution, not a hit.
- Bypassed, with a counted reason: `n > 1`, tool definitions, non-text content
  blocks, an empty conversation, and streaming requests.
- Writes happen off the request path, bounded by `MaxPendingWrites` and dropped
  when that bound is reached. A dropped write is a future miss; a blocked
  `PostLLMHook` is added latency on every miss.
- Cache errors fail open by default: the request goes to the provider, and the
  error is logged and counted. `FailClosed: true` turns a cache error into a
  request error instead.

Because writes are asynchronous, a repeat that arrives before the first answer
has landed is a second miss — the demo waits for writes to settle for exactly
this reason. `semcached` narrows that window by coalescing concurrent identical
misses into one provider call; a plugin cannot, because the provider call belongs
to the gateway and not to the hook. On a cold key, a stampede of identical
requests is paid for once per request.

`Counters()` returns outcomes by kind (`exact`, `verified`, `reject`, `miss`),
bypasses by reason, and errors by stage. There is no separate metrics endpoint —
the gateway already exports its own, and the cache is a guest here.

## One weaker guarantee than the proxy

`semcached` stores the provider's response body byte for byte. A plugin cannot:
it runs inside Bifrost core and sees `*schemas.BifrostChatResponse`, a parsed
struct, not bytes. The payload is therefore that struct marshalled to JSON, and
a hit unmarshals it back — so a hit returns whatever the struct round-trips, and
any provider field Bifrost itself drops is dropped from a cache hit too. That is
a property of where the hook sits, not of the cache.

## Demo

```bash
OPENAI_API_KEY=... docker compose -f plugin/bifrost/compose.yaml up --build
```

Brings up Postgres with pgvector and asks four questions through a gateway with
the plugin installed: the same question twice, a paraphrase, and the question
with the opposite meaning. A run against `gpt-4o-mini`:

| Request | Outcome | Latency |
|---|---|---|
| cold, nothing cached | `miss` | 2.85 s |
| the same question again | `exact` | **0.24 s** |
| "What's the procedure for resetting my password?" | `verified` | 0.96 s |
| "How do I **stop** my password from being reset?" | `reject` | 3.83 s, answered by the provider |

The last row is the point, and it is the row a cosine threshold gets wrong.

Without Docker, and against an in-process store:

```bash
OPENAI_API_KEY=... go run ./cmd/semcache-bifrost
```

## Tests

The hooks are plain methods, so the tests call them directly — no gateway, no
network, no provider keys:

```bash
cd plugin/bifrost && go test -race ./...
```

This is a separate Go module on purpose. `bifrost/core` pulls in sonic, fasthttp
and zerolog, and a library whose selling point is that it has almost no
dependencies cannot acquire an HTTP stack because one of its adapters needs one.
