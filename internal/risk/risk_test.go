package risk

import (
	"testing"
	"time"
)

type fakeLookup map[string]float64

func (f fakeLookup) PositionQty(s string) float64 { return f[s] }

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
	c.Record("AAPL", "buy", 100)
	if err := c.Check("AAPL", "buy", 100); err == nil {
		t.Fatal("expected debounce rejection after Record")
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
	c.Unlock()
	if err := c.Check("AAPL", "buy", 1); err != nil {
		t.Fatalf("unlocked: %v", err)
	}
}
