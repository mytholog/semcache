// Package embed содержит эмбеддеры, реализующие semcache.Embedder.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultBatch — предел OpenAI на число входов в одном запросе.
	DefaultBatch = 128

	maxAttempts  = 4
	maxBodyBytes = 512
)

// OpenAI — клиент к любому API, совместимому с /v1/embeddings.
type OpenAI struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string
	dims    int
	batch   int
}

// OpenAIConfig — параметры клиента. Обязателен только APIKey.
type OpenAIConfig struct {
	APIKey string

	// BaseURL по умолчанию https://api.openai.com/v1. Здесь же указывается
	// локальный совместимый сервер.
	BaseURL string

	// Model по умолчанию text-embedding-3-small.
	Model string

	// Dims запрашивает укороченный вектор. Ноль — размерность модели.
	Dims int

	// Batch — сколько текстов уходит в одном запросе. Ноль — DefaultBatch.
	Batch int

	Timeout time.Duration
}

// Usage — потраченные токены, чтобы стоимость можно было посчитать.
type Usage struct {
	TotalTokens int
}

// ErrNoAPIKey возвращается, если ключ не задан.
var ErrNoAPIKey = errors.New("embed: API key is required")

func NewOpenAI(cfg OpenAIConfig) (*OpenAI, error) {
	if cfg.APIKey == "" {
		return nil, ErrNoAPIKey
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "text-embedding-3-small"
	}
	if cfg.Batch <= 0 {
		cfg.Batch = DefaultBatch
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	return &OpenAI{
		http:    &http.Client{Timeout: cfg.Timeout},
		baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		dims:    cfg.Dims,
		batch:   cfg.Batch,
	}, nil
}

// Model — имя модели: им имеет смысл разделять пространства имён кэша, потому
// что векторы разных моделей несравнимы.
func (c *OpenAI) Model() string { return c.model }

// Embed реализует semcache.Embedder.
func (c *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, _, err := c.EmbedWithUsage(ctx, texts)
	return vecs, err
}

// EmbedWithUsage дополнительно возвращает потраченные токены.
func (c *OpenAI) EmbedWithUsage(ctx context.Context, texts []string) ([][]float32, Usage, error) {
	if len(texts) == 0 {
		return nil, Usage{}, nil
	}

	out := make([][]float32, 0, len(texts))
	var usage Usage
	for chunk := range slices.Chunk(texts, c.batch) {
		resp, err := c.post(ctx, chunk)
		if err != nil {
			return nil, usage, err
		}

		vecs := make([][]float32, len(chunk))
		for _, item := range resp.Data {
			if item.Index < 0 || item.Index >= len(chunk) {
				return nil, usage, fmt.Errorf("embeddings API returned out-of-range index %d for batch of %d", item.Index, len(chunk))
			}
			// Векторы нормируются: тогда косинус — это скалярное
			// произведение, и store может считать его как угодно.
			Normalize(item.Embedding)
			vecs[item.Index] = item.Embedding
		}
		for i, v := range vecs {
			if v == nil {
				return nil, usage, fmt.Errorf("embeddings API missing vector for index %d", i)
			}
		}
		out = append(out, vecs...)
		usage.TotalTokens += resp.Usage.TotalTokens
	}
	return out, usage, nil
}

// Normalize приводит вектор к единичной длине на месте.
func Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
}

type request struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// HTTPError — ответ API с кодом, отличным от 200.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("embeddings API returned %d: %s", e.Status, e.Body)
}

// Retryable отделяет перегрузку от ошибки в запросе: повторять стоит только
// первое.
func (e *HTTPError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func (c *OpenAI) post(ctx context.Context, batch []string) (*response, error) {
	var lastErr error
	for attempt := range maxAttempts {
		if attempt > 0 {
			delay := time.Duration(1<<attempt) * time.Second
			slog.Warn("retrying embeddings request", "attempt", attempt+1, "delay", delay, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := c.do(ctx, batch)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		var httpErr *HTTPError
		if errors.As(err, &httpErr) && !httpErr.Retryable() {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, errors.Join(err, ctx.Err())
		}
	}
	return nil, fmt.Errorf("embeddings request failed after %d attempts: %w", maxAttempts, lastErr)
}

func (c *OpenAI) do(ctx context.Context, batch []string) (*response, error) {
	body, err := json.Marshal(request{
		Input:          batch,
		Model:          c.model,
		Dimensions:     c.dims,
		EncodingFormat: "float",
	})
	if err != nil {
		return nil, fmt.Errorf("encode embeddings request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call embeddings API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		return nil, &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(out.Data) != len(batch) {
		return nil, fmt.Errorf("embeddings API returned %d vectors for %d inputs", len(out.Data), len(batch))
	}
	return &out, nil
}
