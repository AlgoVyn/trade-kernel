package ui

import (
	"testing"
	"time"

	"trade-kernel/internal/bars"
)

func benchSnap(b *testing.B, nBars int) bars.Snapshot {
	b.Helper()
	agg := bars.NewAggregator(9, 21)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < nBars+5; i++ {
		p := 100.0 + float64(i%7) - 3
		t0 := base.Add(time.Duration(i) * time.Minute)
		agg.OnTrade("SOXL", p, 1000, t0)
		agg.OnTrade("SOXL", p+0.4, 800, t0.Add(10*time.Second))
		agg.OnTrade("SOXL", p-0.3, 600, t0.Add(20*time.Second))
	}
	return agg.Snapshot(bars.TF1m, nBars, 0)
}

func BenchmarkRenderCandles(b *testing.B) {
	snap := benchSnap(b, 80)
	opts := ChartOpts{ShowEMA: true, ShowEMA2: true, ShowVWAP: true, SessionShading: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderCandles(snap, 120, 30, opts)
	}
}

func BenchmarkRenderVolume(b *testing.B) {
	snap := benchSnap(b, 80)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = renderVolume(snap, 120, 6, true)
	}
}

func BenchmarkGridRender(b *testing.B) {
	g := newGrid(120, 30)
	for x := 0; x < 120; x += 2 {
		for y := 5; y < 25; y++ {
			col := colUp
			if x%4 == 0 {
				col = colDown
			}
			g.setCandle(x, y, '█', col)
		}
		g.setColBg(x, bgPreMarket)
	}
	for i := 0; i < 40; i++ {
		g.drawIndLine(i*3, 10, i*3+2, 20, colEMA)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = g.render()
	}
}
