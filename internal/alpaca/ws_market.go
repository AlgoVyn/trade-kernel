package alpaca

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// MarketWS is a SIP market-data WebSocket client with automatic
// reconnect, resubscribe, and symbol hot-switching.
type MarketWS struct {
	url       string
	keyID     string
	secretKey string

	mu       sync.Mutex
	symbol   string
	conn     *websocket.Conn // non-nil while connected
	firstSub bool            // set after the first successful subscribe

	// Callbacks are invoked from the read goroutine; they must be fast
	// (hand off to channels) and must not call back into MarketWS.
	OnTrade     func(Trade)
	OnQuote     func(Quote)
	OnInitial   func() // fires once after the first successful auth+resubscribe
	OnReconnect func() // fires after every subsequent re-auth+resubscribe
	OnError     func(error)
}

// NewMarketWS creates a SIP client for the given credentials.
func NewMarketWS(keyID, secretKey string) *MarketWS {
	return &MarketWS{url: MarketDataWS, keyID: keyID, secretKey: secretKey}
}

// SetSymbol hot-switches the subscription. Safe to call before Run and
// from any goroutine. If connected, it sends unsubscribe+subscribe.
func (m *MarketWS) SetSymbol(symbol string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.symbol
	m.symbol = symbol
	if m.conn == nil || old == symbol {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if old != "" {
		_ = m.write(ctx, map[string]any{
			"action": "unsubscribe", "trades": []string{old}, "quotes": []string{old},
		})
	}
	return m.subscribeLocked(ctx)
}

// Symbol returns the currently subscribed symbol.
func (m *MarketWS) Symbol() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.symbol
}

func (m *MarketWS) write(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.conn.Write(ctx, websocket.MessageText, b)
}

func (m *MarketWS) subscribeLocked(ctx context.Context) error {
	if m.symbol == "" {
		return nil
	}
	return m.write(ctx, map[string]any{
		"action": "subscribe", "trades": []string{m.symbol}, "quotes": []string{m.symbol},
	})
}

// Run connects and maintains the connection until ctx is cancelled,
// reconnecting with exponential backoff (250ms → 10s cap).
func (m *MarketWS) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		err := m.runOnce(ctx)
		if err != nil && m.OnError != nil && ctx.Err() == nil {
			m.OnError(err)
		}
		m.setConn(nil)
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

func (m *MarketWS) setConn(c *websocket.Conn) {
	m.mu.Lock()
	m.conn = c
	m.mu.Unlock()
}

func (m *MarketWS) runOnce(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, m.url, nil)
	if err != nil {
		return fmt.Errorf("dial market ws: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(1 << 20)

	m.setConn(conn)

	auth := map[string]any{"action": "auth", "key": m.keyID, "secret": m.secretKey}
	b, _ := json.Marshal(auth)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("auth write: %w", err)
	}

	// Read auth responses, then subscribe.
	authenticated := false
	subscribed := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("market ws read: %w", err)
		}
		var msgs []json.RawMessage
		if err := json.Unmarshal(data, &msgs); err != nil {
			continue
		}
		for _, raw := range msgs {
			var head struct {
				T    string `json:"T"`
				Msg  string `json:"msg"`
				Code int    `json:"code"`
			}
			if json.Unmarshal(raw, &head) != nil {
				continue
			}
			switch head.T {
			case "success":
				if head.Msg == "authenticated" {
					authenticated = true
					m.mu.Lock()
					err := m.subscribeLocked(ctx)
					m.mu.Unlock()
					if err != nil {
						return fmt.Errorf("subscribe: %w", err)
					}
				}
			case "subscription":
				// First successful auth+subscribe → OnInitial (once);
				// every later dial → OnReconnect.
				if !subscribed && authenticated {
					subscribed = true
					if !m.firstSub {
						m.firstSub = true
						if m.OnInitial != nil {
							m.OnInitial()
						}
					} else if m.OnReconnect != nil {
						m.OnReconnect()
					}
				}
			case "error":
				return fmt.Errorf("market ws error %d: %s", head.Code, head.Msg)
			case "t":
				var tr Trade
				if json.Unmarshal(raw, &tr) == nil && m.OnTrade != nil {
					m.OnTrade(tr)
				}
			case "q":
				var q Quote
				if json.Unmarshal(raw, &q) == nil && m.OnQuote != nil {
					m.OnQuote(q)
				}
			}
		}
	}
}
