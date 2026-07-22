package bars

import (
	"math"
	"testing"
	"time"
)

// Invalid live prices must never enter OHLC rings (a zero Low collapses the UI Y-axis).
func TestOnTradeRejectsInvalidPrices(t *testing.T) {
	agg := NewAggregator(9, 21)
	now := time.Now().UTC().Truncate(time.Minute)
	for i := 0; i < 5; i++ {
		agg.OnTrade("SOXL", 148+float64(i)*0.01, 10, now.Add(time.Duration(i)*time.Second))
	}
	before, _ := agg.LatestTrade()
	agg.OnTrade("SOXL", 0, 1, now.Add(10*time.Second))
	agg.OnTrade("SOXL", -1, 1, now.Add(11*time.Second))
	agg.OnTrade("SOXL", 1e20, 1, now.Add(12*time.Second)) // absurd vs last — rejected
	agg.OnTrade("SOXL", math.NaN(), 1, now.Add(13*time.Second))
	agg.OnTrade("SOXL", math.Inf(1), 1, now.Add(14*time.Second))
	agg.OnTrade("SOXL", math.Inf(-1), 1, now.Add(15*time.Second))

	for _, tf := range []TF{TF1s, TF1m} {
		snap := agg.Snapshot(tf, 50, 0)
		for i, b := range snap.Bars {
			if b.Low <= 0 || b.High <= 0 || b.Open <= 0 || b.Close <= 0 {
				t.Errorf("tf=%s bar[%d] non-positive OHLC: %+v", tf, i, b)
			}
			if b.High > before*maxLiveTradeJumpRatio {
				t.Errorf("tf=%s bar[%d] absurd high entered OHLC: %+v", tf, i, b)
			}
		}
	}
	px, _ := agg.LatestTrade()
	if px != before {
		t.Fatalf("last trade should stay %v after invalid/absurd prints, got %v", before, px)
	}
}
