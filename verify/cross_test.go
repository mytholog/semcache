package verify

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func stubExec(t *testing.T, calls *int, mu *sync.Mutex) ExecFunc {
	t.Helper()
	return func(_ context.Context, stdin []byte) ([]byte, error) {
		var req rerankRequest
		if err := json.Unmarshal(stdin, &req); err != nil {
			return nil, err
		}
		mu.Lock()
		*calls++
		mu.Unlock()
		scores := make([]float64, len(req.Pairs))
		for i := range scores {
			scores[i] = 0.1 * float64(i+1)
		}
		return json.Marshal(rerankResponse{Scores: scores})
	}
}

func TestCrossEncoderCachesScores(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls int
	)
	dir := t.TempDir()
	ce := NewCrossEncoder("stub-reranker", CrossEncoderOptions{
		CacheDir: dir,
		Exec:     stubExec(t, &calls, &mu),
	})

	pairs := [][2]string{{"a", "b"}, {"c", "d"}}
	got, err := ce.ScorePairs(context.Background(), pairs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 0.1 || got[1] != 0.2 {
		t.Fatalf("scores = %v", got)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	if _, err := ce.ScorePairs(context.Background(), pairs); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("in-memory cache missed, calls = %d", calls)
	}

	// Только новая пара уходит в sidecar, старая берётся из кэша.
	got, err = ce.ScorePairs(context.Background(), [][2]string{{"a", "b"}, {"e", "f"}})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("partial cache: calls = %d, want 2", calls)
	}
	if got[0] != 0.1 || got[1] != 0.1 {
		t.Fatalf("mixed scores = %v", got)
	}

	// Новый инстанс поднимает тот же дисковый кэш.
	fresh := NewCrossEncoder("stub-reranker", CrossEncoderOptions{
		CacheDir: dir,
		Exec:     stubExec(t, &calls, &mu),
	})
	if _, err := fresh.ScorePairs(context.Background(), pairs); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("disk cache missed, calls = %d", calls)
	}
}

func TestCrossEncoderConcurrent(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls int
	)
	ce := NewCrossEncoder("stub-reranker", CrossEncoderOptions{
		CacheDir: t.TempDir(),
		Exec:     stubExec(t, &calls, &mu),
	})
	v := Threshold{Scorer: ce, Min: 0.05}

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Interchangeable(context.Background(), "a", string(rune('a'+i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestCrossEncoderEmpty(t *testing.T) {
	t.Parallel()
	got, err := NewCrossEncoder("stub", CrossEncoderOptions{}).ScorePairs(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func TestCrossEncoderScoreCountMismatch(t *testing.T) {
	t.Parallel()
	ce := NewCrossEncoder("stub", CrossEncoderOptions{
		Exec: func(context.Context, []byte) ([]byte, error) {
			return json.Marshal(rerankResponse{Scores: []float64{0.5}})
		},
	})
	if _, err := ce.ScorePairs(context.Background(), [][2]string{{"a", "b"}, {"c", "d"}}); err == nil {
		t.Fatal("expected mismatch error")
	}
}
