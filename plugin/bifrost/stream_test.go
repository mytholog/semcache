package bifrost

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/mytholog/semcache"
	"github.com/mytholog/semcache/verify"
)

func streamRequest(prompt string) *schemas.BifrostRequest {
	req := chatRequest(prompt, nil)
	req.RequestType = schemas.ChatCompletionStreamRequest
	return req
}

// chunkResponse — один чанк потока: дельта текста и, в последнем, finish_reason.
func chunkResponse(delta string, finish *string) *schemas.BifrostResponse {
	return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		ID:     "chatcmpl-stream",
		Model:  "gpt-4o-mini",
		Object: objectChunk,
		Usage:  &schemas.BifrostLLMUsage{TotalTokens: 11},
		Choices: []schemas.BifrostResponseChoice{{
			Index:        0,
			FinishReason: finish,
			ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{
				Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &delta},
			},
		}},
	}}
}

// streamMiss прогоняет стримовый промах: PreLLMHook, затем чанки в PostLLMHook
// так, как их подаёт core — по одному, с признаком конца перед последним.
func streamMiss(t *testing.T, p *Plugin, prompt string, deltas ...string) {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	_, short, err := p.PreLLMHook(ctx, streamRequest(prompt))
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}
	if short != nil {
		t.Fatal("first streaming request must be a miss")
	}

	for _, d := range deltas {
		if _, _, err := p.PostLLMHook(ctx, chunkResponse(d, nil), nil); err != nil {
			t.Fatalf("PostLLMHook: %v", err)
		}
	}
	stop := finishStop
	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	if _, _, err := p.PostLLMHook(ctx, chunkResponse("", &stop), nil); err != nil {
		t.Fatalf("PostLLMHook: %v", err)
	}
	p.pending.Wait()
}

func TestStreamedAnswerIsAssembledAndCached(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	const prompt = "How do I reset my password?"
	streamMiss(t, p, prompt, "Open ", "settings ", "and click reset.")

	if got := p.Counters()["written"]; got != 1 {
		t.Fatalf("writes = %d, want 1: a streamed answer must reach the cache", got)
	}

	// Записанное читается и нестримовым запросом: иначе стримящий клиент
	// наполнял бы кэш, из которого никто не может прочитать.
	short := roundTrip(t, p, chatRequest(prompt, nil), nil)
	if short == nil || short.Response == nil {
		t.Fatal("the assembled answer must be served to a non-streaming request")
	}
	if got, want := answerText(short.Response.ChatResponse), "Open settings and click reset."; got != want {
		t.Errorf("assembled answer = %q, want %q", got, want)
	}
	if usage := short.Response.ChatResponse.Usage; usage == nil || usage.TotalTokens != 11 {
		t.Errorf("usage = %+v, want total_tokens 11 carried over from the final chunk", usage)
	}
}

func TestStreamedHitIsReplayedAsAStream(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	const prompt = "How do I reset my password?"
	if short := roundTrip(t, p, chatRequest(prompt, nil), chatResponse("Open settings.")); short != nil {
		t.Fatal("first request must be a miss")
	}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, short, err := p.PreLLMHook(ctx, streamRequest(prompt))
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}
	if short == nil || short.Stream == nil {
		t.Fatal("a streaming request must be served from cache as a stream, not as a whole response")
	}
	if short.Response != nil {
		t.Error("a streaming short-circuit must not also carry a whole response")
	}

	var chunks []*schemas.BifrostStreamChunk
	for c := range short.Stream {
		chunks = append(chunks, c)
	}
	// Канал обязан закрыться: core дренирует его в цикле и иначе ждал бы вечно.
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}

	chunk := chunks[0].BifrostChatResponse
	if chunk == nil {
		t.Fatal("chunk carries no chat response")
	}
	if chunk.Object != objectChunk {
		t.Errorf("object = %q, want %q", chunk.Object, objectChunk)
	}
	if len(chunk.Choices) != 1 || chunk.Choices[0].ChatStreamResponseChoice == nil {
		t.Fatalf("chunk choices = %+v, want one stream choice", chunk.Choices)
	}
	delta := chunk.Choices[0].ChatStreamResponseChoice.Delta
	if delta == nil || delta.Content == nil || *delta.Content != "Open settings." {
		t.Errorf("delta content = %+v, want the cached answer", delta)
	}
	if delta.Role == nil || *delta.Role != roleAssistant {
		t.Errorf("delta role = %+v, want %q", delta.Role, roleAssistant)
	}
	if fr := chunk.Choices[0].FinishReason; fr == nil || *fr != finishStop {
		t.Errorf("finish_reason = %+v, want %q", fr, finishStop)
	}
	if done, _ := ctx.Value(schemas.BifrostContextKeyStreamEndIndicator).(bool); !done {
		t.Error("the producer must mark the end of the stream so core can finalize the request")
	}
}

func TestReplayedStreamIsNotCachedAgain(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	const prompt = "How do I reset my password?"
	if short := roundTrip(t, p, chatRequest(prompt, nil), chatResponse("Open settings.")); short != nil {
		t.Fatal("first request must be a miss")
	}

	// core вызывает PostLLMHook и на отданных нами чанках. Записи это
	// порождать не должно: писать нечего, ответ пришёл из кэша.
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, short, err := p.PreLLMHook(ctx, streamRequest(prompt))
	if err != nil {
		t.Fatalf("PreLLMHook: %v", err)
	}
	if short == nil || short.Stream == nil {
		t.Fatal("cached streaming request must short-circuit")
	}
	for c := range short.Stream {
		if _, _, err := p.PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: c.BifrostChatResponse}, nil); err != nil {
			t.Fatalf("PostLLMHook: %v", err)
		}
	}
	p.pending.Wait()

	if got := p.Counters()["written"]; got != 1 {
		t.Errorf("writes = %d, want 1: replaying a hit must not write it back", got)
	}
}

func TestStreamWithoutEndIndicatorIsCachedOnFinishReason(t *testing.T) {
	t.Parallel()
	// Признак конца ставит провайдер, и не каждый его ставит. Без запасного
	// признака такой поток не попал бы в кэш никогда.
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, _, err := p.PreLLMHook(ctx, streamRequest("When is the invoice due?")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.PostLLMHook(ctx, chunkResponse("Due on the 1st.", nil), nil); err != nil {
		t.Fatal(err)
	}
	stop := finishStop
	if _, _, err := p.PostLLMHook(ctx, chunkResponse("", &stop), nil); err != nil {
		t.Fatal(err)
	}
	p.pending.Wait()

	if got := p.Counters()["written"]; got != 1 {
		t.Errorf("writes = %d, want 1", got)
	}
}

func TestEmptyStreamIsNotCached(t *testing.T) {
	t.Parallel()
	// Поток, оборвавшийся без единой дельты, кэшировать нечем: пустой ответ
	// в кэше хуже промаха, потому что он выглядит как настоящий.
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if _, _, err := p.PreLLMHook(ctx, streamRequest("How do I reset my password?")); err != nil {
		t.Fatal(err)
	}
	stop := finishStop
	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	if _, _, err := p.PostLLMHook(ctx, chunkResponse("", &stop), nil); err != nil {
		t.Fatal(err)
	}
	p.pending.Wait()

	if got := p.Counters()["written"]; got != 0 {
		t.Errorf("writes = %d, want 0", got)
	}
	if got := p.Counters()[counterError+":assemble"]; got != 1 {
		t.Errorf("assemble errors = %d, want 1", got)
	}
}

func TestStreamingRequestStillHonoursBypass(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	two := 2
	req := streamRequest("How do I reset my password?")
	req.ChatRequest.Params = &schemas.ChatParameters{N: &two}

	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, short, err := p.PreLLMHook(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if short != nil {
		t.Fatal("a bypassed streaming request must not short-circuit")
	}
	if got := p.Counters()[counterBypass+":"+bypassMultipleChoices]; got != 1 {
		t.Errorf("bypass %s = %d, want 1", bypassMultipleChoices, got)
	}
}

func TestNonStreamingHitStaysAWholeResponse(t *testing.T) {
	t.Parallel()
	p := newTestPlugin(t, verify.Noop{}, stubEmbedder{})

	const prompt = "How do I reset my password?"
	if short := roundTrip(t, p, chatRequest(prompt, nil), chatResponse("Open settings.")); short != nil {
		t.Fatal("first request must be a miss")
	}

	short := roundTrip(t, p, chatRequest(prompt, nil), nil)
	if short == nil || short.Response == nil {
		t.Fatal("cached request must short-circuit with a whole response")
	}
	if short.Stream != nil {
		t.Error("a non-streaming request must not be answered with a stream")
	}
	if got := p.Counters()[semcache.KindExact]; got != 1 {
		t.Errorf("exact outcomes = %d, want 1", got)
	}
}
