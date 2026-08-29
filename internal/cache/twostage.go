package cache

import (
	"context"
	"fmt"

	"github.com/mytholog/semcache/internal/store"
	"github.com/mytholog/semcache/internal/verify"
)

const (
	KindMiss     = "miss"
	KindExact    = "exact"
	KindVerified = "verified"
	KindReject   = "reject"
)

// TwoStage — retrieval по косинусу, затем верификатор взаимозаменяемости.
type TwoStage struct {
	Store       store.Store
	Verifier    verify.Verifier
	RetrieveMin float64
	K           int
}

func (t TwoStage) Get(ctx context.Context, incoming, promptHash string, vec []float32) (store.Entry, string, error) {
	k := t.K
	if k <= 0 {
		k = 5
	}
	cands, err := t.Store.Lookup(ctx, promptHash, vec, k)
	if err != nil {
		return store.Entry{}, KindMiss, err
	}
	if len(cands) == 0 {
		return store.Entry{}, KindMiss, nil
	}
	if cands[0].Hash == promptHash {
		return cands[0].Entry, KindExact, nil
	}

	for _, c := range cands {
		if c.Score < t.RetrieveMin {
			continue
		}
		d, err := t.Verifier.Interchangeable(ctx, incoming, c.Prompt)
		if err != nil {
			return store.Entry{}, KindMiss, fmt.Errorf("verify: %w", err)
		}
		if d.OK {
			return c.Entry, KindVerified, nil
		}
	}
	if cands[0].Score >= t.RetrieveMin {
		return store.Entry{}, KindReject, nil
	}
	return store.Entry{}, KindMiss, nil
}
