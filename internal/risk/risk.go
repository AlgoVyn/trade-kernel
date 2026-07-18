// Package risk enforces pre-trade safety rails and the daily-loss
// kill-switch.
package risk

import (
	"fmt"
	"sync"
	"time"
)

// PositionLookup reports the signed position quantity for a symbol.
type PositionLookup interface {
	PositionQty(symbol string) float64
}

// Limits configure the pre-trade checks.
type Limits struct {
	MaxOrderQty    int
	MaxPositionQty int
	Debounce       time.Duration
}

// Checker performs pre-trade checks: kill-switch lock, order-size cap,
// projected-position cap, and duplicate-order debounce.
type Checker struct {
	limits Limits
	lookup PositionLookup

	mu      sync.Mutex
	locked  bool
	reason  string
	lastKey string
	lastAt  time.Time
	now     func() time.Time
}

// NewChecker creates a Checker. now is injectable for tests.
func NewChecker(l Limits, lookup PositionLookup, now func() time.Time) *Checker {
	if now == nil {
		now = time.Now
	}
	return &Checker{limits: l, lookup: lookup, now: now}
}

// Lock engages the kill-switch: all orders are rejected until Unlock.
func (c *Checker) Lock(reason string) {
	c.mu.Lock()
	c.locked = true
	c.reason = reason
	c.mu.Unlock()
}

// Unlock manually releases the kill-switch.
func (c *Checker) Unlock() {
	c.mu.Lock()
	c.locked = false
	c.reason = ""
	c.mu.Unlock()
}

// Locked reports the kill-switch state and reason.
func (c *Checker) Locked() (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.locked, c.reason
}

// Check validates an order intent. side is "buy" or "sell".
func (c *Checker) Check(symbol, side string, qty int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locked {
		return fmt.Errorf("kill-switch locked (%s): use :unlock to re-enable", c.reason)
	}
	if qty <= 0 {
		return fmt.Errorf("qty must be positive")
	}
	if c.limits.MaxOrderQty > 0 && qty > c.limits.MaxOrderQty {
		return fmt.Errorf("qty %d exceeds max order size %d", qty, c.limits.MaxOrderQty)
	}
	if c.limits.MaxPositionQty > 0 && c.lookup != nil {
		cur := c.lookup.PositionQty(symbol)
		signed := float64(qty)
		if side == "sell" {
			signed = -signed
		}
		proj := cur + signed
		if abs(proj) > float64(c.limits.MaxPositionQty) && abs(proj) > abs(cur) {
			return fmt.Errorf("projected position %.0f exceeds max position size %d", proj, c.limits.MaxPositionQty)
		}
	}
	key := fmt.Sprintf("%s|%s|%d", symbol, side, qty)
	if c.limits.Debounce > 0 && key == c.lastKey && c.now().Sub(c.lastAt) < c.limits.Debounce {
		return fmt.Errorf("duplicate order debounced (same order within %s)", c.limits.Debounce)
	}
	c.lastKey = key
	c.lastAt = c.now()
	return nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// LossMonitor tracks account equity against a daily-loss limit. The
// reference equity is the first reading of each ET calendar day.
type LossMonitor struct {
	limit float64

	mu       sync.Mutex
	day      string // ET date "2006-01-02"
	startEq  float64
	tripped  bool
	onBreach func(loss float64)
	now      func() time.Time
}

// NewLossMonitor creates a monitor; limit<=0 disables it. onBreach is
// called (synchronously, once per trip) when the limit is breached.
func NewLossMonitor(limit float64, onBreach func(loss float64), now func() time.Time) *LossMonitor {
	if now == nil {
		now = time.Now
	}
	return &LossMonitor{limit: limit, onBreach: onBreach, now: now}
}

// Update feeds the latest equity reading. dayLoc must be
// America/New_York so "daily" means the trading day in ET.
func (m *LossMonitor) Update(equity float64, dayLoc *time.Location) {
	if m.limit <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	day := m.now().In(dayLoc).Format("2006-01-02")
	if day != m.day {
		m.day = day
		m.startEq = equity
		m.tripped = false
		return
	}
	if m.tripped {
		return
	}
	if loss := m.startEq - equity; loss >= m.limit {
		m.tripped = true
		if m.onBreach != nil {
			m.onBreach(loss)
		}
	}
}

// Tripped reports whether the monitor has fired today.
func (m *LossMonitor) Tripped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tripped
}
