package ui

import (
	"math"
	"testing"
	"time"

	"trade-kernel/internal/bars"
)

// After a session VWAP reset, live VWAP sits on the new session while older
// bars keep prior vwapAtClose. Soft-clip must not leave VWAP above max
// (that clamps every sample to y=0 → flat line on the top edge).
func TestVWAPNotPinnedToTopAfterReset(t *testing.T) {
	agg := bars.NewAggregator(9, 21)
	agg.SetVWAPAnchor("session")
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	hist := make([]bars.Bar, 40)
	for i := range hist {
		p := 148.0 + float64(i%10)*0.05
		hist[i] = bars.Bar{
			Start: base.Add(time.Duration(i) * time.Minute),
			Open: p, High: p + 0.2, Low: p - 0.2, Close: p, Volume: 1000, VWAP: p,
		}
	}
	agg.Load(bars.TF1m, hist)
	for i := 0; i < 30; i++ {
		agg.OnTrade("SOXL", 149+float64(i)*0.01, 50, base.Add(40*time.Minute+time.Duration(i)*time.Second))
	}
	agg.ResetVWAP()
	for i := 0; i < 20; i++ {
		agg.OnTrade("SOXL", 149.2+float64(i)*0.01, 20, base.Add(41*time.Minute+time.Duration(i)*time.Second))
	}

	snap := agg.Snapshot(bars.TF1m, 50, 0)
	min, max, ok := priceRange(snap, ChartOpts{ShowVWAP: true, ShowEMA: true, ShowEMA2: true})
	if !ok {
		t.Fatal("no range")
	}
	t.Logf("range=[%.4f, %.4f] n=%d", min, max, len(snap.Bars))

	for i, v := range snap.VWAP {
		if math.IsNaN(v) || v <= 0 {
			continue
		}
		if v < min || v > max {
			t.Errorf("vwap[%d]=%v outside scale [%v,%v]", i, v, min, max)
		}
	}
	last := snap.VWAP[len(snap.VWAP)-1]
	if math.IsNaN(last) || last <= 0 {
		t.Fatalf("forming vwap missing: %v", last)
	}
	// Map like yOfDot: value == max → row 0 (top). Being inside (min,max]
	// with room below means the line is not glued to the top edge alone.
	if last > max {
		t.Fatalf("forming vwap %v > scale max %v (clamps to top)", last, max)
	}
	// Forming tape is above the older 148s; scale must include it.
	if max < last {
		t.Fatalf("max %v < last vwap %v", max, last)
	}
}
