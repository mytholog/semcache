package bifrost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/mytholog/semcache"
	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
)

// stubEmbedder даёт близкие векторы текстам с общими словами: тестам нужен
// порядок кандидатов, а не качество эмбеддингов.
type stubEmbedder struct{ fail error }

func (s stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 26)
		for _, r := range t {
			if r >= 'a' && r <= 'z' {
				v[r-'a']++
			}
		}
		norm := float32(0)
		for _, x := range v {
			norm += x * x
		}
		if norm > 0 {
			for j := range v {
				v[j] /= float32(sqrt(float64(norm)))
			}
		}
		out[i] = v
	}
	return out, nil
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for range 20 {
		z = (z + x/z) / 2
	}
	return z
}

type rejectingVerifier struct{}

func (rejectingVerifier) Interchangeable(context.Context, string, string) (verify.Decision, error) {
	return verify.Decision{OK: false, Reason: "different intent"}, nil
}

type failingVerifier struct{}

func (failingVerifier) Interchangeable(context.Context, string, string) (verify.Decision, error) {
	return verify.Decision{}, errors.New("verifier must not be called")
}

func newTestPlugin(t *testing.T, v verify.Verifier, emb semcache.Embedder) *Plugin {
	t.Helper()
	p, err := New(Config{
		Cache: &semcache.Cache{
			Store:       store.NewMemory(),
			Embedder:    emb,
			Verifier:    v,
			RetrieveMin: 0.70,
			K:           5,
		},
		MaxPendingWrites: 1,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Cleanup() })
	return p
}

func chatRequest(prompt string, mutate func(*schemas.BifrostChatRequest)) *schemas.BifrostRequest {
	req := &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    "gpt-4o-mini",
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &prompt},
		}},
	}
	if mutate != nil {
		mutate(req)
	}
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: req}
}

func chatResponse(answer string) *schemas.BifrostResponse {
	finish := "stop"
	return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		ID:                "chatcmpl-1",
		Model:             "gpt-4o-mini",
		Object:            "chat.completion",
		SystemFingerprint: "fp_test",
		Usage:             &schemas.BifrostLLMUsage{TotalTokens: 7},
		Choices: []schemas.BifrostResponseChoice{{
			Index:        0,
			FinishReason: &finish,
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
				Message: &schemas.ChatMessage{
					Role:    schemas.ChatMessageRoleAssistant,
					Content: &schemas.ChatMessageContent{ContentStr: &answer},
				},
			},
		}},
	}}
}

// roundTrip прогоняет один запрос через оба хука и возвращает закоротку.
func roundTrip(t *testing.T, p *Plugin, req *schemas.BifrostRequest, resp *schemas.BifrostResponse) *schemas.LLMPluginShortCircuit {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, short, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}
	if short != nil {
		return short
	}
	if _, _, err := p.PostLLMHook(ctx, resp, nil); err != nil {
		t.Fatalf("PostLLMHook: %v", err)
	}
	p.pending.Wait()
	return nil
}

func TestExactHitShortCircuits(t *testing.T) {
	t.Parallel()
	// Верификатор падает: точное совпадение не должно его звать.
	p := newTestPlugin(t, failingVerifier{}, stubEmbedder{})

	req := chatRequest("How do I reset my password?", nil)
	if short := roundTrip(t, p, req, chatResponse("Open settings and click reset.")); short != nil {
		t.Fatal("first request must be a miss")
	}

	short := roundTrip(t, p, chatRequest("How do I reset my password?", nil), nil)
	if short == nil || short.Response == nil || short.Response.ChatResponse == nil {
		t.Fatal("second identical request must short-circuit with a cached response")
	}
	if got := p.Counters()[semcache.KindExact]; got != 1 {
		t.Errorf("exact outcomes = %d, want 1", got)
	}
}

func TestVerifiedHitShortCircuits(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	if short := roundTrip(t, p, chatRequest("How do I reset my password?", nil), chatResponse("Open settings.")); short != nil {
		t.Fatal("first request must be a miss")
	}

	short := roundTrip(t, p, chatRequest("How do i reset my passwords?", nil), nil)
	if short == nil || short.Response == nil {
		t.Fatal("a paraphrase accepted by the verifier must short-circuit")
	}
	if got := p.Counters()[semcache.KindVerified]; got != 1 {
		t.Errorf("verified outcomes = %d, want 1", got)
	}
}

func TestRejectedCandidateGoesUpstream(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, rejectingVerifier{}, stubEmbedder{})

	if short := roundTrip(t, p, chatRequest("How do I reset my password?", nil), chatResponse("Open settings.")); short != nil {
		t.Fatal("first request must be a miss")
	}

	short := roundTrip(t, p, chatRequest("How do i reset my passwords?", nil), chatResponse("Другой ответ."))
	if short != nil {
		t.Fatal("a rejected candidate must not short-circuit")
	}
	if got := p.Counters()[semcache.KindReject]; got != 1 {
		t.Errorf("reject outcomes = %d, want 1", got)
	}
}

func TestPayloadSurvivesRoundTrip(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	req := chatRequest("When is the invoice due?", nil)
	original := chatResponse("The invoice is due on the 1st.")
	if short := roundTrip(t, p, req, original); short != nil {
		t.Fatal("first request must be a miss")
	}

	short := roundTrip(t, p, chatRequest("When is the invoice due?", nil), nil)
	if short == nil || short.Response == nil {
		t.Fatal("cached request must short-circuit")
	}
	got := short.Response.ChatResponse
	want := original.ChatResponse
	if got.ID != want.ID {
		t.Errorf("id = %q, want %q", got.ID, want.ID)
	}
	if got.SystemFingerprint != want.SystemFingerprint {
		t.Errorf("system_fingerprint = %q, want %q", got.SystemFingerprint, want.SystemFingerprint)
	}
	if got.Usage == nil || got.Usage.TotalTokens != want.Usage.TotalTokens {
		t.Errorf("usage = %+v, want total_tokens %d", got.Usage, want.Usage.TotalTokens)
	}
	if answerText(got) != answerText(want) {
		t.Errorf("answer = %q, want %q", answerText(got), answerText(want))
	}
}

func TestBypassIsNotCached(t *testing.T) {
	t.Parallel()
	two := 2
	blockType := schemas.ChatContentBlockTypeText
	text := "How do I reset my password?"

	cases := []struct {
		name   string
		req    *schemas.BifrostRequest
		reason string
	}{
		{
			name: "multiple choices",
			req: chatRequest(text, func(r *schemas.BifrostChatRequest) {
				r.Params = &schemas.ChatParameters{N: &two}
			}),
			reason: bypassMultipleChoices,
		},
		{
			name: "tools",
			req: chatRequest(text, func(r *schemas.BifrostChatRequest) {
				r.Params = &schemas.ChatParameters{Tools: []schemas.ChatTool{{Type: schemas.ChatToolTypeFunction}}}
			}),
			reason: bypassTools,
		},
		{
			name: "non-text content",
			req: chatRequest(text, func(r *schemas.BifrostChatRequest) {
				r.Input[0].Content = &schemas.ChatMessageContent{
					ContentBlocks: []schemas.ChatContentBlock{{Type: blockType, Text: &text}},
				}
			}),
			reason: bypassNonTextContent,
		},
		{
			name:   "no messages",
			req:    chatRequest(text, func(r *schemas.BifrostChatRequest) { r.Input = nil }),
			reason: bypassNoMessages,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

			if short := roundTrip(t, p, tc.req, chatResponse("Open settings.")); short != nil {
				t.Fatal("a bypassed request must not short-circuit")
			}
			if got := p.Counters()[counterBypass+":"+tc.reason]; got != 1 {
				t.Errorf("bypass %s = %d, want 1", tc.reason, got)
			}
			if got := p.Counters()["written"]; got != 0 {
				t.Errorf("writes = %d, want 0: a bypassed request must not be cached", got)
			}
		})
	}
}

func TestDifferentProviderOrModelIsNotAHit(t *testing.T) {
	t.Parallel()
	const prompt = "How do I reset my password?"

	cases := []struct {
		name   string
		mutate func(*schemas.BifrostChatRequest)
	}{
		{"other model", func(r *schemas.BifrostChatRequest) { r.Model = "gpt-4o" }},
		{"other provider", func(r *schemas.BifrostChatRequest) { r.Provider = schemas.Anthropic }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

			if short := roundTrip(t, p, chatRequest(prompt, nil), chatResponse("Open settings.")); short != nil {
				t.Fatal("first request must be a miss")
			}
			// Без этой проверки тест проходил бы и тогда, когда первая запись
			// вообще не легла в кэш, — то есть по неверной причине.
			if got := p.Counters()["written"]; got != 1 {
				t.Fatalf("writes = %d, want 1 before the second request", got)
			}
			if short := roundTrip(t, p, chatRequest(prompt, tc.mutate), chatResponse("Open settings.")); short != nil {
				t.Fatal("one deployment's answer must not be served for another")
			}
		})
	}
}

func TestLookupErrorFailsOpen(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{fail: errors.New("embedder is down")})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, short, err := p.PreLLMHook(ctx, chatRequest("How do I reset my password?", nil))
	if err != nil {
		t.Fatalf("PreLLMHook returned an error: %v", err)
	}
	if short != nil {
		t.Fatal("a broken cache must let the request reach the provider")
	}
	if got := p.Counters()[counterError+":lookup"]; got != 1 {
		t.Errorf("lookup errors = %d, want 1", got)
	}
}

func TestLookupErrorFailsClosed(t *testing.T) {
	t.Parallel()
	p, err := New(Config{
		Cache: &semcache.Cache{
			Store:    store.NewMemory(),
			Embedder: stubEmbedder{fail: errors.New("embedder is down")},
			Verifier: verify.Noop{},
		},
		FailClosed: true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Cleanup() })

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, short, hookErr := p.PreLLMHook(ctx, chatRequest("How do I reset my password?", nil))
	if hookErr != nil {
		t.Fatalf("PreLLMHook returned an error: %v", hookErr)
	}
	if short == nil || short.Error == nil {
		t.Fatal("FailClosed must short-circuit with an error")
	}
}

func TestUpstreamErrorIsNotCached(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, _, err := p.PreLLMHook(ctx, chatRequest("How do I reset my password?", nil)); err != nil {
		t.Fatal(err)
	}
	bifrostErr := &schemas.BifrostError{Error: &schemas.ErrorField{Message: "slow down"}}
	if _, _, err := p.PostLLMHook(ctx, nil, bifrostErr); err != nil {
		t.Fatal(err)
	}
	p.pending.Wait()

	if got := p.Counters()["written"]; got != 0 {
		t.Errorf("writes = %d, want 0: a provider error must not become a cache entry", got)
	}
}

func TestNonChatRequestIsIgnored(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, failingVerifier{}, stubEmbedder{})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	req := &schemas.BifrostRequest{RequestType: schemas.EmbeddingRequest}
	_, short, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if short != nil {
		t.Fatal("a non-chat request must pass through untouched")
	}
}

func TestNewRequiresCache(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); !errors.Is(err, ErrNoCache) {
		t.Fatalf("New without a cache: %v, want ErrNoCache", err)
	}
}
