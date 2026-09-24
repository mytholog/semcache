package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Completion — ответ судьи и его счётчик токенов.
// Prompt и Completion раздельно, потому что у моделей разная цена входа и выхода.
// Если API отдал только total, всё попадает в Completion, а Prompt остаётся нулём.
type Completion struct {
	Text       string
	Prompt     int
	Completion int
}

// Tokens — полный счёт, по которому сравниваются прогоны.
func (c Completion) Tokens() int { return c.Prompt + c.Completion }

// Completer — узкий порт к LLM, чтобы тесты не ходили в сеть.
type Completer interface {
	Complete(ctx context.Context, system, user string) (Completion, error)
}

type judgeCacheFile struct {
	OK               bool    `json:"ok"`
	Score            float64 `json:"score"`
	Reason           string  `json:"reason"`
	Tokens           int     `json:"tokens"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
}

// Judge — LLM-верификатор с дисковым кэшем точных пар.
type Judge struct {
	Complete Completer
	CacheDir string
	PricePer float64 // USD за миллион токенов, когда разбивки на вход/выход нет

	// InputPerM и OutputPerM — USD за миллион входных и выходных токенов.
	// Нулевые значения оставляют плоскую оценку PricePer.
	InputPerM  float64
	OutputPerM float64

	// MaxMemo — предел памяти решений. Ноль означает «без предела», что
	// годится для прогона бенча и не годится для сервера.
	MaxMemo int

	mu    sync.Mutex
	mem   map[string]judged
	group singleflight.Group

	// Calls и SpentTokens — что реально ушло в API в этом прогоне.
	// Tokens — сколько стоил бы холодный прогон: только эта величина
	// не зависит от состояния кэша, поэтому cost model считается по ней.
	// PromptTokens и CompletionTokens — та же сумма, разложенная по цене.
	Calls            int
	CacheHits        int
	Tokens           int
	PromptTokens     int
	CompletionTokens int
	SpentTokens      int
}

// judged — решение вместе с ценой его получения.
type judged struct {
	decision   Decision
	tokens     int
	prompt     int
	completion int
}

// DefaultMaxMemo ограничивает память решений: в долгоживущем процессе карта
// без предела — это утечка.
const DefaultMaxMemo = 10_000

func NewJudge(c Completer, cacheDir string) *Judge {
	return &Judge{
		Complete: c,
		CacheDir: cacheDir,
		PricePer: 0.20,
		MaxMemo:  DefaultMaxMemo,
		mem:      make(map[string]judged),
	}
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
		j.note(rec)
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
	j.note(rec)
	j.mu.Unlock()
	return rec.decision, true, nil
}

// note учитывает решение из кэша в холодной стоимости. Вызывать под j.mu.
func (j *Judge) note(rec judged) {
	j.CacheHits++
	j.Tokens += rec.tokens
	j.PromptTokens += rec.prompt
	j.CompletionTokens += rec.completion
}

func (j *Judge) complete(ctx context.Context, key, incoming, cached string) (Decision, error) {
	user := "A: " + incoming + "\nB: " + cached
	got, err := j.Complete.Complete(ctx, judgeRubric, user)
	if err != nil {
		return Decision{}, fmt.Errorf("judge complete: %w", err)
	}

	var parsed struct {
		Interchangeable bool   `json:"interchangeable"`
		Reason          string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(got.Text), &parsed); err != nil {
		return Decision{}, fmt.Errorf("judge json: %w", err)
	}
	d := Decision{OK: parsed.Interchangeable, Reason: parsed.Reason}
	if d.OK {
		d.Score = 1
	}
	tokens := got.Tokens()

	j.mu.Lock()
	j.Calls++
	j.Tokens += tokens
	j.PromptTokens += got.Prompt
	j.CompletionTokens += got.Completion
	j.SpentTokens += tokens
	// Сброс целиком вместо вытеснения по возрасту: память экономит только
	// повторные пары в пределах короткого окна, для этого точность
	// вытеснения не нужна, а дисковый кэш остаётся на месте.
	if j.MaxMemo > 0 && len(j.mem) >= j.MaxMemo {
		j.mem = make(map[string]judged, j.MaxMemo)
	}
	rec := judged{decision: d, tokens: tokens, prompt: got.Prompt, completion: got.Completion}
	j.mem[key] = rec
	j.mu.Unlock()

	if err := j.save(key, rec); err != nil {
		return d, err
	}
	return d, nil
}

// USD — стоимость холодного прогона: считается по Tokens, а не по фактически
// потраченным, иначе повторный запуск покажет бесплатную верификацию.
func (j *Judge) USD() float64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Разбивка есть только у свежих записей. Старый кэш хранит один total,
	// и для него остаётся плоская PricePer — иначе повтор августа 2026
	// пересчитает уже опубликованные 5.3%.
	if (j.InputPerM > 0 || j.OutputPerM > 0) && j.PromptTokens+j.CompletionTokens > 0 {
		return float64(j.PromptTokens)/1_000_000*j.InputPerM +
			float64(j.CompletionTokens)/1_000_000*j.OutputPerM
	}
	return float64(j.Tokens) / 1_000_000 * j.PricePer
}

// ModelPrices — цены OpenAI на 2026-09-24, USD за миллион токенов.
// Неизвестная модель оставляет плоскую оценку PricePer.
func ModelPrices(model string) (inputPerM, outputPerM float64, ok bool) {
	switch model {
	case "gpt-4o-mini":
		return 0.15, 0.60, true
	case "gpt-5-nano":
		return 0.05, 0.40, true
	case "gpt-5-mini":
		return 0.25, 2.00, true
	default:
		return 0, 0, false
	}
}

// JudgeCacheDir кладёт решения каждой модели в свой каталог.
// gpt-4o-mini остаётся в историческом judge/: там уже лежит прогон,
// на котором посчитаны числа в README.
func JudgeCacheDir(root, model string) string {
	dir := filepath.Join(root, "judge")
	if model == "" || model == "gpt-4o-mini" {
		return dir
	}
	return filepath.Join(dir, strings.ReplaceAll(model, "/", "_"))
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
		decision:   Decision{OK: rec.OK, Score: rec.Score, Reason: rec.Reason},
		tokens:     rec.Tokens,
		prompt:     rec.PromptTokens,
		completion: rec.CompletionTokens,
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
		OK:               rec.decision.OK,
		Score:            rec.decision.Score,
		Reason:           rec.decision.Reason,
		Tokens:           rec.tokens,
		PromptTokens:     rec.prompt,
		CompletionTokens: rec.completion,
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

func (c OpenAICompleter) Complete(ctx context.Context, system, user string) (Completion, error) {
	req := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"response_format": map[string]string{"type": "json_object"},
	}
	// gpt-5 принимает только temperature=1 и молча тратит бюджет на рассуждение.
	// Для классификатора это лишние выходные токены: minimal убирает их,
	// а поле temperature не отправляем — ноль API отвергает.
	if strings.HasPrefix(c.Model, "gpt-5") {
		req["reasoning_effort"] = "minimal"
	} else {
		req["temperature"] = 0
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return Completion{}, fmt.Errorf("encode judge request: %w", err)
	}
	raw, err := c.Do(ctx, payload)
	if err != nil {
		return Completion{}, err
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Completion{}, fmt.Errorf("decode judge response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return Completion{}, fmt.Errorf("judge returned no choices")
	}
	prompt, completion := resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	if prompt+completion == 0 {
		completion = resp.Usage.TotalTokens
	}
	return Completion{
		Text:       resp.Choices[0].Message.Content,
		Prompt:     prompt,
		Completion: completion,
	}, nil
}
