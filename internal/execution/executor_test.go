package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
