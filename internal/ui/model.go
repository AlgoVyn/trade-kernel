// Package ui implements the bubbletea TUI: braille candlestick chart,
// indicator overlays, position/order panel, status bar, hotkeys and the
// ':' command line.
package ui

import (
	"context"
	"fmt"
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
	// skipRisk for panic flows that must bypass the checker.
}

// Model is the bubbletea root model.
type Model struct {
	d Deps

	symbol string
	tfs    []bars.TF
	tfIdx  int
	preset int
	keyFor map[string]string // key string -> action
	width  int
	height int

	cmdActive bool
	cmdBuf    string

	pending  *pendingAction
	quitting bool
	confirmQ bool // quit confirmation

	status    string
	statusErr bool
	statusAt  time.Time

	showSMA  bool
	showEMA  bool
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
		showSMA:  true,
		showEMA:  true,
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
	if tf, ok := bars.ParseTF(d.Cfg.Chart.Timeframe); ok {
		for i, t := range m.tfs {
			if t == tf {
				m.tfIdx = i
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
		"B":      "buy",
		"S":      "sell",
		"A":      "add",
		"D":      "reduce",
		"F":      "flatten",
		"C":      "cancel_all",
		"X":      "panic",
		":":      "cmdline",
		"tab":    "cycle_tf",
		"i":      "cycle_indicators",
		"q":      "quit",
		"ctrl+c": "quit_force",
	}
}

// Init starts the render ticker.
func (m *Model) Init() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
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
		m.d.Latency.Record(msg.latency)
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
	case "cancel_all":
		return m, m.cancelAllIntent()
	case "panic":
		return m, m.panicIntent()
	case "cmdline":
		m.cmdActive = true
		m.cmdBuf = ""
		return m, nil
	case "cycle_tf":
		m.tfIdx = (m.tfIdx + 1) % len(m.tfs)
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
	// Cycle: all → SMA only → EMA only → VWAP only → none → all.
	switch {
	case m.showSMA && m.showEMA && m.showVWAP:
		m.showEMA, m.showVWAP = false, false
	case m.showSMA:
		m.showSMA, m.showEMA = false, true
	case m.showEMA:
		m.showEMA, m.showVWAP = false, true
	case m.showVWAP:
		m.showVWAP = false
	default:
		m.showSMA, m.showEMA, m.showVWAP = true, true, true
	}
}

func (m *Model) qty() int { return m.d.Cfg.SizePresets[m.preset%len(m.d.Cfg.SizePresets)] }

func (m *Model) setStatus(s string, isErr bool) {
	m.status, m.statusErr, m.statusAt = s, isErr, time.Now()
}

// orderIntent runs risk checks and either confirms or submits.
func (m *Model) orderIntent(side string, qty int, limit float64, skipRisk bool) tea.Cmd {
	symbol := m.symbol
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
	p := &pendingAction{label: label, run: run}
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
	qty := int(abs(pos))
	side := "sell"
	if pos < 0 {
		side = "buy"
	}
	if !skipRisk {
		if err := m.d.Risk.Check(symbol, side, qty); err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
	}
	label := fmt.Sprintf("FLATTEN %s (%s %d)", symbol, strings.ToUpper(side), qty)
	p := &pendingAction{
		label: label,
		run:   func(ctx context.Context) (alpaca.Order, error) { return m.d.Exec.Flatten(ctx, symbol, pos) },
	}
	if m.d.Cfg.ConfirmOrders && !skipRisk {
		m.pending = p
		return nil
	}
	return m.execAsync(p)
}

func (m *Model) cancelAllIntent() tea.Cmd {
	p := &pendingAction{
		label: "CANCEL ALL ORDERS",
		run: func(ctx context.Context) (alpaca.Order, error) {
			return alpaca.Order{}, m.d.Exec.CancelAll(ctx)
		},
	}
	return m.execAsync(p)
}

// panicIntent: cancel all + flatten, bypassing the risk checker and
// confirmation. This is the emergency exit.
func (m *Model) panicIntent() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start := time.Now()
		_ = m.d.Exec.CancelAll(ctx)
		pos := m.d.Store.PositionQty(m.symbol)
		if pos == 0 {
			return orderResultMsg{label: "PANIC", detail: "cancelled all; already flat", latency: time.Since(start)}
		}
		o, err := m.d.Exec.Flatten(ctx, m.symbol, pos)
		if err != nil {
			return orderResultMsg{label: "PANIC", err: err, latency: time.Since(start)}
		}
		return orderResultMsg{label: "PANIC", detail: "flatten order " + o.ID, latency: time.Since(start)}
	}
}

func (m *Model) execAsync(p *pendingAction) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		start := time.Now()
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
		tf, ok := bars.ParseTF(c.TF)
		if !ok {
			m.setStatus("unknown timeframe "+c.TF, true)
			return nil
		}
		for i, t := range m.tfs {
			if t == tf {
				m.tfIdx = i
			}
		}
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
		return m.cancelAllIntent()
	case cmdline.KindUnlock:
		m.d.Risk.Unlock()
		m.setStatus("kill-switch unlocked", false)
		return nil
	case cmdline.KindConfirm:
		m.d.Cfg.ConfirmOrders = c.On
		m.setStatus(fmt.Sprintf("confirm %v", onOff(c.On)), false)
		return nil
	case cmdline.KindShading:
		m.shading = c.On
		return nil
	case cmdline.KindQuit:
		m.done = true
		return nil
	case cmdline.KindHelp:
		m.setStatus("B/S buy/sell A/D add/reduce F flatten C cancel X panic 1-4 preset : cmd tab tf i indicators q quit", false)
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

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// View renders the full screen.
func (m *Model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	sideW := 0
	if m.width >= 100 {
		sideW = 38
	}
	chartW := m.width - sideW
	statusH, bottomH := 1, 1
	volH := (m.height - statusH - bottomH) / 5
	if volH < 3 {
		volH = 3
	}
	if volH > 8 {
		volH = 8
	}
	candleH := m.height - statusH - bottomH - volH
	if candleH < 1 {
		candleH = 1
	}

	snap := m.d.Agg.Snapshot(m.tfs[m.tfIdx], chartW)
	chartLines := renderCandles(snap, chartW, candleH, ChartOpts{
		ShowSMA: m.showSMA, ShowEMA: m.showEMA, ShowVWAP: m.showVWAP,
		SessionShading: m.shading,
	})
	volLines := renderVolume(snap, chartW, volH, m.shading)

	left := lipgloss.JoinVertical(lipgloss.Left, append(chartLines, volLines...)...)

	var body string
	if sideW > 0 {
		panel := m.renderPanel(sideW, candleH+volH)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, panel)
	} else {
		body = left
	}

	return lipgloss.JoinVertical(lipgloss.Left, m.renderStatusBar(), body, m.renderBottom())
}

func (m *Model) renderStatusBar() string {
	fg := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	mode := lipgloss.NewStyle().Background(lipgloss.Color("28")).Foreground(lipgloss.Color("15")).Bold(true).Render(" PAPER ")
	if !m.d.Paper {
		mode = lipgloss.NewStyle().Background(lipgloss.Color("9")).Foreground(lipgloss.Color("15")).Bold(true).Render(" LIVE ")
	}
	sessSt := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	switch m.sess {
	case session.Regular:
		sessSt = lipgloss.NewStyle().Background(lipgloss.Color("22")).Foreground(lipgloss.Color("15")).Bold(true)
	case session.PreMarket, session.AfterHours:
		sessSt = lipgloss.NewStyle().Background(lipgloss.Color("94")).Foreground(lipgloss.Color("0")).Bold(true)
	case session.Overnight:
		sessSt = lipgloss.NewStyle().Background(lipgloss.Color("18")).Foreground(lipgloss.Color("15")).Bold(true)
	}
	last, lastAt := m.d.Agg.LatestTrade()
	price := "—"
	if last > 0 {
		price = fmt.Sprintf("%.2f", last)
	}
	bid, ask, _ := m.d.Agg.LatestQuote()
	spread := ""
	if bid > 0 && ask > 0 {
		spread = fmt.Sprintf(" %.2f×%.2f", bid, ask)
	}
	p50, p99 := m.d.Latency.Percentiles()
	lat := ""
	if p50 > 0 {
		lat = fmt.Sprintf(" p50 %s p99 %s", p50.Round(time.Millisecond), p99.Round(time.Millisecond))
	}
	lock := ""
	if locked, reason := m.d.Risk.Locked(); locked {
		lock = lipgloss.NewStyle().Background(lipgloss.Color("9")).Foreground(lipgloss.Color("15")).Bold(true).
			Render(" LOCKED: " + reason + " ")
	}
	stale := ""
	if !lastAt.IsZero() && time.Since(lastAt) > 10*time.Second && m.sess != session.Closed {
		stale = fg.Faint(true).Render(" (feed quiet)")
	}
	line := fmt.Sprintf("%s %s %s  %s%s  %s  size %d  tf %s%s%s",
		mode, sessSt.Render(" "+m.sess.String()+" "),
		lipgloss.NewStyle().Bold(true).Render(m.symbol),
		price, spread,
		time.Now().In(session.Location()).Format("15:04:05 ET"),
		m.qty(), m.tfs[m.tfIdx], lat, stale)
	return lipgloss.NewStyle().Width(m.width).Render(line + lock)
}

func (m *Model) renderBottom() string {
	st := lipgloss.NewStyle().Width(m.width)
	if m.pending != nil {
		return st.Background(lipgloss.Color("94")).Foreground(lipgloss.Color("0")).Bold(true).
			Render("CONFIRM: " + m.pending.label + "  (y/n)")
	}
	if m.confirmQ {
		return st.Background(lipgloss.Color("94")).Foreground(lipgloss.Color("0")).Bold(true).
			Render("Position open in " + m.symbol + " — quit anyway? (y/n)")
	}
	if m.cmdActive {
		return st.Render(":" + m.cmdBuf + "█")
	}
	if m.status != "" && time.Since(m.statusAt) < 8*time.Second {
		if m.statusErr {
			return st.Foreground(lipgloss.Color("9")).Render(m.status)
		}
		return st.Foreground(lipgloss.Color("10")).Render(m.status)
	}
	return st.Faint(true).Render(": for commands · :help for keys")
}

func (m *Model) renderPanel(w, h int) string {
	var b []string
	add := func(s string) { b = append(b, s) }
	title := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dim := lipgloss.NewStyle().Faint(true)

	last, _ := m.d.Agg.LatestTrade()
	bid, ask, qAt := m.d.Agg.LatestQuote()
	add(title.Render(m.symbol))
	if last > 0 {
		add(fmt.Sprintf("last   %.2f", last))
	} else {
		add("last   —")
	}
	if bid > 0 && ask > 0 {
		age := time.Since(qAt).Round(time.Second)
		add(fmt.Sprintf("bid    %.2f", bid))
		add(fmt.Sprintf("ask    %.2f  (%s ago)", ask, age))
	} else {
		add("bid/ask —")
	}
	if v := m.d.Agg.SessionVWAP(); v == v { // NaN check
		add(fmt.Sprintf("vwap   %.2f", v))
	}
	add("")

	if p := m.d.Store.Position(m.symbol); p != nil {
		pl := lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
		if p.UnrealizedPL < 0 {
			pl = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
		}
		add(title.Render("POSITION"))
		add(fmt.Sprintf("qty    %.0f (%s)", p.Qty, p.Side))
		add(fmt.Sprintf("avg    %.2f", p.AvgEntryPrice))
		add(pl.Render(fmt.Sprintf("uPL    %.2f", p.UnrealizedPL)))
	} else {
		add(title.Render("POSITION"))
		add(dim.Render("flat"))
	}
	add("")

	if acct, ok := m.d.Store.Account(); ok {
		add(title.Render("ACCOUNT"))
		add(fmt.Sprintf("equity %.2f", acct.Equity))
		add(fmt.Sprintf("cash   %.2f", acct.Cash))
		add(fmt.Sprintf("bp     %.0f", acct.BuyingPower))
		add("")
	}

	orders := m.d.Store.OpenOrders()
	add(title.Render(fmt.Sprintf("OPEN ORDERS (%d)", len(orders))))
	shown := 0
	for _, o := range orders {
		if shown >= 6 {
			add(dim.Render(fmt.Sprintf("… +%d more", len(orders)-shown)))
			break
		}
		line := fmt.Sprintf("%s %s %.0f", o.Side, o.Symbol, o.Qty-o.FilledQty)
		if o.Type == "limit" {
			line += fmt.Sprintf(" @%.2f", o.LimitPrice)
		}
		if o.FilledQty > 0 {
			line += fmt.Sprintf(" (%.0f filled)", o.FilledQty)
		}
		add(line)
		shown++
	}
	if len(orders) == 0 {
		add(dim.Render("none"))
	}
	add("")

	add(title.Render("PRESETS"))
	var ps []string
	for i, p := range m.d.Cfg.SizePresets {
		s := fmt.Sprintf("%d:%d", i+1, p)
		if i == m.preset {
			s = lipgloss.NewStyle().Background(lipgloss.Color("24")).Render(s)
		}
		ps = append(ps, s)
	}
	add(strings.Join(ps, " "))
	add("")
	add(dim.Render(fmt.Sprintf("sma %s ema %s vwap %s shade %s",
		onOff(m.showSMA), onOff(m.showEMA), onOff(m.showVWAP), onOff(m.shading))))

	panel := lipgloss.NewStyle().Width(w).PaddingLeft(1).Render(strings.Join(b, "\n"))
	lines := strings.Split(panel, "\n")
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}
