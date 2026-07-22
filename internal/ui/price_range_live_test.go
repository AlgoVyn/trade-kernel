package ui

import (
	"math"
	"testing"
	"time"

	"trade-kernel/internal/bars"
)

func TestPriceRangeIgnoresZeroOHLCAndIndicators(t *testing.T) {
	// Hand-built snapshot: real tape plus a corrupt zero low and a zero EMA.
	snap := bars.Snapshot{
		Bars: []bars.Bar{
			{Open: 148, High: 149, Low: 147, Close: 148.5, Volume: 100},
			{Open: 148.5, High: 149.2, Low: 148.0, Close: 149.0, Volume: 100},
			{Open: 0, High: 0, Low: 0, Close: 0, Volume: 1}, // corrupt
		},
		EMA:  []float64{148.2, 148.5, 0},
		EMA2: []float64{148.1, 148.4, math.NaN()},
		VWAP: []float64{148.3, 148.6, math.NaN()},
	}
	min, max, ok := priceRange(snap, ChartOpts{ShowEMA: true, ShowEMA2: true, ShowVWAP: true})
	if !ok {
		t.Fatal("expected ok range")
	}
	if min <= 0 {
		t.Fatalf("zero OHLC/EMA must not pin scale to origin: min=%v max=%v", min, max)
	}
	if max-min > 10 {
		t.Fatalf("range span %v too wide — zero low likely included", max-min)
	}
}

func TestPriceRangeSoftClipsIsolatedOutlierWick(t *testing.T) {
	// Mostly ~100 tape with one wild high wick (body stays at ~100) — clip.
	snap := bars.Snapshot{
		Bars: make([]bars.Bar, 20),
		EMA:  make([]float64, 20),
		EMA2: make([]float64, 20),
		VWAP: make([]float64, 20),
	}
	for i := 0; i < 20; i++ {
		p := 100.0 + float64(i%5)*0.1
		snap.Bars[i] = bars.Bar{Open: p, High: p + 0.2, Low: p - 0.2, Close: p, Volume: 10}
		snap.EMA[i], snap.EMA2[i], snap.VWAP[i] = p, p, p
	}
	// Pure wick: high explodes but open/close stay on the tape.
	snap.Bars[10].High = 500
	min, max, ok := priceRange(snap, ChartOpts{})
	if !ok {
		t.Fatal("expected ok")
	}
	if max >= 500 {
		t.Fatalf("outlier high 500 must soft-clip: max=%v", max)
	}
	if max < 100 || min > 101 {
		t.Fatalf("core tape lost: range=[%v,%v]", min, max)
	}
}

func TestPriceRangeKeepsBodyDrivenOneBarSpike(t *testing.T) {
	// One-bar news spike that closes at the high is real price action — keep it.
	snap := bars.Snapshot{
		Bars: make([]bars.Bar, 20),
		EMA:  make([]float64, 20),
		EMA2: make([]float64, 20),
		VWAP: make([]float64, 20),
	}
	for i := 0; i < 20; i++ {
		p := 100.0 + float64(i%5)*0.1
		snap.Bars[i] = bars.Bar{Open: p, High: p + 0.2, Low: p - 0.2, Close: p, Volume: 10}
		snap.EMA[i], snap.EMA2[i], snap.VWAP[i] = p, p, p
	}
	snap.Bars[10] = bars.Bar{Open: 100.2, High: 103.5, Low: 100.1, Close: 103.4, Volume: 500}
	min, max, ok := priceRange(snap, ChartOpts{})
	if !ok {
		t.Fatal("expected ok")
	}
	if max < 103.4 {
		t.Fatalf("body-driven spike must expand scale: max=%v (want ≥ 103.4)", max)
	}
	if min > 100 {
		t.Fatalf("older tape should remain: min=%v", min)
	}
}

func TestPriceRangeKeepsStepMoveAndVWAP(t *testing.T) {
	// Real step higher (several bars), not a single wick — scale must follow.
	// VWAP on the new level must stay inside the scale (not clamped to top).
	snap := bars.Snapshot{
		Bars: make([]bars.Bar, 24),
		EMA:  make([]float64, 24),
		EMA2: make([]float64, 24),
		VWAP: make([]float64, 24),
	}
	for i := 0; i < 20; i++ {
		p := 148.0 + float64(i%5)*0.05
		snap.Bars[i] = bars.Bar{Open: p, High: p + 0.15, Low: p - 0.15, Close: p, Volume: 10}
		snap.EMA[i], snap.EMA2[i], snap.VWAP[i] = p, p, 148.1
	}
	for i := 20; i < 24; i++ {
		p := 149.2 + float64(i-20)*0.05
		snap.Bars[i] = bars.Bar{Open: p, High: p + 0.1, Low: p - 0.05, Close: p, Volume: 10}
		snap.EMA[i], snap.EMA2[i], snap.VWAP[i] = p, p, 149.25
	}
	min, max, ok := priceRange(snap, ChartOpts{ShowVWAP: true, ShowEMA: true, ShowEMA2: true})
	if !ok {
		t.Fatal("expected ok")
	}
	if max < 149.25 {
		t.Fatalf("step + VWAP must expand scale: max=%v (want ≥ 149.25)", max)
	}
	if min > 148 {
		t.Fatalf("older tape should remain in scale: min=%v", min)
	}
}

func TestPriceRangeZeroPriceTradeDoesNotSquash(t *testing.T) {
	agg := bars.NewAggregator(9, 21)
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		agg.OnTrade("SOXL", 148.0, 10, base.Add(time.Duration(i)*time.Minute))
	}
	agg.OnTrade("SOXL", 0, 1, base.Add(10*time.Minute))
	agg.OnTrade("SOXL", -5, 1, base.Add(11*time.Minute))
	snap := agg.Snapshot(bars.TF1m, 20, 0)
	min, max, ok := priceRange(snap, ChartOpts{ShowVWAP: true})
	if !ok {
		t.Fatal("expected ok")
	}
	if min <= 0 {
		t.Fatalf("after zero/negative trades scale still pinned low: min=%v max=%v last=%+v",
			min, max, snap.Bars[len(snap.Bars)-1])
	}
	if max-min > 5 {
		t.Fatalf("unexpected wide span %v", max-min)
	}
}
