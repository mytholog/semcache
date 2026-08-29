package semcache

import (
	"context"
	"strings"
	"testing"

	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
)

type rejectAll struct{}

func (rejectAll) Interchangeable(context.Context, string, string) (verify.Decision, error) {
	return verify.Decision{OK: false}, nil
}

// stubLang считает текст написанным на языке, код которого встречается в самом
// тексте: тесту нужна предсказуемость, а не определитель языка.
type stubLang struct{}

func (stubLang) Detect(text string) (string, bool) {
	for _, lang := range []string{"en", "de", "ru"} {
		if strings.Contains(text, "["+lang+"]") {
			return lang, true
		}
	}
	return "", false
}

func (s stubLang) NotLanguage(text, lang string) bool {
	detected, confident := s.Detect(text)
	return confident && detected != lang
}

func TestTwoStageExactSkipsVerifier(t *testing.T) {
	t.Parallel()
	mem := store.NewMemory()
	ctx := context.Background()
	if err := mem.Put(ctx, store.Entry{
		ID: "1", Hash: "abc", Prompt: "same", Vector: []float32{1, 0}, Payload: "hit",
	}); err != nil {
		t.Fatal(err)
	}

	ts := TwoStage{Store: mem, Verifier: rejectAll{}, RetrieveMin: 0.1, K: 3}
	e, kind, _, err := ts.Get(ctx, Query{Prompt: "same", Hash: "abc", Vector: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindExact || e.Payload != "hit" {
		t.Fatalf("kind=%s payload=%q", kind, e.Payload)
	}
}

func TestTwoStageRejectsNearMiss(t *testing.T) {
	t.Parallel()
	mem := store.NewMemory()
	ctx := context.Background()
	if err := mem.Put(ctx, store.Entry{
		ID: "1", Hash: "other", Prompt: "without a key", Vector: []float32{1, 0}, Payload: "wrong",
	}); err != nil {
		t.Fatal(err)
	}

	ts := TwoStage{Store: mem, Verifier: rejectAll{}, RetrieveMin: 0.5, K: 3}
	_, kind, _, err := ts.Get(ctx, Query{Prompt: "with a key", Hash: "incoming", Vector: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindReject {
		t.Fatalf("kind=%s, want reject", kind)
	}
}

func TestTwoStageVerifiedHit(t *testing.T) {
	t.Parallel()
	mem := store.NewMemory()
	ctx := context.Background()
	if err := mem.Put(ctx, store.Entry{
		ID: "1", Hash: "other", Prompt: "reset password", Vector: []float32{1, 0}, Payload: "ok",
	}); err != nil {
		t.Fatal(err)
	}

	ts := TwoStage{Store: mem, Verifier: verify.Noop{}, RetrieveMin: 0.5, K: 3}
	e, kind, score, err := ts.Get(ctx, Query{Prompt: "how do I reset my password", Hash: "incoming", Vector: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindVerified || e.Payload != "ok" {
		t.Fatalf("kind=%s payload=%q", kind, e.Payload)
	}
	if score < 0.99 {
		t.Errorf("score = %v, want the candidate cosine", score)
	}
}

// Записанный язык — это то, чего не знает вторая стадия: у неё есть только два
// коротких промпта, а у записи язык определён по полному ответу.
func TestTwoStageRejectsOnStoredLanguage(t *testing.T) {
	t.Parallel()
	mem := store.NewMemory()
	ctx := context.Background()
	if err := mem.Put(ctx, store.Entry{
		ID: "1", Hash: "other", Prompt: "2FA aktivieren", Vector: []float32{1, 0},
		Payload: "German answer", Lang: "de",
	}); err != nil {
		t.Fatal(err)
	}

	// Верификатор пропускает всё: отклонить должен именно языковой гейт.
	ts := TwoStage{Store: mem, Verifier: verify.Noop{}, RetrieveMin: 0.5, K: 3, Lang: stubLang{}}
	_, kind, _, err := ts.Get(ctx, Query{Prompt: "how do I enable 2FA [en]", Hash: "incoming", Vector: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindReject {
		t.Fatalf("kind=%s, want reject: a German answer is not interchangeable with an English question", kind)
	}
}

func TestTwoStageKeepsCandidateWithoutStoredLanguage(t *testing.T) {
	t.Parallel()
	mem := store.NewMemory()
	ctx := context.Background()
	// Язык не записан — гейт обязан промолчать, а не отклонить.
	if err := mem.Put(ctx, store.Entry{
		ID: "1", Hash: "other", Prompt: "enable 2FA", Vector: []float32{1, 0}, Payload: "ok",
	}); err != nil {
		t.Fatal(err)
	}

	ts := TwoStage{Store: mem, Verifier: verify.Noop{}, RetrieveMin: 0.5, K: 3, Lang: stubLang{}}
	_, kind, _, err := ts.Get(ctx, Query{Prompt: "how do I enable 2FA [en]", Hash: "incoming", Vector: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindVerified {
		t.Fatalf("kind=%s, want verified", kind)
	}
}

func TestTwoStageIsolatesNamespaces(t *testing.T) {
	t.Parallel()
	mem := store.NewMemory()
	ctx := context.Background()
	if err := mem.Put(ctx, store.Entry{
		ID: "1", Namespace: "gpt-4o", Hash: "abc", Prompt: "same", Vector: []float32{1, 0}, Payload: "from 4o",
	}); err != nil {
		t.Fatal(err)
	}

	ts := TwoStage{Store: mem, Verifier: verify.Noop{}, RetrieveMin: 0.5, K: 3}
	_, kind, _, err := ts.Get(ctx, Query{
		Namespace: "gpt-4o-mini", Prompt: "same", Hash: "abc", Vector: []float32{1, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindMiss {
		t.Fatalf("kind=%s, want miss: another model's answer is not a hit", kind)
	}
}
