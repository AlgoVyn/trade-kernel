package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/bars"
	"trade-kernel/internal/config"
	"trade-kernel/internal/execution"
	"trade-kernel/internal/risk"
	"trade-kernel/internal/session"
	"trade-kernel/internal/state"
)

type fakeExec struct {
	calls []string
}

func (f *fakeExec) Buy(_ context.Context, sym string, qty int) (alpaca.Order, error) {
	f.calls = append(f.calls, "buy "+sym)
	return alpaca.Order{ID: "o1"}, nil
}
func (f *fakeExec) Sell(_ context.Context, sym string, qty int) (alpaca.Order, error) {
	f.calls = append(f.calls, "sell "+sym)
	return alpaca.Order{ID: "o2"}, nil
}
func (f *fakeExec) LimitBuy(_ context.Context, sym string, qty int, px float64) (alpaca.Order, error) {
	f.calls = append(f.calls, "limitbuy "+sym)
	return alpaca.Order{ID: "o3"}, nil
}
func (f *fakeExec) LimitSell(_ context.Context, sym string, qty int, px float64) (alpaca.Order, error) {
	f.calls = append(f.calls, "limitsell "+sym)
	return alpaca.Order{ID: "o4"}, nil
}
func (f *fakeExec) Flatten(_ context.Context, sym string, qty float64) (alpaca.Order, error) {
	f.calls = append(f.calls, "flatten "+sym)
	return alpaca.Order{ID: "o5"}, nil
}
func (f *fakeExec) CancelAll(_ context.Context) error {
	f.calls = append(f.calls, "cancelall")
	return nil
}

func testDeps(t *testing.T) (Deps, *fakeExec, *state.Store, *bars.Aggregator) {
	t.Helper()
	cfg := &config.Config{
		SizePresets: []int{100, 250},
		Chart:       config.Chart{Timeframe: "1m", BarsVisible: 60},
	}
	fx := &fakeExec{}
	agg := bars.NewAggregator(3, 3)
	st := state.NewStore()
	st.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		[]alpaca.Position{{Symbol: "AAPL", Qty: 300, Side: "long", AvgEntryPrice: 150, UnrealizedPL: 42}},
		nil,
	)
	eng := session.NewEngine(nil)
	d := Deps{
		Cfg:          cfg,
		Agg:          agg,
		Store:        st,
		Exec:         fx,
		Risk:         risk.NewChecker(risk.Limits{MaxOrderQty: 1000, MaxPositionQty: 5000}, st, nil),
		Sessions:     eng,
		Builder:      execution.NewBuilder(agg, nil, 25, 3*time.Second),
		Latency:      NewLatencyTracker(16),
		SwitchSymbol: func(s string) error { return nil },
		Paper:        true,
	}
	cfg.DefaultSymbol = "AAPL"
	cfg.ConfirmOrders = false
	return d, fx, st, agg
}

func key(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func drain(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		m.Update(msg)
	}
}

func TestHotkeyBuyFlow(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	_, cmd := m.handleKey(key('B'))
	drain(m, cmd)
	if len(fx.calls) != 1 || fx.calls[0] != "buy AAPL" {
		t.Fatalf("calls = %v", fx.calls)
	}

	// Sell via hotkey.
	_, cmd = m.handleKey(key('S'))
	drain(m, cmd)
	if len(fx.calls) != 2 || fx.calls[1] != "sell AAPL" {
		t.Fatalf("calls = %v", fx.calls)
	}
}

func TestConfirmationFlow(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	d.Cfg.ConfirmOrders = true
	m := NewModel(d)

	_, cmd := m.handleKey(key('B'))
	if cmd != nil {
		t.Fatal("no order should fire before confirmation")
	}
	if m.pending == nil {
		t.Fatal("expected pending confirmation")
	}
	// Abort with 'n'.
	m.handleKey(key('n'))
	if m.pending != nil || len(fx.calls) != 0 {
		t.Fatal("abort failed")
	}
	// Confirm with 'y'.
	m.handleKey(key('B'))
	_, cmd = m.handleKey(key('y'))
	drain(m, cmd)
	if len(fx.calls) != 1 {
		t.Fatalf("calls = %v", fx.calls)
	}
}

func TestAddReduceDirection(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	m := NewModel(d)
	// Long 300 AAPL: reduce (D) sells.
	_, cmd := m.handleKey(key('D'))
	drain(m, cmd)
	if len(fx.calls) != 1 || fx.calls[0] != "sell AAPL" {
		t.Fatalf("reduce: calls = %v", fx.calls)
	}
	// Add (A) buys.
	_, cmd = m.handleKey(key('A'))
	drain(m, cmd)
	if len(fx.calls) != 2 || fx.calls[1] != "buy AAPL" {
		t.Fatalf("add: calls = %v", fx.calls)
	}
}

func TestPanicFlow(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	m := NewModel(d)
	_, cmd := m.handleKey(key('X'))
	drain(m, cmd)
	if len(fx.calls) != 2 || fx.calls[0] != "cancelall" || fx.calls[1] != "flatten AAPL" {
		t.Fatalf("panic: calls = %v", fx.calls)
	}
}

func TestCommandLine(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	m := NewModel(d)
	switched := ""
	d.SwitchSymbol = func(s string) error { switched = s; return nil }
	m.d.SwitchSymbol = d.SwitchSymbol

	// Open cmdline, type a symbol switch command.
	m.handleKey(key(':'))
	for _, r := range "sym NVDA" {
		if r == ' ' {
			m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
		} else {
			m.handleKey(key(r))
		}
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	drain(m, cmd)
	if switched != "NVDA" || m.symbol != "NVDA" {
		t.Fatalf("switched=%q symbol=%q", switched, m.symbol)
	}

	// Limit order via cmdline.
	m.handleKey(key(':'))
	for _, r := range "buy 50 lmt 123.45" {
		if r == ' ' {
			m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
		} else {
			m.handleKey(key(r))
		}
	}
	_, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	drain(m, cmd)
	if len(fx.calls) != 1 || fx.calls[0] != "limitbuy NVDA" {
		t.Fatalf("calls = %v", fx.calls)
	}
}

func TestRiskBlocksHotkey(t *testing.T) {
	d, fx, st, _ := testDeps(t)
	d.Risk = risk.NewChecker(risk.Limits{MaxOrderQty: 50}, st, nil)
	m := NewModel(d)
	m.d = d
	_, cmd := m.handleKey(key('B')) // preset 100 > max 50
	drain(m, cmd)
	if len(fx.calls) != 0 {
		t.Fatalf("risk should have blocked: %v", fx.calls)
	}
	if !strings.Contains(m.status, "max order size") {
		t.Fatalf("status = %q", m.status)
	}
}

func TestViewRenders(t *testing.T) {
	d, _, _, agg := testDeps(t)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		px := 150 + float64(i%7)
		agg.OnTrade("AAPL", px, 10, base.Add(time.Duration(i)*time.Minute))
	}
	agg.OnQuote("AAPL", 150.1, 150.2, base)

	out := m.View()
	if out == "" {
		t.Fatal("empty view")
	}
	for _, want := range []string{"AAPL", "PAPER", "POSITION", "equity", "150"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q", want)
		}
	}
	// Narrow terminal hides the side panel without panicking.
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	if m.View() == "" {
		t.Fatal("empty narrow view")
	}
}

func TestRenderCandlesShape(t *testing.T) {
	agg := bars.NewAggregator(2, 3)
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		agg.OnTrade("AAPL", 100+float64(i%5), 5, base.Add(time.Duration(i)*time.Minute))
	}
	snap := agg.Snapshot(bars.TF1m, 40)
	lines := renderCandles(snap, 40, 12, ChartOpts{ShowSMA: true, ShowEMA: true, ShowVWAP: true})
	if len(lines) != 12 {
		t.Fatalf("want 12 lines, got %d", len(lines))
	}
	vol := renderVolume(snap, 40, 4, false)
	if len(vol) != 4 {
		t.Fatalf("want 4 volume lines, got %d", len(vol))
	}
	// Empty snapshot renders blanks without panic.
	if got := renderCandles(bars.Snapshot{}, 10, 3, ChartOpts{}); len(got) != 3 {
		t.Fatalf("empty: %d lines", len(got))
	}
}
