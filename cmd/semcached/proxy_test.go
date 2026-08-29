package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mytholog/semcache"
	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
)

// roundTripFunc подменяет транспорт: тесты не открывают сокетов, поэтому в них
// нет ни портов, ни ожидания готовности сервера.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeUpstream отвечает заданным текстом и считает вызовы: именно их число
// показывает, работает ли кэш.
type fakeUpstream struct {
	calls  atomic.Int64
	answer string
	status int
	body   string
}

func (f *fakeUpstream) upstream() *Upstream {
	return &Upstream{
		BaseURL: "http://provider.invalid/v1",
		APIKey:  "test",
		MaxBody: 1 << 20,
		Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			f.calls.Add(1)
			body := f.body
			if body == "" {
				body = fmt.Sprintf(`{"id":"chatcmpl-1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"total_tokens":7}}`, f.answer)
			}
			status := f.status
			if status == 0 {
				status = http.StatusOK
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    r,
			}, nil
		})},
	}
}

// stubEmbedder: близость задаётся первым словом промпта, чтобы тест проверял
// прокси, а не модель эмбеддингов.
type stubEmbedder struct {
	fail error
}

func (s stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		switch {
		case strings.Contains(t, "password"):
			out[i] = []float32{1, 0}
		case strings.Contains(t, "invoice"):
			out[i] = []float32{0, 1}
		default:
			out[i] = []float32{0.7071, 0.7071}
		}
	}
	return out, nil
}

func newTestServer(t *testing.T, up *Upstream, v verify.Verifier, emb semcache.Embedder) *Server {
	t.Helper()
	return &Server{
		Cache: &semcache.Cache{
			Store:       store.NewMemory(),
			Embedder:    emb,
			Verifier:    v,
			RetrieveMin: 0.70,
			K:           5,
		},
		Upstream: up,
		Metrics:  NewMetrics(),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBody:  1 << 20,
		FailOpen: true,
	}
}

func chatBody(t *testing.T, prompt string, extra map[string]any) []byte {
	t.Helper()
	req := map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	for k, v := range extra {
		req[k] = v
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func post(t *testing.T, srv *Server, path string, body []byte, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, vs := range header {
		r.Header[k] = vs
	}
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w
}

func TestChatCachesAndServesVerifiedHit(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Open settings and click reset."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	first := post(t, srv, "/v1/chat/completions", chatBody(t, "How do I reset my password?", nil), nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get(headerControl); got != semcache.KindMiss {
		t.Errorf("%s = %q, want miss", headerControl, got)
	}

	// Другая формулировка того же вопроса: попадание должно прийти из кэша,
	// а провайдер — не увидеть второго запроса.
	second := post(t, srv, "/v1/chat/completions", chatBody(t, "password reset, how?", nil), nil)
	if got := second.Header().Get(headerControl); got != semcache.KindVerified {
		t.Fatalf("%s = %q, want verified", headerControl, got)
	}
	if calls := up.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}

	// Попадание отдаёт тело провайдера как есть, а не пересобранное.
	if first.Body.String() != second.Body.String() {
		t.Errorf("cache hit body differs from the provider response:\n%s\n%s", first.Body.String(), second.Body.String())
	}
}

func TestChatExactHitSkipsVerifier(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	// Верификатор, который падает: точное совпадение не должно его звать.
	srv := newTestServer(t, up.upstream(), failingVerifier{}, stubEmbedder{})

	body := chatBody(t, "How do I reset my password?", nil)
	post(t, srv, "/v1/chat/completions", body, nil)
	second := post(t, srv, "/v1/chat/completions", body, nil)

	if got := second.Header().Get(headerControl); got != semcache.KindExact {
		t.Fatalf("%s = %q, want exact", headerControl, got)
	}
	if calls := up.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}
}

type failingVerifier struct{}

func (failingVerifier) Interchangeable(context.Context, string, string) (verify.Decision, error) {
	return verify.Decision{}, errors.New("verifier must not be called")
}

func TestChatRejectedCandidateGoesUpstream(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), rejectingVerifier{}, stubEmbedder{})

	post(t, srv, "/v1/chat/completions", chatBody(t, "How do I reset my password?", nil), nil)
	second := post(t, srv, "/v1/chat/completions", chatBody(t, "password reset, how?", nil), nil)

	if got := second.Header().Get(headerControl); got != semcache.KindReject {
		t.Fatalf("%s = %q, want reject", headerControl, got)
	}
	if calls := up.calls.Load(); calls != 2 {
		t.Errorf("upstream calls = %d, want 2: a rejected candidate must not be served", calls)
	}
}

type rejectingVerifier struct{}

func (rejectingVerifier) Interchangeable(context.Context, string, string) (verify.Decision, error) {
	return verify.Decision{OK: false, Reason: "different intent"}, nil
}

func TestChatDifferentModelIsNotAHit(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	prompt := "How do I reset my password?"
	post(t, srv, "/v1/chat/completions", chatBody(t, prompt, nil), nil)
	second := post(t, srv, "/v1/chat/completions", chatBody(t, prompt, map[string]any{"model": "gpt-4o"}), nil)

	if got := second.Header().Get(headerControl); got != semcache.KindMiss {
		t.Fatalf("%s = %q, want miss: gpt-4o must not get gpt-4o-mini's answer", headerControl, got)
	}
}

func TestChatBypass(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	cases := []struct {
		name   string
		body   []byte
		header http.Header
		reason string
	}{
		{
			name:   "client asks to bypass",
			body:   chatBody(t, "How do I reset my password?", nil),
			header: http.Header{headerControl: []string{"bypass"}},
			reason: "client",
		},
		{
			name:   "several samples requested",
			body:   chatBody(t, "How do I reset my password?", map[string]any{"n": 3}),
			reason: "multiple_choices",
		},
		{
			name:   "tools change the answer",
			body:   chatBody(t, "How do I reset my password?", map[string]any{"tools": []map[string]string{{"type": "function"}}}),
			reason: "tools",
		},
		{
			name: "multimodal content is not a text key",
			body: []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
			// Ключ из одного текста не описывает картинку — кэшировать нельзя.
			reason: "non_text_content",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := up.calls.Load()
			w := post(t, srv, "/v1/chat/completions", tc.body, tc.header)
			if got := w.Header().Get(headerReason); got != tc.reason {
				t.Errorf("%s = %q, want %q", headerReason, got, tc.reason)
			}
			if up.calls.Load() != before+1 {
				t.Errorf("the request must still reach the provider")
			}
			// Повтор тоже уходит наверх: обход кэша не должен ничего записать.
			w = post(t, srv, "/v1/chat/completions", tc.body, tc.header)
			if got := w.Header().Get(headerControl); got == semcache.KindExact || got == semcache.KindVerified {
				t.Errorf("%s = %q: a bypassed request must not have been cached", headerControl, got)
			}
		})
	}
}

func TestChatStreamingHitIsReplayedAsSSE(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Open settings."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	// Промах с stream=true не кэшируется: тело — кадры SSE, а не ответ.
	post(t, srv, "/v1/chat/completions", chatBody(t, "How do I reset my password?", nil), nil)
	w := post(t, srv, "/v1/chat/completions", chatBody(t, "password reset, how?", map[string]any{"stream": true}), nil)

	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Open settings.") || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream body is not a replay of the cached answer:\n%s", body)
	}
	if calls := up.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}
}

func TestChatUpstreamErrorIsNotCached(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{status: http.StatusTooManyRequests, body: `{"error":{"message":"slow down"}}`}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	body := chatBody(t, "How do I reset my password?", nil)
	first := post(t, srv, "/v1/chat/completions", body, nil)
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the provider status passed through", first.Code)
	}

	second := post(t, srv, "/v1/chat/completions", body, nil)
	if got := second.Header().Get(headerControl); got != semcache.KindMiss {
		t.Errorf("%s = %q: an error response must not become a cache entry", headerControl, got)
	}
	if calls := up.calls.Load(); calls != 2 {
		t.Errorf("upstream calls = %d, want 2", calls)
	}
}

func TestChatFailOpenOnCacheError(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{fail: errors.New("embedder is down")})

	w := post(t, srv, "/v1/chat/completions", chatBody(t, "How do I reset my password?", nil), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the request to survive a broken cache", w.Code)
	}
	if calls := up.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1", calls)
	}
}

func TestChatFailClosedOnCacheError(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{fail: errors.New("embedder is down")})
	srv.FailOpen = false

	w := post(t, srv, "/v1/chat/completions", chatBody(t, "How do I reset my password?", nil), nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if calls := up.calls.Load(); calls != 0 {
		t.Errorf("upstream calls = %d, want 0", calls)
	}
}

func TestInvalidateByTagRemovesEntry(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "The invoice is due on the 1st."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	body := chatBody(t, "When is the invoice due?", nil)
	post(t, srv, "/v1/chat/completions", body, http.Header{headerTags: []string{"doc:42, tpl:v3"}})

	w := post(t, srv, "/invalidate", []byte(`{"tags":["doc:42"]}`), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got struct{ Removed int }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Removed != 1 {
		t.Fatalf("removed = %d, want 1", got.Removed)
	}

	again := post(t, srv, "/v1/chat/completions", body, nil)
	if got := again.Header().Get(headerControl); got != semcache.KindMiss {
		t.Errorf("%s = %q, want miss after invalidation", headerControl, got)
	}
}

func TestInvalidateRequiresTags(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, (&fakeUpstream{}).upstream(), verify.Noop{}, stubEmbedder{})

	w := post(t, srv, "/invalidate", []byte(`{"tags":[]}`), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: invalidating everything must be explicit", w.Code)
	}
}

// Фоновая запись — отдельный путь: обработчик про неё только ставит задачу.
func TestBackgroundWriteQueue(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	var dropped atomic.Int64
	srv.writes = newWriteQueue(1, 4, 5*time.Second, func() { dropped.Add(1) })
	defer srv.writes.close()

	body := chatBody(t, "How do I reset my password?", nil)
	post(t, srv, "/v1/chat/completions", body, nil)
	srv.writes.drain()

	if got := post(t, srv, "/v1/chat/completions", body, nil).Header().Get(headerControl); got != semcache.KindExact {
		t.Fatalf("%s = %q, want exact after the background write", headerControl, got)
	}
	if dropped.Load() != 0 {
		t.Errorf("dropped = %d, want 0", dropped.Load())
	}
}

// Стадо одинаковых запросов на холодном кэше должно стоить один вызов
// провайдера, а не столько, сколько клиентов пришло одновременно.
func TestConcurrentMissesCoalesce(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	up := &fakeUpstream{answer: "Answer."}
	upstream := up.upstream()
	inner := upstream.Client.Transport
	upstream.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-release // держим первый вызов, пока не соберутся остальные
		return inner.RoundTrip(r)
	})
	srv := newTestServer(t, upstream, verify.Noop{}, stubEmbedder{})

	const clients = 8
	body := chatBody(t, "How do I reset my password?", nil)
	var wg sync.WaitGroup
	codes := make([]int, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = post(t, srv, "/v1/chat/completions", body, nil).Code
		}()
	}

	// Ждём, пока все окажутся в singleflight, и только затем отпускаем
	// провайдера: иначе тест измерял бы скорость горутин.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("client %d got %d", i, code)
		}
	}
	if calls := up.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d, want 1 for %d identical concurrent requests", calls, clients)
	}
}

func TestMetricsAreExposed(t *testing.T) {
	t.Parallel()
	up := &fakeUpstream{answer: "Answer."}
	srv := newTestServer(t, up.upstream(), verify.Noop{}, stubEmbedder{})

	body := chatBody(t, "How do I reset my password?", nil)
	post(t, srv, "/v1/chat/completions", body, nil)
	post(t, srv, "/v1/chat/completions", body, nil)

	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	out := w.Body.String()
	for _, want := range []string{
		`semcache_requests_total 2`,
		`semcache_lookups_total{kind="exact"} 1`,
		`semcache_upstream_requests_total 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics do not contain %q:\n%s", want, out)
		}
	}
}
