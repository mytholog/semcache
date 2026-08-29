package main

import (
	"math"
	"slices"

	"github.com/mytholog/semcache/internal/dataset"
)

// normalize приводит вектор к единичной длине. OpenAI отдаёт нормализованные векторы,
// но полагаться на это нельзя: харнесс должен одинаково работать с любым провайдером.
func normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
}

// similarity — косинус между нормализованными векторами, то есть скалярное произведение.
func similarity(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// scored — пара вместе с посчитанной близостью.
type scored struct {
	dataset.Pair
	sim float64
}

// row — одна строка сетки порогов.
//
// Положительный класс — пары, у которых ответы взаимозаменяемы: их кэш обязан ловить.
// Отрицательный класс — пары, у которых ответы не взаимозаменяемы: попадание здесь
// означает, что клиенту молча отдали неверный ответ.
type row struct {
	threshold float64
	tp, fn    int // взаимозаменяемые: поймано / упущено
	fp, tn    int // невзаимозаменяемые: ложное попадание / корректно отклонено
	hitRate   float64
	falseHit  float64
	precision float64
	f1        float64
}

func sweep(pairs []scored, from, to, step float64) []row {
	var rows []row
	for t := from; t <= to+1e-9; t += step {
		r := row{threshold: t}
		for _, p := range pairs {
			hit := p.sim >= t
			switch {
			case p.Interchangeable && hit:
				r.tp++
			case p.Interchangeable && !hit:
				r.fn++
			case !p.Interchangeable && hit:
				r.fp++
			default:
				r.tn++
			}
		}
		r.hitRate = ratio(r.tp, r.tp+r.fn)
		r.falseHit = ratio(r.fp, r.fp+r.tn)
		r.precision = ratio(r.tp, r.tp+r.fp)
		if r.precision+r.hitRate > 0 {
			r.f1 = 2 * r.precision * r.hitRate / (r.precision + r.hitRate)
		}
		rows = append(rows, r)
	}
	return rows
}

func ratio(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// categoryStats — распределение близости внутри категории. Агрегат по датасету
// прячет главное: категории ведут себя по-разному, и лечатся они тоже по-разному.
type categoryStats struct {
	category         string
	interchangeable  bool
	n                int
	min, median, max float64
	aboveThreshold   map[float64]int
}

func byCategory(pairs []scored, probes []float64) []categoryStats {
	groups := make(map[string][]scored)
	for _, p := range pairs {
		groups[p.Category] = append(groups[p.Category], p)
	}

	out := make([]categoryStats, 0, len(groups))
	for category, group := range groups {
		sims := make([]float64, 0, len(group))
		for _, p := range group {
			sims = append(sims, p.sim)
		}
		slices.Sort(sims)

		stats := categoryStats{
			category:        category,
			interchangeable: group[0].Interchangeable,
			n:               len(group),
			min:             sims[0],
			median:          sims[len(sims)/2],
			max:             sims[len(sims)-1],
			aboveThreshold:  make(map[float64]int, len(probes)),
		}
		for _, t := range probes {
			for _, s := range sims {
				if s >= t {
					stats.aboveThreshold[t]++
				}
			}
		}
		out = append(out, stats)
	}

	// Сначала невзаимозаменяемые категории — именно они интересны в этом исследовании.
	slices.SortFunc(out, func(a, b categoryStats) int {
		if a.interchangeable != b.interchangeable {
			if a.interchangeable {
				return 1
			}
			return -1
		}
		if a.median != b.median {
			if a.median > b.median {
				return -1
			}
			return 1
		}
		return 0
	})
	return out
}

// bestF1 — порог с максимальным F1: точка, которую выбрал бы наивный тюнинг.
func bestF1(rows []row) row {
	best := rows[0]
	for _, r := range rows[1:] {
		if r.f1 > best.f1 {
			best = r
		}
	}
	return best
}

// verdict — оценка премисы проекта, критерий зафиксирован в спеке до прогона:
// в диапазоне порогов, сохраняющих осмысленную полноту на взаимозаменяемых парах,
// смотрим минимально достижимую долю ложных попаданий.
type verdict struct {
	minRecall     float64
	feasible      bool
	atThreshold   float64
	falseHit      float64
	falseHitCount int
	negatives     int
	conclusion    string
}

func judge(rows []row, minRecall float64) verdict {
	v := verdict{minRecall: minRecall, falseHit: math.Inf(1)}
	for _, r := range rows {
		if r.hitRate < minRecall {
			continue
		}
		if r.falseHit < v.falseHit {
			v.feasible = true
			v.falseHit = r.falseHit
			v.falseHitCount = r.fp
			v.atThreshold = r.threshold
			v.negatives = r.fp + r.tn
		}
	}

	switch {
	case !v.feasible:
		v.falseHit = 0
		v.conclusion = "no threshold in the swept range keeps the required recall — widen the range or revisit the dataset"
	case v.falseHit >= 0.10:
		v.conclusion = "THESIS HOLDS: a threshold cannot separate the classes, so verification is the product"
	case v.falseHit >= 0.02:
		v.conclusion = "THESIS WEAK: pivot the headline to dependency-tagged invalidation, keep verification as a footnote"
	default:
		v.conclusion = "THESIS DEAD: the threshold separates the classes cleanly — stop here and move to ragd"
	}
	return v
}
