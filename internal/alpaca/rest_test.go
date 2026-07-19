package alpaca

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// runPaginated spins up a test server that returns `pages` of JSON
// envelopes sequentially (each with the given next-page token), so we can
// exercise the pagination loop without real network calls.
func runPaginated(t *testing.T, pages [][]byte) *httptest.Server {
	t.Helper()
	idx := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if idx >= len(pages) {
			t.Fatalf("server got %d-th request, only %d pages queued", idx+1, len(pages))
		}
		w.Write(pages[idx])
		idx++
	}))
}

func TestTradesPagination(t *testing.T) {
	// Two pages: first carries a next-page token, second terminates.
	tr1 := []byte(`{"trades":[{"S":"AAPL","p":150.25,"s":10,"t":"2026-07-15T14:00:00Z"},{"S":"AAPL","p":150.30,"s":5,"t":"2026-07-15T14:00:01Z"}],"next_page_token":"tok2"}`)
	tr2 := []byte(`{"trades":[{"S":"AAPL","p":150.40,"s":8,"t":"2026-07-15T14:00:02Z"}],"next_page_token":""}`)
	srv := runPaginated(t, [][]byte{tr1, tr2})
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetDataURL(srv.URL) // test seam

	got, err := rest.Trades(t.Context(), "AAPL", time.Now().Add(-time.Hour), time.Now(), 1000, "sip")
	if err != nil {
		t.Fatalf("Trades: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 trades across pages, got %d", len(got))
	}
	// Spot-check price and that the JSON number form decoded into flexFloat.
	if float64(got[0].Price) != 150.25 {
		t.Fatalf("trade[0] price = %v, want 150.25", got[0].Price)
	}
	if float64(got[2].Size) != 8 {
		t.Fatalf("trade[2] size = %v, want 8", got[2].Size)
	}
}

func TestBarsPagination(t *testing.T) {
	// Mirrors TestTradesPagination for the existing Bars method, locking in
	// the shared pagination contract.
	b1 := []byte(`{"bars":[{"t":"2026-07-15T14:00:00Z","o":150,"h":151,"l":149,"c":150.5,"v":1000}],"next_page_token":"x"}`)
	b2 := []byte(`{"bars":[{"t":"2026-07-15T14:01:00Z","o":150.5,"h":152,"l":150,"c":151.5,"v":2000}],"next_page_token":""}`)
	srv := runPaginated(t, [][]byte{b1, b2})
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetDataURL(srv.URL)

	got, err := rest.Bars(t.Context(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now(), 1000, "sip")
	if err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 bars, got %d", len(got))
	}
	if float64(got[1].Close) != 151.5 {
		t.Fatalf("bar[1] close = %v, want 151.5", got[1].Close)
	}
}

// TestBarsFeedQuery locks in that the feed query param is forwarded so
// overnight (boats) requests actually hit the BOATS feed.
func TestBarsFeedQuery(t *testing.T) {
	var gotFeed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFeed = r.URL.Query().Get("feed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"bars":[],"next_page_token":""}`))
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetDataURL(srv.URL)
	if _, err := rest.Bars(t.Context(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now(), 100, "boats"); err != nil {
		t.Fatalf("Bars: %v", err)
	}
	if gotFeed != "boats" {
		t.Fatalf("feed query = %q, want boats", gotFeed)
	}
}

// TestMergeBars interleaves SIP and BOATS series and prefers SIP on ties.
func TestMergeBars(t *testing.T) {
	t0 := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)  // overnight
	t1 := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC) // regular
	t2 := time.Date(2026, 7, 15, 23, 0, 0, 0, time.UTC) // overnight
	sip := []Bar{
		{Timestamp: t1, Close: 100},
	}
	boats := []Bar{
		{Timestamp: t0, Close: 90},
		{Timestamp: t1, Close: 99}, // collision — SIP should win
		{Timestamp: t2, Close: 95},
	}
	merged := MergeBars(sip, boats)
	if len(merged) != 3 {
		t.Fatalf("want 3 bars, got %d", len(merged))
	}
	if float64(merged[0].Close) != 90 || !merged[0].Timestamp.Equal(t0) {
		t.Fatalf("first (overnight) = %+v", merged[0])
	}
	if float64(merged[1].Close) != 100 { // SIP wins collision
		t.Fatalf("collision close = %v, want SIP 100", merged[1].Close)
	}
	if float64(merged[2].Close) != 95 || !merged[2].Timestamp.Equal(t2) {
		t.Fatalf("last (overnight) = %+v", merged[2])
	}
	// Empty sides.
	if got := MergeBars(sip, nil); len(got) != 1 {
		t.Fatalf("sip-only: %d", len(got))
	}
	if got := MergeBars(nil, boats); len(got) != 3 {
		t.Fatalf("boats-only: %d", len(got))
	}
}

// TestTradesErrorResponse ensures a non-2xx surfaces as an error (the
// apiError envelope is decoded for a friendlier message).
func TestTradesErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(apiError{Code: 400, Message: "bad symbol"})
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetDataURL(srv.URL)

	_, err := rest.Trades(t.Context(), "BAD", time.Now().Add(-time.Hour), time.Now(), 100, "sip")
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

// TestTradesFeedQuery locks in that the feed query param is forwarded.
func TestTradesFeedQuery(t *testing.T) {
	var gotFeed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFeed = r.URL.Query().Get("feed")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trades":[],"next_page_token":""}`))
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetDataURL(srv.URL)
	if _, err := rest.Trades(t.Context(), "AAPL", time.Now().Add(-time.Hour), time.Now(), 100, "boats"); err != nil {
		t.Fatalf("Trades: %v", err)
	}
	if gotFeed != "boats" {
		t.Fatalf("feed query = %q, want boats", gotFeed)
	}
}

func TestMergeTrades(t *testing.T) {
	t0 := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	sip := []Trade{{Timestamp: t1, Price: 100, Size: 1, ID: 1}}
	boats := []Trade{
		{Timestamp: t0, Price: 90, Size: 2, ID: 2},
		// Distinct print at same timestamp as SIP — keep both.
		{Timestamp: t1, Price: 99, Size: 3, ID: 3},
	}
	merged := MergeTrades(sip, boats)
	if len(merged) != 3 {
		t.Fatalf("want 3 trades, got %d", len(merged))
	}
	if float64(merged[0].Price) != 90 {
		t.Fatalf("first = %v, want boats 90", merged[0].Price)
	}
	if float64(merged[1].Price) != 100 {
		t.Fatalf("sip at t1 = %v, want 100", merged[1].Price)
	}
	if float64(merged[2].Price) != 99 {
		t.Fatalf("boats at t1 = %v, want 99", merged[2].Price)
	}

	// Equal price+size without IDs is NOT treated as a duplicate — keep both.
	samePx := MergeTrades(
		[]Trade{{Timestamp: t1, Price: 100, Size: 5}},
		[]Trade{{Timestamp: t1, Price: 100, Size: 5}},
	)
	if len(samePx) != 2 {
		t.Fatalf("equal price+size without IDs should keep both, got %+v", samePx)
	}

	// True duplicate (same non-zero trade id) → SIP only.
	dup := MergeTrades(
		[]Trade{{Timestamp: t1, Price: 100, Size: 5, ID: 42}},
		[]Trade{{Timestamp: t1, Price: 100, Size: 5, ID: 42}},
	)
	if len(dup) != 1 || float64(dup[0].Price) != 100 || dup[0].ID != 42 {
		t.Fatalf("id-duplicate collision = %+v", dup)
	}
}

// TestMergeBarsDoesNotAlias ensures empty-side results are copies.
func TestMergeBarsDoesNotAlias(t *testing.T) {
	sip := []Bar{{Timestamp: time.Unix(1, 0), Close: 1}}
	out := MergeBars(sip, nil)
	out[0].Close = 99
	if float64(sip[0].Close) != 1 {
		t.Fatal("MergeBars aliased input slice")
	}
}

func TestCancelSymbolEmptyRejected(t *testing.T) {
	// Must not call the API at all when symbol is empty (would cancel all).
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	err := rest.CancelSymbol(t.Context(), "")
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
	if called {
		t.Fatal("CancelSymbol(\"\") must not hit the API")
	}
}

// TestCancelSymbolDeletesAllIDs fans out DELETEs for every open order id
// (bounded concurrency) and short-circuits empty symbol separately.
func TestCancelSymbolDeletesAllIDs(t *testing.T) {
	var (
		mu      sync.Mutex
		deleted []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/orders":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":"o1","symbol":"AAPL","status":"new"},
				{"id":"o2","symbol":"AAPL","status":"new"},
				{"id":"o3","symbol":"AAPL","status":"new"}
			]`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v2/orders/"):
			id := strings.TrimPrefix(r.URL.Path, "/v2/orders/")
			mu.Lock()
			deleted = append(deleted, id)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	if err := rest.CancelSymbol(t.Context(), "AAPL"); err != nil {
		t.Fatalf("CancelSymbol: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 3 {
		t.Fatalf("deleted = %v, want 3 ids", deleted)
	}
	seen := map[string]bool{}
	for _, id := range deleted {
		seen[id] = true
	}
	for _, id := range []string{"o1", "o2", "o3"} {
		if !seen[id] {
			t.Fatalf("missing delete for %s in %v", id, deleted)
		}
	}
}
