package state

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"trade-kernel/internal/alpaca"
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

func TestReconcileDayPnLRealizedOnly(t *testing.T) {
	s := NewStore()
	// Equity +1500 vs prior close, of which +200 is open intraday mark → realized +1300.
	// Total unrealized vs cost is +500 (includes multi-day mark).
	s.Reconcile(
		alpaca.Account{Equity: 101500, LastEquity: 100000, Cash: 50000},
		[]alpaca.Position{{Symbol: "AAPL", Qty: 10, Side: "long", UnrealizedIntradayPL: 200, UnrealizedPL: 500}},
		nil,
	)
	p := s.PnL()
	if !p.HasDay || p.Day != 1300 {
		t.Fatalf("day realized = %+v, want HasDay Day=1300", p)
	}
	s.SetWeekPnL(3200)
	p = s.PnL()
	// Week raw 3200 − total open unrealized 500 → 2700 (multi-day marks stripped).
	if !p.HasWeek || p.Week != 2700 {
		t.Fatalf("week realized = %+v, want 2700", p)
	}
}

func TestPnLIgnoresFillZeroedUnrealized(t *testing.T) {
	s := NewStore()
	s.Reconcile(
		alpaca.Account{Equity: 101500, LastEquity: 100000},
		[]alpaca.Position{{Symbol: "AAPL", Qty: 10, Side: "long", UnrealizedIntradayPL: 200, UnrealizedPL: 500, AvgEntryPrice: 100}},
		nil,
	)
	s.SetWeekPnL(3200)
	// Scale-in fill rebuilds position without mark fields (zeros).
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "f1", Symbol: "AAPL", Qty: 5, FilledQty: 5, Side: "buy",
			FilledAvgPrice: 110,
		},
		Price:       110,
		Qty:         5,
		PositionQty: qty(15),
	})
	p := s.PnL()
	if !p.HasDay || p.Day != 1300 {
		t.Fatalf("day must stay snapshotted after fill: %+v", p)
	}
	if !p.HasWeek || p.Week != 2700 {
		t.Fatalf("week must stay snapshotted after fill: %+v", p)
	}
}

// TestSetWeekPnLUsesRESTMarkSnap: week history refresh after a WS fill that
// zeroed mark fields must still strip the last REST open-unrealized snap.
func TestSetWeekPnLUsesRESTMarkSnap(t *testing.T) {
	s := NewStore()
	s.Reconcile(
		alpaca.Account{Equity: 101500, LastEquity: 100000},
		[]alpaca.Position{{Symbol: "AAPL", Qty: 10, Side: "long", UnrealizedIntradayPL: 200, UnrealizedPL: 500, AvgEntryPrice: 100}},
		nil,
	)
	// Fill zeros mark fields on the live positions map.
	s.ApplyUpdate(alpaca.TradeUpdate{
		Event: "fill",
		Order: alpaca.Order{
			ID: "f1", Symbol: "AAPL", Qty: 5, FilledQty: 5, Side: "buy",
			FilledAvgPrice: 110,
		},
		Price:       110,
		Qty:         5,
		PositionQty: qty(15),
	})
	// Week refresh lands in the WS-zeroed window (no Reconcile yet).
	s.SetWeekPnL(3200)
	p := s.PnL()
	if !p.HasWeek || p.Week != 2700 {
		t.Fatalf("week must use REST mark snap not WS-zeroed map: %+v, want Week=2700", p)
	}
}

func TestReconcileClearsDayWhenLastEquityMissing(t *testing.T) {
	s := NewStore()
	s.Reconcile(
		alpaca.Account{Equity: 101500, LastEquity: 100000},
		nil, nil,
	)
	if p := s.PnL(); !p.HasDay {
		t.Fatal("expected day after first reconcile")
	}
	s.Reconcile(alpaca.Account{Equity: 101500, LastEquity: 0}, nil, nil)
	if p := s.PnL(); p.HasDay {
		t.Fatalf("missing last_equity must clear day PnL: %+v", p)
	}
}

type fakeRefresher struct {
	acct    alpaca.Account
	pos     []alpaca.Position
	ord     []alpaca.Order
	hist    alpaca.PortfolioHistory
	histErr error
}

func (f fakeRefresher) Account(context.Context) (alpaca.Account, error) { return f.acct, nil }
func (f fakeRefresher) Positions(context.Context) ([]alpaca.Position, error) {
	return f.pos, nil
}
func (f fakeRefresher) OpenOrders(context.Context, string) ([]alpaca.Order, error) {
	return f.ord, nil
}
func (f fakeRefresher) PortfolioHistory(context.Context, string, string) (alpaca.PortfolioHistory, error) {
	return f.hist, f.histErr
}

func TestRefreshDayPnLFromAccount(t *testing.T) {
	s := NewStore()
	r := fakeRefresher{
		acct: alpaca.Account{Equity: 102000, LastEquity: 100000},
	}
	if err := s.Refresh(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	if !p.HasDay || p.Day != 2000 {
		t.Fatalf("after refresh day = %+v, want Day=2000", p)
	}
	// Refresh no longer pulls week history.
	if p.HasWeek {
		t.Fatalf("week should stay unset without RefreshWeekPnL: %+v", p)
	}
}

func TestRefreshWeekPnLFromHistory(t *testing.T) {
	s := NewStore()
	var hist alpaca.PortfolioHistory
	if err := json.Unmarshal([]byte(`{"profit_loss":["0","800","2500"],"base_value":"100000"}`), &hist); err != nil {
		t.Fatal(err)
	}
	r := fakeRefresher{
		acct: alpaca.Account{Equity: 102000, LastEquity: 100000},
		pos:  []alpaca.Position{{Symbol: "AAPL", Qty: 10, Side: "long", UnrealizedPL: 400, UnrealizedIntradayPL: 100}},
		hist: hist,
	}
	if err := s.Refresh(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshWeekPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	p := s.PnL()
	if !p.HasDay || p.Day != 1900 { // 2000 − 100 open intraday
		t.Fatalf("day = %+v, want Day=1900", p)
	}
	// Week 2500 − total open unrealized 400.
	if !p.HasWeek || p.Week != 2100 {
		t.Fatalf("week = %+v, want 2100", p)
	}
}

func TestRefreshWeekPnLErrorKeepsPriorSample(t *testing.T) {
	s := NewStore()
	s.Reconcile(alpaca.Account{Equity: 102000, LastEquity: 100000}, nil, nil)
	s.SetWeekPnL(2500)
	if !s.PnL().HasWeek {
		t.Fatal("setup: want week set")
	}
	r := fakeRefresher{histErr: context.DeadlineExceeded}
	if err := s.RefreshWeekPnL(context.Background(), r); err == nil {
		t.Fatal("expected history error")
	}
	p := s.PnL()
	// Transient error must not wipe a recent good sample (TTL handles freeze).
	if !p.HasWeek || p.Week != 2500 {
		t.Fatalf("history error must keep prior week: %+v", p)
	}
	if !p.HasDay || p.Day != 2000 {
		t.Fatalf("day must remain: %+v", p)
	}
}

func TestRefreshWeekPnLEmptySeriesClearsWeek(t *testing.T) {
	s := NewStore()
	s.SetWeekPnL(2500)
	r := fakeRefresher{hist: alpaca.PortfolioHistory{}} // no profit_loss / equity
	if err := s.RefreshWeekPnL(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if s.PnL().HasWeek {
		t.Fatal("empty history series must clear week")
	}
}

func TestWeekPnLStaleHidden(t *testing.T) {
	s := NewStore()
	s.SetWeekPnL(1000)
	s.mu.Lock()
	s.weekUpdatedAt = time.Now().Add(-weekPnLStaleAfter - time.Minute)
	s.mu.Unlock()
	if s.PnL().HasWeek {
		t.Fatal("stale week sample must be hidden")
	}
}

func TestRefreshDoesNotFetchWeek(t *testing.T) {
	s := NewStore()
	var hist alpaca.PortfolioHistory
	if err := json.Unmarshal([]byte(`{"profit_loss":["2500"]}`), &hist); err != nil {
		t.Fatal(err)
	}
	r := fakeRefresher{
		acct: alpaca.Account{Equity: 102000, LastEquity: 100000},
		hist: hist,
	}
	if err := s.Refresh(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if s.PnL().HasWeek {
		t.Fatal("Refresh must not set week PnL (use RefreshWeekPnL)")
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
