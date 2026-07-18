package bars

import (
	"math"
	"testing"
	"time"
)

type tick struct {
	sec   int // seconds after base
	price float64
	size  float64
}

var base = time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC) // 10:00 ET

func feed(a *Aggregator, ticks []tick) {
	for _, tk := range ticks {
		a.OnTrade("TEST", tk.price, tk.size, base.Add(time.Duration(tk.sec)*time.Second))
	}
}

func TestAggregation1m(t *testing.T) {
	a := NewAggregator(2, 3)
	feed(a, []tick{
		{0, 100, 10}, {5, 101, 10}, {30, 99, 5}, // bar 14:00
		{60, 102, 10}, {90, 103, 10}, // bar 14:01
	})
	snap := a.Snapshot(TF1m, 10)
	if len(snap.Bars) != 2 {
		t.Fatalf("want 2 bars, got %d", len(snap.Bars))
	}
	b0 := snap.Bars[0]
	if b0.Open != 100 || b0.High != 101 || b0.Low != 99 || b0.Close != 99 || b0.Volume != 25 {
		t.Fatalf("bar0 = %+v", b0)
	}
	// Second bar is the forming bar.
	b1 := snap.Bars[1]
	if b1.Open != 102 || b1.High != 103 || b1.Close != 103 || b1.Volume != 20 {
		t.Fatalf("bar1 = %+v", b1)
	}
	if !b0.Start.Equal(base) || !b1.Start.Equal(base.Add(time.Minute)) {
		t.Fatalf("starts: %v %v", b0.Start, b1.Start)
	}
}

func TestAggregation5sAndRoll(t *testing.T) {
	a := NewAggregator(2, 3)
	feed(a, []tick{
		{0, 100, 1}, {2, 102, 1}, // 5s bucket 0
		{6, 104, 1}, // bucket 1: closes bucket 0
		{12, 98, 1}, // bucket 2: closes bucket 1
	})
	snap := a.Snapshot(TF5s, 10)
	if len(snap.Bars) != 3 {
		t.Fatalf("want 3 bars, got %d", len(snap.Bars))
	}
	if snap.Bars[0].Close != 102 || snap.Bars[1].Close != 104 || snap.Bars[2].Close != 98 {
		t.Fatalf("closes: %v %v %v", snap.Bars[0].Close, snap.Bars[1].Close, snap.Bars[2].Close)
	}
	// SMA(2) at bar1 close = (102+104)/2 = 103.
	if got := snap.SMA[1]; math.Abs(got-103) > 1e-9 {
		t.Fatalf("SMA = %v, want 103", got)
	}
	// VWAP cumulative: (100+102+104+98)/4 = 101.
	if got := a.SessionVWAP(); math.Abs(got-101) > 1e-9 {
		t.Fatalf("VWAP = %v, want 101", got)
	}
}

func TestVWAPReset(t *testing.T) {
	a := NewAggregator(2, 3)
	feed(a, []tick{{0, 100, 1}, {1, 110, 1}})
	a.ResetVWAP()
	if !math.IsNaN(a.SessionVWAP()) {
		t.Fatal("VWAP should be NaN after reset")
	}
	feed(a, []tick{{2, 200, 1}})
	if got := a.SessionVWAP(); math.Abs(got-200) > 1e-9 {
		t.Fatalf("VWAP after reset = %v, want 200", got)
	}
}

func TestLateTradeUpdatesHistoricalBar(t *testing.T) {
	a := NewAggregator(2, 3)
	feed(a, []tick{
		{0, 100, 1}, {6, 104, 1},
	})
	// Late trade into bucket 0 (sec 3) with a higher high.
	a.OnTrade("TEST", 105, 2, base.Add(3*time.Second))
	snap := a.Snapshot(TF5s, 10)
	if snap.Bars[0].High != 105 {
		t.Fatalf("late trade: high = %v, want 105", snap.Bars[0].High)
	}
	if snap.Bars[0].Volume != 3 {
		t.Fatalf("late trade: vol = %v, want 3", snap.Bars[0].Volume)
	}
	// Close must remain the in-order close.
	if snap.Bars[0].Close != 100 {
		t.Fatalf("late trade: close = %v, want 100", snap.Bars[0].Close)
	}
}

func TestDailyBucketAnchor(t *testing.T) {
	// 21:00 ET Wednesday and 02:00 ET Thursday belong to the same daily
	// bar (anchored 20:00 ET).
	loc := mustET(t)
	wed21 := time.Date(2026, 7, 15, 21, 0, 0, 0, loc)
	thu02 := time.Date(2026, 7, 16, 2, 0, 0, 0, loc)
	b1 := bucket(TF1d, wed21)
	b2 := bucket(TF1d, thu02)
	if !b1.Equal(b2) {
		t.Fatalf("daily buckets differ: %v vs %v", b1, b2)
	}
	if b1.Hour() != 20 {
		t.Fatalf("daily anchor = %v, want 20:00", b1)
	}
	// Thursday 10:00 ET (regular session) is the same trading day too.
	thu10 := time.Date(2026, 7, 16, 10, 0, 0, 0, loc)
	if !bucket(TF1d, thu10).Equal(b1) {
		t.Fatalf("regular-hours bucket %v != %v", bucket(TF1d, thu10), b1)
	}
}

func TestLoadBackfill(t *testing.T) {
	a := NewAggregator(2, 3)
	hist := []Bar{
		{Start: base, Open: 1, High: 1, Low: 1, Close: 10, Volume: 100},
		{Start: base.Add(time.Minute), Open: 1, High: 1, Low: 1, Close: 20, Volume: 100},
		{Start: base.Add(2 * time.Minute), Open: 1, High: 1, Low: 1, Close: 30, Volume: 100},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10)
	if len(snap.Bars) != 3 {
		t.Fatalf("want 3 bars, got %d", len(snap.Bars))
	}
	// SMA(2) at last close = 25.
	if got := snap.SMA[2]; math.Abs(got-25) > 1e-9 {
		t.Fatalf("SMA after backfill = %v, want 25", got)
	}
	// EMA(3) k=0.5: 10, 15, 22.5
	if got := snap.EMA[2]; math.Abs(got-22.5) > 1e-9 {
		t.Fatalf("EMA after backfill = %v, want 22.5", got)
	}
	// Live trades extend the backfilled series.
	feed(a, []tick{{180, 40, 10}})
	snap = a.Snapshot(TF1m, 10)
	if len(snap.Bars) != 4 || snap.Bars[3].Close != 40 {
		t.Fatalf("after live tick: %+v", snap.Bars)
	}
}

func TestSnapshotLimit(t *testing.T) {
	a := NewAggregator(2, 3)
	for i := 0; i < 100; i++ {
		a.OnTrade("TEST", float64(100+i), 1, base.Add(time.Duration(i)*time.Minute))
	}
	snap := a.Snapshot(TF1m, 10)
	if len(snap.Bars) != 10 {
		t.Fatalf("want 10 bars, got %d", len(snap.Bars))
	}
	if snap.Bars[9].Close != 199 {
		t.Fatalf("newest close = %v, want 199", snap.Bars[9].Close)
	}
}

func mustET(t *testing.T) *time.Location {
	t.Helper()
	l, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable:", err)
	}
	return l
}
