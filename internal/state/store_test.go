package state

import (
	"math"
	"testing"

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
