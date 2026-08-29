package dataset

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Pair — пара промптов. Вопрос разметки: можно ли отдать на запрос A
// ответ, ранее сгенерированный для запроса B.
type Pair struct {
	ID              string `json:"id"`
	Category        string `json:"category"`
	A               string `json:"a"`
	B               string `json:"b"`
	Interchangeable bool   `json:"interchangeable"`
	HumanAuthored   bool   `json:"human_authored"`
	Source          string `json:"source,omitempty"`
	Note            string `json:"note,omitempty"`
}

// Categories задаёт таксономию и ожидаемую разметку: категория определяет
// метку, поэтому расхождение — это опечатка, а не мнение.
var Categories = map[string]bool{
	"negation":        false,
	"entity_swap":     false,
	"numeric":         false,
	"temporal":        false,
	"scope":           false,
	"paraphrase":      true,
	"format_only":     true,
	"language_switch": false,
}

// Load читает JSONL и валидирует его строго: на маленьком датасете любая
// ошибка разметки заметно двигает итоговые числа.
func Load(path string) ([]Pair, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open dataset: %w", err)
	}
	defer f.Close()

	var (
		pairs    []Pair
		problems []string
		seen     = make(map[string]int)
		scanner  = bufio.NewScanner(f)
		lineNo   int
	)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var p Pair
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", lineNo, err)
		}

		switch {
		case p.ID == "":
			problems = append(problems, fmt.Sprintf("line %d: empty id", lineNo))
		case p.A == "" || p.B == "":
			problems = append(problems, fmt.Sprintf("%s: empty prompt side", p.ID))
		case p.A == p.B:
			problems = append(problems, fmt.Sprintf("%s: identical sides", p.ID))
		}
		if prev, dup := seen[p.ID]; dup {
			problems = append(problems, fmt.Sprintf("%s: duplicate id, first seen on line %d", p.ID, prev))
		}
		seen[p.ID] = lineNo

		expected, known := Categories[p.Category]
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s: unknown category %q", p.ID, p.Category))
		case expected != p.Interchangeable:
			problems = append(problems, fmt.Sprintf(
				"%s: category %q implies interchangeable=%v, got %v",
				p.ID, p.Category, expected, p.Interchangeable))
		}

		pairs = append(pairs, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read dataset: %w", err)
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("dataset has %d problem(s):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
	if len(pairs) == 0 {
		return nil, fmt.Errorf("dataset %s is empty", path)
	}
	return pairs, nil
}

// Texts возвращает уникальные строки датасета — то, что нужно отправить в эмбеддер.
func Texts(pairs []Pair) []string {
	seen := make(map[string]struct{}, len(pairs)*2)
	out := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		for _, s := range []string{p.A, p.B} {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// WriteJSONL пишет пары по одной на строку, без HTML-escape.
func WriteJSONL(path string, pairs []Pair) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create dataset: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, p := range pairs {
		if err := enc.Encode(p); err != nil {
			return fmt.Errorf("encode %s: %w", p.ID, err)
		}
	}
	return nil
}
