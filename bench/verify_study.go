package main

import (
	"cmp"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/mytholog/semcache/internal/dataset"
	"github.com/mytholog/semcache/internal/verify"
	"golang.org/x/sync/errgroup"
)

func runVerify(ctx context.Context, cfg config) error {
	pairs, err := dataset.Load(cfg.dataset)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}

	names := splitModels(cfg.models)
	if len(names) != 1 {
		return fmt.Errorf("verify mode expects one embedding model, got %d", len(names))
	}

	scoredPairs, emb, err := embedScored(ctx, cfg, pairs, names[0])
	if err != nil {
		return err
	}

	gate := sweepLanguageGate(os.Stdout, scoredPairs, cfg.retrieveMin)

	sims, labels := splitScores(scoredPairs)
	noopOK := make([]bool, len(scoredPairs))
	for i := range noopOK {
		noopOK[i] = true
	}
	noop := verify.Evaluate(sims, labels, cfg.retrieveMin, noopOK)
	cosineRows := sweep(scoredPairs, cfg.from, cfg.to, cfg.step)

	if err := verify.ScriptExists(cfg.rerankScript); err != nil {
		return err
	}
	ce := verify.NewCrossEncoder(cfg.rerankModel, verify.CrossEncoderOptions{
		Script:   cfg.rerankScript,
		CacheDir: cfg.cacheDir,
		Timeout:  cfg.rerankTO,
	})
	cePairs := make([][2]string, len(scoredPairs))
	for i, p := range scoredPairs {
		cePairs[i] = [2]string{p.A, p.B}
	}
	slog.Info("scoring pairs with cross-encoder", "n", len(cePairs), "model", cfg.rerankModel)
	ceScores, err := ce.ScorePairs(ctx, cePairs)
	if err != nil {
		return err
	}

	ceRows := sweepVerifier(sims, labels, ceScores, cfg.retrieveMin, cfg.ceFrom, cfg.ceTo, cfg.ceStep)

	gateOK := languageGateMask(scoredPairs, gate)
	gatedCERows := sweepVerifier(sims, labels, maskScores(ceScores, gateOK), cfg.retrieveMin, cfg.ceFrom, cfg.ceTo, cfg.ceStep)

	var (
		judgeCounts verify.Counts
		judgeCost   verify.Cost
		judgeOK     []bool
		haveJudge   bool
	)
	if !cfg.skipJudge {
		jc, cost, ok, err := runJudge(ctx, cfg, scoredPairs, sims, labels)
		if err != nil {
			slog.Warn("llm judge skipped", "error", err)
		} else {
			judgeCounts, judgeCost, judgeOK, haveJudge = jc, cost, ok, true
		}
	}

	var gatedJudge verify.Counts
	if haveJudge {
		gatedJudge = verify.Evaluate(sims, labels, cfg.retrieveMin, andMask(judgeOK, gateOK))
	}

	reportVerify(os.Stdout, cfg, names[0], scoredPairs, emb, cosineRows, noop, ceRows, ceScores, haveJudge, judgeCounts, judgeCost, judgeOK)
	reportGate(os.Stdout, cfg, ceRows, gatedCERows, haveJudge, judgeCounts, gatedJudge)

	csvPath := filepath.Join(cfg.outDir, "verify-sweep.csv")
	if err := writeVerifyCSV(csvPath, cosineRows, ceRows, haveJudge, judgeCounts); err != nil {
		return err
	}
	slog.Info("verify sweep written", "path", csvPath)

	sweeps := []namedSweep{
		{name: "noop (cosine)", rows: cosineRows},
		{name: "cross-encoder", rows: ceRows},
	}
	if haveJudge {
		sweeps = append(sweeps, namedSweep{
			name: "llm-judge",
			rows: []row{countsToRow(1, judgeCounts)},
		})
	}
	svgPath := filepath.Join(cfg.outDir, "verify-frontier.svg")
	if err := writeFrontierSVG(svgPath, sweeps); err != nil {
		return err
	}
	slog.Info("verify plot written", "path", svgPath)
	return nil
}

func splitScores(pairs []scored) ([]float64, []bool) {
	sims := make([]float64, len(pairs))
	labels := make([]bool, len(pairs))
	for i, p := range pairs {
		sims[i] = p.sim
		labels[i] = p.Interchangeable
	}
	return sims, labels
}

// quantize держит порог на сетке в 1e-4: без этого накопленная ошибка шага
// даёт пороги вида 0.9500000000000001 и ломает поиск строки по значению.
func quantize(t float64) float64 {
	return math.Round(t*1e4) / 1e4
}

func sweepVerifier(sims []float64, labels []bool, scores []float64, retrieveMin, from, to, step float64) []row {
	seen := map[float64]bool{}
	var rows []row
	add := func(t float64) {
		t = quantize(t)
		if t < 0 || t > 1 || seen[t] {
			return
		}
		seen[t] = true
		ok := make([]bool, len(scores))
		for i, s := range scores {
			ok[i] = s >= t
		}
		rows = append(rows, countsToRow(t, verify.Evaluate(sims, labels, retrieveMin, ok)))
	}
	if step <= 0 {
		step = 0.05
	}
	for t := from; t <= to+1e-9; t += step {
		add(t)
	}
	// Сигмоида реранкера прижата к единице, поэтому у верхней границы нужна
	// сетка мельче шага: иначе «лучший порог» — артефакт разрешения.
	for _, t := range []float64{0.97, 0.98, 0.99, 0.995, 0.998, 0.999, 0.9995, 0.9999} {
		if t >= from-1e-9 && t <= to+1e-9 {
			add(t)
		}
	}
	slices.SortFunc(rows, func(a, b row) int {
		return cmp.Compare(a.threshold, b.threshold)
	})
	return rows
}

// printScoreSpread печатает квантили оценки кросс-энкодера по категориям среди
// пар, дошедших до второй стадии. Медианы показывают, есть ли зазор между
// классами; таблица порогов показывает только то, какой порог выиграл здесь.
func printScoreSpread(out *os.File, pairs []scored, scores []float64, retrieveMin float64) {
	groups := map[string][]float64{}
	labels := map[string]bool{}
	for i, p := range pairs {
		if p.sim < retrieveMin {
			continue
		}
		groups[p.Category] = append(groups[p.Category], scores[i])
		labels[p.Category] = p.Interchangeable
	}

	fmt.Fprintf(out, "\nCross-encoder score spread on retrieved pairs (sigmoid):\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  category\tlabel\tn\tp10\tmedian\tp90\tmax")
	for _, cat := range categoryOrder {
		vals := groups[cat]
		if len(vals) == 0 {
			continue
		}
		slices.Sort(vals)
		fmt.Fprintf(w, "  %s\t%v\t%d\t%.4f\t%.4f\t%.4f\t%.4f\n",
			cat, labels[cat], len(vals),
			quantileAt(vals, 0.10), quantileAt(vals, 0.50), quantileAt(vals, 0.90), vals[len(vals)-1])
	}
	w.Flush()
}

// falseHitExcluding считает ложные попадания без одной категории: у реранкера
// весь остаток ошибок сидит в language_switch, и агрегат это прячет.
func falseHitExcluding(pairs []scored, retrieveMin float64, ok []bool, skip string) (fp, negatives int) {
	for i, p := range pairs {
		if p.Interchangeable || p.Category == skip {
			continue
		}
		negatives++
		if p.sim >= retrieveMin && ok[i] {
			fp++
		}
	}
	return fp, negatives
}

func quantileAt(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * q)
	return sorted[i]
}

var categoryOrder = []string{
	"negation", "entity_swap", "numeric", "temporal", "scope", "language_switch",
	"paraphrase", "format_only",
}

func countsToRow(threshold float64, c verify.Counts) row {
	r := row{
		threshold: threshold,
		tp:        c.TP,
		fp:        c.FP,
		fn:        c.FN,
		tn:        c.TN,
		hitRate:   c.HitRate(),
		falseHit:  c.FalseHit(),
		precision: c.Precision(),
		f1:        c.F1(),
	}
	return r
}

func runJudge(ctx context.Context, cfg config, pairs []scored, sims []float64, labels []bool) (verify.Counts, verify.Cost, []bool, error) {
	do, err := newChatDo(cfg.baseURL, os.Getenv("OPENAI_API_KEY"), cfg.timeout)
	if err != nil {
		return verify.Counts{}, verify.Cost{}, nil, err
	}
	j := verify.NewJudge(verify.OpenAICompleter{
		Do:    do,
		Model: cfg.judgeModel,
	}, filepath.Join(cfg.cacheDir, "judge"))

	ok := make([]bool, len(pairs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, p := range pairs {
		if sims[i] < cfg.retrieveMin {
			continue
		}
		g.Go(func() error {
			d, err := j.Interchangeable(gctx, p.A, p.B)
			if err != nil {
				return fmt.Errorf("judge %s: %w", p.ID, err)
			}
			ok[i] = d.OK
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return verify.Counts{}, verify.Cost{}, nil, err
	}
	c := verify.Evaluate(sims, labels, cfg.retrieveMin, ok)
	cost := verify.Cost{
		Hits:            c.TP + c.FP,
		VerifyCalls:     c.VerifyCalls,
		VerifyCacheHits: j.CacheHits,
		JudgeTokens:     j.Tokens,
		ProviderUSD:     cfg.providerUSD,
		VerifyUSD:       j.USD(),
	}
	return c, cost, ok, nil
}

func reportVerify(
	out *os.File,
	cfg config,
	model string,
	pairs []scored,
	emb *embedder,
	cosineRows []row,
	noop verify.Counts,
	ceRows []row,
	ceScores []float64,
	haveJudge bool,
	judgeCounts verify.Counts,
	judgeCost verify.Cost,
	judgeOK []bool,
) {
	fmt.Fprintf(out, "\nTwo-stage verify study\n")
	fmt.Fprintf(out, "Dataset: %s | embed %s | retrieve-min %.2f\n", cfg.dataset, model, cfg.retrieveMin)
	fmt.Fprintf(out, "  pairs %d | embedded now %d, from cache %d | embed cost $%.4f\n",
		len(pairs), emb.misses, emb.hits, emb.costUSD())
	fmt.Fprintf(out, "  reranker %s | judge %s\n", cfg.rerankModel, cfg.judgeModel)
	fmt.Fprintf(out, "  provider $%.4f per cache hit (assumed completion cost)\n", cfg.providerUSD)

	fmt.Fprintf(out, "\nNoop at retrieve-min %.2f (today's cosine gate):\n", cfg.retrieveMin)
	printCounts(out, noop)
	printCost(out, verify.Cost{
		Hits:        noop.TP + noop.FP,
		VerifyCalls: noop.VerifyCalls,
		ProviderUSD: cfg.providerUSD,
	})

	cosineAtFloor := judge(cosineRows, cfg.minRecall)
	fmt.Fprintf(out, "\nNoop cosine sweep, recall floor %.0f%%: false-hit %.1f%% at θ=%.2f\n",
		cfg.minRecall*100, cosineAtFloor.falseHit*100, cosineAtFloor.atThreshold)

	fmt.Fprintf(out, "\nCross-encoder threshold sweep (retrieve then verify):\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  threshold\thit rate\tfalse hits\tprecision\tF1\tTP\tFP\tFN\tTN")
	for _, r := range ceRows {
		fmt.Fprintf(w, "  %.4f\t%.1f%%\t%.1f%% (%d/%d)\t%.1f%%\t%.3f\t%d\t%d\t%d\t%d\n",
			r.threshold, r.hitRate*100, r.falseHit*100, r.fp, r.fp+r.tn,
			r.precision*100, r.f1, r.tp, r.fp, r.fn, r.tn)
	}
	w.Flush()

	printScoreSpread(out, pairs, ceScores, cfg.retrieveMin)

	bestCE := judge(ceRows, cfg.minRecall)
	if !bestCE.feasible {
		fmt.Fprintf(out, "\nCross-encoder: no threshold keeps recall above %.0f%%\n", cfg.minRecall*100)
		return
	}
	best := rowAt(ceRows, bestCE.atThreshold)
	fmt.Fprintf(out, "\nCross-encoder at recall floor %.0f%%: false-hit %.1f%% at τ=%.4f (hit %.1f%%)\n",
		cfg.minRecall*100, bestCE.falseHit*100, bestCE.atThreshold, best.hitRate*100)
	printCost(out, verify.Cost{
		Hits:        best.tp + best.fp,
		VerifyCalls: noop.VerifyCalls,
		ProviderUSD: cfg.providerUSD,
	})
	fmt.Fprintf(out, "  (cross-encoder is local: verify USD = 0)\n")

	ceOK := okAt(ceScores, bestCE.atThreshold)
	printCategoryVerify(out, "cross-encoder @ floor", pairs, cfg.retrieveMin, ceOK)

	fp, neg := falseHitExcluding(pairs, cfg.retrieveMin, ceOK, "language_switch")
	fmt.Fprintf(out, "  excluding language_switch: false-hit %.1f%% (%d/%d)\n", ratio(fp, neg)*100, fp, neg)

	if haveJudge {
		fmt.Fprintf(out, "\nLLM judge (%s) at retrieve-min %.2f:\n", cfg.judgeModel, cfg.retrieveMin)
		printCounts(out, judgeCounts)
		printCost(out, judgeCost)
		printCategoryVerify(out, "llm-judge", pairs, cfg.retrieveMin, judgeOK)
		jfp, jneg := falseHitExcluding(pairs, cfg.retrieveMin, judgeOK, "language_switch")
		fmt.Fprintf(out, "  excluding language_switch: false-hit %.1f%% (%d/%d)\n", ratio(jfp, jneg)*100, jfp, jneg)
	}

	fmt.Fprintf(out, "\nCompare at ~%.0f%% hit rate:\n", cfg.minRecall*100)
	fmt.Fprintf(out, "  noop cosine     hit %.1f%%  false-hit %.1f%%\n",
		rowAt(cosineRows, cosineAtFloor.atThreshold).hitRate*100, cosineAtFloor.falseHit*100)
	fmt.Fprintf(out, "  cross-encoder   hit %.1f%%  false-hit %.1f%%  (τ=%.4f)\n",
		best.hitRate*100, bestCE.falseHit*100, bestCE.atThreshold)
	if haveJudge {
		fmt.Fprintf(out, "  llm-judge       hit %.1f%%  false-hit %.1f%%  verify share %.1f%%\n",
			judgeCounts.HitRate()*100, judgeCounts.FalseHit()*100, judgeCost.VerifyShare()*100)
	}
	fmt.Fprintln(out)
}

func printCounts(out *os.File, c verify.Counts) {
	fmt.Fprintf(out, "  hit %.1f%%  false-hit %.1f%% (%d/%d)  precision %.1f%%  F1 %.3f  TP %d FP %d FN %d TN %d  verify-calls %d\n",
		c.HitRate()*100, c.FalseHit()*100, c.FP, c.FP+c.TN, c.Precision()*100, c.F1(),
		c.TP, c.FP, c.FN, c.TN, c.VerifyCalls)
}

// printCost печатает экономику холодного прогона: verify-calls — вызовы второй
// стадии, cache-hits — сколько из них закрыл кэш решений, но стоимость всё равно
// считается как за полный прогон.
func printCost(out *os.File, c verify.Cost) {
	fmt.Fprintf(out, "  cost: hits %d  saved $%.4f  verify $%.4f  verify/saved %.1f%%  tokens %d  verify-calls %d  from cache %d\n",
		c.Hits, c.SavedUSD(), c.VerifyUSD, c.VerifyShare()*100, c.JudgeTokens, c.VerifyCalls, c.VerifyCacheHits)
}

func okAt(scores []float64, min float64) []bool {
	ok := make([]bool, len(scores))
	for i, s := range scores {
		ok[i] = s >= min
	}
	return ok
}

func rowAt(rows []row, threshold float64) row {
	for _, r := range rows {
		if math.Abs(r.threshold-threshold) < 1e-9 {
			return r
		}
	}
	return row{}
}

func printCategoryVerify(out *os.File, title string, pairs []scored, retrieveMin float64, ok []bool) {
	fmt.Fprintf(out, "\nPer-category (%s):\n", title)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  category\tlabel\tn\tretrieved\taccepted\tfalse/miss")
	type agg struct {
		n, retrieved, accepted int
		label                  bool
	}
	groups := map[string]*agg{}
	for i, p := range pairs {
		a := groups[p.Category]
		if a == nil {
			a = &agg{label: p.Interchangeable}
			groups[p.Category] = a
		}
		a.n++
		if p.sim < retrieveMin {
			continue
		}
		a.retrieved++
		if ok[i] {
			a.accepted++
		}
	}
	for _, cat := range categoryOrder {
		a, exists := groups[cat]
		if !exists {
			continue
		}
		note := "ok"
		switch {
		case !a.label && a.accepted > 0:
			note = fmt.Sprintf("FP %d", a.accepted)
		case a.label && a.accepted < a.n:
			note = fmt.Sprintf("FN %d", a.n-a.accepted)
		}
		fmt.Fprintf(w, "  %s\t%v\t%d\t%d\t%d\t%s\n", cat, a.label, a.n, a.retrieved, a.accepted, note)
	}
	w.Flush()
}

func writeVerifyCSV(path string, cosine, ce []row, haveJudge bool, judge verify.Counts) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"stage", "threshold", "hit_rate", "false_hit_rate", "precision", "f1", "tp", "fp", "fn", "tn"}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	write := func(stage string, rows []row) error {
		for _, r := range rows {
			rec := []string{
				stage,
				strconv.FormatFloat(r.threshold, 'f', 2, 64),
				strconv.FormatFloat(r.hitRate, 'f', 4, 64),
				strconv.FormatFloat(r.falseHit, 'f', 4, 64),
				strconv.FormatFloat(r.precision, 'f', 4, 64),
				strconv.FormatFloat(r.f1, 'f', 4, 64),
				strconv.Itoa(r.tp), strconv.Itoa(r.fp), strconv.Itoa(r.fn), strconv.Itoa(r.tn),
			}
			if err := w.Write(rec); err != nil {
				return fmt.Errorf("write csv row: %w", err)
			}
		}
		return nil
	}
	if err := write("noop-cosine", cosine); err != nil {
		return err
	}
	if err := write("cross-encoder", ce); err != nil {
		return err
	}
	if haveJudge {
		if err := write("llm-judge", []row{countsToRow(1, judge)}); err != nil {
			return err
		}
	}
	return w.Error()
}
