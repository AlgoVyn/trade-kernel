// Package bars aggregates a trade stream into OHLCV bars at multiple
// resolutions, maintaining incremental SMA/EMA/session-VWAP alongside.
// All state lives behind one mutex; buffers are preallocated rings.
package bars

import (
	"math"
	"sync"
	"time"

	"trade-kernel/internal/indicators"
	"trade-kernel/internal/session"
)

// TF is a bar resolution.
type TF int

const (
	TF1s TF = iota
	TF5s
	TF15s
	TF1m
	TF5m
	TF15m
	TF1h
	TF1d
	numTF
)

var tfDur = [numTF]time.Duration{
	time.Second, 5 * time.Second, 15 * time.Second,
	time.Minute, 5 * time.Minute, 15 * time.Minute,
	time.Hour, 24 * time.Hour,
}

var tfName = [numTF]string{"1s", "5s", "15s", "1m", "5m", "15m", "1h", "1d"}

// String returns the short name ("1m", "1h", ...).
func (t TF) String() string { return tfName[t] }

// All lists every timeframe in increasing order.
func All() []TF {
	return []TF{TF1s, TF5s, TF15s, TF1m, TF5m, TF15m, TF1h, TF1d}
}

// ParseTF converts "1s"/"5m"/"1h"/"1d" etc. to a TF.
func ParseTF(s string) (TF, bool) {
	for i, n := range tfName {
		if n == s {
			return TF(i), true
		}
	}
	return 0, false
}

// ChartTFs are the resolutions selectable with Tab, in cycle order.
func ChartTFs() []TF {
	return []TF{TF1m, TF5m, TF15m, TF1h, TF1d, TF1s, TF5s, TF15s}
}

// Bar is one aggregated OHLCV candle.
type Bar struct {
	Start  time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

func newBar(start time.Time, price, vol float64) Bar {
	return Bar{Start: start, Open: price, High: price, Low: price, Close: price, Volume: vol}
}

func (b *Bar) add(price, vol float64) {
	if price > b.High {
		b.High = price
	}
	if price < b.Low {
		b.Low = price
	}
	b.Close = price
	b.Volume += vol
}

// ring is a fixed-capacity circular buffer of bars with parallel
// per-bar indicator values (NaN until ready).
type ring struct {
	bars        []Bar
	sma, ema    []float64
	vwapAtClose []float64
	start       int
	count       int
}

func newRing(cap int) *ring {
	return &ring{
		bars:        make([]Bar, cap),
		sma:         make([]float64, cap),
		ema:         make([]float64, cap),
		vwapAtClose: make([]float64, cap),
	}
}

func (r *ring) push(b Bar, smaV, emaV, vwapV float64) {
	if r.count < len(r.bars) {
		i := (r.start + r.count) % len(r.bars)
		r.set(i, b, smaV, emaV, vwapV)
		r.count++
		return
	}
	r.set(r.start, b, smaV, emaV, vwapV)
	r.start = (r.start + 1) % len(r.bars)
}

func (r *ring) set(i int, b Bar, smaV, emaV, vwapV float64) {
	r.bars[i] = b
	r.sma[i] = smaV
	r.ema[i] = emaV
	r.vwapAtClose[i] = vwapV
}

func (r *ring) at(i int) int { return (r.start + i) % len(r.bars) }

func (r *ring) last() int { return r.at(r.count - 1) }

// series holds the ring plus forming-bar and indicator state for one TF.
type series struct {
	ring    *ring
	forming Bar
	open    bool // forming is valid
	sma     *indicators.SMA
	ema     *indicators.EMA
}

// BarEvent is emitted when a bar closes.
type BarEvent struct {
	TF  TF
	Bar Bar
}

const ringCap = 2048

// Snapshot is a render-ready copy of recent bars and indicator overlays.
// The final element is the live forming bar with Peek'd indicators.
type Snapshot struct {
	Bars []Bar
	SMA  []float64
	EMA  []float64
	VWAP []float64 // session VWAP value at each bar's close
}

// Aggregator builds bars for all timeframes from one trade stream.
// All methods are safe for concurrent use.
type Aggregator struct {
	mu sync.Mutex

	series [numTF]*series
	smaN   int
	emaN   int

	vwap indicators.VWAP // session-anchored, fed per trade

	lastTradePrice float64
	lastTradeAt    time.Time
	bid, ask       float64
	quoteAt        time.Time

	events chan BarEvent
}

// NewAggregator creates an Aggregator with the given indicator periods.
func NewAggregator(smaPeriod, emaPeriod int) *Aggregator {
	a := &Aggregator{events: make(chan BarEvent, 64), smaN: smaPeriod, emaN: emaPeriod}
	for tf := TF(0); tf < numTF; tf++ {
		a.series[tf] = &series{
			ring: newRing(ringCap),
			sma:  indicators.NewSMA(smaPeriod),
			ema:  indicators.NewEMA(emaPeriod),
		}
	}
	return a
}

// Events returns the bar-close event channel (lossy if unconsumed).
func (a *Aggregator) Events() <-chan BarEvent { return a.events }

// bucket returns the bar start time for ts at tf. Intraday buckets align
// on UTC boundaries, which coincide with ET boundaries (whole-hour
// offset). Daily buckets anchor at the 20:00 ET overnight open.
func bucket(tf TF, ts time.Time) time.Time {
	if tf != TF1d {
		return ts.UTC().Truncate(tfDur[tf])
	}
	t := ts.In(session.Location())
	if t.Hour() >= 20 {
		return time.Date(t.Year(), t.Month(), t.Day(), 20, 0, 0, 0, session.Location())
	}
	prev := t.AddDate(0, 0, -1)
	return time.Date(prev.Year(), prev.Month(), prev.Day(), 20, 0, 0, 0, session.Location())
}

// OnTrade folds one trade into every timeframe. Must not block.
func (a *Aggregator) OnTrade(symbol string, price, size float64, ts time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastTradePrice = price
	a.lastTradeAt = ts
	a.vwap.Update(price, size)
	for tf := TF(0); tf < numTF; tf++ {
		a.aggregateInto(tf, price, size, ts)
	}
}

// ReplayTrades feeds a historical trade sequence into the sub-minute
// timeframes (1s/5s/15s) only. Used to backfill those TFs from the REST
// trades endpoint, since the bars endpoint doesn't serve sub-minute
// resolutions. Trades must be in ascending timestamp order.
//
// It deliberately does NOT touch the 1m+ TFs (already backfilled from the
// bars endpoint) nor the session VWAP / last-trade cache (live-only
// state) — replaying into those would double-count volume and overwrite
// the live edge with stale prices.
func (a *Aggregator) ReplayTrades(trades []TradeReplay) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, tr := range trades {
		for _, tf := range []TF{TF1s, TF5s, TF15s} {
			a.aggregateInto(tf, tr.Price, tr.Size, tr.Timestamp)
		}
	}
}

// TradeReplay is a historical trade to replay into the aggregator. A
// narrow type (not alpaca.Trade) keeps the bars package free of the
// alpaca dependency.
type TradeReplay struct {
	Price     float64
	Size      float64
	Timestamp time.Time
}

// aggregateInto folds one trade into a single timeframe. Caller holds the
// mutex. Handles bucket rollover and late/out-of-order trades.
func (a *Aggregator) aggregateInto(tf TF, price, size float64, ts time.Time) {
	s := a.series[tf]
	b := bucket(tf, ts)
	switch {
	case !s.open:
		s.forming = newBar(b, price, size)
		s.open = true
	case b.Equal(s.forming.Start):
		s.forming.add(price, size)
	case b.After(s.forming.Start):
		a.closeBar(tf, s)
		s.forming = newBar(b, price, size)
		s.open = true
	default:
		// Late/out-of-order trade into an older bucket: adjust H/L/V
		// if the bar is still in the ring. Close is left alone.
		r := s.ring
		for n := 0; n < r.count; n++ {
			i := (r.start + r.count - 1 - n + len(r.bars)) % len(r.bars)
			if r.bars[i].Start.Before(b) {
				break
			}
			if r.bars[i].Start.Equal(b) {
				if price > r.bars[i].High {
					r.bars[i].High = price
				}
				if price < r.bars[i].Low {
					r.bars[i].Low = price
				}
				r.bars[i].Volume += size
				break
			}
		}
	}
}

func (a *Aggregator) closeBar(tf TF, s *series) {
	b := s.forming
	smaV := s.sma.Update(b.Close)
	emaV := s.ema.Update(b.Close)
	s.ring.push(b, smaV, emaV, a.vwap.Value())
	select {
	case a.events <- BarEvent{TF: tf, Bar: b}:
	default:
	}
	s.open = false
}

// OnQuote caches the latest NBBO.
func (a *Aggregator) OnQuote(symbol string, bid, ask float64, ts time.Time) {
	a.mu.Lock()
	a.bid, a.ask, a.quoteAt = bid, ask, ts
	a.mu.Unlock()
}

// LatestQuote returns the cached NBBO and its timestamp.
func (a *Aggregator) LatestQuote() (bid, ask float64, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bid, a.ask, a.quoteAt
}

// LatestTrade returns the last trade price and timestamp.
func (a *Aggregator) LatestTrade() (price float64, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastTradePrice, a.lastTradeAt
}

// SessionVWAP returns the live session-anchored VWAP.
func (a *Aggregator) SessionVWAP() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.vwap.Value()
}

// ResetVWAP clears the session VWAP accumulator. Called by the wiring
// layer on session transitions per the configured anchor.
func (a *Aggregator) ResetVWAP() {
	a.mu.Lock()
	a.vwap.Reset()
	a.mu.Unlock()
}

// ResetMarket clears the cached last trade and NBBO. Called on symbol
// switch so the status bar and order builder don't price off the previous
// symbol's quote until a new tick for the new symbol arrives.
func (a *Aggregator) ResetMarket() {
	a.mu.Lock()
	a.lastTradePrice = 0
	a.lastTradeAt = time.Time{}
	a.bid, a.ask = 0, 0
	a.quoteAt = time.Time{}
	a.mu.Unlock()
}

// Load replaces one timeframe's history with backfilled bars (e.g. from
// the REST bars endpoint) and resets its indicator state accordingly.
func (a *Aggregator) Load(tf TF, hist []Bar) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.series[tf]
	s.ring = newRing(ringCap)
	s.sma = indicators.NewSMA(a.smaN)
	s.ema = indicators.NewEMA(a.emaN)
	s.open = false
	for _, b := range hist {
		smaV := s.sma.Update(b.Close)
		emaV := s.ema.Update(b.Close)
		s.ring.push(b, smaV, emaV, math.NaN())
	}
}

// Snapshot returns up to n bars for rendering.
//
// offset shifts the view back from the live edge:
//
//	- offset == 0: the live edge. Includes the forming bar (with Peek'd
//	  indicator values and the live session VWAP) at the right edge.
//	- offset  > 0: closed bars only. offset counts how many bars back from
//	  the live edge the right edge of the view sits: offset=1 shows the
//	  newest closed bar at the right edge, offset=2 the next one back, etc.
//	  The forming bar is never included when panned back. Closed bars carry
//	  their own frozen sma/ema/vwapAtClose captured at bar close, so panned
//	  views are self-contained.
//
// offset is clamped to the available closed history; an offset past the
// oldest retained bar returns the oldest available bars.
func (a *Aggregator) Snapshot(tf TF, n, offset int) Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 {
		return Snapshot{}
	}
	s := a.series[tf]
	r := s.ring
	if r.count == 0 && !s.open {
		return Snapshot{}
	}

	if offset < 0 {
		offset = 0
	}
	// offset=0 is the live edge (forming bar visible if open). Any offset>0
	// skips the forming bar and steps back through closed bars.
	if offset == 0 && s.open {
		// Live edge: r.count closed bars + the forming bar, newest at right.
		count := r.count + 1
		if count > n {
			count = n
		}
		out := Snapshot{
			Bars: make([]Bar, count),
			SMA:  make([]float64, count),
			EMA:  make([]float64, count),
			VWAP: make([]float64, count),
		}
		closed := count - 1 // reserve last slot for the forming bar
		if closed > r.count {
			closed = r.count
		}
		if closed < 0 {
			closed = 0
		}
		startIdx := r.count - closed
		for i := 0; i < closed; i++ {
			j := r.at(startIdx + i)
			out.Bars[i] = r.bars[j]
			out.SMA[i] = r.sma[j]
			out.EMA[i] = r.ema[j]
			out.VWAP[i] = r.vwapAtClose[j]
		}
		f := s.forming
		i := count - 1
		out.Bars[i] = f
		out.SMA[i] = s.sma.Peek(f.Close)
		out.EMA[i] = s.ema.Peek(f.Close)
		out.VWAP[i] = a.vwap.Value()
		return out
	}

	// Panned back (offset > 0), or offset==0 with no forming bar: show
	// closed bars only. offset counts bars back from the live edge:
	//	- forming bar open:  offset=1 → newest closed at right edge;
	//	  offset=2 → one further back, etc. (offset=0 is handled above.)
	//	- no forming bar:    offset=0 → newest closed at right edge;
	//	  offset=1 → one further back, etc.
	back := offset
	if s.open {
		back = offset - 1 // first step just drops the forming bar
	}
	if back < 0 {
		back = 0
	}
	if back >= r.count {
		// Past the oldest retained bar: show just the oldest.
		back = r.count - 1
	}
	// Logical index (0-based from oldest) of the newest visible closed bar.
	newest := r.count - 1 - back
	if newest < 0 {
		return Snapshot{}
	}
	// Number of closed bars visible: indices 0..newest.
	count := newest + 1
	if count > n {
		count = n
	}
	if count <= 0 {
		return Snapshot{}
	}
	out := Snapshot{
		Bars: make([]Bar, count),
		SMA:  make([]float64, count),
		EMA:  make([]float64, count),
		VWAP: make([]float64, count),
	}
	startIdx := newest - count + 1
	for i := 0; i < count; i++ {
		j := r.at(startIdx + i)
		out.Bars[i] = r.bars[j]
		out.SMA[i] = r.sma[j]
		out.EMA[i] = r.ema[j]
		out.VWAP[i] = r.vwapAtClose[j]
	}
	return out
}

// HistoryDepth returns the number of closed bars retained for tf, i.e. how
// many bars back from the live edge the view can pan. Used by the UI to
// clamp the pan offset.
func (a *Aggregator) HistoryDepth(tf TF) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.series[tf].ring.count
}

// LastBarTime returns the start time of the newest retained bar for tf
// (closed or forming). ok=false if there are no bars at all. Used to
// anchor history fetches to a real trading time instead of wall-clock
// now (which may fall outside market hours).
func (a *Aggregator) LastBarTime(tf TF) (t time.Time, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.series[tf]
	if s.open {
		return s.forming.Start, true
	}
	r := s.ring
	if r.count == 0 {
		return time.Time{}, false
	}
	return r.bars[r.at(r.count-1)].Start, true
}
