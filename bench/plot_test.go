package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFrontierSVG(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "frontier.svg")
	sweeps := []namedSweep{{
		name: "toy",
		rows: []row{
			{threshold: 0.50, hitRate: 1, falseHit: 1},
			{threshold: 0.95, hitRate: 0.1, falseHit: 0.15},
		},
	}}
	if err := writeFrontierSVG(path, sweeps); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<polyline") {
		t.Fatal("svg missing polyline")
	}
}

func TestWriteRecallSVG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalidation-recall.svg")
	lines := []recallLine{
		{name: "eager tagged DELETE", shares: []float64{0.25, 0.99}, recall: []float64{0.98, 0.982}},
		{name: "TTL only", shares: []float64{0.25, 0.99}, recall: []float64{0.97, 0.073}},
	}
	if err := writeRecallSVG(path, lines, 5); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<polyline", "TTL only", "recall@5"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("svg does not contain %q", want)
		}
	}
}

func TestWriteFrontierSVGSinglePoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frontier.svg")
	sweeps := []namedSweep{{
		name: "llm-judge",
		rows: []row{{threshold: 1, hitRate: 0.97, falseHit: 0.049}},
	}}
	if err := writeFrontierSVG(path, sweeps); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<circle") {
		t.Fatal("single-point sweep is invisible: svg has no circle")
	}
}
