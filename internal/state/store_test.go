package state

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

func qty(v float64) alpaca.OptionalFloat {
	return alpaca.OptionalFloat{V: v, Valid: true}
}

func TestApplyUpdateFillSetsPosition(t *testing.T) {
	s := NewStore()
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "o1", Symbol: "AAPL", Qty: 100, FilledQty: 100, Side: "buy",
			FilledAvgPrice: 150,
		},
		PositionQty: qty(100),
	})
	if got := s.PositionQty("AAPL"); got != 100 {
		t.Fatalf("PositionQty = %v, want 100", got)
	}
	p := s.Position("AAPL")
	if p == nil || float64(p.AvgEntryPrice) != 150 {
		t.Fatalf("position = %+v, want avg 150", p)
	}
	if len(s.OpenOrders()) != 0 {
		t.Fatal("fill should remove open order")
	}
}

func TestApplyUpdateFillToFlat(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{Symbol: "AAPL", Qty: 50, Side: "long"}}, nil)
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event:       "fill",
		Order:       alpaca.Order{ID: "o2", Symbol: "AAPL", Qty: 50, FilledQty: 50, Side: "sell"},
		PositionQty: qty(0),
	})
	if got := s.PositionQty("AAPL"); got != 0 {
		t.Fatalf("PositionQty = %v, want 0 after flat fill", got)
	}
	if s.Position("AAPL") != nil {
		t.Fatal("expected position removed")
	}
}

func TestApplyUpdateOmitPositionQtyDoesNotClear(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 10}}, nil)
	// Omitted position_qty (Valid=false) must not wipe the position.
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{ID: "oOmit", Symbol: "AAPL", Qty: 10, FilledQty: 10, Side: "buy"},
	})
	if got := s.PositionQty("AAPL"); got != 100 {
		t.Fatalf("PositionQty = %v, want 100 preserved when position_qty omitted", got)
	}
}

func TestApplyUpdatePartialFillKeepsOrderAndPosition(t *testing.T) {
	s := NewStore()
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event:       "partial_fill",
		Order:       alpaca.Order{ID: "o3", Symbol: "NVDA", Qty: 100, FilledQty: 40, Side: "buy", FilledAvgPrice: 900},
		PositionQty: qty(40),
	})
	if got := s.PositionQty("NVDA"); got != 40 {
		t.Fatalf("PositionQty = %v, want 40", got)
	}
	ords := s.OpenOrders()
	if len(ords) != 1 || ords[0].Status != "partial_fill" {
		t.Fatalf("orders = %+v", ords)
	}
}

func TestApplyUpdateShortPosition(t *testing.T) {
	s := NewStore()
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event:       "fill",
		Order:       alpaca.Order{ID: "o4", Symbol: "TSLA", Qty: 10, FilledQty: 10, Side: "sell", FilledAvgPrice: 200},
		PositionQty: qty(-10),
	})
	if got := s.PositionQty("TSLA"); got != -10 {
		t.Fatalf("PositionQty = %v, want -10", got)
	}
	p := s.Position("TSLA")
	if p == nil || p.Side != "short" {
		t.Fatalf("position = %+v, want short", p)
	}
	if float64(p.AvgEntryPrice) != 200 {
		t.Fatalf("avg = %v, want 200", p.AvgEntryPrice)
	}
}

func TestApplyUpdatePartialZeroDoesNotClear(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{Symbol: "AAPL", Qty: 100, Side: "long"}}, nil)
	// Ambiguous zero on partial_fill must not wipe the position.
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event:       "partial_fill",
		Order:       alpaca.Order{ID: "o5", Symbol: "AAPL", Qty: 50, FilledQty: 10, Side: "sell"},
		PositionQty: qty(0),
	})
	if got := s.PositionQty("AAPL"); got != 100 {
		t.Fatalf("PositionQty = %v, want 100 preserved", got)
	}
}

func TestApplyUpdateSideFlipResetsEconomics(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 150,
		MarketValue: 16000, UnrealizedPL: 1000,
	}}, nil)
	// Flip to short 50 via one 150-share sell. Cumulative order avg blends
	// cover+open — without event price we leave avg 0 for REST reconcile.
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "o6", Symbol: "AAPL", Qty: 150, FilledQty: 150, Side: "sell",
			FilledAvgPrice: 155,
		},
		// Event fill price for the print that opened the short residual.
		Price:       157,
		PositionQty: qty(-50),
	})
	p := s.Position("AAPL")
	if p == nil || p.Side != "short" || float64(p.Qty) != 50 {
		t.Fatalf("position = %+v", p)
	}
	if float64(p.AvgEntryPrice) != 157 {
		t.Fatalf("avg after flip = %v, want 157 from event price", p.AvgEntryPrice)
	}
	if float64(p.UnrealizedPL) != 0 || float64(p.MarketValue) != 0 {
		t.Fatalf("stale economics kept: mv=%v upl=%v", p.MarketValue, p.UnrealizedPL)
	}
}

func TestApplyUpdateSideFlipOvershootLeavesAvgZeroWithoutEventPrice(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 150,
	}}, nil)
	// 150-share sell → short 50; FilledAvgPrice blends cover+open.
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "o6b", Symbol: "AAPL", Qty: 150, FilledQty: 150, Side: "sell",
			FilledAvgPrice: 155,
		},
		PositionQty: qty(-50),
	})
	p := s.Position("AAPL")
	if p == nil || p.Side != "short" {
		t.Fatalf("position = %+v", p)
	}
	if float64(p.AvgEntryPrice) != 0 {
		t.Fatalf("avg after overshoot flip = %v, want 0 until REST reconcile", p.AvgEntryPrice)
	}
}

func TestApplyUpdateSideFlipExactOpenUsesOrderAvg(t *testing.T) {
	// Flat → short 50 with a pure open order: FilledQty == absQty.
	s := NewStore()
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "o6c", Symbol: "AAPL", Qty: 50, FilledQty: 50, Side: "sell",
			FilledAvgPrice: 155,
		},
		PositionQty: qty(-50),
	})
	p := s.Position("AAPL")
	if p == nil || float64(p.AvgEntryPrice) != 155 {
		t.Fatalf("position = %+v, want avg 155", p)
	}
}

func TestApplyUpdateScaleInRecomputesAvg(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 100,
		UnrealizedPL: 500,
	}}, nil)
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "o7", Symbol: "AAPL", Qty: 100, FilledQty: 100, Side: "buy",
			FilledAvgPrice: 120,
		},
		PositionQty: qty(200),
	})
	p := s.Position("AAPL")
	if p == nil {
		t.Fatal("nil position")
	}
	// Weighted: (100*100 + 100*120) / 200 = 110
	if float64(p.AvgEntryPrice) != 110 {
		t.Fatalf("avg = %v, want 110", p.AvgEntryPrice)
	}
	if float64(p.UnrealizedPL) != 0 {
		t.Fatalf("uPL should clear on size change, got %v", p.UnrealizedPL)
	}
}

// Multi-partial on a brand-new position: FilledAvgPrice is cumulative, so
// using it as the incremental print would corrupt avg (110+130 → 115).
// After both partials the true avg must be 120.
func TestApplyUpdateMultiPartialNewPositionAvg(t *testing.T) {
	s := NewStore()
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "partial_fill",
		Order: alpaca.Order{
			ID: "mp1", Symbol: "AAPL", Qty: 80, FilledQty: 40, Side: "buy",
			FilledAvgPrice: 110,
		},
		Price:       110,
		Qty:         40,
		PositionQty: qty(40),
	})
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "partial_fill",
		Order: alpaca.Order{
			ID: "mp1", Symbol: "AAPL", Qty: 80, FilledQty: 80, Side: "buy",
			FilledAvgPrice: 120, // cumulative of 110 and 130
		},
		// No event price — force derivation from cumulative filled avg change.
		PositionQty: qty(80),
	})
	p := s.Position("AAPL")
	if p == nil {
		t.Fatal("nil position")
	}
	if float64(p.AvgEntryPrice) != 120 {
		t.Fatalf("avg = %v, want 120 (not corrupted cumulative reweight)", p.AvgEntryPrice)
	}
}

// Scale-in multi-partial: derive incremental print from order filled state.
func TestApplyUpdateMultiPartialScaleInAvg(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 100,
	}}, nil)
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "partial_fill",
		Order: alpaca.Order{
			ID: "mp2", Symbol: "AAPL", Qty: 80, FilledQty: 40, Side: "buy",
			FilledAvgPrice: 110,
		},
		PositionQty: qty(140),
	})
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "mp2", Symbol: "AAPL", Qty: 80, FilledQty: 80, Side: "buy",
			FilledAvgPrice: 120, // means second print was 130
		},
		PositionQty: qty(180),
	})
	p := s.Position("AAPL")
	if p == nil {
		t.Fatal("nil position")
	}
	// (100*100 + 40*110 + 40*130) / 180 = 19600/180
	want := (100.0*100 + 40*110 + 40*130) / 180
	if math.Abs(float64(p.AvgEntryPrice)-want) > 1e-9 {
		t.Fatalf("avg = %v, want %v", p.AvgEntryPrice, want)
	}
}

func TestApplyUpdatePrefersEventPrice(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 100,
	}}, nil)
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "ep1", Symbol: "AAPL", Qty: 50, FilledQty: 50, Side: "buy",
			// Deliberately wrong cumulative if misused as incremental alone on multi-fill,
			// but single fill of 50 @ event 150 with filled_avg 150 is fine either way.
			FilledAvgPrice: 150,
		},
		Price:       150,
		Qty:         50,
		PositionQty: qty(150),
	})
	p := s.Position("AAPL")
	if p == nil {
		t.Fatal("nil position")
	}
	// (100*100 + 50*150) / 150 = 350/3 ≈ 116.666...
	want := (100.0*100 + 50*150) / 150
	if math.Abs(float64(p.AvgEntryPrice)-want) > 1e-9 {
		t.Fatalf("avg = %v, want %v", p.AvgEntryPrice, want)
	}
}

// Concurrent fills can make PositionQty jump by more than this event's size.
// Scale-in must weight only this fill's qty at this fill's price.
func TestApplyUpdateScaleInUsesFillSizeNotPositionDelta(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 100,
	}}, nil)
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "cf1", Symbol: "AAPL", Qty: 50, FilledQty: 50, Side: "buy",
			FilledAvgPrice: 120,
		},
		Price: 120,
		Qty:   50,
		// Position already includes another concurrent +50 that this event
		// must not attribute to price 120.
		PositionQty: qty(200),
	})
	p := s.Position("AAPL")
	if p == nil {
		t.Fatal("nil position")
	}
	if float64(p.Qty) != 200 {
		t.Fatalf("qty = %v, want 200 from broker", p.Qty)
	}
	// Known fill 50@120; concurrent residual 50 attributed at prevAvg 100 so
	// avg is consistent with qty=200: (100*100 + 50*120 + 50*100) / 200 = 105.
	// Not (100*100+100*120)/200 = 110 (full jump at 120) and not /150 with qty 200.
	want := (100.0*100 + 50*120 + 50*100) / 200
	if math.Abs(float64(p.AvgEntryPrice)-want) > 1e-9 {
		t.Fatalf("avg = %v, want %v (fill size + residual@prevAvg)", p.AvgEntryPrice, want)
	}
}

func TestSetRealizedPnL(t *testing.T) {
	s := NewStore()
	s.SetRealizedPnL(1300, 1000)
	p := s.PnL()
	if !p.HasDay || p.Day != 1300 || !p.HasWeek || p.Week != 1000 {
		t.Fatalf("pnl = %+v, want day=1300 week=1000", p)
	}
}

func TestRealizedPnLIndependentOfReconcile(t *testing.T) {
	s := NewStore()
	s.SetRealizedPnL(500, 800)
	// Reconcile must not clobber fill-based realized figures.
	s.Reconcile(
		alpaca.Account{Equity: 101500, LastEquity: 100000},
		[]alpaca.Position{{Symbol: "AAPL", Qty: 10, Side: "long", UnrealizedIntradayPL: 200}},
		nil,
	)
	p := s.PnL()
	if !p.HasDay || p.Day != 500 || !p.HasWeek || p.Week != 800 {
		t.Fatalf("reconcile must not overwrite realized: %+v", p)
	}
}

type fakeRefresher struct {
	acct     alpaca.Account
	pos      []alpaca.Position
	ord      []alpaca.Order
	fills    []alpaca.Fill
	fillsErr error
	closed   []alpaca.Order
	closedEr error
}

func (f fakeRefresher) Account(context.Context) (alpaca.Account, error) { return f.acct, nil }
func (f fakeRefresher) Positions(context.Context) ([]alpaca.Position, error) {
	return f.pos, nil
}
func (f fakeRefresher) OpenOrders(context.Context, string) ([]alpaca.Order, error) {
	return f.ord, nil
}
func (f fakeRefresher) Fills(context.Context, time.Time, time.Time) ([]alpaca.Fill, error) {
	return f.fills, f.fillsErr
}
func (f fakeRefresher) ClosedOrders(context.Context, time.Time, time.Time) ([]alpaca.Order, error) {
	return f.closed, f.closedEr
}

func TestRefreshDoesNotFetchRealized(t *testing.T) {
	s := NewStore()
	r := fakeRefresher{
		acct: alpaca.Account{Equity: 102000, LastEquity: 100000},
		// Would produce realized if RefreshRealizedPnL were called.
		fills: []alpaca.Fill{{
			ID: "1", Symbol: "A", Side: "buy", Qty: 10, Price: 10,
			Timestamp: time.Now(),
		}},
	}
	if err := s.Refresh(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if s.PnL().HasDay || s.PnL().HasWeek {
		t.Fatal("Refresh must not set realized PnL (use RefreshRealizedPnL)")
	}
}

func TestRefreshRealizedPnLFromFills(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	week0 := session.WeekStart(now)
	r := fakeRefresher{
		fills: []alpaca.Fill{
			{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 100, Timestamp: day0.Add(time.Hour)},
			{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 100, Price: 105, Timestamp: day0.Add(2 * time.Hour)},
			// Earlier this week, before day boundary if day0 > week0:
			{ID: "3", Symbol: "X", Side: "buy", Qty: 10, Price: 10, Timestamp: week0.Add(time.Minute)},
			{ID: "4", Symbol: "X", Side: "sell", Qty: 10, Price: 12, Timestamp: week0.Add(2 * time.Minute)},
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	// SOXL +500 today; X +20 this week. If day0==week0 (Sun after 20:00), both in day.
	if !p.HasDay || !p.HasWeek {
		t.Fatalf("want both set: %+v", p)
	}
	if p.Day < 500-1e-9 {
		t.Fatalf("day = %v, want at least 500 (SOXL round-trip)", p.Day)
	}
	if p.Week < 520-1e-9 && week0.Before(day0) {
		// week includes X +20 and SOXL +500 when week starts before day
		t.Fatalf("week = %v, want >= 520", p.Week)
	}
	if math.Abs(p.Day-500) > 1e-9 && day0.Equal(week0) {
		// when day and week share boundary, X also counts in day
		if math.Abs(p.Day-520) > 1e-9 {
			t.Fatalf("day = %v, want 500 or 520", p.Day)
		}
	}
}

func TestRefreshRealizedPnLFallsBackToOrders(t *testing.T) {
	// Cold start (no prior sample): ClosedOrders may seed rday/rwk.
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	r := fakeRefresher{
		fillsErr: context.DeadlineExceeded,
		closed: []alpaca.Order{
			{ID: "o1", Symbol: "A", Side: "buy", FilledQty: 50, FilledAvgPrice: 10, FilledAt: day0.Add(time.Hour)},
			{ID: "o2", Symbol: "A", Side: "sell", FilledQty: 50, FilledAvgPrice: 11, FilledAt: day0.Add(2 * time.Hour)},
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-50) > 1e-9 {
		t.Fatalf("order fallback day = %+v, want 50", p)
	}
}

func TestRefreshRealizedPnLErrorKeepsPrior(t *testing.T) {
	s := NewStore()
	s.SetRealizedPnL(100, 200)
	r := fakeRefresher{fillsErr: context.DeadlineExceeded, closedEr: context.DeadlineExceeded}
	if err := s.RefreshRealizedPnL(context.Background(), r); err == nil {
		t.Fatal("expected error")
	}
	p := s.PnL()
	if !p.HasDay || p.Day != 100 || !p.HasWeek || p.Week != 200 {
		t.Fatalf("error must keep prior: %+v", p)
	}
}

// A prior fill-derived sample must not be overwritten by ClosedOrders, and
// the fill cache must survive a transient Fills failure.
func TestRefreshRealizedPnLFillsErrorKeepsPriorAndCache(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	f1 := alpaca.Fill{ID: "1", Symbol: "A", Side: "buy", Qty: 10, Price: 10, Timestamp: day0.Add(time.Hour)}
	f2 := alpaca.Fill{ID: "2", Symbol: "A", Side: "sell", Qty: 10, Price: 11, Timestamp: day0.Add(2 * time.Hour)}
	r := &countingFillsRefresher{fakeRefresher: fakeRefresher{fills: []alpaca.Fill{f1, f2}}}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !s.PnL().HasDay || math.Abs(s.PnL().Day-10) > 1e-9 {
		t.Fatalf("setup day = %+v, want 10", s.PnL())
	}
	s.mu.RLock()
	cacheLen := len(s.fillCache)
	cacheAfter := s.fillCacheAfter
	s.mu.RUnlock()
	if cacheLen != 2 {
		t.Fatalf("cache len = %d, want 2", cacheLen)
	}

	// Fills fail; ClosedOrders would report a wrong higher total if used.
	r.fakeRefresher = fakeRefresher{
		fillsErr: context.DeadlineExceeded,
		closed: []alpaca.Order{
			{ID: "o1", Symbol: "A", Side: "buy", FilledQty: 100, FilledAvgPrice: 10, FilledAt: day0.Add(time.Hour)},
			{ID: "o2", Symbol: "A", Side: "sell", FilledQty: 100, FilledAvgPrice: 20, FilledAt: day0.Add(2 * time.Hour)},
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err == nil {
		t.Fatal("expected fills error with prior kept")
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-10) > 1e-9 {
		t.Fatalf("must keep fill-derived day=10, got %+v (orders would be 1000)", p)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.fillCache) != cacheLen || !s.fillCacheAfter.Equal(cacheAfter) {
		t.Fatalf("fill cache must survive Fills error: len=%d after=%v", len(s.fillCache), s.fillCacheAfter)
	}
}

func TestRefreshRealizedPnLSeedsLongHeldPartial(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	// Still long 60 @ 50 after selling 40 today; open predates lookback.
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "TQQQ", Qty: 60, Side: "long", AvgEntryPrice: 50,
	}}, nil)
	r := fakeRefresher{
		fills: []alpaca.Fill{
			{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: day0.Add(time.Hour)},
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-400) > 1e-9 {
		t.Fatalf("seeded partial close day = %+v, want 400", p)
	}
}

func TestRefreshRealizedPnLInconsistentKeepsPrior(t *testing.T) {
	s := NewStore()
	s.SetRealizedPnL(100, 200)
	now := time.Now()
	day0 := session.DayStart(now)
	// Sell with no open seed and no prior buy → ghost short inventory.
	// Only unreconcilable symbols remain → keep prior sample (do not clear).
	r := fakeRefresher{
		fills: []alpaca.Fill{
			{ID: "1", Symbol: "ZZZ", Side: "sell", Qty: 10, Price: 5, Timestamp: day0.Add(time.Hour)},
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err == nil {
		t.Fatal("expected inventory inconsistency error")
	}
	p := s.PnL()
	if !p.HasDay || p.Day != 100 || !p.HasWeek || p.Week != 200 {
		t.Fatalf("inconsistent inventory must keep prior realized: %+v", p)
	}
}

// One unreconcilable name must not blank account-wide rday when another is clean.
func TestRefreshRealizedPnLPartialSkipsBadSymbol(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	r := fakeRefresher{
		fills: []alpaca.Fill{
			{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 100, Timestamp: day0.Add(time.Hour)},
			{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 100, Price: 105, Timestamp: day0.Add(2 * time.Hour)},
			{ID: "3", Symbol: "ZZZ", Side: "sell", Qty: 10, Price: 5, Timestamp: day0.Add(3 * time.Hour)},
		},
	}
	err := s.RefreshRealizedPnL(context.Background(), r)
	if err == nil {
		t.Fatal("expected soft warning about excluded symbols")
	}
	if !strings.Contains(err.Error(), "partial") || !strings.Contains(err.Error(), "ZZZ") {
		t.Fatalf("error = %v, want partial sample mentioning ZZZ", err)
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-500) > 1e-9 {
		t.Fatalf("partial day = %+v, want 500 (SOXL only)", p)
	}
	if !p.Partial || !strings.Contains(p.PartialNote, "ZZZ") {
		t.Fatalf("partial flags = %+v, want Partial with ZZZ note", p)
	}
}

// After REST goes flat, retained avg must keep partial+full close of a
// long-held name continuous with other symbols' realized.
func TestRefreshRealizedPnLRetainedBasisAfterFullClose(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	// First reconcile: still long residual after a partial (seed path).
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "TQQQ", Qty: 60, Side: "long", AvgEntryPrice: 50,
	}}, nil)
	// Flat: retain avg 50 from prior REST snapshot.
	s.Reconcile(alpaca.Account{}, nil, nil)
	r := fakeRefresher{
		fills: []alpaca.Fill{
			{ID: "1", Symbol: "SOXL", Side: "buy", Qty: 100, Price: 10, Timestamp: day0.Add(time.Hour)},
			{ID: "2", Symbol: "SOXL", Side: "sell", Qty: 100, Price: 12, Timestamp: day0.Add(2 * time.Hour)}, // +200
			{ID: "3", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: day0.Add(3 * time.Hour)},  // +400
			{ID: "4", Symbol: "TQQQ", Side: "sell", Qty: 60, Price: 55, Timestamp: day0.Add(4 * time.Hour)},  // +300
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-900) > 1e-9 {
		t.Fatalf("retained full close day = %+v, want 900", p)
	}
	if p.Partial {
		t.Fatalf("full reconcilable sample must not be partial: %+v", p)
	}
}

// Seeds must come from the last REST reconcile, not a fresher WS position.
func TestRefreshRealizedPnLSeedsFromRESTNotWS(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	// REST still shows long 100 @ 50 (fills not yet reflected in activities).
	s.Reconcile(alpaca.Account{}, []alpaca.Position{{
		Symbol: "TQQQ", Qty: 100, Side: "long", AvgEntryPrice: 50,
	}}, nil)
	// WS already applied a sell to 60 — must not drive seeds (would overshoot
	// vs fills that still lack the sell, or undersize the synthetic open).
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{ID: "o1", Symbol: "TQQQ", Side: "sell", FilledQty: 40, FilledAvgPrice: 60},
		PositionQty: alpaca.OptionalFloat{Valid: true, V: 60},
		Price:       60,
		Qty:         40,
	})
	// Only partial sell in fill history; REST seed 100 @ 50 bridges correctly.
	r := fakeRefresher{
		fills: []alpaca.Fill{
			{ID: "1", Symbol: "TQQQ", Side: "sell", Qty: 40, Price: 60, Timestamp: day0.Add(time.Hour)},
		},
	}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	// Sold 40 of 100 @50 → (60-50)*40 = 400; seed qty 100 not WS 60.
	if !p.HasDay || math.Abs(p.Day-400) > 1e-9 {
		t.Fatalf("REST-seeded day = %+v, want 400", p)
	}
}

func TestMergeFillsByIDDropsEmptyIDs(t *testing.T) {
	base := []alpaca.Fill{
		{ID: "1", Symbol: "A", Side: "buy", Qty: 1, Price: 1},
		{ID: "", Symbol: "A", Side: "buy", Qty: 1, Price: 1},
	}
	newer := []alpaca.Fill{
		{ID: "1", Symbol: "A", Side: "buy", Qty: 1, Price: 1},
		{ID: "", Symbol: "A", Side: "sell", Qty: 1, Price: 2},
		{ID: "2", Symbol: "A", Side: "sell", Qty: 1, Price: 2},
	}
	got := mergeFillsByID(base, newer)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (empty ids dropped, id1 de-duped)", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("ids = %q,%q want 1,2", got[0].ID, got[1].ID)
	}
}

// countingFillsRefresher records how many Fills calls and the after bound.
type countingFillsRefresher struct {
	fakeRefresher
	calls int
	after []time.Time
}

func (c *countingFillsRefresher) Fills(ctx context.Context, after, until time.Time) ([]alpaca.Fill, error) {
	c.calls++
	c.after = append(c.after, after)
	return c.fakeRefresher.Fills(ctx, after, until)
}

func TestRefreshRealizedPnLFillCacheDelta(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	f1 := alpaca.Fill{ID: "1", Symbol: "A", Side: "buy", Qty: 10, Price: 10, Timestamp: day0.Add(time.Hour)}
	f2 := alpaca.Fill{ID: "2", Symbol: "A", Side: "sell", Qty: 10, Price: 11, Timestamp: day0.Add(2 * time.Hour)}
	r := &countingFillsRefresher{fakeRefresher: fakeRefresher{fills: []alpaca.Fill{f1, f2}}}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("first refresh calls = %d, want 1", r.calls)
	}
	// Second refresh with same fills (delta may re-fetch overlap) must still
	// produce correct PnL and use a later-or-equal after bound when cached.
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("second refresh calls = %d, want 2", r.calls)
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-10) > 1e-9 {
		t.Fatalf("cached refresh day = %+v, want 10", p)
	}
	// Delta after should be at/after first fill time minus overlap, not a full
	// re-scan from weekStart−lookback only if cache held — accept either but
	// require second after >= first after.
	if r.after[1].Before(r.after[0]) {
		t.Fatalf("delta after %v before full after %v", r.after[1], r.after[0])
	}
}

// After fillCacheFullRefreshEvery, a warm cache must full-walk again so
// delayed FILLs older than the high-water mark are discovered.
func TestRefreshRealizedPnLWarmCachePeriodicFullHeal(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	f1 := alpaca.Fill{ID: "1", Symbol: "A", Side: "buy", Qty: 10, Price: 10, Timestamp: day0.Add(time.Hour)}
	f2 := alpaca.Fill{ID: "2", Symbol: "A", Side: "sell", Qty: 10, Price: 11, Timestamp: day0.Add(2 * time.Hour)}
	// Complete round-trip that arrived late with timestamps older than the HWM
	// (would miss the 2s delta overlap).
	dBuy := alpaca.Fill{ID: "3", Symbol: "B", Side: "buy", Qty: 2, Price: 5, Timestamp: day0.Add(30 * time.Minute)}
	dSell := alpaca.Fill{ID: "4", Symbol: "B", Side: "sell", Qty: 2, Price: 6, Timestamp: day0.Add(40 * time.Minute)}

	r := &countingFillsRefresher{fakeRefresher: fakeRefresher{fills: []alpaca.Fill{f1, f2}}}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	// Age the last full walk so the next refresh forces a full heal.
	s.mu.Lock()
	s.fillCacheLastFull = time.Now().Add(-fillCacheFullRefreshEvery - time.Minute)
	s.mu.Unlock()

	r.fakeRefresher.fills = []alpaca.Fill{f1, f2, dBuy, dSell}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("calls = %d, want 2", r.calls)
	}
	// Full heal uses the original lookback after, not the HWM delta.
	if !r.after[1].Equal(r.after[0]) {
		t.Fatalf("periodic full heal after %v != first full after %v", r.after[1], r.after[0])
	}
	s.mu.RLock()
	n := len(s.fillCache)
	s.mu.RUnlock()
	if n != 4 {
		t.Fatalf("cache len = %d, want 4 (delayed fills absorbed)", n)
	}
	p := s.PnL()
	// A: +10, B: +2
	if !p.HasDay || math.Abs(p.Day-12) > 1e-9 {
		t.Fatalf("day = %+v, want 12 after delayed round-trip", p)
	}
}

// Empty successful FILL history must not re-fetch the full lookback every tick.
func TestRefreshRealizedPnLEmptyCacheShortRecheck(t *testing.T) {
	s := NewStore()
	r := &countingFillsRefresher{fakeRefresher: fakeRefresher{fills: nil}}
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 1 {
		t.Fatalf("first empty refresh calls = %d, want 1", r.calls)
	}
	fullAfter := r.after[0]
	if err := s.RefreshRealizedPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if r.calls != 2 {
		t.Fatalf("second empty refresh calls = %d, want 2", r.calls)
	}
	// Second call should use a much later after bound (empty-cache recheck tail).
	if !r.after[1].After(fullAfter) {
		t.Fatalf("empty-cache recheck after %v should be after full lookback after %v", r.after[1], fullAfter)
	}
	// empty recheck is fillCacheEmptyRecheck (30m): second after ≈ until−30m, not a 2m tail.
	// fullAfter is weekStart−180d, so after[1] is many months later — already checked.
	// Pin that recheck is not still the full lookback (would equal fullAfter).
	if r.after[1].Equal(fullAfter) {
		t.Fatal("empty-cache recheck must not re-use full lookback after")
	}
}

// Mid-pagination Fills errors must still warm the fill cache so the next tick
// resumes from the high-water mark instead of re-walking page 1.
func TestRefreshRealizedPnLPartialFillsCheckpointsCache(t *testing.T) {
	s := NewStore()
	now := time.Now()
	day0 := session.DayStart(now)
	partial := []alpaca.Fill{
		{ID: "1", Symbol: "A", Side: "buy", Qty: 10, Price: 10, Timestamp: day0.Add(time.Hour)},
	}
	r := fakeRefresher{fills: partial, fillsErr: context.DeadlineExceeded}
	if err := s.RefreshRealizedPnL(context.Background(), r); err == nil {
		t.Fatal("expected incomplete fills error")
	}
	s.mu.RLock()
	if len(s.fillCache) != 1 || s.fillCache[0].ID != "1" {
		t.Fatalf("partial fills must checkpoint cache, got %+v", s.fillCache)
	}
	if s.fillCacheAfter.IsZero() {
		t.Fatal("fillCacheAfter must be set after partial checkpoint")
	}
	s.mu.RUnlock()
	// No complete sample yet.
	if s.PnL().HasDay {
		t.Fatal("incomplete walk must not publish rday")
	}

	// Next refresh completes via delta merge (buy+sell) and publishes PnL.
	r2 := &countingFillsRefresher{fakeRefresher: fakeRefresher{fills: []alpaca.Fill{
		{ID: "1", Symbol: "A", Side: "buy", Qty: 10, Price: 10, Timestamp: day0.Add(time.Hour)},
		{ID: "2", Symbol: "A", Side: "sell", Qty: 10, Price: 11, Timestamp: day0.Add(2 * time.Hour)},
	}}}
	if err := s.RefreshRealizedPnL(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	if r2.calls != 1 {
		t.Fatalf("calls = %d, want 1", r2.calls)
	}
	// Delta after should be near the cached fill, not a full 180d rescan.
	if len(r2.after) != 1 || r2.after[0].Before(day0) {
		t.Fatalf("delta after = %v, want at/after day0 (resume from cache)", r2.after)
	}
	p := s.PnL()
	if !p.HasDay || math.Abs(p.Day-10) > 1e-9 {
		t.Fatalf("day = %+v, want 10 after complete delta", p)
	}
}

func TestRealizedPnLStaleHidden(t *testing.T) {
	s := NewStore()
	s.SetRealizedPnL(100, 200)
	s.mu.Lock()
	s.pnlUpdatedAt = time.Now().Add(-realizedPnLStaleAfter - time.Minute)
	s.mu.Unlock()
	p := s.PnL()
	if p.HasDay || p.HasWeek {
		t.Fatalf("stale realized must be hidden: %+v", p)
	}
}

// TestReconciledGate: a fresh store reports Reconciled()==false until the
// first Reconcile lands. Order entry in the UI gates on this so a failed
// startup snapshot can't let the operator trade against an empty view.
func TestReconciledGate(t *testing.T) {
	s := NewStore()
	if s.Reconciled() {
		t.Fatal("fresh store must report Reconciled()==false")
	}
	s.Reconcile(alpaca.Account{Equity: 100, LastEquity: 100}, nil, nil)
	if !s.Reconciled() {
		t.Fatal("Reconcile must flip Reconciled() to true")
	}
}

// TestReconciledRequiresSuccess: a failed Refresh (REST error) must not flip
// the gate — only a successful Reconcile counts.
func TestReconciledRequiresSuccess(t *testing.T) {
	s := NewStore()
	r := errRefresher{}
	if err := s.Refresh(context.Background(), r); err == nil {
		t.Fatal("expected Refresh error from errRefresher")
	}
	if s.Reconciled() {
		t.Fatal("failed Refresh must not flip Reconciled()")
	}
}

type errRefresher struct{}

func (errRefresher) Account(context.Context) (alpaca.Account, error) {
	return alpaca.Account{}, context.DeadlineExceeded
}
func (errRefresher) Positions(context.Context) ([]alpaca.Position, error) {
	return nil, nil
}
func (errRefresher) OpenOrders(context.Context, string) ([]alpaca.Order, error) {
	return nil, nil
}
