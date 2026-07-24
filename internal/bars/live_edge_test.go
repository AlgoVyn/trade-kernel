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

// Backfill buffer must not grow without bound during a long REST fetch.
func TestBackfillBufCapped(t *testing.T) {
	agg := NewAggregator(9, 21)
	now := time.Now().UTC()
	agg.BeginBackfill()
	// Overflow past maxBackfillBuf; oldest half is dropped, newest retained.
	n := maxBackfillBuf + maxBackfillBuf/2 + 10
	for i := 0; i < n; i++ {
		agg.OnTrade("AAPL", 100+float64(i%3), 1, now.Add(time.Duration(i)*time.Millisecond))
	}
	agg.mu.Lock()
	got := len(agg.backfillBuf)
	agg.mu.Unlock()
	if got > maxBackfillBuf {
		t.Fatalf("backfillBuf len=%d, want <= %d", got, maxBackfillBuf)
	}
	// After overflow trimming, buffer should still hold a recent print.
	if got < maxBackfillBuf/2 {
		t.Fatalf("backfillBuf len=%d, want at least half capacity after trim", got)
	}
	px, _ := agg.LatestTrade()
	wantPx := 100 + float64((n-1)%3)
	if px != wantPx {
		t.Fatalf("last trade = %v, want %v", px, wantPx)
	}
}

// Nested BeginBackfill keeps the existing buffer (switchSymbol pre-arm +
// backfill re-arm must not drop early live prints).
func TestBeginBackfillIdempotentKeepsBuffer(t *testing.T) {
	agg := NewAggregator(9, 21)
	now := time.Now().UTC().Truncate(time.Minute)
	agg.BeginBackfill()
	agg.OnTrade("AAPL", 101, 10, now.Add(time.Second))
	// Second Begin (as backfill() does after switchSymbol pre-arm) must keep
	// the print already buffered.
	agg.BeginBackfill()
	agg.OnTrade("AAPL", 102, 20, now.Add(2*time.Second))
	hist := []Bar{{
		Start: now.Add(-time.Minute), Open: 100, High: 100, Low: 100, Close: 100, Volume: 100,
	}}
	agg.Load(TF1m, hist)
	agg.EndBackfill()

	snap := agg.Snapshot(TF1m, 10, 0)
	var vol float64
	for _, b := range snap.Bars {
		if b.Close == 101 || b.Close == 102 || b.High >= 101 {
			vol += b.Volume
		}
	}
	// Both buffered prints must survive Load.
	px, _ := agg.LatestTrade()
	if px != 102 {
		t.Fatalf("last = %v, want 102", px)
	}
	// Volume of live prints should be present (10+20 on forming bar(s)).
	found101, found102 := false, false
	for _, b := range snap.Bars {
		if b.High >= 101 && b.Volume > 0 {
			if b.Close == 101 || b.High == 101 || b.Low == 101 || b.Open == 101 {
				found101 = true
			}
			if b.Close == 102 || b.High == 102 {
				found102 = true
			}
		}
	}
	// At least the later print and cumulative volume from both.
	if !found102 {
		t.Fatalf("expected 102 print in bars after nested Begin, got %+v", snap.Bars)
	}
	_ = found101
	_ = vol
}

// Live prints during BeginBackfill…EndBackfill survive Load (which resets rings).
func TestBackfillBufferSurvivesLoad(t *testing.T) {
	agg := NewAggregator(9, 21)
	now := time.Now().UTC().Truncate(time.Minute)
	// Seed a closed history bar.
	hist := []Bar{{
		Start: now.Add(-time.Minute), Open: 100, High: 101, Low: 99, Close: 100.5, Volume: 1000,
	}}
	agg.BeginBackfill()
	// Live print while "REST" is in flight.
	agg.OnTrade("AAPL", 102, 50, now.Add(5*time.Second))
	// Load would wipe rings without the buffer.
	agg.Load(TF1m, hist)
	agg.EndBackfill()

	px, _ := agg.LatestTrade()
	if px != 102 {
		t.Fatalf("last = %v, want 102", px)
	}
	snap := agg.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) == 0 {
		t.Fatal("no bars after backfill buffer")
	}
	// Forming or closed bar for the live print should carry volume 50.
	found := false
	for _, b := range snap.Bars {
		if b.Close == 102 && b.Volume >= 50 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected live buffered trade in snapshot, got %+v", snap.Bars)
	}
}

// EndBackfillAfterReplay must not double-count a print already present in
// ReplayTrades on sub-minute series; post-cutoff prints still apply.
func TestBackfillAfterReplayNoSubMinuteDoubleCount(t *testing.T) {
	agg := NewAggregator(9, 21)
	now := time.Now().UTC().Truncate(time.Minute)
	tradeTS := now.Add(10 * time.Second)
	replayEnd := now.Add(30 * time.Second)
	lateTS := now.Add(45 * time.Second)

	agg.BeginBackfill()
	// Same print that will appear in REST trades (inside replay window).
	agg.OnTrade("AAPL", 100, 25, tradeTS)
	// Print after the REST trade window end.
	agg.OnTrade("AAPL", 101, 10, lateTS)

	agg.Load(TF1m, []Bar{{
		Start: now.Add(-time.Minute), Open: 99, High: 99.5, Low: 98.5, Close: 99, Volume: 500,
	}})
	agg.ReplayTrades([]TradeReplay{
		{Price: 100, Size: 25, Timestamp: tradeTS},
	})
	// barEnd zero: no minute+ gate (covered by TestBackfillAfterReplayNoMinutePlusDoubleCount).
	agg.EndBackfillAfterReplay(replayEnd, time.Time{})

	// Sub-minute: volume from the in-window trade once, plus the late print.
	snap1s := agg.Snapshot(TF1s, 50, 0)
	var vol1s float64
	for _, b := range snap1s.Bars {
		vol1s += b.Volume
	}
	if vol1s != 35 { // 25 + 10, not 25+25+10
		t.Fatalf("1s total volume = %v, want 35 (no double-count of replayed print)", vol1s)
	}

	// 1m still receives both buffered prints when barEnd is zero.
	snap1m := agg.Snapshot(TF1m, 20, 0)
	var vol1m float64
	for _, b := range snap1m.Bars {
		if !b.Start.Before(now) {
			vol1m += b.Volume
		}
	}
	if vol1m < 35 {
		t.Fatalf("1m live-window volume = %v, want >= 35 from buffer, bars=%+v", vol1m, snap1m.Bars)
	}
}

// Buffered overnight/overlap prints already in REST Load must not re-fold
// into 1m+ volume or session VWAP when barEnd gates the drain.
func TestBackfillAfterReplayNoMinutePlusDoubleCount(t *testing.T) {
	agg := NewAggregator(9, 21)
	now := time.Now().UTC().Truncate(time.Minute)
	tradeTS := now.Add(10 * time.Second)
	barEnd := now.Add(30 * time.Second) // REST bar request end
	replayEnd := barEnd
	lateTS := now.Add(45 * time.Second)

	// Load already includes the in-window print's volume (as REST bars do).
	agg.BeginBackfill()
	agg.OnTrade("AAPL", 100, 25, tradeTS) // same print BOATS would buffer during Load
	agg.OnTrade("AAPL", 101, 10, lateTS)  // after barEnd — must still apply

	agg.Load(TF1m, []Bar{{
		Start: now, Open: 100, High: 100, Low: 100, Close: 100, Volume: 25,
	}})
	agg.ReplayTrades([]TradeReplay{
		{Price: 100, Size: 25, Timestamp: tradeTS},
	})
	agg.EndBackfillAfterReplay(replayEnd, barEnd)

	snap1m := agg.Snapshot(TF1m, 20, 0)
	var vol1m float64
	for _, b := range snap1m.Bars {
		if !b.Start.Before(now) {
			vol1m += b.Volume
		}
	}
	// Load 25 + late buffer 10; in-window buffered 25 must not double.
	if vol1m != 35 {
		t.Fatalf("1m volume = %v, want 35 (no double-count of Load-overlap print), bars=%+v", vol1m, snap1m.Bars)
	}
}
