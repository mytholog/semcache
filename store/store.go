package store

import (
	"context"
	"errors"
	"time"
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

	// Namespace изолирует записи, которые нельзя путать между собой, даже
	// если промпты взаимозаменяемы: разные модели, разные версии шаблона,
	// разные арендаторы. Ответ модели A на запрос к модели B — не попадание
	// кэша, а подмена.
	Namespace string

	// Lang — язык ответа, определённый при записи по полному тексту. Гейт
	// второй стадии на коротком запросе языка не знает; записанный язык знает.
	Lang string

	// ExpiresAt — срок жизни записи. Нулевое значение означает «без TTL»:
	// в этом дизайне запись убивает инвалидация по тегам, а не время.
	ExpiresAt time.Time
}

// Candidate — результат Lookup: запись и косинус к запросу.
type Candidate struct {
	Entry
	Score float64
}

// expired — общее для всех реализаций правило: истёкшую запись отдавать
// нельзя даже при точном совпадении хеша.
func expired(e Entry, now time.Time) bool {
	return !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now)
}

// Store владеет записями, векторами и тегами вместе: инвалидация должна
// убирать вектор в той же операции, что и payload.
type Store interface {
	// Lookup ищет только внутри namespace и никогда не возвращает истёкшие
	// записи. Вектор в кандидате возвращать не обязан: попадание кэша отдаёт
	// payload.
	Lookup(ctx context.Context, namespace, promptHash string, vec []float32, k int) ([]Candidate, error)
	Put(ctx context.Context, e Entry) error
	InvalidateTags(ctx context.Context, tags []string) (removed int, err error)
}
