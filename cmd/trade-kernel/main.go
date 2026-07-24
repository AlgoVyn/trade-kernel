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
	exec := execution.NewRESTExecutor(rest, builder, sessions.Current, cfg.RegularTIF())

	checker := risk.NewChecker(risk.Limits{
		MaxOrderQty:    cfg.Limits.MaxOrderQty,
		MaxPositionQty: cfg.Limits.MaxPositionQty,
		Debounce:       cfg.Debounce(),
	}, store, nil)
	// Include resting open-order exposure in projected max position.
	checker.SetWorkingLookup(store)

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
	// block TUI startup indefinitely; the periodic ticker will retry.
	// Rate-limit repeated identical errors (e.g. inventory inconsistent)
	// so a stuck symbol does not spam the log every tick.
	var (
		realizedLogMu     sync.Mutex
		lastRealizedErr   string
		lastRealizedLogAt time.Time
	)
	const realizedLogCooldown = 5 * time.Minute
	// FILL history pagination is slower than account reconcile; bound each
	// walk so a multi-page fetch cannot hang the ticker or post-fill kick.
	const realizedTickBudget = 60 * time.Second
	// Periodic heal when no fills fire (long-held marks / delayed activities).
	// Live fills update rday/rwk immediately via the trading WS path.
	const realizedTickEvery = 30 * time.Second
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
		t := time.NewTicker(realizedTickEvery)
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
	// liveCursor holds a symbol-scoped high-water (backfillHighWater) of the
	// most recent successful backfill. The overnight poller reads it so it
	// never re-applies trades that Load / ReplayTrades already folded in,
	// and never inherits another symbol's wall clock after a switch.
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
	// Debounced REST realized refresh after fills: WS path updates rday/rwk
	// immediately from trade_updates; this heals the fill cache when the
	// activities endpoint catches up (and covers fills that lack event qty).
	var (
		realizedKickMu   sync.Mutex
		realizedKickAt   time.Time
		realizedKickPend bool
	)
	const realizedKickDelay = 2 * time.Second
	// Trailing-edge debounce: kicks only stamp realizedKickAt while a worker
	// is pending. The worker keeps pend true through the wait+refresh, then
	// re-checks kickAt so a kick that lands after wait<=0 (or during refresh)
	// is not lost until the 30s ticker.
	kickRealizedRefresh := func() {
		realizedKickMu.Lock()
		realizedKickAt = time.Now()
		if realizedKickPend {
			realizedKickMu.Unlock()
			return
		}
		realizedKickPend = true
		realizedKickMu.Unlock()
		go func() {
			for {
				realizedKickMu.Lock()
				wait := realizedKickDelay - time.Since(realizedKickAt)
				// Leave pend true while waiting/refreshing so concurrent kicks
				// only update kickAt (they must not spawn a second worker).
				realizedKickMu.Unlock()
				if wait > 0 {
					time.Sleep(wait)
					continue
				}
				rctx, cancel := context.WithTimeout(ctx, realizedTickBudget)
				if err := store.RefreshRealizedPnL(rctx, rest); err != nil {
					logRealizedErr("realized PnL (post-fill)", err)
				}
				cancel()
				realizedKickMu.Lock()
				wait = realizedKickDelay - time.Since(realizedKickAt)
				if wait > 0 {
					// Kick arrived during refresh (or after wait hit 0); loop.
					realizedKickMu.Unlock()
					continue
				}
				realizedKickPend = false
				realizedKickMu.Unlock()
				return
			}
		}()
	}

	tws := alpaca.NewTradingWS(cfg.APIKeyID, cfg.APISecretKey, !cfg.Live())
	tws.OnUpdate = func(u alpaca.TradeUpdate) {
		store.ApplyUpdate(u)
		if u.Event == "fill" || u.Event == "partial_fill" {
			kickRealizedRefresh()
		}
	}
	// Initial reconcile happens synchronously below before the TUI starts;
	// OnReconnect re-syncs from REST only on a real drop+reconnect.
	tws.OnReconnect = func() {
		go func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := store.Refresh(rctx, rest); err != nil {
				log.Printf("reconcile after trading ws reconnect: %v", err)
			}
			kickRealizedRefresh()
		}()
	}
	tws.OnError = func(err error) { log.Printf("trading ws: %v", err) }
	go tws.Run(ctx)

	// --- Symbol switching (re-subscribe + backfill) ---
	// Transactional under backfillMu: ResetMarket / activeSymbol / backfill
	// never interleave with runBackfill. Gen is bumped under the lock so a
	// concurrent scheduleBackfill Add cannot false-supersede a switch that
	// has not yet taken the lock. Reconnect may still bump gen while we hold
	// the lock; that only queues a later force catch-up after we unlock and
	// must not abort work already past the gate.
	// switchSymbol returns the committed active symbol after success or
	// rollback so the UI can re-sync m.symbol even when the request fails.
	switchSymbol := func(symbol string) (active string, err error) {
		backfillMu.Lock()
		defer backfillMu.Unlock()

		prev, _ := activeSymbol.Load().(string)
		if prev == symbol {
			return symbol, nil
		}
		// Invalidate in-flight runBackfill generations. Do not re-check gen
		// after Add: scheduleBackfill may bump while we hold the lock.
		_ = backfillGen.Add(1)

		// Pause ingest for both symbols while the subscription moves.
		// Clear overnight high-water so the poller does not inherit prev's clock.
		activeSymbol.Store("")
		liveCursor.Store(backfillHighWater{})
		if err := mws.SetSymbol(symbol); err != nil {
			activeSymbol.Store(prev)
			if prev != "" {
				if err2 := mws.SetSymbol(prev); err2 != nil {
					activeSymbol.Store("")
					return "", fmt.Errorf("subscribe %s: %w; restore %s: %v", symbol, err, prev, err2)
				}
			}
			return prev, err
		}
		// ResetMarket clears all TF rings + any foreign backfill buffer so
		// partial Load / old-symbol prints cannot mix into the new book.
		agg.ResetMarket()
		agg.ResetVWAP()
		// Arm the live-trade buffer before re-enabling ingest. Otherwise
		// OnTrade accepts the new symbol, folds into rings, and Load wipes
		// those prints without them ever entering the buffer. backfill's
		// BeginBackfill is idempotent and keeps this pre-armed buffer.
		agg.BeginBackfill()
		activeSymbol.Store(symbol)
		if err := backfill(context.Background(), rest, agg, symbol, liveCursor); err != nil {
			// Critical failure (no usable TF1m): roll back filter + book so
			// the UI can re-sync to prev. ResetMarket clears any partial
			// coarser TFs that Load may have applied for the new symbol.
			activeSymbol.Store(prev)
			agg.ResetMarket()
			agg.ResetVWAP()
			liveCursor.Store(backfillHighWater{})
			if prev != "" {
				if err2 := mws.SetSymbol(prev); err2 != nil {
					// Do not claim alignment when subscribe restore failed.
					activeSymbol.Store("")
					return "", fmt.Errorf("backfill %s: %w; restore subscribe %s: %v", symbol, err, prev, err2)
				}
				// Async re-seed previous symbol. scheduleBackfill bumps gen —
				// safe after we finish; caller still holds the lock until
				// return, so the async path waits.
				scheduleBackfill(prev, true)
			}
			return prev, err
		}
		// Stamp debounce map for consistency with runBackfill so a later
		// non-forced schedule does not immediately re-fetch the same symbol.
		stampBackfill(symbol, time.Now())
		prefetchElig(symbol)
		return symbol, nil
	}

	// Warm eligibility for the default symbol before the first keystroke.
	prefetchElig(cfg.DefaultSymbol)

	// LivePosition: REST-first position lookup for flatten/panic; the UI falls
	// back to the local book only when REST errors (see resolvePositionQty).
	// Use state.SignedPositionQty so already-negative short REST qty is not
	// double-negated into a false long (would exit the wrong side).
	livePosition := func(ctx context.Context, symbol string) (float64, error) {
		p, err := rest.Position(ctx, symbol)
		if err != nil {
			return 0, err
		}
		if p == nil {
			return 0, nil
		}
		return state.SignedPositionQty(*p), nil
	}

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
		LivePosition: livePosition,
		Paper:        !cfg.Live(),
	}
	p := tea.NewProgram(ui.NewModel(deps), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
	}
	stop()
}

// backfillHighWater is the overnight poller's high-water mark after a
// successful backfill. Symbol-scoped so a switch cannot inherit another
// symbol's clock and skip the new tape until backfill finishes.
type backfillHighWater struct {
	Symbol string
	At     time.Time
}

// backfill loads historical bars for the REST-backed timeframes, then
// fetches recent trades and replays them into the sub-minute timeframes
// (1s/5s/15s) — which the bars endpoint doesn't serve.
//
// 24/5 coverage: SIP feed supplies regular + pre/after-hours bars; BOATS
// feed supplies the overnight session (20:00–04:00 ET). The two series are
// merged by timestamp so the chart shows the full trading day.
//
// Live OnTrade prints that arrive during the multi-second REST window are
// buffered (Aggregator.BeginBackfill / EndBackfillAfterReplay) so Load cannot
// wipe them; sub-minute applies only prints after the REST trade window and
// minute+/VWAP only prints after the REST bar window (avoids overnight BOATS
// double-count against Load). liveCursor is stamped with max(reqEnd, tradeEnd)
// for the active symbol (not a late wall stamp that opens a poller hole).
//
// Returns an error only on critical failure (TF1m unusable). Soft SIP errors
// on coarser TFs or trades are logged and do not fail the switch.
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
	// Buffer live OnTrade for the whole fetch+Load+Replay window so multi-second
	// REST work cannot leave permanent volume holes when Load resets rings.
	// Begin before capturing reqEnd so prints in the arm→reqEnd window are
	// buffered (not applied then wiped by Load). Drain with EndBackfillAfterReplay
	// so REST windows are not double-counted. Idempotent if switchSymbol already
	// pre-armed the buffer before activeSymbol.Store.
	agg.BeginBackfill()
	// Bar-request end: also gates minute+/VWAP drain of the live buffer.
	reqEnd := time.Now()
	var tradeEnd time.Time
	defer func() {
		if !tradeEnd.IsZero() {
			agg.EndBackfillAfterReplay(tradeEnd, reqEnd)
		} else {
			agg.EndBackfill()
		}
	}()

	type tfResult struct {
		tf       bars.TF
		api      string
		ab       []alpaca.Bar
		errSIP   error
		errBoats error
		nSIP     int
		nBoats   int
	}
	results := make([]tfResult, len(windows))
	var wg sync.WaitGroup
	// Parallelize TF fetches with a small semaphore so free/paper rate limits
	// are less likely to 429 when all five timeframes fire at once. SIP+BOATS
	// for a given TF stay sequential inside each worker.
	const maxTFFetchers = 3
	sem := make(chan struct{}, maxTFFetchers)
	for i, w := range windows {
		wg.Add(1)
		go func(i int, w struct {
			tf       bars.TF
			api      string
			lookback time.Duration
		}) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := reqEnd.Add(-w.lookback)
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			sip, errSIP := rest.Bars(cctx, symbol, w.api, start, reqEnd, 10000, "sip")
			cancel()
			cctx, cancel = context.WithTimeout(ctx, 30*time.Second)
			boats, errBoats := rest.Bars(cctx, symbol, w.api, start, reqEnd, 10000, "boats")
			cancel()
			results[i] = tfResult{
				tf: w.tf, api: w.api,
				ab: alpaca.MergeBars(sip, boats),
				errSIP: errSIP, errBoats: errBoats,
				nSIP: len(sip), nBoats: len(boats),
			}
		}(i, w)
	}
	wg.Wait()

	// Apply TF1m first so session VWAP seed and custom rebuilds see 1m history
	// before coarser TFs (Load only seeds VWAP from TF1m). Built from TF tags
	// so reordering windows cannot silently break the seed order.
	order := make([]int, 0, len(windows))
	for i, w := range windows {
		if w.tf == bars.TF1m {
			order = append(order, i)
		}
	}
	for i, w := range windows {
		if w.tf != bars.TF1m {
			order = append(order, i)
		}
	}

	// Apply TF1m first; on critical failure (SIP hard-failed, nothing loaded)
	// return before coarser Loads or trade replay so reconnect cannot leave a
	// mixed multi-TF book (new 5m/15m/… over unusable/missing 1m).
	var tf1mLoaded bool
	var tf1mErr error
	loadTF := func(r tfResult) {
		if r.errSIP != nil {
			log.Printf("backfill %s %s sip: %v", symbol, r.api, r.errSIP)
			if r.tf == bars.TF1m && tf1mErr == nil {
				tf1mErr = r.errSIP
			}
		}
		if r.errBoats != nil {
			log.Printf("backfill %s %s boats (overnight): %v", symbol, r.api, r.errBoats)
		}
		if len(r.ab) == 0 {
			if r.errSIP != nil {
				return
			}
			log.Printf("backfill %s %s: empty (sip=%d boats=%d)", symbol, r.api, r.nSIP, r.nBoats)
			return
		}
		if r.nBoats > 0 {
			log.Printf("backfill %s %s: %d bars (sip=%d + boats overnight=%d → merged=%d)",
				symbol, r.api, len(r.ab), r.nSIP, r.nBoats, len(r.ab))
		}
		hist := make([]bars.Bar, len(r.ab))
		for j, b := range r.ab {
			hist[j] = bars.Bar{
				Start: b.Timestamp, Open: float64(b.Open), High: float64(b.High),
				Low: float64(b.Low), Close: float64(b.Close), Volume: float64(b.Volume),
				VWAP: float64(b.VWAP),
			}
		}
		agg.Load(r.tf, hist)
		if r.tf == bars.TF1m {
			tf1mLoaded = true
		}
	}
	for _, i := range order {
		if results[i].tf != bars.TF1m {
			continue
		}
		loadTF(results[i])
	}
	// Critical path: TF1m SIP hard-failed with nothing loaded. Empty history
	// without error (halted/new listing) is still a successful switch.
	if !tf1mLoaded && tf1mErr != nil {
		return fmt.Errorf("backfill %s 1m: %w", symbol, tf1mErr)
	}
	for _, i := range order {
		if results[i].tf == bars.TF1m {
			continue
		}
		loadTF(results[i])
	}

	// Sub-minute TFs: replay recent trades. Anchor to newest 1m bar when present.
	tradesCtx, tradesCancel := context.WithTimeout(ctx, 30*time.Second)
	defer tradesCancel()
	trLookback := 30 * time.Minute
	// Use wall-now for the trade window end so partial current minute is covered.
	// Captured for EndBackfillAfterReplay (sub-minute cutoff).
	tradeEnd = time.Now()
	anchor := tradeEnd
	if last, ok := agg.LastBarTime(bars.TF1m); ok {
		anchor = last
	}
	startTr := anchor.Add(-trLookback)
	var (
		sipTr, boatsTr   []alpaca.Trade
		errSIP, errBoats error
	)
	var twg sync.WaitGroup
	twg.Add(2)
	go func() {
		defer twg.Done()
		var e error
		sipTr, e = rest.Trades(tradesCtx, symbol, startTr, tradeEnd, 2000, "sip")
		errSIP = e
	}()
	go func() {
		defer twg.Done()
		var e error
		boatsTr, e = rest.Trades(tradesCtx, symbol, startTr, tradeEnd, 2000, "boats")
		errBoats = e
	}()
	twg.Wait()
	if errSIP != nil {
		log.Printf("backfill trades %s sip: %v", symbol, errSIP)
	}
	if errBoats != nil {
		log.Printf("backfill trades %s boats (overnight): %v", symbol, errBoats)
	}
	atr := alpaca.MergeTrades(sipTr, boatsTr)
	replay := make([]bars.TradeReplay, len(atr))
	for i, tr := range atr {
		replay[i] = bars.TradeReplay{
			Price: float64(tr.Price), Size: float64(tr.Size), Timestamp: tr.Timestamp,
		}
	}
	agg.ReplayTrades(replay)

	// High-water for overnight poller: max(reqEnd, tradeEnd) — REST already
	// covers that window; buffered live prints cover post-reqEnd overlap.
	// Avoid stamping a later wall clock that opens a poller hole if no live
	// OnTrade arrived during the multi-second fetch.
	hw := reqEnd
	if tradeEnd.After(hw) {
		hw = tradeEnd
	}
	if liveCursor != nil {
		liveCursor.Store(backfillHighWater{Symbol: symbol, At: hw})
	}
	return nil
}
