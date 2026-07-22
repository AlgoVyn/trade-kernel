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
func (f *fakeExec) CancelSymbol(_ context.Context, sym string) (int, error) {
	f.calls = append(f.calls, "cancelsym "+sym)
	return 0, nil
}

func testDeps(t *testing.T) (Deps, *fakeExec, *state.Store, *bars.Aggregator) {
	t.Helper()
	cfg := &config.Config{
		SizePresets: []int{100, 250},
		Chart:       config.Chart{Timeframe: "1m"}, // BarsVisible 0 = fill width
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

// TestOrderBlockedBeforeReconcile: a fresh store with no Reconcile must block
// the buy hotkey (a failed startup snapshot must not let the operator trade
// against an empty view). After Reconcile lands, the hotkey goes through.
// Flatten still works either way (tested separately).
func TestOrderBlockedBeforeReconcile(t *testing.T) {
	d, fx, st, _ := testDeps(t)
	// Replace the reconciled store with a fresh, unreconciled one.
	fresh := state.NewStore()
	d.Store = fresh
	d.Risk = risk.NewChecker(risk.Limits{MaxOrderQty: 1000, MaxPositionQty: 5000}, fresh, nil)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	if fresh.Reconciled() {
		t.Fatal("setup: store must be unreconciled")
	}

	// B is blocked.
	_, cmd := m.handleKey(key('B'))
	if cmd != nil {
		drain(m, cmd)
	}
	if len(fx.calls) != 0 {
		t.Fatalf("order must be blocked before reconcile, calls = %v", fx.calls)
	}
	if !m.statusErr || !strings.Contains(m.status, "reconcile pending") {
		t.Fatalf("status = %q err=%v, want reconcile-pending error", m.status, m.statusErr)
	}

	// First successful reconcile flips the gate; B now goes through.
	fresh.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		nil, nil,
	)
	_ = st
	_, cmd = m.handleKey(key('B'))
	drain(m, cmd)
	if len(fx.calls) != 1 || fx.calls[0] != "buy AAPL" {
		t.Fatalf("after reconcile, calls = %v, want buy AAPL", fx.calls)
	}
}

// slowExec delays Buy so keypress→ack latency includes the wait.
type slowExec struct {
	fakeExec
	delay time.Duration
}

func (s *slowExec) Buy(ctx context.Context, sym string, qty int) (alpaca.Order, error) {
	time.Sleep(s.delay)
	return s.fakeExec.Buy(ctx, sym, qty)
}

// TestOrderLatencyIncludesBrokerWait checks that recorded latency includes
// the broker RTT (slowExec sleep). Combined with the pre-Cmd sleep below, it
// also pins the design invariant that the timer starts at submit commit
// (outside the bubbletea Cmd), not only inside the Cmd body.
func TestOrderLatencyIncludesBrokerWait(t *testing.T) {
	d, _, _, _ := testDeps(t)
	const brokerWait = 25 * time.Millisecond
	const dispatchDelay = 30 * time.Millisecond
	sx := &slowExec{delay: brokerWait}
	d.Exec = sx
	m := NewModel(d)
	_, cmd := m.handleKey(key('B'))
	if cmd == nil {
		t.Fatal("expected submit cmd")
	}
	// Simulate bubbletea scheduling delay between Cmd construction and run.
	// If start were inside the Cmd, latency would exclude this sleep.
	time.Sleep(dispatchDelay)
	msg := cmd()
	orm, ok := msg.(orderResultMsg)
	if !ok {
		t.Fatalf("msg type %T", msg)
	}
	if orm.err != nil {
		t.Fatalf("order err: %v", orm.err)
	}
	// Must cover both pre-Cmd dispatch delay and broker wait.
	minWant := dispatchDelay + brokerWait - 5*time.Millisecond
	if orm.latency < minWant {
		t.Fatalf("latency %v < %v (timer must start at submit commit, before Cmd)", orm.latency, minWant)
	}
	// Record into tracker the same way Update does.
	m.d.Latency.Record(orm.latency)
	p50, _ := m.d.Latency.Percentiles()
	if p50 < minWant {
		t.Fatalf("tracker p50 = %v, want >= %v", p50, minWant)
	}
}

func TestTickForAdaptive(t *testing.T) {
	base := 50 * time.Millisecond
	// Short TFs speed up from default base (50→33).
	if g := TickFor(bars.TF1s, 0, session.Regular, base); g != 33*time.Millisecond {
		t.Fatalf("1s live: %v", g)
	}
	// Aggressive configs are honored (not forced up to 33ms).
	if g := TickFor(bars.TF1s, 0, session.Regular, 20*time.Millisecond); g != 20*time.Millisecond {
		t.Fatalf("1s aggressive: %v", g)
	}
	if g := TickFor(bars.TF1s, 0, session.Regular, 16*time.Millisecond); g != 16*time.Millisecond {
		t.Fatalf("1s min config: %v", g)
	}
	// Mid TFs keep base.
	if g := TickFor(bars.TF1m, 0, session.Regular, base); g != base {
		t.Fatalf("1m live: %v", g)
	}
	// High TFs / pan / closed slow to ≥100ms.
	if g := TickFor(bars.TF1h, 0, session.Regular, base); g != 100*time.Millisecond {
		t.Fatalf("1h live: %v", g)
	}
	if g := TickFor(bars.TF1m, 5, session.Regular, base); g != 100*time.Millisecond {
		t.Fatalf("panned: %v", g)
	}
	if g := TickFor(bars.TF1s, 0, session.Closed, base); g != 100*time.Millisecond {
		t.Fatalf("closed: %v", g)
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

func TestCancelActiveSymbolOnly(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	m := NewModel(d)
	_, cmd := m.handleKey(key('C'))
	drain(m, cmd)
	if len(fx.calls) != 1 || fx.calls[0] != "cancelsym AAPL" {
		t.Fatalf("cancel: calls = %v, want [cancelsym AAPL]", fx.calls)
	}
}

// TestChartWidthFillsScreenWhenBarsVisibleZero: omit/0 means paint full width
// (no blank left gutter from an artificial cap). A positive bars_visible still
// caps below the width-fit count.
func TestChartWidthFillsScreenWhenBarsVisibleZero(t *testing.T) {
	d, _, _, _ := testDeps(t)
	// Wide terminal: plotW = 300-10 = 290 → maxBars = (289)/2+1 = 145.
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 300, Height: 40})
	if d.Cfg.Chart.BarsVisible != 0 {
		t.Fatalf("test deps BarsVisible = %d, want 0", d.Cfg.Chart.BarsVisible)
	}
	wantFill := maxBars(300 - priceAxisWidth)
	if got := m.chartWidth(); got != wantFill {
		t.Fatalf("chartWidth fill = %d, want %d (full width)", got, wantFill)
	}
	d.Cfg.Chart.BarsVisible = 80
	if got := m.chartWidth(); got != 80 {
		t.Fatalf("chartWidth capped = %d, want 80", got)
	}
	// Cap above width-fit does not shrink further.
	d.Cfg.Chart.BarsVisible = 10_000
	if got := m.chartWidth(); got != wantFill {
		t.Fatalf("chartWidth high cap = %d, want width-fit %d", got, wantFill)
	}
}

// TestInfoBarShowsDayWeekPnL renders fill-based realized day/wk.
func TestInfoBarShowsDayWeekPnL(t *testing.T) {
	d, _, st, _ := testDeps(t)
	st.Reconcile(
		alpaca.Account{Equity: 101500, LastEquity: 100000, Cash: 50000, BuyingPower: 200000},
		[]alpaca.Position{{
			Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 150,
			UnrealizedIntradayPL: 200, UnrealizedPL: 500,
		}},
		nil,
	)
	st.SetRealizedPnL(1300, 1000)
	m := NewModel(d)
	m.width = 160
	row1, _ := m.buildInfoBarRows(160, st.Snapshot(), bars.MarketSnapshot{})
	if !strings.Contains(row1, "rday") || !strings.Contains(row1, "+1.3k") {
		t.Fatalf("row1 missing realized day PnL: %q", row1)
	}
	if !strings.Contains(row1, "rwk") || !strings.Contains(row1, "+1.0k") {
		t.Fatalf("row1 missing realized week PnL: %q", row1)
	}
	if strings.Contains(row1, "rday*") || strings.Contains(row1, "rwk*") {
		t.Fatalf("full sample must not show partial marker: %q", row1)
	}
}

// TestInfoBarShowsPartialRealizedMarker marks undercount samples with *.
func TestInfoBarShowsPartialRealizedMarker(t *testing.T) {
	d, _, st, _ := testDeps(t)
	st.Reconcile(alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000}, nil, nil)
	st.SetRealizedPnLSample(500, 500, []string{"ZZZ"})
	m := NewModel(d)
	m.width = 160
	row1, _ := m.buildInfoBarRows(160, st.Snapshot(), bars.MarketSnapshot{})
	if !strings.Contains(row1, "rday*") || !strings.Contains(row1, "rwk*") {
		t.Fatalf("partial sample must mark rday*/rwk*: %q", row1)
	}
}

// TestChartOrdersForFiltersSymbolAndMarket keeps priced orders for the
// active symbol (limits + stops); market/unpriced are skipped.
// stop_limit yields both trigger and limit markers.
func TestChartOrdersForFiltersSymbolAndMarket(t *testing.T) {
	orders := []alpaca.Order{
		{ID: "1", Symbol: "AAPL", Side: "buy", Type: "limit", LimitPrice: 150},
		{ID: "2", Symbol: "AAPL", Side: "sell", Type: "market"}, // no price
		{ID: "3", Symbol: "NVDA", Side: "buy", Type: "limit", LimitPrice: 900},
		{ID: "4", Symbol: "AAPL", Side: "sell", Type: "limit", LimitPrice: 160},
		{ID: "5", Symbol: "AAPL", Side: "sell", Type: "stop", StopPrice: 145},
		{ID: "6", Symbol: "AAPL", Side: "buy", Type: "stop_limit", StopPrice: 148, LimitPrice: 149},
	}
	got := chartOrdersFor("AAPL", orders)
	if len(got) != 5 {
		t.Fatalf("got %d markers, want 5 (AAPL limit+stop+stop_limit×2): %+v", len(got), got)
	}
	if got[0].Side != "buy" || got[0].Price != 150 {
		t.Fatalf("first = %+v, want buy@150", got[0])
	}
	if got[1].Side != "sell" || got[1].Price != 160 {
		t.Fatalf("second = %+v, want sell@160", got[1])
	}
	if got[2].Side != "sell" || got[2].Price != 145 {
		t.Fatalf("third = %+v, want sell stop@145", got[2])
	}
	if got[3].Side != "buy" || got[3].Price != 148 {
		t.Fatalf("fourth = %+v, want buy stop@148 (trigger)", got[3])
	}
	if got[4].Side != "buy" || got[4].Price != 149 {
		t.Fatalf("fifth = %+v, want buy limit@149 (stop_limit leg)", got[4])
	}
}

// TestViewRendersOrderMarkers puts buy/sell glyphs on the chart for open limits.
func TestViewRendersOrderMarkers(t *testing.T) {
	d, _, st, agg := testDeps(t)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		agg.OnTrade("AAPL", 150+float64(i%5), 10, base.Add(time.Duration(i)*time.Minute))
	}
	st.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		nil,
		[]alpaca.Order{
			{ID: "1", Symbol: "AAPL", Side: "buy", Type: "limit", LimitPrice: 149, Status: "open"},
			{ID: "2", Symbol: "AAPL", Side: "sell", Type: "limit", LimitPrice: 154, Status: "open"},
			{ID: "3", Symbol: "NVDA", Side: "buy", Type: "limit", LimitPrice: 900, Status: "open"},
		},
	)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	out := stripANSI(m.View())
	if !strings.ContainsRune(out, orderBuyMark) {
		t.Fatalf("view should show buy marker %q, got snippet %q", string(orderBuyMark), truncate(out, 200))
	}
	if !strings.ContainsRune(out, orderSellMark) {
		t.Fatalf("view should show sell marker %q, got snippet %q", string(orderSellMark), truncate(out, 200))
	}
}

// TestInfoBarOrdersActiveSymbolOnly: ORD strip lists active symbol only, but
// the "+N open" badge counts other symbols with positions or resting orders.
func TestInfoBarOrdersActiveSymbolOnly(t *testing.T) {
	d, _, st, _ := testDeps(t)
	st.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		[]alpaca.Position{{Symbol: "AAPL", Qty: 100, Side: "long", AvgEntryPrice: 150}},
		[]alpaca.Order{
			{ID: "1", Symbol: "NVDA", Side: "buy", Qty: 10, Type: "limit", LimitPrice: 900, Status: "open"},
			{ID: "2", Symbol: "AAPL", Side: "sell", Qty: 50, Type: "limit", LimitPrice: 160, Status: "open"},
			{ID: "3", Symbol: "MSFT", Side: "buy", Qty: 5, Type: "market", Status: "open"},
		},
	)
	m := NewModel(d)
	m.width = 120
	if m.infoBarRows() != 2 {
		t.Fatalf("infoBarRows = %d, want 2 (active has open order)", m.infoBarRows())
	}
	if m.openOrderCount("AAPL") != 1 {
		t.Fatalf("openOrderCount AAPL = %d", m.openOrderCount("AAPL"))
	}
	row1, row2 := m.buildInfoBarRows(120, st.Snapshot(), bars.MarketSnapshot{})
	if !strings.Contains(row2, "ORD 1") {
		t.Fatalf("row2 should show ORD 1 for AAPL only, got %q", row2)
	}
	if strings.Contains(row2, "NVDA") || strings.Contains(row2, "MSFT") {
		t.Fatalf("row2 must not list other symbols: %q", row2)
	}
	// NVDA + MSFT have resting orders only → +2 open badge on row1.
	if !strings.Contains(row1, "+2 open") {
		t.Fatalf("row1 should show +2 open for other symbols with orders, got %q", row1)
	}
	// Flat active with only other-symbol orders: still one info row, but +N open.
	m.symbol = "TSLA"
	if m.infoBarRows() != 1 {
		t.Fatalf("infoBarRows for TSLA = %d, want 1 (no active open orders)", m.infoBarRows())
	}
	row1, row2 = m.buildInfoBarRows(120, st.Snapshot(), bars.MarketSnapshot{})
	if row2 != "" {
		t.Fatalf("expected empty order row for TSLA, got %q", row2)
	}
	// AAPL position + NVDA/MSFT orders → 3 other open symbols.
	if !strings.Contains(row1, "+3 open") {
		t.Fatalf("row1 should show +3 open (AAPL pos + NVDA/MSFT orders), got %q", row1)
	}
}

func TestCountOtherOpenSymbolsUnionsPosAndOrders(t *testing.T) {
	st := state.NewStore()
	st.Reconcile(
		alpaca.Account{},
		[]alpaca.Position{{Symbol: "NVDA", Qty: 1, Side: "long"}},
		[]alpaca.Order{
			{ID: "1", Symbol: "MSFT", Side: "buy", Qty: 1, Status: "open"},
			{ID: "2", Symbol: "NVDA", Side: "sell", Qty: 1, Status: "open"}, // same as pos
			{ID: "3", Symbol: "AAPL", Side: "buy", Qty: 1, Status: "open"},  // active — ignored
		},
	)
	if n := countOtherOpenSymbols(st, "AAPL"); n != 2 {
		t.Fatalf("countOtherOpenSymbols = %d, want 2 (NVDA pos+order, MSFT order)", n)
	}
	if n := countOtherOpenSymbols(nil, "AAPL"); n != 0 {
		t.Fatalf("nil store = %d, want 0", n)
	}
}

func TestPanicRemindsOtherOpenOrders(t *testing.T) {
	d, fx, st, _ := testDeps(t)
	// Flat AAPL, resting order on NVDA only — must still remind after panic.
	st.Reconcile(
		alpaca.Account{Equity: 100000, Cash: 50000, BuyingPower: 200000},
		nil,
		[]alpaca.Order{{ID: "1", Symbol: "NVDA", Side: "buy", Qty: 10, Type: "limit", LimitPrice: 900, Status: "open"}},
	)
	m := NewModel(d)
	_, cmd := m.handleKey(key('X'))
	if cmd == nil {
		t.Fatal("expected panic cmd")
	}
	msg := cmd()
	orm, ok := msg.(orderResultMsg)
	if !ok {
		t.Fatalf("msg type %T", msg)
	}
	if orm.err != nil {
		t.Fatalf("panic err: %v", orm.err)
	}
	if !strings.Contains(orm.detail, "1 other symbol") {
		t.Fatalf("detail should remind about other open orders, got %q", orm.detail)
	}
	if len(fx.calls) != 1 || fx.calls[0] != "cancelsym AAPL" {
		t.Fatalf("calls = %v, want cancelsym only (already flat)", fx.calls)
	}
}

func TestPanicAllLegacyAlias(t *testing.T) {
	d, fx, _, _ := testDeps(t)
	m := NewModel(d)
	// Simulate config override mapping a key to panic_all.
	m.keyFor["ctrl+x"] = "panic_all"
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlX})
	drain(m, cmd)
	if len(fx.calls) != 2 || fx.calls[0] != "cancelsym AAPL" || fx.calls[1] != "flatten AAPL" {
		t.Fatalf("panic_all alias should run symbol panic: %v", fx.calls)
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
// only the currently selected symbol — other positions remain untouched.
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
	for _, c := range fx.calls {
		if strings.Contains(c, "NVDA") || c == "cancelall" {
			t.Fatalf("panic must not touch other symbols or cancel-all: %v", fx.calls)
		}
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

// TestCustomTFCommand sets :tf 2m and clears it when Tab cycles built-ins.
func TestCustomTFCommand(t *testing.T) {
	d, _, _, agg := testDeps(t)
	feedHistory(t, agg, 20)
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	m.handleKey(key(':'))
	for _, r := range "tf 2m" {
		if r == ' ' {
			m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
		} else {
			m.handleKey(key(r))
		}
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.customName != "2m" || m.customDur != 2*time.Minute {
		t.Fatalf("custom = %q/%v, want 2m", m.customName, m.customDur)
	}
	if name, ok := agg.CustomActive(); !ok || name != "2m" {
		t.Fatalf("agg custom = %q ok=%v", name, ok)
	}
	if m.chartTFName() != "2m" {
		t.Fatalf("chartTFName = %q", m.chartTFName())
	}
	// View should render without panic.
	if out := m.View(); out == "" {
		t.Fatal("empty view on custom TF")
	}
	// Tab leaves custom and returns to the previous built-in (no advance).
	prevIdx := m.tfIdx
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	if m.customName != "" {
		t.Fatalf("custom should clear on cycle_tf, got %q", m.customName)
	}
	if _, ok := agg.CustomActive(); ok {
		t.Fatal("agg custom should be cleared after Tab")
	}
	if m.tfIdx != prevIdx {
		t.Fatalf("first Tab from custom should keep tfIdx=%d, got %d", prevIdx, m.tfIdx)
	}
	// Second Tab advances the built-in cycle.
	m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	want := (prevIdx + 1) % len(m.tfs)
	if m.tfIdx != want {
		t.Fatalf("second Tab tfIdx=%d, want %d", m.tfIdx, want)
	}
}

// TestConfigCustomTimeframe enables custom TF from chart.timeframe at startup.
func TestConfigCustomTimeframe(t *testing.T) {
	d, _, _, agg := testDeps(t)
	d.Cfg.Chart.Timeframe = "3m"
	m := NewModel(d)
	if m.customName != "3m" || m.customDur != 3*time.Minute {
		t.Fatalf("custom from config = %q/%v", m.customName, m.customDur)
	}
	if name, ok := agg.CustomActive(); !ok || name != "3m" {
		t.Fatalf("agg custom = %q ok=%v", name, ok)
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
