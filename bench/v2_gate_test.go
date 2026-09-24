package main

import (
	"path/filepath"
	"testing"

	"github.com/mytholog/semcache/internal/dataset"
	"github.com/mytholog/semcache/verify/lingua"
)

// Языки, которые реально видит гейт. Китайский есть в датасете, но не в
// детекторе, и NotLanguage по нему всегда молчит.
var detectorLangs = map[string]bool{
	"en": true, "ru": true, "ja": true, "tr": true, "it": true,
	"fr": true, "de": true, "pt": true, "pl": true, "es": true,
}

func TestV2DatasetLoads(t *testing.T) {
	pairs, err := dataset.Load(filepath.Join("dataset", "v2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, p := range pairs {
		if p.Answer == "" || p.AnswerLang == "" {
			t.Fatalf("%s: v2 pair is missing the recorded answer", p.ID)
		}
		if p.Category == "paraphrase" || p.Category == "format_only" {
			if p.Source == "template" && len(p.ID) >= 3 && p.ID[:3] == "v2-" {
				seen[p.AnswerLang]++
			}
		}
	}
	for lang := range answersInV2() {
		if seen[lang] == 0 {
			t.Errorf("language %s: no same-language positive pairs", lang)
		}
	}
}

func answersInV2() map[string]bool {
	return map[string]bool{
		"ru": true, "de": true, "fr": true, "es": true, "ja": true,
		"zh": true, "pl": true, "it": true, "pt": true, "tr": true,
	}
}

func TestV2RecordedLanguageKeepsPositives(t *testing.T) {
	pairs, err := dataset.Load(filepath.Join("dataset", "v2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	c := lingua.New(nil)

	var positives, lost, switches, caught int
	for _, p := range pairs {
		if !detectorLangs[p.AnswerLang] {
			continue
		}
		switch p.Category {
		case "paraphrase", "format_only":
			positives++
			if c.NotLanguage(p.A, p.AnswerLang) || c.NotLanguage(p.B, p.AnswerLang) {
				lost++
				t.Errorf("%s (%s): recorded language %s rejected a positive", p.ID, p.AnswerLang, p.AnswerLang)
			}
		case "language_switch":
			switches++
			if c.NotLanguage(p.A, p.AnswerLang) {
				caught++
			}
		}
	}
	t.Logf("recorded-language gate: kept %d/%d positives, caught %d/%d language switches",
		positives-lost, positives, caught, switches)
	if positives == 0 || switches == 0 {
		t.Fatal("v2 did not contain both positives and switches inside the detector")
	}
}
