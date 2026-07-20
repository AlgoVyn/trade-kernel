package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// ErrResponseTruncated is returned by REST methods when a response body
// exceeded the per-call read cap (1 MB). Pagination keeps individual Alpaca
// responses well under this; a truncation indicates an unexpectedly large
// payload whose tail would otherwise be silently dropped and mis-decoded.
var ErrResponseTruncated = errors.New("response body exceeded size limit")

// respBufPool reuses read buffers across REST calls so the 5 s reconcile
// path (account/positions/orders) and paginated backfills don't each grow a
// fresh buffer. Buffers are borrowed for the duration of one do() call —
// read, status-checked, and decoded before being returned — so the decoded
// value never aliases pooled memory.
var respBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// REST is a client for the Alpaca trading and market-data REST APIs.
// Trading and market-data use separate HTTP clients so order/account paths
// can fail fast on header stalls while multi-page bar/trade backfills
// tolerate slower first-byte latency under load.
type REST struct {
	tradingBase string
	dataBase    string
	keyID       string
	secretKey   string
	tradingHC   *http.Client
	dataHC      *http.Client
}

func newHTTPClient(timeout, headerTimeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       5 * time.Minute,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// NewREST builds a REST client. paper selects the paper trading endpoint.
func NewREST(keyID, secretKey string, paper bool) *REST {
	trading := LiveTradingURL
	if paper {
		trading = PaperTradingURL
	}
	return &REST{
		tradingBase: trading,
		dataBase:    DataURL,
		keyID:       keyID,
		secretKey:   secretKey,
		// Trading: tight header timeout so hung order/account calls fail quickly.
		tradingHC: newHTTPClient(10*time.Second, 5*time.Second),
		// Data: longer budgets for paginated bars/trades under load.
		dataHC: newHTTPClient(30*time.Second, 15*time.Second),
	}
}

// Warm primes the trading-host TLS/HTTP connection with a cheap GET so the
// first hotkey order does not pay DNS+TCP+TLS. Returns the /v2/clock payload
// so callers can seed the session engine with the same RTT. Failures are
// returned to the caller (log and continue — order path will surface real errors).
func (c *REST) Warm(ctx context.Context) (Clock, error) {
	return c.Clock(ctx)
}

// SetBaseURL overrides the trading base URL. Intended for httptest-based
// integration tests; not used in production.
func (c *REST) SetBaseURL(u string) { c.tradingBase = u }

// SetDataURL overrides the market-data base URL. Intended for httptest-
// based integration tests; not used in production.
func (c *REST) SetDataURL(u string) { c.dataBase = u }

func (c *REST) do(ctx context.Context, hc *http.Client, method, base, path string, query url.Values, body any, out any) error {
	u := base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("APCA-API-KEY-ID", c.keyID)
	req.Header.Set("APCA-API-SECRET-KEY", c.secretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// 1 MB cap per response — enough for any single Alpaca envelope we
	// consume (positions/orders/bars pages are far smaller; pagination keeps
	// individual calls bounded). Reading up to N+1 bytes lets us detect a
	// truncation rather than silently chopping the tail, which would yield a
	// misleading decode error or, worse, a partial parse.
	//
	// A pooled buffer backs the read, status check, and decode — which all
	// happen synchronously here — so callers never hold a reference into it
	// and no per-response []byte escapes into the heap.
	buf := respBufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		respBufPool.Put(buf)
	}()
	buf.Grow(1<<20 + 1)
	if _, err := io.Copy(buf, io.LimitReader(resp.Body, 1<<20+1)); err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	data := buf.Bytes()
	if len(data) > 1<<20 {
		return fmt.Errorf("read %s %s: %w", method, path, ErrResponseTruncated)
	}
	if resp.StatusCode >= 400 {
		var ae apiError
		if json.Unmarshal(data, &ae) == nil && ae.Message != "" {
			return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, ae.Message)
		}
		return fmt.Errorf("%s %s: %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *REST) trading(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.do(ctx, c.tradingHC, method, c.tradingBase, path, q, body, out)
}

func (c *REST) data(ctx context.Context, method, path string, q url.Values, out any) error {
	return c.do(ctx, c.dataHC, method, c.dataBase, path, q, nil, out)
}

// Account fetches /v2/account.
func (c *REST) Account(ctx context.Context) (Account, error) {
	var a Account
	err := c.trading(ctx, http.MethodGet, "/v2/account", nil, nil, &a)
	return a, err
}

// PortfolioHistory fetches equity/PnL timeseries.
// period examples: "1D", "1W", "1M". timeframe examples: "1Min", "15Min", "1D".
// For equities extended-hours sessions use intraday_reporting=extended_hours.
func (c *REST) PortfolioHistory(ctx context.Context, period, timeframe string) (PortfolioHistory, error) {
	q := url.Values{}
	if period != "" {
		q.Set("period", period)
	}
	if timeframe != "" {
		q.Set("timeframe", timeframe)
	}
	// Include pre/post so overnight/extended marks are reflected in day/week PnL.
	if timeframe != "" && timeframe != "1D" {
		q.Set("intraday_reporting", "extended_hours")
	}
	var h PortfolioHistory
	err := c.trading(ctx, http.MethodGet, "/v2/account/portfolio/history", q, nil, &h)
	return h, err
}

// Positions fetches all open positions.
func (c *REST) Positions(ctx context.Context) ([]Position, error) {
	var p []Position
	err := c.trading(ctx, http.MethodGet, "/v2/positions", nil, nil, &p)
	return p, err
}

// Position fetches the position for symbol. Returns (nil, nil) when flat.
func (c *REST) Position(ctx context.Context, symbol string) (*Position, error) {
	var p Position
	err := c.trading(ctx, http.MethodGet, "/v2/positions/"+url.PathEscape(symbol), nil, nil, &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ClosePosition flattens symbol via DELETE /v2/positions/{symbol}.
func (c *REST) ClosePosition(ctx context.Context, symbol string) error {
	return c.trading(ctx, http.MethodDelete, "/v2/positions/"+url.PathEscape(symbol), nil, nil, nil)
}

// PlaceOrder submits an order.
func (c *REST) PlaceOrder(ctx context.Context, req OrderRequest) (Order, error) {
	var o Order
	err := c.trading(ctx, http.MethodPost, "/v2/orders", nil, req, &o)
	return o, err
}

// OpenOrders fetches orders with status=open, optionally for one symbol.
func (c *REST) OpenOrders(ctx context.Context, symbol string) ([]Order, error) {
	q := url.Values{"status": {"open"}}
	if symbol != "" {
		q.Set("symbols", symbol)
	}
	var o []Order
	err := c.trading(ctx, http.MethodGet, "/v2/orders", q, nil, &o)
	return o, err
}

// CancelAll cancels every open order.
func (c *REST) CancelAll(ctx context.Context) error {
	return c.trading(ctx, http.MethodDelete, "/v2/orders", nil, nil, nil)
}

// CancelOrder cancels a single open order by ID.
func (c *REST) CancelOrder(ctx context.Context, id string) error {
	return c.trading(ctx, http.MethodDelete, "/v2/orders/"+url.PathEscape(id), nil, nil, nil)
}

// CancelSymbol cancels every open order for symbol. It fetches open orders
// filtered by symbol and deletes them concurrently (bounded worker pool) so
// panic/flatten paths with many resting child orders stay under timeout
// without bursting unbounded DELETEs.
//
// Returns the number of per-order DELETEs that failed (failures) and the
// first error encountered (err). Every cancel is still attempted so a single
// failure does not leave siblings open; failures>0 tells the operator (and
// the panic path) that some orders may remain resting.
//
// Empty symbol is rejected so CancelSymbol never becomes account-wide cancel
// (OpenOrders("") returns all open orders).
func (c *REST) CancelSymbol(ctx context.Context, symbol string) (failures int, err error) {
	if symbol == "" {
		return 0, fmt.Errorf("CancelSymbol: empty symbol")
	}
	ords, err := c.OpenOrders(ctx, symbol)
	if err != nil {
		return 0, err
	}
	const cancelWorkers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		first     error
		failedIDs []string // local to this call; do not hoist to package scope
		sem       = make(chan struct{}, cancelWorkers)
	)
	for _, o := range ords {
		// Defense in depth: never cancel an order for a different symbol.
		if o.Symbol != "" && o.Symbol != symbol {
			continue
		}
		id := o.ID
		if id == "" {
			continue
		}
		// Stop scheduling more work if the caller's deadline already fired.
		select {
		case <-ctx.Done():
			wg.Wait()
			mu.Lock()
			n := len(failedIDs)
			f := first
			mu.Unlock()
			if f != nil {
				return n, f
			}
			return n, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.CancelOrder(ctx, id); err != nil {
				mu.Lock()
				failedIDs = append(failedIDs, id)
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	mu.Lock()
	n := len(failedIDs)
	f := first
	mu.Unlock()
	return n, f
}

// Clock fetches /v2/clock.
func (c *REST) Clock(ctx context.Context) (Clock, error) {
	var cl Clock
	err := c.trading(ctx, http.MethodGet, "/v2/clock", nil, nil, &cl)
	return cl, err
}

// Asset fetches /v2/assets/{symbol}.
func (c *REST) Asset(ctx context.Context, symbol string) (Asset, error) {
	var a Asset
	err := c.trading(ctx, http.MethodGet, "/v2/assets/"+url.PathEscape(symbol), nil, nil, &a)
	return a, err
}

// barsResponse is the market-data bars envelope.
type barsResponse struct {
	Bars          []Bar  `json:"bars"`
	NextPageToken string `json:"next_page_token"`
}

// Bars fetches historical bars for symbol at the given timeframe
// ("1Min", "5Min", "15Min", "1Hour", "1Day") between start and end,
// following pagination.
//
// feed selects the data source:
//   - "sip"   — regular + pre-market + after-hours (default when empty)
//   - "boats" — overnight session (20:00–04:00 ET); required for 24/5
//     historical overnight bars (SIP does not include them)
//
// For a full 24/5 series, call Bars twice (sip + boats) and MergeBars.
func (c *REST) Bars(ctx context.Context, symbol, timeframe string, start, end time.Time, limit int, feed string) ([]Bar, error) {
	if feed == "" {
		feed = "sip"
	}
	var out []Bar
	page := ""
	for {
		q := url.Values{
			"timeframe":  {timeframe},
			"start":      {start.UTC().Format(time.RFC3339)},
			"end":        {end.UTC().Format(time.RFC3339)},
			"limit":      {strconv.Itoa(limit)},
			"adjustment": {"raw"},
			"feed":       {feed},
			"sort":       {"asc"},
		}
		if page != "" {
			q.Set("page_token", page)
		}
		var br barsResponse
		if err := c.data(ctx, http.MethodGet, "/v2/stocks/"+url.PathEscape(symbol)+"/bars", q, &br); err != nil {
			return nil, err
		}
		out = append(out, br.Bars...)
		if br.NextPageToken == "" {
			return out, nil
		}
		page = br.NextPageToken
	}
}

// MergeBars merges two ascending bar series (typically SIP + BOATS) by
// timestamp. On an exact timestamp collision the first series (sip) wins,
// since SIP is the authority for regular and extended hours.
//
// The result is always a newly allocated slice (never aliases the inputs),
// so callers may append without mutating the originals.
func MergeBars(sip, boats []Bar) []Bar {
	if len(boats) == 0 {
		out := make([]Bar, len(sip))
		copy(out, sip)
		return out
	}
	if len(sip) == 0 {
		out := make([]Bar, len(boats))
		copy(out, boats)
		return out
	}
	out := make([]Bar, 0, len(sip)+len(boats))
	i, j := 0, 0
	for i < len(sip) || j < len(boats) {
		switch {
		case j >= len(boats):
			out = append(out, sip[i])
			i++
		case i >= len(sip):
			out = append(out, boats[j])
			j++
		case sip[i].Timestamp.Before(boats[j].Timestamp):
			out = append(out, sip[i])
			i++
		case boats[j].Timestamp.Before(sip[i].Timestamp):
			out = append(out, boats[j])
			j++
		default:
			// Same timestamp: prefer SIP.
			out = append(out, sip[i])
			i++
			j++
		}
	}
	return out
}

// tradesResponse is the market-data trades envelope.
type tradesResponse struct {
	Trades        []Trade `json:"trades"`
	NextPageToken string  `json:"next_page_token"`
}

// Trades fetches historical trades for symbol between start and end,
// following pagination. Used to backfill the sub-minute timeframes
// (1s/5s/15s), which the bars endpoint doesn't serve — replaying these
// through the aggregator produces their bars.
//
// feed selects the data source (same as Bars): "sip" (default) or "boats"
// for overnight. For full 24/5 sub-minute coverage, call twice and MergeTrades.
func (c *REST) Trades(ctx context.Context, symbol string, start, end time.Time, limit int, feed string) ([]Trade, error) {
	if feed == "" {
		feed = "sip"
	}
	var out []Trade
	page := ""
	for {
		q := url.Values{
			"start": {start.UTC().Format(time.RFC3339)},
			"end":   {end.UTC().Format(time.RFC3339)},
			"limit": {strconv.Itoa(limit)},
			"feed":  {feed},
			"sort":  {"asc"},
		}
		if page != "" {
			q.Set("page_token", page)
		}
		var tr tradesResponse
		if err := c.data(ctx, http.MethodGet, "/v2/stocks/"+url.PathEscape(symbol)+"/trades", q, &tr); err != nil {
			return nil, err
		}
		out = append(out, tr.Trades...)
		if tr.NextPageToken == "" {
			return out, nil
		}
		page = tr.NextPageToken
	}
}

// LatestQuote fetches the most recent quote for symbol from the given feed
// ("sip" default; "boats" for the overnight session). Used by the overnight
// poller because the SIP websocket does not stream overnight quotes.
func (c *REST) LatestQuote(ctx context.Context, symbol, feed string) (Quote, error) {
	if feed == "" {
		feed = "sip"
	}
	q := url.Values{"feed": {feed}}
	var out struct {
		Quote Quote `json:"quote"`
	}
	err := c.data(ctx, http.MethodGet, "/v2/stocks/"+url.PathEscape(symbol)+"/quotes/latest", q, &out)
	return out.Quote, err
}

// MergeTrades merges two ascending trade series (SIP + BOATS) by timestamp.
// On an exact timestamp collision both prints are kept unless they share a
// non-zero trade id (true duplicate); then SIP wins. Equal price+size alone
// is not treated as a duplicate — distinct prints can share those fields.
// Result is always newly allocated.
func MergeTrades(sip, boats []Trade) []Trade {
	if len(boats) == 0 {
		out := make([]Trade, len(sip))
		copy(out, sip)
		return out
	}
	if len(sip) == 0 {
		out := make([]Trade, len(boats))
		copy(out, boats)
		return out
	}
	out := make([]Trade, 0, len(sip)+len(boats))
	i, j := 0, 0
	for i < len(sip) || j < len(boats) {
		switch {
		case j >= len(boats):
			out = append(out, sip[i])
			i++
		case i >= len(sip):
			out = append(out, boats[j])
			j++
		case sip[i].Timestamp.Before(boats[j].Timestamp):
			out = append(out, sip[i])
			i++
		case boats[j].Timestamp.Before(sip[i].Timestamp):
			out = append(out, boats[j])
			j++
		default:
			// Same timestamp: keep SIP always; keep BOATS only if not a true ID duplicate.
			out = append(out, sip[i])
			if !tradeDuplicate(sip[i], boats[j]) {
				out = append(out, boats[j])
			}
			i++
			j++
		}
	}
	return out
}

// tradeDuplicate reports whether a and b are the same print by trade id.
// Only when both IDs are non-zero; equal price+size alone is not enough
// (would understate volume when SIP/BOATS share a print without IDs).
func tradeDuplicate(a, b Trade) bool {
	return a.ID != 0 && b.ID != 0 && a.ID == b.ID
}
