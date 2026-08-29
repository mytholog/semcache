package verify

import (
	"context"
	"fmt"
)

// Decision — ответ верификатора: можно ли отдать закэшированный ответ.
type Decision struct {
	OK     bool
	Score  float64
	Reason string
}

// Verifier решает, взаимозаменяемы ли два промпта как ключи кэша.
type Verifier interface {
	Interchangeable(ctx context.Context, incoming, cached string) (Decision, error)
}

// PairScorer считает оценку для пачки пар — нужен кросс-энкодеру, чтобы
// не поднимать Python на каждую пару.
type PairScorer interface {
	ScorePairs(ctx context.Context, pairs [][2]string) ([]float64, error)
}

// Noop принимает любую пару: это сегодняшний порог косинуса без второй стадии.
type Noop struct{}

func (Noop) Interchangeable(context.Context, string, string) (Decision, error) {
	return Decision{OK: true, Score: 1, Reason: "noop"}, nil
}

// Threshold оборачивает скорер: OK, если оценка не ниже Min.
type Threshold struct {
	Scorer PairScorer
	Min    float64
}

func (t Threshold) Interchangeable(ctx context.Context, incoming, cached string) (Decision, error) {
	scores, err := t.Scorer.ScorePairs(ctx, [][2]string{{incoming, cached}})
	if err != nil {
		return Decision{}, fmt.Errorf("score pairs: %w", err)
	}
	if len(scores) == 0 {
		return Decision{}, fmt.Errorf("score pairs: empty result")
	}
	score := scores[0]
	return Decision{OK: score >= t.Min, Score: score}, nil
}

var (
	_ Verifier   = Noop{}
	_ Verifier   = Threshold{}
	_ Verifier   = (*Judge)(nil)
	_ PairScorer = (*CrossEncoder)(nil)
)
