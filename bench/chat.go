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

func newChatDo(baseURL, apiKey string, timeout time.Duration) (func(context.Context, []byte) ([]byte, error), error) {
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}
	client := &http.Client{Timeout: timeout}
	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	return func(ctx context.Context, body []byte) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build chat request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("call chat API: %w", err)
		}
		defer resp.Body.Close()

		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("read chat response: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("chat API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return raw, nil
	}, nil
}
