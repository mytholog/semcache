package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/mytholog/semcache/internal/dataset"
	"github.com/mytholog/semcache/store"
)

// corpusEntry — одна запись кэша в демо: промпт, вектор и теги-зависимости.
type corpusEntry struct {
	entry store.Entry
	doc   int
}

// scenario — измерение одного состояния хранилища.
type scenario struct {
	deadShare float64
	rows      int
	live      int
	recall    float64
	returned  float64
	latency   time.Duration
	indexSize int64

	// scan — чем выполнялась ANN-ветка. Без этого таблица нечитаема: точный
	// перебор даёт recall 1.000 и выглядит как победа, хотя означает, что
	// векторный индекс обойдён.
	scan string
}

// deadShares — доли мёртвых записей в свипе. Одна точка ничего не показывает:
// при небольшой доле запаса ef_search хватает, и разница появляется только
// когда живых кандидатов в окрестности запроса становится мало.
var deadShares = []float64{0.25, 0.50, 0.75, 0.90, 0.95, 0.99}

// runInvalidate измеряет то, ради чего один бэкенд владеет записями,
// векторами и тегами вместе: что ANN-поиск делает с мёртвыми записями,
// оставленными в индексе.
func runInvalidate(ctx context.Context, cfg config) error {
	if cfg.pgDSN == "" {
		return fmt.Errorf("-pg-dsn is required for -mode invalidate (try `make pg-up`)")
	}

	pairs, err := dataset.Load(cfg.dataset)
	if err != nil {
		return err
	}
	names := splitModels(cfg.models)
	if len(names) == 0 {
		return fmt.Errorf("no models in -models")
	}
	_, emb, err := embedScored(ctx, cfg, pairs, names[0])
	if err != nil {
		return err
	}

	// Корпус собирается из левых сторон пар, запросы — из правых: так запрос
	// не совпадает с записью и ANN-поиск действительно ищет.
	corpus, queries, err := buildCorpus(pairs, emb, cfg.docs, cfg.corpusSize)
	if err != nil {
		return err
	}
	dims := len(corpus[0].entry.Vector)

	fmt.Fprintf(os.Stdout, "\nInvalidation study\n")
	fmt.Fprintf(os.Stdout, "Dataset: %s | embed %s (%d dims) | corpus %d entries over %d docs | queries %d | k=%d, ef_search=%d\n",
		cfg.dataset, names[0], dims, len(corpus), cfg.docs, len(queries), cfg.k, cfg.efSearch)

	slog.Info("ranking corpus exactly for the recall reference")
	rankings := rankAll(corpus, queries)

	// Полный перебор запрещён: иначе на убывающей таблице планировщик уходит
	// в Seq Scan, получает recall 1.000 бесплатно, и сравнение оказывается про
	// размер таблицы, а не про то, что делают мёртвые записи с ANN-поиском.
	opts := store.PostgresOptions{
		EFSearch:       cfg.efSearch,
		MaxConns:       4,
		Schema:         cfg.pgSchema,
		DisableSeqScan: true,
	}
	pg, err := store.OpenPostgres(ctx, cfg.pgDSN, dims, opts)
	if err != nil {
		return err
	}
	defer pg.Close()
	if err := pg.Migrate(ctx); err != nil {
		return err
	}

	iterOpts := opts
	iterOpts.IterativeScan = "relaxed_order"
	iter, err := store.OpenPostgres(ctx, cfg.pgDSN, dims, iterOpts)
	if err != nil {
		return err
	}
	defer iter.Close()

	// Корпус заливается один раз: TTL-прогон снимается обратной UPDATE, а не
	// перезаливкой, иначе минуты уходят на вставку.
	if err := fillCorpus(ctx, pg, corpus); err != nil {
		return err
	}
	if err := pg.SetAutovacuum(ctx, false); err != nil {
		return err
	}
	defer func() {
		if err := pg.SetAutovacuum(context.WithoutCancel(ctx), true); err != nil {
			slog.Error("failed to re-enable autovacuum", "error", err)
		}
	}()
	scan, plan, err := pg.ScanMethod(ctx, "", queries[0].Vector, cfg.k)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "ANN branch executes as: %s\n", scan)
	if scan != "hnsw" {
		fmt.Fprintf(os.Stdout, "  the vector index is bypassed, so what follows is exact search, not ANN.\n"+
			"  raise -corpus-size to make the comparison meaningful. Plan:\n%s\n", plan)
	}

	intact, err := measure(ctx, pg, corpus, queries, rankings, nil, 0, cfg.k)
	if err != nil {
		return err
	}

	// Прогон 1: TTL. Записи мертвы, но лежат в индексе — так работают все
	// существующие семантические кэши.
	var lazy, lazyIter []scenario
	dead := make([]bool, len(corpus))
	for _, share := range deadShares {
		if err := markDead(ctx, pg, corpus, dead, share, false); err != nil {
			return err
		}
		s, err := measure(ctx, pg, corpus, queries, rankings, dead, share, cfg.k)
		if err != nil {
			return err
		}
		lazy = append(lazy, s)

		s, err = measure(ctx, iter, corpus, queries, rankings, dead, share, cfg.k)
		if err != nil {
			return err
		}
		lazyIter = append(lazyIter, s)
	}

	// Прогон 2: eager на том же корпусе. TTL снимается, записи удаляются по
	// тегу. Каждая доля измеряется дважды: сразу после DELETE и после VACUUM.
	// Разница между ними и есть цена того, что HNSW не убирает узлы удалённых
	// записей из графа сам.
	if err := pg.ClearExpiry(ctx); err != nil {
		return err
	}
	before, err := pg.Stats(ctx)
	if err != nil {
		return err
	}

	var (
		eager        []scenario
		vacuumed     []scenario
		removedTotal int
		deleteTook   time.Duration
		vacuumTook   time.Duration
	)
	dead = make([]bool, len(corpus))
	for _, share := range deadShares {
		start := time.Now()
		if err := markDead(ctx, pg, corpus, dead, share, true); err != nil {
			return err
		}
		deleteTook += time.Since(start)

		s, err := measure(ctx, pg, corpus, queries, rankings, dead, share, cfg.k)
		if err != nil {
			return err
		}
		eager = append(eager, s)
		removedTotal = len(corpus) - s.rows

		start = time.Now()
		if err := pg.Vacuum(ctx); err != nil {
			return err
		}
		vacuumTook += time.Since(start)

		s, err = measure(ctx, pg, corpus, queries, rankings, dead, share, cfg.k)
		if err != nil {
			return err
		}
		vacuumed = append(vacuumed, s)
	}

	afterVacuum, err := pg.Stats(ctx)
	if err != nil {
		return err
	}

	reportInvalidate(os.Stdout, cfg, reportData{
		intact:      intact,
		eager:       eager,
		vacuumed:    vacuumed,
		lazy:        lazy,
		lazyIter:    lazyIter,
		removed:     removedTotal,
		deleteTook:  deleteTook,
		vacuumTook:  vacuumTook,
		before:      before,
		afterVacuum: afterVacuum,
	})

	svgPath := filepath.Join(cfg.outDir, "invalidation-recall.svg")
	err = writeRecallSVG(svgPath, []recallLine{
		recallOf("eager tagged DELETE", eager),
		recallOf("TTL + iterative scan", lazyIter),
		recallOf("TTL only", lazy),
	}, cfg.k)
	if err != nil {
		return err
	}
	slog.Info("plot written", "path", svgPath)

	if cfg.pgKeep {
		slog.Info("leaving the corpus in place", "schema", cfg.pgSchema)
		return nil
	}
	return pg.Truncate(ctx)
}

func recallOf(name string, scenarios []scenario) recallLine {
	line := recallLine{name: name}
	for _, s := range scenarios {
		line.shares = append(line.shares, s.deadShare)
		line.recall = append(line.recall, s.recall)
	}
	return line
}

// markDead доводит долю мёртвых записей до share. Инкрементально: документы
// нумерованы, и растущая доля — это растущий набор изменённых документов.
func markDead(ctx context.Context, pg *store.Postgres, corpus []corpusEntry, dead []bool, share float64, remove bool) error {
	docs := 0
	for _, c := range corpus {
		docs = max(docs, c.doc+1)
	}
	cutoff := int(math.Round(share * float64(docs)))

	var tags []string
	for doc := range cutoff {
		tags = append(tags, fmt.Sprintf("doc:%d", doc))
	}
	for i, c := range corpus {
		if c.doc < cutoff {
			dead[i] = true
		}
	}

	var err error
	if remove {
		_, err = pg.InvalidateTags(ctx, tags)
	} else {
		_, err = pg.ExpireTags(ctx, tags)
	}
	if err != nil {
		return err
	}
	// Планировщик должен решать по актуальной статистике: именно от её
	// свежести зависит, пойдёт ли запрос в HNSW или в полный перебор.
	return pg.Analyze(ctx)
}

// buildCorpus раскладывает записи по документам по кругу: тогда мёртвые
// записи распределены по всему векторному пространству, а не собраны в один
// его угол, где их легко не заметить.
//
// Корпус добивается до size возмущениями реальных векторов. Без этого в
// таблице сотни строк, планировщик выбирает Seq Scan с точной сортировкой, и
// бенч измеряет полный перебор с идеальным recall вместо ANN-поиска. Шум
// вокруг настоящих промптов — заодно и реалистичная форма кэша: в нём много
// почти-дубликатов.
func buildCorpus(pairs []dataset.Pair, emb *embedder, docs, size int) ([]corpusEntry, []store.Entry, error) {
	if docs <= 0 {
		return nil, nil, fmt.Errorf("-docs must be positive")
	}

	seen := make(map[string]bool)
	var corpus []corpusEntry
	add := func(prompt string, vec []float32) {
		i := len(corpus)
		doc := i % docs
		corpus = append(corpus, corpusEntry{
			doc: doc,
			entry: store.Entry{
				ID:      fmt.Sprintf("e-%06d", i),
				Prompt:  prompt,
				Hash:    fmt.Sprintf("hash-%06d", i),
				Vector:  vec,
				Payload: "cached answer for " + prompt,
				Lang:    "en",
				Tags:    []string{fmt.Sprintf("doc:%d", doc), "model:" + emb.model},
			},
		})
	}

	var real [][]float32
	for _, p := range pairs {
		if seen[p.A] {
			continue
		}
		seen[p.A] = true

		vec, ok := emb.vector(p.A)
		if !ok {
			return nil, nil, fmt.Errorf("missing embedding for %q", p.A)
		}
		real = append(real, vec)
		add(p.A, vec)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	for len(corpus) < size {
		base := real[rng.IntN(len(real))]
		add(fmt.Sprintf("synthetic near-duplicate %d", len(corpus)), perturb(base, 0.15, rng))
	}

	var queries []store.Entry
	for _, p := range pairs {
		if seen[p.B] {
			continue
		}
		seen[p.B] = true
		vec, ok := emb.vector(p.B)
		if !ok {
			return nil, nil, fmt.Errorf("missing embedding for %q", p.B)
		}
		queries = append(queries, store.Entry{Prompt: p.B, Vector: vec})
	}

	if len(corpus) == 0 || len(queries) == 0 {
		return nil, nil, fmt.Errorf("corpus %d entries, %d queries: dataset too small", len(corpus), len(queries))
	}
	return corpus, queries, nil
}

// perturb сдвигает вектор гауссовым шумом и нормирует обратно: получается
// сосед исходного промпта, а не случайная точка, почти ортогональная всему.
func perturb(vec []float32, scale float64, rng *rand.Rand) []float32 {
	out := make([]float32, len(vec))
	for i, v := range vec {
		out[i] = v + float32(rng.NormFloat64()*scale/math.Sqrt(float64(len(vec))))
	}
	normalize(out)
	return out
}

func fillCorpus(ctx context.Context, pg *store.Postgres, corpus []corpusEntry) error {
	if err := pg.Truncate(ctx); err != nil {
		return err
	}

	entries := make([]store.Entry, 0, len(corpus))
	for _, c := range corpus {
		entries = append(entries, c.entry)
	}

	start := time.Now()
	if err := pg.PutBatch(ctx, entries, 200); err != nil {
		return fmt.Errorf("fill corpus: %w", err)
	}
	slog.Info("corpus loaded", "entries", len(entries), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// rankAll считает точный порядок всех записей для каждого запроса один раз.
// Удаление записей порядок не меняет, поэтому эталон для любой доли мёртвых —
// это тот же список с пропуском мёртвых.
func rankAll(corpus []corpusEntry, queries []store.Entry) [][]int32 {
	out := make([][]int32, len(queries))
	for qi, q := range queries {
		type scoredIdx struct {
			idx   int32
			score float64
		}
		all := make([]scoredIdx, len(corpus))
		for i, c := range corpus {
			all[i] = scoredIdx{idx: int32(i), score: similarity(q.Vector, c.entry.Vector)}
		}
		sort.Slice(all, func(a, b int) bool { return all[a].score > all[b].score })

		rank := make([]int32, len(all))
		for i, s := range all {
			rank[i] = s.idx
		}
		out[qi] = rank
	}
	return out
}

// measure считает recall@k против точного перебора по живым записям. Именно
// это число деградирует, когда мёртвые записи остаются в индексе.
func measure(ctx context.Context, pg *store.Postgres, corpus []corpusEntry, queries []store.Entry, rankings [][]int32, dead []bool, share float64, k int) (scenario, error) {
	index := make(map[string]int32, len(corpus))
	for i, c := range corpus {
		index[c.entry.ID] = int32(i)
	}

	live := len(corpus)
	for _, d := range dead {
		if d {
			live--
		}
	}

	// Прогрев: первый запрос после смены состояния платит за холодные
	// страницы индекса, и в среднем это даёт десятки миллисекунд шума.
	for _, q := range queries[:min(20, len(queries))] {
		if _, err := pg.Lookup(ctx, "", "", q.Vector, k); err != nil {
			return scenario{}, err
		}
	}

	var (
		recallSum   float64
		returnedSum int
		latencies   = make([]time.Duration, 0, len(queries))
	)
	for qi, q := range queries {
		want := topKLive(rankings[qi], dead, k)

		start := time.Now()
		// Пустой хеш промпта: точное совпадение не должно подменять ANN-поиск.
		got, err := pg.Lookup(ctx, "", "", q.Vector, k)
		latencies = append(latencies, time.Since(start))
		if err != nil {
			return scenario{}, err
		}

		returnedSum += len(got)
		hit := 0
		for _, c := range got {
			if slices.Contains(want, index[c.ID]) {
				hit++
			}
		}
		if len(want) > 0 {
			recallSum += float64(hit) / float64(len(want))
		}
	}

	st, err := pg.Stats(ctx)
	if err != nil {
		return scenario{}, err
	}
	scan, _, err := pg.ScanMethod(ctx, "", queries[0].Vector, k)
	if err != nil {
		return scenario{}, err
	}
	n := float64(len(queries))
	return scenario{
		deadShare: share,
		scan:      scan,
		rows:      st.Rows,
		live:      live,
		recall:    recallSum / n,
		returned:  float64(returnedSum) / n,
		latency:   median(latencies),
		indexSize: st.IndexSize,
	}, nil
}

// median устойчивее среднего: одна попавшая в прогон autovacuum-пауза сдвигает
// среднее на десятки миллисекунд и делает таблицу нечитаемой.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

func topKLive(rank []int32, dead []bool, k int) []int32 {
	out := make([]int32, 0, k)
	for _, idx := range rank {
		if len(out) == k {
			break
		}
		if dead == nil || !dead[idx] {
			out = append(out, idx)
		}
	}
	return out
}

// reportData собирает всё измеренное, чтобы у отчёта не было списка из
// десяти позиционных аргументов.
type reportData struct {
	intact      scenario
	eager       []scenario
	vacuumed    []scenario
	lazy        []scenario
	lazyIter    []scenario
	removed     int
	deleteTook  time.Duration
	vacuumTook  time.Duration
	before      store.Stats
	afterVacuum store.Stats
}

func reportInvalidate(out *os.File, cfg config, d reportData) {
	fmt.Fprintf(out, "\nIntact cache: recall@%d %.3f, %.2f candidates per lookup, %s median.\n",
		cfg.k, d.intact.recall, d.intact.returned, d.intact.latency.Round(10*time.Microsecond))
	fmt.Fprintf(out, "Eager invalidation removed %d entries and %d tag rows: %s of DELETE, %s of VACUUM.\n",
		d.removed, d.before.Tags-d.afterVacuum.Tags,
		d.deleteTook.Round(time.Millisecond), d.vacuumTook.Round(time.Millisecond))

	fmt.Fprintf(out, "\nRecall@%d over live entries as the dead share grows (exact search over the same live set = 1.000).\n", cfg.k)
	fmt.Fprintf(out, "Anything other than \"hnsw\" means the vector index was bypassed: exact, and orders of magnitude slower.\n")
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  dead\tlive\tDELETE\tDELETE+VACUUM\tTTL only\tTTL + iterative")
	for i := range d.eager {
		fmt.Fprintf(w, "  %.0f%%\t%d\t%.3f (%s)\t%.3f (%s)\t%.3f (%s)\t%.3f (%s)\n",
			d.eager[i].deadShare*100, d.eager[i].live,
			d.eager[i].recall, d.eager[i].scan,
			d.vacuumed[i].recall, d.vacuumed[i].scan,
			d.lazy[i].recall, d.lazy[i].scan,
			d.lazyIter[i].recall, d.lazyIter[i].scan)
	}
	w.Flush()

	fmt.Fprintf(out, "\nCandidates returned per lookup (k=%d; fewer means the index ran out of live neighbours):\n", cfg.k)
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  dead\tDELETE\tDELETE+VACUUM\tTTL only\tTTL + iterative")
	for i := range d.eager {
		fmt.Fprintf(w, "  %.0f%%\t%.2f\t%.2f\t%.2f\t%.2f\n",
			d.eager[i].deadShare*100,
			d.eager[i].returned, d.vacuumed[i].returned, d.lazy[i].returned, d.lazyIter[i].returned)
	}
	w.Flush()

	fmt.Fprintf(out, "\nMedian latency per lookup:\n")
	w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  dead\tDELETE\tDELETE+VACUUM\tTTL only\tTTL + iterative")
	for i := range d.eager {
		fmt.Fprintf(w, "  %.0f%%\t%s\t%s\t%s\t%s\n",
			d.eager[i].deadShare*100,
			d.eager[i].latency.Round(10*time.Microsecond),
			d.vacuumed[i].latency.Round(10*time.Microsecond),
			d.lazy[i].latency.Round(10*time.Microsecond),
			d.lazyIter[i].latency.Round(10*time.Microsecond))
	}
	w.Flush()

	fmt.Fprintf(out, "\nStorage: %d rows and %s of HNSW index before invalidation; %d rows and %s after deleting %.0f%% and vacuuming.\n\n",
		d.before.Rows, humanBytes(d.before.IndexSize),
		d.afterVacuum.Rows, humanBytes(d.afterVacuum.IndexSize),
		deadShares[len(deadShares)-1]*100)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f TB", v)
}
