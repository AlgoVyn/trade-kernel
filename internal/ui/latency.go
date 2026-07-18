package ui

import (
	"sort"
	"sync"
	"time"
)

// LatencyTracker keeps a ring of recent order round-trip durations and
// computes approximate percentiles for the status bar.
type LatencyTracker struct {
	mu    sync.Mutex
	buf   []time.Duration
	idx   int
	count int
}

// NewLatencyTracker creates a tracker holding the last n measurements.
func NewLatencyTracker(n int) *LatencyTracker {
	if n <= 0 {
		n = 256
	}
	return &LatencyTracker{buf: make([]time.Duration, n)}
}

// Record adds one measurement.
func (t *LatencyTracker) Record(d time.Duration) {
	t.mu.Lock()
	t.buf[t.idx] = d
	t.idx = (t.idx + 1) % len(t.buf)
	if t.count < len(t.buf) {
		t.count++
	}
	t.mu.Unlock()
}

// Percentiles returns (p50, p99) of recorded samples.
func (t *LatencyTracker) Percentiles() (time.Duration, time.Duration) {
	t.mu.Lock()
	vals := make([]time.Duration, t.count)
	copy(vals, t.buf[:t.count])
	t.mu.Unlock()
	if len(vals) == 0 {
		return 0, 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	q := func(p float64) time.Duration {
		i := int(p * float64(len(vals)-1))
		return vals[i]
	}
	return q(0.50), q(0.99)
}
