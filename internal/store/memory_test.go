package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func vec(xs ...float32) []float32 {
	out := make([]float32, len(xs))
	copy(out, xs)
	return out
}

func TestMemoryPutRejectsIncompleteEntry(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	ctx := context.Background()

	tests := []struct {
		name  string
		entry Entry
	}{
		{name: "empty id", entry: Entry{Hash: "h", Vector: vec(1)}},
		{name: "empty hash", entry: Entry{ID: "1", Vector: vec(1)}},
		{name: "empty vector", entry: Entry{ID: "1", Hash: "h"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := m.Put(ctx, tt.entry)
			if err == nil {
				t.Fatal("Put succeeded, want ErrInvalidEntry")
			}
		})
	}
}

func TestMemoryLookupExactHashBeatsNearerNeighbour(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	ctx := context.Background()

	near := Entry{ID: "near", Hash: "other", Prompt: "other", Vector: vec(0.99, 0.01), Payload: "near"}
	exact := Entry{ID: "exact", Hash: "want", Prompt: "want", Vector: vec(0, 1), Payload: "exact"}
	if err := m.Put(ctx, near); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, exact); err != nil {
		t.Fatal(err)
	}

	got, err := m.Lookup(ctx, "want", vec(1, 0), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2", len(got))
	}
	if got[0].ID != "exact" {
		t.Errorf("first candidate = %q, want exact hash match", got[0].ID)
	}
}

func TestMemoryLookupANNOrder(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	ctx := context.Background()

	if err := m.Put(ctx, Entry{ID: "far", Hash: "a", Vector: vec(0, 1), Payload: "far"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, Entry{ID: "close", Hash: "b", Vector: vec(1, 0), Payload: "close"}); err != nil {
		t.Fatal(err)
	}

	got, err := m.Lookup(ctx, "missing", vec(1, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "close" {
		t.Fatalf("got %+v, want close", got)
	}
}

func TestMemoryInvalidateTagsRemovesVector(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	ctx := context.Background()

	if err := m.Put(ctx, Entry{
		ID: "doc47", Hash: "h1", Vector: vec(1, 0), Payload: "old", Tags: []string{"doc:47"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Put(ctx, Entry{
		ID: "other", Hash: "h2", Vector: vec(0, 1), Payload: "keep", Tags: []string{"doc:1"},
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := m.InvalidateTags(ctx, []string{"doc:47"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	got, err := m.Lookup(ctx, "h1", vec(1, 0), 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.ID == "doc47" {
			t.Fatal("invalidated entry still returned by ANN lookup")
		}
	}
	if len(got) != 1 || got[0].ID != "other" {
		t.Fatalf("remaining = %+v, want only other", got)
	}
}

func TestMemoryLookupCopiesEntry(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	ctx := context.Background()
	if err := m.Put(ctx, Entry{
		ID: "1", Hash: "h", Vector: vec(1, 0), Payload: "p", Tags: []string{"t"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := m.Lookup(ctx, "h", vec(1, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	got[0].Payload = "mutated"
	got[0].Vector[0] = 0
	got[0].Tags[0] = "x"

	again, err := m.Lookup(ctx, "h", vec(1, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Payload != "p" || again[0].Vector[0] != 1 || again[0].Tags[0] != "t" {
		t.Fatalf("store entry was aliased: %+v", again[0])
	}
}

func TestSingleflightCoalescesLoader(t *testing.T) {
	t.Parallel()
	m := NewMemory()
	g := NewGroup(m)

	var loads atomic.Int32
	load := func(context.Context) (Entry, error) {
		loads.Add(1)
		return Entry{ID: "1", Hash: "same", Vector: vec(1, 0), Payload: "ok"}, nil
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			_, _, err := g.GetOrLoad(ctx, "same", vec(1, 0), load)
			errCh <- err
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := loads.Load(); n != 1 {
		t.Errorf("loader called %d times, want 1", n)
	}
}
