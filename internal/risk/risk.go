// Package risk enforces pre-trade safety rails (size caps, debounce,
// manual kill-switch lock via Lock/Unlock — no automatic daily-loss trip).
package risk

import (
	"fmt"
	"math"
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
//
// Debounce is evaluated but not recorded here — call Record when the
// order is committed to submission (start of the async submit path) so a
// declined confirmation does not burn the debounce window, while
// in-flight duplicates during broker RTT are still blocked.
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
		if math.Abs(proj) > float64(c.limits.MaxPositionQty) && math.Abs(proj) > math.Abs(cur) {
			return fmt.Errorf("projected position %.0f exceeds max position size %d", proj, c.limits.MaxPositionQty)
		}
	}
	key := debounceKey(symbol, side, qty)
	if c.limits.Debounce > 0 && key == c.lastKey && c.now().Sub(c.lastAt) < c.limits.Debounce {
		return fmt.Errorf("duplicate order debounced (same order within %s)", c.limits.Debounce)
	}
	return nil
}

// Record marks an order intent as committed for debounce purposes. Call
// when submission starts (not only after broker ACK) so a second identical
// hotkey during network RTT is blocked. Do not call on a declined
// confirmation. Broker rejects still burn the short debounce window.
func (c *Checker) Record(symbol, side string, qty int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limits.Debounce <= 0 {
		return
	}
	c.lastKey = debounceKey(symbol, side, qty)
	c.lastAt = c.now()
}

func debounceKey(symbol, side string, qty int) string {
	return fmt.Sprintf("%s|%s|%d", symbol, side, qty)
}
