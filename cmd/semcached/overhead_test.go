package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/mytholog/semcache"
	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
)

// TestOverhead измеряет добавленную задержку прокси на локальном upstream,
// который отвечает сразу. Число имеет смысл только так: реальный провайдер
// спрятал бы его в собственном p99. Запуск: SEMCACHE_OVERHEAD=1 go test -run TestOverhead -v
func TestOverhead(t *testing.T) {
	if os.Getenv("SEMCACHE_OVERHEAD") == "" {
		t.Skip("set SEMCACHE_OVERHEAD=1 to measure proxy overhead")
	}

	const (
		workers = 8
		each    = 100
	)
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"How do I enable 2FA?"}]}`)
	answer := []byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"Open Settings, then Security, then turn on 2FA."},"finish_reason":"stop"}],"usage":{"total_tokens":20}}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	}))
	defer upstream.Close()

	proxy := httptest.NewServer((&Server{
		Cache: &semcache.Cache{
			Store:    store.NewMemory(),
			Embedder: stubEmbedder{},
			Verifier: verify.Noop{},
		},
		Upstream: NewUpstream(upstream.URL, "test", 5*time.Second, 1<<20),
		Metrics:  NewMetrics(),
		Log:      slog.New(slog.DiscardHandler),
		MaxBody:  1 << 20,
		FailOpen: true,
	}).Routes())
	defer proxy.Close()

	post := func(url string, payload []byte) error {
		resp, err := http.Post(url+"/v1/chat/completions", "application/json", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}
	if err := post(proxy.URL, body); err != nil {
		t.Fatalf("warm: %v", err)
	}

	sample := func(url string, payload []byte) []time.Duration {
		out := make([]time.Duration, workers*each)
		var wg sync.WaitGroup
		errCh := make(chan error, workers)
		for w := range workers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := range each {
					start := time.Now()
					if err := post(url, payload); err != nil {
						errCh <- err
						return
					}
					out[w*each+i] = time.Since(start)
				}
			}(w)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatal(err)
		}
		return out
	}

	missBody := func(tag string, i int) []byte {
		return fmt.Appendf(nil, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"unique %s %d"}]}`, tag, i)
	}
	sampleMiss := func(tag string) []time.Duration {
		out := make([]time.Duration, workers*each)
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := range each {
					payload := missBody(tag, w*each+i)
					start := time.Now()
					if err := post(proxy.URL, payload); err != nil {
						t.Errorf("miss: %v", err)
						return
					}
					out[w*each+i] = time.Since(start)
				}
			}(w)
		}
		wg.Wait()
		return out
	}

	// Первый проход только прогревает пул соединений. Иначе холодный прямой
	// вызов выглядит медленнее прокси, и «накладные расходы» уходят в минус.
	_ = sample(upstream.URL, body)
	_ = sample(proxy.URL, body)
	_ = sampleMiss("warm")

	direct := sample(upstream.URL, body)
	hit := sample(proxy.URL, body)
	miss := sampleMiss("miss")

	d50, d99 := percentile(direct, 0.50), percentile(direct, 0.99)
	h50, h99 := percentile(hit, 0.50), percentile(hit, 0.99)
	m50, m99 := percentile(miss, 0.50), percentile(miss, 0.99)
	t.Logf("direct  p50=%s p99=%s", d50, d99)
	t.Logf("exact   p50=%s p99=%s", h50, h99)
	t.Logf("miss    p50=%s p99=%s", m50, m99)
	t.Logf("overhead on miss p50=%s p99=%s", m50-d50, m99-d99)
}

func percentile(samples []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}
