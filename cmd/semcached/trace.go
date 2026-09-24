package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Tracer пишет один спан на запрос в OTLP/HTTP JSON.
//
// SDK OpenTelemetry в этот модуль не входит: его граф зависимостей попал бы
// в библиотеку, у которой почти нет зависимостей. Имена атрибутов — gen_ai.*.
// Пустой Endpoint означает «не отправлять»; тесты подставляют Export.
type Tracer struct {
	Endpoint string
	Service  string
	Log      *slog.Logger
	Export   func(ctx context.Context, body []byte)
}

type span struct {
	tracer  *Tracer
	name    string
	traceID string
	spanID  string
	start   time.Time
	attrs   map[string]string
}

func (t *Tracer) start(r *http.Request, name string) *span {
	if t == nil {
		return nil
	}
	sp := &span{
		tracer:  t,
		name:    name,
		traceID: traceIDFrom(r),
		spanID:  randomHex(8),
		start:   time.Now(),
		attrs:   map[string]string{},
	}
	if sp.traceID == "" {
		sp.traceID = randomHex(16)
	}
	return sp
}

func (sp *span) set(key, value string) {
	if sp == nil || value == "" {
		return
	}
	sp.attrs[key] = value
}

func (sp *span) finish(outcome string) {
	if sp == nil || sp.tracer == nil {
		return
	}
	sp.set("semcache.outcome", outcome)
	body, err := json.Marshal(sp.otlp(time.Now()))
	if err != nil {
		return
	}
	if sp.tracer.Export != nil {
		sp.tracer.Export(context.Background(), body)
		return
	}
	if sp.tracer.Endpoint == "" {
		return
	}
	endpoint, log := sp.tracer.Endpoint, sp.tracer.Log
	go postTrace(endpoint, log, body)
}

func postTrace(endpoint string, log *slog.Logger, body []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if log != nil {
			log.Warn("export span failed", "error", err)
		}
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 && log != nil {
		log.Warn("export span failed", "status", resp.StatusCode)
	}
}

func (sp *span) otlp(end time.Time) map[string]any {
	attrs := make([]any, 0, len(sp.attrs)+1)
	attrs = append(attrs, attr("gen_ai.operation.name", "chat"))
	for k, v := range sp.attrs {
		attrs = append(attrs, attr(k, v))
	}
	service := sp.tracer.Service
	if service == "" {
		service = "semcached"
	}
	return map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{attr("service.name", service)},
			},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "semcached"},
				"spans": []any{map[string]any{
					"traceId":           sp.traceID,
					"spanId":            sp.spanID,
					"name":              sp.name,
					"kind":              2,
					"startTimeUnixNano": uint64(sp.start.UnixNano()),
					"endTimeUnixNano":   uint64(end.UnixNano()),
					"attributes":        attrs,
				}},
			}},
		}},
	}
}

func attr(key, value string) map[string]any {
	return map[string]any{
		"key": key,
		"value": map[string]any{
			"stringValue": value,
		},
	}
}

func traceIDFrom(r *http.Request) string {
	if r == nil {
		return ""
	}
	h := r.Header.Get("traceparent")
	parts := strings.Split(h, "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	return parts[1]
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
