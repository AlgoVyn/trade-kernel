package risk

import (
	"testing"
	"time"
)

type fakeLookup map[string]float64

func (f fakeLookup) PositionQty(s string) float64 { return f[s] }

// fakeExposure implements PositionLookup + ExposureLookup under one snapshot.
type fakeExposure struct {
	pos, buy, sell float64
}

func (f fakeExposure) PositionQty(string) float64 { return f.pos }

func (f fakeExposure) Exposure(string) (pos, workingBuy, workingSell float64) {
	return f.pos, f.buy, f.sell
}

func TestCheckerLimits(t *testing.T) {
	now := time.Now()
	c := NewChecker(Limits{MaxOrderQty: 500, MaxPositionQty: 1000}, fakeLookup{"AAPL": 800}, func() time.Time { return now })

	if err := c.Check("AAPL", "buy", 600); err == nil {
		t.Fatal("expected max order size rejection")
	}
	if err := c.Check("AAPL", "buy", 300); err == nil {
		t.Fatal("expected projected position rejection (800+300 > 1000)")
	}
	// Reducing exposure is always allowed.
	if err := c.Check("AAPL", "sell", 300); err != nil {
		t.Fatalf("reduce should pass: %v", err)
	}
	// Small add within cap passes — but first call was debounced? No:
	// the rejected sell above consumed the debounce key; different key.
	if err := c.Check("AAPL", "buy", 100); err != nil {
		t.Fatalf("within limits should pass: %v", err)
	}
}

func TestCheckerDebounce(t *testing.T) {
	base := time.Now()
	cur := base
	c := NewChecker(Limits{Debounce: 300 * time.Millisecond}, fakeLookup{}, func() time.Time { return cur })

	if err := c.Check("AAPL", "buy", 100); err != nil {
		t.Fatal(err)
	}
	// Check alone does not record — declined confirm must not burn the window.
	if err := c.Check("AAPL", "buy", 100); err != nil {
		t.Fatalf("without Record, second Check should still pass: %v", err)
	}
	// Record at submit commit (before broker ACK) blocks in-flight duplicates.
	if err := c.Record("AAPL", "buy", 100); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := c.Check("AAPL", "buy", 100); err == nil {
		t.Fatal("expected debounce rejection after Record")
	}
	// Second Record of the same key fails under the lock (TOCTOU close).
	if err := c.Record("AAPL", "buy", 100); err == nil {
		t.Fatal("expected second Record to fail within debounce window")
	}
	// Different order is not debounced.
	if err := c.Check("AAPL", "buy", 200); err != nil {
		t.Fatalf("different qty should pass: %v", err)
	}
	// After the window the same order passes.
	cur = base.Add(400 * time.Millisecond)
	if err := c.Check("AAPL", "buy", 100); err != nil {
		t.Fatalf("after window should pass: %v", err)
	}
	if err := c.Record("AAPL", "buy", 100); err != nil {
		t.Fatalf("Record after window: %v", err)
	}
}

func TestCheckerLock(t *testing.T) {
	c := NewChecker(Limits{}, fakeLookup{}, nil)
	c.Lock("manual lock")
	if err := c.Check("AAPL", "buy", 1); err == nil {
		t.Fatal("locked checker must reject")
	}
	if locked, _ := c.Locked(); !locked {
		t.Fatal("Locked() = false")
	}
	// Flatten under lock is allowed (SkipLock).
	if err := c.CheckOpts("AAPL", "sell", 100, CheckOpts{SkipLock: true, SkipDebounce: true}); err != nil {
		t.Fatalf("flatten under lock should pass: %v", err)
	}
	c.Unlock()
	if err := c.Check("AAPL", "buy", 1); err != nil {
		t.Fatalf("unlocked: %v", err)
	}
}

// fakeWorking maps "symbol|side" → remaining qty.
type fakeWorking map[string]float64

func (f fakeWorking) WorkingSideQty(symbol, side string) float64 {
	return f[symbol+"|"+side]
}

func TestCheckerWorkingOrdersInProjection(t *testing.T) {
	// Position 800, working buy 150 → buy 100 projects 1050 > 1000.
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 800}, nil)
	c.SetWorkingLookup(fakeWorking{"AAPL|buy": 150})
	if err := c.Check("AAPL", "buy", 100); err == nil {
		t.Fatal("expected projected rejection including working buys")
	}
	// Reduce still allowed (strictly lowers |cur+working|).
	if err := c.Check("AAPL", "sell", 100); err != nil {
		t.Fatalf("reduce should pass: %v", err)
	}
}

// ExposureLookup path: single-shot pos+working must drive the same projection.
func TestCheckerExposureLookupInProjection(t *testing.T) {
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeExposure{pos: 800, buy: 150}, nil)
	// WorkingLookup not set — Exposure on PositionLookup is enough.
	if err := c.Check("AAPL", "buy", 100); err == nil {
		t.Fatal("expected projected rejection via ExposureLookup")
	}
	if err := c.Check("AAPL", "sell", 100); err != nil {
		t.Fatalf("reduce should pass via ExposureLookup: %v", err)
	}
	// Within cap.
	c2 := NewChecker(Limits{MaxPositionQty: 1000}, fakeExposure{pos: 800, buy: 50}, nil)
	if err := c2.Check("AAPL", "buy", 100); err != nil {
		t.Fatalf("within cap should pass: %v", err)
	}
}

// Long + stacked working buys: a small sell that reduces |position| alone must
// still be allowed when it reduces net projected exposure; a same-side add
// that grows net exposure past the cap must be blocked.
func TestCheckerReduceVsWorkingNetExposure(t *testing.T) {
	// Long 100, working buys 1000 → base 1100 over max 1000.
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 100}, nil)
	c.SetWorkingLookup(fakeWorking{"AAPL|buy": 1000})
	// Small sell reduces net 1100→1050: allowed even though still over cap.
	if err := c.Check("AAPL", "sell", 50); err != nil {
		t.Fatalf("net-reducing sell should pass: %v", err)
	}
	// Same-side add grows 1100→1150: blocked.
	if err := c.Check("AAPL", "buy", 50); err == nil {
		t.Fatal("expected rejection: stacked working + buy grows over-cap exposure")
	}
}

func TestCheckerSkipMaxOrder(t *testing.T) {
	c := NewChecker(Limits{MaxOrderQty: 100}, fakeLookup{"AAPL": 500}, nil)
	if err := c.Check("AAPL", "sell", 500); err == nil {
		t.Fatal("expected max order rejection without SkipMaxOrder")
	}
	if err := c.CheckOpts("AAPL", "sell", 500, CheckOpts{SkipMaxOrder: true, SkipDebounce: true}); err != nil {
		t.Fatalf("SkipMaxOrder flatten-size should pass: %v", err)
	}
}

// Flatten must not be blocked by same-side working that would fail max-position
// projection (e.g. long exit with large resting sells, or cover with resting buys).
func TestCheckerSkipMaxPosition(t *testing.T) {
	// Short 100, resting buys 1000: cover buy 100 projects base -100+1000 = 900 → 1000 at max;
	// grow case: short 100 + working buys 2000 + buy 100 → base 1900 → proj 2000 over max.
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": -100}, nil)
	c.SetWorkingLookup(fakeWorking{"AAPL|buy": 2000})
	if err := c.Check("AAPL", "buy", 100); err == nil {
		t.Fatal("expected max-position rejection with stacked working buys")
	}
	if err := c.CheckOpts("AAPL", "buy", 100, CheckOpts{
		SkipLock: true, SkipDebounce: true, SkipMaxOrder: true, SkipMaxPosition: true,
	}); err != nil {
		t.Fatalf("SkipMaxPosition flatten cover should pass: %v", err)
	}
	// Long exit with stacked working sells.
	c2 := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 100}, nil)
	c2.SetWorkingLookup(fakeWorking{"AAPL|sell": 2000})
	if err := c2.Check("AAPL", "sell", 100); err == nil {
		t.Fatal("expected max-position rejection with stacked working sells")
	}
	if err := c2.CheckOpts("AAPL", "sell", 100, CheckOpts{SkipMaxPosition: true, SkipDebounce: true}); err != nil {
		t.Fatalf("SkipMaxPosition flatten long should pass: %v", err)
	}
}

// Over-cap equal-magnitude reverse (long 1100 → short 1100) is not a reduce.
func TestCheckerOverCapFullReverseRejected(t *testing.T) {
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 1100}, nil)
	if err := c.Check("AAPL", "sell", 2200); err == nil {
		t.Fatal("expected rejection: over-cap full reverse is not a reduce")
	}
	// Full flatten to flat is still allowed.
	if err := c.Check("AAPL", "sell", 1100); err != nil {
		t.Fatalf("full flatten should pass: %v", err)
	}
	// Partial reduce still allowed while over cap.
	if err := c.Check("AAPL", "sell", 50); err != nil {
		t.Fatalf("partial reduce should pass: %v", err)
	}
}

// Oversize reverse that remains over the cap (long 1100 → short 1050) must not
// pass through the "reduce" door.
func TestCheckerOverCapReverseToOtherSideRejected(t *testing.T) {
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 1100}, nil)
	if err := c.Check("AAPL", "sell", 2150); err == nil {
		t.Fatal("expected rejection: over-cap reverse to short is not a pure exit")
	}
	// Short over cap → reverse to long must also fail.
	c2 := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": -1100}, nil)
	if err := c2.Check("AAPL", "buy", 2150); err == nil {
		t.Fatal("expected rejection: over-cap reverse to long is not a pure exit")
	}
}

func TestCheckerWorkingOppositeSideNoCredit(t *testing.T) {
	// Position 0, resting sell 500 must not allow buy 1500 under max 1000
	// (net working would be -500 + 1500 = 1000 and incorrectly pass).
	c := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 0}, nil)
	c.SetWorkingLookup(fakeWorking{"AAPL|sell": 500})
	if err := c.Check("AAPL", "buy", 1500); err == nil {
		t.Fatal("expected rejection: opposite-side resting must not credit new buys")
	}
	// Same-side working still counts.
	c2 := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 0}, nil)
	c2.SetWorkingLookup(fakeWorking{"AAPL|buy": 600})
	if err := c2.Check("AAPL", "buy", 500); err == nil {
		t.Fatal("expected rejection for stacked same-side buys")
	}
	// Buy within cap with only opposite-side working still uses bare size.
	c3 := NewChecker(Limits{MaxPositionQty: 1000}, fakeLookup{"AAPL": 0}, nil)
	c3.SetWorkingLookup(fakeWorking{"AAPL|sell": 500})
	if err := c3.Check("AAPL", "buy", 400); err != nil {
		t.Fatalf("buy within cap should pass: %v", err)
	}
}

func TestCheckerMaxOrderQty(t *testing.T) {
	c := NewChecker(Limits{MaxOrderQty: 100}, nil, nil)
	if err := c.CheckMaxOrderQty(101); err == nil {
		t.Fatal("expected max order rejection")
	}
	if err := c.CheckMaxOrderQty(50); err != nil {
		t.Fatalf("within max: %v", err)
	}
}

func TestCheckerSkipDebounce(t *testing.T) {
	base := time.Now()
	c := NewChecker(Limits{Debounce: 300 * time.Millisecond}, fakeLookup{}, func() time.Time { return base })
	if err := c.Record("AAPL", "sell", 100); err != nil {
		t.Fatal(err)
	}
	if err := c.Check("AAPL", "sell", 100); err == nil {
		t.Fatal("expected debounce")
	}
	if err := c.CheckOpts("AAPL", "sell", 100, CheckOpts{SkipDebounce: true}); err != nil {
		t.Fatalf("SkipDebounce should pass: %v", err)
	}
}
