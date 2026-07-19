package alpaca

import (
	"encoding/json"
	"math"
	"testing"
)

func TestFlexFloatUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		json string
		want float64
	}{
		{"number", `150.25`, 150.25},
		{"quoted string", `"150.25"`, 150.25},
		{"integer string", `"300"`, 300},
		{"null", `null`, 0},
		{"empty string quoted", `""`, 0},
		{"zero string", `"0"`, 0},
		{"negative", `"-42.5"`, -42.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f flexFloat
			if err := json.Unmarshal([]byte(tc.json), &f); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			if math.Abs(float64(f)-tc.want) > 1e-9 {
				t.Fatalf("got %v, want %v", float64(f), tc.want)
			}
		})
	}
}

func TestOrderDecodesNullLimitPrice(t *testing.T) {
	// Alpaca returns limit_price: null for market orders and a quoted
	// string for limits. Both must decode without error.
	for _, raw := range []string{
		`{"limit_price":null,"qty":"100"}`,
		`{"limit_price":"150.25","qty":"100"}`,
		`{"limit_price":"","qty":""}`,
		`{}`,
	} {
		var o Order
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	// Spot-check the string case carries through.
	var o Order
	if err := json.Unmarshal([]byte(`{"limit_price":"152.30","qty":"100","filled_qty":"0"}`), &o); err != nil {
		t.Fatal(err)
	}
	if float64(o.LimitPrice) != 152.30 {
		t.Fatalf("LimitPrice = %v, want 152.30", o.LimitPrice)
	}
	if float64(o.Qty) != 100 {
		t.Fatalf("Qty = %v, want 100", o.Qty)
	}
}

func TestAccountDecodes(t *testing.T) {
	// Account sends everything as quoted strings.
	var a Account
	raw := `{"equity":"100000.50","last_equity":"99000.00","cash":"50000","buying_power":"200000"}`
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if float64(a.Equity) != 100000.50 {
		t.Fatalf("Equity = %v", a.Equity)
	}
	if float64(a.LastEquity) != 99000 {
		t.Fatalf("LastEquity = %v", a.LastEquity)
	}
	if float64(a.Cash) != 50000 {
		t.Fatalf("Cash = %v", a.Cash)
	}
}

func TestPortfolioHistoryLatestPnL(t *testing.T) {
	h := PortfolioHistory{
		Equity:     []flexFloat{100000, 100500, 101200},
		ProfitLoss: []flexFloat{0, 500, 1200},
		BaseValue:  100000,
	}
	v, ok := h.LatestPnL()
	if !ok || v != 1200 {
		t.Fatalf("LatestPnL = %v %v, want 1200 true", v, ok)
	}
	// Fall back to equity − base when profit_loss omitted.
	h2 := PortfolioHistory{Equity: []flexFloat{100000, 100250}, BaseValue: 100000}
	v, ok = h2.LatestPnL()
	if !ok || v != 250 {
		t.Fatalf("fallback LatestPnL = %v %v", v, ok)
	}
}

func TestTradeUpdateOptionalPositionQty(t *testing.T) {
	var tu TradeUpdate
	if err := json.Unmarshal([]byte(`{"event":"fill","order":{"id":"1"}}`), &tu); err != nil {
		t.Fatal(err)
	}
	if tu.PositionQty.Valid {
		t.Fatal("omitted position_qty should be invalid")
	}
	if err := json.Unmarshal([]byte(`{"event":"fill","order":{"id":"1"},"position_qty":"100.5"}`), &tu); err != nil {
		t.Fatal(err)
	}
	if !tu.PositionQty.Valid || tu.PositionQty.V != 100.5 {
		t.Fatalf("got %+v", tu.PositionQty)
	}
	if err := json.Unmarshal([]byte(`{"event":"fill","order":{"id":"1"},"position_qty":null}`), &tu); err != nil {
		t.Fatal(err)
	}
	if tu.PositionQty.Valid {
		t.Fatal("null position_qty should be invalid")
	}
	if err := json.Unmarshal([]byte(`{"event":"fill","order":{"id":"1"},"position_qty":""}`), &tu); err != nil {
		t.Fatal(err)
	}
	if tu.PositionQty.Valid {
		t.Fatal("empty-string position_qty should be invalid")
	}
}
