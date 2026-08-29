package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	maxBatch    = 128
	maxAttempts = 4
)

// modelSpec описывает, куда слать тексты. Имена из спеки: openai, bge-m3, e5-large.
type modelSpec struct {
	name     string
	kind     string // openai | local
	remote   string
	prefix   string
	pricePer float64
}

var catalog = map[string]modelSpec{
	"text-embedding-3-small": {name: "text-embedding-3-small", kind: "openai", remote: "text-embedding-3-small", pricePer: 0.02},
	"openai":                 {name: "text-embedding-3-small", kind: "openai", remote: "text-embedding-3-small", pricePer: 0.02},
	"bge-m3":                 {name: "bge-m3", kind: "local", remote: "BAAI/bge-m3"},
	"e5-large":               {name: "e5-large", kind: "local", remote: "intfloat/multilingual-e5-large", prefix: "query: "},
}

func resolveModel(name string) (modelSpec, error) {
	if spec, ok := catalog[name]; ok {
		return spec, nil
	}
	return modelSpec{}, fmt.Errorf("unknown model %q (want text-embedding-3-small, bge-m3, e5-large)", name)
}

type batchEmbed func(ctx context.Context, texts []string) (vectors [][]float32, tokens int, err error)

// embedder считает эмбеддинги с кэшем на диске: сетка порогов гоняется много раз,
// а платить за одни и те же векторы повторно смысла нет.
type embedder struct {
	model     string
	dims      int
	cacheFile string
	embed     batchEmbed
	pricePer  float64
	batchSize int

	vectors map[string][]float32
	hits    int
	misses  int
	tokens  int
}

func newCachedEmbedder(spec modelSpec, dims int, cacheDir string, embed batchEmbed) (*embedder, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	name := strings.NewReplacer("/", "_", ":", "_").Replace(spec.name)
	e := &embedder{
		model:     spec.name,
		dims:      dims,
		cacheFile: filepath.Join(cacheDir, fmt.Sprintf("%s-%d.json", name, dims)),
		embed:     embed,
		pricePer:  spec.pricePer,
		batchSize: maxBatch,
		vectors:   make(map[string][]float32),
	}
	if spec.kind == "local" {
		// Один процесс Python грузит модель минутами; резать на 128 нельзя.
		e.batchSize = 100_000
	}
	if err := e.loadCache(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *embedder) key(text string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%s", e.model, e.dims, text)
	return hex.EncodeToString(h.Sum(nil))
}

func (e *embedder) loadCache() error {
	data, err := os.ReadFile(e.cacheFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read embedding cache: %w", err)
	}
	if err := json.Unmarshal(data, &e.vectors); err != nil {
		return fmt.Errorf("parse embedding cache %s: %w", e.cacheFile, err)
	}
	return nil
}

func (e *embedder) saveCache() error {
	data, err := json.Marshal(e.vectors)
	if err != nil {
		return fmt.Errorf("encode embedding cache: %w", err)
	}
	tmp := e.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write embedding cache: %w", err)
	}
	if err := os.Rename(tmp, e.cacheFile); err != nil {
		return fmt.Errorf("replace embedding cache: %w", err)
	}
	return nil
}

func (e *embedder) embedAll(ctx context.Context, all []string) error {
	var missing []string
	queued := make(map[string]struct{})
	for _, text := range all {
		k := e.key(text)
		if _, ok := e.vectors[k]; ok {
			e.hits++
			continue
		}
		if _, ok := queued[k]; ok {
			continue
		}
		queued[k] = struct{}{}
		missing = append(missing, text)
	}
	if len(missing) == 0 {
		slog.Info("all embeddings served from cache", "model", e.model, "texts", len(all))
		return nil
	}

	slog.Info("embedding texts", "model", e.model, "missing", len(missing), "cached", e.hits)
	for batch := range slices.Chunk(missing, e.batchSize) {
		vecs, tokens, err := e.embed(ctx, batch)
		if err != nil {
			if saveErr := e.saveCache(); saveErr != nil {
				slog.Error("failed to save partial embedding cache", "error", saveErr)
			}
			return err
		}
		if len(vecs) != len(batch) {
			return fmt.Errorf("embedder returned %d vectors for %d inputs", len(vecs), len(batch))
		}
		for i, vec := range vecs {
			normalize(vec)
			e.vectors[e.key(batch[i])] = vec
		}
		e.misses += len(batch)
		e.tokens += tokens
	}
	return e.saveCache()
}

func (e *embedder) vector(text string) ([]float32, bool) {
	v, ok := e.vectors[e.key(text)]
	return v, ok
}

func (e *embedder) costUSD() float64 {
	return float64(e.tokens) / 1_000_000 * e.pricePer
}

type embedRequest struct {
	Input          []string `json:"input"`
	Model          string   `json:"model"`
	Dimensions     int      `json:"dimensions,omitempty"`
	EncodingFormat string   `json:"encoding_format"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("embeddings API returned %d: %s", e.status, e.body)
}

func (e *httpError) retryable() bool {
	return e.status == http.StatusTooManyRequests || e.status >= 500
}

type openaiClient struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string
	dims    int
}

func newOpenAI(baseURL, apiKey, model string, dims int, timeout time.Duration) (*openaiClient, error) {
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is not set")
	}
	return &openaiClient{
		http:    &http.Client{Timeout: timeout},
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		dims:    dims,
	}, nil
}

func (c *openaiClient) embed(ctx context.Context, batch []string) ([][]float32, int, error) {
	resp, err := c.post(ctx, batch)
	if err != nil {
		return nil, 0, err
	}
	out := make([][]float32, len(batch))
	for _, item := range resp.Data {
		if item.Index < 0 || item.Index >= len(batch) {
			return nil, 0, fmt.Errorf("embeddings API returned out-of-range index %d for batch of %d", item.Index, len(batch))
		}
		out[item.Index] = item.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, 0, fmt.Errorf("embeddings API missing vector for index %d", i)
		}
	}
	return out, resp.Usage.TotalTokens, nil
}

func (c *openaiClient) post(ctx context.Context, batch []string) (*embedResponse, error) {
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

		var httpErr *httpError
		if errors.As(err, &httpErr) && !httpErr.retryable() {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, errors.Join(err, ctx.Err())
		}
	}
	return nil, fmt.Errorf("embeddings request failed after %d attempts: %w", maxAttempts, lastErr)
}

func (c *openaiClient) do(ctx context.Context, batch []string) (*embedResponse, error) {
	body, err := json.Marshal(embedRequest{
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
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(out.Data) != len(batch) {
		return nil, fmt.Errorf("embeddings API returned %d vectors for %d inputs", len(out.Data), len(batch))
	}
	return &out, nil
}
