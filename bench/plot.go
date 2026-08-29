package main

import (
	"fmt"
	"math"
	"os"
	"strings"
)

type namedSweep struct {
	name string
	rows []row
}

// writeFrontierSVG рисует hit rate (x) против false-hit rate (y) по сетке порогов.
// Идеальный семантический кэш жил бы внизу справа; порог косинуса на этом датасете
// идёт по диагонали.
func writeFrontierSVG(path string, sweeps []namedSweep) error {
	const (
		width, height = 720.0, 520.0
		left, right   = 70.0, 24.0
		top, bottom   = 36.0, 56.0
	)
	plotW := width - left - right
	plotH := height - top - bottom
	x := func(v float64) float64 { return left + v*plotW }
	y := func(v float64) float64 { return top + (1-v)*plotH }

	colors := []string{"#1d4ed8", "#b45309", "#047857", "#be123c"}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="ui-sans-serif, system-ui, sans-serif" font-size="12">`+"\n", width, height, width, height)
	b.WriteString(`<rect width="100%" height="100%" fill="#fafafa"/>` + "\n")
	fmt.Fprintf(&b, `<text x="%.1f" y="22" font-size="16" font-weight="600">Hit rate vs false-hit rate</text>`+"\n", left)
	b.WriteString(`<text x="360" y="508" text-anchor="middle">hit rate (interchangeable pairs)</text>` + "\n")
	b.WriteString(`<text transform="translate(18 260) rotate(-90)" text-anchor="middle">false-hit rate (near-miss pairs)</text>` + "\n")

	// Grid and axes.
	fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="#fff" stroke="#d4d4d4"/>`+"\n", left, top, plotW, plotH)
	for _, t := range []float64{0, 0.25, 0.5, 0.75, 1} {
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#eee"/>`+"\n", x(t), y(0), x(t), y(1))
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#eee"/>`+"\n", x(0), y(t), x(1), y(t))
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" fill="#666">%.0f%%</text>`+"\n", x(t), y(0)+18, t*100)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="end" fill="#666">%.0f%%</text>`+"\n", x(0)-8, y(t)+4, t*100)
	}

	for i, s := range sweeps {
		color := colors[i%len(colors)]
		var pts []string
		for _, r := range s.rows {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(r.hitRate), y(r.falseHit)))
		}
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`+"\n", color, strings.Join(pts, " "))
		if len(s.rows) == 0 {
			continue
		}
		// Mark the default-gateway threshold 0.95 if present.
		for _, r := range s.rows {
			if math.Abs(r.threshold-0.95) < 1e-9 {
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s"/>`+"\n", x(r.hitRate), y(r.falseHit), color)
				break
			}
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="%s">%s</text>`+"\n",
			left+8, top+18+float64(i)*16, color, s.name)
	}
	b.WriteString("</svg>\n")

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write svg: %w", err)
	}
	return nil
}
