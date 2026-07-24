package execution

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

type fakeQuotes struct {
	bid, ask float64
	qAt      time.Time
	last     float64
	tAt      time.Time
}

func (f fakeQuotes) LatestQuote() (float64, float64, time.Time) { return f.bid, f.ask, f.qAt }
func (f fakeQuotes) LatestTrade() (float64, time.Time)          { return f.last, f.tAt }

type fakeElig map[string]bool

func (f fakeElig) OvernightTradable(_ context.Context, s string) (bool, error) {
	return f[s], nil
}

var testNow = time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)

func newTestBuilder(q fakeQuotes, elig Eligibility) *Builder {
	b := NewBuilder(q, elig, 25, 3*time.Second) // 25 bps, 3s staleness
	b.SetClock(func() time.Time { return testNow })
	return b
}

func TestBuildRegularMarket(t *testing.T) {
	b := newTestBuilder(fakeQuotes{}, nil)
	req, warn, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 100, Session: session.Regular,
	})
	if err != nil || warn != "" {
		t.Fatalf("err=%v warn=%q", err, warn)
	}
	if req.Type != "market" || req.TimeInForce != "day" || req.ExtendedHours {
		t.Fatalf("req = %+v", req)
	}
	if req.ClientOrderID != "" {
		t.Fatal("builder must not set client order id (executor does)")
	}
}

func TestBuildRegularLimitIOC(t *testing.T) {
	b := newTestBuilder(fakeQuotes{}, nil)
	req, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "sell", Qty: 50, LimitPrice: 152.3,
		Session: session.Regular, RegularTIF: "ioc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Type != "limit" || req.LimitPrice != "152.30" || req.TimeInForce != "ioc" || req.ExtendedHours {
		t.Fatalf("req = %+v", req)
	}
}

func TestBuildExtendedAggressiveBuy(t *testing.T) {
	q := fakeQuotes{bid: 100.00, ask: 100.10, qAt: testNow.Add(-time.Second), last: 100.05, tAt: testNow.Add(-time.Second)}
	b := newTestBuilder(q, nil)
	req, warn, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 100, Session: session.PreMarket,
	})
	if err != nil || warn != "" {
		t.Fatalf("err=%v warn=%q", err, warn)
	}
	// ask * 1.0025 = 100.35025 → ceil to cent = 100.36
	if req.Type != "limit" || req.LimitPrice != "100.36" || !req.ExtendedHours || req.TimeInForce != "day" {
		t.Fatalf("req = %+v", req)
	}
}

func TestBuildExtendedAggressiveSell(t *testing.T) {
	q := fakeQuotes{bid: 100.00, ask: 100.10, qAt: testNow.Add(-time.Second)}
	b := newTestBuilder(q, nil)
	req, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "sell", Qty: 100, Session: session.AfterHours,
	})
	if err != nil {
		t.Fatal(err)
	}
	// bid * 0.9975 = 99.75 → floor to cent = 99.75
	if req.LimitPrice != "99.75" {
		t.Fatalf("req = %+v", req)
	}
}

// One-sided books (common in thin extended hours) must still price the
// aggressive side without requiring both bid and ask.
func TestBuildExtendedOneSidedQuote(t *testing.T) {
	// Bid-only: sell Flatten off bid.
	q := fakeQuotes{bid: 100.00, ask: 0, qAt: testNow.Add(-time.Second)}
	b := newTestBuilder(q, nil)
	req, warn, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "sell", Qty: 100, Session: session.AfterHours,
	})
	if err != nil || warn != "" {
		t.Fatalf("bid-only sell: err=%v warn=%q", err, warn)
	}
	if req.LimitPrice != "99.75" {
		t.Fatalf("bid-only sell price = %s, want 99.75", req.LimitPrice)
	}
	// Ask-only: buy off ask.
	q = fakeQuotes{bid: 0, ask: 100.10, qAt: testNow.Add(-time.Second)}
	b = newTestBuilder(q, nil)
	req, warn, err = b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 100, Session: session.PreMarket,
	})
	if err != nil || warn != "" {
		t.Fatalf("ask-only buy: err=%v warn=%q", err, warn)
	}
	if req.LimitPrice != "100.36" {
		t.Fatalf("ask-only buy price = %s, want 100.36", req.LimitPrice)
	}
	// Bid-only cannot price a buy — fall back to last trade when present.
	q = fakeQuotes{bid: 100.00, ask: 0, qAt: testNow.Add(-time.Second), last: 101.00, tAt: testNow.Add(-time.Second)}
	b = newTestBuilder(q, nil)
	req, warn, err = b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 100, Session: session.PreMarket,
	})
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("expected last-trade warning when buy has no ask")
	}
	if req.LimitPrice != "101.26" {
		t.Fatalf("buy without ask = %s, want last-trade 101.26", req.LimitPrice)
	}
}

func TestBuildExtendedStaleQuoteFallsBack(t *testing.T) {
	q := fakeQuotes{
		bid: 100.00, ask: 100.10, qAt: testNow.Add(-time.Hour), // stale
		last: 101.00, tAt: testNow.Add(-time.Second),
	}
	b := newTestBuilder(q, nil)
	req, warn, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 100, Session: session.PreMarket,
	})
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("expected stale-quote warning")
	}
	// last * 1.0025 = 101.2525 → ceil = 101.26
	if req.LimitPrice != "101.26" {
		t.Fatalf("req = %+v", req)
	}
}

// Exit paths may price off a last trade older than quoteStaleFor (quiet tape).
func TestBuildAllowStaleLastTradeForExit(t *testing.T) {
	q := fakeQuotes{
		last: 100.00, tAt: testNow.Add(-30 * time.Second), // older than 3s stale
	}
	b := newTestBuilder(q, nil)
	// Normal hotkey: hard fail.
	_, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "sell", Qty: 100, Session: session.Overnight,
	})
	if err == nil {
		t.Fatal("expected ErrNoExtendedPrice without AllowStaleLastTrade")
	}
	// Flatten/panic: allow stale last trade within exitStaleLastTradeMax.
	req, warn, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "sell", Qty: 100, Session: session.AfterHours,
		AllowStaleLastTrade: true,
	})
	if err != nil {
		t.Fatalf("exit path: %v", err)
	}
	if warn == "" {
		t.Fatal("expected quiet-tape warning")
	}
	if req.LimitPrice != "99.75" {
		t.Fatalf("limit = %s, want 99.75", req.LimitPrice)
	}
}

// Future-dated last trade (clock skew) must not skip pricing; age is clamped to 0.
func TestBuildFutureLastTradeAgeClamped(t *testing.T) {
	q := fakeQuotes{
		last: 100.00, tAt: testNow.Add(30 * time.Second), // future stamp
	}
	b := newTestBuilder(q, nil)
	req, warn, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "sell", Qty: 10, Session: session.AfterHours,
	})
	if err != nil {
		t.Fatalf("future trade should price as fresh: %v", err)
	}
	if warn != "NBBO stale: priced off last trade" {
		t.Fatalf("warn = %q", warn)
	}
	if req.LimitPrice != "99.75" {
		t.Fatalf("limit = %s, want 99.75", req.LimitPrice)
	}
}

func TestBuildOvernightEligibility(t *testing.T) {
	q := fakeQuotes{bid: 100, ask: 100.1, qAt: testNow.Add(-time.Second)}
	elig := fakeElig{"AAPL": true, "XYZ": false}
	b := newTestBuilder(q, elig)

	if _, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 10, Session: session.Overnight,
	}); err != nil {
		t.Fatalf("eligible symbol rejected: %v", err)
	}
	_, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "XYZ", Side: "buy", Qty: 10, Session: session.Overnight,
	})
	if err == nil || !errors.Is(err, ErrNotOvernightTradable) {
		t.Fatalf("ineligible symbol: err = %v", err)
	}
}

func TestBuildClosedRejected(t *testing.T) {
	b := newTestBuilder(fakeQuotes{}, nil)
	_, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 10, Session: session.Closed,
	})
	if err == nil {
		t.Fatal("expected error when market closed")
	}
}

// TestBuildEmptySymbolRejected guards the builder boundary: an empty symbol
// must never produce an order request (UI paths always set it, but the
// builder is the safety boundary). Runs across every session so the guard is
// not accidentally bypassed via a session-specific branch.
func TestBuildEmptySymbolRejected(t *testing.T) {
	b := newTestBuilder(fakeQuotes{}, nil)
	for _, sess := range []session.Session{session.Regular, session.PreMarket, session.AfterHours, session.Overnight, session.Closed} {
		_, _, err := b.Build(context.Background(), BuildInput{
			Symbol: "", Side: "buy", Qty: 10, Session: sess,
		})
		if err == nil {
			t.Fatalf("session %v: expected empty-symbol rejection", sess)
		}
	}
}

func TestBuildExtendedExplicitLimit(t *testing.T) {
	b := newTestBuilder(fakeQuotes{}, nil)
	req, _, err := b.Build(context.Background(), BuildInput{
		Symbol: "AAPL", Side: "buy", Qty: 10, LimitPrice: 99.5, Session: session.PreMarket,
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.LimitPrice != "99.50" || !req.ExtendedHours {
		t.Fatalf("req = %+v", req)
	}
}

// TestEligibilityCacheHit: first OvernightTradable hits assets once; second
// within TTL is served from cache (no second HTTP call).
func TestEligibilityCacheHit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/assets/AAPL" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"symbol":"AAPL","tradable":true,"overnight_tradable":true}`))
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	cache := NewEligibilityCache(rest)

	ok, err := cache.OvernightTradable(context.Background(), "AAPL")
	if err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	ok, err = cache.OvernightTradable(context.Background(), "AAPL")
	if err != nil || !ok {
		t.Fatalf("second: ok=%v err=%v", ok, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("asset hits = %d, want 1 (second call cached)", hits.Load())
	}
}

// TestEligibilityCacheExpiry: after TTL elapses, OvernightTradable refetches.
func TestEligibilityCacheExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/assets/AAPL" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"symbol":"AAPL","tradable":true,"overnight_tradable":true}`))
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	cache := NewEligibilityCache(rest)
	cache.ttl = 20 * time.Millisecond

	if _, err := cache.OvernightTradable(context.Background(), "AAPL"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits after first = %d", hits.Load())
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := cache.OvernightTradable(context.Background(), "AAPL"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("asset hits = %d, want 2 after TTL expiry", hits.Load())
	}
}

// TestEligibilityAttributesArray: eligibility from the attributes array
// (the shape Alpaca actually returns today — no top-level boolean).
func TestEligibilityAttributesArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/assets/SOXL":
			_, _ = w.Write([]byte(`{"symbol":"SOXL","tradable":true,"attributes":["fractional_eh_enabled","has_options","overnight_tradable"]}`))
		case "/v2/assets/XYZ":
			_, _ = w.Write([]byte(`{"symbol":"XYZ","tradable":true,"attributes":["has_options"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	cache := NewEligibilityCache(rest)

	ok, err := cache.OvernightTradable(context.Background(), "SOXL")
	if err != nil || !ok {
		t.Fatalf("SOXL: ok=%v err=%v, want true (overnight_tradable attribute)", ok, err)
	}
	ok, err = cache.OvernightTradable(context.Background(), "XYZ")
	if err != nil || ok {
		t.Fatalf("XYZ: ok=%v err=%v, want false (attribute absent)", ok, err)
	}
}
