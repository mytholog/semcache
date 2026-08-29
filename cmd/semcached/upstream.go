package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Upstream пересылает запросы провайдеру. Тело не трогается: клиент должен
// получить ровно то, что ответил провайдер, включая поля, о которых прокси
// ничего не знает.
type Upstream struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
	MaxBody int64
}

type upstreamResponse struct {
	Status int
	Body   []byte
	Header http.Header
}

func NewUpstream(baseURL, apiKey string, timeout time.Duration, maxBody int64) *Upstream {
	return &Upstream{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: timeout},
		MaxBody: maxBody,
	}
}

// Do пересылает запрос по пути path. Авторизация берётся из запроса клиента,
// если он её прислал: ключ прокси — только фолбэк, иначе прокси незаметно
// начинает платить за чужие запросы.
func (u *Upstream) Do(ctx context.Context, path string, body []byte, in http.Header) (upstreamResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return upstreamResponse{}, fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth := in.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	} else if u.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}
	for _, h := range []string{"OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta"} {
		if v := in.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	resp, err := u.Client.Do(req)
	if err != nil {
		return upstreamResponse{}, fmt.Errorf("call upstream: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, u.MaxBody))
	if err != nil {
		return upstreamResponse{}, fmt.Errorf("read upstream response: %w", err)
	}
	return upstreamResponse{Status: resp.StatusCode, Body: raw, Header: resp.Header.Clone()}, nil
}

// ChatDo подходит под verify.OpenAICompleter: судья ходит тем же путём, что и
// обычный трафик, но ключом прокси — это его собственный расход, а не клиента.
func (u *Upstream) ChatDo(ctx context.Context, body []byte) ([]byte, error) {
	resp, err := u.Do(ctx, "/chat/completions", body, http.Header{})
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, fmt.Errorf("upstream returned %d: %s", resp.Status, snippet(resp.Body))
	}
	return resp.Body, nil
}

func snippet(b []byte) string {
	const limit = 256
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}
