// Package bifrost подключает двухстадийный кэш semcache к гейтвею Bifrost как
// schemas.LLMPlugin.
//
// Ставится он так же, как собственный semantic_cache Bifrost:
//
//	plugin, err := bifrost.New(bifrost.Config{Cache: cache})
//	client, err := core.Init(ctx, schemas.BifrostConfig{
//	    Account:    account,
//	    LLMPlugins: []schemas.LLMPlugin{plugin},
//	})
//
// Разница с semantic_cache — в том, что решает попадание. Там это порог
// косинуса (по умолчанию 0.8), здесь — верификатор взаимозаменяемости, потому
// что на размеченном наборе порог 0.8 отдаёт 66% неверных ответов: у вопроса с
// противоположным смыслом косинус выше, чем у законной перефразировки.
package bifrost

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"golang.org/x/sync/semaphore"

	"github.com/mytholog/semcache"
)

// Config — что нужно плагину. Обязателен только кэш: всё остальное имеет
// осмысленное нулевое значение.
type Config struct {
	// Cache — уже собранный кэш: store, эмбеддер, верификатор, языковой гейт.
	// Плагин ничего из этого не выбирает за вызывающего.
	Cache *semcache.Cache

	// NamespacePrefix отделяет пространства имён одного развёртывания от
	// другого в общей базе. К нему всегда дописываются провайдер и модель.
	NamespacePrefix string

	// Tags добавляются к каждой записи. Сюда идёт то, от чего зависят все
	// ответы этого развёртывания: версия шаблона, версия корпуса.
	Tags []string

	// FailClosed превращает ошибку кэша в ошибку запроса. По умолчанию плагин
	// открывается: кэш — оптимизация, и падать из-за него нельзя.
	FailClosed bool

	// MaxPendingWrites — сколько записей может идти одновременно. Ноль
	// означает 4. Запись стоит обращения к эмбеддеру, и делать её внутри хука
	// значит добавить это время к каждому промаху.
	MaxPendingWrites int64

	// WriteTimeout — дедлайн одной записи. Ноль означает 30 секунд.
	WriteTimeout time.Duration

	// Log получает ошибки кэша. Ноль означает slog.Default().
	Log *slog.Logger
}

// ErrNoCache возвращается, если плагин создан без кэша.
var ErrNoCache = errors.New("semcache/bifrost: Cache is required")

// Plugin реализует schemas.LLMPlugin.
type Plugin struct {
	cache      *semcache.Cache
	prefix     string
	tags       []string
	failClosed bool
	timeout    time.Duration
	log        *slog.Logger

	writes  *semaphore.Weighted
	pending sync.WaitGroup

	mu      sync.Mutex
	counter map[string]int
}

// Ключи контекста приватны: PostLLMHook получает ответ и ошибку, но не запрос,
// поэтому ключ и пространство имён, посчитанные на входе, кладутся в контекст.
// Он для этого и сделан изменяемым.
type contextKey string

const (
	keyPrompt    contextKey = "semcache.prompt"
	keyNamespace contextKey = "semcache.namespace"
)

// Виды исхода в счётчиках: те же, что у кэша, плюс bypass и ошибки.
const (
	counterBypass = "bypass"
	counterError  = "error"
)

// New собирает плагин.
func New(cfg Config) (*Plugin, error) {
	if cfg.Cache == nil {
		return nil, ErrNoCache
	}
	writes := cfg.MaxPendingWrites
	if writes <= 0 {
		writes = 4
	}
	timeout := cfg.WriteTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Plugin{
		cache:      cfg.Cache,
		prefix:     cfg.NamespacePrefix,
		tags:       cfg.Tags,
		failClosed: cfg.FailClosed,
		timeout:    timeout,
		log:        log,
		writes:     semaphore.NewWeighted(writes),
		counter:    make(map[string]int),
	}, nil
}

// GetName возвращает имя плагина.
func (p *Plugin) GetName() string { return "semcache" }

// PreRequestHook — фаза маршрутизации. Кэш в ней не участвует: он не выбирает
// ни провайдера, ни модель.
func (p *Plugin) PreRequestHook(*schemas.BifrostContext, *schemas.BifrostRequest) error {
	return nil
}

// PreLLMHook ищет взаимозаменяемый ответ и при попадании закорачивает вызов
// провайдера.
func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if req == nil || req.ChatRequest == nil {
		// Плагин занимается только чатом: у эмбеддингов и речи нет промпта,
		// взаимозаменяемость которого можно проверить.
		return req, nil, nil
	}

	streaming := req.RequestType == schemas.ChatCompletionStreamRequest
	prompt, reason := cacheKey(req.ChatRequest, streaming)
	if reason != "" {
		p.count(counterBypass + ":" + reason)
		return req, nil, nil
	}

	namespace := namespaceOf(p.prefix, req.ChatRequest.Provider, req.ChatRequest.Model)
	res, err := p.cache.Get(ctx, semcache.Query{Prompt: prompt, Namespace: namespace})
	if err != nil {
		p.count(counterError + ":lookup")
		p.log.Error("semcache lookup failed", "error", err, "namespace", namespace)
		if p.failClosed {
			return req, &schemas.LLMPluginShortCircuit{
				Error: &schemas.BifrostError{Error: &schemas.ErrorField{Message: "semcache lookup: " + err.Error()}},
			}, nil
		}
		return req, nil, nil
	}
	p.count(res.Kind)

	if !res.Hit() {
		// Промах и отклонение равно уходят провайдеру, но различаются в
		// счётчиках: без этого не видно, работает ли вторая стадия.
		ctx.SetValue(keyPrompt, prompt)
		ctx.SetValue(keyNamespace, namespace)
		return req, nil, nil
	}

	resp, err := decodePayload(res.Entry.Payload)
	if err != nil {
		// Запись есть, но прочесть её нельзя: это ошибка кэша, а не ответ.
		p.count(counterError + ":decode")
		p.log.Error("semcache payload is not a chat response", "error", err, "namespace", namespace)
		ctx.SetValue(keyPrompt, prompt)
		ctx.SetValue(keyNamespace, namespace)
		return req, nil, nil
	}

	ctx.Log(schemas.LogLevelDebug, "semcache "+res.Kind+" score="+strconv.FormatFloat(res.Score, 'f', 4, 64))
	return req, &schemas.LLMPluginShortCircuit{Response: &schemas.BifrostResponse{ChatResponse: resp}}, nil
}

// PostLLMHook кладёт ответ провайдера в кэш. Запись идёт вне хука: она стоит
// обращения к эмбеддеру, а клиент ждёт ответ.
func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if bifrostErr != nil || resp == nil || resp.ChatResponse == nil {
		// Ошибку провайдера кэшировать нельзя: она пройдёт, а запись останется.
		return resp, bifrostErr, nil
	}
	prompt, _ := ctx.Value(keyPrompt).(string)
	namespace, _ := ctx.Value(keyNamespace).(string)
	if prompt == "" || namespace == "" {
		// Либо запрос не кэшируется, либо это попадание, которое мы же и отдали.
		return resp, bifrostErr, nil
	}

	payload, err := encodePayload(resp.ChatResponse)
	if err != nil {
		p.count(counterError + ":encode")
		p.log.Error("semcache encode failed", "error", err, "namespace", namespace)
		return resp, bifrostErr, nil
	}

	w := semcache.Write{
		Prompt:    prompt,
		Namespace: namespace,
		Payload:   payload,
		Answer:    answerText(resp.ChatResponse),
		// model:<имя> в тегах позволяет разом убрать ответы модели при смене
		// её версии; изолирует их namespace, а не тег.
		Tags: append(append([]string(nil), p.tags...), "model:"+resp.ChatResponse.Model),
	}
	p.enqueue(w)
	return resp, bifrostErr, nil
}

// Cleanup дожидается начатых записей: ответ, за который уже заплатили, не
// должен потеряться при остановке гейтвея.
func (p *Plugin) Cleanup() error {
	p.pending.Wait()
	return nil
}

// Counters отдаёт копию счётчиков: исходы по видам, отказы по причинам,
// ошибки по стадиям. Отдельного пути метрик у плагина нет — гейтвей уже
// экспортирует свои, а кэш здесь гость.
func (p *Plugin) Counters() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.counter))
	for k, v := range p.counter {
		out[k] = v
	}
	return out
}

// enqueue пишет в кэш вне хука и с ограничением параллелизма. Переполнение
// теряет запись: потерянная запись — это будущий промах, а заблокированный хук
// — это задержка на каждом промахе.
func (p *Plugin) enqueue(w semcache.Write) {
	if !p.writes.TryAcquire(1) {
		p.count("write_dropped")
		return
	}
	p.pending.Add(1)
	go func() {
		defer p.pending.Done()
		defer p.writes.Release(1)

		// Контекст свой: запрос к этому моменту уже завершён, а запись нужна.
		ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
		defer cancel()
		if err := p.cache.Put(ctx, w); err != nil {
			p.count(counterError + ":put")
			p.log.Error("semcache put failed", "error", err, "namespace", w.Namespace)
			return
		}
		p.count("written")
	}()
}

func (p *Plugin) count(key string) {
	p.mu.Lock()
	p.counter[key]++
	p.mu.Unlock()
}

// Плагин обязан удовлетворять интерфейсу гейтвея во время компиляции, а не при
// первом запросе.
var _ schemas.LLMPlugin = (*Plugin)(nil)
