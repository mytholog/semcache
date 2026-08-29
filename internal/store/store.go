package store

import (
	"context"
	"errors"
)

// ErrInvalidEntry — запись нельзя положить в store: нет ключа, хеша или вектора.
var ErrInvalidEntry = errors.New("invalid cache entry")

// Entry — одна закэшированная пара «промпт → ответ» вместе с вектором и тегами.
type Entry struct {
	ID      string
	Prompt  string
	Hash    string
	Vector  []float32
	Payload string
	Tags    []string
}

// Candidate — результат Lookup: запись и косинус к запросу.
type Candidate struct {
	Entry
	Score float64
}

// Store владеет записями, векторами и тегами вместе: инвалидация должна
// убирать вектор в той же операции, что и payload.
type Store interface {
	Lookup(ctx context.Context, promptHash string, vec []float32, k int) ([]Candidate, error)
	Put(ctx context.Context, e Entry) error
	InvalidateTags(ctx context.Context, tags []string) (removed int, err error)
}
