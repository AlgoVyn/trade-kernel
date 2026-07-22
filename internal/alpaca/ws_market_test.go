package alpaca

import (
	"encoding/json"
	"testing"
	"time"
)

// Alpaca market-data frames always include "T" (message type) alongside "t"
// (timestamp). encoding/json matches keys case-insensitively, so Trade/Quote
// must declare an exact "T" field or Timestamp steals the type letter and
// Unmarshal fails — dropping every live print.
func TestUnmarshalTrades_WSFrameWithTypeField(t *testing.T) {
	raw := []byte(`[{"T":"t","i":71675275204442,"S":"SOXL","x":"D","p":148.18,"s":100,"t":"2026-07-22T08:27:05.710666767Z","c":[" ","T"],"z":"B"}]`)
	ts := unmarshalTrades(raw)
	if len(ts) != 1 {
		// Direct decode error for diagnostics when the helper returns nil.
		var probe []Trade
		err := json.Unmarshal(raw, &probe)
		t.Fatalf("unmarshalTrades len=%d (json err=%v)", len(ts), err)
	}
	tr := ts[0]
	if tr.Symbol != "SOXL" {
		t.Errorf("Symbol=%q", tr.Symbol)
	}
	if float64(tr.Price) != 148.18 {
		t.Errorf("Price=%v", tr.Price)
	}
	if float64(tr.Size) != 100 {
		t.Errorf("Size=%v", tr.Size)
	}
	if tr.ID != 71675275204442 {
		t.Errorf("ID=%d", tr.ID)
	}
	wantTS := time.Date(2026, 7, 22, 8, 27, 5, 710666767, time.UTC)
	if !tr.Timestamp.Equal(wantTS) {
		t.Errorf("Timestamp=%v want %v", tr.Timestamp, wantTS)
	}
	if peekMessageType(raw) != "t" {
		t.Errorf("peekMessageType=%q", peekMessageType(raw))
	}
}

func TestUnmarshalQuotes_WSFrameWithTypeField(t *testing.T) {
	raw := []byte(`[{"T":"q","S":"SOXL","bx":"K","bp":148,"bs":300,"ax":"K","ap":148.18,"as":100,"c":["R"],"z":"B","t":"2026-07-22T08:27:14.871787877Z"}]`)
	qs := unmarshalQuotes(raw)
	if len(qs) != 1 {
		var probe []Quote
		err := json.Unmarshal(raw, &probe)
		t.Fatalf("unmarshalQuotes len=%d (json err=%v)", len(qs), err)
	}
	q := qs[0]
	if q.Symbol != "SOXL" {
		t.Errorf("Symbol=%q", q.Symbol)
	}
	if float64(q.BidPrice) != 148 || float64(q.AskPrice) != 148.18 {
		t.Errorf("bid/ask=%v/%v", q.BidPrice, q.AskPrice)
	}
	if peekMessageType(raw) != "q" {
		t.Errorf("peekMessageType=%q", peekMessageType(raw))
	}
}

// REST trade pages omit "T"; unmarshaling must still succeed.
func TestTradeUnmarshal_RESTShapeNoTypeField(t *testing.T) {
	raw := []byte(`[{"i":1,"S":"SOXL","p":100.5,"s":10,"t":"2026-07-22T08:00:00Z"}]`)
	ts := unmarshalTrades(raw)
	if len(ts) != 1 || ts[0].Symbol != "SOXL" || float64(ts[0].Price) != 100.5 {
		t.Fatalf("got %+v", ts)
	}
}
