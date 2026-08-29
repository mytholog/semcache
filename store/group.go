package store

import (
	"context"

	"golang.org/x/sync/singleflight"
)

// Group сворачивает одновременные промахи с одним prompt hash в один Put.
type Group struct {
	store Store
	sf    singleflight.Group
}

func NewGroup(s Store) *Group {
	return &Group{store: s}
}

// GetOrLoad сначала ищет точное совпадение хеша. При промахе loader
// вызывается один раз на хеш, даже если запросы пришли пачкой.
func (g *Group) GetOrLoad(ctx context.Context, namespace, promptHash string, vec []float32, loader func(context.Context) (Entry, error)) (Entry, bool, error) {
	hits, err := g.store.Lookup(ctx, namespace, promptHash, vec, 1)
	if err != nil {
		return Entry{}, false, err
	}
	if len(hits) > 0 && hits[0].Hash == promptHash {
		return hits[0].Entry, true, nil
	}

	// Ключ включает namespace: одинаковый промпт к разным моделям — это два
	// разных промаха, сворачивать их в один нельзя.
	v, err, _ := g.sf.Do(namespace+"\x00"+promptHash, func() (any, error) {
		// Повторная проверка после входа в singleflight: сосед мог уже положить запись.
		hits, err := g.store.Lookup(ctx, namespace, promptHash, vec, 1)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 && hits[0].Hash == promptHash {
			return hits[0].Entry, nil
		}
		e, err := loader(ctx)
		if err != nil {
			return nil, err
		}
		if e.Hash == "" {
			e.Hash = promptHash
		}
		if e.Namespace == "" {
			e.Namespace = namespace
		}
		if err := g.store.Put(ctx, e); err != nil {
			return nil, err
		}
		return e, nil
	})
	if err != nil {
		return Entry{}, false, err
	}
	return v.(Entry), false, nil
}
