package alpaca

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func TestPortfolioHistory(t *testing.T) {
	body := []byte(`{"timestamp":[1,2],"equity":["100000","101500"],"profit_loss":["0","1500"],"base_value":"100000","timeframe":"1D"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/account/portfolio/history" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("period") != "1W" || r.URL.Query().Get("timeframe") != "1D" {
			t.Fatalf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	h, err := rest.PortfolioHistory(t.Context(), "1W", "1D")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := h.LatestPnL()
	if !ok || v != 1500 {
		t.Fatalf("LatestPnL = %v %v", v, ok)
	}
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
	failures, err := rest.CancelSymbol(t.Context(), "")
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0 for empty symbol", failures)
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
	failures, err := rest.CancelSymbol(t.Context(), "AAPL")
	if err != nil {
		t.Fatalf("CancelSymbol: %v", err)
	}
	if failures != 0 {
		t.Fatalf("failures = %d, want 0 when all DELETEs succeed", failures)
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

func TestWarmHitsClock(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/clock" {
			http.NotFound(w, r)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_open":true,"next_open":"2026-07-20T13:30:00Z","next_close":"2026-07-20T20:00:00Z","timestamp":"2026-07-19T15:00:00Z"}`))
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	cl, err := rest.Warm(t.Context())
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if !cl.IsOpen {
		t.Fatalf("Warm clock IsOpen = false, want true")
	}
	if hits != 1 {
		t.Fatalf("clock hits = %d, want 1", hits)
	}
}

// TestCancelSymbolReturnsFailureCount: when one of several DELETEs fails,
// CancelSymbol returns failures>0 and the first error, while still
// attempting the rest. The panic path uses the count to tell the operator
// how many orders may still be resting.
func TestCancelSymbolReturnsFailureCount(t *testing.T) {
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
			// Fail exactly one cancel (o2) so we can assert failures==1 and
			// that the other two still went through.
			if id == "o2" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
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
	failures, err := rest.CancelSymbol(t.Context(), "AAPL")
	if err == nil {
		t.Fatal("expected first error from failed DELETE")
	}
	if failures != 1 {
		t.Fatalf("failures = %d, want 1", failures)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 successful cancels despite one failure", deleted)
	}
}

// GET 429 retries honor a bounded Retry-After and succeed after the wait.
func TestGET429HonorsRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"rate limit"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"acct","equity":"1","cash":"1","buying_power":"1","status":"ACTIVE"}`))
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	start := time.Now()
	if _, err := rest.Account(t.Context()); err != nil {
		t.Fatalf("Account after 429: %v", err)
	}
	// Fixed backoff is 250ms; Retry-After=1s is preferred and capped at 2s.
	// Require we waited at least ~750ms so the header path was used (not 250ms alone).
	if elapsed := time.Since(start); elapsed < 750*time.Millisecond {
		t.Fatalf("elapsed %v, want ≥750ms (Retry-After path)", elapsed)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
}

// Final-attempt 429 must surface the error (no silent exhausted-retries path).
func TestGET429ExhaustedReturnsError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"slow down"}`))
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	_, err := rest.Account(t.Context())
	if err == nil {
		t.Fatal("expected 429 error after retries")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want 429", err)
	}
	// 1 initial + doRateLimitRetries
	if want := int32(1 + doRateLimitRetries); hits.Load() != want {
		t.Fatalf("hits = %d, want %d", hits.Load(), want)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("2"); d != 2*time.Second {
		t.Fatalf("seconds: got %v", d)
	}
	if d := parseRetryAfter(""); d != 0 {
		t.Fatalf("empty: got %v", d)
	}
	if d := parseRetryAfter("not-a-number"); d != 0 {
		t.Fatalf("http-date/invalid: got %v", d)
	}
}

// TestResponseTruncatedDetected: a body larger than the 1 MB cap must return
// ErrResponseTruncated rather than being silently chopped (which would
// produce a misleading decode error or a partial parse).
func TestResponseTruncatedDetected(t *testing.T) {
	// 2 MB of body — well past the 1 MB read cap in REST.do.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A single huge array element; size matters, not validity.
		_, _ = w.Write([]byte("[\""))
		chunk := make([]byte, 1<<20) // 1 MB
		for i := range chunk {
			chunk[i] = 'a'
		}
		// Write twice to exceed the cap; the read boundary is 1<<20 bytes.
		_, _ = w.Write(chunk)
		_, _ = w.Write(chunk)
		_, _ = w.Write([]byte("\"]"))
	}))
	defer srv.Close()

	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	_, err := rest.Account(t.Context())
	if !errors.Is(err, ErrResponseTruncated) {
		t.Fatalf("err = %v, want ErrResponseTruncated", err)
	}
}

func TestFillsStuckPageTokenErrors(t *testing.T) {
	// Full page with empty last activity id must not be treated as complete.
	page := make([]map[string]any, fillPageSize)
	for i := range page {
		page[i] = map[string]any{
			"id":               "",
			"symbol":           "AAPL",
			"side":             "buy",
			"qty":              "1",
			"price":            "10",
			"transaction_time": "2026-07-15T14:00:00Z",
		}
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v2/account/activities/FILL") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.Fills(t.Context(), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected partial-history error on stuck page_token")
	}
	if got != nil {
		t.Fatalf("on error must return nil fills, got len=%d", len(got))
	}
	if !strings.Contains(err.Error(), "page_token did not advance") {
		t.Fatalf("err = %v", err)
	}
}

func TestClosedOrdersIdenticalSubmittedAtFullPageErrors(t *testing.T) {
	// Full page of equal submitted_at cannot be continued with exclusive after.
	ts := "2026-07-15T14:00:00.000000000Z"
	page := make([]map[string]any, closedOrdersPage)
	for i := range page {
		page[i] = map[string]any{
			"id":           "ord-" + strconv.Itoa(i),
			"symbol":       "AAPL",
			"side":         "buy",
			"qty":          "1",
			"filled_qty":   "1",
			"status":       "filled",
			"submitted_at": ts,
			"filled_at":    ts,
		}
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v2/orders") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.ClosedOrders(t.Context(), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected partial-history error on identical submitted_at full page")
	}
	if got != nil {
		t.Fatalf("on error must return nil orders, got len=%d", len(got))
	}
	if !strings.Contains(err.Error(), "identical submitted_at") {
		t.Fatalf("err = %v", err)
	}
	if n != 1 {
		t.Fatalf("requests = %d, want 1 (fail before next page)", n)
	}
}

func TestClosedOrdersTrailingSameSubmittedAtStuckErrors(t *testing.T) {
	// Overlap cursor (last−1ns) re-fetches the same full page when the
	// server keeps returning it; zero new ids must fail closed, not loop.
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	page := make([]map[string]any, closedOrdersPage)
	lastTS := base.Add(time.Duration(closedOrdersPage-2) * time.Second)
	for i := range page {
		ts := base.Add(time.Duration(i) * time.Second)
		if i >= closedOrdersPage-2 {
			ts = lastTS
		}
		page[i] = map[string]any{
			"id":           "ord-" + strconv.Itoa(i),
			"symbol":       "AAPL",
			"side":         "buy",
			"qty":          "1",
			"filled_qty":   "1",
			"status":       "filled",
			"submitted_at": ts.Format(time.RFC3339Nano),
			"filled_at":    ts.Format(time.RFC3339Nano),
		}
	}
	body, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.ClosedOrders(t.Context(), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected partial-history error when pagination yields only duplicates")
	}
	if got != nil {
		t.Fatalf("on error must return nil orders, got len=%d", len(got))
	}
	if !strings.Contains(err.Error(), "no new order ids") {
		t.Fatalf("err = %v", err)
	}
	if n != 2 {
		t.Fatalf("requests = %d, want 2 (first page + stuck overlap page)", n)
	}
}

// Exclusive after=T used to drop the second order at T when a full page
// ended with trailingSame==1. Overlap cursor must include the sibling.
func TestClosedOrdersSingleTrailingSameTimestampIncludesSibling(t *testing.T) {
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	// Page 1: 499 unique times + first of two orders at lastTS.
	lastTS := base.Add(time.Duration(closedOrdersPage-1) * time.Second)
	page1 := make([]map[string]any, closedOrdersPage)
	for i := range page1 {
		ts := base.Add(time.Duration(i) * time.Second)
		if i == closedOrdersPage-1 {
			ts = lastTS
		}
		page1[i] = map[string]any{
			"id":           "ord-" + strconv.Itoa(i),
			"symbol":       "AAPL",
			"side":         "buy",
			"qty":          "1",
			"filled_qty":   "1",
			"status":       "filled",
			"submitted_at": ts.Format(time.RFC3339Nano),
			"filled_at":    ts.Format(time.RFC3339Nano),
		}
	}
	// Page 2: sibling at same lastTS, then one later order (short page).
	page2 := []map[string]any{
		{
			"id":           "ord-sibling",
			"symbol":       "AAPL",
			"side":         "sell",
			"qty":          "1",
			"filled_qty":   "1",
			"status":       "filled",
			"submitted_at": lastTS.Format(time.RFC3339Nano),
			"filled_at":    lastTS.Format(time.RFC3339Nano),
		},
		{
			"id":           "ord-later",
			"symbol":       "AAPL",
			"side":         "buy",
			"qty":          "1",
			"filled_qty":   "1",
			"status":       "filled",
			"submitted_at": lastTS.Add(time.Second).Format(time.RFC3339Nano),
			"filled_at":    lastTS.Add(time.Second).Format(time.RFC3339Nano),
		},
	}
	body1, err := json.Marshal(page1)
	if err != nil {
		t.Fatal(err)
	}
	body2, err := json.Marshal(page2)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write(body1)
			return
		}
		// Overlap page may re-include last of page1; return sibling + later.
		// Optionally prepend last of page1 to simulate real after=last-1ns.
		overlap := append([]map[string]any{page1[closedOrdersPage-1]}, page2...)
		b, _ := json.Marshal(overlap)
		_, _ = w.Write(b)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.ClosedOrders(t.Context(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ClosedOrders: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, o := range got {
		ids[o.ID] = true
	}
	if !ids["ord-sibling"] {
		t.Fatal("sibling at same submitted_at must not be dropped across pages")
	}
	if !ids["ord-later"] {
		t.Fatal("later order missing")
	}
	if len(got) != closedOrdersPage+2 {
		t.Fatalf("len = %d, want %d (page1 + sibling + later, de-duped)", len(got), closedOrdersPage+2)
	}
	if n != 2 {
		t.Fatalf("requests = %d, want 2", n)
	}
	_ = body2 // page2 embedded via overlap construction
}

func TestFillsDedupesByActivityID(t *testing.T) {
	// Overlapping page content must not double-count the same activity id.
	page1 := make([]map[string]any, fillPageSize)
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	for i := range page1 {
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		page1[i] = map[string]any{
			"id":               "fill-" + strconv.Itoa(i),
			"symbol":           "AAPL",
			"side":             "buy",
			"qty":              "1",
			"price":            "10",
			"transaction_time": ts,
		}
	}
	// Second page: last id of page1 repeated, then one new fill.
	page2 := []map[string]any{
		page1[fillPageSize-1],
		{
			"id":               "fill-new",
			"symbol":           "AAPL",
			"side":             "sell",
			"qty":              "1",
			"price":            "11",
			"transaction_time": base.Add(time.Duration(fillPageSize) * time.Second).Format(time.RFC3339Nano),
		},
	}
	b1, err := json.Marshal(page1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(page2)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write(b1)
			return
		}
		_, _ = w.Write(b2)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.Fills(t.Context(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// fillPageSize unique from page1 + 1 new (duplicate of last dropped).
	if len(got) != fillPageSize+1 {
		t.Fatalf("len(fills) = %d, want %d", len(got), fillPageSize+1)
	}
	if got[len(got)-1].ID != "fill-new" {
		t.Fatalf("last id = %q, want fill-new", got[len(got)-1].ID)
	}
	seen := make(map[string]int)
	for _, f := range got {
		seen[f.ID]++
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("id %s counted %d times", id, c)
		}
	}
}

func TestClosedOrdersHTTPErrorReturnsPartial(t *testing.T) {
	// Mid-pagination HTTP error returns rows already fetched with err so
	// callers can checkpoint; err != nil still means incomplete history.
	okPage := make([]map[string]any, closedOrdersPage)
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	for i := range okPage {
		ts := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		okPage[i] = map[string]any{
			"id":           "ord-" + strconv.Itoa(i),
			"symbol":       "AAPL",
			"side":         "buy",
			"qty":          "1",
			"filled_qty":   "1",
			"status":       "filled",
			"submitted_at": ts,
			"filled_at":    ts,
		}
	}
	body, err := json.Marshal(okPage)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.ClosedOrders(t.Context(), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected HTTP error on second page")
	}
	if len(got) != closedOrdersPage {
		t.Fatalf("partial checkpoint: got len=%d, want %d", len(got), closedOrdersPage)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err should mark incomplete: %v", err)
	}
}

func TestFillsHTTPErrorReturnsPartial(t *testing.T) {
	okPage := make([]map[string]any, fillPageSize)
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	for i := range okPage {
		okPage[i] = map[string]any{
			"id":               "fill-" + strconv.Itoa(i),
			"symbol":           "AAPL",
			"side":             "buy",
			"qty":              "1",
			"price":            "10",
			"transaction_time": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
		}
	}
	body, err := json.Marshal(okPage)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	got, err := rest.Fills(t.Context(), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected HTTP error on second page")
	}
	if len(got) != fillPageSize {
		t.Fatalf("partial checkpoint: got len=%d, want %d", len(got), fillPageSize)
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err should mark incomplete: %v", err)
	}
}

// Position returns (nil, nil) on HTTP 404 (flat) so flatten REST fallback
// can treat empty books as zero size without surfacing a hard error.
func TestPositionNotFoundIsFlat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2/positions/") {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"position does not exist"}`))
	}))
	defer srv.Close()
	rest := NewREST("k", "s", true)
	rest.SetBaseURL(srv.URL)
	p, err := rest.Position(t.Context(), "AAPL")
	if err != nil {
		t.Fatalf("404 should be flat: %v", err)
	}
	if p != nil {
		t.Fatalf("want nil position, got %+v", p)
	}
}
