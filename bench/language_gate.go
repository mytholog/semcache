package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/mytholog/semcache/verify"
	"github.com/mytholog/semcache/verify/lingua"
)

// gateStats — что языковой гейт делает с парами, дошедшими до второй стадии.
// Caught — отброшенные невзаимозаменяемые пары (польза), Lost — отброшенные
// взаимозаменяемые (цена).
type gateStats struct {
	name    string
	caught  int
	lost    int
	abstain int
}

// namedComparer — вариант гейта для сравнения на данных.
type namedComparer struct {
	name     string
	comparer verify.LangComparer
}

// gateVariants перечисляет варианты от бесплатного к дорогому. Цепочка стоит
// последней: сравнение систем письма отвечает точно и бесплатно, модель нужна
// только внутри латиницы.
func gateVariants() []namedComparer {
	return []namedComparer{
		{name: "script only", comparer: verify.ScriptComparer{}},
		{name: "lingua only", comparer: lingua.New(nil)},
		{name: "script + lingua", comparer: verify.Comparers{verify.ScriptComparer{}, lingua.New(nil)}},
	}
}

// sweepLanguageGate измеряет варианты гейта на парах, прошедших первую стадию.
func sweepLanguageGate(out *os.File, pairs []scored, retrieveMin float64) verify.LangComparer {
	variants := gateVariants()

	stats := make([]gateStats, len(variants))
	for i, v := range variants {
		stats[i] = gateStats{name: v.name}
		for _, p := range pairs {
			if p.sim < retrieveMin {
				continue
			}
			same, confident := v.comparer.SameLanguage(p.A, p.B)
			switch {
			case !confident:
				stats[i].abstain++
			case !same && p.Interchangeable:
				stats[i].lost++
			case !same:
				stats[i].caught++
			}
		}
	}

	fmt.Fprintf(out, "\nLanguage gate on retrieved pairs (lingua over %d languages):\n", len(lingua.DefaultLanguages))
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  gate\tcaught (near-miss)\tlost (interchangeable)\tno opinion")
	for _, st := range stats {
		fmt.Fprintf(w, "  %s\t%d\t%d\t%d\n", st.name, st.caught, st.lost, st.abstain)
	}
	w.Flush()

	// Гейт имеет право стоять перед второй стадией только если он не стоит
	// полноты: отклонение — это промах, а промахи мы платим деньгами.
	best := len(variants) - 1
	if stats[best].lost > 0 {
		fmt.Fprintf(out, "  %s costs %d interchangeable pairs, falling back to script only\n",
			stats[best].name, stats[best].lost)
		best = 0
	}
	fmt.Fprintf(out, "  using %q: catches %d near-miss pairs, costs %d interchangeable\n",
		stats[best].name, stats[best].caught, stats[best].lost)
	return variants[best].comparer
}

// reportGate печатает эффект гейта на обеих стадиях: только он закрывает
// категорию language_switch, которую ни косинус, ни верификаторы не держат.
func reportGate(out *os.File, cfg config, ceRows, gatedCERows []row, haveJudge bool, judgeCounts, gatedJudge verify.Counts) {
	fmt.Fprintf(out, "\nWith the language gate in front of stage two:\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  verifier\thit rate\tfalse hits\tprecision")

	ce := judge(ceRows, cfg.minRecall)
	gatedCE := judge(gatedCERows, cfg.minRecall)
	ceRow, gatedCERow := rowAt(ceRows, ce.atThreshold), rowAt(gatedCERows, gatedCE.atThreshold)
	fmt.Fprintf(w, "  cross-encoder\t%.1f%% → %.1f%%\t%.1f%% → %.1f%%\t%.1f%% → %.1f%%\n",
		ceRow.hitRate*100, gatedCERow.hitRate*100,
		ce.falseHit*100, gatedCE.falseHit*100,
		ceRow.precision*100, gatedCERow.precision*100)

	if haveJudge {
		fmt.Fprintf(w, "  llm-judge\t%.1f%% → %.1f%%\t%.1f%% → %.1f%%\t%.1f%% → %.1f%%\n",
			judgeCounts.HitRate()*100, gatedJudge.HitRate()*100,
			judgeCounts.FalseHit()*100, gatedJudge.FalseHit()*100,
			judgeCounts.Precision()*100, gatedJudge.Precision()*100)
	}
	w.Flush()
	fmt.Fprintln(out)
}

// languageGateMask возвращает для каждой пары ответ «гейт пропускает её дальше».
func languageGateMask(pairs []scored, c verify.LangComparer) []bool {
	pass := make([]bool, len(pairs))
	for i, p := range pairs {
		same, confident := c.SameLanguage(p.A, p.B)
		pass[i] = !(confident && !same)
	}
	return pass
}

// andMask комбинирует решение верификатора с решением гейта.
func andMask(ok, pass []bool) []bool {
	out := make([]bool, len(ok))
	for i := range ok {
		out[i] = ok[i] && pass[i]
	}
	return out
}

// maskScores обнуляет оценку у пар, отклонённых гейтом: для свипа порогов это
// то же самое, что «вторая стадия их не увидела».
func maskScores(scores []float64, pass []bool) []float64 {
	out := make([]float64, len(scores))
	for i, s := range scores {
		if pass[i] {
			out[i] = s
		}
	}
	return out
}
