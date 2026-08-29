package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metrics — счётчики в формате Prometheus.
//
// Формат пишется руками намеренно. Здесь десяток счётчиков, а
// prometheus/client_golang тянет protobuf и procfs; спека проекта требует
// держать зависимости на минимуме, потому что код рассчитан на встраивание в
// чужой гейтвей, и каждая лишняя зависимость там — возражение на ревью.
type Metrics struct {
	mu       sync.Mutex
	counters map[string]map[string]float64 // имя -> метка -> значение
	help     map[string]string
}

func NewMetrics() *Metrics {
	m := &Metrics{
		counters: make(map[string]map[string]float64),
		help:     make(map[string]string),
	}
	m.help["semcache_requests_total"] = "Chat completion requests received."
	m.help["semcache_lookups_total"] = "Cache lookups by outcome."
	m.help["semcache_bypassed_total"] = "Requests that skipped the cache, by reason."
	m.help["semcache_upstream_requests_total"] = "Requests forwarded to the provider."
	m.help["semcache_upstream_errors_total"] = "Failed provider requests."
	m.help["semcache_errors_total"] = "Internal errors by stage."
	m.help["semcache_invalidated_entries_total"] = "Entries removed by tag invalidation."
	m.help["semcache_lookup_seconds_total"] = "Time spent in cache lookups."
	m.help["semcache_upstream_seconds_total"] = "Time spent waiting for the provider."
	return m
}

func (m *Metrics) Add(name, label string, delta float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters[name] == nil {
		m.counters[name] = make(map[string]float64)
	}
	m.counters[name][label] += delta
}

func (m *Metrics) Inc(name, label string) { m.Add(name, label, 1) }

func (m *Metrics) Observe(name, label string, d time.Duration) {
	m.Add(name, label, d.Seconds())
}

// Write печатает счётчики в текстовом формате Prometheus. Не WriteTo:
// io.WriterTo обещает другую подпись, а частичную запись здесь считать нечем.
func (m *Metrics) Write(w io.Writer) error {
	m.mu.Lock()
	snapshot := make(map[string]map[string]float64, len(m.counters))
	for name, byLabel := range m.counters {
		snapshot[name] = maps.Clone(byLabel)
	}
	m.mu.Unlock()

	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(snapshot)) {
		if help := m.help[name]; help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		}
		fmt.Fprintf(&b, "# TYPE %s counter\n", name)

		labels := slices.Collect(maps.Keys(snapshot[name]))
		sort.Strings(labels)
		for _, label := range labels {
			if label == "" {
				fmt.Fprintf(&b, "%s %g\n", name, snapshot[name][label])
				continue
			}
			fmt.Fprintf(&b, "%s{%s} %g\n", name, label, snapshot[name][label])
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
