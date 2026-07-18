// Package alpaca implements clients for the Alpaca trading REST API, the
// market-data REST API, and the market-data / trading WebSocket streams.
package alpaca

import "time"

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
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	Currency         string  `json:"currency"`
	Cash             float64 `json:"cash,string"`
	Equity           float64 `json:"equity,string"`
	BuyingPower      float64 `json:"buying_power,string"`
	PortfolioValue   float64 `json:"portfolio_value,string"`
	PatternDayTrader bool    `json:"pattern_day_trader"`
}

// Position is a /v2/positions entry.
type Position struct {
	Symbol        string  `json:"symbol"`
	Qty           float64 `json:"qty,string"`
	AvgEntryPrice float64 `json:"avg_entry_price,string"`
	MarketValue   float64 `json:"market_value,string"`
	UnrealizedPL  float64 `json:"unrealized_pl,string"`
	Side          string  `json:"side"` // "long" or "short"
}

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
	Qty            float64   `json:"qty,string"`
	FilledQty      float64   `json:"filled_qty,string"`
	Side           string    `json:"side"`
	Type           string    `json:"type"`
	TimeInForce    string    `json:"time_in_force"`
	LimitPrice     float64   `json:"limit_price,string"`
	Status         string    `json:"status"`
	SubmittedAt    time.Time `json:"submitted_at"`
	FilledAt       time.Time `json:"filled_at"`
	FilledAvgPrice float64   `json:"filled_avg_price,string"`
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
	Open       float64   `json:"o"`
	High       float64   `json:"h"`
	Low        float64   `json:"l"`
	Close      float64   `json:"c"`
	Volume     float64   `json:"v"`
	VWAP       float64   `json:"vw"`
	TradeCount int       `json:"n"`
}

// Trade is a trade message from the market-data WS ("T":"t").
type Trade struct {
	Symbol    string    `json:"S"`
	Price     float64   `json:"p"`
	Size      float64   `json:"s"`
	Timestamp time.Time `json:"t"`
	ID        int64     `json:"i"`
}

// Quote is a quote message from the market-data WS ("T":"q").
type Quote struct {
	Symbol    string    `json:"S"`
	BidPrice  float64   `json:"bp"`
	BidSize   float64   `json:"bs"`
	AskPrice  float64   `json:"ap"`
	AskSize   float64   `json:"as"`
	Timestamp time.Time `json:"t"`
}

// TradeUpdate is an event from the trading WS stream ("trade_updates").
type TradeUpdate struct {
	Event string `json:"event"` // fill, partial_fill, canceled, expired, rejected, ...
	Order Order  `json:"order"`
	// PositionQty is present on fill events for the affected position.
	PositionQty float64 `json:"position_qty,string"`
}

// apiError is Alpaca's error envelope.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }
