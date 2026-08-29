package cache

import (
	"context"
	"testing"

	"github.com/mytholog/semcache/internal/store"
	"github.com/mytholog/semcache/internal/verify"
)

type rejectAll struct{}

func (rejectAll) Interchangeable(context.Context, string, string) (verify.Decision, error) {
	return verify.Decision{OK: false}, nil
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
	e, kind, err := ts.Get(ctx, "same", "abc", []float32{1, 0})
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
	_, kind, err := ts.Get(ctx, "with a key", "incoming", []float32{1, 0})
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
	e, kind, err := ts.Get(ctx, "how do I reset my password", "incoming", []float32{1, 0})
	if err != nil {
		t.Fatal(err)
	}
	if kind != KindVerified || e.Payload != "ok" {
		t.Fatalf("kind=%s payload=%q", kind, e.Payload)
	}
}
