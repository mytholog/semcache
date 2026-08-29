// Command bench измеряет, способен ли порог косинусной близости отличить
// взаимозаменяемые пары промптов от near-miss, где отдача кэша — молчаливая ошибка.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mytholog/semcache/internal/dataset"
)

type config struct {
	dataset   string
	models    string
	dims      int
	cacheDir  string
	baseURL   string
	script    string
	from      float64
	to        float64
	step      float64
	minRecall float64
	outDir    string
	timeout   time.Duration
	localTO   time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.dataset, "dataset", "bench/dataset/pilot.jsonl", "path to the JSONL dataset")
	flag.StringVar(&cfg.models, "models", "text-embedding-3-small", "comma-separated model ids: text-embedding-3-small,bge-m3,e5-large")
	flag.IntVar(&cfg.dims, "dimensions", 0, "embedding dimensions for OpenAI (0 = model default)")
	flag.StringVar(&cfg.cacheDir, "cache", "bench/.cache", "directory for the embedding cache")
	flag.StringVar(&cfg.baseURL, "base-url", "https://api.openai.com/v1", "OpenAI-compatible embeddings API base URL")
	flag.StringVar(&cfg.script, "local-script", "bench/embed_local.py", "Python sidecar for local models")
	flag.Float64Var(&cfg.from, "from", 0.50, "lowest threshold in the sweep")
	flag.Float64Var(&cfg.to, "to", 0.99, "highest threshold in the sweep")
	flag.Float64Var(&cfg.step, "step", 0.01, "threshold step")
	flag.Float64Var(&cfg.minRecall, "min-recall", 0.70, "recall on interchangeable pairs required for a threshold to count as usable")
	flag.StringVar(&cfg.outDir, "out", "bench/out", "directory for CSV and SVG artifacts")
	flag.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "per-request timeout for the OpenAI API")
	flag.DurationVar(&cfg.localTO, "local-timeout", 30*time.Minute, "timeout for one local embedding batch (model load included)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, cfg); err != nil {
		slog.Error("bench failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) error {
	pairs, err := dataset.Load(cfg.dataset)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}

	names := splitModels(cfg.models)
	if len(names) == 0 {
		return fmt.Errorf("no models in -models")
	}

	var sweeps []namedSweep
	for _, name := range names {
		res, err := runModel(ctx, cfg, pairs, name)
		if err != nil {
			return err
		}
		sweeps = append(sweeps, namedSweep{name: name, rows: res.rows})
		csvPath := filepath.Join(cfg.outDir, name+"-sweep.csv")
		if err := writeCSV(csvPath, res.rows); err != nil {
			return err
		}
		slog.Info("sweep written", "path", csvPath)
	}

	svgPath := filepath.Join(cfg.outDir, "frontier.svg")
	if err := writeFrontierSVG(svgPath, sweeps); err != nil {
		return err
	}
	slog.Info("plot written", "path", svgPath)
	return nil
}

type modelResult struct {
	rows []row
}

func runModel(ctx context.Context, cfg config, pairs []dataset.Pair, name string) (modelResult, error) {
	spec, err := resolveModel(name)
	if err != nil {
		return modelResult{}, err
	}

	var embed batchEmbed
	switch spec.kind {
	case "openai":
		client, err := newOpenAI(cfg.baseURL, os.Getenv("OPENAI_API_KEY"), spec.remote, cfg.dims, cfg.timeout)
		if err != nil {
			return modelResult{}, err
		}
		embed = client.embed
	case "local":
		if err := localScriptExists(cfg.script); err != nil {
			return modelResult{}, err
		}
		embed = newLocal(spec.remote, spec.prefix, cfg.script, cfg.localTO)
	default:
		return modelResult{}, fmt.Errorf("unsupported embedder kind %q", spec.kind)
	}

	emb, err := newCachedEmbedder(spec, cfg.dims, cfg.cacheDir, embed)
	if err != nil {
		return modelResult{}, err
	}
	if err := emb.embedAll(ctx, dataset.Texts(pairs)); err != nil {
		return modelResult{}, err
	}

	scoredPairs := make([]scored, 0, len(pairs))
	for _, p := range pairs {
		va, okA := emb.vector(p.A)
		vb, okB := emb.vector(p.B)
		if !okA || !okB {
			return modelResult{}, fmt.Errorf("missing embedding for pair %s", p.ID)
		}
		scoredPairs = append(scoredPairs, scored{Pair: p, sim: similarity(va, vb)})
	}

	rows := sweep(scoredPairs, cfg.from, cfg.to, cfg.step)
	if len(rows) == 0 {
		return modelResult{}, fmt.Errorf("empty sweep: check -from/-to/-step")
	}
	report(os.Stdout, cfg, spec.name, scoredPairs, rows, emb)
	return modelResult{rows: rows}, nil
}

func splitModels(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func report(out *os.File, cfg config, model string, pairs []scored, rows []row, emb *embedder) {
	var positives, negatives, handwritten int
	for _, p := range pairs {
		if p.Interchangeable {
			positives++
		} else {
			negatives++
		}
		if p.HumanAuthored {
			handwritten++
		}
	}

	fmt.Fprintf(out, "\nDataset: %s\n", cfg.dataset)
	fmt.Fprintf(out, "  pairs %d | interchangeable %d | not interchangeable %d | hand-written %d\n",
		len(pairs), positives, negatives, handwritten)
	fmt.Fprintf(out, "  model %s", model)
	if cfg.dims > 0 {
		fmt.Fprintf(out, " (%d dims)", cfg.dims)
	}
	fmt.Fprintf(out, " | embedded now %d, from cache %d | cost $%.4f\n",
		emb.misses, emb.hits, emb.costUSD())

	probes := []float64{0.90, 0.95, 0.98}
	fmt.Fprintf(out, "\nSimilarity by category (share of pairs at or above each threshold):\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  category\tinterchangeable\tn\tmin\tmedian\tmax\t>=0.90\t>=0.95\t>=0.98")
	for _, s := range byCategory(pairs, probes) {
		fmt.Fprintf(w, "  %s\t%v\t%d\t%.3f\t%.3f\t%.3f", s.category, s.interchangeable, s.n, s.min, s.median, s.max)
		for _, t := range probes {
			fmt.Fprintf(w, "\t%d/%d", s.aboveThreshold[t], s.n)
		}
		fmt.Fprintln(w)
	}
	w.Flush()

	fmt.Fprintf(out, "\nThreshold sweep:\n")
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  threshold\thit rate\tfalse hits\tprecision\tF1\tTP\tFP\tFN\tTN")
	for _, r := range rows {
		fmt.Fprintf(w, "  %.2f\t%.1f%%\t%.1f%% (%d/%d)\t%.1f%%\t%.3f\t%d\t%d\t%d\t%d\n",
			r.threshold, r.hitRate*100, r.falseHit*100, r.fp, r.fp+r.tn,
			r.precision*100, r.f1, r.tp, r.fp, r.fn, r.tn)
	}
	w.Flush()

	best := bestF1(rows)
	fmt.Fprintf(out, "\nBest F1 at threshold %.2f: hit rate %.1f%%, false hits %.1f%%\n",
		best.threshold, best.hitRate*100, best.falseHit*100)

	v := judge(rows, cfg.minRecall)
	fmt.Fprintf(out, "\nVerdict (recall floor %.0f%%):\n", v.minRecall*100)
	if v.feasible {
		fmt.Fprintf(out, "  best achievable false-hit rate %.1f%% (%d of %d) at threshold %.2f\n",
			v.falseHit*100, v.falseHitCount, v.negatives, v.atThreshold)
	}
	fmt.Fprintf(out, "  %s\n", v.conclusion)
	fmt.Fprintf(out, "\n  Granularity: with %d negative pairs, one pair is %.1f percentage points.\n\n",
		negatives, 100/float64(max(negatives, 1)))
}

func writeCSV(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"threshold", "hit_rate", "false_hit_rate", "precision", "f1", "tp", "fp", "fn", "tn"}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, r := range rows {
		rec := []string{
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
	return w.Error()
}
