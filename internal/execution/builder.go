// Package execution turns order intents into Alpaca order requests using
// session-aware rules, and submits them through a REST executor behind
// the Executor interface (a FIX executor can slot in later).
package execution

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

// Sentinel errors from Build. Flatten matches these with errors.Is rather
// than substring matching so reworded messages still fall back safely.
var (
	// ErrMarketClosed is returned when the session is Closed.
	ErrMarketClosed = errors.New("market is closed")
	// ErrNoExtendedPrice is returned when extended-hours pricing has no
	// usable NBBO or last trade.
	ErrNoExtendedPrice = errors.New("no quote or last trade available to price extended-hours order")
	// ErrNotOvernightTradable is returned when the symbol fails the
	// overnight-tradability check (wrapped with the symbol name).
	ErrNotOvernightTradable = errors.New("not overnight-tradable on Alpaca")
	// ErrOvernightEligibility is returned when the eligibility lookup itself fails.
	ErrOvernightEligibility = errors.New("overnight eligibility check failed")
)

// QuoteSource provides the latest NBBO and last trade for the active
// symbol (implemented by bars.Aggregator).
type QuoteSource interface {
	LatestQuote() (bid, ask float64, at time.Time)
	LatestTrade() (price float64, at time.Time)
}

// Eligibility reports whether a symbol may trade in the overnight
// session.
type Eligibility interface {
	OvernightTradable(ctx context.Context, symbol string) (bool, error)
}

// BuildInput describes a desired order before session rules are applied.
type BuildInput struct {
	Symbol     string
	Side       string // "buy" | "sell"
	Qty        int
	Session    session.Session
	LimitPrice float64 // >0: explicit limit (from ':' command); 0: hotkey default
	RegularTIF string  // "day" | "ioc" for regular-hours orders
}

// Builder applies session-aware order-form rules.
type Builder struct {
	quotes        QuoteSource
	elig          Eligibility
	slippageBps   float64
	quoteStaleFor time.Duration
	now           func() time.Time
}

// NewBuilder creates a Builder.
func NewBuilder(qs QuoteSource, elig Eligibility, slippageBps float64, quoteStaleFor time.Duration) *Builder {
	if slippageBps <= 0 {
		slippageBps = 25 // 0.25% default
	}
	if quoteStaleFor <= 0 {
		quoteStaleFor = 3 * time.Second
	}
	return &Builder{
		quotes:        qs,
		elig:          elig,
		slippageBps:   slippageBps,
		quoteStaleFor: quoteStaleFor,
		now:           time.Now,
	}
}

// SetClock overrides the clock (tests).
func (b *Builder) SetClock(now func() time.Time) { b.now = now }

// Build converts in into an Alpaca OrderRequest. A non-empty warning
// means the order was built with a fallback (stale quote) and the UI
// should surface it.
func (b *Builder) Build(ctx context.Context, in BuildInput) (alpaca.OrderRequest, string, error) {
	if in.Qty <= 0 {
		return alpaca.OrderRequest{}, "", fmt.Errorf("qty must be positive")
	}
	if in.Side != "buy" && in.Side != "sell" {
		return alpaca.OrderRequest{}, "", fmt.Errorf("side must be buy or sell")
	}
	req := alpaca.OrderRequest{
		Symbol: in.Symbol,
		Qty:    fmt.Sprintf("%d", in.Qty),
		Side:   in.Side,
	}
	switch in.Session {
	case session.Regular:
		if in.LimitPrice > 0 {
			req.Type = "limit"
			req.LimitPrice = fmt.Sprintf("%.2f", in.LimitPrice)
		} else {
			req.Type = "market"
		}
		if in.RegularTIF == "ioc" {
			req.TimeInForce = "ioc"
		} else {
			req.TimeInForce = "day"
		}
		return req, "", nil

	case session.PreMarket, session.AfterHours, session.Overnight:
		if in.Session == session.Overnight && b.elig != nil {
			ok, err := b.elig.OvernightTradable(ctx, in.Symbol)
			if err != nil {
				return alpaca.OrderRequest{}, "", fmt.Errorf("%w: %v", ErrOvernightEligibility, err)
			}
			if !ok {
				return alpaca.OrderRequest{}, "", fmt.Errorf("%s is %w", in.Symbol, ErrNotOvernightTradable)
			}
		}
		req.Type = "limit"
		req.TimeInForce = "day"
		req.ExtendedHours = true
		if in.LimitPrice > 0 {
			req.LimitPrice = fmt.Sprintf("%.2f", in.LimitPrice)
			return req, "", nil
		}
		price, warn := b.aggressivePrice(in.Side)
		if price <= 0 {
			return alpaca.OrderRequest{}, "", ErrNoExtendedPrice
		}
		req.LimitPrice = fmt.Sprintf("%.2f", price)
		return req, warn, nil

	default:
		return alpaca.OrderRequest{}, "", ErrMarketClosed
	}
}

// aggressivePrice computes the far-side price with slippage for the
// given side, falling back to the last trade when the quote is stale.
func (b *Builder) aggressivePrice(side string) (float64, string) {
	slip := b.slippageBps / 10000.0
	now := b.now()
	if b.quotes != nil {
		bid, ask, qAt := b.quotes.LatestQuote()
		if bid > 0 && ask > 0 && now.Sub(qAt) <= b.quoteStaleFor {
			if side == "buy" {
				return math.Ceil(ask*(1+slip)*100) / 100, ""
			}
			return math.Floor(bid*(1-slip)*100) / 100, ""
		}
		price, tAt := b.quotes.LatestTrade()
		if price > 0 && now.Sub(tAt) <= b.quoteStaleFor {
			if side == "buy" {
				return math.Ceil(price*(1+slip)*100) / 100, "NBBO stale: priced off last trade"
			}
			return math.Floor(price*(1-slip)*100) / 100, "NBBO stale: priced off last trade"
		}
	}
	return 0, ""
}

// PreviewLimit returns the aggressive limit price that Build would use
// for a hotkey order right now (for the status-bar confirmation line).
func (b *Builder) PreviewLimit(side string) (float64, string) {
	return b.aggressivePrice(side)
}

// EligibilityCache caches per-symbol overnight-tradability from the
// assets endpoint with a TTL.
type EligibilityCache struct {
	mu   sync.Mutex
	rest *alpaca.REST
	ttl  time.Duration
	ent  map[string]eligEntry
}

type eligEntry struct {
	ok bool
	at time.Time
}

// NewEligibilityCache creates a cache backed by rest.
func NewEligibilityCache(rest *alpaca.REST) *EligibilityCache {
	return &EligibilityCache{rest: rest, ttl: time.Hour, ent: make(map[string]eligEntry)}
}

// OvernightTradable reports the cached/fetched overnight eligibility.
func (c *EligibilityCache) OvernightTradable(ctx context.Context, symbol string) (bool, error) {
	c.mu.Lock()
	if e, ok := c.ent[symbol]; ok && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.ok, nil
	}
	c.mu.Unlock()
	a, err := c.rest.Asset(ctx, symbol)
	if err != nil {
		return false, err
	}
	ok := a.Tradable && a.OvernightTradable
	c.mu.Lock()
	c.ent[symbol] = eligEntry{ok: ok, at: time.Now()}
	c.mu.Unlock()
	return ok, nil
}
