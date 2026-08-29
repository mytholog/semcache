# Eager invalidation buys recall, not disk

Date: 2026-08-29
Store: pgvector 0.8.6 on Postgres 17, HNSW, `vector_cosine_ops`
Corpus: 434 real prompts from `bench/dataset/v1.jsonl` padded to 20,000 entries, 1536 dims, 100 documents
Reproduce: `make invalidate-study`

Every semantic cache on the market ships TTL and nothing else. The design doc for this one claims that a vector cache cannot afford that, because an ANN query does not know which entries are dead: they occupy slots in the top-k, so recall over live entries degrades. M3 implements the alternative — tagged entries, one transactional `DELETE` that takes the payload, the vector and the tag rows together — and measures what the claim is worth.

The answer is that the claim is true, larger than expected at the tail, and smaller than expected in the middle. And the second half of the design doc's promise — that the index shrinks — is simply false.

## Result

One `DELETE` by tag removed 19,800 entries and their 39,600 tag rows in **948 ms**. Recall@5 is measured against exact brute force over the same live set, so 1.000 means the index found everything a full scan would:

| Dead share | Live entries | `DELETE` | `DELETE` + `VACUUM` | TTL only | TTL + iterative scan |
|---|---|---|---|---|---|
| 25% | 15,000 | 0.839 | 0.898 | 0.811 | 0.811 |
| 50% | 10,000 | 0.899 | 0.913 | 0.813 | 0.813 |
| 75% | 5,000 | 0.914 | 0.954 | 0.812 | 0.815 |
| 90% | 2,000 | 0.952 | 0.996 | **0.561** | 0.722 |
| 95% | 1,000 | 0.995 | 0.998 | **0.317** | 0.668 |
| 99% | 200 | 0.982 | **1.000** | **0.073** | 0.761 |

The intact cache scores 0.815 — that is HNSW at the default `ef_search = 40` on a corpus deliberately full of near-duplicates, not a bug. Eager deletion tracks or beats that line everywhere and reaches 1.000 as the live set shrinks, because fewer entries means less for the graph to get wrong.

TTL holds up while dead entries are a minority and then falls off a cliff. The more legible number is how many candidates come back at all, out of the five requested:

| Dead share | `DELETE` | TTL only | TTL + iterative scan |
|---|---|---|---|
| 75% | 5.00 | 4.95 | 5.00 |
| 90% | 5.00 | 3.33 | 4.97 |
| 95% | 5.00 | 1.79 | 4.95 |
| 99% | 5.00 | **0.39** | 4.93 |

At 99% dead, a cache with 200 perfectly good live entries answers 0.39 candidates per lookup. It is not returning stale answers — the store filters those out unconditionally, so a hit is never wrong — it is returning *nothing*, having walked a graph of 20,000 nodes of which 19,800 are corpses. The cost of lazy invalidation is not staleness. It is that the cache quietly stops being a cache.

This is the shape the design doc named as the realistic worst case: a document re-publish invalidating most of the cache at once. It is also where the difference between a key-value cache and a vector cache lives. Lazy invalidation in a KV store costs one lookup, discovered on read of a known key. Here it costs the whole neighbourhood, because the query is "who is near me" and the dead are still near.

## The counter-argument, measured

pgvector 0.8 added iterative index scans for exactly this problem: when the filter rejects too much, keep pulling from the graph instead of returning short. It works — recall recovers from 0.073 to 0.761 at 99% dead, and candidate count from 0.39 to 4.93.

It does not close the gap. Median latency stays at ~1 ms for eager deletion at every dead share, while the iterative scan pays for the extra traversal: 1.3 ms at 90% dead, 1.5 ms at 95%, **2.9 ms at 99%** — 2.7× the eager path, for recall that is still 24 points worse. And it is a read-side mitigation for a write-side problem: every lookup forever pays to walk around garbage that one `DELETE` would have removed once.

## Three ways to lose the index without noticing

All three of these produced clean, plausible, wrong benchmark tables before being found. Each one returned correct answers, which is why none of them announced itself.

**A CTE that gets materialized.** The lookup serves both cache layers in one round trip — exact `prompt_hash` match and ANN candidates — so the liveness filter started life as a shared CTE referenced by both branches of a `UNION ALL`. Postgres materializes a CTE referenced more than once, and a materialized CTE has no index. The ANN branch silently became a full scan with an exact sort: same rows, 200× the latency, and a recall of exactly 1.000 that looks like a triumph. The condition is now repeated in both branches, with a comment saying why the tidier version is wrong.

**An index on the TTL column.** `create index on entries (expires_at)` is the obvious companion to a TTL filter, and it hands the planner an escape route: fetch every live row by bitmap scan, then sort exactly, bypassing HNSW entirely. 110 ms instead of 1 ms, again with perfect recall. The schema now carries a `drop index` for it and an explanation.

**A verification that checks a different query.** The bench asserted "the plan uses the HNSW index" against a hand-written simplification of the lookup, not the lookup itself. It reported `true` while the real query was doing a sequential scan. `ExplainLookup` now runs `EXPLAIN` on the exact same SQL constant that `Lookup` executes, and the study prints the access method for every row of every table, because `hnsw` versus `bitmap` is the difference between measuring ANN behaviour and measuring nothing.

There is a fourth, milder trap in the measurement itself: statistics. With autovacuum disabled for a controlled run but no explicit `ANALYZE`, stale row estimates flipped the planner into full scans; with fresh statistics on a shrinking table it flips *legitimately*, since a 200-row table genuinely does not need an index. Both make the comparison about table size rather than about dead vectors, so the study runs `ANALYZE` after every state change and forbids sequential scans while measuring.

## The index does not shrink

The design doc promised a demo showing "that the index shrank accordingly". It does not shrink, at all:

| State | Rows | HNSW index |
|---|---|---|
| Before invalidation | 20,000 | 172.2 MB |
| After deleting 99% and `VACUUM` | 200 | 172.2 MB |

`VACUUM` removes the deleted tuples from the graph and marks their pages reusable, which is what restores recall to 1.000. It does not return the pages to the filesystem — that needs `VACUUM FULL` or a `REINDEX`, both of which take an `ACCESS EXCLUSIVE` lock, and neither belongs on an invalidation path. So the honest claim is narrower than the design doc's and more useful: eager invalidation buys back **recall and latency**, not bytes. The index keeps its high-water mark, and the freed space is reused by the next writes.

`VACUUM` is also not free: 1 m 20 s across the six increments, against 948 ms for all the `DELETE`s. Recall is already near-perfect on the un-vacuumed path (0.952 at 90% dead vs 0.996 after), so vacuuming is a background chore, not part of the transaction — but it is the step that actually reclaims the graph, and leaving it to autovacuum's defaults after a bulk invalidation is a decision, not a default.

## What is in the store

`entries(id, prompt_hash, prompt, payload, lang, embedding, created_at, expires_at)` plus `entry_tags(entry_id, tag)` with `on delete cascade`, which is what makes the invalidation atomic for free: one `DELETE` on `entries` takes the payload, the vector and the tags in one transaction, and there is no window where a candidate exists without its answer.

The `lang` column is the debt M2 left. The language gate closes 17 of 24 `language_switch` false hits, and the 7 survivors are all short English queries where no detector can be confident. The fix is not a better guess at the language of a five-word query but to stop guessing: `lang` is written once, from the full answer text, where there is enough signal. Wiring it into the gate is M4, since it needs the gateway to say what it wants.

Two smaller things worth keeping:

- **Migrations run under an advisory lock.** `create extension if not exists` and `create index if not exists` are not concurrency-safe — two instances starting together fail on a duplicate key in `pg_type`. The whole DDL is wrapped in `pg_advisory_xact_lock`.
- **Integration tests get a schema each.** They share one database and run in parallel, so each test creates `semcache_test_<name>`, migrates into it and drops it after. Without that, one test's `TRUNCATE` breaks another's expectations, and the failure looks like a bug in the store.

## Caveats

- **Padded corpus.** 434 real prompts is far too few for the planner to prefer an index, so the corpus is padded to 20,000 with Gaussian perturbations of real vectors. That is a fair model of a cache full of near-duplicates, and it is why the intact baseline is 0.815 rather than ~1.0: the top-5 is genuinely crowded. It is not a claim about behaviour at 10⁶ entries.
- **Not a latency benchmark.** Medians on a laptop Docker container with `enable_seqscan = off` and autovacuum disabled. The ~1 ms figure is a floor for comparison between states, not a production number.
- **The dead share is swept, not sampled from reality.** How often a real workload sits at 90% dead depends entirely on its mutation rate; the table says what happens *if*, not how likely it is. Below 75% dead, TTL costs a few points of recall and nothing else.
- **`ef_search` is left at the default 40.** Raising it lifts every column, including TTL's — a bigger candidate pool is another read-side mitigation with the same shape as iterative scan, paid on every lookup.
- **One backend.** The Redis path in the design doc (eager `DEL` driven by a reverse index) is not implemented, so "one store owns entries, vectors and tags" is demonstrated, not compared.

## Next

M4: the gateway plugin, a compose stack, and the `lang` column wired into the language gate — where the last of M2's false hits goes to die.
