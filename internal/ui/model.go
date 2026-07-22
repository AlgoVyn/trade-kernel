// Package ui implements the bubbletea TUI: solid block candlestick chart with
// braille indicator overlays, position/order panel, status bar, hotkeys and
// the ':' command line.
package ui

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/bars"
	"trade-kernel/internal/cmdline"
	"trade-kernel/internal/config"
	"trade-kernel/internal/execution"
	"trade-kernel/internal/risk"
	"trade-kernel/internal/session"
	"trade-kernel/internal/state"
)

// Deps are the UI's connections to the rest of the system. All are
// read-pull: the render loop snapshots state on a ticker and never
// touches the ingest hot path.
type Deps struct {
	Cfg      *config.Config
	Agg      *bars.Aggregator
	Store    *state.Store
	Exec     execution.Executor
	Risk     *risk.Checker
	Sessions *session.Engine
	Builder  *execution.Builder
	Latency  *LatencyTracker
	// SwitchSymbol resubscribes market data and backfills bars. It may
	// block briefly; the UI calls it from a command goroutine.
	SwitchSymbol func(symbol string) error
	Paper        bool
}

type tickMsg time.Time

type orderResultMsg struct {
	label   string
	detail  string
	err     error
	latency time.Duration
}

type symbolSwitchedMsg struct {
	symbol string
	err    error
}

type pendingAction struct {
	label string
	run   func(ctx context.Context) (alpaca.Order, error)
	// record is called when submission is committed (start of execAsync) so
	// in-flight duplicates are blocked during broker RTT. Declined confirm
	// never reaches execAsync and does not burn the debounce window.
	record func()
}

// Model is the bubbletea root model.
type Model struct {
	d Deps

	symbol string
	tfs    []bars.TF
	tfIdx  int
	// customName / customDur select a non-built-in chart resolution
	// (e.g. 2m via config or :tf). Empty name means use tfs[tfIdx].
	customName string
	customDur  time.Duration
	preset     int
	keyFor     map[string]string // key string -> action
	width      int
	height     int

	// panOffset is how many bars the chart view is shifted back from the
	// live edge. 0 = live (forming bar at the right edge); >0 = viewing
	// history. Reset to 0 on symbol/resolution switch.
	panOffset int

	// leftCrop trims bars from the left edge of the chart window while
	// keeping the right edge at live (or at panOffset). It rebases the
	// volume/price scale onto the recent window — useful when a new
	// low-volume session starts and the prior session's peaks would
	// otherwise squash it flat. 0 = no crop (full width). Reset to 0
	// wherever panOffset is reset (symbol/resolution switch).
	leftCrop int

	cmdActive bool
	cmdBuf    string

	pending  *pendingAction
	quitting bool
	confirmQ bool // quit confirmation

	status    string
	statusErr bool
	statusAt  time.Time

	showEMA  bool
	showEMA2 bool
	showVWAP bool
	shading  bool

	sess session.Session
	done bool
}

// NewModel builds the root model.
func NewModel(d Deps) *Model {
	m := &Model{
		d:        d,
		symbol:   d.Cfg.DefaultSymbol,
		tfs:      bars.ChartTFs(),
		keyFor:   defaultKeys(),
		showEMA:  true,
		showEMA2: true,
		showVWAP: true,
		shading:  d.Cfg.Chart.SessionShading,
		sess:     session.Closed,
	}
	// Apply keybinding overrides: config maps action -> key.
	for action, key := range d.Cfg.Keys {
		for k, a := range m.keyFor {
			if a == action {
				delete(m.keyFor, k)
			}
		}
		m.keyFor[key] = action
	}
	if spec, ok := bars.ParseChartTF(d.Cfg.Chart.Timeframe); ok {
		if spec.IsCustom() {
			m.customName = spec.Name
			m.customDur = spec.Custom
			if d.Agg != nil {
				d.Agg.EnableCustom(spec.Custom, spec.Name)
			}
		} else {
			for i, t := range m.tfs {
				if t == spec.Builtin {
					m.tfIdx = i
					break
				}
			}
		}
	}
	if d.Sessions != nil {
		m.sess = d.Sessions.Current()
	}
	return m
}

func defaultKeys() map[string]string {
	return map[string]string{
		"B":         "buy",
		"S":         "sell",
		"A":         "add",
		"D":         "reduce",
		"F":         "flatten",
		"C":         "cancel",
		"X":         "panic",
		":":         "cmdline",
		"tab":       "cycle_tf",
		"shift+tab": "cycle_tf_back",
		"left":      "pan_left",
		"right":     "pan_right",
		"[":         "crop_in",
		"]":         "crop_out",
		"i":         "cycle_indicators",
		"q":         "quit",
		"ctrl+c":    "quit_force",
	}
}

// Init starts the render ticker.
func (m *Model) Init() tea.Cmd {
	return m.tick()
}

// tickInterval returns the render period for the current view state.
// Adaptive: short TFs refresh faster (down to 33ms when base is higher);
// panned history and closed sessions slow down to save CPU. Base comes
// from config chart.tick_ms (default 50).
func (m *Model) tickInterval() time.Duration {
	base := 50 * time.Millisecond
	if m.d.Cfg != nil {
		base = m.d.Cfg.Tick()
	}
	return TickForDuration(m.chartBarDuration(), m.panOffset, m.sess, base)
}

// usingCustomTF reports whether the chart is on a non-built-in resolution.
func (m *Model) usingCustomTF() bool {
	return m.customName != "" && m.customDur > 0
}

// chartBarDuration is the active resolution length for tick heuristics.
func (m *Model) chartBarDuration() time.Duration {
	if m.usingCustomTF() {
		return m.customDur
	}
	return m.tfs[m.tfIdx].Duration()
}

// chartTFName is the status-bar / ruler resolution label.
func (m *Model) chartTFName() string {
	if m.usingCustomTF() {
		return m.customName
	}
	return m.tfs[m.tfIdx].String()
}

// chartSnapshot pulls bars for the active built-in or custom resolution.
func (m *Model) chartSnapshot(n, offset int) bars.Snapshot {
	if m.d.Agg == nil {
		return bars.Snapshot{}
	}
	if m.usingCustomTF() {
		return m.d.Agg.SnapshotCustom(n, offset)
	}
	return m.d.Agg.Snapshot(m.tfs[m.tfIdx], n, offset)
}

// chartHistoryDepth is pan-clamp depth for the active resolution.
func (m *Model) chartHistoryDepth() int {
	if m.d.Agg == nil {
		return 0
	}
	if m.usingCustomTF() {
		return m.d.Agg.HistoryDepthCustom()
	}
	return m.d.Agg.HistoryDepth(m.tfs[m.tfIdx])
}

// clearCustomTF leaves a custom resolution and disables the aggregator series.
func (m *Model) clearCustomTF() {
	if !m.usingCustomTF() {
		return
	}
	m.customName = ""
	m.customDur = 0
	if m.d.Agg != nil {
		m.d.Agg.ClearCustom()
	}
}

// TickFor chooses a render interval from TF, pan, session, and configured base.
// Exported for unit tests.
//
//	short TFs (1s/5s/15s): min(base, 33ms) — faster than a default 50ms base;
//	  aggressive configs (e.g. 16ms) are honored, not forced up to 33ms.
//	high TFs (1h/1d), pan, closed: max(base, 100ms) — slower redraw is fine.
//	mid TFs (1m etc.): base unchanged.
func TickFor(tf bars.TF, panOffset int, sess session.Session, base time.Duration) time.Duration {
	return TickForDuration(tf.Duration(), panOffset, sess, base)
}

// TickForDuration is the duration-based form of TickFor (custom TFs use this).
func TickForDuration(barLen time.Duration, panOffset int, sess session.Session, base time.Duration) time.Duration {
	if base <= 0 {
		base = 50 * time.Millisecond
	}
	// History pan or closed market: slower redraw is fine.
	if panOffset > 0 || sess == session.Closed {
		if base < 100*time.Millisecond {
			return 100 * time.Millisecond
		}
		return base
	}
	switch {
	case barLen > 0 && barLen < time.Minute:
		// Speed up from a slower base (50→33); keep aggressive configs as-is.
		if base > 33*time.Millisecond {
			return 33 * time.Millisecond
		}
		return base
	case barLen >= time.Hour:
		if base < 100*time.Millisecond {
			return 100 * time.Millisecond
		}
		return base
	default:
		return base
	}
}

func (m *Model) tick() tea.Cmd {
	d := m.tickInterval()
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		if m.d.Sessions != nil {
			m.sess = m.d.Sessions.Current()
		}
		if m.done {
			return m, tea.Quit
		}
		return m, m.tick()

	case orderResultMsg:
		if m.d.Latency != nil {
			m.d.Latency.Record(msg.latency)
		}
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("%s FAILED: %v", msg.label, msg.err), true)
		} else {
			m.setStatus(fmt.Sprintf("%s ✓ %s (%s)", msg.label, msg.detail, msg.latency.Round(time.Millisecond)), false)
		}
		return m, nil

	case symbolSwitchedMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("switch %s: %v", msg.symbol, msg.err), true)
		} else {
			m.symbol = msg.symbol
			m.panOffset = 0
			m.leftCrop = 0
			m.setStatus("symbol → "+msg.symbol, false)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Pending confirmation consumes y/n/esc.
	if m.pending != nil {
		switch key {
		case "y", "Y", "enter":
			p := m.pending
			m.pending = nil
			// Debounce is recorded when execAsync commits the submit.
			return m, m.execAsync(p)
		default:
			m.pending = nil
			m.setStatus("order aborted", false)
			return m, nil
		}
	}
	if m.confirmQ {
		switch key {
		case "y", "Y", "enter":
			m.confirmQ = false
			m.done = true
			return m, nil
		default:
			m.confirmQ = false
			m.setStatus("quit cancelled", false)
			return m, nil
		}
	}

	// Command-line mode.
	if m.cmdActive {
		switch msg.Type {
		case tea.KeyEnter:
			input := m.cmdBuf
			m.cmdActive = false
			m.cmdBuf = ""
			return m, m.runCommand(input)
		case tea.KeyEsc:
			m.cmdActive = false
			m.cmdBuf = ""
			return m, nil
		case tea.KeyBackspace:
			if len(m.cmdBuf) > 0 {
				m.cmdBuf = m.cmdBuf[:len(m.cmdBuf)-1]
			}
			return m, nil
		case tea.KeySpace:
			m.cmdBuf += " "
			return m, nil
		case tea.KeyRunes:
			m.cmdBuf += string(msg.Runes)
			return m, nil
		}
		return m, nil
	}

	// Preset selection 1-9.
	if len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		n := int(key[0] - '1')
		if n < len(m.d.Cfg.SizePresets) {
			m.preset = n
			m.setStatus(fmt.Sprintf("preset %d → size %d", n+1, m.d.Cfg.SizePresets[n]), false)
		}
		return m, nil
	}

	action, ok := m.keyFor[key]
	if !ok {
		return m, nil
	}
	switch action {
	case "buy":
		return m, m.orderIntent("buy", m.qty(), 0, false)
	case "sell":
		return m, m.orderIntent("sell", m.qty(), 0, false)
	case "add":
		return m, m.addReduce(true)
	case "reduce":
		return m, m.addReduce(false)
	case "flatten":
		return m, m.flattenIntent(false)
	case "cancel", "cancel_all": // cancel_all is a legacy alias
		return m, m.cancelIntent()
	case "panic", "panic_all": // panic_all is a legacy alias (now symbol-scoped)
		return m, m.panicIntent()
	case "cmdline":
		m.cmdActive = true
		m.cmdBuf = ""
		return m, nil
	case "cycle_tf":
		// Leaving a custom TF returns to the last built-in (tfIdx unchanged).
		// A second Tab then advances the built-in cycle.
		if m.usingCustomTF() {
			m.clearCustomTF()
			m.panOffset = 0
			m.leftCrop = 0
			return m, nil
		}
		m.tfIdx = (m.tfIdx + 1) % len(m.tfs)
		m.panOffset = 0
		m.leftCrop = 0
		return m, nil
	case "cycle_tf_back":
		if m.usingCustomTF() {
			m.clearCustomTF()
			m.panOffset = 0
			m.leftCrop = 0
			return m, nil
		}
		m.tfIdx = (m.tfIdx - 1 + len(m.tfs)) % len(m.tfs)
		m.panOffset = 0
		m.leftCrop = 0
		return m, nil
	case "pan_left":
		m.pan(-1)
		return m, nil
	case "pan_right":
		m.pan(1)
		return m, nil
	case "crop_in": // [ — narrow window toward the live edge
		m.crop(1)
		return m, nil
	case "crop_out": // ] — widen back to full width
		m.crop(-1)
		return m, nil
	case "cycle_indicators":
		m.cycleIndicators()
		return m, nil
	case "quit":
		if m.d.Cfg.ConfirmOrders && m.d.Store.PositionQty(m.symbol) != 0 {
			m.confirmQ = true
			return m, nil
		}
		m.done = true
		return m, nil
	case "quit_force":
		m.done = true
		return m, nil
	}
	return m, nil
}

func (m *Model) cycleIndicators() {
	// Cycle: all → EMA only → EMA2 only → VWAP only → none → all.
	switch {
	case m.showEMA && m.showEMA2 && m.showVWAP:
		m.showEMA2, m.showVWAP = false, false
	case m.showEMA && !m.showEMA2 && !m.showVWAP:
		m.showEMA, m.showEMA2 = false, true
	case m.showEMA2 && !m.showEMA && !m.showVWAP:
		m.showEMA2, m.showVWAP = false, true
	case m.showVWAP && !m.showEMA && !m.showEMA2:
		m.showVWAP = false
	default:
		m.showEMA, m.showEMA2, m.showVWAP = true, true, true
	}
}

// chartWidth returns how many bars fit in the candle plot (excluding the
// right-side price axis gutter). Accounts for barStride spacing. Mirrors
// the layout math in View() so pan/focus steps match what is painted.
func (m *Model) chartWidth() int {
	axisW := priceAxisWidth
	// Drop the price axis when the window is too narrow to host it.
	if m.width <= axisW+20 {
		axisW = 0
	}
	w := m.width - axisW
	if w < 1 {
		w = 1
	}
	return maxBars(w)
}

// pan shifts the chart view through history. dir < 0 pans back to older
// bars (←), dir > 0 pans forward toward live (→). Each press jumps a
// quarter of the chart width so scrolling feels responsive.
func (m *Model) pan(dir int) {
	depth := m.chartHistoryDepth()
	if depth <= 0 {
		return
	}
	// offset=0 is live (forming bar); offset=depth shows the oldest
	// retained closed bar. Snapshot clamps anything beyond that, but we
	// clamp here too so the status-bar counter stays truthful.
	maxBack := depth
	step := m.chartWidth() / 4
	if step < 1 {
		step = 1
	}
	switch {
	case dir < 0:
		m.panOffset += step
		if m.panOffset > maxBack {
			m.panOffset = maxBack
		}
		m.setStatus(fmt.Sprintf("◀ back %d bars", m.panOffset), false)
	default: // dir > 0: forward toward live
		if m.panOffset == 0 {
			return
		}
		m.panOffset -= step
		if m.panOffset < 0 {
			m.panOffset = 0
		}
		if m.panOffset == 0 {
			m.setStatus("back to live", false)
		} else {
			m.setStatus(fmt.Sprintf("▶ forward, now −%d", m.panOffset), false)
		}
	}
}

// crop narrows (dir > 0) or widens (dir < 0) the left edge of the chart
// window, keeping the right edge pinned at the live edge (or at panOffset).
// Each press jumps a quarter of the chart width. Dropping older bars lets
// volume/price scale rebase onto the recent window — useful when a new
// low-volume session starts and the prior session's peaks squash it flat.
// Capped so at least one bar stays visible.
func (m *Model) crop(dir int) {
	cap := m.chartWidth() - 1
	if cap < 0 {
		cap = 0
	}
	step := m.chartWidth() / 4
	if step < 1 {
		step = 1
	}
	switch {
	case dir > 0: // narrow: drop older bars from the left
		m.leftCrop += step
		if m.leftCrop > cap {
			m.leftCrop = cap
		}
		m.setStatus(fmt.Sprintf("▶ focus −%d", m.leftCrop), false)
	default: // dir < 0: widen toward full width
		if m.leftCrop == 0 {
			return
		}
		m.leftCrop -= step
		if m.leftCrop < 0 {
			m.leftCrop = 0
		}
		if m.leftCrop == 0 {
			m.setStatus("focus off", false)
		} else {
			m.setStatus(fmt.Sprintf("▶ focus −%d", m.leftCrop), false)
		}
	}
}

func (m *Model) qty() int { return m.d.Cfg.SizePresets[m.preset%len(m.d.Cfg.SizePresets)] }

func (m *Model) setStatus(s string, isErr bool) {
	m.status, m.statusErr, m.statusAt = s, isErr, time.Now()
}

// orderIntent runs risk checks and either confirms or submits.
func (m *Model) orderIntent(side string, qty int, limit float64, skipRisk bool) tea.Cmd {
	symbol := m.symbol
	// Block new orders until the first REST reconcile lands so a failed
	// startup snapshot can't let the operator trade against a stale/empty
	// view. Flatten and panic bypass this (see flattenIntent / panicIntent):
	// they read PositionQty live and fall back to DELETE /v2/positions.
	if m.d.Store != nil && !m.d.Store.Reconciled() {
		m.setStatus("reconcile pending — order blocked (flatten/panic still work)", true)
		return nil
	}
	if !skipRisk {
		if err := m.d.Risk.Check(symbol, side, qty); err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
	}
	var run func(ctx context.Context) (alpaca.Order, error)
	label := fmt.Sprintf("%s %d %s", strings.ToUpper(side), qty, symbol)
	if limit > 0 {
		label += fmt.Sprintf(" lmt %.2f", limit)
		if side == "buy" {
			run = func(ctx context.Context) (alpaca.Order, error) { return m.d.Exec.LimitBuy(ctx, symbol, qty, limit) }
		} else {
			run = func(ctx context.Context) (alpaca.Order, error) { return m.d.Exec.LimitSell(ctx, symbol, qty, limit) }
		}
	} else {
		// Show the converted limit in extended sessions.
		if session.Extended(m.sess) && m.d.Builder != nil {
			if px, warn := m.d.Builder.PreviewLimit(side); px > 0 {
				label += fmt.Sprintf(" ~lmt %.2f", px)
				if warn != "" {
					label += " [" + warn + "]"
				}
			}
		}
		if side == "buy" {
			run = func(ctx context.Context) (alpaca.Order, error) { return m.d.Exec.Buy(ctx, symbol, qty) }
		} else {
			run = func(ctx context.Context) (alpaca.Order, error) { return m.d.Exec.Sell(ctx, symbol, qty) }
		}
	}
	// Debounce is recorded when execAsync commits the submit — not at Check
	// time and not when the user declines confirm — so declines do not burn
	// the window, but in-flight duplicates during RTT are blocked.
	p := &pendingAction{
		label: label,
		run:   run,
		record: func() {
			if !skipRisk {
				m.d.Risk.Record(symbol, side, qty)
			}
		},
	}
	if m.d.Cfg.ConfirmOrders {
		m.pending = p
		return nil
	}
	return m.execAsync(p)
}

func (m *Model) addReduce(add bool) tea.Cmd {
	pos := m.d.Store.PositionQty(m.symbol)
	qty := m.qty()
	if add {
		if pos < 0 {
			return m.orderIntent("sell", qty, 0, false)
		}
		return m.orderIntent("buy", qty, 0, false)
	}
	// Reduce.
	if pos == 0 {
		m.setStatus("no position to reduce", true)
		return nil
	}
	if pos > 0 {
		if qty > int(pos) {
			qty = int(pos)
		}
		return m.orderIntent("sell", qty, 0, false)
	}
	if qty > int(-pos) {
		qty = int(-pos)
	}
	return m.orderIntent("buy", qty, 0, false)
}

func (m *Model) flattenIntent(skipRisk bool) tea.Cmd {
	symbol := m.symbol
	pos := m.d.Store.PositionQty(symbol)
	if pos == 0 {
		m.setStatus("no position in "+symbol, true)
		return nil
	}
	qty := int(math.Abs(pos))
	side := "sell"
	if pos < 0 {
		side = "buy"
	}
	// Sub-share residuals (0 < |pos| < 1) have qty==0 for risk sizing but must
	// still flatten via ClosePosition. Only enforce kill-switch for those;
	// whole-share flattens go through the full Check.
	if !skipRisk {
		if qty == 0 {
			if locked, reason := m.d.Risk.Locked(); locked {
				m.setStatus(fmt.Sprintf("kill-switch locked (%s): use :unlock to re-enable", reason), true)
				return nil
			}
		} else if err := m.d.Risk.Check(symbol, side, qty); err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
	}
	label := fmt.Sprintf("FLATTEN %s (%s %s)", symbol, strings.ToUpper(side), formatFlattenQty(pos))
	// Show the converted limit price + any stale-quote warning in extended
	// sessions, mirroring orderIntent.
	if session.Extended(m.sess) && m.d.Builder != nil {
		if px, warn := m.d.Builder.PreviewLimit(side); px > 0 {
			label += fmt.Sprintf(" ~lmt %.2f", px)
			if warn != "" {
				label += " [" + warn + "]"
			}
		}
	}
	p := &pendingAction{
		label: label,
		run:   func(ctx context.Context) (alpaca.Order, error) { return m.d.Exec.Flatten(ctx, symbol, pos) },
		record: func() {
			if !skipRisk && qty > 0 {
				m.d.Risk.Record(symbol, side, qty)
			}
		},
	}
	if m.d.Cfg.ConfirmOrders && !skipRisk {
		m.pending = p
		return nil
	}
	return m.execAsync(p)
}

func formatFlattenQty(pos float64) string {
	abs := math.Abs(pos)
	if abs == float64(int(abs)) {
		return fmt.Sprintf("%d", int(abs))
	}
	return fmt.Sprintf("%.4g", abs)
}

// cancelIntent cancels open orders for the active symbol only.
func (m *Model) cancelIntent() tea.Cmd {
	symbol := m.symbol
	p := &pendingAction{
		label: "CANCEL " + symbol,
		run: func(ctx context.Context) (alpaca.Order, error) {
			failures, err := m.d.Exec.CancelSymbol(ctx, symbol)
			if err != nil {
				if failures > 0 {
					return alpaca.Order{}, fmt.Errorf("%w (%d cancel(s) still failed)", err, failures)
				}
				return alpaca.Order{}, err
			}
			if failures > 0 {
				// All attempted, but some DELETEs failed — surface as an error
				// so the operator knows siblings may still be resting.
				return alpaca.Order{}, fmt.Errorf("%d cancel(s) failed (orders may still be open)", failures)
			}
			return alpaca.Order{}, nil
		},
	}
	return m.execAsync(p)
}

// countOtherOpenSymbols returns how many symbols other than active have a
// non-zero position and/or resting open order. Used for post-panic reminders
// and the info-bar "+N open" badge so multi-name risk is not silent when the
// book is flat but orders remain.
func countOtherOpenSymbols(st *state.Store, symbol string) int {
	if st == nil {
		return 0
	}
	seen := make(map[string]struct{})
	for _, p := range st.Positions() {
		if p.Symbol != "" && p.Symbol != symbol {
			seen[p.Symbol] = struct{}{}
		}
	}
	for _, o := range st.OpenOrders() {
		if o.Symbol != "" && o.Symbol != symbol {
			seen[o.Symbol] = struct{}{}
		}
	}
	return len(seen)
}

// panicIntent: cancel open orders for the active symbol + flatten that
// symbol, bypassing the risk checker and confirmation. Scope is always
// the currently selected symbol only.
func (m *Model) panicIntent() tea.Cmd {
	symbol := m.symbol
	// Same as execAsync: start before the bubbletea Cmd so dispatch delay
	// is included in keypress→ack (status-bar p50/p99).
	start := time.Now()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cancelFailures, cancelErr := m.d.Exec.CancelSymbol(ctx, symbol)
		qty := m.d.Store.PositionQty(symbol)
		otherOpen := countOtherOpenSymbols(m.d.Store, symbol)
		otherNote := ""
		if otherOpen > 0 {
			otherNote = fmt.Sprintf("; %d other symbol(s) still open — :sym then X", otherOpen)
		}
		cancelNote := ""
		if cancelFailures > 0 {
			cancelNote = fmt.Sprintf("; %d cancel(s) failed (orders may be open)", cancelFailures)
		} else if cancelErr != nil {
			cancelNote = "; cancel err: " + cancelErr.Error()
		}
		if qty == 0 {
			detail := "cancelled " + symbol + " orders; already flat" + otherNote + cancelNote
			return orderResultMsg{
				label: "PANIC", detail: detail,
				latency: time.Since(start),
			}
		}
		o, err := m.d.Exec.Flatten(ctx, symbol, qty)
		if err != nil {
			if cancelErr != nil {
				err = fmt.Errorf("%w (also cancel err: %v)", err, cancelErr)
			}
			return orderResultMsg{label: "PANIC", err: err, latency: time.Since(start)}
		}
		id := o.ID
		if id == "" {
			id = "done"
		}
		detail := "flatten " + symbol + " " + id + otherNote + cancelNote
		return orderResultMsg{
			label: "PANIC", detail: detail,
			latency: time.Since(start),
		}
	}
}

func (m *Model) execAsync(p *pendingAction) tea.Cmd {
	// Record debounce as soon as we commit to submit so a second identical
	// hotkey during broker RTT cannot pass Check. Declined confirm never
	// reaches here. Broker rejects still burn the short debounce window.
	if p.record != nil {
		p.record()
	}
	// Keypress→ack (or confirm-accept→ack): start before the bubbletea
	// command goroutine so dispatch delay is included, matching DESIGN.
	start := time.Now()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		o, err := p.run(ctx)
		lat := time.Since(start)
		if err != nil {
			return orderResultMsg{label: p.label, err: err, latency: lat}
		}
		detail := "id " + o.ID
		if o.ID == "" {
			detail = "done"
		}
		return orderResultMsg{label: p.label, detail: detail, latency: lat}
	}
}

// runCommand parses and executes a ':' command.
func (m *Model) runCommand(input string) tea.Cmd {
	c, err := cmdline.Parse(input)
	if err != nil {
		m.setStatus(err.Error(), true)
		return nil
	}
	switch c.Kind {
	case cmdline.KindOrder:
		return m.orderIntent(c.Side, c.Qty, c.Limit, false)
	case cmdline.KindSymbol:
		if c.Symbol == m.symbol {
			return nil
		}
		m.setStatus("switching to "+c.Symbol+"…", false)
		return func() tea.Msg {
			err := m.d.SwitchSymbol(c.Symbol)
			return symbolSwitchedMsg{symbol: c.Symbol, err: err}
		}
	case cmdline.KindTF:
		spec, ok := bars.ParseChartTF(c.TF)
		if !ok {
			m.setStatus("unknown timeframe "+c.TF+" (try 1m, 5m, 2m, 30s, 1h, 1d)", true)
			return nil
		}
		if spec.IsCustom() {
			m.customName = spec.Name
			m.customDur = spec.Custom
			if m.d.Agg != nil {
				m.d.Agg.EnableCustom(spec.Custom, spec.Name)
			}
		} else {
			m.clearCustomTF()
			for i, t := range m.tfs {
				if t == spec.Builtin {
					m.tfIdx = i
					break
				}
			}
		}
		m.panOffset = 0
		m.leftCrop = 0
		m.setStatus("timeframe "+spec.Name, false)
		return nil
	case cmdline.KindPreset:
		if c.Preset < 1 || c.Preset > len(m.d.Cfg.SizePresets) {
			m.setStatus(fmt.Sprintf("preset must be 1-%d", len(m.d.Cfg.SizePresets)), true)
			return nil
		}
		m.preset = c.Preset - 1
		m.setStatus(fmt.Sprintf("preset %d → size %d", c.Preset, m.qty()), false)
		return nil
	case cmdline.KindFlatten:
		return m.flattenIntent(false)
	case cmdline.KindCancel:
		return m.cancelIntent()
	case cmdline.KindLock:
		reason := c.Reason
		if reason == "" {
			reason = "manual"
		}
		m.d.Risk.Lock(reason)
		m.setStatus("kill-switch locked ("+reason+")", true)
		return nil
	case cmdline.KindUnlock:
		m.d.Risk.Unlock()
		m.setStatus("kill-switch unlocked", false)
		return nil
	case cmdline.KindPanic:
		return m.panicIntent()
	case cmdline.KindConfirm:
		m.d.Cfg.ConfirmOrders = c.On
		m.setStatus(fmt.Sprintf("confirm %v", onOff(c.On)), false)
		return nil
	case cmdline.KindShading:
		m.shading = c.On
		return nil
	case cmdline.KindFocus:
		// Clamp to the chart width (same ceiling crop() uses) so an absolute
		// ":focus 9999" can't zero out the view. At least one bar stays.
		cap := m.chartWidth() - 1
		if cap < 0 {
			cap = 0
		}
		if c.Focus > cap {
			c.Focus = cap
		}
		m.leftCrop = c.Focus
		if m.leftCrop > 0 {
			m.setStatus(fmt.Sprintf("focus −%d", m.leftCrop), false)
		} else {
			m.setStatus("focus off", false)
		}
		return nil
	case cmdline.KindQuit:
		m.done = true
		return nil
	case cmdline.KindHelp:
		m.setStatus("B/S buy/sell A/D add/reduce F flatten C cancel(active) X panic(active only) 1-9 preset :tf 1m|2m|30s… tab cycle ←/→ pan [/] focus :focus N|off i ind :lock/:unlock q", false)
		return nil
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// View renders the full screen.
//
// Layout (top → bottom):
//
//	status bar (mode/session/quote + size presets + indicators + tf)
//	info strip (position / account / orders) — full width
//	candles + price axis
//	time ruler
//	volume
//	bottom (confirm / cmdline / status)
//
// The info strip sits above the chart so the candle pane can use the full
// terminal width (minus the price-axis gutter only). Size presets and
// indicator chips live only on the status bar.
func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	// One locked snapshot of the store and one of the market cache per frame.
	// This replaces ~5 separate mutex acquisitions + slice allocations that
	// the status bar, info bar, and chart used to each perform.
	var book state.BookSnapshot
	if m.d.Store != nil {
		book = m.d.Store.Snapshot()
	}
	var market bars.MarketSnapshot
	if m.d.Agg != nil {
		market = m.d.Agg.MarketSnapshot()
	}

	// TradingView-style price scale sits on the right of the candle plot.
	// Reserve it unless the window is too narrow (plot would be unusable).
	axisW := priceAxisWidth
	if m.width <= axisW+20 {
		axisW = 0
	}
	plotW := m.width - axisW
	if plotW < 1 {
		plotW = 1
	}
	statusH, bottomH := 1, 1
	panelH := infoBarRowsFor(book, m.symbol)
	// Keep the info strip when there's room; collapse it on tiny terminals
	// so the chart still fits.
	if m.height < statusH+bottomH+panelH+8 {
		panelH = 0
	}
	avail := m.height - statusH - bottomH - panelH
	if avail < 2 {
		avail = 2
	}
	volH := avail / 5
	if volH < 3 {
		volH = 3
	}
	if volH > 8 {
		volH = 8
	}
	rulerH := 1 // time axis between candles and volume
	candleH := avail - volH - rulerH
	if candleH < 1 {
		candleH = 1
	}

	opts := ChartOpts{
		ShowEMA: m.showEMA, ShowEMA2: m.showEMA2, ShowVWAP: m.showVWAP,
		SessionShading: m.shading,
		Orders:         chartOrdersFor(m.symbol, book.Orders),
	}
	// Request only as many bars as fit at barStride spacing; the renderers
	// still paint into the full plotW-cell canvas with empty spacer columns.
	// leftCrop narrows the window from the left (focus mode): bars cluster at
	// the live edge and the blank left region lets volume/price scale rebase
	// onto the recent window. barCol right-aligns so fewer bars = blank left.
	// chart.bars_visible caps painted bars (Validate defaults omit/≤0 → 120).
	// When the cap is below the width-fit count, the left gutter stays blank.
	nBars := maxBars(plotW) - m.leftCrop
	if m.d.Cfg != nil {
		if cap := m.d.Cfg.Chart.BarsVisible; cap > 0 && nBars > cap {
			nBars = cap
		}
	}
	if nBars < 1 {
		nBars = 1
	}
	snap := m.chartSnapshot(nBars, m.panOffset)
	chartLines := renderCandles(snap, plotW, candleH, opts)

	// Join each candle row with its matching price-axis label.
	if axisW > 0 {
		min, max, ok := priceRange(snap, opts)
		if !ok {
			min, max = 0, 1
		}
		axisLines := renderPriceAxis(min, max, axisW, candleH)
		for i := range chartLines {
			chartLines[i] = chartLines[i] + axisLines[i]
		}
	}

	// Time ruler + volume span the plot width; pad under the price axis.
	// Custom TFs use intraday label format (only built-in 1d is daily).
	rulerTF := bars.TF1m
	if !m.usingCustomTF() {
		rulerTF = m.tfs[m.tfIdx]
	}
	axisPad := strings.Repeat(" ", axisW)
	rulerLine := renderTimeRuler(snap, plotW, rulerTF) + axisPad
	volLines := renderVolume(snap, plotW, volH, m.shading)
	for i := range volLines {
		volLines[i] = volLines[i] + axisPad
	}

	// Stack: candles (+ price axis), then the time ruler, then volume.
	// Plain "\n" join — not lipgloss.JoinVertical — so we never re-scan full
	// ANSI chart lines for display width on the 10–30 Hz path (stringWidth
	// was the post-bake CPU hot spot). Safe without JoinVertical's width
	// normalization: chart/ruler/volume rows are fixed to plotW+axisW, and
	// status/info/bottom already target m.width.
	chartParts := make([]string, 0, len(chartLines)+1+len(volLines))
	chartParts = append(chartParts, chartLines...)
	chartParts = append(chartParts, rulerLine)
	chartParts = append(chartParts, volLines...)
	chart := strings.Join(chartParts, "\n")

	// Pass chart snap into the status bar so quiet-tape OHLCV fallback does
	// not take a second Aggregator.Snapshot (same mutex as ingest).
	parts := []string{m.renderStatusBar(snap, market)}
	if panelH > 0 {
		parts = append(parts, m.renderInfoBar(m.width, panelH, book, market))
	}
	parts = append(parts, chart, m.renderBottom())
	return strings.Join(parts, "\n")
}

// statusBar palette + cached styles (Tokyo Night; rebuilt every frame was
// pure CPU on the 10–30 Hz render path).
var (
	statusMutedFg = lipgloss.Color("#565f89")
	statusSymFg   = lipgloss.Color("#c0caf5")
	statusPriceFg = lipgloss.Color("#e0af68")
	statusTFFg    = lipgloss.Color("#bb9af7")
	statusSepFg   = lipgloss.Color("#3b4261")
	statusFgOnBg  = lipgloss.Color("#1a1b26")

	stMuted     = lipgloss.NewStyle().Foreground(statusMutedFg)
	stSym       = lipgloss.NewStyle().Foreground(statusSymFg).Bold(true)
	stPrice     = lipgloss.NewStyle().Foreground(statusPriceFg).Bold(true)
	stTF        = lipgloss.NewStyle().Foreground(statusTFFg).Bold(true)
	stSep       = lipgloss.NewStyle().Foreground(statusSepFg)
	stPaper     = lipgloss.NewStyle().Background(lipgloss.Color("#9ece6a")).Foreground(statusFgOnBg).Bold(true)
	stLive      = lipgloss.NewStyle().Background(lipgloss.Color("#f7768e")).Foreground(statusFgOnBg).Bold(true)
	stSessDef   = lipgloss.NewStyle().Background(lipgloss.Color("#414868")).Foreground(statusSymFg).Bold(true)
	stSessReg   = lipgloss.NewStyle().Background(lipgloss.Color("#9ece6a")).Foreground(statusFgOnBg).Bold(true)
	stSessExt   = lipgloss.NewStyle().Background(lipgloss.Color("#e0af68")).Foreground(statusFgOnBg).Bold(true)
	stSessON    = lipgloss.NewStyle().Background(lipgloss.Color("#7aa2f7")).Foreground(statusFgOnBg).Bold(true)
	stSizeOn    = lipgloss.NewStyle().Background(infoSizeOnBg).Foreground(infoSizeOnFg).Bold(true)
	stIndOn     = lipgloss.NewStyle().Foreground(infoIndOnFg).Bold(true)
	stLocked    = lipgloss.NewStyle().Background(lipgloss.Color("#f7768e")).Foreground(statusFgOnBg).Bold(true)
	stHistory   = lipgloss.NewStyle().Background(lipgloss.Color("#e0af68")).Foreground(statusFgOnBg).Bold(true)
	stStatusBar = lipgloss.NewStyle().Background(lipgloss.Color("#16161e"))

	// Info-bar signed PnL / side colors (avoid NewStyle per frame).
	stPLUp    = lipgloss.NewStyle().Foreground(infoUpFg).Bold(true)
	stPLDown  = lipgloss.NewStyle().Foreground(infoDownFg).Bold(true)
	stPLMuted = lipgloss.NewStyle().Foreground(infoMutedFg)
	stSideUp  = lipgloss.NewStyle().Foreground(infoUpFg)
	stSideDn  = lipgloss.NewStyle().Foreground(infoDownFg)
)

func (m *Model) renderStatusBar(chartSnap bars.Snapshot, market bars.MarketSnapshot) string {
	sep := stSep.Render(" │ ")
	sepW := lipgloss.Width(sep)

	// Mode badge.
	mode := stPaper.Render(" PAPER ")
	if !m.d.Paper {
		mode = stLive.Render(" LIVE ")
	}

	// Session badge.
	sessStyle := stSessDef
	switch m.sess {
	case session.Regular:
		sessStyle = stSessReg
	case session.PreMarket, session.AfterHours:
		sessStyle = stSessExt
	case session.Overnight:
		sessStyle = stSessON
	}
	sessBadge := sessStyle.Render(" " + m.sess.String() + " ")

	// Quote block: SYMBOL  price  bid×ask. When the tape is quiet, fall
	// back to the newest (live-edge) chart bar close + compact OHLCV so a
	// closed session still shows a print comparable to TradingView's header.
	// Reuse the frame chart snap only when live (panOffset==0); when panned
	// the chart window is historical and must not overwrite the status price.
	last := market.LastPrice
	lastAt := market.LastAt
	price := "—"
	var lastBarOHLCV string
	if last > 0 {
		price = fmt.Sprintf("%.2f", last)
	} else {
		var edge bars.Snapshot
		if m.panOffset == 0 {
			edge = chartSnap
		} else {
			edge = m.chartSnapshot(1, 0)
		}
		if len(edge.Bars) > 0 {
			b := edge.Bars[len(edge.Bars)-1]
			price = fmt.Sprintf("%.2f", b.Close)
			lastBarOHLCV = fmt.Sprintf(" O%.2f H%.2f L%.2f V%.0f", b.Open, b.High, b.Low, b.Volume)
		}
	}
	quote := stSym.Render(m.symbol) + " " + stPrice.Render(price)
	if lastBarOHLCV != "" {
		quote += stMuted.Render(lastBarOHLCV)
	}
	if market.Bid > 0 && market.Ask > 0 {
		quote += stMuted.Render(fmt.Sprintf("  %.2f×%.2f", market.Bid, market.Ask))
	}

	// Clock.
	clock := stMuted.Render(time.Now().In(session.Location()).Format("15:04:05 ET"))

	// Size presets (chips) — top row only.
	var sizeBits []string
	for i, p := range m.d.Cfg.SizePresets {
		txt := fmt.Sprintf("%d", p)
		if i == m.preset {
			sizeBits = append(sizeBits, stSizeOn.Render(" "+txt+" "))
		} else {
			sizeBits = append(sizeBits, stMuted.Render(txt))
		}
	}
	sizeSeg := stMuted.Render("sz ") + strings.Join(sizeBits, " ")

	// Indicator chips — top row only.
	chip := func(tag string, on bool) string {
		if on {
			return stIndOn.Render(tag)
		}
		return stMuted.Render(tag)
	}
	indsSeg := chip("ema", m.showEMA) + " " +
		chip("ema2", m.showEMA2) + " " +
		chip("vwap", m.showVWAP) + " " +
		chip("sh", m.shading)

	// Timeframe (built-in or custom, e.g. 2m).
	tfSeg := stTF.Render(m.chartTFName())

	// Latency.
	latSeg := ""
	if m.d.Latency != nil {
		if p50, p99 := m.d.Latency.Percentiles(); p50 > 0 {
			latSeg = stMuted.Render(fmt.Sprintf("p50 %s p99 %s",
				p50.Round(time.Millisecond), p99.Round(time.Millisecond)))
		}
	}

	// Alerts: unreconciled / lock / history pan / stale feed.
	alerts := ""
	if m.d.Store != nil && !m.d.Store.Reconciled() {
		alerts += stLocked.Render(" UNRECONCILED: order entry blocked ")
	}
	if locked, reason := m.d.Risk.Locked(); locked {
		alerts += stLocked.Render(" LOCKED: " + reason + " ")
	}
	if m.panOffset > 0 {
		alerts += stHistory.Render(fmt.Sprintf(" ◀ HISTORY −%d ", m.panOffset))
	}
	if m.leftCrop > 0 {
		alerts += stHistory.Render(fmt.Sprintf(" ▶ FOCUS −%d ", m.leftCrop))
	}
	if !lastAt.IsZero() && time.Since(lastAt) > 10*time.Second && m.sess != session.Closed {
		alerts += stMuted.Render(" feed quiet")
	}

	// Fit segments under terminal width (alerts always appended last if room).
	// Priority: mode/sess/quote first, then size & indicators, then clock/tf/lat.
	const (
		pMode  = 1
		pSess  = 2
		pQuote = 3
		pSize  = 4
		pInds  = 5
		pTF    = 6
		pClock = 7
		pLat   = 8
	)
	// Leave room for alerts when present.
	budget := m.width
	if alerts != "" {
		aw := lipgloss.Width(alerts)
		if aw < budget {
			budget -= aw
		}
	}
	core := fitInfoSegs(budget, sep, sepW, []infoSeg{
		{mode, pMode},
		{sessBadge, pSess},
		{quote, pQuote},
		{sizeSeg, pSize},
		{indsSeg, pInds},
		{tfSeg, pTF},
		{clock, pClock},
		{latSeg, pLat},
	})
	line := core + alerts
	return stStatusBar.Width(m.width).MaxWidth(m.width).Render(line)
}

// Bottom-bar styles. Hoisted to package vars so renderBottom (called every
// frame) doesn't call lipgloss.NewStyle(); only the dynamic width is applied
// at render time via Width(), which is a struct copy, not a re-init.
var (
	bottomConfirmBG = lipgloss.Color("94")
	bottomConfirmFG = lipgloss.Color("0")
	bottomErrFG     = lipgloss.Color("9")
	bottomOkFG      = lipgloss.Color("10")

	stBottomConfirm = lipgloss.NewStyle().Background(bottomConfirmBG).Foreground(bottomConfirmFG).Bold(true)
	stBottomErr     = lipgloss.NewStyle().Foreground(bottomErrFG)
	stBottomOk      = lipgloss.NewStyle().Foreground(bottomOkFG)
	stBottomFaint   = lipgloss.NewStyle().Faint(true)
)

func (m *Model) renderBottom() string {
	// Width is dynamic; apply on top of the prebuilt styles.
	if m.pending != nil {
		return stBottomConfirm.Width(m.width).Render("CONFIRM: " + m.pending.label + "  (y/n)")
	}
	if m.confirmQ {
		return stBottomConfirm.Width(m.width).Render("Position open in " + m.symbol + " — quit anyway? (y/n)")
	}
	if m.cmdActive {
		return stBottomFaint.Width(m.width).Render(":" + m.cmdBuf + "█")
	}
	if m.status != "" && time.Since(m.statusAt) < 8*time.Second {
		if m.statusErr {
			return stBottomErr.Width(m.width).Render(m.status)
		}
		return stBottomOk.Width(m.width).Render(m.status)
	}
	return stBottomFaint.Width(m.width).Render(": for commands · :help for keys")
}

// infoBar palette — foreground accents only (no full-row backgrounds).
var (
	infoLabelFg  = lipgloss.Color("#7aa2f7")
	infoMutedFg  = lipgloss.Color("#565f89")
	infoValueFg  = lipgloss.Color("#c0caf5")
	infoUpFg     = lipgloss.Color("#9ece6a")
	infoDownFg   = lipgloss.Color("#f7768e")
	infoVwapFg   = lipgloss.Color("#e0af68")
	infoSizeOnBg = lipgloss.Color("#3d59a1")
	infoSizeOnFg = lipgloss.Color("#c0caf5")
	infoIndOnFg  = lipgloss.Color("#7dcfff")

	// Hoisted info-bar styles (rebuilt per frame was pure CPU on the render path).
	stInfoMuted = lipgloss.NewStyle().Foreground(infoMutedFg)
	stInfoValue = lipgloss.NewStyle().Foreground(infoValueFg)
	stInfoLabel = lipgloss.NewStyle().Foreground(infoLabelFg).Bold(true)
	stInfoVwap  = lipgloss.NewStyle().Foreground(infoVwapFg)
	stInfoEq    = lipgloss.NewStyle().Foreground(infoValueFg).Bold(true)
	// Order side glyphs (B/S) — bold tinted.
	stInfoSideBuy  = lipgloss.NewStyle().Foreground(infoUpFg).Bold(true)
	stInfoSideSell = lipgloss.NewStyle().Foreground(infoDownFg).Bold(true)
)

// infoSeg is one pipe-separated info-bar unit with a fit priority
// (lower = keep first when width is tight).
type infoSeg struct {
	text string
	prio int
}

// infoBarRowsFor returns how many rows the info strip should use (0–2),
// based on a store snapshot taken once per frame. A second row is only used
// when the active symbol has open orders.
func infoBarRowsFor(book state.BookSnapshot, symbol string) int {
	for _, o := range book.Orders {
		if o.Symbol == symbol {
			return 2
		}
	}
	return 1
}

// infoBarRows returns how many rows the info strip should use (0–2).
// A second row is only used when the active symbol has open orders.
// Size presets and indicators live exclusively on the status bar (top row).
func (m *Model) infoBarRows() int {
	if m.width <= 0 {
		return 0
	}
	if m.openOrderCount(m.symbol) > 0 {
		return 2
	}
	return 1
}

// openOrderCount returns how many cached open orders belong to symbol.
func (m *Model) openOrderCount(symbol string) int {
	if m.d.Store == nil {
		return 0
	}
	n := 0
	for _, o := range m.d.Store.OpenOrders() {
		if o.Symbol == symbol {
			n++
		}
	}
	return n
}

// chartOrdersFor converts open broker orders for symbol into candle-pane
// markers. Limits plot at limit_price; stops at stop_price; stop_limit plots
// both trigger and limit when present (two markers). Market/unpriced skipped.
func chartOrdersFor(symbol string, orders []alpaca.Order) []ChartOrder {
	var out []ChartOrder
	for _, o := range orders {
		if o.Symbol != symbol {
			continue
		}
		for _, px := range chartOrderPrices(o) {
			out = append(out, ChartOrder{Side: o.Side, Price: px})
		}
	}
	return out
}

// chartOrderPrices returns chart-visible price levels for a working order.
// stop_limit yields stop then limit when both are valid and distinct.
func chartOrderPrices(o alpaca.Order) []float64 {
	switch strings.ToLower(o.Type) {
	case "stop":
		if px := float64(o.StopPrice); validOrderPrice(px) {
			return []float64{px}
		}
		return nil
	case "stop_limit":
		var out []float64
		if px := float64(o.StopPrice); validOrderPrice(px) {
			out = append(out, px)
		}
		if px := float64(o.LimitPrice); validOrderPrice(px) {
			if len(out) == 0 || out[0] != px {
				out = append(out, px)
			}
		}
		return out
	default:
		// limit, and any other type that carries a resting limit price
		if px := float64(o.LimitPrice); validOrderPrice(px) {
			return []float64{px}
		}
		return nil
	}
}

// renderInfoBar draws a width-aware strip under the status bar.
//
// Quote/symbol/size/indicators live on the status line. This strip is book
// + risk + working orders: richer POS metrics, priority-trimmed account
// segments, and open orders for the active symbol only.
//
// book and market are the per-frame snapshots taken once in View() — this
// function never touches the store/agg mutexes directly.
func (m *Model) renderInfoBar(w, h int, book state.BookSnapshot, market bars.MarketSnapshot) string {
	if h <= 0 || w <= 0 {
		return ""
	}
	row1, row2 := m.buildInfoBarRows(w, book, market)
	// Soft band under the status bar so the strip reads as a unit without
	// competing with the chart below.
	rowStyle := lipgloss.NewStyle().Width(w).MaxWidth(w).
		Background(lipgloss.Color("#1a1b26")).
		PaddingLeft(1)
	if h == 1 || row2 == "" {
		return rowStyle.Render(row1)
	}
	return rowStyle.Render(row1) + "\n" + rowStyle.Render(row2)
}

func (m *Model) buildInfoBarRows(w int, book state.BookSnapshot, market bars.MarketSnapshot) (row1, row2 string) {
	// Leave room for PaddingLeft(1) applied in renderInfoBar.
	if w > 1 {
		w--
	}
	muted := stInfoMuted
	value := stInfoValue
	lbl := stInfoLabel
	sep := muted.Render("  ·  ")
	sepW := lipgloss.Width(sep)

	last := market.LastPrice
	vwapVal := market.VWAP
	hasVWAP := vwapVal == vwapVal // NaN check

	// --- POS (priority 1) ---
	posText := lbl.Render("POS") + " " + muted.Render("flat")
	if p := book.ActivePosition(m.symbol); p != nil {
		side := "L"
		sideSt := stSideUp
		if p.Side == "short" {
			side = "S"
			sideSt = stSideDn
		}
		plSt := stPLUp
		if p.UnrealizedPL < 0 {
			plSt = stPLDown
		}
		posText = lbl.Render("POS") + " " +
			value.Render(fmt.Sprintf("%.0f", p.Qty)) +
			sideSt.Render(side) +
			value.Render(fmt.Sprintf("@%.2f", p.AvgEntryPrice)) +
			" " + plSt.Render(fmt.Sprintf("%+.2f", p.UnrealizedPL))

		// uPL % vs entry notional.
		entryNotional := math.Abs(float64(p.AvgEntryPrice) * float64(p.Qty))
		if entryNotional > 0 {
			pct := float64(p.UnrealizedPL) / entryNotional * 100
			posText += plSt.Render(fmt.Sprintf(" (%+.2f%%)", pct))
		}
		// Distance to session VWAP (last − vwap).
		if hasVWAP && last > 0 {
			d := last - vwapVal
			dSt := stSideUp
			if d < 0 {
				dSt = stSideDn
			}
			posText += " " + dSt.Render(fmt.Sprintf("Δv%+.2f", d))
		}
		// Notional at last (or avg if no last).
		mark := last
		if mark <= 0 {
			mark = float64(p.AvgEntryPrice)
		}
		if mark > 0 {
			posText += muted.Render(" ") + muted.Render(compactMoney(mark*float64(p.Qty)))
		}
	}

	// Other symbols with positions or resting orders (C/X only touch active).
	otherN := book.OtherOpenCount(m.symbol)
	otherPos := ""
	if otherN > 0 {
		// Reminder that C/X are symbol-scoped when multi-name risk is open.
		otherPos = muted.Render(fmt.Sprintf("+%d open (:sym+X)", otherN))
	}

	// --- Account: eq + realized day/week PnL + bp; cash lower ---
	// rday/rwk are fill-based realized P&L (closed orders), not equity MTM.
	// Per-position unrealized stays on the POS segment.
	eqText, cashText, bpText, dayText, weekText := "", "", "", "", ""
	if book.HasAccount {
		a := book.Account
		eqText = lbl.Render("eq") + " " + stInfoEq.Render(compactMoney(float64(a.Equity)))
		bpText = lbl.Render("bp") + " " + value.Render(compactMoney(float64(a.BuyingPower)))
		cashText = lbl.Render("cash") + " " + value.Render(compactMoney(float64(a.Cash)))
	} else {
		eqText = lbl.Render("eq") + " " + muted.Render("—")
	}
	pnl := book.PnL
	if pnl.HasDay {
		dayLbl := "rday"
		if pnl.Partial {
			// Asterisk: sample excluded unreconcilable symbols (may undercount).
			dayLbl = "rday*"
		}
		dayText = lbl.Render(dayLbl) + " " + signedPLStyle(pnl.Day).Render(signedCompactMoney(pnl.Day))
	}
	if pnl.HasWeek {
		weekLbl := "rwk"
		if pnl.Partial {
			weekLbl = "rwk*"
		}
		weekText = lbl.Render(weekLbl) + " " + signedPLStyle(pnl.Week).Render(signedCompactMoney(pnl.Week))
	}

	// --- VWAP absolute (delta already on POS) ---
	vwapText := ""
	if hasVWAP {
		vwapText = lbl.Render("vwap") + " " +
			stInfoVwap.Render(fmt.Sprintf("%.2f", vwapVal))
	}

	// --- Orders: active symbol only (trading focus) ---
	activeOrds := book.ActiveOrders(m.symbol)
	formatOrd := func(o alpaca.Order) string {
		sideSt := stInfoSideBuy
		sideMark := "B"
		if strings.EqualFold(o.Side, "sell") {
			sideSt = stInfoSideSell
			sideMark = "S"
		}
		s := sideSt.Render(sideMark)
		s += value.Render(fmt.Sprintf(" %.0f", o.Qty-o.FilledQty))
		if o.FilledQty > 0 {
			s += muted.Render(fmt.Sprintf("(%.0f/%.0f)", o.FilledQty, o.Qty))
		}
		switch strings.ToLower(o.Type) {
		case "limit":
			s += muted.Render(fmt.Sprintf("@%.2f", o.LimitPrice))
		case "stop":
			if px := float64(o.StopPrice); validOrderPrice(px) {
				s += muted.Render(fmt.Sprintf(" stop@%.2f", px))
			}
		case "stop_limit":
			if sp := float64(o.StopPrice); validOrderPrice(sp) {
				s += muted.Render(fmt.Sprintf(" stop@%.2f", sp))
			}
			if lp := float64(o.LimitPrice); validOrderPrice(lp) {
				s += muted.Render(fmt.Sprintf(" lim@%.2f", lp))
			}
		}
		return s
	}
	ordText := ""
	busyOrders := len(activeOrds) > 0
	if busyOrders {
		var bits []string
		n := 0
		for _, o := range activeOrds {
			if n >= 3 {
				break
			}
			bits = append(bits, formatOrd(o))
			n++
		}
		extra := len(activeOrds) - n
		head := lbl.Render(fmt.Sprintf("ORD %d", len(activeOrds)))
		ordText = head + " " + strings.Join(bits, " ")
		if extra > 0 {
			ordText += muted.Render(fmt.Sprintf(" +%d", extra))
		}
	}

	// Segment priorities (lower = keep first under width pressure).
	// Size presets + indicators live only on the status bar.
	const (
		pPOS   = 1
		pORD   = 2
		pEQ    = 3
		pDay   = 4
		pWeek  = 5
		pBP    = 6
		pVWAP  = 7
		pCash  = 8
		pOther = 9
	)

	if busyOrders {
		// Row 1: book + account + PnL. Row 2: orders only.
		r1 := fitInfoSegs(w, sep, sepW, []infoSeg{
			{posText, pPOS},
			{eqText, pEQ},
			{dayText, pDay},
			{weekText, pWeek},
			{bpText, pBP},
			{vwapText, pVWAP},
			{cashText, pCash},
			{otherPos, pOther},
		})
		r2 := fitInfoSegs(w, sep, sepW, []infoSeg{
			{ordText, pORD},
		})
		return r1, r2
	}

	// Quiet row: POS · eq · rday · rwk · bp · vwap · cash · +N sym.
	r1 := fitInfoSegs(w, sep, sepW, []infoSeg{
		{posText, pPOS},
		{eqText, pEQ},
		{dayText, pDay},
		{weekText, pWeek},
		{bpText, pBP},
		{vwapText, pVWAP},
		{cashText, pCash},
		{otherPos, pOther},
	})
	return r1, ""
}

// fitInfoSegs keeps segments in display order but drops lower-priority ones
// when the visible width would exceed maxW.
func fitInfoSegs(maxW int, sep string, sepW int, segs []infoSeg) string {
	// Filter empty.
	clean := make([]infoSeg, 0, len(segs))
	for _, s := range segs {
		if strings.TrimSpace(s.text) == "" {
			continue
		}
		clean = append(clean, s)
	}
	if len(clean) == 0 {
		return ""
	}
	// Greedy by priority: try to include highest-priority first.
	widths := make([]int, len(clean))
	for i, s := range clean {
		widths[i] = lipgloss.Width(s.text)
	}
	include := make([]bool, len(clean))
	// Order indices by priority then original index.
	order := make([]int, len(clean))
	for i := range order {
		order[i] = i
	}
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			if clean[order[j]].prio < clean[order[i]].prio ||
				(clean[order[j]].prio == clean[order[i]].prio && order[j] < order[i]) {
				order[i], order[j] = order[j], order[i]
			}
		}
	}
	used := 0
	nOn := 0
	for _, i := range order {
		extra := widths[i]
		if nOn > 0 {
			extra += sepW
		}
		if used+extra > maxW && nOn > 0 {
			continue
		}
		// Always try to keep at least the first priority item even if over.
		if nOn == 0 && widths[i] > maxW {
			include[i] = true
			used = widths[i]
			nOn = 1
			continue
		}
		if used+extra > maxW {
			continue
		}
		include[i] = true
		used += extra
		nOn++
	}
	var parts []string
	for i, s := range clean {
		if include[i] {
			parts = append(parts, s.text)
		}
	}
	return strings.Join(parts, sep)
}

// compactMoney shortens large currency figures for the info strip.
func compactMoney(v float64) string {
	neg := v < 0
	a := math.Abs(v)
	var s string
	switch {
	case a >= 1_000_000:
		s = fmt.Sprintf("%.2fM", a/1_000_000)
	case a >= 10_000:
		s = fmt.Sprintf("%.1fk", a/1000)
	case a >= 1000:
		s = fmt.Sprintf("%.1fk", a/1000)
	default:
		s = fmt.Sprintf("%.0f", a)
	}
	if neg {
		return "-" + s
	}
	return s
}

// signedCompactMoney formats PnL with an explicit +/− sign.
func signedCompactMoney(v float64) string {
	if v > 0 {
		return "+" + compactMoney(v)
	}
	if v < 0 {
		return compactMoney(v) // already has leading "-"
	}
	return "0"
}

func signedPLStyle(v float64) lipgloss.Style {
	if v > 0 {
		return stPLUp
	}
	if v < 0 {
		return stPLDown
	}
	return stPLMuted
}
