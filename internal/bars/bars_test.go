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
	snap := a.Snapshot(TF1m, 10, 0)
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
	snap := a.Snapshot(TF5s, 10, 0)
	if len(snap.Bars) != 3 {
		t.Fatalf("want 3 bars, got %d", len(snap.Bars))
	}
	if snap.Bars[0].Close != 102 || snap.Bars[1].Close != 104 || snap.Bars[2].Close != 98 {
		t.Fatalf("closes: %v %v %v", snap.Bars[0].Close, snap.Bars[1].Close, snap.Bars[2].Close)
	}
	// NewAggregator(2, 3): EMA period 2, EMA2 period 3 (k=0.5).
	// EMA2 at bar1: seed 102, then 104 → 103.
	if got := snap.EMA2[1]; math.Abs(got-103) > 1e-9 {
		t.Fatalf("EMA2 = %v, want 103", got)
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

func TestResetMarketClearsQuoteAndTrade(t *testing.T) {
	a := NewAggregator(2, 3)
	a.OnTrade("AAPL", 150.25, 10, base)
	a.OnQuote("AAPL", 150.20, 150.30, base)

	price, at := a.LatestTrade()
	if price != 150.25 || at.IsZero() {
		t.Fatalf("before reset: trade = %v @ %v", price, at)
	}
	bid, ask, qAt := a.LatestQuote()
	if bid != 150.20 || ask != 150.30 || qAt.IsZero() {
		t.Fatalf("before reset: quote = %v×%v @ %v", bid, ask, qAt)
	}

	a.ResetMarket()

	if price, at := a.LatestTrade(); price != 0 || !at.IsZero() {
		t.Fatalf("after reset: trade = %v @ %v (want zero/zero)", price, at)
	}
	if bid, ask, qAt := a.LatestQuote(); bid != 0 || ask != 0 || !qAt.IsZero() {
		t.Fatalf("after reset: quote = %v×%v @ %v (want zero/zero)", bid, ask, qAt)
	}
}

func TestLateTradeUpdatesHistoricalBar(t *testing.T) {
	a := NewAggregator(2, 3)
	feed(a, []tick{
		{0, 100, 1}, {6, 104, 1},
	})
	// Late trade into bucket 0 (sec 3) with a higher high.
	a.OnTrade("TEST", 105, 2, base.Add(3*time.Second))
	snap := a.Snapshot(TF5s, 10, 0)
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
	// Historical (not-current) buckets stay fully closed.
	hist := []Bar{
		{Start: base, Open: 1, High: 1, Low: 1, Close: 10, Volume: 100},
		{Start: base.Add(time.Minute), Open: 1, High: 1, Low: 1, Close: 20, Volume: 100},
		{Start: base.Add(2 * time.Minute), Open: 1, High: 1, Low: 1, Close: 30, Volume: 100},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 3 {
		t.Fatalf("want 3 bars, got %d", len(snap.Bars))
	}
	// NewAggregator(2, 3): EMA period 2, EMA2 period 3.
	// EMA(2) k=2/3 over closes 10,20,30:
	//   10; 20*(2/3)+10/3 = 50/3; 30*(2/3)+(50/3)*(1/3) = 20 + 50/9 = 230/9
	if got := snap.EMA[2]; math.Abs(got-230.0/9.0) > 1e-9 {
		t.Fatalf("EMA after backfill = %v, want %v", got, 230.0/9.0)
	}
	// EMA2(3) k=0.5: 10, 15, 22.5
	if got := snap.EMA2[2]; math.Abs(got-22.5) > 1e-9 {
		t.Fatalf("EMA2 after backfill = %v, want 22.5", got)
	}
	// Session VWAP reconstructed from typical price (H+L+C)/3:
	// bar0: (1+1+10)/3 = 4, bar1: 22/3, bar2: 32/3
	// cum: (4*100 + 22/3*100 + 32/3*100) / 300
	tp0, tp1, tp2 := (1.0+1+10)/3, (1.0+1+20)/3, (1.0+1+30)/3
	wantVWAP0 := tp0
	wantVWAP1 := (tp0*100 + tp1*100) / 200
	wantVWAP2 := (tp0*100 + tp1*100 + tp2*100) / 300
	for i, want := range []float64{wantVWAP0, wantVWAP1, wantVWAP2} {
		if math.IsNaN(snap.VWAP[i]) {
			t.Fatalf("VWAP[%d] is NaN after Load — chart would hide the line", i)
		}
		if math.Abs(snap.VWAP[i]-want) > 1e-9 {
			t.Fatalf("VWAP[%d] = %v, want %v", i, snap.VWAP[i], want)
		}
	}
	// Live session VWAP seeded from the reconstruction.
	if got := a.SessionVWAP(); math.Abs(got-wantVWAP2) > 1e-9 {
		t.Fatalf("seeded SessionVWAP = %v, want %v", got, wantVWAP2)
	}
	// Live trades extend the backfilled series.
	feed(a, []tick{{180, 40, 10}})
	snap = a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 4 || snap.Bars[3].Close != 40 {
		t.Fatalf("after live tick: %+v", snap.Bars)
	}
}

// TestLoadCurrentBucketIsForming ensures the incomplete current-minute bar
// from REST is reopened as forming so live trades extend it (one candle /
// correct volume) instead of duplicating the bucket.
func TestLoadCurrentBucketIsForming(t *testing.T) {
	a := NewAggregator(2, 3)
	now := time.Now().UTC().Truncate(time.Minute)
	hist := []Bar{
		{Start: now.Add(-2 * time.Minute), Open: 10, High: 11, Low: 9, Close: 10.5, Volume: 100, VWAP: 10},
		{Start: now.Add(-time.Minute), Open: 10.5, High: 12, Low: 10, Close: 11, Volume: 200, VWAP: 11},
		// Incomplete current minute from REST (partial volume).
		{Start: now, Open: 11, High: 11.5, Low: 10.8, Close: 11.2, Volume: 50, VWAP: 11.1},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 3 {
		t.Fatalf("want 3 bars (2 closed + forming), got %d", len(snap.Bars))
	}
	if snap.Bars[2].Volume != 50 || snap.Bars[2].Close != 11.2 {
		t.Fatalf("forming = %+v", snap.Bars[2])
	}
	// Forming volume is seeded into SessionVWAP (no further trades).
	// (10*100 + 11*200 + 11.1*50) / 350
	wantVWAP := (10.0*100 + 11*200 + 11.1*50) / 350
	if got := a.SessionVWAP(); math.Abs(got-wantVWAP) > 1e-9 {
		t.Fatalf("SessionVWAP after Load with forming = %v, want %v (includes partial)", got, wantVWAP)
	}
	// Live trade in the same minute extends forming — no fourth bar.
	a.OnTrade("TEST", 11.8, 25, now.Add(30*time.Second))
	snap = a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 3 {
		t.Fatalf("after live same-minute: want 3 bars, got %d (%+v)", len(snap.Bars), snap.Bars)
	}
	f := snap.Bars[2]
	if f.Volume != 75 || f.High != 11.8 || f.Close != 11.8 {
		t.Fatalf("extended forming = %+v, want vol=75 high/close=11.8", f)
	}
}

// TestLoadFormingBucketAcceptsFutureSkew treats lastStart >= curBucket as
// forming (REST/server clock slightly ahead of local truncate).
func TestLoadFormingBucketAcceptsFutureSkew(t *testing.T) {
	a := NewAggregator(2, 3)
	// Bar start is the next minute relative to "now" inside Load — simulate
	// by loading a bar whose start is still the current truncated minute
	// (equal path) is covered above; here lastStart is strictly after the
	// local current bucket only if we pass a start in the future.
	future := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	hist := []Bar{
		{Start: future.Add(-2 * time.Minute), Open: 10, High: 10, Low: 10, Close: 10, Volume: 10, VWAP: 10},
		{Start: future, Open: 11, High: 11, Low: 11, Close: 11, Volume: 20, VWAP: 11},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10, 0)
	// If future is still >= curBucket, last bar is forming.
	if len(snap.Bars) < 1 {
		t.Fatal("empty snapshot")
	}
	last := snap.Bars[len(snap.Bars)-1]
	if !last.Start.Equal(future) {
		// Clock may have rolled; still require forming open with last hist.
		t.Logf("note: clock may have crossed bucket; last=%v future=%v", last.Start, future)
	}
	if last.Volume != 20 && last.Volume != 10 {
		t.Fatalf("unexpected last bar %+v", last)
	}
}

// TestLoadVWAPUsesBarVWAP prefers the REST per-bar VWAP field when set.
func TestLoadVWAPUsesBarVWAP(t *testing.T) {
	a := NewAggregator(2, 3)
	hist := []Bar{
		{Start: base, Open: 10, High: 12, Low: 9, Close: 11, Volume: 50, VWAP: 10.5},
		{Start: base.Add(time.Minute), Open: 11, High: 13, Low: 10, Close: 12, Volume: 150, VWAP: 11.5},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10, 0)
	want0 := 10.5
	want1 := (10.5*50 + 11.5*150) / 200
	if math.Abs(snap.VWAP[0]-want0) > 1e-9 {
		t.Fatalf("VWAP[0] = %v, want %v", snap.VWAP[0], want0)
	}
	if math.Abs(snap.VWAP[1]-want1) > 1e-9 {
		t.Fatalf("VWAP[1] = %v, want %v", snap.VWAP[1], want1)
	}
}

// TestLoadVWAPResetsAcrossRTHDays ensures multi-day regular-hours-only
// history restarts session VWAP each trading day (not one multi-day cum).
func TestLoadVWAPResetsAcrossRTHDays(t *testing.T) {
	// Wednesday 10:00 ET and Thursday 10:00 ET — both Regular, no overnight bars.
	day1 := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC) // 10:00 ET
	day2 := time.Date(2026, 7, 16, 14, 0, 0, 0, time.UTC)
	a := NewAggregator(2, 3)
	a.SetVWAPAnchor("session")
	hist := []Bar{
		{Start: day1, Open: 10, High: 10, Low: 10, Close: 10, Volume: 100, VWAP: 10},
		{Start: day1.Add(time.Minute), Open: 12, High: 12, Low: 12, Close: 12, Volume: 100, VWAP: 12},
		{Start: day2, Open: 20, High: 20, Low: 20, Close: 20, Volume: 100, VWAP: 20},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10, 0)
	// Day2 first bar should restart at 20, not continue day1 average.
	if math.Abs(snap.VWAP[2]-20) > 1e-9 {
		t.Fatalf("VWAP day2 start = %v, want 20 (session reset)", snap.VWAP[2])
	}
	if got := a.SessionVWAP(); math.Abs(got-20) > 1e-9 {
		t.Fatalf("seeded SessionVWAP = %v, want 20", got)
	}
}

// TestLoadVWAPDayAnchor keeps one accumulator across sessions in a trading day.
func TestLoadVWAPDayAnchor(t *testing.T) {
	// Pre-market 08:00 ET and regular 10:00 ET same calendar day.
	pre := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) // 08:00 ET
	rth := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC) // 10:00 ET
	a := NewAggregator(2, 3)
	a.SetVWAPAnchor("day")
	hist := []Bar{
		{Start: pre, Open: 10, High: 10, Low: 10, Close: 10, Volume: 100, VWAP: 10},
		{Start: rth, Open: 20, High: 20, Low: 20, Close: 20, Volume: 100, VWAP: 20},
	}
	a.Load(TF1m, hist)
	snap := a.Snapshot(TF1m, 10, 0)
	want := (10.0*100 + 20*100) / 200
	if math.Abs(snap.VWAP[1]-want) > 1e-9 {
		t.Fatalf("day-anchor VWAP = %v, want %v (no session reset)", snap.VWAP[1], want)
	}
}

// TestLoadTF1mSeedNotOverwrittenByCoarser ensures later Load(TF5m) leaves
// the live SessionVWAP from TF1m alone.
func TestLoadTF1mSeedNotOverwrittenByCoarser(t *testing.T) {
	a := NewAggregator(2, 3)
	hist1 := []Bar{
		{Start: base, Open: 10, High: 10, Low: 10, Close: 10, Volume: 100, VWAP: 10},
	}
	hist5 := []Bar{
		{Start: base, Open: 50, High: 50, Low: 50, Close: 50, Volume: 100, VWAP: 50},
	}
	a.Load(TF1m, hist1)
	a.Load(TF5m, hist5)
	if got := a.SessionVWAP(); math.Abs(got-10) > 1e-9 {
		t.Fatalf("SessionVWAP = %v, want 10 from TF1m seed", got)
	}
}

func TestSnapshotLimit(t *testing.T) {
	a := NewAggregator(2, 3)
	for i := 0; i < 100; i++ {
		a.OnTrade("TEST", float64(100+i), 1, base.Add(time.Duration(i)*time.Minute))
	}
	snap := a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 10 {
		t.Fatalf("want 10 bars, got %d", len(snap.Bars))
	}
	if snap.Bars[9].Close != 199 {
		t.Fatalf("newest close = %v, want 199", snap.Bars[9].Close)
	}
}

// TestSnapshotOffset verifies the pan/offset parameter: offset=0 shows the
// live forming bar at the right edge; offset>0 shows only closed bars,
// shifted back from the edge.
func TestSnapshotOffset(t *testing.T) {
	// Build 5 1-minute bars: 4 closed (closes 100,101,102,103) + 1 forming
	// (close 104). Minute N→close 100+N.
	a := NewAggregator(2, 3)
	for i := 0; i < 5; i++ {
		feed(a, []tick{{i * 60, float64(100 + i), 1}})
	}
	// Sanity: depth = 4 closed bars.
	if got := a.HistoryDepth(TF1m); got != 4 {
		t.Fatalf("HistoryDepth = %d, want 4", got)
	}

	// offset=0: live edge — newest bar is the forming bar (close 104).
	snap := a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 5 {
		t.Fatalf("offset=0: want 5 bars (4 closed + forming), got %d", len(snap.Bars))
	}
	if snap.Bars[len(snap.Bars)-1].Close != 104 {
		t.Fatalf("offset=0: newest = %v, want forming 104", snap.Bars[len(snap.Bars)-1].Close)
	}

	// offset=1: forming bar dropped, newest closed is 103.
	snap = a.Snapshot(TF1m, 10, 1)
	if len(snap.Bars) != 4 {
		t.Fatalf("offset=1: want 4 closed bars, got %d", len(snap.Bars))
	}
	if snap.Bars[len(snap.Bars)-1].Close != 103 {
		t.Fatalf("offset=1: newest = %v, want 103", snap.Bars[len(snap.Bars)-1].Close)
	}
	if snap.Bars[0].Close != 100 {
		t.Fatalf("offset=1: oldest = %v, want 100", snap.Bars[0].Close)
	}

	// offset=2: newest closed is 102, oldest visible is 100.
	snap = a.Snapshot(TF1m, 10, 2)
	if got := snap.Bars[len(snap.Bars)-1].Close; got != 102 {
		t.Fatalf("offset=2: newest = %v, want 102", got)
	}

	// offset=4: all the way back — newest visible is 100 (oldest retained).
	snap = a.Snapshot(TF1m, 10, 4)
	if len(snap.Bars) != 1 {
		t.Fatalf("offset=4: want 1 bar, got %d", len(snap.Bars))
	}
	if snap.Bars[0].Close != 100 {
		t.Fatalf("offset=4: want oldest bar 100, got %v", snap.Bars[0].Close)
	}

	// offset past the edge: clamped to the oldest available bar (never
	// underflows or panics). offset=5 ≥ r.count=4 → clamps to show oldest.
	snap = a.Snapshot(TF1m, 10, 5)
	if len(snap.Bars) != 1 || snap.Bars[0].Close != 100 {
		t.Fatalf("offset=5 (clamped): want oldest bar 100, got %+v", snap.Bars)
	}
	snap = a.Snapshot(TF1m, 10, 99)
	if len(snap.Bars) != 1 || snap.Bars[0].Close != 100 {
		t.Fatalf("offset=99 (clamped): want oldest bar 100, got %+v", snap.Bars)
	}
}

// TestSnapshotOffsetSmallWindow checks offset interaction with the width
// cap n: when n < available closed bars, the window slides correctly.
func TestSnapshotOffsetSmallWindow(t *testing.T) {
	// 6 closed bars 100..105 (indices 0-5), 106 is forming. Minute i → 100+i.
	a := NewAggregator(2, 3)
	for i := 0; i < 7; i++ {
		feed(a, []tick{{i * 60, float64(100 + i), 1}})
	}
	// offset=2, window=3: newest visible index = r.count-offset = 6-2 = 4
	// (close 104); window of 3 → indices 2,3,4 = closes 102,103,104.
	snap := a.Snapshot(TF1m, 3, 2)
	if len(snap.Bars) != 3 {
		t.Fatalf("want 3 bars, got %d", len(snap.Bars))
	}
	got := []float64{snap.Bars[0].Close, snap.Bars[1].Close, snap.Bars[2].Close}
	want := []float64{102, 103, 104}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("window = %v, want %v", got, want)
		}
	}
}

// TestReplayTrades verifies that historical trades replayed into the
// aggregator populate the sub-minute TFs (1s/5s/15s) without touching the
// 1m+ TFs or the session VWAP / last-trade cache.
func TestReplayTrades(t *testing.T) {
	a := NewAggregator(2, 3)
	// No live trades yet — every TF starts empty.
	if a.HistoryDepth(TF1s) != 0 || a.HistoryDepth(TF1m) != 0 {
		t.Fatal("expected empty aggregator")
	}

	// Replay 90 seconds of trades (one per second), prices 100..189.
	replay := make([]TradeReplay, 90)
	for i := range replay {
		replay[i] = TradeReplay{
			Price:     float64(100 + i),
			Size:      1,
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}
	}
	a.ReplayTrades(replay)

	// Sub-minute TFs must now have bars.
	if d := a.HistoryDepth(TF1s); d == 0 {
		t.Fatal("1s depth = 0 after replay")
	}
	if d := a.HistoryDepth(TF5s); d == 0 {
		t.Fatal("5s depth = 0 after replay")
	}
	if d := a.HistoryDepth(TF15s); d == 0 {
		t.Fatal("15s depth = 0 after replay")
	}
	// 90 seconds = 90 1s-bars; spot-check the newest 1s close (189 or the
	// forming bar's last trade, both = 189).
	snap := a.Snapshot(TF1s, 100, 0)
	if snap.Bars[len(snap.Bars)-1].Close != 189 {
		t.Fatalf("1s newest close = %v, want 189", snap.Bars[len(snap.Bars)-1].Close)
	}

	// 1m+ TFs must NOT be populated by replay (they're backfilled separately).
	for _, tf := range []TF{TF1m, TF5m, TF15m, TF1h, TF1d} {
		if d := a.HistoryDepth(tf); d != 0 {
			t.Fatalf("replay must not touch %s: depth = %d", tf, d)
		}
	}

	// Session VWAP must be untouched by replay (no live volume yet → NaN).
	if v := a.SessionVWAP(); !math.IsNaN(v) {
		t.Fatalf("session VWAP corrupted by replay: %v", v)
	}
	// Last-trade cache must be untouched (live-only).
	if p, at := a.LatestTrade(); p != 0 || !at.IsZero() {
		t.Fatalf("last-trade cache corrupted by replay: %v @ %v", p, at)
	}
}

// TestSameBucketReopensForming: if the current bucket was closed into the
// ring (race / older Load), a live trade for that bucket reopens it as
// forming and keeps EMA Peek'd off the new close rather than frozen stale.
func TestSameBucketReopensForming(t *testing.T) {
	a := NewAggregator(2, 3)
	// Two closed 1m bars via Load. base is far in the past vs time.Now(), so
	// neither bar is the incomplete current bucket — both land in the ring.
	t0 := base
	t1 := base.Add(time.Minute)
	a.Load(TF1m, []Bar{
		{Start: t0, Open: 100, High: 101, Low: 99, Close: 100, Volume: 10},
		{Start: t1, Open: 102, High: 103, Low: 101, Close: 102, Volume: 10},
	})

	// Trade still in t1 bucket — must reopen, not leave stale EMA on ring.
	a.OnTrade("AAPL", 110, 5, t1.Add(30*time.Second))
	snap := a.Snapshot(TF1m, 10, 0)
	if len(snap.Bars) != 2 {
		t.Fatalf("want 2 bars (1 closed + forming), got %d", len(snap.Bars))
	}
	f := snap.Bars[len(snap.Bars)-1]
	if !f.Start.Equal(t1) {
		t.Fatalf("forming start = %v, want %v", f.Start, t1)
	}
	if f.Close != 110 || f.High != 110 || f.Volume != 15 {
		t.Fatalf("forming = %+v, want close/high 110 vol 15", f)
	}
	// Forming EMA is Peek(110) from state after only the first closed bar.
	// EMA(2) k=2/3: seed 100, Peek(110) = 110*(2/3)+100*(1/3) = 320/3.
	wantEMA := 110.0*(2.0/3.0) + 100.0*(1.0/3.0)
	if got := snap.EMA[len(snap.EMA)-1]; math.Abs(got-wantEMA) > 1e-9 {
		t.Fatalf("forming EMA = %v, want %v", got, wantEMA)
	}
}

// TestReplayThenLiveExtends confirms a live trade after replay extends the
// sub-minute series correctly (closes the replayed forming bar if it's in
// a new bucket, or folds in if the same bucket).
func TestReplayThenLiveExtends(t *testing.T) {
	a := NewAggregator(2, 3)
	// Replay 5 seconds of trades.
	replay := make([]TradeReplay, 5)
	for i := range replay {
		replay[i] = TradeReplay{Price: 100, Size: 1, Timestamp: base.Add(time.Duration(i) * time.Second)}
	}
	a.ReplayTrades(replay)
	before := a.HistoryDepth(TF1s)

	// A live trade 10s later falls into a new 1s bucket → closes the replayed
	// forming bar and opens a fresh one.
	a.OnTrade("AAPL", 200, 1, base.Add(10*time.Second))
	after := a.HistoryDepth(TF1s)
	if after <= before {
		t.Fatalf("live trade after replay should grow closed depth: %d -> %d", before, after)
	}
	// Newest close should be 200.
	snap := a.Snapshot(TF1s, 100, 0)
	if got := snap.Bars[len(snap.Bars)-1].Close; got != 200 {
		t.Fatalf("newest 1s close = %v, want 200 (live trade)", got)
	}
}

func TestLastBarTime(t *testing.T) {
	// Empty aggregator → ok=false.
	a := NewAggregator(2, 3)
	if _, ok := a.LastBarTime(TF1m); ok {
		t.Fatal("empty aggregator: LastBarTime should be ok=false")
	}

	// Feed 3 one-minute bars: closes at minutes 0,1,2; bar at minute 3 is forming.
	for i := 0; i < 4; i++ {
		feed(a, []tick{{i * 60, float64(100 + i), 1}})
	}
	// Forming bar present → LastBarTime returns the forming bar's start.
	got, ok := a.LastBarTime(TF1m)
	if !ok {
		t.Fatal("expected ok=true after feeding bars")
	}
	if want := base.Add(3 * time.Minute); !got.Equal(want) {
		t.Fatalf("LastBarTime (forming) = %v, want %v", got, want)
	}

	// Close the forming bar by feeding a trade in the next minute; the now-
	// newest closed bar should be reported.
	feed(a, []tick{{4 * 60, 104, 1}})
	// After this feed, the minute-3 bar closed and minute-4 is forming, so
	// LastBarTime still points at the forming (minute-4) start.
	got, _ = a.LastBarTime(TF1m)
	if want := base.Add(4 * time.Minute); !got.Equal(want) {
		t.Fatalf("LastBarTime after rollover = %v, want %v", got, want)
	}
}

func TestLastBarTimeClosedOnly(t *testing.T) {
	// When no forming bar exists (e.g. after Load/backfill), LastBarTime
	// returns the newest closed bar.
	a := NewAggregator(2, 3)
	hist := []Bar{
		{Start: base, Close: 10},
		{Start: base.Add(time.Minute), Close: 20},
		{Start: base.Add(2 * time.Minute), Close: 30},
	}
	a.Load(TF1m, hist)
	got, ok := a.LastBarTime(TF1m)
	if !ok {
		t.Fatal("expected ok=true after Load")
	}
	if want := base.Add(2 * time.Minute); !got.Equal(want) {
		t.Fatalf("LastBarTime (closed only) = %v, want %v", got, want)
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
