package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type localRequest struct {
	Model string   `json:"model"`
	Texts []string `json:"texts"`
}

type localResponse struct {
	Vectors [][]float32 `json:"vectors"`
}

func newLocal(remote, prefix, script string, timeout time.Duration) batchEmbed {
	return func(ctx context.Context, texts []string) ([][]float32, int, error) {
		in := texts
		if prefix != "" {
			in = make([]string, len(texts))
			for i, t := range texts {
				in[i] = prefix + t
			}
		}
		body, err := json.Marshal(localRequest{Model: remote, Texts: in})
		if err != nil {
			return nil, 0, fmt.Errorf("encode local embed request: %w", err)
		}

		cmdCtx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			cmdCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}

		cmd := exec.CommandContext(cmdCtx, "uv", "run", "--project", "bench", "python", script)
		cmd.Stdin = bytes.NewReader(body)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, 0, fmt.Errorf("local embedder %s: %w\n%s", remote, err, stderr.String())
		}

		var out localResponse
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			return nil, 0, fmt.Errorf("decode local embedder output: %w", err)
		}
		if len(out.Vectors) != len(texts) {
			return nil, 0, fmt.Errorf("local embedder returned %d vectors for %d inputs", len(out.Vectors), len(texts))
		}
		return out.Vectors, 0, nil
	}
}

func localScriptExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("local embed script: %w", err)
	}
	return nil
}
