package semcache

import (
	"context"
	"fmt"

	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
)

// Виды исхода обращения к кэшу.
const (
	KindMiss     = "miss"     // ничего похожего не нашлось
	KindExact    = "exact"    // совпал хеш промпта
	KindVerified = "verified" // сосед по вектору признан взаимозаменяемым
	KindReject   = "reject"   // сосед был достаточно близок, но верификатор его отклонил
)

// TwoStage — retrieval по косинусу, затем верификатор взаимозаменяемости.
// Работает с готовыми векторами и хешами; обёртка, которая считает их сама,
// это Cache.
type TwoStage struct {
	Store    store.Store
	Verifier verify.Verifier

	// RetrieveMin — порог косинуса, ниже которого кандидат не доходит до
	// второй стадии. Ставится низко (0.70): отсеивать должен верификатор,
	// а не порог, иначе возвращается ровно та задача, которую он решает.
	RetrieveMin float64

	// K — сколько кандидатов забирать из store. Ноль означает 5.
	K int

	// Lang отклоняет кандидата, если входящий запрос уверенно написан не на
	// языке записанного ответа. Необязателен: без него язык проверяет только
	// вторая стадия, которой видны лишь два промпта.
	Lang LangChecker
}

// Get возвращает запись, вид исхода и косинус кандидата, который его дал.
func (t TwoStage) Get(ctx context.Context, req Query) (store.Entry, string, float64, error) {
	k := t.K
	if k <= 0 {
		k = 5
	}
	cands, err := t.Store.Lookup(ctx, req.Namespace, req.Hash, req.Vector, k)
	if err != nil {
		return store.Entry{}, KindMiss, 0, err
	}
	if len(cands) == 0 {
		return store.Entry{}, KindMiss, 0, nil
	}
	if cands[0].Hash == req.Hash {
		return cands[0].Entry, KindExact, 1, nil
	}

	for _, c := range cands {
		if c.Score < t.RetrieveMin {
			continue
		}
		if t.wrongLanguage(req.Prompt, c.Lang) {
			continue
		}
		d, err := t.Verifier.Interchangeable(ctx, req.Prompt, c.Prompt)
		if err != nil {
			return store.Entry{}, KindMiss, 0, fmt.Errorf("verify: %w", err)
		}
		if d.OK {
			return c.Entry, KindVerified, c.Score, nil
		}
	}
	if cands[0].Score >= t.RetrieveMin {
		return store.Entry{}, KindReject, cands[0].Score, nil
	}
	return store.Entry{}, KindMiss, 0, nil
}

// wrongLanguage — гейт по записанному языку. У кэшированной записи язык
// определён при записи по полному тексту ответа, поэтому здесь остаётся один
// вопрос: может ли входящий запрос быть на том же языке. Утверждение делается
// только отрицательное — ошибка в эту сторону стоит промаха, а не подмены.
func (t TwoStage) wrongLanguage(prompt, lang string) bool {
	if t.Lang == nil || lang == "" {
		return false
	}
	return t.Lang.NotLanguage(prompt, lang)
}
