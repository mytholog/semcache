# semcache — architecture decisions

- Cosine similarity is not a safe cache key. At theta 0.90 on the 616-pair v1 set it serves
  37% wrong answers, because "disable 2FA" scores higher against "enable 2FA" than a
  legitimate paraphrase does. No threshold fixes it: the ranking itself is wrong.
- The second stage sees both prompts. LLM judge: 97% hit, 4.9% false-hit, 5.3% of the spend
  it saves. Cross-encoder at tau 0.999: 72% hit, 2.8% false-hit, free and local.
- A deterministic language gate runs in front of stage 2. It removes 3.5 points of false hits
  at zero cost in recall, because an answer in the wrong language is never interchangeable.
- Language is detected once at write time from the full answer and stored in `Entry.Lang`.
  A five-word query carries too little signal; the only honest statement about a short query
  is "it is definitely not in language L", which is why `LangChecker` has `NotLanguage`.
- Invalidation is eager and tagged, not TTL. At 99% dead entries TTL-only retrieval returns
  0.39 candidates per lookup because the ANN graph is full of corpses; eager DELETE holds
  0.979 recall. Eager invalidation buys back recall and latency, not disk: the HNSW index
  keeps its high-water mark until VACUUM FULL, which does not belong on an invalidation path.
- A cache hit returns the provider's response body byte for byte. Rebuilding a response around
  stored answer text would silently drop fields the proxy does not know about.
- Entries are namespaced by model. One model's answer is never served for another's request.
- Metrics are hand-written Prometheus text. `client_golang` pulls in protobuf and procfs, and
  the code is meant to be embeddable in someone else's gateway, where every dependency is a
  review objection.
