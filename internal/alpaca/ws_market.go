package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// wsPingInterval / wsPingTimeout drive the connection watchdog. Alpaca's
// market-data stream can go completely silent for hours (the overnight
// session has no SIP traffic), and a half-open TCP connection would
// otherwise leave the chart frozen with no read error. The ping forces
// traffic; a missing pong kills the connection so Run reconnects.
const (
	wsPingInterval = 30 * time.Second
	wsPingTimeout  = 15 * time.Second
)

// pingWatchdog pings the connection every wsPingInterval until ctx is
// done. A failed ping (dead peer, no pong within wsPingTimeout) closes
// the connection so the blocked Read errors out and the Run loop
// reconnects. coder/websocket Ping waits for the pong, which the
// concurrent Read delivers.
func pingWatchdog(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, wsPingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				_ = conn.Close(websocket.StatusGoingAway, "ping watchdog")
				return
			}
		}
	}
}

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
//
// Backoff resets only when a session both authenticated and stayed up for
// at least 5s. Auth-then-die flaps (common during brief network blips) keep
// the exponential schedule so we do not hammer the broker; once a session
// is healthy for 5s, the next reconnect starts again at 250ms.
func (m *MarketWS) Run(ctx context.Context) {
	backoff := 250 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		authed, err := m.runOnce(ctx)
		if err != nil && m.OnError != nil && ctx.Err() == nil {
			m.OnError(err)
		}
		m.setConn(nil)
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

func (m *MarketWS) setConn(c *websocket.Conn) {
	m.mu.Lock()
	m.conn = c
	m.mu.Unlock()
}

// runOnce returns authed=true if authentication completed (so the Run
// loop can reset backoff after a healthy session ends).
func (m *MarketWS) runOnce(ctx context.Context) (authed bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, m.url, nil)
	if err != nil {
		return false, fmt.Errorf("dial market ws: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(1 << 20)
	go pingWatchdog(ctx, conn)

	m.setConn(conn)

	auth := map[string]any{"action": "auth", "key": m.keyID, "secret": m.secretKey}
	b, _ := json.Marshal(auth)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return false, fmt.Errorf("auth write: %w", err)
	}

	// Read auth responses, then subscribe.
	authenticated := false
	subscribed := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return authenticated, fmt.Errorf("market ws read: %w", err)
		}
		// Alpaca market-data frames are a top-level JSON array of messages
		// that all share the same "T" field (e.g. "t", "q", "success"). The
		// array always opens with [{"T":"…" and every member repeats the
		// same T within one frame. Peek the T cheaply off the first object
		// so high-volume trade/quote frames skip the per-message head
		// unmarshal (one fewer decode pass per message on the ingest path).
		T := peekMessageType(data)
		switch T {
		case "t":
			if m.OnTrade != nil {
				for _, tr := range unmarshalTrades(data) {
					m.OnTrade(tr)
				}
			}
		case "q":
			if m.OnQuote != nil {
				for _, q := range unmarshalQuotes(data) {
					m.OnQuote(q)
				}
			}
		case "success":
			for _, raw := range splitMessages(data) {
				var head struct {
					Msg string `json:"msg"`
				}
				if json.Unmarshal(raw, &head) != nil {
					continue
				}
				if head.Msg == "authenticated" {
					authenticated = true
					m.mu.Lock()
					err := m.subscribeLocked(ctx)
					m.mu.Unlock()
					if err != nil {
						return true, fmt.Errorf("subscribe: %w", err)
					}
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
			var head struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			}
			for _, raw := range splitMessages(data) {
				if json.Unmarshal(raw, &head) == nil && head.Code != 0 {
					break
				}
			}
			return authenticated, fmt.Errorf("market ws error %d: %s", head.Code, head.Msg)
		}
	}
}

// peekMessageType returns the value of the first message's "T" field in a
// top-level JSON array of Alpaca market-data frames, scanning only the bytes
// up to the field rather than decoding the whole message. Returns "" when the
// frame shape is not recognized (caller falls back to no-op). T is always the
// first key in Alpaca's envelope, so this stays bounded and allocation-free.
func peekMessageType(data []byte) string {
	// Expect [{"T":"x" … Find the first opening brace, then the "T" key.
	i := bytes.IndexByte(data, '{')
	if i < 0 {
		return ""
	}
	s := data[i:]
	// Find "\"T\"" as the key.
	k := bytes.Index(s, []byte(`"T"`))
	if k < 0 {
		// Fall back to a field order tolerant search for "T" in compact form.
		k = bytes.Index(s, []byte(`"T":`))
		if k < 0 {
			return ""
		}
	}
	// Skip past the key and any whitespace/colon to the value.
	rest := s[k:]
	c := bytes.IndexByte(rest, ':')
	if c < 0 {
		return ""
	}
	rest = rest[c+1:]
	// Trim leading whitespace.
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) < 2 || rest[0] != '"' {
		return ""
	}
	// Read until the closing quote.
	end := bytes.IndexByte(rest[1:], '"')
	if end < 0 {
		return ""
	}
	return string(rest[1 : 1+end])
}

// splitMessages returns each top-level object in a JSON array as a RawMessage
// slice. Used for control-channel frames (success/error) that are infrequent
// and need their fields decoded — the hot trade/quote path uses the typed
// unmarshalTrades/unmarshalQuotes helpers instead.
func splitMessages(data []byte) []json.RawMessage {
	var msgs []json.RawMessage
	if json.Unmarshal(data, &msgs) != nil {
		return nil
	}
	return msgs
}

// unmarshalTrades decodes a top-level array of trade messages directly into
// []Trade, avoiding the intermediate per-message RawMessage + head decode.
func unmarshalTrades(data []byte) []Trade {
	var ts []Trade
	if json.Unmarshal(data, &ts) != nil {
		return nil
	}
	return ts
}

// unmarshalQuotes decodes a top-level array of quote messages directly into
// []Quote, avoiding the intermediate per-message RawMessage + head decode.
func unmarshalQuotes(data []byte) []Quote {
	var qs []Quote
	if json.Unmarshal(data, &qs) != nil {
		return nil
	}
	return qs
}
