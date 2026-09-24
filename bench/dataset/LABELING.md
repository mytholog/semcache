# Dataset labeling and provenance

## Files

| File | What it is |
|---|---|
| `pilot.jsonl` | 80 hand-written pairs, 10 per category. Gold subset. `human_authored: true`, `source: hand`. |
| `v1.jsonl` | Gold plus template expansions. Built by `go run ./tools/genv1`. Target 600–1000 pairs. |
| `v2.jsonl` | v1 plus an answer and `answer_lang` on every pair, and same-language paraphrase/format pairs for each language in the switch set. Built by `go run ./tools/genv2`. |

Do not edit `v1.jsonl` or `v2.jsonl` by hand. Change the generator and regenerate.

## Question the label answers

> Can the cached answer for prompt B be served for prompt A?

Not "are these topically similar". Not "would a search engine retrieve both".

## Category → label (hard rule)

The category implies `interchangeable`. A mismatch is a dataset bug, not a judgment call.

| Category | Interchangeable | What differs |
|---|---|---|
| `negation` | no | Polarity (`with`/`without`, `include`/`exclude`, `enable`/`disable`) |
| `entity_swap` | no | A named entity (jurisdiction, env, vendor, region) |
| `numeric` | no | A number that is the point of the question |
| `temporal` | no | Version, date, before/after |
| `scope` | no | Who or what the rule applies to |
| `paraphrase` | yes | Wording; same information need and same language |
| `format_only` | yes | Politeness, case, punctuation, whitespace, emoji |
| `language_switch` | no | Same information need, different language of the answer |

`language_switch` is "no" because serving a cached English answer to a Russian question is a production bug even when the facts match.

## How v1 was built

1. Hand-write 10 pairs per category (the pilot). These are the only pairs claimed as fully human-authored.
2. Expand **minimal-pair templates** in `tools/genv1`. Each generated pair flips exactly one feature of the taxonomy. `human_authored: false`, `source: template`.
3. Dedup by id and by unordered `(a, b)` text against the gold set.

Template expansions are not individually copy-edited. Review is of the template families, plus spot-checks. That is a limitation of v1 and must be stated next to any published number.

## Known limitations

- Domain is synthetic SaaS/infra support, not a production trace.
- English-heavy; `language_switch` is mostly EN paired with RU/DE/FR/ES/JA/ZH/PL/IT/PT/TR.
- v2 answers are one canned paragraph per language, not a model completion. `answer_lang` is assigned by script and a few Latin cues, then checked by failing the generator when a switch cannot be labeled. The new positive pairs are templates and are not individually reviewed. Chinese is in the file, but the detector used by the gate does not include it, so those pairs do not move the gate measurement.
- `paraphrase` templates lean on "How do I X?" / "What is the process for X?" — easier than messy real paraphrases, which makes the cosine-vs-paraphrase result *conservative* if it still fails.
- One pair is ~0.1–0.2 percentage points on v1; treat per-category cells under ~50 pairs as noisy.
