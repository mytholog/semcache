package main

import (
	"testing"

	"github.com/mytholog/semcache/internal/verify"
)

func TestSweepVerifierAppliesRetrieveThenScore(t *testing.T) {
	t.Parallel()

	sims := []float64{0.95, 0.40, 0.96}
	labels := []bool{true, true, false}
	scores := []float64{0.9, 0.9, 0.1}

	rows := sweepVerifier(sims, labels, scores, 0.70, 0.50, 0.50, 0.10)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	got := rows[0]
	// пара 0: retrieved + score ok → TP
	// пара 1: не retrieved → FN
	// пара 2: retrieved + score reject → TN
	if got.tp != 1 || got.fn != 1 || got.tn != 1 || got.fp != 0 {
		t.Fatalf("row = %+v", got)
	}
}

func TestCountsToRowCopiesRates(t *testing.T) {
	t.Parallel()
	c := verify.Counts{TP: 8, FN: 2, FP: 1, TN: 9}
	r := countsToRow(0.4, c)
	if r.hitRate != c.HitRate() || r.falseHit != c.FalseHit() || r.threshold != 0.4 {
		t.Fatalf("row = %+v counts = %+v", r, c)
	}
}
