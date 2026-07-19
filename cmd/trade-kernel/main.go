// trade-kernel: low-latency terminal trading app for Alpaca.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/bars"
	"trade-kernel/internal/config"
	"trade-kernel/internal/execution"
	"trade-kernel/internal/risk"
	"trade-kernel/internal/session"
	"trade-kernel/internal/state"
	"trade-kernel/internal/ui"
)

func main() {
	configPath := flag.String("config", "trade-kernel.yaml", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Async log to file: never write to stdout/stderr once the TUI owns
	// the screen, and never log on the order hot path.
	logf, err := os.OpenFile("trade-kernel.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		defer logf.Close()
		log.SetOutput(logf)
	} else {
		log.SetOutput(os.Stderr)
	}

	// Startup banner (before TUI takes over the screen).
	mode := "PAPER"
	if cfg.Live() {
		mode = " *** LIVE *** "
	}
	fmt.Printf("trade-kernel starting — mode: %s | symbol: %s | presets: %v | max order %d | max position %d\n",
		mode, cfg.DefaultSymbol, cfg.SizePresets, cfg.Limits.MaxOrderQty, cfg.Limits.MaxPositionQty)
	if cfg.Live() {
		fmt.Println("WARNING: LIVE TRADING ENABLED. Orders will route to the live account.")
		time.Sleep(2 * time.Second)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Core components ---
	rest := alpaca.NewREST(cfg.APIKeyID, cfg.APISecretKey, !cfg.Live())
	sessions := session.NewEngine(nil)
	agg := bars.NewAggregator(cfg.Indicators.EMAPeriod, cfg.Indicators.EMA2Period)
	agg.SetVWAPAnchor(cfg.Indicators.VWAPAnchor)
	store := state.NewStore()

	elig := execution.NewEligibilityCache(rest)
	builder := execution.NewBuilder(agg, elig, cfg.ExtendedHours.SlippageBps, cfg.QuoteStaleAfter())
	exec := execution.NewRESTExecutor(rest, builder, sessions.Current, "day")

	checker := risk.NewChecker(risk.Limits{
		MaxOrderQty:    cfg.Limits.MaxOrderQty,
		MaxPositionQty: cfg.Limits.MaxPositionQty,
		Debounce:       cfg.Debounce(),
	}, store, nil)

	// Prefetch overnight eligibility for a symbol so the first overnight
	// hotkey does not serialize GET /v2/assets before PlaceOrder.
	prefetchElig := func(symbol string) {
		if symbol == "" {
			return
		}
		go func(sym string) {
			pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := elig.OvernightTradable(pctx, sym); err != nil {
				log.Printf("eligibility prefetch %s: %v", sym, err)
			}
		}(symbol)
	}

	// --- Session engine + clock sync + trading-host warm-up ---
	// REST.Warm GETs /v2/clock once: primes TLS/HTTP keep-alive so the first
	// PlaceOrder avoids a cold handshake, and seeds the session engine.
	if clock, err := rest.Warm(ctx); err == nil {
		sessions.SyncClock(clock.IsOpen, clock.NextOpen)
	} else {
		log.Printf("clock sync / warm: %v", err)
	}
	go sessions.Run(ctx)
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if clock, err := rest.Clock(ctx); err == nil {
					sessions.SyncClock(clock.IsOpen, clock.NextOpen)
				}
			}
		}
	}()

	// VWAP reset on session transitions per configured anchor.
	// Poll Current() each second rather than relying solely on the Engine
	// event channel (which can drop under a slow consumer).
	go func() {
		prev := sessions.Current()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cur := sessions.Current()
				if cur == prev {
					continue
				}
				log.Printf("session %s -> %s", prev, cur)
				if cfg.Indicators.VWAPAnchor == "session" || cur == session.Overnight {
					agg.ResetVWAP()
				}
				prev = cur
			}
		}
	}()

	// --- State reconciliation ---
	// Account/positions/orders every 5s; week portfolio history on a slower
	// cadence so the hot reconcile path is not paying an extra history RTT.
	if err := store.Refresh(ctx, rest); err != nil {
		log.Printf("initial reconcile: %v", err)
	}
	if err := store.RefreshWeekPnL(ctx, rest); err != nil {
		log.Printf("initial week PnL: %v", err)
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := store.Refresh(ctx, rest); err != nil {
					log.Printf("reconcile: %v", err)
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Refresh first so SetWeekPnL pairs history with REST mark
				// snaps (not WS-zeroed unrealized fields after fills).
				if err := store.Refresh(ctx, rest); err != nil {
					log.Printf("reconcile before week PnL: %v", err)
					// Still attempt history; strip uses last good REST snap.
				}
				if err := store.RefreshWeekPnL(ctx, rest); err != nil {
					log.Printf("week PnL: %v", err)
				}
			}
		}
	}()

	// --- Market data ---
	activeSymbol := atomic.Value{}
	activeSymbol.Store(cfg.DefaultSymbol)

	// Serialise backfills and ignore superseded generations so reconnect
	// + symbol switch cannot interleave Loads for different symbols.
	var (
		backfillMu  sync.Mutex
		backfillGen atomic.Uint64
	)
	runBackfill := func(symbol string, gen uint64) {
		backfillMu.Lock()
		defer backfillMu.Unlock()
		if backfillGen.Load() != gen {
			return
		}
		if activeSymbol.Load().(string) != symbol {
			return
		}
		if err := backfill(context.Background(), rest, agg, symbol); err != nil {
			log.Printf("backfill %s: %v", symbol, err)
		}
	}
	scheduleBackfill := func(symbol string) {
		gen := backfillGen.Add(1)
		go runBackfill(symbol, gen)
	}

	mws := alpaca.NewMarketWS(cfg.APIKeyID, cfg.APISecretKey)
	mws.OnTrade = func(tr alpaca.Trade) {
		if tr.Symbol != activeSymbol.Load().(string) {
			return
		}
		agg.OnTrade(tr.Symbol, float64(tr.Price), float64(tr.Size), tr.Timestamp)
	}
	mws.OnQuote = func(q alpaca.Quote) {
		if q.Symbol != activeSymbol.Load().(string) {
			return
		}
		agg.OnQuote(q.Symbol, float64(q.BidPrice), float64(q.AskPrice), q.Timestamp)
	}
	mws.OnInitial = func() {
		log.Printf("market ws subscribed; backfilling %s", mws.Symbol())
		scheduleBackfill(mws.Symbol())
	}
	mws.OnReconnect = func() {
		log.Printf("market ws reconnected; backfilling %s", mws.Symbol())
		scheduleBackfill(mws.Symbol())
	}
	mws.OnError = func(err error) { log.Printf("market ws: %v", err) }
	_ = mws.SetSymbol(cfg.DefaultSymbol)
	go mws.Run(ctx)

	// --- Trading stream ---
	tws := alpaca.NewTradingWS(cfg.APIKeyID, cfg.APISecretKey, !cfg.Live())
	tws.OnUpdate = func(u alpaca.TradeUpdate) { store.ApplyUpdate(u) }
	// Initial reconcile happens synchronously below before the TUI starts;
	// OnReconnect re-syncs from REST only on a real drop+reconnect.
	tws.OnReconnect = func() {
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := store.Refresh(rctx, rest); err != nil {
				log.Printf("reconcile after trading ws reconnect: %v", err)
			}
		}()
	}
	tws.OnError = func(err error) { log.Printf("trading ws: %v", err) }
	go tws.Run(ctx)

	// --- Symbol switching (re-subscribe + backfill) ---
	// Order: update active filter + clear stale quotes before subscribe so
	// ticks for the new symbol are not dropped and old prices cannot price
	// the new symbol's orders.
	switchSymbol := func(symbol string) error {
		gen := backfillGen.Add(1)
		activeSymbol.Store(symbol)
		agg.ResetMarket()
		agg.ResetVWAP()
		if err := mws.SetSymbol(symbol); err != nil {
			return err
		}
		// Synchronous backfill under the same serialisation as async ones.
		backfillMu.Lock()
		defer backfillMu.Unlock()
		if backfillGen.Load() != gen {
			return nil
		}
		if err := backfill(context.Background(), rest, agg, symbol); err != nil {
			return err
		}
		prefetchElig(symbol)
		return nil
	}

	// Warm eligibility for the default symbol before the first keystroke.
	prefetchElig(cfg.DefaultSymbol)

	// --- UI ---
	deps := ui.Deps{
		Cfg:          &cfg,
		Agg:          agg,
		Store:        store,
		Exec:         exec,
		Risk:         checker,
		Sessions:     sessions,
		Builder:      builder,
		Latency:      ui.NewLatencyTracker(256),
		SwitchSymbol: switchSymbol,
		Paper:        !cfg.Live(),
	}
	p := tea.NewProgram(ui.NewModel(deps), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
	}
	stop()
}

// backfill loads historical bars for the REST-backed timeframes, then
// fetches recent trades and replays them into the sub-minute timeframes
// (1s/5s/15s) — which the bars endpoint doesn't serve.
//
// 24/5 coverage: SIP feed supplies regular + pre/after-hours bars; BOATS
// feed supplies the overnight session (20:00–04:00 ET). The two series are
// merged by timestamp so the chart shows the full trading day.
func backfill(ctx context.Context, rest *alpaca.REST, agg *bars.Aggregator, symbol string) error {
	windows := []struct {
		tf       bars.TF
		api      string
		lookback time.Duration
	}{
		{bars.TF1m, "1Min", 5 * 24 * time.Hour},
		{bars.TF5m, "5Min", 10 * 24 * time.Hour},
		{bars.TF15m, "15Min", 15 * 24 * time.Hour},
		{bars.TF1h, "1Hour", 30 * 24 * time.Hour},
		{bars.TF1d, "1Day", 365 * 24 * time.Hour},
	}
	end := time.Now()
	var firstErr error
	for _, w := range windows {
		start := end.Add(-w.lookback)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		sip, errSIP := rest.Bars(cctx, symbol, w.api, start, end, 10000, "sip")
		cancel()
		if errSIP != nil {
			log.Printf("backfill %s %s sip: %v", symbol, w.api, errSIP)
			if firstErr == nil {
				firstErr = errSIP
			}
		}
		// Overnight (BOATS). Soft-fail: paper/free plans may lack access;
		// we still show SIP (regular + extended) if boats is unavailable.
		cctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		boats, errBoats := rest.Bars(cctx, symbol, w.api, start, end, 10000, "boats")
		cancel()
		if errBoats != nil {
			log.Printf("backfill %s %s boats (overnight): %v", symbol, w.api, errBoats)
			// Don't set firstErr solely from boats — SIP-only is still usable.
		}
		ab := alpaca.MergeBars(sip, boats)
		if len(ab) == 0 {
			if errSIP != nil {
				continue
			}
			log.Printf("backfill %s %s: empty (sip=%d boats=%d)", symbol, w.api, len(sip), len(boats))
			continue
		}
		if len(boats) > 0 {
			log.Printf("backfill %s %s: %d bars (sip=%d + boats overnight=%d → merged=%d)",
				symbol, w.api, len(ab), len(sip), len(boats), len(ab))
		}
		hist := make([]bars.Bar, len(ab))
		for i, b := range ab {
			hist[i] = bars.Bar{
				Start: b.Timestamp, Open: float64(b.Open), High: float64(b.High),
				Low: float64(b.Low), Close: float64(b.Close), Volume: float64(b.Volume),
				// Per-bar VWAP from Alpaca; Load reconstructs session VWAP from it.
				VWAP: float64(b.VWAP),
			}
		}
		agg.Load(w.tf, hist)
	}

	// Sub-minute TFs: replay recent trades through the aggregator so 1s/5s/15s
	// charts aren't blank at startup / on symbol switch. Anchor the window to
	// the newest backfilled 1m bar's time (a real trading time) rather than
	// time.Now() — otherwise a launch outside market hours (weekend, holiday,
	// pre-market) fetches an empty window and sub-minute charts stay blank.
	// SIP + BOATS (soft-fail boats) for overnight 24/5 coverage.
	tradesCtx, tradesCancel := context.WithTimeout(ctx, 30*time.Second)
	defer tradesCancel()
	trLookback := 30 * time.Minute
	anchor := end
	if last, ok := agg.LastBarTime(bars.TF1m); ok {
		anchor = last
	}
	startTr := anchor.Add(-trLookback)
	sipTr, errSIP := rest.Trades(tradesCtx, symbol, startTr, anchor, 10000, "sip")
	if errSIP != nil {
		log.Printf("backfill trades %s sip: %v", symbol, errSIP)
		if firstErr == nil {
			firstErr = errSIP
		}
	}
	boatsTr, errBoats := rest.Trades(tradesCtx, symbol, startTr, anchor, 10000, "boats")
	if errBoats != nil {
		log.Printf("backfill trades %s boats (overnight): %v", symbol, errBoats)
	}
	atr := alpaca.MergeTrades(sipTr, boatsTr)
	if len(atr) > 0 {
		replay := make([]bars.TradeReplay, len(atr))
		for i, tr := range atr {
			replay[i] = bars.TradeReplay{
				Price: float64(tr.Price), Size: float64(tr.Size), Timestamp: tr.Timestamp,
			}
		}
		agg.ReplayTrades(replay)
	}
	return firstErr
}
