package store

import (
	"context"
	"testing"
	"time"
)

// Правила, одинаковые для всех реализаций Store. Memory используется в
// бенчмарках как эталон, поэтому расхождение с Postgres в поведении — это
// расхождение между измерениями и продакшном.
func TestStoreConformance(t *testing.T) {
	t.Parallel()

	impls := []struct {
		name string
		open func(t *testing.T) Store
	}{
		{
			name: "memory",
			open: func(*testing.T) Store { return NewMemory() },
		},
		{
			name: "postgres",
			open: func(t *testing.T) Store { return openTest(t, PostgresOptions{}) },
		},
	}

	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()

			t.Run("expired entries are never served", func(t *testing.T) {
				s := impl.open(t)
				ctx := context.Background()

				dead := entry("dead", 0)
				dead.ExpiresAt = time.Now().Add(-time.Minute)
				live := entry("live", 1)
				for _, e := range []Entry{dead, live} {
					if err := s.Put(ctx, e); err != nil {
						t.Fatal(err)
					}
				}

				// Ни точное совпадение хеша, ни ANN не должны отдать истёкшую запись.
				byHash, err := s.Lookup(ctx, "", "hash-dead", unit(0), 5)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range byHash {
					if c.ID == "dead" {
						t.Error("expired entry served on an exact hash match")
					}
				}

				byVector, err := s.Lookup(ctx, "", "no-such-hash", unit(0), 5)
				if err != nil {
					t.Fatal(err)
				}
				for _, c := range byVector {
					if c.ID == "dead" {
						t.Error("expired entry served as a nearest neighbour")
					}
				}
				if len(byVector) != 1 || byVector[0].ID != "live" {
					t.Errorf("got %d candidates, want only the live one", len(byVector))
				}
			})

			t.Run("namespaces are isolated", func(t *testing.T) {
				s := impl.open(t)
				ctx := context.Background()

				mini := entry("mini", 0)
				mini.Namespace = "gpt-4o-mini"
				big := entry("big", 0)
				big.Namespace = "gpt-4o"
				big.Hash = mini.Hash // один и тот же промпт к двум моделям
				for _, e := range []Entry{mini, big} {
					if err := s.Put(ctx, e); err != nil {
						t.Fatal(err)
					}
				}

				got, err := s.Lookup(ctx, "gpt-4o-mini", mini.Hash, unit(0), 5)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 1 || got[0].ID != "mini" {
					t.Fatalf("got %d candidates %+v, want only the one from gpt-4o-mini", len(got), got)
				}

				// Пустой namespace — это отдельное пространство, а не «любое».
				if got, err = s.Lookup(ctx, "", mini.Hash, unit(0), 5); err != nil {
					t.Fatal(err)
				} else if len(got) != 0 {
					t.Errorf("got %+v for the default namespace, want nothing", got)
				}
			})

			t.Run("invalidation removes the vector too", func(t *testing.T) {
				s := impl.open(t)
				ctx := context.Background()

				if err := s.Put(ctx, entry("a", 0, "doc:1")); err != nil {
					t.Fatal(err)
				}
				removed, err := s.InvalidateTags(ctx, []string{"doc:1"})
				if err != nil {
					t.Fatal(err)
				}
				if removed != 1 {
					t.Fatalf("removed = %d, want 1", removed)
				}

				got, err := s.Lookup(ctx, "", "no-such-hash", unit(0), 5)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 0 {
					t.Errorf("got %+v, want nothing: the vector outlived the entry", got)
				}
			})
		})
	}
}
