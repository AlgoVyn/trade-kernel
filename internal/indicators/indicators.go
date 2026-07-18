// Package indicators provides incremental O(1)-per-update technical
// indicators: SMA, EMA, and resettable VWAP.
package indicators

import "math"

// SMA is a simple moving average over a rolling window of values.
type SMA struct {
	n      int
	window []float64
	sum    float64
	idx    int
	count  int
}

// NewSMA creates an SMA over the last n values.
func NewSMA(n int) *SMA {
	if n <= 0 {
		n = 1
	}
	return &SMA{n: n, window: make([]float64, n)}
}

// Update feeds v and returns the current average, or NaN until n values
// have been seen.
func (s *SMA) Update(v float64) float64 {
	if s.count < s.n {
		s.count++
	} else {
		s.sum -= s.window[s.idx]
	}
	s.window[s.idx] = v
	s.sum += v
	s.idx = (s.idx + 1) % s.n
	return s.Value()
}

// Value returns the current average, or NaN if the window isn't full.
func (s *SMA) Value() float64 {
	if s.count < s.n {
		return math.NaN()
	}
	return s.sum / float64(s.n)
}

// Peek returns what Value would be after Update(v), without mutating.
func (s *SMA) Peek(v float64) float64 {
	if s.count < s.n {
		if s.count+1 < s.n {
			return math.NaN()
		}
		return (s.sum + v) / float64(s.n)
	}
	return (s.sum - s.window[s.idx] + v) / float64(s.n)
}

// Ready reports whether the window is full.
func (s *SMA) Ready() bool { return s.count >= s.n }

// EMA is an exponential moving average.
type EMA struct {
	k       float64
	value   float64
	started bool
}

// NewEMA creates an EMA with the given period (k = 2/(n+1)).
func NewEMA(n int) *EMA {
	if n <= 0 {
		n = 1
	}
	return &EMA{k: 2.0 / float64(n+1)}
}

// Update feeds v and returns the current EMA value.
func (e *EMA) Update(v float64) float64 {
	if !e.started {
		e.value = v
		e.started = true
		return e.value
	}
	e.value = v*e.k + e.value*(1-e.k)
	return e.value
}

// Value returns the current EMA value, or NaN before the first update.
func (e *EMA) Value() float64 {
	if !e.started {
		return math.NaN()
	}
	return e.value
}

// Peek returns what Value would be after Update(v), without mutating.
func (e *EMA) Peek(v float64) float64 {
	if !e.started {
		return v
	}
	return v*e.k + e.value*(1-e.k)
}

// VWAP is a cumulative volume-weighted average price, resettable at
// session boundaries.
type VWAP struct {
	pv  float64 // Σ price*volume
	vol float64 // Σ volume
}

// Reset clears the accumulator (called on session transitions).
func (v *VWAP) Reset() { v.pv, v.vol = 0, 0 }

// Update feeds one trade and returns the current VWAP.
func (v *VWAP) Update(price, volume float64) float64 {
	v.pv += price * volume
	v.vol += volume
	return v.Value()
}

// Value returns the current VWAP, or NaN with no volume seen.
func (v *VWAP) Value() float64 {
	if v.vol == 0 {
		return math.NaN()
	}
	return v.pv / v.vol
}

// Peek returns what Value would be after Update(price, volume).
func (v *VWAP) Peek(price, volume float64) float64 {
	pv := v.pv + price*volume
	vol := v.vol + volume
	if vol == 0 {
		return math.NaN()
	}
	return pv / vol
}
