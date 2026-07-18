package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// REST is a client for the Alpaca trading and market-data REST APIs.
// The zero values of http.Client settings are tuned for low latency:
// keep-alive connections are reused across calls.
type REST struct {
	tradingBase string
	dataBase    string
	keyID       string
	secretKey   string
	hc          *http.Client
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
		hc: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (c *REST) do(ctx context.Context, method, base, path string, query url.Values, body any, out any) error {
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
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
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
	return c.do(ctx, method, c.tradingBase, path, q, body, out)
}

func (c *REST) data(ctx context.Context, method, path string, q url.Values, out any) error {
	return c.do(ctx, method, c.dataBase, path, q, nil, out)
}

// Account fetches /v2/account.
func (c *REST) Account(ctx context.Context) (Account, error) {
	var a Account
	err := c.trading(ctx, http.MethodGet, "/v2/account", nil, nil, &a)
	return a, err
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
// following pagination. The SIP feed includes extended-hours trades, so
// no additional flag is needed to receive extended-session bars.
func (c *REST) Bars(ctx context.Context, symbol, timeframe string, start, end time.Time, limit int) ([]Bar, error) {
	var out []Bar
	page := ""
	for {
		q := url.Values{
			"timeframe":  {timeframe},
			"start":      {start.UTC().Format(time.RFC3339)},
			"end":        {end.UTC().Format(time.RFC3339)},
			"limit":      {strconv.Itoa(limit)},
			"adjustment": {"raw"},
			"feed":       {"sip"},
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
