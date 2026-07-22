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
	// Account/positions/open orders every 5s. Realized day/week PnL comes
	// from closed-order / FILL history (heavier) on a slower cadence.
	if err := store.Refresh(ctx, rest); err != nil {
		log.Printf("initial reconcile: %v", err)
	}
	// Bound the initial FILL history walk so multi-page accounts do not
	// block TUI startup indefinitely; the 60s ticker will retry.
	// Rate-limit repeated identical errors (e.g. inventory inconsistent)
	// so a stuck symbol does not spam the log every tick.
	var (
		realizedLogMu     sync.Mutex
		lastRealizedErr   string
		lastRealizedLogAt time.Time
	)
	const realizedLogCooldown = 5 * time.Minute
	logRealizedErr := func(prefix string, err error) {
		if err == nil {
			return
		}
		msg := err.Error()
		realizedLogMu.Lock()
		defer realizedLogMu.Unlock()
		if msg == lastRealizedErr && time.Since(lastRealizedLogAt) < realizedLogCooldown {
			return
		}
		lastRealizedErr = msg
		lastRealizedLogAt = time.Now()
		log.Printf("%s: %v", prefix, err)
	}
	// Multi-page FILL history can exceed a short budget on active accounts.
	// Run the initial walk asynchronously so market WS, trading WS, and the
	// TUI start immediately — rday/rwk is display-only and may stay blank
	// until the first successful sample (same as a cold/stale status bar).
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		if err := store.RefreshRealizedPnL(rctx, rest); err != nil {
			logRealizedErr("initial realized PnL", err)
		}
	}()
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
		// FILL history pagination is slower than account reconcile; 60s is
		// enough for rday/rwk while keeping rate-limit headroom. Bound each
		// tick so a multi-page walk cannot block the goroutine indefinitely.
		const realizedTickBudget = 60 * time.Second
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				func() {
					rctx, cancel := context.WithTimeout(ctx, realizedTickBudget)
					defer cancel()
					if err := store.RefreshRealizedPnL(rctx, rest); err != nil {
						logRealizedErr("realized PnL", err)
					}
				}()
			}
		}
	}()

	// --- Market data ---
	activeSymbol := atomic.Value{}
	activeSymbol.Store(cfg.DefaultSymbol)

	// Serialise backfills and ignore superseded generations so reconnect
	// + symbol switch cannot interleave Loads for different symbols.
	var (
		backfillMu       sync.Mutex
		backfillGen      atomic.Uint64
		lastBackfillTime = make(map[string]time.Time)
	)
	const backfillDebounce = 5 * time.Second
	// stampBackfill records a successful backfill and prunes expired debounce
	// entries. Caller must hold backfillMu.
	stampBackfill := func(symbol string, now time.Time) {
		lastBackfillTime[symbol] = now
		for k, t := range lastBackfillTime {
			if now.Sub(t) >= backfillDebounce {
				delete(lastBackfillTime, k)
			}
		}
	}
	// liveCursor holds the end time (time.Time) of the most recent
	// backfill. The overnight poller reads it so it never re-applies
	// trades that Load / ReplayTrades already folded into the aggregator.
	liveCursor := &atomic.Value{}
	runBackfill := func(symbol string, gen uint64, force bool) {
		backfillMu.Lock()
		defer backfillMu.Unlock()
		if backfillGen.Load() != gen {
			return
		}
		if activeSymbol.Load().(string) != symbol {
			return
		}
		// Debounce only non-forced paths (e.g. duplicate OnInitial). Reconnect
		// always forces so a drop within 5s of the last success still catch-ups.
		if !force {
			if last, ok := lastBackfillTime[symbol]; ok && time.Since(last) < backfillDebounce {
				return
			}
		}
		if err := backfill(context.Background(), rest, agg, symbol, liveCursor); err != nil {
			log.Printf("backfill %s: %v", symbol, err)
			return
		}
		// Stamp only after success so a failed backfill does not block retries.
		stampBackfill(symbol, time.Now())
	}
	scheduleBackfill := func(symbol string, force bool) {
		gen := backfillGen.Add(1)
		go runBackfill(symbol, gen, force)
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
		scheduleBackfill(mws.Symbol(), false)
	}
	mws.OnReconnect = func() {
		log.Printf("market ws reconnected; backfilling %s", mws.Symbol())
		scheduleBackfill(mws.Symbol(), true)
	}
	mws.OnError = func(err error) { log.Printf("market ws: %v", err) }
	_ = mws.SetSymbol(cfg.DefaultSymbol)
	go mws.Run(ctx)

	// --- Overnight live data ---
	// The SIP websocket does not stream overnight (20:00–04:00 ET) prints;
	// poll the BOATS REST trades endpoint during Overnight instead so the
	// forming candle, new candles, volume, and the last price keep updating.
	onFeed := newOvernightFeed(agg, sessions,
		func() string { return activeSymbol.Load().(string) },
		func(ctx context.Context, symbol string, start, end time.Time, limit int) ([]alpaca.Trade, error) {
			return rest.Trades(ctx, symbol, start, end, limit, "boats")
		},
		func(ctx context.Context, symbol string) (alpaca.Quote, error) {
			return rest.LatestQuote(ctx, symbol, "boats")
		},
		liveCursor)
	go onFeed.run(ctx)

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
		if err := backfill(context.Background(), rest, agg, symbol, liveCursor); err != nil {
			return err
		}
		// Stamp debounce map for consistency with runBackfill so a later
		// non-forced schedule does not immediately re-fetch the same symbol.
		stampBackfill(symbol, time.Now())
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
//
// liveCursor (when non-nil) receives the fetch end time once the trades
// replay has landed; the overnight poller uses it as a high-water mark so
// it never re-applies trades already folded into the aggregator here.
func backfill(ctx context.Context, rest *alpaca.REST, agg *bars.Aggregator, symbol string, liveCursor *atomic.Value) error {
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
	// charts aren't blank at startup / on symbol switch. Anchor the window
	// start to the newest backfilled 1m bar's time (a real trading time)
	// rather than time.Now() — otherwise a launch outside market hours
	// (weekend, holiday, pre-market) fetches an empty window and sub-minute
	// charts stay blank. The window ends at now so the partial current
	// minute's prints also land in the sub-minute rings; trades only exist
	// up to the last print, so a far-future end costs nothing.
	// SIP + BOATS (soft-fail boats) for overnight 24/5 coverage.
	// Page limit 2000 keeps each response well under the 1 MB read cap —
	// 30 min of an active symbol at 10000/page overflows it.
	tradesCtx, tradesCancel := context.WithTimeout(ctx, 30*time.Second)
	defer tradesCancel()
	trLookback := 30 * time.Minute
	anchor := end
	if last, ok := agg.LastBarTime(bars.TF1m); ok {
		anchor = last
	}
	startTr := anchor.Add(-trLookback)
	sipTr, errSIP := rest.Trades(tradesCtx, symbol, startTr, end, 2000, "sip")
	if errSIP != nil {
		log.Printf("backfill trades %s sip: %v", symbol, errSIP)
		if firstErr == nil {
			firstErr = errSIP
		}
	}
	boatsTr, errBoats := rest.Trades(tradesCtx, symbol, startTr, end, 2000, "boats")
	if errBoats != nil {
		log.Printf("backfill trades %s boats (overnight): %v", symbol, errBoats)
	}
	atr := alpaca.MergeTrades(sipTr, boatsTr)
	// Always ReplayTrades (even empty): resets 1s/5s/15s and rebuilds any
	// 1s-sourced custom TF so reconnect/symbol switch cannot keep stale rings.
	replay := make([]bars.TradeReplay, len(atr))
	for i, tr := range atr {
		replay[i] = bars.TradeReplay{
			Price: float64(tr.Price), Size: float64(tr.Size), Timestamp: tr.Timestamp,
		}
	}
	agg.ReplayTrades(replay)
	// Publish the high-water mark for the overnight poller: trades at or
	// before end are now reflected in the aggregator (bars Load covers the
	// current partial minute; replay covers the sub-minute rings), so the
	// poller must only apply strictly newer prints.
	if liveCursor != nil {
		liveCursor.Store(end)
	}
	return firstErr
}
