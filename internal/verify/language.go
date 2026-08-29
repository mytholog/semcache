package verify

import (
	"context"
	"unicode"
)

// LangComparer отвечает на единственный вопрос, который нужен кэшу: написаны ли
// два промпта на одном языке. Это заметно легче, чем назвать язык: на строке из
// пяти слов определитель может не решить между английским и итальянским, но
// уверенно сказать, что английский и русский — не одно и то же.
//
// Второе значение — уверенность. Гейт отклоняет пару только при confident=true,
// поэтому воздержание всегда безопасно: пара просто идёт на вторую стадию.
type LangComparer interface {
	SameLanguage(a, b string) (same bool, confident bool)
}

// LanguageGate отклоняет пару до второй стадии, если языки уверенно разные.
// Ответ на другом языке не взаимозаменяем никогда, и проверить это
// детерминированно дешевле и надёжнее, чем просить об этом модель: судья с
// явным требованием «same language» в рубрике всё равно пропускает такие пары.
type LanguageGate struct {
	Comparer LangComparer
	Next     Verifier

	Rejected int
}

func (g *LanguageGate) Interchangeable(ctx context.Context, incoming, cached string) (Decision, error) {
	if same, confident := g.Comparer.SameLanguage(incoming, cached); confident && !same {
		g.Rejected++
		return Decision{OK: false, Reason: "language mismatch"}, nil
	}
	return g.Next.Interchangeable(ctx, incoming, cached)
}

// Comparers — цепочка: возвращает первый уверенный ответ. Порядок имеет смысл
// от дешёвого к дорогому — сравнение систем письма бесплатно и точно, модель
// нужна только внутри одной системы письма.
type Comparers []LangComparer

func (cs Comparers) SameLanguage(a, b string) (bool, bool) {
	for _, c := range cs {
		if same, confident := c.SameLanguage(a, b); confident {
			return same, true
		}
	}
	return true, false
}

// ScriptComparer — сравнение без зависимостей: различает только систему письма.
// Уверен, когда системы письма разные (английский против русского или
// японского), и молчит внутри одной системы, где испанский от португальского
// не отличить.
type ScriptComparer struct{}

func (ScriptComparer) SameLanguage(a, b string) (bool, bool) {
	sa, okA := dominantScript(a)
	sb, okB := dominantScript(b)
	if !okA || !okB {
		return true, false
	}
	if sa != sb {
		return false, true
	}
	// Одна система письма ещё не означает один язык.
	return true, false
}

func dominantScript(text string) (string, bool) {
	counts := map[string]int{}
	total := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		var script string
		switch {
		case unicode.Is(unicode.Latin, r):
			script = "latin"
		case unicode.Is(unicode.Cyrillic, r):
			script = "cyrillic"
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			script = "cjk"
		case unicode.Is(unicode.Hangul, r):
			script = "hangul"
		case unicode.Is(unicode.Arabic, r):
			script = "arabic"
		case unicode.Is(unicode.Greek, r):
			script = "greek"
		case unicode.Is(unicode.Hebrew, r):
			script = "hebrew"
		default:
			continue
		}
		counts[script]++
		total++
	}
	if total == 0 {
		return "", false
	}

	best := ""
	for script, n := range counts {
		if n > counts[best] {
			best = script
		}
	}
	return best, true
}

var (
	_ LangComparer = ScriptComparer{}
	_ LangComparer = Comparers(nil)
	_ Verifier     = (*LanguageGate)(nil)
)
