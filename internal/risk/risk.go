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

// WorkingLookup reports remaining open-order quantity on one side for a
// symbol. Optional — when nil, only filled position is used for the
// projected cap. Same-side working is counted so opposite-side resting
// orders cannot create "credit" that masks risk-increasing size.
// Opposite-side resting is intentionally not a gross cap (see config
// max_position_qty comment).
type WorkingLookup interface {
	// WorkingSideQty returns remaining unfilled open-order qty for symbol
	// on side ("buy" or "sell").
	WorkingSideQty(symbol, side string) float64
}

// ExposureLookup returns filled position and working buy/sell qty for a
// symbol under a single lock. When PositionLookup implements this, CheckOpts
// prefers it over separate PositionQty + WorkingSideQty reads to avoid a
// torn (position, working) snapshot under concurrent fills.
type ExposureLookup interface {
	Exposure(symbol string) (pos, workingBuy, workingSell float64)
}

// Limits configure the pre-trade checks.
type Limits struct {
	MaxOrderQty    int
	MaxPositionQty int
	Debounce       time.Duration
}

// CheckOpts modifies Check behavior for exit / emergency paths.
type CheckOpts struct {
	// SkipLock allows flatten under the kill-switch (new risk still blocked).
	SkipLock bool
	// SkipDebounce allows flatten/panic sizing that matches a recent order.
	SkipDebounce bool
	// SkipMaxOrder allows flatten/panic exits larger than MaxOrderQty
	// (positions can legitimately exceed the new-risk order size cap).
	SkipMaxOrder bool
	// SkipMaxPosition skips projected max-position checks (including same-side
	// working). Flatten must exit the filled book even when stacked resting
	// orders would push |cur+working+exit| further outside the cap.
	SkipMaxPosition bool
}

// Checker performs pre-trade checks: kill-switch lock, order-size cap,
// projected-position cap (position + working orders + new order), and
// duplicate-order debounce.
type Checker struct {
	limits  Limits
	lookup  PositionLookup
	working WorkingLookup

	mu      sync.Mutex
	locked  bool
	reason  string
	lastKey string
	lastAt  time.Time
	now     func() time.Time
}

// NewChecker creates a Checker. now is injectable for tests.
// working may be nil (position-only projection).
func NewChecker(l Limits, lookup PositionLookup, now func() time.Time) *Checker {
	if now == nil {
		now = time.Now
	}
	return &Checker{limits: l, lookup: lookup, now: now}
}

// SetWorkingLookup attaches open-order exposure for projected max position.
func (c *Checker) SetWorkingLookup(w WorkingLookup) {
	c.mu.Lock()
	c.working = w
	c.mu.Unlock()
}

// Lock engages the kill-switch: new risk-increasing orders are rejected
// until Unlock. Flatten and panic still work (they do not call Check).
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
	return c.CheckOpts(symbol, side, qty, CheckOpts{})
}

// CheckMaxOrderQty rejects qty above MaxOrderQty (and non-positive qty).
// Prefer CheckOpts with SkipMaxOrder for gated exit paths; this helper remains
// for callers that only need the size cap without a full projection check.
func (c *Checker) CheckMaxOrderQty(qty int) error {
	c.mu.Lock()
	max := c.limits.MaxOrderQty
	c.mu.Unlock()
	if qty <= 0 {
		return fmt.Errorf("qty must be positive")
	}
	if max > 0 && qty > max {
		return fmt.Errorf("qty %d exceeds max order size %d", qty, max)
	}
	return nil
}

// CheckOpts is Check with options for exit paths (skip lock/debounce/max-order/
// max-position projection). Production flatten/panic do not call this — they
// bypass the checker entirely; the Skip* flags remain for tests and any
// future gated exit that still wants a single choke point.
func (c *Checker) CheckOpts(symbol, side string, qty int, opts CheckOpts) error {
	// Snapshot under the checker lock, then release before store lookups so
	// fill-path store contention cannot hold c.mu and we never nest store
	// locks under the risk lock (latent deadlock if store ever called risk).
	c.mu.Lock()
	locked := c.locked
	reason := c.reason
	limits := c.limits
	lookup := c.lookup
	working := c.working
	lastKey := c.lastKey
	lastAt := c.lastAt
	nowFn := c.now
	c.mu.Unlock()

	if locked && !opts.SkipLock {
		return fmt.Errorf("kill-switch locked (%s): use :unlock to re-enable", reason)
	}
	if qty <= 0 {
		return fmt.Errorf("qty must be positive")
	}
	if !opts.SkipMaxOrder && limits.MaxOrderQty > 0 && qty > limits.MaxOrderQty {
		return fmt.Errorf("qty %d exceeds max order size %d", qty, limits.MaxOrderQty)
	}
	if !opts.SkipMaxPosition && limits.MaxPositionQty > 0 && lookup != nil {
		cur, workingQty := projectedExposure(lookup, working, symbol, side)
		signed := float64(qty)
		if side == "sell" {
			signed = -signed
		}
		// Net exposure including same-side working. Allow orders that strictly
		// reduce |cur+working| without flipping sign even when already outside
		// the cap (partial exits). Reject risk-increasing size, equal-magnitude
		// reverses (long 1100→short 1100), and oversize reverses that remain
		// over the cap (long 1100→short 1050) — those are not pure exits.
		// Same-side resting only: opposite-side open orders must not create
		// credit (e.g. resting sells must not allow buys past the cap if those
		// sells cancel). Stacked unfilled buys still count fully. Opposite-side
		// resting is not a gross notional cap.
		base := cur + workingQty
		proj := base + signed
		max := float64(limits.MaxPositionQty)
		if math.Abs(proj) > max {
			// Strict reduce same-sign or to flat: |proj| < |base| and
			// (proj == 0 || base*proj > 0). Flat is handled explicitly so a
			// zero-base edge cannot mis-classify via product >= 0 alone.
			sameOrFlat := proj == 0 || base*proj > 0
			if math.Abs(proj) >= math.Abs(base) || !sameOrFlat {
				return fmt.Errorf("projected position %.0f exceeds max position size %d", proj, limits.MaxPositionQty)
			}
		}
	}
	if !opts.SkipDebounce {
		key := debounceKey(symbol, side, qty)
		if limits.Debounce > 0 && key == lastKey && nowFn().Sub(lastAt) < limits.Debounce {
			return fmt.Errorf("duplicate order debounced (same order within %s)", limits.Debounce)
		}
	}
	return nil
}

// Record marks an order intent as committed for debounce purposes. Call
// when submission starts (not only after broker ACK) so a second identical
// hotkey during network RTT is blocked. Do not call on a declined
// confirmation. Broker rejects still burn the short debounce window.
// Flatten/panic should not call Record (or should use SkipDebounce on Check).
//
// Record re-checks the debounce window under the lock so two concurrent
// Check calls that both passed a clear window cannot both commit submit;
// the second Record returns an error and the caller must abort submission.
func (c *Checker) Record(symbol, side string, qty int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limits.Debounce <= 0 {
		return nil
	}
	key := debounceKey(symbol, side, qty)
	if key == c.lastKey && c.now().Sub(c.lastAt) < c.limits.Debounce {
		return fmt.Errorf("duplicate order debounced (same order within %s)", c.limits.Debounce)
	}
	c.lastKey = key
	c.lastAt = c.now()
	return nil
}

func debounceKey(symbol, side string, qty int) string {
	return fmt.Sprintf("%s|%s|%d", symbol, side, qty)
}

// projectedExposure returns filled position and signed same-side working qty
// for projection. Prefer ExposureLookup (single lock) when the position
// lookup implements it; otherwise fall back to separate PositionQty and
// WorkingSideQty calls (possible torn snapshot under concurrent fills).
func projectedExposure(lookup PositionLookup, working WorkingLookup, symbol, side string) (cur, workingQty float64) {
	if el, ok := lookup.(ExposureLookup); ok {
		pos, wb, ws := el.Exposure(symbol)
		cur = pos
		if side == "buy" {
			workingQty = wb
		} else {
			workingQty = -ws
		}
		return cur, workingQty
	}
	cur = lookup.PositionQty(symbol)
	if working != nil {
		if side == "buy" {
			workingQty = working.WorkingSideQty(symbol, "buy")
		} else {
			workingQty = -working.WorkingSideQty(symbol, "sell")
		}
	}
	return cur, workingQty
}
