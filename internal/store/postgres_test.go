package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDims держится маленькой: HNSW работает на любой размерности, а тесты
// быстрее.
const testDims = 8

// openTest даёт store на реальной базе. Без SEMCACHE_TEST_DSN тесты
// пропускаются: CI не обязан иметь Postgres, но локально `make pg-up` его даёт.
//
// Каждый тест получает свою схему: параллельные тесты на общей базе иначе
// затирают друг другу данные, а `truncate` одного ломает ожидания другого.
func openTest(t *testing.T, opts PostgresOptions) *Postgres {
	t.Helper()

	dsn := os.Getenv("SEMCACHE_TEST_DSN")
	if dsn == "" {
		t.Skip("SEMCACHE_TEST_DSN is not set; run `make pg-up` to start pgvector")
	}
	if opts.Schema == "" {
		opts.Schema = testSchema(t.Name())
	}

	ctx := context.Background()
	pg, err := OpenPostgres(ctx, dsn, testDims, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)

	if err := pg.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := pg.DropSchema(context.Background()); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	return pg
}

// testSchema делает из имени теста валидный идентификатор схемы.
func testSchema(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, name)
	return "semcache_test_" + safe
}

// unit вектор в направлении i: косинус между разными i равен нулю, между
// одинаковыми — единице, поэтому ожидания в тестах не зависят от арифметики.
func unit(i int) []float32 {
	v := make([]float32, testDims)
	v[i%testDims] = 1
	return v
}

func entry(id string, dir int, tags ...string) Entry {
	return Entry{
		ID:      id,
		Prompt:  "prompt " + id,
		Hash:    "hash-" + id,
		Vector:  unit(dir),
		Payload: "answer " + id,
		Lang:    "en",
		Tags:    tags,
	}
}

func TestPostgresPutAndLookup(t *testing.T) {
	t.Parallel()
	pg := openTest(t, PostgresOptions{})
	ctx := context.Background()

	if err := pg.Put(ctx, entry("a", 0, "doc:1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := pg.Put(ctx, entry("b", 1, "doc:2")); err != nil {
		t.Fatalf("put: %v", err)
	}

	t.Run("exact hash comes first with score 1", func(t *testing.T) {
		got, err := pg.Lookup(ctx, "hash-b", unit(0), 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 || got[0].ID != "b" || got[0].Score != 1 {
			t.Fatalf("got %+v, want entry b with score 1 first", got)
		}
	})

	t.Run("ann orders by cosine and carries the payload", func(t *testing.T) {
		got, err := pg.Lookup(ctx, "no-such-hash", unit(0), 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d candidates, want 2", len(got))
		}
		if got[0].ID != "a" {
			t.Errorf("nearest = %q, want a", got[0].ID)
		}
		if got[0].Score < 0.99 {
			t.Errorf("score = %v, want ~1 for an identical vector", got[0].Score)
		}
		if got[0].Payload != "answer a" || got[0].Lang != "en" {
			t.Errorf("payload = %q, lang = %q", got[0].Payload, got[0].Lang)
		}
		// Вектор намеренно не возвращается: попадание кэша отдаёт payload.
		if got[0].Vector != nil {
			t.Errorf("vector = %v, want nil", got[0].Vector)
		}
	})

	t.Run("no duplicates when hash and ann agree", func(t *testing.T) {
		got, err := pg.Lookup(ctx, "hash-a", unit(0), 5)
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]int{}
		for _, c := range got {
			ids[c.ID]++
		}
		if ids["a"] != 1 {
			t.Errorf("entry a appears %d times, want 1: %+v", ids["a"], got)
		}
	})
}

func TestPostgresPutRewritesTags(t *testing.T) {
	t.Parallel()
	pg := openTest(t, PostgresOptions{})
	ctx := context.Background()

	if err := pg.Put(ctx, entry("a", 0, "doc:1", "doc:2")); err != nil {
		t.Fatal(err)
	}
	// Перезапись с другим набором зависимостей: старый тег не должен убивать
	// запись, которая от него больше не зависит.
	if err := pg.Put(ctx, entry("a", 0, "doc:2")); err != nil {
		t.Fatal(err)
	}

	removed, err := pg.InvalidateTags(ctx, []string{"doc:1"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0: stale tag still points at the entry", removed)
	}

	if removed, err = pg.InvalidateTags(ctx, []string{"doc:2"}); err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
}

func TestPostgresInvalidateIsAtomicWithVector(t *testing.T) {
	t.Parallel()
	pg := openTest(t, PostgresOptions{})
	ctx := context.Background()

	for i, id := range []string{"a", "b", "c"} {
		tag := "doc:shared"
		if id == "c" {
			tag = "doc:other"
		}
		if err := pg.Put(ctx, entry(id, i, tag)); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := pg.InvalidateTags(ctx, []string{"doc:shared"})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}

	// Вектор обязан уйти вместе с payload: иначе ANN продолжит отдавать
	// кандидатов, которых уже нет.
	got, err := pg.Lookup(ctx, "hash-a", unit(0), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.ID == "a" || c.ID == "b" {
			t.Errorf("invalidated entry %q is still a candidate", c.ID)
		}
	}

	st, err := pg.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 1 {
		t.Errorf("rows = %d, want 1", st.Rows)
	}
	// Теги уходят каскадом в той же транзакции.
	if st.Tags != 1 {
		t.Errorf("tag rows = %d, want 1", st.Tags)
	}
}

func TestPostgresExpiredEntriesAreNeverServed(t *testing.T) {
	t.Parallel()
	pg := openTest(t, PostgresOptions{})
	ctx := context.Background()

	dead := entry("dead", 0)
	dead.ExpiresAt = time.Now().Add(-time.Minute)
	if err := pg.Put(ctx, dead); err != nil {
		t.Fatal(err)
	}

	// Точное совпадение хеша тоже не спасает: просроченный ответ отдавать нельзя.
	got, err := pg.Lookup(ctx, "hash-dead", unit(0), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want no candidates", got)
	}

	st, err := pg.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 1 || st.Live != 0 || st.Expired != 1 {
		t.Errorf("stats = %+v, want 1 row, 0 live, 1 expired", st)
	}
}

func TestPostgresRejectsBadEntries(t *testing.T) {
	t.Parallel()
	pg := openTest(t, PostgresOptions{})
	ctx := context.Background()

	tests := []struct {
		name string
		e    Entry
	}{
		{name: "no id", e: Entry{Hash: "h", Vector: unit(0)}},
		{name: "no hash", e: Entry{ID: "x", Vector: unit(0)}},
		{name: "no vector", e: Entry{ID: "x", Hash: "h"}},
		{name: "wrong dims", e: Entry{ID: "x", Hash: "h", Vector: []float32{1, 2}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := pg.Put(ctx, tt.e); !errors.Is(err, ErrInvalidEntry) {
				t.Errorf("err = %v, want ErrInvalidEntry", err)
			}
		})
	}

	if _, err := pg.Lookup(ctx, "h", []float32{1, 2}, 5); !errors.Is(err, ErrInvalidEntry) {
		t.Errorf("lookup err = %v, want ErrInvalidEntry", err)
	}
}

func TestPostgresConcurrentPutSameID(t *testing.T) {
	t.Parallel()
	pg := openTest(t, PostgresOptions{MaxConns: 8})
	ctx := context.Background()

	// Одновременная перезапись одной записи — то, что делает Group при
	// промахе на нескольких соединениях. Upsert плюс перезапись тегов не
	// должны падать на конфликте.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := entry("shared", 0, fmt.Sprintf("doc:%d", i))
			if err := pg.Put(ctx, e); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent put: %v", err)
	}

	st, err := pg.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 1 {
		t.Errorf("rows = %d, want 1", st.Rows)
	}
	if st.Tags != 1 {
		t.Errorf("tag rows = %d, want 1: tag rewrite left leftovers", st.Tags)
	}
}

func TestVectorLiteralRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		vec  []float32
		want string
	}{
		{name: "integers", vec: []float32{1, 2, 3}, want: "[1,2,3]"},
		{name: "fractions", vec: []float32{0.5, -0.25}, want: "[0.5,-0.25]"},
		{name: "single", vec: []float32{1}, want: "[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := vectorLiteral(tt.vec)
			if got != tt.want {
				t.Fatalf("vectorLiteral = %q, want %q", got, tt.want)
			}
			back, err := parseVector(got)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(back, tt.vec) {
				t.Errorf("round trip = %v, want %v", back, tt.vec)
			}
		})
	}
}

func TestParseVectorRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parseVector("[1,oops]"); err == nil {
		t.Fatal("err = nil, want a parse error")
	}
}
