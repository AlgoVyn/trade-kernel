package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestFlattenPricingFailureFallsBack closes via DELETE when the builder
// cannot price an extended-hours exit (no quote).
func TestFlattenPricingFailureFallsBack(t *testing.T) {
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
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.PreMarket }, "day")

	o, err := exec.Flatten(context.Background(), "AAPL", 100)
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if gotDelete != "AAPL" {
		t.Fatalf("expected ClosePosition fallback, got %q", gotDelete)
	}
	if !strings.HasPrefix(o.Status, "close_requested") {
		t.Fatalf("status = %q, want close_requested…", o.Status)
	}
	if !strings.Contains(o.Status, "no quote") {
		t.Fatalf("status should surface original builder error, got %q", o.Status)
	}
}

// TestFlattenPricingAndCloseBothFail joins both errors for the operator.
func TestFlattenPricingAndCloseBothFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, "position close failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rest := alpaca.NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	b := NewBuilder(fakeQuotes{}, nil, 25, 0)
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.PreMarket }, "day")

	_, err := exec.Flatten(context.Background(), "AAPL", 100)
	if err == nil {
		t.Fatal("expected joined error")
	}
	if !strings.Contains(err.Error(), "close position") {
		t.Fatalf("error should include ClosePosition detail, got %v", err)
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
// truncates to a partial exit via the order path.
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
	exec := NewRESTExecutor(rest, b, func() session.Session { return session.Regular }, "day")

	for _, qty := range []float64{100.5, 0.5, -2.25} {
		gotDelete, gotPost = "", 0
		if _, err := exec.Flatten(context.Background(), "AAPL", qty); err != nil {
			t.Fatalf("Flatten(%v): %v", qty, err)
		}
		if gotDelete != "AAPL" {
			t.Fatalf("qty=%v: expected ClosePosition, delete=%q post=%d", qty, gotDelete, gotPost)
		}
		if gotPost != 0 {
			t.Fatalf("qty=%v: should not POST whole-share order, got %d", qty, gotPost)
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
