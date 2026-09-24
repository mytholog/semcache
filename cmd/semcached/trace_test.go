package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSpanCarriesGenAIAttributes(t *testing.T) {
	var body []byte
	tr := &Tracer{Export: func(context.Context, []byte) {}}
	tr.Export = func(_ context.Context, b []byte) { body = b }
	req, err := http.NewRequest(http.MethodPost, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	sp := tr.start(req, "chat")
	sp.set("gen_ai.request.model", "gpt-4o-mini")
	sp.finish("exact")

	if !strings.Contains(string(body), `"gen_ai.operation.name"`) ||
		!strings.Contains(string(body), `"gen_ai.request.model"`) ||
		!strings.Contains(string(body), `"semcache.outcome"`) ||
		!strings.Contains(string(body), "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("span body:\n%s", body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestNilTracerIsSafe(t *testing.T) {
	var tr *Tracer
	sp := tr.start(nil, "chat")
	sp.set("gen_ai.request.model", "gpt-4o-mini")
	sp.finish("miss")
}
