package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

// TestFlattenClosedFallback verifies that when the market is locally
// Closed, Flatten falls back to DELETE /v2/positions/{symbol} instead of
// attempting to build an order (which the builder would reject with
// "market is closed").
func TestFlattenClosedFallback(t *testing.T) {
	var (
		gotDelete string
		gotPost   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/"):
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orders":
			gotPost++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(alpaca.Order{ID: "ord-1"})
		}
	}))
	defer srv.Close()

	// Point a real REST client at the test server.
	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL) // test seam
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.Closed }, "day")

	o, err := exec.Flatten(context.Background(), "AAPL", 300)
	if err != nil {
		t.Fatalf("Flatten in closed session: %v", err)
	}
	if gotDelete != "AAPL" {
		t.Fatalf("expected ClosePosition(AAPL), got %q (post=%d)", gotDelete, gotPost)
	}
	if gotPost != 0 {
		t.Fatalf("should not POST an order when Closed, got %d POSTs", gotPost)
	}
	if o.Symbol != "AAPL" {
		t.Fatalf("order symbol = %q, want AAPL", o.Symbol)
	}
}

// TestFlattenExtendedPricingFailureNoMarketFallback: when the builder
// cannot price an extended-hours exit, Flatten returns the error and must
// NOT fall back to DELETE /v2/positions — that endpoint liquidates with a
// market order, which is not allowed outside regular hours.
func TestFlattenExtendedPricingFailureNoMarketFallback(t *testing.T) {
	var gotDelete string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/") {
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	// Empty quotes → builder fails for extended hours.
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)
	for _, sess := range []session.Session{session.PreMarket, session.AfterHours, session.Overnight} {
		gotDelete = ""
		exec := NewRESTExecutor(rest, b, func() session.Session { return sess }, "day")
		_, err := exec.Flatten(context.Background(), "AAPL", 100)
		if err == nil {
			t.Fatalf("%s: expected pricing error, got nil", sess)
		}
		if !strings.Contains(err.Error(), "no quote") {
			t.Fatalf("%s: error should be the builder pricing failure, got %v", sess, err)
		}
		if gotDelete != "" {
			t.Fatalf("%s: must not ClosePosition (market order) in extended session, got delete %q", sess, gotDelete)
		}
	}
}

// TestFlattenOvernightLongShortLimitOrder: overnight flattens of both a
// long (sell) and a short (buy) position submit aggressive LIMIT orders
// with extended_hours=true — never market orders.
func TestFlattenOvernightLongShortLimitOrder(t *testing.T) {
	type captured struct {
		Type          string `json:"type"`
		Side          string `json:"side"`
		TimeInForce   string `json:"time_in_force"`
		ExtendedHours bool   `json:"extended_hours"`
		LimitPrice    string `json:"limit_price"`
		Qty           string `json:"qty"`
	}
	var (
		got       captured
		gotDelete string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/"):
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orders":
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(alpaca.Order{ID: "ord-1"})
		}
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	quotes := fakeQuotes{bid: 100.00, ask: 100.10, qAt: time.Now(), last: 100.05, tAt: time.Now()}
	b := NewBuilder(quotes, nil, 25, 0)
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.Overnight }, "day")

	// Long exit → sell limit at bid × (1 − 25bps) = 99.75.
	got = captured{}
	if _, err := exec.Flatten(context.Background(), "AAPL", 100); err != nil {
		t.Fatalf("flatten long: %v", err)
	}
	if got.Type != "limit" || got.Side != "sell" || !got.ExtendedHours || got.LimitPrice != "99.75" || got.Qty != "100" {
		t.Fatalf("long flatten order = %+v, want limit sell 100 @99.75 extended_hours", got)
	}
	// Short exit → buy limit at ask × (1 + 25bps) = 100.36 (ceiled).
	got = captured{}
	if _, err := exec.Flatten(context.Background(), "AAPL", -100); err != nil {
		t.Fatalf("flatten short: %v", err)
	}
	if got.Type != "limit" || got.Side != "buy" || !got.ExtendedHours || got.LimitPrice != "100.36" || got.Qty != "100" {
		t.Fatalf("short flatten order = %+v, want limit buy 100 @100.36 extended_hours", got)
	}
	if gotDelete != "" {
		t.Fatalf("must not ClosePosition for whole-share overnight flatten, got delete %q", gotDelete)
	}
}

// TestFlattenBrokerRejectDoesNotFallback: PlaceOrder HTTP/reject errors must
// not liquidate via ClosePosition (order may already be live).
func TestFlattenBrokerRejectDoesNotFallback(t *testing.T) {
	var (
		gotDelete string
		gotPost   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/"):
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orders":
			gotPost++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"internal server error"}`))
		}
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.Regular }, "day")

	_, err := exec.Flatten(context.Background(), "AAPL", 100)
	if err == nil {
		t.Fatal("expected PlaceOrder error")
	}
	if gotPost != 1 {
		t.Fatalf("expected 1 POST, got %d", gotPost)
	}
	if gotDelete != "" {
		t.Fatalf("must not ClosePosition after broker reject, got delete %q", gotDelete)
	}
}

// TestFlattenFractionalUsesClosePosition ensures non-integral qty never
// truncates to a partial exit via the order path in Regular/Closed.
func TestFlattenFractionalUsesClosePosition(t *testing.T) {
	var (
		gotDelete string
		gotPost   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/"):
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orders":
			gotPost++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(alpaca.Order{ID: "ord-1"})
		}
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)

	for _, sess := range []session.Session{session.Regular, session.Closed} {
		exec := NewRESTExecutor(rest, b, func() session.Session { return sess }, "day")
		for _, qty := range []float64{100.5, 0.5, -2.25} {
			gotDelete, gotPost = "", 0
			if _, err := exec.Flatten(context.Background(), "AAPL", qty); err != nil {
				t.Fatalf("%s Flatten(%v): %v", sess, qty, err)
			}
			if gotDelete != "AAPL" {
				t.Fatalf("%s qty=%v: expected ClosePosition, delete=%q post=%d", sess, qty, gotDelete, gotPost)
			}
			if gotPost != 0 {
				t.Fatalf("%s qty=%v: should not POST whole-share order, got %d", sess, qty, gotPost)
			}
		}
	}
}

// TestFlattenFractionalExtendedRejected: fractionals in extended hours must
// not call ClosePosition (market liquidate) — return a clear error instead.
func TestFlattenFractionalExtendedRejected(t *testing.T) {
	var gotDelete string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/") {
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)

	for _, sess := range []session.Session{session.PreMarket, session.AfterHours, session.Overnight} {
		exec := NewRESTExecutor(rest, b, func() session.Session { return sess }, "day")
		gotDelete = ""
		_, err := exec.Flatten(context.Background(), "AAPL", 100.5)
		if err == nil {
			t.Fatalf("%s: expected error for fractional flatten", sess)
		}
		if !strings.Contains(err.Error(), "fractional") {
			t.Fatalf("%s: error %q should mention fractional", sess, err)
		}
		if gotDelete != "" {
			t.Fatalf("%s: must not ClosePosition for fractional in extended hours, got %q", sess, gotDelete)
		}
	}
}

// TestFlattenOpenSessionUsesOrder: when the session is Regular, Flatten
// builds and submits an order (no liquidation fallback).
func TestFlattenOpenSessionUsesOrder(t *testing.T) {
	var (
		gotDelete string
		gotPost   int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/v2/positions/"):
			gotDelete = r.URL.Path[len("/v2/positions/"):]
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/orders":
			gotPost++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(alpaca.Order{ID: "ord-1"})
		}
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.Regular }, "day")

	o, err := exec.Flatten(context.Background(), "AAPL", 300)
	if err != nil {
		t.Fatalf("Flatten in regular session: %v", err)
	}
	if gotPost != 1 {
		t.Fatalf("expected 1 POST order, got %d", gotPost)
	}
	if gotDelete != "" {
		t.Fatalf("should not ClosePosition in regular session, got %q", gotDelete)
	}
	if o.ID != "ord-1" {
		t.Fatalf("order ID = %q, want ord-1", o.ID)
	}
}
