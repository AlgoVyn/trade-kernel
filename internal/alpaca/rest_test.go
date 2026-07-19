package alpaca

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	got, err := rest.Trades(t.Context(), "AAPL", time.Now().Add(-time.Hour), time.Now(), 1000)
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

	got, err := rest.Bars(t.Context(), "AAPL", "1Min", time.Now().Add(-time.Hour), time.Now(), 1000)
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

	_, err := rest.Trades(t.Context(), "BAD", time.Now().Add(-time.Hour), time.Now(), 100)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}
