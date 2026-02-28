package latency

import (
	"math"
	"sort"
	"sync"
	"time"
)

const ringSize = 1024

type Stats struct {
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	AvgMs float64 `json:"avg_ms"`
	Count int64   `json:"count"`
}

type bufferKey struct {
	vault     [8]byte
	operation string
}

type ringBuffer struct {
	samples [ringSize]float64
	pos     int
	filled  bool
	count   int64
	sum     float64
}

type Tracker struct {
	mu      sync.RWMutex
	buffers map[bufferKey]*ringBuffer
}

func New() *Tracker {
	return &Tracker{
		buffers: make(map[bufferKey]*ringBuffer),
	}
}

func (t *Tracker) Record(ws [8]byte, operation string, d time.Duration) {
	ms := float64(d.Nanoseconds()) / 1e6
	t.mu.Lock()
	key := bufferKey{vault: ws, operation: operation}
	rb, ok := t.buffers[key]
	if !ok {
		rb = &ringBuffer{}
		t.buffers[key] = rb
	}
	rb.samples[rb.pos] = ms
	rb.sum += ms
	rb.count++
	rb.pos++
	if rb.pos >= ringSize {
		rb.pos = 0
		rb.filled = true
	}
	t.mu.Unlock()
}

func (t *Tracker) For(ws [8]byte, operation string) Stats {
	t.mu.RLock()
	rb, ok := t.buffers[bufferKey{vault: ws, operation: operation}]
	if !ok {
		t.mu.RUnlock()
		return Stats{}
	}
	stats := computeStats(rb)
	t.mu.RUnlock()
	return stats
}

func (t *Tracker) Snapshot() map[[8]byte]map[string]Stats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[[8]byte]map[string]Stats)
	for key, rb := range t.buffers {
		if _, ok := result[key.vault]; !ok {
			result[key.vault] = make(map[string]Stats)
		}
		result[key.vault][key.operation] = computeStats(rb)
	}
	return result
}

func computeStats(rb *ringBuffer) Stats {
	n := rb.pos
	if rb.filled {
		n = ringSize
	}
	if n == 0 {
		return Stats{Count: rb.count}
	}
	sorted := make([]float64, n)
	copy(sorted, rb.samples[:n])
	sort.Float64s(sorted)
	return Stats{
		P50Ms: percentile(sorted, 0.50),
		P95Ms: percentile(sorted, 0.95),
		P99Ms: percentile(sorted, 0.99),
		AvgMs: rb.sum / float64(rb.count),
		Count: rb.count,
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper || upper >= len(sorted) {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
