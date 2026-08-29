package lingua

import "testing"

func TestComparerSeparatesLanguages(t *testing.T) {
	t.Parallel()
	c := New(nil)

	// Пары между неанглийскими языками опознаются уверенно, включая трудную
	// испанский-португальский: у коротких вопросов на этих языках отрыв
	// верхнего языка от второго доходит до 0.98.
	pairs := [][2]string{
		{"Wie aktiviere ich 2FA?", "¿Cómo elimino mi cuenta?"},
		{"Jak zresetować hasło?", "Come reimposto la password?"},
		{"¿Cómo elimino mi cuenta?", "Como cancelo a assinatura?"},
		{"Wo ist die Statusseite?", "Jak anulować subskrypcję?"},
		{"Come reimposto la password?", "Wie kündige ich mein Abonnement?"},
	}
	for _, p := range pairs {
		same, confident := c.SameLanguage(p[0], p[1])
		if same || !confident {
			t.Errorf("SameLanguage(%q, %q) = (%v, %v), want (false, true)", p[0], p[1], same, confident)
		}
	}
}

// TestComparerAbstainsOnShortEnglish фиксирует известное ограничение, а не
// желаемое поведение: у английского распределение размазано (верхний язык с
// отрывом 0.04), поэтому пара «английский против европейского» уходит на
// вторую стадию. Отделить её от keyword-запроса порогом нельзя — см.
// TestComparerKeepsSameLanguagePairs. Это остаточная утечка language_switch,
// которую закрывает не определитель, а язык, записанный вместе с ответом.
func TestComparerAbstainsOnShortEnglish(t *testing.T) {
	t.Parallel()
	c := New(nil)

	pairs := [][2]string{
		{"How do I reset my password?", "Wie aktiviere ich 2FA?"},
		{"How do I reset my password?", "Come reimposto la password?"},
		{"How do I export logs?", "Wie exportiere ich Logs?"},
	}
	for _, p := range pairs {
		if _, confident := c.SameLanguage(p[0], p[1]); confident {
			t.Errorf("SameLanguage(%q, %q) is now confident; the gate got stronger, "+
				"re-measure the language_switch leak with `make verify-study`", p[0], p[1])
		}
	}
}

func TestComparerKeepsSameLanguagePairs(t *testing.T) {
	t.Parallel()
	c := New(nil)

	// Пары, которые кэш обязан пропускать на вторую стадию: это парафразы и
	// разница в форме, а не в языке. Keyword-фрагменты здесь принципиальны:
	// в них нет служебных слов, и по argmax модель уверенно читает «reset
	// password» как итальянский, а «api key» — как турецкий. Гейт не имеет
	// права им верить: отклонённая пара — это промах за деньги.
	pairs := [][2]string{
		{"How do I reset my password?", "How can I change my password?"},
		{"How do I reset my password?", "please reset my password, thanks!"},
		{"reset password", "How do I reset my password?"},
		{"password reset", "How do I reset my password?"},
		{"reset my password", "How do I reset my password?"},
		{"how to reset password", "How do I reset my password?"},
		{"api key", "How do I create an API key?"},
		{"cancel subscription", "How do I cancel my subscription?"},
		{"Wie aktiviere ich 2FA?", "Wie schalte ich die Zwei-Faktor-Authentifizierung ein?"},
		{"Как сбросить пароль?", "Как поменять пароль?"},
	}
	for _, p := range pairs {
		same, confident := c.SameLanguage(p[0], p[1])
		if confident && !same {
			t.Errorf("SameLanguage(%q, %q) rejected a same-language pair", p[0], p[1])
		}
	}
}

func TestComparerCaches(t *testing.T) {
	t.Parallel()
	c := New(nil)
	if _, _ = c.SameLanguage("Как сбросить пароль?", "Как поменять пароль?"); len(c.cache) != 2 {
		t.Fatalf("cache size = %d, want 2", len(c.cache))
	}
	if _, _ = c.SameLanguage("Как сбросить пароль?", "Как поменять пароль?"); len(c.cache) != 2 {
		t.Fatalf("cache grew on a repeated pair: %d", len(c.cache))
	}
}
