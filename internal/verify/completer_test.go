package verify

import (
	"context"
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
	text, tokens, err := c.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"interchangeable":true,"reason":"paraphrase"}` || tokens != 12 {
		t.Fatalf("text=%q tokens=%d", text, tokens)
	}
}

func TestOpenAICompleterPropagatesError(t *testing.T) {
	t.Parallel()
	c := OpenAICompleter{
		Do: func(context.Context, []byte) ([]byte, error) {
			return nil, errStub
		},
	}
	if _, _, err := c.Complete(context.Background(), "sys", "user"); !errors.Is(err, errStub) {
		t.Fatalf("err = %v, want %v", err, errStub)
	}
}

func TestOpenAICompleterNoChoices(t *testing.T) {
	t.Parallel()
	c := OpenAICompleter{
		Do: func(context.Context, []byte) ([]byte, error) {
			return []byte(`{"choices":[]}`), nil
		},
	}
	_, _, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected error")
	}
}
