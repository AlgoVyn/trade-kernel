package alpaca

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// TradingWS is a client for the Alpaca trading WebSocket stream
// (trade_updates: fills, cancellations, rejections).
type TradingWS struct {
	url       string
	keyID     string
	secretKey string

	firstAuth bool // set on first successful auth; gates OnReconnect

	// OnReconnect fires after every re-auth (the initial startup reconcile
	// is synchronous in cmd/trade-kernel; only live drops need a callback).
	// Runs on the read goroutine.
	OnUpdate    func(TradeUpdate)
	OnReconnect func()
	OnError     func(error)
}

// NewTradingWS creates a trading stream client; paper selects the paper
// endpoint.
func NewTradingWS(keyID, secretKey string, paper bool) *TradingWS {
	url := LiveTradingWS
	if paper {
		url = PaperTradingWS
	}
	return &TradingWS{url: url, keyID: keyID, secretKey: secretKey}
}

// Run connects and maintains the connection until ctx is cancelled.
//
// Backoff (250ms → 10s cap) resets only when a session both authorized and
// stayed up for at least 5s — same anti-flap rule as MarketWS. Sub-5s
// auth-then-die sessions keep exponential backoff.
func (t *TradingWS) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		authed, err := t.runOnce(ctx)
		if err != nil && t.OnError != nil && ctx.Err() == nil {
			t.OnError(err)
		}
		if authed && time.Since(start) >= 5*time.Second {
			backoff = 250 * time.Millisecond
		} else {
			backoff *= 2
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func (t *TradingWS) runOnce(ctx context.Context) (authed bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, t.url, nil)
	if err != nil {
		return false, fmt.Errorf("dial trading ws: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(1 << 20)
	go pingWatchdog(ctx, conn)

	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return conn.Write(ctx, websocket.MessageText, b)
	}

	if err := write(map[string]any{
		"action": "authenticate",
		"data":   map[string]string{"key_id": t.keyID, "secret_key": t.secretKey},
	}); err != nil {
		return false, fmt.Errorf("auth write: %w", err)
	}
	if err := write(map[string]any{
		"action": "listen",
		"data":   map[string]any{"streams": []string{"trade_updates"}},
	}); err != nil {
		return false, fmt.Errorf("listen write: %w", err)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return authed, fmt.Errorf("trading ws read: %w", err)
		}
		var env struct {
			Stream string          `json:"stream"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Stream {
		case "authorization":
			var st struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(env.Data, &st) == nil && st.Status == "authorized" {
				authed = true
				// First auth establishes the session (startup reconcile is
				// synchronous in main.go); every later auth is a reconnect.
				if !t.firstAuth {
					t.firstAuth = true
				} else if t.OnReconnect != nil {
					t.OnReconnect()
				}
			}
		case "trade_updates":
			var tu TradeUpdate
			if json.Unmarshal(env.Data, &tu) == nil && t.OnUpdate != nil {
				t.OnUpdate(tu)
			}
		}
	}
}
