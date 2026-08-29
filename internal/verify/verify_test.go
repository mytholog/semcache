package verify

import (
	"context"
	"testing"
)

type stubScorer struct {
	scores []float64
	err    error
	calls  int
}

func (s *stubScorer) ScorePairs(context.Context, [][2]string) ([]float64, error) {
	s.calls++
	return s.scores, s.err
}

func TestNoopAlwaysAccepts(t *testing.T) {
	t.Parallel()
	d, err := Noop{}.Interchangeable(context.Background(), "a", "not a")
	if err != nil {
		t.Fatal(err)
	}
	if !d.OK {
		t.Fatal("Noop rejected a pair")
	}
}

func TestThresholdUsesScore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		score float64
		min   float64
		want  bool
	}{
		{name: "above", score: 0.8, min: 0.5, want: true},
		{name: "equal", score: 0.5, min: 0.5, want: true},
		{name: "below", score: 0.4, min: 0.5, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := Threshold{Scorer: &stubScorer{scores: []float64{tt.score}}, Min: tt.min}
			d, err := v.Interchangeable(context.Background(), "a", "b")
			if err != nil {
				t.Fatal(err)
			}
			if d.OK != tt.want || d.Score != tt.score {
				t.Errorf("Decision = %+v, want OK=%v score=%v", d, tt.want, tt.score)
			}
		})
	}
}
