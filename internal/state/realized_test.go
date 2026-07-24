package state

import (
	"math"
	"testing"
	"time"

	"trade-kernel/internal/alpaca"
)

func tUTC(h, m int) time.Time {
	return time.Date(2026, 7, 22, h, m, 0, 0, time.UTC)
}

func TestRealizedRoundTripLong(t *testing.T) {
	// buy 100@10, sell 100@12 → +200 realized on the close.
	day0 := tUTC(0, 0)
	week0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "AAPL", Side: "buy", Qty: 100, Price: 10, Timestamp: tUTC(1, 0)},
		{ID: "2", Symbol: "AAPL", Side: "sell", Qty: 100, Price: 12, Timestamp: tUTC(2, 0)},
	}
	got := RealizedFromFills(fills, day0, week0)
	if math.Abs(got.Day-200) > 1e-9 || math.Abs(got.Week-200) > 1e-9 {
		t.Fatalf("got %+v, want day=week=200", got)
	}
}

func TestRealizedPartialAndScale(t *testing.T) {
	day0 := tUTC(0, 0)
	week0 := day0.Add(-24 * time.Hour)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 100, Timestamp: tUTC(1, 0)},
		{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 40, Price: 110, Timestamp: tUTC(2, 0)}, // +400
		{ID: "3", Symbol: "SOXL", Side: "buy", Qty: 20, Price: 105, Timestamp: tUTC(3, 0)},  // add; no realize
		// inventory: 80 @ (100*60+105*20)/80 = 101.25
		{ID: "4", Symbol: "SOXL", Side: "sell", Qty: 80, Price: 101.25, Timestamp: tUTC(4, 0)}, // 0
	}
	got := RealizedFromFills(fills, day0, week0)
	if math.Abs(got.Day-400) > 1e-6 {
		t.Fatalf("day = %v, want 400", got.Day)
	}
}

func TestRealizedShortRoundTrip(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "X", Side: "sell", Qty: 50, Price: 20, Timestamp: tUTC(1, 0)}, // short
		{ID: "2", Symbol: "X", Side: "buy", Qty: 50, Price: 18, Timestamp: tUTC(2, 0)},  // cover +100
	}
	got := RealizedFromFills(fills, day0, day0)
	if math.Abs(got.Day-100) > 1e-9 {
		t.Fatalf("short cover day = %v, want 100", got.Day)
	}
}

func TestRealizedFlipThroughZero(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "Y", Side: "buy", Qty: 10, Price: 10, Timestamp: tUTC(1, 0)},
		// sell 15: close 10 long @12 → +20, open short 5 @12
		{ID: "2", Symbol: "Y", Side: "sell", Qty: 15, Price: 12, Timestamp: tUTC(2, 0)},
		// cover short 5 @11 → +5
		{ID: "3", Symbol: "Y", Side: "buy", Qty: 5, Price: 11, Timestamp: tUTC(3, 0)},
	}
	got := RealizedFromFills(fills, day0, day0)
	if math.Abs(got.Day-25) > 1e-9 {
		t.Fatalf("flip day = %v, want 25", got.Day)
	}
}

func TestRealizedWindowBuckets(t *testing.T) {
	week0 := tUTC(0, 0)
	day0 := tUTC(12, 0)
	fills := []alpaca.Fill{
		// Before week: establish long, close for +50 — neither bucket
		{ID: "1", Symbol: "Z", Side: "buy", Qty: 10, Price: 10, Timestamp: week0.Add(-48 * time.Hour)},
		{ID: "2", Symbol: "Z", Side: "sell", Qty: 10, Price: 15, Timestamp: week0.Add(-24 * time.Hour)},
		// This week, before day: +30
		{ID: "3", Symbol: "Z", Side: "buy", Qty: 10, Price: 10, Timestamp: week0.Add(time.Hour)},
		{ID: "4", Symbol: "Z", Side: "sell", Qty: 10, Price: 13, Timestamp: week0.Add(2 * time.Hour)},
		// Today: +40
		{ID: "5", Symbol: "Z", Side: "buy", Qty: 10, Price: 10, Timestamp: day0.Add(time.Hour)},
		{ID: "6", Symbol: "Z", Side: "sell", Qty: 10, Price: 14, Timestamp: day0.Add(2 * time.Hour)},
	}
	got := RealizedFromFills(fills, day0, week0)
	if math.Abs(got.Week-70) > 1e-9 { // 30+40, not prior 50
		t.Fatalf("week = %v, want 70", got.Week)
	}
	if math.Abs(got.Day-40) > 1e-9 {
		t.Fatalf("day = %v, want 40", got.Day)
	}
}

func TestRealizedFromOrdersUsesFilledAvg(t *testing.T) {
	day0 := tUTC(0, 0)
	orders := []alpaca.Order{
		{ID: "o1", Symbol: "A", Side: "buy", FilledQty: 100, FilledAvgPrice: 50, FilledAt: tUTC(1, 0)},
		{ID: "o2", Symbol: "A", Side: "sell", FilledQty: 100, FilledAvgPrice: 55, FilledAt: tUTC(2, 0)},
	}
	got := RealizedFromOrders(orders, day0, day0)
	if math.Abs(got.Day-500) > 1e-9 {
		t.Fatalf("from orders day = %v, want 500", got.Day)
	}
}

func TestRealizedIgnoresUnfilledOrders(t *testing.T) {
	day0 := tUTC(0, 0)
	orders := []alpaca.Order{
		{ID: "o1", Symbol: "A", Side: "buy", FilledQty: 0, FilledAvgPrice: 50, FilledAt: tUTC(1, 0)},
		{ID: "o2", Symbol: "A", Side: "sell", Qty: 10, FilledQty: 0, Status: "canceled"},
	}
	got := RealizedFromOrders(orders, day0, day0)
	if got.Day != 0 || got.Week != 0 {
		t.Fatalf("empty fills should yield 0, got %+v", got)
	}
}

// Long-held long closed in-window: fill history only has the sell. Without a
// seed this would open a synthetic short (realized 0); with seed from REST
// open qty 0 we cannot recover avg — inventory inconsistent.
func TestRealizedLongHeldPartialCloseUsesSeed(t *testing.T) {
	day0 := tUTC(0, 0)
	// Opened years ago @50; only today's partial sell is in the lookback.
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: tUTC(2, 0)},
	}
	// REST still long 60 @ 50 after selling 40 of 100.
	seeds := []PositionSeed{{Symbol: "TQQQ", Qty: 60, Avg: 50}}
	got, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("expected inventory consistent after seed")
	}
	// Sold 40 of pre-window 100 @50 → realized (60-50)*40 = 400
	if math.Abs(got.Day-400) > 1e-9 {
		t.Fatalf("day = %v, want 400", got.Day)
	}
}

func TestRealizedLongHeldFullCloseWithoutSeedUnreliable(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 100, Price: 60, Timestamp: tUTC(2, 0)},
	}
	_, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, nil)
	if ok {
		t.Fatal("full close of pre-lookback lot without seed must be unreliable")
	}
}

// Complete in-window open matching REST qty must stay reliable even when the
// broker omits avg entry (qty-only seed). Realized from in-window round-trips
// does not need cost-basis bridging.
func TestRealizedQtyOnlySeedMatchesFillBook(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 10, Timestamp: tUTC(1, 0)},
		{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 40, Price: 12, Timestamp: tUTC(2, 0)},
	}
	// REST still long 60; avg missing/zero.
	seeds := []PositionSeed{{Symbol: "SOXL", Qty: 60, Avg: 0}}
	got, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("qty-only seed matching fill book must be reliable")
	}
	// (12-10)*40 = 80
	if math.Abs(got.Day-80) > 1e-9 {
		t.Fatalf("day = %v, want 80", got.Day)
	}
}

func TestSeedsFromPositionsIncludesZeroAvg(t *testing.T) {
	seeds := SeedsFromPositions([]alpaca.Position{
		{Symbol: "A", Qty: 10, Side: "long", AvgEntryPrice: 0},
		{Symbol: "B", Qty: 5, Side: "short", AvgEntryPrice: 0},
		{Symbol: "C", Qty: 0, Side: "long", AvgEntryPrice: 1},
	})
	if len(seeds) != 2 {
		t.Fatalf("len = %d, want 2 (drop zero qty only)", len(seeds))
	}
	by := map[string]PositionSeed{}
	for _, s := range seeds {
		by[s.Symbol] = s
	}
	if by["A"].Qty != 10 || by["A"].Avg != 0 {
		t.Fatalf("A = %+v", by["A"])
	}
	if by["B"].Qty != -5 || by["B"].Avg != 0 {
		t.Fatalf("B = %+v", by["B"])
	}
}

// Alpaca REST often returns negative qty for shorts. Seeds must still sign
// correctly and not drop the position (q <= 0 was the old bug).
func TestSeedsFromPositionsSignedRESTShortQty(t *testing.T) {
	seeds := SeedsFromPositions([]alpaca.Position{
		{Symbol: "PUT", Qty: -20, Side: "short", AvgEntryPrice: 4.825},
		{Symbol: "LONG", Qty: 10, Side: "long", AvgEntryPrice: 1.26},
	})
	if len(seeds) != 2 {
		t.Fatalf("len = %d, want 2", len(seeds))
	}
	by := map[string]PositionSeed{}
	for _, s := range seeds {
		by[s.Symbol] = s
	}
	if by["PUT"].Qty != -20 || by["PUT"].Avg != 4.825 {
		t.Fatalf("PUT = %+v, want qty=-20 avg=4.825", by["PUT"])
	}
	if by["LONG"].Qty != 10 {
		t.Fatalf("LONG = %+v", by["LONG"])
	}
}

// Activity FILL uses side=sell_short for short opens. Those must realize like sell.
func TestRealizedSellShortRoundTrip(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "DRAM", Side: "sell_short", Qty: 100, Price: 58.73, Timestamp: tUTC(1, 0)},
		{ID: "2", Symbol: "DRAM", Side: "buy", Qty: 100, Price: 58.56, Timestamp: tUTC(2, 0)},
	}
	got := RealizedFromFills(fills, day0, day0)
	// Cover short: (58.73 - 58.56) * 100 = 17
	if math.Abs(got.Day-17) > 1e-9 {
		t.Fatalf("day = %v, want 17", got.Day)
	}
}

// Short open at REST with sell_short-only history must seed and stay consistent.
func TestRealizedShortSeedWithSellShortFills(t *testing.T) {
	day0 := tUTC(0, 0)
	// Opened short via sell_short; REST still short 20 @ 4.825 (signed REST qty).
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "PUT", Side: "sell_short", Qty: 20, Price: 4.825, Timestamp: tUTC(1, 0)},
	}
	seeds := SeedsFromPositions([]alpaca.Position{
		{Symbol: "PUT", Qty: -20, Side: "short", AvgEntryPrice: 4.825},
	})
	got, ok, excl := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("short seed + sell_short open must be consistent")
	}
	if len(excl) != 0 {
		t.Fatalf("excluded = %v, want none", excl)
	}
	if got.Day != 0 {
		t.Fatalf("open only → day = %v, want 0", got.Day)
	}
}

func TestRealizedSeedOnlyLongHeldNoCloses(t *testing.T) {
	day0 := tUTC(0, 0)
	seeds := []PositionSeed{{Symbol: "TQQQ", Qty: 100, Avg: 50}}
	got, ok, _ := RealizedFromFillsWithSeed(nil, day0, day0, seeds)
	if !ok {
		t.Fatal("seed-only open position should be consistent")
	}
	if got.Day != 0 || got.Week != 0 {
		t.Fatalf("no closes → zero realized, got %+v", got)
	}
}

// Fill inventory larger than REST seed on the same side is REST-ahead-of-FILL
// lag: trust the fill book, do not invent a closing fill, do not wipe.
func TestRealizedSeedLagTrustsFillBook(t *testing.T) {
	day0 := tUTC(0, 0)
	// REST already shows 60 after a sell; FILL feed still only has the open buy.
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 10, Timestamp: tUTC(1, 0)},
	}
	seeds := []PositionSeed{{Symbol: "SOXL", Qty: 60, Avg: 10}}
	got, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("same-side seed lag must stay ok (trust fill book)")
	}
	if got.Day != 0 || got.Week != 0 {
		t.Fatalf("no closes yet → zero realized, got %+v", got)
	}
}

// Opposite-side fill book vs seed still bridges when missing shares the seed
// sign: buy-only history vs a short seed means a larger pre-window short was
// partially covered (REST is authoritative for open qty).
func TestRealizedSeedOppositeSideBridges(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 10, Timestamp: tUTC(1, 0)},
	}
	// REST short 50 @ 10; fills only show the cover of 100 → synth sell 150.
	seeds := []PositionSeed{{Symbol: "SOXL", Qty: -50, Avg: 10}}
	got, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("opposite-side with bridgeable missing must be consistent")
	}
	// Cover 100 of short @ same avg → realized 0.
	if got.Day != 0 || got.Week != 0 {
		t.Fatalf("flat cover → zero realized, got %+v", got)
	}
}

// Sold more than remaining open (abs(endOnly) > abs(seed)) is the common
// long-held partial path and must still bridge, not be marked unreliable.
func TestRealizedLongHeldPartialExitLargerThanRemaining(t *testing.T) {
	day0 := tUTC(0, 0)
	// Pre-lookback long 100 @ 50; sell 80 @ 60 in-window; REST still long 20.
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 80, Price: 60, Timestamp: tUTC(2, 0)},
	}
	seeds := []PositionSeed{{Symbol: "TQQQ", Qty: 20, Avg: 50}}
	got, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("partial exit larger than remaining must bridge (not unreliable)")
	}
	// Realized (60-50)*80 = 800
	if math.Abs(got.Day-800) > 1e-9 {
		t.Fatalf("day = %v, want 800", got.Day)
	}
}

// One ghost symbol must not wipe realized from a clean round-trip on another.
func TestRealizedPartialSkipsUnreconcilableSymbol(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		// Clean SOXL day P&L: +200
		{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 10, Timestamp: tUTC(1, 0)},
		{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 100, Price: 12, Timestamp: tUTC(2, 0)},
		// Full close of pre-lookback TQQQ lot — no seed, ghost short if counted.
		{ID: "3", Symbol: "TQQQ", Side: "sell", Qty: 50, Price: 60, Timestamp: tUTC(3, 0)},
	}
	got, ok, excl := RealizedFromFillsWithSeed(fills, day0, day0, nil)
	if !ok {
		t.Fatal("good symbols must still publish when one name is excluded")
	}
	if math.Abs(got.Day-200) > 1e-9 {
		t.Fatalf("day = %v, want 200 (SOXL only)", got.Day)
	}
	if len(excl) != 1 || excl[0] != "TQQQ" {
		t.Fatalf("excluded = %v, want [TQQQ]", excl)
	}
}

func TestRealizedSeedUndershootStillBridges(t *testing.T) {
	// Same as long-held partial: fill book short of seed → synth open.
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: tUTC(2, 0)},
	}
	seeds := []PositionSeed{{Symbol: "TQQQ", Qty: 60, Avg: 50}}
	got, ok, _ := RealizedFromFillsWithSeed(fills, day0, day0, seeds)
	if !ok {
		t.Fatal("undershoot toward open seed should synthesize and be consistent")
	}
	if math.Abs(got.Day-400) > 1e-9 {
		t.Fatalf("day = %v, want 400", got.Day)
	}
}

// Multi-name: SOXL in-window round-trip + TQQQ long-held partial then full
// close with retained avg must keep TQQQ partials in day totals (no step-down).
func TestRealizedRetainedHintKeepsPartialAfterFullClose(t *testing.T) {
	day0 := tUTC(0, 0)
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 10, Timestamp: tUTC(1, 0)},
		{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 100, Price: 12, Timestamp: tUTC(2, 0)}, // +200
		// Long-held TQQQ: partial then residual close; no REST seed (flat).
		{ID: "3", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: tUTC(3, 0)}, // +400 @50
		{ID: "4", Symbol: "TQQQ", Side: "sell", Qty: 60, Price: 55, Timestamp: tUTC(4, 0)}, // +300 @50
	}
	// Without hint: TQQQ is ghost → day=200 only, excluded.
	got, ok, excl := RealizedFromFillsWithSeed(fills, day0, day0, nil)
	if !ok {
		t.Fatal("SOXL alone must still publish")
	}
	if math.Abs(got.Day-200) > 1e-9 {
		t.Fatalf("without hint day = %v, want 200", got.Day)
	}
	if len(excl) != 1 || excl[0] != "TQQQ" {
		t.Fatalf("excluded = %v, want [TQQQ]", excl)
	}
	// With retained avg from when REST still showed the long: keep both legs.
	hints := []CostBasisHint{{Symbol: "TQQQ", Avg: 50}}
	got, ok, excl = RealizedFromFillsWithHints(fills, day0, day0, nil, hints)
	if !ok {
		t.Fatal("hinted full close must be consistent")
	}
	if len(excl) != 0 {
		t.Fatalf("excluded = %v, want none", excl)
	}
	// (60-50)*40 + (55-50)*60 + SOXL 200 = 400+300+200 = 900
	if math.Abs(got.Day-900) > 1e-9 {
		t.Fatalf("day = %v, want 900", got.Day)
	}
}

func TestRealizedHintDoesNotOverrideLiveSeed(t *testing.T) {
	day0 := tUTC(0, 0)
	// Seed 60@50 bridges sell 40; stale hint avg 10 must not apply.
	fills := []alpaca.Fill{
		{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: tUTC(2, 0)},
	}
	seeds := []PositionSeed{{Symbol: "TQQQ", Qty: 60, Avg: 50}}
	hints := []CostBasisHint{{Symbol: "TQQQ", Avg: 10}}
	got, ok, _ := RealizedFromFillsWithHints(fills, day0, day0, seeds, hints)
	if !ok {
		t.Fatal("live seed must win over hint")
	}
	if math.Abs(got.Day-400) > 1e-9 {
		t.Fatalf("day = %v, want 400 (seed avg 50)", got.Day)
	}
}
