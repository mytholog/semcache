package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/mytholog/semcache"
)

// Заголовки протокола прокси.
const (
	headerControl = "X-Semcache"       // bypass в запросе; исход в ответе
	headerScore   = "X-Semcache-Score" // косинус кандидата, который дал попадание
	headerTags    = "X-Semcache-Tags"  // от чего зависит ответ: doc:42,tpl:v3
	headerReason  = "X-Semcache-Bypass"
)

// Server — прокси, совместимый с /v1/chat/completions.
type Server struct {
	Cache    *semcache.Cache
	Upstream *Upstream
	Metrics  *Metrics
	Log      *slog.Logger

	// NamespacePrefix отделяет пространства имён одного развёртывания от
	// другого в общей базе.
	NamespacePrefix string

	// MaxBody — предел размера запроса.
	MaxBody int64

	// FailOpen: при ошибке кэша запрос всё равно уходит провайдеру. Кэш —
	// оптимизация, и падать из-за него нельзя. Ошибка при этом остаётся в
	// метриках и логе, а не проглатывается.
	FailOpen bool

	writes   *writeQueue
	inflight singleflight.Group
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /invalidate", s.handleInvalidate)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	return mux
}

// chatRequest — только те поля, которые влияют на решение кэшировать.
// Остальное прокси не разбирает и пересылает как есть.
type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	N        *int   `json:"n"`
	Messages []struct {
		Role string `json:"role"`
		// Content — строка или массив частей у мультимодальных запросов.
		// Второй случай кэш пропускает: ключом должен быть весь вход, а
		// картинку этот ключ не описывает.
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools json.RawMessage `json:"tools"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.Metrics.Inc("semcache_requests_total", "")

	body, err := io.ReadAll(io.LimitReader(r.Body, s.MaxBody))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "read request body", err)
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.fail(w, http.StatusBadRequest, "parse request body", err)
		return
	}

	prompt, reason := cacheKey(&req, r.Header)
	if reason != "" {
		s.Metrics.Inc("semcache_bypassed_total", `reason="`+reason+`"`)
		w.Header().Set(headerReason, reason)
		s.forward(r.Context(), w, r, body, &req, "", nil)
		return
	}

	namespace := s.NamespacePrefix + req.Model
	q := semcache.Query{Prompt: prompt, Namespace: namespace}

	start := time.Now()
	res, err := s.Cache.Get(r.Context(), q)
	s.Metrics.Observe("semcache_lookup_seconds_total", "", time.Since(start))
	if err != nil {
		s.Metrics.Inc("semcache_errors_total", `stage="lookup"`)
		s.Log.Error("cache lookup failed", "error", err, "namespace", namespace)
		if !s.FailOpen {
			s.fail(w, http.StatusBadGateway, "cache lookup", err)
			return
		}
		s.forward(r.Context(), w, r, body, &req, prompt, nil)
		return
	}
	s.Metrics.Inc("semcache_lookups_total", `kind="`+res.Kind+`"`)

	if res.Hit() {
		s.serveHit(w, res, req.Stream)
		return
	}

	// Отклонение сообщается клиенту вместе с косинусом: иначе близкий, но
	// отвергнутый кандидат выглядит как обычный промах, и понять, работает ли
	// вторая стадия, по трафику нельзя.
	w.Header().Set(headerControl, res.Kind)
	if res.Kind == semcache.KindReject {
		w.Header().Set(headerScore, strconv.FormatFloat(res.Score, 'f', 4, 64))
	}
	s.forward(r.Context(), w, r, body, &req, prompt, tagsFrom(r.Header))
}

// cacheKey строит ключ из всего диалога: ответ зависит от системного промпта и
// предыдущих реплик не меньше, чем от последнего вопроса. Второе значение —
// причина, по которой запрос кэшировать нельзя.
func cacheKey(req *chatRequest, h http.Header) (string, string) {
	if strings.EqualFold(h.Get(headerControl), "bypass") {
		return "", "client"
	}
	if req.N != nil && *req.N > 1 {
		// Клиент просит несколько разных ответов — кэш вернул бы один.
		return "", "multiple_choices"
	}
	if len(req.Tools) > 0 {
		// Результат зависит от описания инструментов и от их выполнения;
		// ключом из одного текста это не описывается.
		return "", "tools"
	}
	if len(req.Messages) == 0 {
		return "", "no_messages"
	}

	var b strings.Builder
	for _, m := range req.Messages {
		var content string
		if err := json.Unmarshal(m.Content, &content); err != nil {
			return "", "non_text_content"
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(content)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), ""
}

func tagsFrom(h http.Header) []string {
	raw := h.Get(headerTags)
	if raw == "" {
		return nil
	}
	var tags []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

// serveHit отдаёт закэшированное тело провайдера без изменений: клиент получает
// тот же JSON, что получил бы от провайдера, с теми же полями.
func (s *Server) serveHit(w http.ResponseWriter, res semcache.Result, stream bool) {
	w.Header().Set(headerControl, res.Kind)
	w.Header().Set(headerScore, strconv.FormatFloat(res.Score, 'f', 4, 64))
	if stream {
		s.streamHit(w, res)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, res.Entry.Payload); err != nil {
		s.Log.Warn("write cache hit failed", "error", err)
	}
}

// streamHit пересобирает закэшированный ответ в SSE: клиент, попросивший
// поток, обязан получить поток, иначе кэш просто не работает для большинства
// клиентов.
func (s *Server) streamHit(w http.ResponseWriter, res semcache.Result) {
	var parsed chatResponse
	if err := json.Unmarshal([]byte(res.Entry.Payload), &parsed); err != nil || len(parsed.Choices) == 0 {
		s.Metrics.Inc("semcache_errors_total", `stage="stream_hit"`)
		s.Log.Error("cached payload is not a chat completion", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, res.Entry.Payload)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	chunk := map[string]any{
		"id":      "semcache-" + shortID(res.Entry.ID),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   parsed.Model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]string{"role": "assistant", "content": parsed.Choices[0].Message.Content},
			"finish_reason": "stop",
		}},
	}
	data, err := json.Marshal(chunk)
	if err != nil {
		s.Log.Error("encode stream chunk failed", "error", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// forward отправляет запрос провайдеру и, если ответ можно кэшировать,
// сохраняет его. Пустой prompt означает, что кэшировать нельзя.
func (s *Server) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, req *chatRequest, prompt string, tags []string) {
	resp, shared, err := s.callUpstream(ctx, r, body, req, prompt)
	if err != nil {
		s.fail(w, http.StatusBadGateway, "upstream", err)
		return
	}
	if shared {
		w.Header().Set(headerControl, "coalesced")
	}

	if w.Header().Get(headerControl) == "" {
		w.Header().Set(headerControl, semcache.KindMiss)
	}
	copyHeader(w.Header(), resp.Header, "Content-Type", "X-Request-Id", "OpenAI-Version", "OpenAI-Processing-Ms")
	w.WriteHeader(resp.Status)
	if _, err := w.Write(resp.Body); err != nil {
		s.Log.Warn("write upstream response failed", "error", err)
		return
	}

	if prompt == "" || resp.Status != http.StatusOK || req.Stream {
		// Поток не кэшируется: тело здесь — последовательность SSE-кадров,
		// а не ответ. Попадание в кэш поток отдать умеет, промах — нет.
		if req.Stream && prompt != "" {
			s.Metrics.Inc("semcache_bypassed_total", `reason="stream_miss"`)
		}
		return
	}

	// Запись идёт после ответа клиенту: тот не должен ждать эмбеддинг.
	// Контекст запроса к этому моменту отменён, поэтому берётся отдельный.
	model, body := req.Model, resp.Body
	s.enqueue(func(ctx context.Context) { s.store(ctx, prompt, model, body, tags) })
}

// callUpstream схлопывает одновременные одинаковые промахи в один запрос к
// провайдеру. На холодном кэше без этого стадо одинаковых запросов
// оплачивается целиком, а в кэш всё равно попадёт один ответ.
//
// Второе возвращаемое значение — что ответ разделён с другими запросами.
func (s *Server) callUpstream(ctx context.Context, r *http.Request, body []byte, req *chatRequest, prompt string) (upstreamResponse, bool, error) {
	call := func(callCtx context.Context) (upstreamResponse, error) {
		s.Metrics.Inc("semcache_upstream_requests_total", "")
		start := time.Now()
		resp, err := s.Upstream.Do(callCtx, "/chat/completions", body, r.Header)
		s.Metrics.Observe("semcache_upstream_seconds_total", "", time.Since(start))
		if err != nil {
			s.Metrics.Inc("semcache_upstream_errors_total", "")
		}
		return resp, err
	}

	if prompt == "" {
		// Некэшируемый запрос схлопывать нельзя: клиент просил именно свой
		// ответ, а не чей-то ещё.
		resp, err := call(ctx)
		return resp, false, err
	}

	key := s.NamespacePrefix + req.Model + "\x00" + semcache.Hash(prompt)
	v, err, shared := s.inflight.Do(key, func() (any, error) {
		// Контекст отвязан от запроса ведущего намеренно: ответ пойдёт в кэш
		// и остальным ждущим, поэтому отключившийся клиент не должен
		// отменять работу, за которую уже платят. Дедлайн даёт Upstream.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.Upstream.Client.Timeout)
		defer cancel()
		return call(callCtx)
	})
	if err != nil {
		return upstreamResponse{}, shared, err
	}
	return v.(upstreamResponse), shared, nil
}

func (s *Server) store(ctx context.Context, prompt, model string, payload []byte, tags []string) {
	var parsed chatResponse
	if err := json.Unmarshal(payload, &parsed); err != nil || len(parsed.Choices) == 0 {
		s.Metrics.Inc("semcache_errors_total", `stage="parse_response"`)
		s.Log.Error("upstream response is not a chat completion", "error", err)
		return
	}

	// model:<имя> попадает в теги, чтобы смена версии модели могла разом
	// убрать её ответы; изолирует их namespace, а не тег.
	tags = append(tags, "model:"+model)

	err := s.Cache.Put(ctx, semcache.Write{
		Prompt:    prompt,
		Namespace: s.NamespacePrefix + model,
		Payload:   string(payload),
		Answer:    parsed.Choices[0].Message.Content,
		Tags:      tags,
	})
	if err != nil {
		s.Metrics.Inc("semcache_errors_total", `stage="put"`)
		s.Log.Error("cache put failed", "error", err)
	}
}

type invalidateRequest struct {
	Tags []string `json:"tags"`
}

func (s *Server) handleInvalidate(w http.ResponseWriter, r *http.Request) {
	var req invalidateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "parse request body", err)
		return
	}
	if len(req.Tags) == 0 {
		s.fail(w, http.StatusBadRequest, "invalidate", errors.New("tags must not be empty"))
		return
	}

	removed, err := s.Cache.InvalidateTags(r.Context(), req.Tags)
	if err != nil {
		s.Metrics.Inc("semcache_errors_total", `stage="invalidate"`)
		s.fail(w, http.StatusBadGateway, "invalidate", err)
		return
	}
	s.Metrics.Add("semcache_invalidated_entries_total", "", float64(removed))
	s.Log.Info("invalidated cache entries", "tags", req.Tags, "removed", removed)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"removed": removed})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.Metrics.Write(w); err != nil {
		s.Log.Warn("write metrics failed", "error", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, stage string, err error) {
	s.Log.Error("request failed", "stage", stage, "error", err, "status", status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": stage + ": " + err.Error(),
			"type":    "semcache_error",
		},
	})
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func copyHeader(dst, src http.Header, names ...string) {
	for _, name := range names {
		if v := src.Get(name); v != "" {
			dst.Set(name, v)
		}
	}
}
