// Package semcache — семантический кэш ответов LLM, который проверяет
// взаимозаменяемость промптов, а не только их близость.
//
// Косинусная близость не является безопасным ключом кэша: на размеченном
// наборе порог 0.90 отдаёт 37% неверных ответов, потому что «отключить
// двухфакторную аутентификацию» и «включить двухфакторную аутентификацию»
// лежат рядом в пространстве эмбеддингов. Поэтому поиск здесь двухстадийный:
// retrieval по вектору с намеренно низким порогом, затем верификатор, который
// видит оба промпта и решает, взаимозаменяемы ли они как ключи.
//
// Записи помечаются тегами того, от чего зависит ответ, и инвалидация по тегу
// удаляет payload вместе с вектором в одной транзакции. Измерения и обоснование
// решений — в docs/posts.
package semcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mytholog/semcache/store"
	"github.com/mytholog/semcache/verify"
)

// Embedder считает векторы для пачки текстов.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// LangChecker сообщает язык текста и умеет уверенно отрицать язык. Отрицание
// нужно отдельно: у длинного ответа язык определяется надёжно, у запроса из
// двух слов — нет, и единственное честное утверждение про короткий запрос это
// «на языке L он точно не написан».
type LangChecker interface {
	Detect(text string) (lang string, confident bool)
	NotLanguage(text, lang string) bool
}

// Cache — двухстадийный кэш с эмбеддером: считает вектор и хеш сам.
type Cache struct {
	Store    store.Store
	Embedder Embedder
	Verifier verify.Verifier

	// Lang заполняет язык записи при Put и отклоняет иноязычных кандидатов
	// при Get. Необязателен.
	Lang LangChecker

	// RetrieveMin — порог косинуса перед второй стадией. Ноль означает 0.70.
	RetrieveMin float64

	// K — сколько кандидатов проверять. Ноль означает 5.
	K int

	// TTL ограничивает срок жизни записи. Ноль означает «без срока»: записи
	// убивает инвалидация по тегам, а не время. TTL здесь — страховка от
	// того, что кто-то забыл проставить теги, а не основной механизм.
	TTL time.Duration
}

// Query — запрос к кэшу.
type Query struct {
	// Prompt — текст, по которому идёт поиск.
	Prompt string

	// Namespace изолирует записи, которые нельзя путать: модель, версия
	// шаблона, арендатор.
	Namespace string

	// Hash и Vector заполняет Cache; TwoStage принимает их готовыми.
	Hash   string
	Vector []float32
}

// Write — что положить в кэш.
type Write struct {
	// Prompt — ключ: по нему считается вектор и хеш.
	Prompt string

	// Payload — что отдавать при попадании. Для прокси это тело ответа
	// провайдера целиком, чтобы попадание было байт в байт тем же ответом.
	Payload string

	// Answer — текст ответа для определения языка. Пусто — берётся Payload.
	// Разделено потому, что по JSON-обёртке язык не определить.
	Answer string

	// Tags — всё, от чего зависит ответ: документы и их версии, версия
	// модели, версия шаблона. Инвалидация работает по ним.
	Tags []string

	// Lang задаёт язык явно, например из локали вызывающего. Пусто —
	// определяется по Answer, если задан LangChecker.
	Lang string

	Namespace string
}

// Result — исход обращения к кэшу.
type Result struct {
	Entry store.Entry

	// Kind — один из KindMiss, KindExact, KindVerified, KindReject.
	Kind string

	// Score — косинус кандидата, который дал этот исход.
	Score float64
}

// Hit сообщает, можно ли отдавать Entry.Payload.
func (r Result) Hit() bool {
	return r.Kind == KindExact || r.Kind == KindVerified
}

// ErrNoEmbedder возвращается, если Cache создан без эмбеддера.
var ErrNoEmbedder = errors.New("semcache: Embedder is required")

// Get ищет взаимозаменяемый закэшированный ответ.
func (c *Cache) Get(ctx context.Context, q Query) (Result, error) {
	if c.Embedder == nil {
		return Result{}, ErrNoEmbedder
	}
	vec, err := c.embedOne(ctx, q.Prompt)
	if err != nil {
		return Result{}, err
	}

	q.Vector = vec
	q.Hash = Hash(q.Prompt)
	entry, kind, score, err := c.twoStage().Get(ctx, q)
	if err != nil {
		return Result{}, err
	}
	return Result{Entry: entry, Kind: kind, Score: score}, nil
}

// Put кладёт ответ в кэш. Язык определяется здесь и один раз: в тексте ответа
// сигнала достаточно, в запросе из двух слов — нет.
func (c *Cache) Put(ctx context.Context, w Write) error {
	if c.Embedder == nil {
		return ErrNoEmbedder
	}
	if w.Prompt == "" || w.Payload == "" {
		return fmt.Errorf("%w: prompt and payload are required", store.ErrInvalidEntry)
	}

	vec, err := c.embedOne(ctx, w.Prompt)
	if err != nil {
		return err
	}

	lang := w.Lang
	if lang == "" && c.Lang != nil {
		text := w.Answer
		if text == "" {
			text = w.Payload
		}
		if detected, confident := c.Lang.Detect(text); confident {
			lang = detected
		}
	}

	hash := Hash(w.Prompt)
	e := store.Entry{
		ID:        EntryID(w.Namespace, w.Prompt),
		Namespace: w.Namespace,
		Prompt:    w.Prompt,
		Hash:      hash,
		Vector:    vec,
		Payload:   w.Payload,
		Tags:      w.Tags,
		Lang:      lang,
	}
	if c.TTL > 0 {
		e.ExpiresAt = time.Now().Add(c.TTL)
	}
	return c.Store.Put(ctx, e)
}

// InvalidateTags убирает все записи, зависящие от любого из тегов, вместе с их
// векторами.
func (c *Cache) InvalidateTags(ctx context.Context, tags []string) (int, error) {
	return c.Store.InvalidateTags(ctx, tags)
}

func (c *Cache) twoStage() TwoStage {
	retrieveMin := c.RetrieveMin
	if retrieveMin == 0 {
		retrieveMin = 0.70
	}
	v := c.Verifier
	if v == nil {
		v = verify.Noop{}
	}
	return TwoStage{
		Store:       c.Store,
		Verifier:    v,
		RetrieveMin: retrieveMin,
		K:           c.K,
		Lang:        c.Lang,
	}
}

func (c *Cache) embedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.Embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, fmt.Errorf("embed: got %d vectors for one text", len(vecs))
	}
	return vecs[0], nil
}

// Hash — ключ точного совпадения. Регистр и края не значат ничего для смысла
// запроса, поэтому в хеш не попадают; всё остальное попадает.
func Hash(prompt string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(prompt))))
	return hex.EncodeToString(sum[:])
}

// EntryID делает повторную запись того же промпта перезаписью, а не вторым
// экземпляром.
func EntryID(namespace, prompt string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + strings.ToLower(strings.TrimSpace(prompt))))
	return hex.EncodeToString(sum[:])
}
