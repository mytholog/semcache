package store

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Postgres — продакшн-путь: pgvector HNSW для поиска, теги в той же базе.
type Postgres struct {
	pool   *pgxpool.Pool
	dims   int
	schema string
}

var _ Store = (*Postgres)(nil)

// PostgresOptions — необязательные параметры подключения.
type PostgresOptions struct {
	// EFSearch задаёт hnsw.ef_search на каждое соединение. Ноль оставляет
	// значение pgvector по умолчанию (40). Влияет на recall, поэтому в бенчах
	// фиксируется явно.
	EFSearch int

	// IterativeScan включает итеративный обход индекса (pgvector 0.8+):
	// "strict_order" или "relaxed_order". Пустая строка — выключено, и тогда
	// фильтр по живым записям применяется уже после того, как индекс отдал
	// свои k кандидатов.
	IterativeScan string

	// Schema кладёт таблицы кэша в отдельную схему. Пустая строка — public.
	// Тесты дают каждому свою схему, чтобы гонять их параллельно на одной базе.
	Schema string

	// DisableSeqScan запрещает полный перебор. Нужно бенчам: на корпусе в
	// десятки тысяч записей планировщик иногда предпочитает Seq Scan, и тогда
	// сравнение упирается в размер таблицы вместо поведения ANN-поиска.
	DisableSeqScan bool

	MaxConns int32
}

// OpenPostgres подключается и настраивает соединения. Схему не применяет:
// это делает Migrate.
func OpenPostgres(ctx context.Context, dsn string, dims int, opts PostgresOptions) (*Postgres, error) {
	if dims <= 0 {
		return nil, fmt.Errorf("embedding dims must be positive, got %d", dims)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}

	// GUC расширения ставятся после подключения: в startup-пакете их ещё нет,
	// pgvector подгружается по первому обращению.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if opts.Schema != "" {
			stmt := "set search_path = " + quoteIdent(opts.Schema) + ", public"
			if _, err := conn.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("set search_path: %w", err)
			}
		}
		if opts.DisableSeqScan {
			if _, err := conn.Exec(ctx, "set enable_seqscan = off"); err != nil {
				return fmt.Errorf("disable seqscan: %w", err)
			}
		}
		if opts.EFSearch > 0 {
			if _, err := conn.Exec(ctx, "set hnsw.ef_search = "+strconv.Itoa(opts.EFSearch)); err != nil {
				return fmt.Errorf("set hnsw.ef_search: %w", err)
			}
		}
		if opts.IterativeScan != "" {
			if _, err := conn.Exec(ctx, "set hnsw.iterative_scan = "+quoteLiteral(opts.IterativeScan)); err != nil {
				return fmt.Errorf("set hnsw.iterative_scan: %w", err)
			}
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Postgres{pool: pool, dims: dims, schema: opts.Schema}, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// migrateLockID — произвольная константа для advisory-лока миграции.
const migrateLockID = 0x5e3cac4e

// Migrate применяет схему. Идемпотентна, но размерность вектора зашивается в
// таблицу при создании: сменить её можно только пересоздав entries.
//
// Вся DDL идёт под advisory-локом: `create extension if not exists` и
// `create index if not exists` не защищены от гонки и при одновременном
// старте нескольких инстансов падают на duplicate key в системных каталогах.
func (p *Postgres) Migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migrate: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", int64(migrateLockID)); err != nil {
		return fmt.Errorf("lock migration: %w", err)
	}
	if p.schema != "" {
		if _, err := tx.Exec(ctx, "create schema if not exists "+quoteIdent(p.schema)); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}
	sql := strings.ReplaceAll(schemaSQL, ":dims", strconv.Itoa(p.dims))
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrate: %w", err)
	}
	return nil
}

// DropSchema убирает схему целиком — для тестов, которые работают в своей.
func (p *Postgres) DropSchema(ctx context.Context) error {
	if p.schema == "" {
		return fmt.Errorf("refusing to drop the default schema")
	}
	if _, err := p.pool.Exec(ctx, "drop schema if exists "+quoteIdent(p.schema)+" cascade"); err != nil {
		return fmt.Errorf("drop schema: %w", err)
	}
	return nil
}

// Truncate чистит кэш целиком — нужен бенчам и тестам.
func (p *Postgres) Truncate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, "truncate entries cascade"); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	return nil
}

func (p *Postgres) Put(ctx context.Context, e Entry) error {
	if e.ID == "" || e.Hash == "" || len(e.Vector) == 0 {
		return ErrInvalidEntry
	}
	if len(e.Vector) != p.dims {
		return fmt.Errorf("%w: vector has %d dims, store expects %d", ErrInvalidEntry, len(e.Vector), p.dims)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin put: %w", err)
	}
	defer tx.Rollback(ctx)

	var expires any
	if !e.ExpiresAt.IsZero() {
		expires = e.ExpiresAt
	}
	const upsert = `
		insert into entries (id, prompt_hash, prompt, payload, lang, embedding, expires_at)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (id) do update set
			prompt_hash = excluded.prompt_hash,
			prompt      = excluded.prompt,
			payload     = excluded.payload,
			lang        = excluded.lang,
			embedding   = excluded.embedding,
			expires_at  = excluded.expires_at`
	if _, err := tx.Exec(ctx, upsert, e.ID, e.Hash, e.Prompt, e.Payload, e.Lang, vectorLiteral(e.Vector), expires); err != nil {
		return fmt.Errorf("upsert entry: %w", err)
	}

	// Теги переписываются целиком: набор зависимостей ответа при перезаписи
	// меняется, а лишний тег означает лишнюю инвалидацию.
	if _, err := tx.Exec(ctx, "delete from entry_tags where entry_id = $1", e.ID); err != nil {
		return fmt.Errorf("clear tags: %w", err)
	}
	if len(e.Tags) > 0 {
		const insertTags = `
			insert into entry_tags (entry_id, tag)
			select $1, unnest($2::text[])
			on conflict do nothing`
		if _, err := tx.Exec(ctx, insertTags, e.ID, e.Tags); err != nil {
			return fmt.Errorf("insert tags: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit put: %w", err)
	}
	return nil
}

// PutBatch заливает записи пачками в одной транзакции на пачку. Нужен
// бенчам и первичному прогреву кэша: отдельная транзакция на запись
// превращает загрузку корпуса в десятки минут round trip'ов.
func (p *Postgres) PutBatch(ctx context.Context, entries []Entry, chunk int) error {
	if chunk <= 0 {
		chunk = 500
	}
	for batch := range slices.Chunk(entries, chunk) {
		if err := p.putChunk(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (p *Postgres) putChunk(ctx context.Context, entries []Entry) error {
	b := &pgx.Batch{}
	for _, e := range entries {
		if e.ID == "" || e.Hash == "" || len(e.Vector) == 0 {
			return ErrInvalidEntry
		}
		if len(e.Vector) != p.dims {
			return fmt.Errorf("%w: entry %s has %d dims, store expects %d", ErrInvalidEntry, e.ID, len(e.Vector), p.dims)
		}

		var expires any
		if !e.ExpiresAt.IsZero() {
			expires = e.ExpiresAt
		}
		b.Queue(`
			insert into entries (id, prompt_hash, prompt, payload, lang, embedding, expires_at)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (id) do update set
				prompt_hash = excluded.prompt_hash,
				prompt      = excluded.prompt,
				payload     = excluded.payload,
				lang        = excluded.lang,
				embedding   = excluded.embedding,
				expires_at  = excluded.expires_at`,
			e.ID, e.Hash, e.Prompt, e.Payload, e.Lang, vectorLiteral(e.Vector), expires)
		b.Queue("delete from entry_tags where entry_id = $1", e.ID)
		if len(e.Tags) > 0 {
			b.Queue(`
				insert into entry_tags (entry_id, tag)
				select $1, unnest($2::text[])
				on conflict do nothing`, e.ID, e.Tags)
		}
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin batch: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	return nil
}

// lookupSQL держит оба слоя кэша в одном запросе: точное совпадение хеша и
// ANN-кандидатов.
//
// Условие живости повторяется в каждой ветке намеренно. Общий CTE выглядел бы
// чище, но CTE, на который ссылаются дважды, Postgres материализует — и тогда
// ветка ANN теряет HNSW и уходит в полный перебор. Это тихая потеря: запрос
// возвращает те же строки, только на два порядка медленнее.
const lookupSQL = `
	(
		select id, prompt_hash, prompt, payload, lang, expires_at,
		       1.0::float8 as score, 0 as src
		from entries
		where prompt_hash = $2 and (expires_at is null or expires_at > now())
		limit $3
	)
	union all
	(
		select id, prompt_hash, prompt, payload, lang, expires_at,
		       1 - (embedding <=> $1::vector) as score, 1 as src
		from entries
		where expires_at is null or expires_at > now()
		order by embedding <=> $1::vector
		limit $3
	)
	order by src, score desc`

// Lookup отдаёт обе стадии за один round trip: точное совпадение хеша и ANN
// кандидатов. Просроченные записи не возвращаются никогда — истёкший ответ
// нельзя отдать даже при точном совпадении.
//
// Вектор кандидата не возвращается: попадание кэша отдаёт payload, а не
// эмбеддинг, и тащить по 1536 float на каждого кандидата — это десятки
// килобайт на запрос ради данных, которые никто не читает.
func (p *Postgres) Lookup(ctx context.Context, promptHash string, vec []float32, k int) ([]Candidate, error) {
	if k <= 0 {
		return nil, nil
	}
	if len(vec) != p.dims {
		return nil, fmt.Errorf("%w: query vector has %d dims, store expects %d", ErrInvalidEntry, len(vec), p.dims)
	}

	rows, err := p.pool.Query(ctx, lookupSQL, vectorLiteral(vec), promptHash, k)
	if err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	defer rows.Close()

	out := make([]Candidate, 0, k)
	seen := make(map[string]struct{}, k)
	for rows.Next() {
		var (
			c       Candidate
			expires *time.Time
			src     int
		)
		if err := rows.Scan(&c.ID, &c.Hash, &c.Prompt, &c.Payload, &c.Lang, &expires, &c.Score, &src); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		if _, dup := seen[c.ID]; dup {
			continue
		}
		if expires != nil {
			c.ExpiresAt = *expires
		}
		seen[c.ID] = struct{}{}
		out = append(out, c)
		if len(out) == k {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lookup rows: %w", err)
	}
	return out, nil
}

// ExplainLookup объясняет ровно тот запрос, который выполняет Lookup. Иначе
// проверка «используется ли индекс» проверяет не то: на маленькой таблице
// планировщик берёт Seq Scan с точной сортировкой, и бенч измеряет полный
// перебор с идеальным recall, который в продакшне не воспроизведётся.
func (p *Postgres) ExplainLookup(ctx context.Context, vec []float32, k int) (string, error) {
	rows, err := p.pool.Query(ctx, "explain (analyze, buffers) "+lookupSQL, vectorLiteral(vec), "explain-no-such-hash", k)
	if err != nil {
		return "", fmt.Errorf("explain lookup: %w", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("scan plan: %w", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	return plan.String(), rows.Err()
}

// ScanMethod сообщает, чем на самом деле выполняется ANN-ветка. Проверять
// только «использован ли HNSW» недостаточно: у планировщика есть третий
// вариант — bitmap-скан по другому индексу, и он выглядит как идеальный
// recall, хотя означает, что векторный индекс обойдён.
func (p *Postgres) ScanMethod(ctx context.Context, vec []float32, k int) (method, plan string, err error) {
	plan, err = p.ExplainLookup(ctx, vec, k)
	if err != nil {
		return "", "", err
	}
	switch {
	case strings.Contains(plan, "entries_embedding_idx"):
		return "hnsw", plan, nil
	case strings.Contains(plan, "Bitmap"):
		return "bitmap", plan, nil
	case strings.Contains(plan, "Seq Scan"):
		return "seq", plan, nil
	default:
		return "other", plan, nil
	}
}

// InvalidateTags — центральная операция компонента: одна DELETE убирает
// payload, вектор и теги вместе. Никакой отложенной уборки, потому что
// ANN-поиск не умеет пропускать мёртвые записи — он про них не знает.
func (p *Postgres) InvalidateTags(ctx context.Context, tags []string) (int, error) {
	if len(tags) == 0 {
		return 0, nil
	}
	const q = `
		delete from entries
		where id in (select entry_id from entry_tags where tag = any($1::text[]))`
	tag, err := p.pool.Exec(ctx, q, tags)
	if err != nil {
		return 0, fmt.Errorf("invalidate tags: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ExpireTags помечает записи просроченными, не удаляя их: так ведёт себя
// кэш с одним TTL, и это то, с чем сравнивается eager-инвалидация.
func (p *Postgres) ExpireTags(ctx context.Context, tags []string) (int, error) {
	if len(tags) == 0 {
		return 0, nil
	}
	const q = `
		update entries set expires_at = now() - interval '1 hour'
		where id in (select entry_id from entry_tags where tag = any($1::text[]))
		  and (expires_at is null or expires_at > now())`
	tag, err := p.pool.Exec(ctx, q, tags)
	if err != nil {
		return 0, fmt.Errorf("expire tags: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ClearExpiry снимает TTL со всех записей — нужен бенчам, чтобы вернуть
// корпус в исходное состояние без перезаливки.
func (p *Postgres) ClearExpiry(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, "update entries set expires_at = null where expires_at is not null"); err != nil {
		return fmt.Errorf("clear expiry: %w", err)
	}
	return nil
}

// Stats — состояние хранилища: сколько записей живо, сколько просрочено и
// сколько места занимает индекс. Нужно, чтобы утверждения про «индекс
// уменьшился» были измеряемыми, а не декларативными.
type Stats struct {
	Rows      int
	Live      int
	Expired   int
	Tags      int
	IndexSize int64
	TableSize int64
}

func (p *Postgres) Stats(ctx context.Context) (Stats, error) {
	const q = `
		select
			(select count(*) from entries),
			(select count(*) from entries where expires_at is null or expires_at > now()),
			(select count(*) from entry_tags),
			pg_relation_size('entries_embedding_idx'),
			pg_table_size('entries')`
	var s Stats
	err := p.pool.QueryRow(ctx, q).Scan(&s.Rows, &s.Live, &s.Tags, &s.IndexSize, &s.TableSize)
	if err != nil {
		return Stats{}, fmt.Errorf("stats: %w", err)
	}
	s.Expired = s.Rows - s.Live
	return s, nil
}

// SetAutovacuum управляет autovacuum для таблицы entries. Нужен бенчам:
// массовая DELETE будит фоновый autovacuum, и он конкурирует за I/O ровно
// во время измерения задержек, из-за чего даже медиана уезжает на порядок.
func (p *Postgres) SetAutovacuum(ctx context.Context, enabled bool) error {
	stmt := fmt.Sprintf("alter table entries set (autovacuum_enabled = %t)", enabled)
	if _, err := p.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("set autovacuum: %w", err)
	}
	return nil
}

// Analyze обновляет статистику. Без неё планировщик оценивает селективность
// фильтра по живым записям по устаревшим данным и может уйти в полный
// перебор — с идеальным recall и задержкой на два порядка выше.
func (p *Postgres) Analyze(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, "analyze entries"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	return nil
}

// Vacuum возвращает место удалённых записей. Вынесено отдельно, потому что
// autovacuum асинхронен, а измерять размер индекса надо в известном состоянии.
func (p *Postgres) Vacuum(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, "vacuum (analyze) entries"); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	return nil
}

// vectorLiteral кодирует вектор в текстовый формат pgvector. Отдельная
// зависимость pgvector-go для этого не нужна: формат — «[1,2,3]».
func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec) * 8)
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(strconv.AppendFloat(nil, float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func parseVector(raw string) ([]float32, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]float32, len(parts))
	for i, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil, fmt.Errorf("parse vector element %d: %w", i, err)
		}
		out[i] = float32(v)
	}
	return out, nil
}

// quoteLiteral и quoteIdent нужны там, где значение нельзя передать
// параметром: GUC в SET и имя схемы в DDL.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
