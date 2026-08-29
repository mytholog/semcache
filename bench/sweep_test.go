package main

import (
	"fmt"
	"math"
	"testing"

	"github.com/mytholog/semcache/internal/dataset"
)

func pair(id string, interchangeable bool, sim float64) scored {
	return scored{
		Pair: dataset.Pair{ID: id, Category: "paraphrase", Interchangeable: interchangeable},
		sim:  sim,
	}
}

func TestSweepCountsAtThreshold(t *testing.T) {
	pairs := []scored{
		pair("p1", true, 0.99),
		pair("p2", true, 0.93),
		pair("n1", false, 0.96),
		pair("n2", false, 0.80),
	}

	rows := sweep(pairs, 0.95, 0.95, 0.01)
	if len(rows) != 1 {
		t.Fatalf("sweep returned %d rows, want 1", len(rows))
	}

	got := rows[0]
	if got.tp != 1 || got.fn != 1 || got.fp != 1 || got.tn != 1 {
		t.Errorf("confusion matrix = tp %d fn %d fp %d tn %d, want 1/1/1/1", got.tp, got.fn, got.fp, got.tn)
	}
	if math.Abs(got.hitRate-0.5) > 1e-9 {
		t.Errorf("hit rate = %v, want 0.5", got.hitRate)
	}
	if math.Abs(got.falseHit-0.5) > 1e-9 {
		t.Errorf("false-hit rate = %v, want 0.5", got.falseHit)
	}
	if math.Abs(got.precision-0.5) > 1e-9 {
		t.Errorf("precision = %v, want 0.5", got.precision)
	}
}

func TestSweepRangeIsInclusive(t *testing.T) {
	rows := sweep([]scored{pair("p1", true, 1)}, 0.90, 0.95, 0.01)
	if len(rows) != 6 {
		t.Fatalf("sweep 0.90..0.95 step 0.01 returned %d rows, want 6", len(rows))
	}
	if last := rows[len(rows)-1].threshold; math.Abs(last-0.95) > 1e-9 {
		t.Errorf("last threshold = %v, want 0.95", last)
	}
}

func TestJudge(t *testing.T) {
	// Полнота держится на 100%, доля ложных попаданий задаётся числом отрицательных
	// пар, перешагнувших порог. Отрицательных пар ровно 100, поэтому их количество
	// читается как проценты напрямую.
	const negatives = 100
	dataset := func(falseHits int) []scored {
		pairs := []scored{pair("p1", true, 0.99)}
		for i := range negatives {
			sim := 0.10
			if i < falseHits {
				sim = 0.99
			}
			pairs = append(pairs, pair(fmt.Sprintf("n%03d", i), false, sim))
		}
		return pairs
	}

	tests := []struct {
		name      string
		falseHits int
		want      string
	}{
		{name: "clean separation", falseHits: 0, want: "THESIS DEAD"},
		{name: "negligible overlap", falseHits: 1, want: "THESIS DEAD"},
		{name: "marginal overlap", falseHits: 5, want: "THESIS WEAK"},
		{name: "heavy overlap", falseHits: 20, want: "THESIS HOLDS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := judge(sweep(dataset(tt.falseHits), 0.80, 0.99, 0.01), 0.70)
			if !v.feasible {
				t.Fatalf("verdict is not feasible, want a usable threshold")
			}
			if got := v.conclusion[:len(tt.want)]; got != tt.want {
				t.Errorf("conclusion = %q, want prefix %q", v.conclusion, tt.want)
			}
		})
	}
}

func TestJudgeReportsInfeasibleRange(t *testing.T) {
	// Все взаимозаменяемые пары ниже нижней границы сетки — порога, дающего нужную
	// полноту, не существует.
	pairs := []scored{pair("p1", true, 0.50), pair("n1", false, 0.99)}
	if v := judge(sweep(pairs, 0.80, 0.99, 0.01), 0.70); v.feasible {
		t.Errorf("verdict = feasible at threshold %v, want infeasible", v.atThreshold)
	}
}

func TestSimilarityOfNormalizedVectors(t *testing.T) {
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{name: "identical", a: []float32{3, 4}, b: []float32{3, 4}, want: 1},
		{name: "orthogonal", a: []float32{1, 0}, b: []float32{0, 2}, want: 0},
		{name: "opposite", a: []float32{1, 1}, b: []float32{-1, -1}, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := tt.a, tt.b
			normalize(a)
			normalize(b)
			if got := similarity(a, b); math.Abs(got-tt.want) > 1e-6 {
				t.Errorf("similarity = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeZeroVector(t *testing.T) {
	v := []float32{0, 0}
	normalize(v)
	if v[0] != 0 || v[1] != 0 {
		t.Errorf("normalize produced %v, want zeros left untouched", v)
	}
}

func TestByCategoryOrdersNonInterchangeableFirst(t *testing.T) {
	pairs := []scored{
		{Pair: dataset.Pair{ID: "p", Category: "paraphrase", Interchangeable: true}, sim: 0.99},
		{Pair: dataset.Pair{ID: "n", Category: "negation", Interchangeable: false}, sim: 0.95},
	}

	stats := byCategory(pairs, []float64{0.95})
	if stats[0].category != "negation" {
		t.Errorf("first category = %q, want negation", stats[0].category)
	}
	if got := stats[0].aboveThreshold[0.95]; got != 1 {
		t.Errorf("negation pairs at or above 0.95 = %d, want 1", got)
	}
}
