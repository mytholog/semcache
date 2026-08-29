package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type rerankRequest struct {
	Model string      `json:"model"`
	Pairs [][2]string `json:"pairs"`
}

type rerankResponse struct {
	Scores []float64 `json:"scores"`
}

// ExecFunc подменяет вызов sidecar — тесты не поднимают Python.
type ExecFunc func(ctx context.Context, stdin []byte) ([]byte, error)

// CrossEncoder зовёт Python-sidecar со sentence-transformers CrossEncoder.
// Оценки пар кэшируются в памяти и на диске: модель грузится минуты, а один
// и тот же промпт приходит в кэш многократно.
type CrossEncoder struct {
	model    string
	script   string
	workDir  string
	cacheDir string
	timeout  time.Duration
	run      ExecFunc

	mu     sync.Mutex
	scores map[string]float64
	loaded bool

	Calls     int
	CacheHits int
}

// CrossEncoderOptions собирает необязательные параметры: сам по себе нужен
// только идентификатор модели.
type CrossEncoderOptions struct {
	Script   string
	WorkDir  string
	CacheDir string
	Timeout  time.Duration
	Exec     ExecFunc
}

func NewCrossEncoder(model string, opts CrossEncoderOptions) *CrossEncoder {
	script := opts.Script
	if script == "" {
		script = "bench/rerank.py"
	}
	return &CrossEncoder{
		model:    model,
		script:   script,
		workDir:  opts.WorkDir,
		cacheDir: opts.CacheDir,
		timeout:  opts.Timeout,
		run:      opts.Exec,
		scores:   make(map[string]float64),
	}
}

func (c *CrossEncoder) Model() string { return c.model }

func (c *CrossEncoder) ScorePairs(ctx context.Context, pairs [][2]string) ([]float64, error) {
	if len(pairs) == 0 {
		return nil, nil
	}

	c.mu.Lock()
	if !c.loaded {
		if err := c.loadCacheLocked(); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.loaded = true
	}
	out := make([]float64, len(pairs))
	missing := make([]int, 0, len(pairs))
	for i, p := range pairs {
		if s, ok := c.scores[pairKey(c.model, p[0], p[1])]; ok {
			out[i] = s
			c.CacheHits++
			continue
		}
		missing = append(missing, i)
	}
	c.mu.Unlock()

	if len(missing) == 0 {
		return out, nil
	}

	need := make([][2]string, len(missing))
	for j, i := range missing {
		need[j] = pairs[i]
	}
	fresh, err := c.scoreUncached(ctx, need)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for j, i := range missing {
		out[i] = fresh[j]
		c.scores[pairKey(c.model, pairs[i][0], pairs[i][1])] = fresh[j]
	}
	c.Calls += len(missing)
	if err := c.saveCacheLocked(); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *CrossEncoder) scoreUncached(ctx context.Context, pairs [][2]string) ([]float64, error) {
	body, err := json.Marshal(rerankRequest{Model: c.model, Pairs: pairs})
	if err != nil {
		return nil, fmt.Errorf("encode rerank request: %w", err)
	}

	run := c.run
	if run == nil {
		run = c.execSidecar
	}
	raw, err := run(ctx, body)
	if err != nil {
		return nil, err
	}

	var out rerankResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode rerank output: %w", err)
	}
	if len(out.Scores) != len(pairs) {
		return nil, fmt.Errorf("reranker returned %d scores for %d pairs", len(out.Scores), len(pairs))
	}
	return out.Scores, nil
}

func (c *CrossEncoder) execSidecar(ctx context.Context, stdin []byte) ([]byte, error) {
	cmdCtx := ctx
	var cancel context.CancelFunc
	if c.timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, "uv", "run", "--project", "bench", "python", c.script)
	cmd.Dir = c.workDir
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("cross-encoder %s: %w\n%s", c.model, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func (c *CrossEncoder) loadCacheLocked() error {
	path := c.cachePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read rerank cache: %w", err)
	}
	var stored map[string]float64
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse rerank cache: %w", err)
	}
	for k, v := range stored {
		c.scores[k] = v
	}
	return nil
}

func (c *CrossEncoder) saveCacheLocked() error {
	path := c.cachePath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create rerank cache: %w", err)
	}
	data, err := json.Marshal(c.scores)
	if err != nil {
		return fmt.Errorf("encode rerank cache: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write rerank cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace rerank cache: %w", err)
	}
	return nil
}

func (c *CrossEncoder) cachePath() string {
	if c.cacheDir == "" || c.model == "" {
		return ""
	}
	h := sha256.Sum256([]byte(c.model))
	return filepath.Join(c.cacheDir, "rerank-"+hex.EncodeToString(h[:8])+".json")
}

func pairKey(model, a, b string) string {
	h := sha256.Sum256([]byte(model + "\x00" + a + "\x00" + b))
	return hex.EncodeToString(h[:])
}

// ScriptExists проверяет sidecar до того, как харнесс потратит время на эмбеддинги.
func ScriptExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("rerank script: %w", err)
	}
	return nil
}
