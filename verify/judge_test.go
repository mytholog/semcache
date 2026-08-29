package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type stubComplete struct {
	text   string
	tokens int
	err    error
	calls  int
}

func (s *stubComplete) Complete(context.Context, string, string) (string, int, error) {
	s.calls++
	return s.text, s.tokens, s.err
}

func TestJudgeParsesAndCaches(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stub := &stubComplete{text: `{"interchangeable":false,"reason":"negation"}`, tokens: 40}
	j := NewJudge(stub, dir)

	d, err := j.Interchangeable(context.Background(), "with key", "without key")
	if err != nil {
		t.Fatal(err)
	}
	if d.OK {
		t.Fatal("expected rejection")
	}
	if stub.calls != 1 {
		t.Fatalf("calls = %d, want 1", stub.calls)
	}

	d2, err := j.Interchangeable(context.Background(), "with key", "without key")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Reason != "negation" || stub.calls != 1 {
		t.Fatalf("second call: dec=%+v calls=%d", d2, stub.calls)
	}
	if j.CacheHits != 1 {
		t.Errorf("CacheHits = %d, want 1", j.CacheHits)
	}

	j2 := NewJudge(stub, dir)
	if _, err := j2.Interchangeable(context.Background(), "with key", "without key"); err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("disk cache missed, calls = %d", stub.calls)
	}
	// Стоимость холодного прогона должна выживать кэш, иначе cost model
	// показывает ноль на каждом повторном запуске.
	if j2.Tokens != 40 || j2.SpentTokens != 0 {
		t.Errorf("cached run: Tokens = %d, SpentTokens = %d, want 40 and 0", j2.Tokens, j2.SpentTokens)
	}
	if got := j2.USD(); got <= 0 {
		t.Errorf("USD = %v, want > 0 on a cached run", got)
	}
	if _, err := os.Stat(filepath.Join(dir, judgeKey("with key", "without key")+".json")); err != nil {
		t.Fatal(err)
	}
}
