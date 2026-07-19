// Package alpaca implements clients for the Alpaca trading REST API, the
// market-data REST API, and the market-data / trading WebSocket streams.
package alpaca

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// flexFloat unmarshals from a JSON number, a quoted numeric string, null,
// or the empty string — all of which Alpaca emits across endpoints (REST
// sends strings with the occasional null/""; the market WS sends numbers).
// This sidesteps the brittleness of encoding/json's ,string struct tag,
// which errors out on "" and is the reason we don't use it.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*f = 0
		return nil
	}
	// Quoted string: strip the quotes and parse.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
		if s == "" {
			*f = 0
			return nil
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// MarshalJSON emits a number (never a quoted string) so downstream
// consumers that expect a JSON number still work.
func (f flexFloat) MarshalJSON() ([]byte, error) { return json.Marshal(float64(f)) }

// Base URLs.
const (
	LiveTradingURL  = "https://api.alpaca.markets"
	PaperTradingURL = "https://paper-api.alpaca.markets"
	DataURL         = "https://data.alpaca.markets"

	LiveTradingWS  = "wss://api.alpaca.markets/stream"
	PaperTradingWS = "wss://paper-api.alpaca.markets/stream"
	MarketDataWS   = "wss://stream.data.alpaca.markets/v2/sip"
)

// Account is the /v2/account response (subset of fields used).
type Account struct {
	ID               string    `json:"id"`
	Status           string    `json:"status"`
	Currency         string    `json:"currency"`
	Cash             flexFloat `json:"cash"`
	Equity           flexFloat `json:"equity"`
	LastEquity       flexFloat `json:"last_equity"` // prior trading-day close equity
	BuyingPower      flexFloat `json:"buying_power"`
	PortfolioValue   flexFloat `json:"portfolio_value"`
	PatternDayTrader bool      `json:"pattern_day_trader"`
}

// PortfolioHistory is /v2/account/portfolio/history (subset).
// Equity and profit_loss are parallel series; base_value is the PnL baseline.
type PortfolioHistory struct {
	Timestamp  []int64     `json:"timestamp"`
	Equity     []flexFloat `json:"equity"`
	ProfitLoss []flexFloat `json:"profit_loss"`
	BaseValue  flexFloat   `json:"base_value"`
	Timeframe  string      `json:"timeframe"`
}

// LatestPnL returns the most recent profit/loss vs base_value.
// Prefer the last profit_loss sample; fall back to equity − base_value.
func (h PortfolioHistory) LatestPnL() (float64, bool) {
	if n := len(h.ProfitLoss); n > 0 {
		return float64(h.ProfitLoss[n-1]), true
	}
	if n := len(h.Equity); n > 0 && float64(h.BaseValue) != 0 {
		return float64(h.Equity[n-1]) - float64(h.BaseValue), true
	}
	return 0, false
}

// Position is a /v2/positions entry.
type Position struct {
	Symbol               string    `json:"symbol"`
	Qty                  flexFloat `json:"qty"`
	AvgEntryPrice        flexFloat `json:"avg_entry_price"`
	MarketValue          flexFloat `json:"market_value"`
	UnrealizedPL         flexFloat `json:"unrealized_pl"`          // total vs cost basis
	UnrealizedIntradayPL flexFloat `json:"unrealized_intraday_pl"` // mark change since prior close
	Side                 string    `json:"side"`                   // "long" or "short"
}

// SetQty sets the absolute position quantity (package-external writers).
func (p *Position) SetQty(q float64) { p.Qty = flexFloat(q) }

// SetAvgEntryPrice sets the average entry price (package-external writers).
func (p *Position) SetAvgEntryPrice(v float64) { p.AvgEntryPrice = flexFloat(v) }

// OrderRequest is the POST /v2/orders body.
type OrderRequest struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty"`
	Side          string `json:"side"`          // "buy" | "sell"
	Type          string `json:"type"`          // "market" | "limit"
	TimeInForce   string `json:"time_in_force"` // "day" | "ioc" | "gtc"
	LimitPrice    string `json:"limit_price,omitempty"`
	ExtendedHours bool   `json:"extended_hours,omitempty"`
	ClientOrderID string `json:"client_order_id,omitempty"`
}

// Order is a /v2/orders entry.
type Order struct {
	ID             string    `json:"id"`
	ClientOrderID  string    `json:"client_order_id"`
	Symbol         string    `json:"symbol"`
	Qty            flexFloat `json:"qty"`
	FilledQty      flexFloat `json:"filled_qty"`
	Side           string    `json:"side"`
	Type           string    `json:"type"`
	TimeInForce    string    `json:"time_in_force"`
	LimitPrice     flexFloat `json:"limit_price"`
	Status         string    `json:"status"`
	SubmittedAt    time.Time `json:"submitted_at"`
	FilledAt       time.Time `json:"filled_at"`
	FilledAvgPrice flexFloat `json:"filled_avg_price"`
}

// Clock is the /v2/clock response.
type Clock struct {
	IsOpen    bool      `json:"is_open"`
	Timestamp time.Time `json:"timestamp"`
	NextOpen  time.Time `json:"next_open"`
	NextClose time.Time `json:"next_close"`
}

// Asset is the /v2/assets/{symbol} response (subset).
type Asset struct {
	Symbol            string `json:"symbol"`
	Status            string `json:"status"`
	Tradable          bool   `json:"tradable"`
	Shortable         bool   `json:"shortable"`
	Fractionable      bool   `json:"fractionable"`
	OvernightTradable bool   `json:"overnight_tradable"`
}

// Bar is a single OHLCV bar from the market-data API.
type Bar struct {
	Timestamp  time.Time `json:"t"`
	Open       flexFloat `json:"o"`
	High       flexFloat `json:"h"`
	Low        flexFloat `json:"l"`
	Close      flexFloat `json:"c"`
	Volume     flexFloat `json:"v"`
	VWAP       flexFloat `json:"vw"`
	TradeCount int       `json:"n"`
}

// Trade is a trade message from the market-data WS ("T":"t").
type Trade struct {
	Symbol    string    `json:"S"`
	Price     flexFloat `json:"p"`
	Size      flexFloat `json:"s"`
	Timestamp time.Time `json:"t"`
	ID        int64     `json:"i"`
}

// Quote is a quote message from the market-data WS ("T":"q").
type Quote struct {
	Symbol    string    `json:"S"`
	BidPrice  flexFloat `json:"bp"`
	BidSize   flexFloat `json:"bs"`
	AskPrice  flexFloat `json:"ap"`
	AskSize   flexFloat `json:"as"`
	Timestamp time.Time `json:"t"`
}

// OptionalFloat is a JSON number/string that also tracks whether the field
// was present. Missing and null both yield Valid=false so callers can
// distinguish "omitted" from an explicit zero (e.g. position_qty on fills).
type OptionalFloat struct {
	V     float64
	Valid bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *OptionalFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	// null and empty string are "absent" — same class of bug as omit for
	// position_qty (treating "" as valid 0 would wipe a cached position).
	if s == "null" || s == `""` || s == "" {
		o.V, o.Valid = 0, false
		return nil
	}
	var f flexFloat
	if err := f.UnmarshalJSON(b); err != nil {
		return err
	}
	o.V = float64(f)
	o.Valid = true
	return nil
}

// MarshalJSON emits null when invalid, otherwise a number.
func (o OptionalFloat) MarshalJSON() ([]byte, error) {
	if !o.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(o.V)
}

// TradeUpdate is an event from the trading WS stream ("trade_updates").
type TradeUpdate struct {
	Event string `json:"event"` // fill, partial_fill, canceled, expired, rejected, ...
	Order Order  `json:"order"`
	// Price and Qty are the individual fill for this event (not cumulative).
	// Prefer these over Order.FilledAvgPrice when recomputing average entry.
	Price flexFloat `json:"price"`
	Qty   flexFloat `json:"qty"`
	// PositionQty is present on fill events for the affected position.
	// Valid=false when the field is omitted or null — do not treat that as flat.
	PositionQty OptionalFloat `json:"position_qty"`
}

// apiError is Alpaca's error envelope.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }
