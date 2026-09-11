package bifrost

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestNamespaceSeparatesDeployments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		prefix   string
		provider schemas.ModelProvider
		model    string
		want     string
	}{
		{"", schemas.OpenAI, "gpt-4o-mini", "openai/gpt-4o-mini"},
		{"prod:", schemas.OpenAI, "gpt-4o-mini", "prod:openai/gpt-4o-mini"},
		{"prod:", schemas.Azure, "gpt-4o-mini", "prod:azure/gpt-4o-mini"},
	}
	for _, tc := range cases {
		if got := namespaceOf(tc.prefix, tc.provider, tc.model); got != tc.want {
			t.Errorf("namespaceOf(%q, %q, %q) = %q, want %q", tc.prefix, tc.provider, tc.model, got, tc.want)
		}
	}
}

func TestCacheKeyCoversWholeConversation(t *testing.T) {
	t.Parallel()
	system, user := "You are terse.", "Reset my password?"
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &system}},
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user}},
	}}

	key, reason := cacheKey(req)
	if reason != "" {
		t.Fatalf("bypass reason = %q, want none", reason)
	}
	want := "system: You are terse.\nuser: Reset my password?"
	if key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
}

func TestCacheKeyDistinguishesSystemPrompt(t *testing.T) {
	t.Parallel()
	// Ответ зависит от системного промпта, поэтому один и тот же вопрос под
	// разными системными промптами — разные ключи.
	user := "Reset my password?"
	terse, verbose := "You are terse.", "You are verbose."

	keyOf := func(system string) string {
		req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleSystem, Content: &schemas.ChatMessageContent{ContentStr: &system}},
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &user}},
		}}
		key, reason := cacheKey(req)
		if reason != "" {
			t.Fatalf("bypass reason = %q, want none", reason)
		}
		return key
	}

	if keyOf(terse) == keyOf(verbose) {
		t.Error("different system prompts must produce different keys")
	}
}
