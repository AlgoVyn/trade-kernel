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
func (f *fakeExec) CancelSymbol(_ context.Context, sym string) error {
	f.calls = append(f.calls, "cancelsym "+sym)
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
	if len(fx.calls) != 2 || fx.calls[0] != "cancelsym AAPL" || fx.calls[1] != "flatten AAPL" {
		t.Fatalf("panic: calls = %v", fx.calls)
	}
}

// TestPanicFlattensActiveSymbolOnly verifies that X cancels and flattens
// only the currently selected symbol — other positions/orders remain.
func TestPanicFlattensActiveSymbolOnly(t *testing.T) {
	d, fx, st, _ := testDeps(t)
	st.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		[]alpaca.Position{
			{Symbol: "AAPL", Qty: 300, Side: "long", AvgEntryPrice: 150},
			{Symbol: "NVDA", Qty: 100, Side: "long", AvgEntryPrice: 900},
		},
		nil,
	)
	m := NewModel(d)
	_, cmd := m.handleKey(key('X'))
	drain(m, cmd)

	if len(fx.calls) != 2 {
		t.Fatalf("want cancelsym AAPL + flatten AAPL, got %v", fx.calls)
	}
	if fx.calls[0] != "cancelsym AAPL" {
		t.Fatalf("first call = %q, want cancelsym AAPL", fx.calls[0])
	}
	if fx.calls[1] != "flatten AAPL" {
		t.Fatalf("second call = %q, want flatten AAPL (not other symbols)", fx.calls[1])
	}
	if !strings.Contains(m.status, "+1 other symbols still open") {
		t.Fatalf("panic status should warn about other open symbols, got %q", m.status)
	}
}

// TestPanicAllCancelsAndFlattensEverySymbol covers Ctrl+X / :panic all.
func TestPanicAllCancelsAndFlattensEverySymbol(t *testing.T) {
	d, fx, st, _ := testDeps(t)
	st.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		[]alpaca.Position{
			{Symbol: "AAPL", Qty: 300, Side: "long", AvgEntryPrice: 150},
			{Symbol: "NVDA", Qty: 100, Side: "long", AvgEntryPrice: 900},
		},
		nil,
	)
	m := NewModel(d)
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	drain(m, cmd)

	if len(fx.calls) < 3 {
		t.Fatalf("want cancelall + flatten AAPL + flatten NVDA, got %v", fx.calls)
	}
	if fx.calls[0] != "cancelall" {
		t.Fatalf("first call = %q, want cancelall", fx.calls[0])
	}
	seen := map[string]bool{}
	for _, c := range fx.calls[1:] {
		seen[c] = true
	}
	if !seen["flatten AAPL"] || !seen["flatten NVDA"] {
		t.Fatalf("flatten calls incomplete: %v", fx.calls)
	}
}

func TestLockAndUnlockCommands(t *testing.T) {
	d, _, _, _ := testDeps(t)
	m := NewModel(d)
	m.cmdActive = true
	m.cmdBuf = "lock too hot"
	_, c := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	drain(m, c)
	if locked, reason := m.d.Risk.Locked(); !locked || reason != "too hot" {
		t.Fatalf("Locked() = %v %q, want locked too hot", locked, reason)
	}
	m.cmdActive = true
	m.cmdBuf = "unlock"
	_, c = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	drain(m, c)
	if locked, _ := m.d.Risk.Locked(); locked {
		t.Fatal("still locked after :unlock")
	}
}

func keyLeft() tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyLeft} }
func keyRight() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRight} }
func keyShiftTab() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyShiftTab} }

// feedHistory loads N closed 1-minute bars into agg so panning has data.
func feedHistory(t *testing.T, agg *bars.Aggregator, n int) {
	t.Helper()
	base := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		agg.OnTrade("AAPL", float64(100+i), 1, base.Add(time.Duration(i)*time.Minute))
	}
}

func TestPanLeftAndRight(t *testing.T) {
	d, _, _, agg := testDeps(t)
	feedHistory(t, agg, 30) // 29 closed bars + forming
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.panOffset != 0 {
		t.Fatalf("initial panOffset = %d, want 0", m.panOffset)
	}

	// ← pans back (increments offset).
	m.handleKey(keyLeft())
	if m.panOffset == 0 {
		t.Fatal("pan_left should increase panOffset")
	}
	back := m.panOffset

	// → pans forward (decrements offset).
	m.handleKey(keyRight())
	if m.panOffset >= back {
		t.Fatalf("pan_right should decrease panOffset: %d -> %d", back, m.panOffset)
	}

	// → at live edge is a no-op (can't go negative).
	for i := 0; i < 10; i++ {
		m.handleKey(keyRight())
	}
	if m.panOffset != 0 {
		t.Fatalf("pan past live should clamp to 0, got %d", m.panOffset)
	}
}

func TestPanClampsAtHistoryDepth(t *testing.T) {
	d, _, _, agg := testDeps(t)
	feedHistory(t, agg, 10) // 9 closed bars + forming → depth 9
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	depth := agg.HistoryDepth(bars.TF1m)

	// Pan way back; offset should never exceed depth.
	for i := 0; i < 20; i++ {
		m.handleKey(keyLeft())
	}
	if m.panOffset > depth {
		t.Fatalf("panOffset %d exceeded depth %d", m.panOffset, depth)
	}
	if m.panOffset != depth {
		t.Fatalf("panOffset should clamp at depth %d, got %d", depth, m.panOffset)
	}
}

func TestCycleTFBackward(t *testing.T) {
	d, _, _, _ := testDeps(t)
	m := NewModel(d)
	start := m.tfIdx

	// Shift+Tab cycles backward.
	m.handleKey(keyShiftTab())
	if m.tfIdx == start {
		t.Fatal("cycle_tf_back should change tfIdx")
	}
	// And it should be exactly one step back (mod len).
	want := (start - 1 + len(m.tfs)) % len(m.tfs)
	if m.tfIdx != want {
		t.Fatalf("tfIdx = %d, want %d", m.tfIdx, want)
	}
}

func TestPanResetsOnSymbolSwitch(t *testing.T) {
	d, _, _, agg := testDeps(t)
	feedHistory(t, agg, 20)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.handleKey(keyLeft())
	if m.panOffset == 0 {
		t.Fatal("setup: expected non-zero panOffset")
	}

	// Switch symbol via cmdline; the success path resets panOffset.
	m.d.SwitchSymbol = func(s string) error { return nil }
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
	if m.panOffset != 0 {
		t.Fatalf("panOffset after symbol switch = %d, want 0", m.panOffset)
	}
}

func TestPanResetsOnResolutionChange(t *testing.T) {
	d, _, _, agg := testDeps(t)
	feedHistory(t, agg, 20)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.handleKey(keyLeft())
	if m.panOffset == 0 {
		t.Fatal("setup: expected non-zero panOffset")
	}

	// Tab (cycle_tf) resets panOffset.
	m.handleKey(keyLeft())
	beforeTab := m.panOffset
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.panOffset != 0 {
		t.Fatalf("panOffset after cycle_tf = %d (was %d), want 0", m.panOffset, beforeTab)
	}

	// Pan again, then :tf should also reset.
	m.handleKey(keyLeft())
	m.handleKey(key(':'))
	for _, r := range "tf 5m" {
		if r == ' ' {
			m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
		} else {
			m.handleKey(key(r))
		}
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.panOffset != 0 {
		t.Fatalf("panOffset after :tf = %d, want 0", m.panOffset)
	}
}

func TestViewRendersWhilePanned(t *testing.T) {
	d, _, _, agg := testDeps(t)
	feedHistory(t, agg, 40)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	// Pan back several times and ensure View doesn't panic and still
	// produces output containing the symbol.
	for i := 0; i < 5; i++ {
		m.handleKey(keyLeft())
	}
	out := m.View()
	if out == "" {
		t.Fatal("empty view while panned")
	}
	if !strings.Contains(out, "AAPL") {
		t.Fatal("view should contain symbol while panned")
	}
	// The status bar must flag that we're viewing history.
	if !strings.Contains(out, "HISTORY") {
		t.Fatal("status bar should show HISTORY marker when panned back")
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
	// Info strip is above the chart (POS / eq / price present).
	for _, want := range []string{"AAPL", "PAPER", "POS", "eq", "150"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q", want)
		}
	}
	// Richer POS should surface uPL% when positioned.
	if !strings.Contains(out, "%") && !strings.Contains(out, "flat") {
		// With a long position and avg entry, expect a percent token.
		t.Fatalf("view missing uPL%% for open position:\n%s", out)
	}
	// Narrow terminal still renders without panicking.
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
	snap := agg.Snapshot(bars.TF1m, 40, 0)
	lines := renderCandles(snap, 40, 12, ChartOpts{ShowEMA: true, ShowEMA2: true, ShowVWAP: true})
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
