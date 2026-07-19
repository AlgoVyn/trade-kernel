package cmdline

import "testing"

func TestParseOrders(t *testing.T) {
	c, err := Parse("buy 250 lmt 152.30")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != KindOrder || c.Side != "buy" || c.Qty != 250 || c.Limit != 152.30 {
		t.Fatalf("%+v", c)
	}

	c, err = Parse("sell 100 mkt")
	if err != nil {
		t.Fatal(err)
	}
	if c.Side != "sell" || c.Qty != 100 || c.Limit != 0 {
		t.Fatalf("%+v", c)
	}

	c, err = Parse("b 50")
	if err != nil {
		t.Fatal(err)
	}
	if c.Side != "buy" || c.Qty != 50 {
		t.Fatalf("%+v", c)
	}

	if _, err := Parse("buy"); err == nil {
		t.Fatal("expected error: missing qty")
	}
	if _, err := Parse("buy -5"); err == nil {
		t.Fatal("expected error: negative qty")
	}
	if _, err := Parse("buy 10 lmt"); err == nil {
		t.Fatal("expected error: missing price")
	}
	if _, err := Parse("buy 10 foo 3"); err == nil {
		t.Fatal("expected error: bad order type")
	}
}

func TestParseOther(t *testing.T) {
	c, err := Parse("sym nvda")
	if err != nil || c.Kind != KindSymbol || c.Symbol != "NVDA" {
		t.Fatalf("%+v err=%v", c, err)
	}
	c, err = Parse("tf 5m")
	if err != nil || c.Kind != KindTF || c.TF != "5m" {
		t.Fatalf("%+v err=%v", c, err)
	}
	c, err = Parse("tf 2m")
	if err != nil || c.Kind != KindTF || c.TF != "2m" {
		t.Fatalf("custom tf: %+v err=%v", c, err)
	}
	c, err = Parse("tf 30s")
	if err != nil || c.Kind != KindTF || c.TF != "30s" {
		t.Fatalf("custom tf 30s: %+v err=%v", c, err)
	}
	c, err = Parse("preset 2")
	if err != nil || c.Kind != KindPreset || c.Preset != 2 {
		t.Fatalf("%+v err=%v", c, err)
	}
	c, err = Parse("confirm off")
	if err != nil || c.Kind != KindConfirm || c.On {
		t.Fatalf("%+v err=%v", c, err)
	}
	c, err = Parse("shading on")
	if err != nil || c.Kind != KindShading || !c.On {
		t.Fatalf("%+v err=%v", c, err)
	}
	for _, v := range []string{"flatten", "flat", "f"} {
		if c, err := Parse(v); err != nil || c.Kind != KindFlatten {
			t.Fatalf("%s: %+v err=%v", v, c, err)
		}
	}
	if c, err := Parse("cancel"); err != nil || c.Kind != KindCancel {
		t.Fatalf("%+v err=%v", c, err)
	}
	if c, err := Parse("unlock"); err != nil || c.Kind != KindUnlock {
		t.Fatalf("%+v err=%v", c, err)
	}
	if c, err := Parse("lock"); err != nil || c.Kind != KindLock || c.Reason != "manual" {
		t.Fatalf("lock: %+v err=%v", c, err)
	}
	if c, err := Parse("lock too hot"); err != nil || c.Kind != KindLock || c.Reason != "too hot" {
		t.Fatalf("lock reason: %+v err=%v", c, err)
	}
	if c, err := Parse("panic"); err != nil || c.Kind != KindPanic {
		t.Fatalf("panic: %+v err=%v", c, err)
	}
	if _, err := Parse("panic all"); err == nil {
		t.Fatal("expected error for panic all (active symbol only)")
	}
	if c, err := Parse("quit"); err != nil || c.Kind != KindQuit {
		t.Fatalf("%+v err=%v", c, err)
	}
	if _, err := Parse("bogus"); err == nil {
		t.Fatal("expected unknown command error")
	}
}
