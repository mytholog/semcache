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
