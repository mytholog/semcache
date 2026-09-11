# Bifrost plugin — design

Date: 2026-09-11
Status: approved, implementing
Supersedes: D5 of [`2026-08-25-semcache-design.md`](2026-08-25-semcache-design.md)

## Why the original D5 changed

The design spec named the Ferro Labs AI Gateway as the primary plugin target and
Bifrost as the secondary. The primary target cannot be verified as a real,
documented plugin framework; Bifrost can, and it is written in Go. It also ships
its own `semantic_cache` plugin with a cosine `threshold` defaulting to **0.8**
and TTL as the only invalidation primitive — the exact incumbent this project was
built to measure. So Bifrost becomes the target, and the comparison is no longer
against a published number from a vendor but against a default we can run.

## What ships

1. `plugin/bifrost` — a `schemas.LLMPlugin` implementation backed by
   `semcache.Cache`, registered the same way Bifrost's own cache is:
   `LLMPlugins: []schemas.LLMPlugin{plugin}`.
2. A table in the README comparing Bifrost's `semantic_cache` at its own default
   threshold against the two-stage cache, computed from the existing v1 sweep.
3. `plugin/bifrost/cmd/semcache-bifrost` — a demo that drives the Bifrost SDK
   with the plugin installed and prints the outcome of four requests, plus a
   compose file bringing up Postgres for it.

Out of scope here: k6 overhead numbers and a Grafana dashboard. Both are
packaging, neither changes a measurement, and they stay in the later milestones.

## Separate Go module

`plugin/bifrost` gets its own `go.mod` requiring `github.com/maximhq/bifrost/core`
and the parent module, with a `replace` pointing at the repo root for local work.

The reason is the same one the design spec gives for keeping dependencies
minimal: every third-party module in the core library is a review objection, and
`bifrost/core` pulls in sonic, fasthttp and zerolog. A library whose selling
point is that it has almost no dependencies cannot acquire an HTTP stack because
one of its adapters needs one.

## Not a `.so`

Bifrost's published binaries are statically linked, and Go's plugin system needs
dynamic linking, so the officially distributed gateway cannot load a `.so` at
all. Their own semantic cache is not a dynamic plugin either — it is a Go package
compiled in. The plugin here follows that pattern, which means an operator
registers it either through the Go SDK or in a gateway binary they build
themselves. The `path`-based dynamic loading in Bifrost's config stays available
for anyone who builds a dynamic gateway, and requires no change to this code.

## Behaviour

Mirrors `semcached`, because two adapters over the same cache that disagree about
what is cacheable are two different caches.

- **Key** is the whole conversation, role-prefixed, exactly as in the proxy. The
  answer depends on the system prompt and prior turns, not only the last message.
- **Namespace** is `prefix + provider + "/" + model`. The proxy keys per model;
  inside a gateway the provider is part of the identity too, since `openai` and
  `azure` serving the same model name are different deployments. Serving one's
  answer for the other is a substitution, not a hit.
- **Bypass** on `n > 1`, tools (on `Params.Tools`, which is where Bifrost puts
  them), non-text content blocks, and an empty conversation. Each reason is
  counted. Temperature is deliberately not on the list, for the reason given in
  the proxy write-up.
- **Streaming** requests (`ChatCompletionStreamRequest`) are not served from
  cache in this version and not written to it. `LLMPluginShortCircuit` does have
  a `Stream` channel, so replay is possible later; until it is measured against a
  real client, the honest behaviour is to stay out of the way.
- **Writes are off the request path.** A write costs an embedding call, and
  `PostLLMHook` runs before the response reaches the client. Writes go to a
  goroutine bounded by a semaphore and are dropped when the bound is reached: a
  dropped write is a future miss, a blocked hook is added latency on every miss.
- **No coalescing.** The proxy collapses concurrent identical misses into one
  provider call; a plugin cannot, because the provider call belongs to the
  gateway, not to the hook. Combined with asynchronous writes this means a
  repeat arriving before the first answer has landed is a second miss, and a
  cold-key stampede is paid for once per request. Documented rather than worked
  around: the alternatives are blocking the hook or duplicating the gateway's
  provider call, and both are worse than a miss.
- **Fail open** by default. A lookup error is logged and counted, and the request
  continues to the provider. Bifrost logs plugin errors as warnings and never
  surfaces them to the caller, so a returned error would be invisible as well as
  useless.

## Payload

The proxy stores the provider's response body byte for byte. A plugin cannot: it
runs inside Bifrost core and sees `*schemas.BifrostChatResponse`, a parsed
struct, not the bytes. So the payload is that struct marshalled to JSON, and a
hit unmarshals it back.

The consequence is worth stating rather than hiding: a hit returns whatever the
struct round-trips, so any provider field Bifrost itself drops is dropped from a
cache hit too. This is a weaker guarantee than the proxy's, and it is a property
of where the hook sits, not of the cache.

## Prompt across hooks

`PostLLMHook` receives the response and the error, not the request, so the key
computed in `PreLLMHook` is stashed on `*schemas.BifrostContext` via `SetValue`
under a private key type and read back on the way out. Bifrost's own cache plugin
does the same thing; the context is documented as mutable for exactly this.

## Tests

The hooks are plain methods, so the tests call them directly with a
`schemas.NewBifrostContext` and a memory store — no gateway, no network, no
provider keys:

- exact hit short-circuits and never reaches the verifier
- a paraphrase short-circuits only when the verifier accepts it
- a rejected candidate returns no short-circuit
- each bypass reason produces no cache write
- a different provider or model with the same prompt is a miss
- the payload survives a marshal/unmarshal round trip, including `usage` and
  `system_fingerprint`
