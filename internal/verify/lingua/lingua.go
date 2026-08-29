// Package lingua адаптирует github.com/pemistahl/lingua-go к verify.LangComparer.
//
// Пакет отдельный намеренно: lingua встраивает модели всех 75 языков через
// go:embed безусловно, что добавляет около 130 МБ к бинарю независимо от
// выбранного набора языков. Ядро кэша должно уметь линковаться без этого.
package lingua

import (
	"sync"

	lingua "github.com/pemistahl/lingua-go"
)

// Comparer решает, на одном ли языке две короткие строки.
//
// Правило асимметричное, и это главное в нём. Требовать уверенного определения
// с обеих сторон нельзя: у английского модель размазана, и «How do I reset my
// password?» получает en с отрывом 0.04 — законный английский вопрос выглядит
// как неуверенный. Зато короткий вопрос на неанглийском опознаётся с отрывом
// 0.71 и выше. Поэтому достаточно, чтобы одна сторона уверенно принадлежала
// языку L, а другая плохо объяснялась этим L.
//
// Отрыв от второго места важнее самой вероятности: keyword-фрагменты вроде
// «reset password» модель уверенно читает как итальянский по argmax, но отрыв
// у них не превышает 0.24 — именно он отделяет ошибку от факта.
type Comparer struct {
	detector lingua.LanguageDetector

	// MinMargin — минимальный отрыв верхнего языка от второго места, при
	// котором сторона считается опознанной.
	MinMargin float64

	// MaxCross — верхняя граница вероятности чужого языка, при которой строка
	// считается написанной не на нём. Держать заметно ниже 0.30: английский
	// текст получает в английском всего около 0.31, и более высокий порог
	// начал бы отбрасывать англо-английские пары. Значение подобрано свипом
	// на v1 (`make verify-study`).
	MaxCross float64

	mu    sync.Mutex
	cache map[string]topResult
}

type topResult struct {
	lang   lingua.Language
	margin float64
}

// DefaultLanguages — набор, встречающийся в датасете semcache. Ограничивать
// набор обязательно: чем больше языков в решении, тем хуже точность на
// короткой строке.
var DefaultLanguages = []lingua.Language{
	lingua.English,
	lingua.Russian,
	lingua.Japanese,
	lingua.Turkish,
	lingua.Italian,
	lingua.French,
	lingua.German,
	lingua.Portuguese,
	lingua.Polish,
	lingua.Spanish,
}

func New(languages []lingua.Language) *Comparer {
	if len(languages) == 0 {
		languages = DefaultLanguages
	}
	return &Comparer{
		detector: lingua.NewLanguageDetectorBuilder().
			FromLanguages(languages...).
			WithPreloadedLanguageModels().
			Build(),
		MinMargin: 0.30,
		MaxCross:  0.15,
		cache:     make(map[string]topResult),
	}
}

// SameLanguage уверен только в отрицательном ответе: гейт действует лишь по
// нему, а «языки совпадают» на строке из двух слов утверждать нечестно.
//
// Обе стороны обязаны быть опознаны уверенно. Асимметричное правило («одна
// сторона уверенно немецкая, значит пара разноязычная») ловит больше смен
// языка, но неотличимо ломается на keyword-запросах: «api key» модель читает
// как турецкий с отрывом 0.02, и уверенно английский «How do I create an API
// key?» честно сообщает, что он не турецкий — вместе это даёт отклонение
// законной пары. Разделить эти два случая порогом нельзя: и английский текст,
// и keyword-фрагмент дают размазанное распределение.
func (c *Comparer) SameLanguage(a, b string) (bool, bool) {
	langA, marginA := c.top(a)
	langB, marginB := c.top(b)
	if langA == lingua.Unknown || langB == lingua.Unknown {
		return true, false
	}
	if marginA < c.MinMargin || marginB < c.MinMargin {
		return true, false
	}
	if langA == langB {
		return true, true
	}

	// Ярлыки разные — проверяем, что это не спор двух близких языков об одной
	// строке: каждая сторона должна плохо объясняться языком другой.
	if c.notLanguage(a, langB) && c.notLanguage(b, langA) {
		return false, true
	}
	return true, false
}

func (c *Comparer) notLanguage(text string, lang lingua.Language) bool {
	return c.detector.ComputeLanguageConfidence(text, lang) <= c.MaxCross
}

// Detect возвращает ISO 639-1 верхнего языка — для отчётов и отладки.
func (c *Comparer) Detect(text string) (string, bool) {
	lang, margin := c.top(text)
	if lang == lingua.Unknown {
		return "", false
	}
	return lang.IsoCode639_1().String(), margin >= c.MinMargin
}

// Cross возвращает вероятность языка второй строки для первой и наоборот —
// для подбора порогов на данных.
func (c *Comparer) Cross(a, b string) (crossA, crossB float64) {
	langA, _ := c.top(a)
	langB, _ := c.top(b)
	if langA == lingua.Unknown || langB == lingua.Unknown {
		return 0, 0
	}
	return c.detector.ComputeLanguageConfidence(a, langB), c.detector.ComputeLanguageConfidence(b, langA)
}

// Spread возвращает вероятность верхнего языка и отрыв от второго места —
// для подбора порогов на данных.
func (c *Comparer) Spread(text string) (lang string, top, margin float64) {
	values := c.detector.ComputeLanguageConfidenceValues(text)
	if len(values) == 0 {
		return "", 0, 0
	}
	top, margin = values[0].Value(), values[0].Value()
	if len(values) > 1 {
		margin = values[0].Value() - values[1].Value()
	}
	return values[0].Language().IsoCode639_1().String(), top, margin
}

func (c *Comparer) top(text string) (lingua.Language, float64) {
	c.mu.Lock()
	if r, ok := c.cache[text]; ok {
		c.mu.Unlock()
		return r.lang, r.margin
	}
	c.mu.Unlock()

	r := topResult{lang: lingua.Unknown}
	if values := c.detector.ComputeLanguageConfidenceValues(text); len(values) > 0 {
		r.lang = values[0].Language()
		r.margin = values[0].Value()
		if len(values) > 1 {
			r.margin = values[0].Value() - values[1].Value()
		}
	}

	c.mu.Lock()
	c.cache[text] = r
	c.mu.Unlock()
	return r.lang, r.margin
}
