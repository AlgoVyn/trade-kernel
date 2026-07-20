package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/bars"
	"trade-kernel/internal/session"
)

// overnightPollInterval is how often BOATS trades are polled while the
// overnight session is active. Thin overnight tape does not need sub-second
// cadence; 2 s keeps the forming candle visually live.
const overnightPollInterval = 2 * time.Second

// overnightPollTimeout bounds one REST trades fetch.
const overnightPollTimeout = 10 * time.Second

// overnightFetchPage is the per-page trade limit for poller fetches. Polled
// windows are seconds long, so one small page always suffices (no pagination).
const overnightFetchPage = 1000

// maxSeenTradeIDs bounds the dedupe set. Overnight tape is thin; 4096 covers
// many minutes of even an active symbol.
const maxSeenTradeIDs = 4096

// tradeFetcher fetches historical trades (BOATS feed) for the overnight
// poller. Abstracted from REST.Trades so tests can inject a fake.
type tradeFetcher func(ctx context.Context, symbol string, start, end time.Time, limit int) ([]alpaca.Trade, error)

// quoteFetcher fetches the latest quote (BOATS feed) for the overnight
// poller. Abstracted from REST.LatestQuote so tests can inject a fake.
type quoteFetcher func(ctx context.Context, symbol string) (alpaca.Quote, error)

// overnightFeed keeps the chart, volume, and last price live during the
// overnight session (20:00–04:00 ET). Alpaca's market-data websocket (SIP)
// does not stream overnight prints, so without polling the BOATS REST
// endpoint the UI freezes at whatever the startup backfill loaded.
//
// Polled trades are folded into the aggregator exactly like live-tape
// prints (Aggregator.OnTrade): the forming candle updates in place, new
// candles roll on bucket boundaries, and volume / last price / session
// VWAP advance.
type overnightFeed struct {
	agg      *bars.Aggregator
	sessions *session.Engine
	symbol   func() string
	fetch    tradeFetcher
	// fetchQuote refreshes the overnight NBBO each tick so extended-hours
	// order pricing (Builder.aggressivePrice) has a live quote to work
	// with; without it the quote cache is permanently empty overnight and
	// pricing falls back to the last trade, which is often older than the
	// stale-quote window on the thin overnight tape.
	fetchQuote quoteFetcher
	// backfillCursor carries the end time of the most recent backfill
	// (atomic.Value of time.Time). Trades at or before it are already
	// reflected in the aggregator via Load / ReplayTrades, so the poller
	// must never re-apply them (that would double-count volume).
	backfillCursor *atomic.Value

	mu     sync.Mutex
	curSym string
	cursor time.Time // high-water mark of fetched trade timestamps
	seen   map[int64]struct{}
	fifo   []int64
	active bool // activation already logged for this overnight stint
}

func newOvernightFeed(agg *bars.Aggregator, sessions *session.Engine, symbol func() string, fetch tradeFetcher, fetchQuote quoteFetcher, backfillCursor *atomic.Value) *overnightFeed {
	return &overnightFeed{
		agg:            agg,
		sessions:       sessions,
		symbol:         symbol,
		fetch:          fetch,
		fetchQuote:     fetchQuote,
		backfillCursor: backfillCursor,
		seen:           make(map[int64]struct{}),
	}
}

// run polls until ctx is cancelled. Only active in the Overnight session —
// SIP streams pre-market / regular / after-hours fine, and BOATS only
// covers overnight, so polling outside Overnight would at best be a no-op
// and at worst double-feed.
func (f *overnightFeed) run(ctx context.Context) {
	t := time.NewTicker(overnightPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if f.sessions.Current() != session.Overnight {
			f.mu.Lock()
			f.active = false
			f.mu.Unlock()
			continue
		}
		sym := f.symbol()
		if sym == "" {
			continue
		}
		f.pollOnce(ctx, sym)
	}
}

func (f *overnightFeed) pollOnce(ctx context.Context, sym string) {
	f.mu.Lock()
	if f.curSym != sym {
		// Symbol switch: drop all per-symbol state. The new symbol's
		// backfill re-seeds the shared cursor.
		f.curSym = sym
		f.cursor = time.Time{}
		f.seen = make(map[int64]struct{})
		f.fifo = nil
	}
	cursor := f.cursor
	if bc, ok := f.backfillCursor.Load().(time.Time); ok && bc.After(cursor) {
		cursor = bc
	}
	active := f.active
	f.mu.Unlock()

	start := cursor
	if start.IsZero() {
		// No backfill has completed yet (startup race). Load/ReplayTrades
		// will replace/reset every series when it lands, self-correcting
		// any overlap — so a short catch-up window is safe.
		start = time.Now().Add(-2 * time.Minute)
	}
	if !active {
		log.Printf("overnight: polling boats trades for %s (ws does not stream overnight)", sym)
		f.mu.Lock()
		f.active = true
		f.mu.Unlock()
	}

	pctx, cancel := context.WithTimeout(ctx, overnightPollTimeout)
	trades, err := f.fetch(pctx, sym, start, time.Now(), overnightFetchPage)
	cancel()
	if err != nil {
		log.Printf("overnight trades %s: %v", sym, err)
		return
	}

	// Refresh the NBBO cache too: the SIP websocket streams no overnight
	// quotes, so without this the quote cache is empty all session and
	// extended-hours order pricing has nothing fresh to work with.
	if f.fetchQuote != nil {
		qctx, qcancel := context.WithTimeout(ctx, overnightPollTimeout)
		q, qerr := f.fetchQuote(qctx, sym)
		qcancel()
		if qerr != nil {
			log.Printf("overnight quote %s: %v", sym, qerr)
		} else if q.BidPrice > 0 && q.AskPrice > 0 {
			f.agg.OnQuote(sym, float64(q.BidPrice), float64(q.AskPrice), q.Timestamp)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.curSym != sym {
		return // symbol switched mid-fetch; drop the batch
	}
	maxTS := cursor
	for _, tr := range trades {
		if tr.Timestamp.Before(cursor) {
			continue // older than the high-water mark
		}
		if tr.ID != 0 {
			if _, dup := f.seen[tr.ID]; dup {
				continue
			}
		}
		if tr.Timestamp.After(maxTS) {
			maxTS = tr.Timestamp
		}
		f.agg.OnTrade(tr.Symbol, float64(tr.Price), float64(tr.Size), tr.Timestamp)
		if tr.ID != 0 {
			f.seen[tr.ID] = struct{}{}
			f.fifo = append(f.fifo, tr.ID)
		}
	}
	f.cursor = maxTS
	// Bound the dedupe set FIFO-style.
	for len(f.fifo) > maxSeenTradeIDs {
		delete(f.seen, f.fifo[0])
		f.fifo = f.fifo[1:]
	}
}
