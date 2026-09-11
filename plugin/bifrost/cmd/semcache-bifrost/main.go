// Команда semcache-bifrost показывает двухстадийный кэш внутри Bifrost.
//
// Она поднимает гейтвей через Go SDK с плагином semcache и задаёт четыре
// вопроса: исходный, буквально тот же, перефразировку и вопрос с обратным
// смыслом. Видно должно быть четыре разных исхода — miss, exact, verified,
// reject, — и именно последний отличает эту схему от порога по косинусу: у
// вопроса с обратным смыслом косинус выше, чем у законной перефразировки.
//
//	OPENAI_API_KEY=... go run ./cmd/semcache-bifrost
//
// Без SEMCACHE_DSN записи живут в памяти процесса.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	core "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	lingualib "github.com/pemistahl/lingua-go"

	"github.com/mytholog/semcache"
	"github.com/mytholog/semcache/embed"
	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
	"github.com/mytholog/semcache/verify/lingua"

	semcachebifrost "github.com/mytholog/semcache/plugin/bifrost"
)

const (
	chatModel  = "gpt-4o-mini"
	embedModel = "text-embedding-3-small"
	judgeModel = "gpt-4o-mini"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(log)

	if err := run(context.Background(), log); err != nil {
		log.Error("semcache-bifrost failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return errors.New("OPENAI_API_KEY is not set: the cache needs it to embed prompts and to judge them")
	}

	embedder, err := embed.NewOpenAI(embed.OpenAIConfig{
		APIKey:  apiKey,
		BaseURL: "https://api.openai.com/v1",
		Model:   embedModel,
		Timeout: time.Minute,
	})
	if err != nil {
		return err
	}
	// Размерность спрашивается у модели: схема Postgres требует точного числа.
	vecs, err := embedder.Embed(ctx, []string{"semcache"})
	if err != nil {
		return fmt.Errorf("probe embedding dimensions: %w", err)
	}
	dims := len(vecs[0])

	st, closeStore, err := openStore(ctx, dims, log)
	if err != nil {
		return err
	}
	defer closeStore()

	// Судья ходит к провайдеру напрямую, минуя гейтвей: запрос судьи через тот
	// же Bifrost снова попал бы в PreLLMHook и вызвал судью для запроса судьи.
	judge := verify.NewJudge(verify.OpenAICompleter{
		Do:      directChat(apiKey),
		Model:   judgeModel,
		BaseURL: "https://api.openai.com/v1",
	}, "")

	plugin, err := semcachebifrost.New(semcachebifrost.Config{
		Cache: &semcache.Cache{
			Store:       st,
			Embedder:    embedder,
			Verifier:    judge,
			Lang:        lingua.New(lingualib.AllLanguages()),
			RetrieveMin: 0.70,
			K:           5,
		},
		Log: log,
	})
	if err != nil {
		return err
	}

	client, err := core.Init(ctx, schemas.BifrostConfig{
		Account:    &account{apiKey: apiKey},
		LLMPlugins: []schemas.LLMPlugin{plugin},
		Logger:     core.NewDefaultLogger(schemas.LogLevelWarn),
	})
	if err != nil {
		return fmt.Errorf("init bifrost: %w", err)
	}
	defer client.Shutdown()

	questions := []struct {
		label  string
		prompt string
		stream bool
	}{
		{label: "cold", prompt: "How do I reset my password?"},
		{label: "same question", prompt: "How do I reset my password?"},
		{label: "paraphrase", prompt: "What's the procedure for resetting my password?"},
		{label: "opposite meaning", prompt: "How do I stop my password from being reset?"},
		// Стриминг в обе стороны: сначала поток наполняет кэш, потом кэш
		// отдаётся потоком.
		{label: "cold, streaming", prompt: "How do I change my billing address?", stream: true},
		{label: "same, streaming", prompt: "How do I change my billing address?", stream: true},
	}

	fmt.Printf("%-18s  %8s  %s\n", "request", "latency", "answer")
	for _, q := range questions {
		// Запись идёт вне хука, поэтому без ожидания второй вопрос успевает
		// прийти раньше, чем ответ на первый попал в кэш, и становится вторым
		// промахом. В живом трафике запросы разнесены; здесь — нет.
		if err := waitForWrites(ctx, plugin); err != nil {
			return err
		}

		start := time.Now()
		answer, bifrostErr := askOrStream(ctx, client, q.prompt, q.stream)
		took := time.Since(start)
		if bifrostErr != nil {
			return fmt.Errorf("chat request %q: %s", q.label, bifrostErrText(bifrostErr))
		}
		fmt.Printf("%-18s  %7.2fs  %s\n", q.label, took.Seconds(), firstLine(answer))
	}

	fmt.Println()
	fmt.Println("cache outcomes:")
	for key, n := range plugin.Counters() {
		fmt.Printf("  %-24s %d\n", key, n)
	}
	return nil
}

// waitForWrites ждёт, пока каждый некэшированный ответ дойдёт до стора. Плагин
// не схлопывает одновременные одинаковые промахи — этого он и не может: вызов
// провайдера принадлежит гейтвею, а не хуку.
func waitForWrites(ctx context.Context, p *semcachebifrost.Plugin) error {
	deadline := time.Now().Add(time.Minute)
	for {
		c := p.Counters()
		// Кэшируется и промах, и отклонение: новый ответ нужен в кэше в обоих
		// случаях.
		expected := c[semcache.KindMiss] + c[semcache.KindReject]
		settled := c["written"] + c["write_dropped"] + c["error:put"]
		if settled >= expected {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for cache writes to land")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func askOrStream(ctx context.Context, client *core.Bifrost, prompt string, stream bool) (string, *schemas.BifrostError) {
	bctx, cancel := schemas.NewBifrostContextWithTimeout(ctx, 2*time.Minute)
	defer cancel()

	req := &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI,
		Model:    chatModel,
		Input: []schemas.ChatMessage{{
			Role:    schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{ContentStr: &prompt},
		}},
	}
	if !stream {
		resp, bifrostErr := client.ChatCompletionRequest(bctx, req)
		if bifrostErr != nil {
			return "", bifrostErr
		}
		return answerOf(resp), nil
	}

	chunks, bifrostErr := client.ChatCompletionStreamRequest(bctx, req)
	if bifrostErr != nil {
		return "", bifrostErr
	}
	var b strings.Builder
	for chunk := range chunks {
		if chunk == nil {
			continue
		}
		if chunk.BifrostError != nil {
			return "", chunk.BifrostError
		}
		if chunk.BifrostChatResponse == nil {
			continue
		}
		for _, c := range chunk.BifrostChatResponse.Choices {
			if c.ChatStreamResponseChoice == nil || c.ChatStreamResponseChoice.Delta == nil {
				continue
			}
			if content := c.ChatStreamResponseChoice.Delta.Content; content != nil {
				b.WriteString(*content)
			}
		}
	}
	return b.String(), nil
}

func openStore(ctx context.Context, dims int, log *slog.Logger) (store.Store, func(), error) {
	dsn := os.Getenv("SEMCACHE_DSN")
	if dsn == "" {
		log.Warn("SEMCACHE_DSN is not set: entries live in this process only")
		return store.NewMemory(), func() {}, nil
	}
	pg, err := store.OpenPostgres(ctx, dsn, dims, store.PostgresOptions{})
	if err != nil {
		return nil, nil, err
	}
	if err := pg.Migrate(ctx); err != nil {
		pg.Close()
		return nil, nil, err
	}
	return pg, pg.Close, nil
}

// directChat — минимальный клиент chat completions для судьи.
func directChat(apiKey string) func(ctx context.Context, body []byte) ([]byte, error) {
	client := &http.Client{Timeout: time.Minute}
	return func(ctx context.Context, body []byte) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", strings.NewReader(string(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("judge request failed: %s: %s", resp.Status, raw)
		}
		return raw, nil
	}
}

// account — минимальный schemas.Account: один провайдер, один ключ.
type account struct{ apiKey string }

func (a *account) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (a *account) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{
		ID:     "demo",
		Value:  schemas.SecretVar{Val: a.apiKey},
		Models: schemas.WhiteList{chatModel},
		Weight: 1,
	}}, nil
}

func (a *account) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{}, nil
}

func answerOf(resp *schemas.BifrostChatResponse) string {
	if resp == nil {
		return ""
	}
	for _, c := range resp.Choices {
		if c.ChatNonStreamResponseChoice == nil || c.Message == nil || c.Message.Content == nil {
			continue
		}
		if c.Message.Content.ContentStr != nil {
			return *c.Message.Content.ContentStr
		}
	}
	return ""
}

func bifrostErrText(err *schemas.BifrostError) string {
	if err == nil {
		return ""
	}
	if err.Error != nil && err.Error.Message != "" {
		return err.Error.Message
	}
	raw, _ := json.Marshal(err)
	return string(raw)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 72 {
		return s[:72] + "…"
	}
	return s
}
