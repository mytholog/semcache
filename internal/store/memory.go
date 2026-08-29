package store

import (
	"context"
	"math"
	"slices"
	"sync"
)

// Memory — жадное in-memory хранилище для бенчмарков и тестов:
// точный поиск по хешу, затем полный перебор косинуса.
type Memory struct {
	mu      sync.RWMutex
	entries map[string]Entry
	byHash  map[string][]string
	byTag   map[string]map[string]struct{}
}

var _ Store = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{
		entries: make(map[string]Entry),
		byHash:  make(map[string][]string),
		byTag:   make(map[string]map[string]struct{}),
	}
}

func (m *Memory) Put(_ context.Context, e Entry) error {
	if e.ID == "" || e.Hash == "" || len(e.Vector) == 0 {
		return ErrInvalidEntry
	}
	e = cloneEntry(e)

	m.mu.Lock()
	defer m.mu.Unlock()

	if old, ok := m.entries[e.ID]; ok {
		m.unindex(old)
	}
	m.entries[e.ID] = e
	m.byHash[e.Hash] = append(m.byHash[e.Hash], e.ID)
	for _, tag := range e.Tags {
		if m.byTag[tag] == nil {
			m.byTag[tag] = make(map[string]struct{})
		}
		m.byTag[tag][e.ID] = struct{}{}
	}
	return nil
}

func (m *Memory) Lookup(_ context.Context, promptHash string, vec []float32, k int) ([]Candidate, error) {
	if k <= 0 {
		return nil, nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := make(map[string]struct{})
	out := make([]Candidate, 0, k)

	for _, id := range m.byHash[promptHash] {
		e, ok := m.entries[id]
		if !ok {
			continue
		}
		out = append(out, Candidate{Entry: cloneEntry(e), Score: 1})
		seen[id] = struct{}{}
		if len(out) == k {
			return out, nil
		}
	}

	type scored struct {
		id    string
		score float64
	}
	rest := make([]scored, 0, len(m.entries))
	for id, e := range m.entries {
		if _, ok := seen[id]; ok {
			continue
		}
		rest = append(rest, scored{id: id, score: cosine(vec, e.Vector)})
	}
	slices.SortFunc(rest, func(a, b scored) int {
		if a.score == b.score {
			return 0
		}
		if a.score > b.score {
			return -1
		}
		return 1
	})
	for _, s := range rest {
		if len(out) == k {
			break
		}
		out = append(out, Candidate{Entry: cloneEntry(m.entries[s.id]), Score: s.score})
	}
	return out, nil
}

func (m *Memory) InvalidateTags(_ context.Context, tags []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := make(map[string]struct{})
	for _, tag := range tags {
		for id := range m.byTag[tag] {
			ids[id] = struct{}{}
		}
	}
	for id := range ids {
		if e, ok := m.entries[id]; ok {
			m.unindex(e)
			delete(m.entries, id)
		}
	}
	return len(ids), nil
}

func (m *Memory) unindex(e Entry) {
	if ids := m.byHash[e.Hash]; len(ids) > 0 {
		kept := ids[:0]
		for _, id := range ids {
			if id != e.ID {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(m.byHash, e.Hash)
		} else {
			m.byHash[e.Hash] = slices.Clone(kept)
		}
	}
	for _, tag := range e.Tags {
		delete(m.byTag[tag], e.ID)
		if len(m.byTag[tag]) == 0 {
			delete(m.byTag, tag)
		}
	}
}

func cloneEntry(e Entry) Entry {
	e.Vector = slices.Clone(e.Vector)
	e.Tags = slices.Clone(e.Tags)
	return e
}

func cosine(a, b []float32) float64 {
	n := min(len(a), len(b))
	var dot, na, nb float64
	for i := range n {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
