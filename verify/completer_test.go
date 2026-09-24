package verify

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

var errStub = errors.New("stub failure")

func TestOpenAICompleterParsesJSON(t *testing.T) {
	t.Parallel()

	c := OpenAICompleter{
		Model: "gpt-4o-mini",
		Do: func(context.Context, []byte) ([]byte, error) {
			return []byte(`{"choices":[{"message":{"content":"{\"interchangeable\":true,\"reason\":\"paraphrase\"}"}}],"usage":{"total_tokens":12}}`), nil
		},
	}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != `{"interchangeable":true,"reason":"paraphrase"}` || got.Tokens() != 12 {
		t.Fatalf("text=%q tokens=%d", got.Text, got.Tokens())
	}
}

func TestOpenAICompleterPropagatesError(t *testing.T) {
	t.Parallel()
	c := OpenAICompleter{
		Do: func(context.Context, []byte) ([]byte, error) {
			return nil, errStub
		},
	}
	if _, err := c.Complete(context.Background(), "sys", "user"); !errors.Is(err, errStub) {
		t.Fatalf("err = %v, want %v", err, errStub)
	}
}

func TestOpenAICompleterGPT5DropsTemperature(t *testing.T) {
	t.Parallel()

	var raw []byte
	c := OpenAICompleter{
		Model: "gpt-5-nano",
		Do: func(_ context.Context, body []byte) ([]byte, error) {
			raw = append([]byte(nil), body...)
			return []byte(`{"choices":[{"message":{"content":"{\"interchangeable\":false,\"reason\":\"negation\"}"}}],"usage":{"prompt_tokens":40,"completion_tokens":12,"total_tokens":52}}`), nil
		},
	}
	got, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != 40 || got.Completion != 12 || got.Tokens() != 52 {
		t.Fatalf("usage = %+v", got)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if _, ok := req["temperature"]; ok {
		t.Fatalf("temperature sent: %v", req["temperature"])
	}
	if req["reasoning_effort"] != "minimal" {
		t.Fatalf("reasoning_effort = %v", req["reasoning_effort"])
	}
}

func TestJudgeSplitPrice(t *testing.T) {
	t.Parallel()
	j := NewJudge(&stubComplete{}, t.TempDir())
	j.InputPerM = 0.25
	j.OutputPerM = 0.5
	j.PromptTokens = 4_000_000
	j.CompletionTokens = 2_000_000
	j.Tokens = 6_000_000
	// 4*0.25 + 2*0.5 = 2
	if got := j.USD(); got != 2 {
		t.Fatalf("USD = %v, want 2", got)
	}
}

func TestOpenAICompleterNoChoices(t *testing.T) {
	t.Parallel()
	c := OpenAICompleter{
		Do: func(context.Context, []byte) ([]byte, error) {
			return []byte(`{"choices":[]}`), nil
		},
	}
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected error")
	}
}
