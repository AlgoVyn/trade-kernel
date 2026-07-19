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

	// OnInitial is invoked once after the first successful auth. OnReconnect
	// fires after every subsequent re-auth. Both run on the read goroutine.
	OnUpdate    func(TradeUpdate)
	OnInitial   func()
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
func (t *TradingWS) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := t.runOnce(ctx)
		if err != nil && t.OnError != nil && ctx.Err() == nil {
			t.OnError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

func (t *TradingWS) runOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, t.url, nil)
	if err != nil {
		return fmt.Errorf("dial trading ws: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(1 << 20)

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
		return fmt.Errorf("auth write: %w", err)
	}
	if err := write(map[string]any{
		"action": "listen",
		"data":   map[string]any{"streams": []string{"trade_updates"}},
	}); err != nil {
		return fmt.Errorf("listen write: %w", err)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("trading ws read: %w", err)
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
				// First auth → OnInitial; every later auth → OnReconnect.
				if !t.firstAuth {
					t.firstAuth = true
					if t.OnInitial != nil {
						t.OnInitial()
					}
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
