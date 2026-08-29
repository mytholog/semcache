package verify

import "testing"

func TestEvaluateTwoStage(t *testing.T) {
	t.Parallel()

	// 0: paraphrase, high sim, verifier says yes
	// 1: paraphrase, low sim — не доходит до верификатора
	// 2: negation, high sim, verifier says no
	// 3: negation, high sim, verifier says yes (ошибка)
	sim := []float64{0.95, 0.40, 0.96, 0.97}
	labels := []bool{true, true, false, false}
	ok := []bool{true, true, false, true}

	got := Evaluate(sim, labels, 0.70, ok)
	want := Counts{TP: 1, FN: 1, FP: 1, TN: 1, VerifyCalls: 3}
	if got != want {
		t.Errorf("Evaluate = %+v, want %+v", got, want)
	}
}

func TestEvaluateNoopEqualsRetrieveOnly(t *testing.T) {
	t.Parallel()
	sim := []float64{0.9, 0.5}
	labels := []bool{true, false}
	ok := []bool{true, true}

	got := Evaluate(sim, labels, 0.80, ok)
	if got.TP != 1 || got.TN != 1 || got.VerifyCalls != 1 {
		t.Errorf("Evaluate = %+v", got)
	}
}

func TestCostVerifyShare(t *testing.T) {
	t.Parallel()
	c := Cost{Hits: 100, ProviderUSD: 0.002, VerifyUSD: 0.004}
	if got := c.SavedUSD(); got != 0.196 {
		t.Errorf("SavedUSD = %v, want 0.196", got)
	}
	if got := c.VerifyShare(); got < 0.019 || got > 0.021 {
		t.Errorf("VerifyShare = %v, want ~0.02", got)
	}
}
