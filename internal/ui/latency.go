package ui

import (
	"sort"
	"sync"
	"time"
)

// LatencyTracker keeps a ring of recent order round-trip durations and
// computes approximate percentiles for the status bar.
//
// Percentiles are memoized: the status bar reads p50/p99 every render tick
// (10–30 Hz) but the values only change when a new sample lands. Record marks
// the cache dirty; Percentiles rebuilds only when dirty, otherwise returns the
// last-computed values without copying or sorting the ring.
type LatencyTracker struct {
	mu    sync.Mutex
	buf   []time.Duration
	idx   int
	count int

	// Cached percentiles — recomputed lazily when dirty is set by Record.
	dirty bool
	p50   time.Duration
	p99   time.Duration
}

// NewLatencyTracker creates a tracker holding the last n measurements.
func NewLatencyTracker(n int) *LatencyTracker {
	if n <= 0 {
		n = 256
	}
	// dirty starts true so the first Percentiles() on an empty ring still
	// computes (and caches) the (0, 0) result rather than serving garbage.
	return &LatencyTracker{buf: make([]time.Duration, n), dirty: true}
}

// Record adds one measurement and invalidates the cached percentiles.
func (t *LatencyTracker) Record(d time.Duration) {
	t.mu.Lock()
	t.buf[t.idx] = d
	t.idx = (t.idx + 1) % len(t.buf)
	if t.count < len(t.buf) {
		t.count++
	}
	t.dirty = true
	t.mu.Unlock()
}

// Percentiles returns (p50, p99) of recorded samples.
func (t *LatencyTracker) Percentiles() (p50, p99 time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.dirty {
		return t.p50, t.p99
	}
	t.dirty = false
	if t.count == 0 {
		t.p50, t.p99 = 0, 0
		return 0, 0
	}
	vals := make([]time.Duration, t.count)
	copy(vals, t.buf[:t.count])
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	q := func(p float64) time.Duration {
		i := int(p * float64(len(vals)-1))
		return vals[i]
	}
	t.p50, t.p99 = q(0.50), q(0.99)
	return t.p50, t.p99
}
