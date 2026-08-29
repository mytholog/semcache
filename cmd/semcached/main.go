// Команда semcached — прокси, совместимый с OpenAI /v1/chat/completions, с
// семантическим кэшем перед провайдером.
//
// Кэш двухстадийный: близость по вектору отбирает кандидатов, верификатор
// решает, взаимозаменяемы ли промпты. Одного косинуса недостаточно — на
// размеченном наборе порог 0.90 отдаёт 37% неверных ответов.
//
// Без -dsn работает на памяти процесса: так проще всего посмотреть, как он
// себя ведёт. С -dsn данные живут в Postgres, а инвалидация по тегам убирает
// запись вместе с вектором.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	lingualib "github.com/pemistahl/lingua-go"

	"github.com/mytholog/semcache"
	"github.com/mytholog/semcache/embed"
	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
	"github.com/mytholog/semcache/verify/lingua"
)

type config struct {
	addr     string
	upstream string
	dsn      string
	schema   string

	embedURL   string
	embedModel string
	embedDims  int

	verifier   string
	judgeModel string
	judgeCache string

	retrieveMin float64
	k           int
	ttl         time.Duration
	namespace   string

	lang     bool
	failOpen bool

	workers      int
	queueDepth   int
	writeTimeout time.Duration

	maxBody     int64
	timeout     time.Duration
	shutdownMax time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", ":8080", "listen address")
	flag.StringVar(&cfg.upstream, "upstream", "https://api.openai.com/v1", "provider base URL")
	flag.StringVar(&cfg.dsn, "dsn", os.Getenv("SEMCACHE_DSN"), "Postgres DSN; empty means an in-process store")
	flag.StringVar(&cfg.schema, "schema", "", "Postgres schema for the cache tables; empty means public")

	flag.StringVar(&cfg.embedURL, "embed-url", "", "embeddings base URL; empty means -upstream")
	flag.StringVar(&cfg.embedModel, "embed-model", "text-embedding-3-small", "embedding model")
	flag.IntVar(&cfg.embedDims, "embed-dims", 0, "embedding dimensions; 0 means the model default")

	flag.StringVar(&cfg.verifier, "verifier", "judge", "second stage: judge or noop")
	flag.StringVar(&cfg.judgeModel, "judge-model", "gpt-4o-mini", "model for the judge")
	flag.StringVar(&cfg.judgeCache, "judge-cache", "", "directory for judge decisions; empty means memory only")

	flag.Float64Var(&cfg.retrieveMin, "retrieve-min", 0.70, "cosine threshold before the second stage")
	flag.IntVar(&cfg.k, "k", 5, "candidates to verify per lookup")
	flag.DurationVar(&cfg.ttl, "ttl", 0, "entry lifetime; 0 means tags are the only way out")
	flag.StringVar(&cfg.namespace, "namespace", "", "namespace prefix; the model name is always appended")

	flag.BoolVar(&cfg.lang, "lang", true, "detect the answer language and reject cross-language hits")
	flag.BoolVar(&cfg.failOpen, "fail-open", true, "on cache errors, still call the provider")

	flag.IntVar(&cfg.workers, "writers", 4, "background cache writers")
	flag.IntVar(&cfg.queueDepth, "write-queue", 256, "pending cache writes before dropping")
	flag.DurationVar(&cfg.writeTimeout, "write-timeout", 30*time.Second, "deadline for one cache write")

	flag.Int64Var(&cfg.maxBody, "max-body", 4<<20, "request and response size limit in bytes")
	flag.DurationVar(&cfg.timeout, "upstream-timeout", 2*time.Minute, "provider request timeout")
	flag.DurationVar(&cfg.shutdownMax, "shutdown-timeout", 20*time.Second, "graceful shutdown budget")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("semcached failed", "error", err)
		os.Exit(1)
	}
}

func run(cfg config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		// Ключ клиента годится для пересылки, но не для эмбеддингов: их
		// прокси считает сам, до того как увидит, кто платит.
		return errors.New("OPENAI_API_KEY is not set: the proxy needs it to embed prompts")
	}

	// Эмбеддинги и чат — разные провайдеры чаще, чем один: чат идёт туда, где
	// нужная модель, а эмбеддинги — туда, где они дешевле или локальны.
	embedURL := cfg.embedURL
	if embedURL == "" {
		embedURL = cfg.upstream
	}
	embedder, err := embed.NewOpenAI(embed.OpenAIConfig{
		APIKey:  apiKey,
		BaseURL: embedURL,
		Model:   cfg.embedModel,
		Dims:    cfg.embedDims,
		Timeout: cfg.timeout,
	})
	if err != nil {
		return err
	}

	// Размерность выясняется у самой модели, а не берётся из флага: схема
	// Postgres требует точного числа, а ошибиться в нём легко.
	dims, err := probeDims(ctx, embedder)
	if err != nil {
		return err
	}
	log.Info("embedder ready", "model", cfg.embedModel, "dims", dims)

	st, closeStore, err := openStore(ctx, cfg, dims, log)
	if err != nil {
		return err
	}
	defer closeStore()

	upstream := NewUpstream(cfg.upstream, apiKey, cfg.timeout, cfg.maxBody)

	var checker semcache.LangChecker
	if cfg.lang {
		checker = lingua.New(lingualib.AllLanguages())
	}

	verifier, err := newVerifier(cfg, upstream, log)
	if err != nil {
		return err
	}

	srv := &Server{
		Cache: &semcache.Cache{
			Store:       st,
			Embedder:    embedder,
			Verifier:    verifier,
			Lang:        checker,
			RetrieveMin: cfg.retrieveMin,
			K:           cfg.k,
			TTL:         cfg.ttl,
		},
		Upstream:        upstream,
		Metrics:         NewMetrics(),
		Log:             log,
		NamespacePrefix: cfg.namespace,
		MaxBody:         cfg.maxBody,
		FailOpen:        cfg.failOpen,
	}
	metrics := srv.Metrics
	srv.writes = newWriteQueue(cfg.workers, cfg.queueDepth, cfg.writeTimeout, func() {
		metrics.Inc("semcache_errors_total", `stage="write_queue_full"`)
		log.Warn("cache write dropped: queue is full")
	})
	defer srv.writes.close()

	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("semcached listening", "addr", cfg.addr, "upstream", cfg.upstream, "verifier", cfg.verifier)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdownMax)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func probeDims(ctx context.Context, e *embed.OpenAI) (int, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	vecs, err := e.Embed(probeCtx, []string{"semcache"})
	if err != nil {
		return 0, fmt.Errorf("probe embedding dimensions: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return 0, errors.New("probe embedding dimensions: empty vector")
	}
	return len(vecs[0]), nil
}

func openStore(ctx context.Context, cfg config, dims int, log *slog.Logger) (store.Store, func(), error) {
	if cfg.dsn == "" {
		log.Warn("no -dsn given: entries live in this process only")
		return store.NewMemory(), func() {}, nil
	}

	pg, err := store.OpenPostgres(ctx, cfg.dsn, dims, store.PostgresOptions{Schema: cfg.schema})
	if err != nil {
		return nil, nil, err
	}
	if err := pg.Migrate(ctx); err != nil {
		pg.Close()
		return nil, nil, err
	}
	log.Info("postgres store ready", "dims", dims)
	return pg, pg.Close, nil
}

func newVerifier(cfg config, upstream *Upstream, log *slog.Logger) (verify.Verifier, error) {
	switch cfg.verifier {
	case "noop":
		// Ровно тот режим, который отдаёт 37% неверных ответов на пороге
		// 0.90: полезен, чтобы увидеть разницу, а не как настройка.
		log.Warn("second stage disabled: cosine similarity alone is not a safe cache key")
		return verify.Noop{}, nil
	case "judge":
		judge := verify.NewJudge(verify.OpenAICompleter{
			Do:      upstream.ChatDo,
			Model:   cfg.judgeModel,
			BaseURL: cfg.upstream,
		}, cfg.judgeCache)
		return judge, nil
	default:
		return nil, fmt.Errorf("unknown verifier %q: want judge or noop", cfg.verifier)
	}
}
