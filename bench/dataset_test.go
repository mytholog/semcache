package main

import (
	"path/filepath"
	"testing"

	"github.com/mytholog/semcache/internal/dataset"
)

func TestPilotDatasetLoads(t *testing.T) {
	pairs, err := dataset.Load(filepath.Join("dataset", "pilot.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	got := make(map[string]int)
	for _, p := range pairs {
		got[p.Category]++
		if !p.HumanAuthored {
			t.Errorf("%s: expected human_authored=true on the pilot set", p.ID)
		}
	}

	for category := range dataset.Categories {
		if n := got[category]; n != 10 {
			t.Errorf("category %s: %d pairs, want 10", category, n)
		}
	}

	if len(pairs) != 80 {
		t.Errorf("total pairs = %d, want 80", len(pairs))
	}
}

func TestV1DatasetLoads(t *testing.T) {
	pairs, err := dataset.Load(filepath.Join("dataset", "v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pairs); n < 600 || n > 1000 {
		t.Errorf("v1 size = %d, want 600–1000", n)
	}

	var hand int
	got := make(map[string]int)
	for _, p := range pairs {
		got[p.Category]++
		if p.HumanAuthored {
			hand++
		}
	}
	if hand != 80 {
		t.Errorf("hand-written pairs = %d, want 80", hand)
	}
	for category := range dataset.Categories {
		if got[category] < 10 {
			t.Errorf("category %s: %d pairs, want at least the 10 gold ones", category, got[category])
		}
	}
}
