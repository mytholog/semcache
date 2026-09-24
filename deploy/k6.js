// Накладные расходы прокси при фиксированном arrival rate. Upstream должен
// быть локальным эхо, иначе в p99 попадёт задержка провайдера, а не кэша.
// Без k6 ту же разницу на мгновенном upstream даёт
// SEMCACHE_OVERHEAD=1 go test -run TestOverhead ./cmd/semcached.
//
//	k6 run deploy/k6.js
import http from "k6/http";

export const options = {
  scenarios: {
    overhead: {
      executor: "constant-arrival-rate",
      rate: 50,
      timeUnit: "1s",
      duration: "30s",
      preAllocatedVUs: 20,
    },
  },
};

export default function () {
  http.post(
    "http://127.0.0.1:8080/v1/chat/completions",
    JSON.stringify({
      model: "gpt-4o-mini",
      messages: [{ role: "user", content: "How do I enable 2FA?" }],
    }),
    { headers: { "Content-Type": "application/json" } },
  );
}
