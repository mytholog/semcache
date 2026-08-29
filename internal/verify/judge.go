package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sync/singleflight"
)

const judgeRubric = `You decide whether a semantic cache may reuse an answer.

The cache stored a response written for prompt B. A new user sent prompt A.
Reply interchangeable=true only if that stored response is a correct, complete
answer to A: same language, same entities, same numbers, same polarity, same
time window, same account/plan scope.

If A and B differ by negation, a swapped entity, a number, a version/date, or
the audience the rule applies to, answer false.
If they are paraphrases or differ only in politeness/formatting, answer true.

Return JSON only: {"interchangeable": true|false, "reason": "short English"}`

// Completer — узкий порт к LLM, чтобы тесты не ходили в сеть.
type Completer interface {
	Complete(ctx context.Context, system, user string) (text string, tokens int, err error)
}

type judgeCacheFile struct {
	OK     bool    `json:"ok"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
	Tokens int     `json:"tokens"`
}

// Judge — LLM-верификатор с дисковым кэшем точных пар.
type Judge struct {
	Complete Completer
	CacheDir string
	PricePer float64 // USD за миллион токенов, грубая оценка

	mu    sync.Mutex
	mem   map[string]judged
	group singleflight.Group

	// Calls и SpentTokens — что реально ушло в API в этом прогоне.
	// Tokens — сколько стоил бы холодный прогон: только эта величина
	// не зависит от состояния кэша, поэтому cost model считается по ней.
	Calls       int
	CacheHits   int
	Tokens      int
	SpentTokens int
}

// judged — решение вместе с ценой его получения.
type judged struct {
	decision Decision
	tokens   int
}

func NewJudge(c Completer, cacheDir string) *Judge {
	return &Judge{Complete: c, CacheDir: cacheDir, PricePer: 0.20, mem: make(map[string]judged)}
}

func (j *Judge) Interchangeable(ctx context.Context, incoming, cached string) (Decision, error) {
	key := judgeKey(incoming, cached)
	if d, ok, err := j.lookup(key); err != nil {
		return Decision{}, err
	} else if ok {
		return d, nil
	}

	v, err, _ := j.group.Do(key, func() (any, error) {
		if d, ok, err := j.lookup(key); err != nil {
			return nil, err
		} else if ok {
			return d, nil
		}
		return j.complete(ctx, key, incoming, cached)
	})
	if err != nil {
		return Decision{}, err
	}
	return v.(Decision), nil
}

func (j *Judge) lookup(key string) (Decision, bool, error) {
	j.mu.Lock()
	if rec, ok := j.mem[key]; ok {
		j.CacheHits++
		j.Tokens += rec.tokens
		j.mu.Unlock()
		return rec.decision, true, nil
	}
	j.mu.Unlock()

	rec, ok, err := j.load(key)
	if err != nil || !ok {
		return Decision{}, false, err
	}
	j.mu.Lock()
	j.mem[key] = rec
	j.CacheHits++
	j.Tokens += rec.tokens
	j.mu.Unlock()
	return rec.decision, true, nil
}

func (j *Judge) complete(ctx context.Context, key, incoming, cached string) (Decision, error) {
	user := "A: " + incoming + "\nB: " + cached
	text, tokens, err := j.Complete.Complete(ctx, judgeRubric, user)
	if err != nil {
		return Decision{}, fmt.Errorf("judge complete: %w", err)
	}

	var parsed struct {
		Interchangeable bool   `json:"interchangeable"`
		Reason          string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return Decision{}, fmt.Errorf("judge json: %w", err)
	}
	d := Decision{OK: parsed.Interchangeable, Reason: parsed.Reason}
	if d.OK {
		d.Score = 1
	}

	j.mu.Lock()
	j.Calls++
	j.Tokens += tokens
	j.SpentTokens += tokens
	j.mem[key] = judged{decision: d, tokens: tokens}
	j.mu.Unlock()

	if err := j.save(key, judged{decision: d, tokens: tokens}); err != nil {
		return d, err
	}
	return d, nil
}

// USD — стоимость холодного прогона: считается по Tokens, а не по фактически
// потраченным, иначе повторный запуск покажет бесплатную верификацию.
func (j *Judge) USD() float64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return float64(j.Tokens) / 1_000_000 * j.PricePer
}

func judgeKey(a, b string) string {
	h := sha256.Sum256([]byte(a + "\x00" + b))
	return hex.EncodeToString(h[:])
}

func (j *Judge) load(key string) (judged, bool, error) {
	if j.CacheDir == "" {
		return judged{}, false, nil
	}
	data, err := os.ReadFile(filepath.Join(j.CacheDir, key+".json"))
	if os.IsNotExist(err) {
		return judged{}, false, nil
	}
	if err != nil {
		return judged{}, false, fmt.Errorf("read judge cache: %w", err)
	}
	var rec judgeCacheFile
	if err := json.Unmarshal(data, &rec); err != nil {
		return judged{}, false, fmt.Errorf("parse judge cache: %w", err)
	}
	return judged{
		decision: Decision{OK: rec.OK, Score: rec.Score, Reason: rec.Reason},
		tokens:   rec.Tokens,
	}, true, nil
}

func (j *Judge) save(key string, rec judged) error {
	if j.CacheDir == "" {
		return nil
	}
	if err := os.MkdirAll(j.CacheDir, 0o755); err != nil {
		return fmt.Errorf("create judge cache: %w", err)
	}
	data, err := json.Marshal(judgeCacheFile{
		OK:     rec.decision.OK,
		Score:  rec.decision.Score,
		Reason: rec.decision.Reason,
		Tokens: rec.tokens,
	})
	if err != nil {
		return fmt.Errorf("encode judge cache: %w", err)
	}
	tmp := filepath.Join(j.CacheDir, key+".json.tmp")
	path := filepath.Join(j.CacheDir, key+".json")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write judge cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace judge cache: %w", err)
	}
	return nil
}

// OpenAICompleter дергает chat completions с JSON-ответом.
type OpenAICompleter struct {
	Do      func(ctx context.Context, body []byte) ([]byte, error)
	Model   string
	BaseURL string
}

func (c OpenAICompleter) Complete(ctx context.Context, system, user string) (string, int, error) {
	req := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return "", 0, fmt.Errorf("encode judge request: %w", err)
	}
	raw, err := c.Do(ctx, payload)
	if err != nil {
		return "", 0, err
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", 0, fmt.Errorf("decode judge response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", 0, fmt.Errorf("judge returned no choices")
	}
	return resp.Choices[0].Message.Content, resp.Usage.TotalTokens, nil
}
