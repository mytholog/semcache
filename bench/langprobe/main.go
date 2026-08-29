// Command langprobe печатает решение определителя языка по строкам:
// нужно, чтобы выбирать защиту от коротких строк по числам, а не на глаз.
package main

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/mytholog/semcache/internal/verify/lingua"
)

func main() {
	texts := []string{
		"reset password",
		"reset my password",
		"password reset",
		"api key",
		"how to reset password",
		"cancel subscription",
		"How do I reset my password?",
		"Wie aktiviere ich 2FA?",
		"Jak zresetować hasło?",
		"Come reimposto la password?",
		"¿Cómo elimino mi cuenta?",
		"Wo ist die Statusseite?",
		"APIキーの作成方法は？",
		"Как сбросить пароль?",
	}

	c := lingua.New(nil)
	fmt.Printf("%-32s %-6s %6s %7s %6s %7s\n", "text", "lang", "words", "letters", "top", "margin")
	for _, t := range texts {
		lang, top, margin := c.Spread(t)
		fmt.Printf("%-32s %-6s %6d %7d %6.3f %7.3f\n", t, lang, len(strings.Fields(t)), letters(t), top, margin)
	}

	fmt.Printf("\n%-30s %-30s %8s %8s %-6s %s\n", "a", "b", "cross-a", "cross-b", "same", "confident")
	pairs := [][2]string{
		{"reset password", "How do I reset my password?"},
		{"api key", "How do I create an API key?"},
		{"How do I reset my password?", "Wie aktiviere ich 2FA?"},
		{"How do I reset my password?", "Come reimposto la password?"},
		{"How do I reset my password?", "¿Cómo restablezco mi contraseña?"},
		{"How do I export logs?", "Wie exportiere ich Logs?"},
		{"Wie aktiviere ich 2FA?", "¿Cómo elimino mi cuenta?"},
		{"Jak zresetować hasło?", "Come reimposto la password?"},
		{"¿Cómo elimino mi cuenta?", "Como cancelo a assinatura?"},
		{"Wo ist die Statusseite?", "Jak anulować subskrypcję?"},
	}
	for _, p := range pairs {
		same, confident := c.SameLanguage(p[0], p[1])
		crossA, crossB := c.Cross(p[0], p[1])
		fmt.Printf("%-30s %-30s %8.3f %8.3f %-6v %v\n", p[0], p[1], crossA, crossB, same, confident)
	}
}

func letters(s string) int {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			n++
		}
	}
	return n
}
