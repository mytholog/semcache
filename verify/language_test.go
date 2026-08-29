package verify

import (
	"context"
	"testing"
)

// stubComparer возвращает заранее заданный ответ на пару в любом порядке.
type stubComparer struct {
	same      bool
	confident bool
}

func (s stubComparer) SameLanguage(string, string) (bool, bool) {
	return s.same, s.confident
}

type countingVerifier struct{ calls int }

func (c *countingVerifier) Interchangeable(context.Context, string, string) (Decision, error) {
	c.calls++
	return Decision{OK: true}, nil
}

func TestLanguageGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		comparer     stubComparer
		wantOK       bool
		wantNext     int
		wantRejected int
	}{
		{
			name:         "confidently different is rejected before stage two",
			comparer:     stubComparer{same: false, confident: true},
			wantOK:       false,
			wantNext:     0,
			wantRejected: 1,
		},
		{
			name:     "same language reaches the verifier",
			comparer: stubComparer{same: true, confident: true},
			wantOK:   true,
			wantNext: 1,
		},
		{
			name:     "unsure difference reaches the verifier",
			comparer: stubComparer{same: false, confident: false},
			wantOK:   true,
			wantNext: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			next := &countingVerifier{}
			gate := &LanguageGate{Comparer: tt.comparer, Next: next}

			d, err := gate.Interchangeable(context.Background(), "a", "b")
			if err != nil {
				t.Fatal(err)
			}
			if d.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v (reason %q)", d.OK, tt.wantOK, d.Reason)
			}
			if next.calls != tt.wantNext {
				t.Errorf("next calls = %d, want %d", next.calls, tt.wantNext)
			}
			if gate.Rejected != tt.wantRejected {
				t.Errorf("Rejected = %d, want %d", gate.Rejected, tt.wantRejected)
			}
		})
	}
}

func TestComparersUseFirstConfidentAnswer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		chain         Comparers
		wantSame      bool
		wantConfident bool
	}{
		{
			name:          "first confident wins over later comparers",
			chain:         Comparers{stubComparer{same: false, confident: true}, stubComparer{same: true, confident: true}},
			wantSame:      false,
			wantConfident: true,
		},
		{
			name:          "abstention falls through",
			chain:         Comparers{stubComparer{same: true, confident: false}, stubComparer{same: false, confident: true}},
			wantSame:      false,
			wantConfident: true,
		},
		{
			name:     "all abstain",
			chain:    Comparers{stubComparer{}, stubComparer{}},
			wantSame: true,
		},
		{
			name:     "empty chain abstains",
			chain:    nil,
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			same, confident := tt.chain.SameLanguage("a", "b")
			if same != tt.wantSame || confident != tt.wantConfident {
				t.Errorf("SameLanguage = (%v, %v), want (%v, %v)", same, confident, tt.wantSame, tt.wantConfident)
			}
		})
	}
}

func TestScriptComparer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		a, b          string
		wantSame      bool
		wantConfident bool
	}{
		{
			name:          "english against russian",
			a:             "How do I reset my password?",
			b:             "Как сбросить пароль?",
			wantConfident: true,
		},
		{
			name:          "english against japanese",
			a:             "How do I create an API key?",
			b:             "APIキーの作成方法は？",
			wantConfident: true,
		},
		{
			name:     "spanish against english is one script, no opinion",
			a:        "¿Cómo restablezco mi contraseña?",
			b:        "How do I reset my password?",
			wantSame: true,
		},
		{
			name:     "no letters at all",
			a:        "42?",
			b:        "Как сбросить пароль?",
			wantSame: true,
		},
		{
			name:          "latin noise inside cyrillic still reads as cyrillic",
			a:             "Как создать API-ключ?",
			b:             "How do I create an API key?",
			wantConfident: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			same, confident := ScriptComparer{}.SameLanguage(tt.a, tt.b)
			if same != tt.wantSame || confident != tt.wantConfident {
				t.Errorf("SameLanguage(%q, %q) = (%v, %v), want (%v, %v)",
					tt.a, tt.b, same, confident, tt.wantSame, tt.wantConfident)
			}
		})
	}
}
