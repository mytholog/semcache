package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mytholog/semcache/embed"
)

const maxBatch = embed.DefaultBatch

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

// openAIEmbed — адаптер публичного клиента к batchEmbed: бенчу нужны ещё и
// токены, чтобы печатать стоимость холодного прогона.
func openAIEmbed(baseURL, apiKey, model string, dims int, timeout time.Duration) (batchEmbed, error) {
	client, err := embed.NewOpenAI(embed.OpenAIConfig{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Model:   model,
		Dims:    dims,
		Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, texts []string) ([][]float32, int, error) {
		vecs, usage, err := client.EmbedWithUsage(ctx, texts)
		return vecs, usage.TotalTokens, err
	}, nil
}
