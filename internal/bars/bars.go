// Package bars aggregates a trade stream into OHLCV bars at multiple
// resolutions, maintaining incremental dual-EMA and session-VWAP
// alongside. All state lives behind one mutex; buffers are preallocated rings.
package bars

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"trade-kernel/internal/indicators"
	"trade-kernel/internal/session"
)

// TF is a built-in bar resolution.
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
func (t TF) String() string {
	if t < 0 || int(t) >= int(numTF) {
		return "?"
	}
	return tfName[t]
}

// Duration returns the bar length for a built-in TF. Daily is 24h for
// heuristics (actual daily buckets anchor at 20:00 ET).
func (t TF) Duration() time.Duration {
	if t < 0 || int(t) >= int(numTF) {
		return time.Minute
	}
	return tfDur[t]
}

// All lists every timeframe in increasing order.
func All() []TF {
	return []TF{TF1s, TF5s, TF15s, TF1m, TF5m, TF15m, TF1h, TF1d}
}

// ParseTF converts "1s"/"5m"/"1h"/"1d" etc. to a built-in TF.
func ParseTF(s string) (TF, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for i, n := range tfName {
		if n == s {
			return TF(i), true
		}
	}
	return 0, false
}

// ChartTF is a chart resolution: either a built-in TF or a custom bar
// duration (e.g. 2m, 30s, 4h) set via config or :tf.
type ChartTF struct {
	Name    string
	Builtin TF            // valid when Custom == 0
	Custom  time.Duration // non-zero for non-built-in durations
}

// IsCustom reports whether this is a non-built-in duration.
func (c ChartTF) IsCustom() bool { return c.Custom > 0 }

// String returns the short name ("2m", "1m", ...).
func (c ChartTF) String() string { return c.Name }

// Duration returns the bar length (24h for daily).
func (c ChartTF) Duration() time.Duration {
	if c.IsCustom() {
		return c.Custom
	}
	return c.Builtin.Duration()
}

// customDurMin / customDurMax bound non-built-in chart resolutions.
const (
	customDurMin = time.Second
	customDurMax = 24 * time.Hour
)

// ParseChartTF parses a chart resolution string.
// Accepts built-ins (1s, 5s, 15s, 1m, 5m, 15m, 1h, 1d) and custom forms
// like 2m, 3m, 10m, 30s, 4h. Durations that match a built-in map to it.
// Custom durations must be in [1s, 24h].
func ParseChartTF(s string) (ChartTF, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ChartTF{}, false
	}
	if tf, ok := ParseTF(s); ok {
		return ChartTF{Name: tf.String(), Builtin: tf}, true
	}
	d, name, ok := parseDurationToken(s)
	if !ok {
		return ChartTF{}, false
	}
	// Collapse to built-in when the duration matches exactly.
	for tf := TF(0); tf < numTF; tf++ {
		if tfDur[tf] == d {
			return ChartTF{Name: tfName[tf], Builtin: tf}, true
		}
	}
	if d < customDurMin || d > customDurMax {
		return ChartTF{}, false
	}
	return ChartTF{Name: name, Custom: d}, true
}

// parseDurationToken parses "2m", "30s", "4h", "1d" → duration + canonical name.
// Rejects magnitudes that would overflow time.Duration or exceed 24h after
// unit conversion (bounds match customDurMax; ParseChartTF re-checks).
func parseDurationToken(s string) (time.Duration, string, bool) {
	if len(s) < 2 {
		return 0, "", false
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return 0, "", false
	}
	// Cap before multiply so multi-million tokens cannot wrap Duration.
	var maxN int
	var unitDur time.Duration
	switch unit {
	case 's':
		maxN, unitDur = 86400, time.Second // 24h
	case 'm':
		maxN, unitDur = 1440, time.Minute // 24h
	case 'h':
		maxN, unitDur = 24, time.Hour
	case 'd':
		maxN, unitDur = 1, 24 * time.Hour
	default:
		return 0, "", false
	}
	if n > maxN {
		return 0, "", false
	}
	d := time.Duration(n) * unitDur
	return d, fmt.Sprintf("%d%c", n, unit), true
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
	// VWAP is the bar's own volume-weighted average price when known
	// (e.g. from the REST bars endpoint "vw" field). Zero means unknown;
	// session-VWAP reconstruction then falls back to typical price
	// (H+L+C)/3.
	VWAP float64
}

func newBar(start time.Time, price, vol float64) Bar {
	return Bar{Start: start, Open: price, High: price, Low: price, Close: price, Volume: vol}
}

func (b *Bar) add(price, vol float64) {
	// Caller (OnTrade) already rejects non-positive prices; keep the guard
	// so late/replay paths cannot drive Low to 0 and collapse the Y-axis.
	if !validTradePrice(price) {
		return
	}
	if price > b.High {
		b.High = price
	}
	if price < b.Low {
		b.Low = price
	}
	b.Close = price
	if vol > 0 && !math.IsNaN(vol) && !math.IsInf(vol, 0) {
		b.Volume += vol
	}
}

// ring is a fixed-capacity circular buffer of bars with parallel
// per-bar indicator values (NaN until ready).
type ring struct {
	bars        []Bar
	ema, ema2   []float64
	vwapAtClose []float64
	start       int
	count       int
}

func newRing(cap int) *ring {
	return &ring{
		bars:        make([]Bar, cap),
		ema:         make([]float64, cap),
		ema2:        make([]float64, cap),
		vwapAtClose: make([]float64, cap),
	}
}

func (r *ring) push(b Bar, emaV, ema2V, vwapV float64) {
	if r.count < len(r.bars) {
		i := (r.start + r.count) % len(r.bars)
		r.set(i, b, emaV, ema2V, vwapV)
		r.count++
		return
	}
	r.set(r.start, b, emaV, ema2V, vwapV)
	r.start = (r.start + 1) % len(r.bars)
}

func (r *ring) set(i int, b Bar, emaV, ema2V, vwapV float64) {
	r.bars[i] = b
	r.ema[i] = emaV
	r.ema2[i] = ema2V
	r.vwapAtClose[i] = vwapV
}

func (r *ring) at(i int) int { return (r.start + i) % len(r.bars) }

func (r *ring) last() int { return r.at(r.count - 1) }

// popLast removes and returns the newest closed bar. ok is false when empty.
func (r *ring) popLast() (b Bar, ok bool) {
	if r.count == 0 {
		return Bar{}, false
	}
	i := r.last()
	b = r.bars[i]
	r.count--
	return b, true
}

// series holds the ring plus forming-bar and indicator state for one TF.
type series struct {
	ring    *ring
	forming Bar
	open    bool // forming is valid
	ema     *indicators.EMA
	ema2    *indicators.EMA
}

const ringCap = 2048

// Snapshot is a render-ready copy of recent bars and indicator overlays.
// The final element is the live forming bar with Peek'd indicators.
type Snapshot struct {
	Bars []Bar
	EMA  []float64 // fast EMA (ema_period)
	EMA2 []float64 // slow EMA (ema2_period)
	VWAP []float64 // session VWAP value at each bar's close
}

// Aggregator builds bars for all timeframes from one trade stream.
// All methods are safe for concurrent use.
type Aggregator struct {
	mu sync.Mutex

	series [numTF]*series
	emaN   int
	ema2N  int

	// Optional custom chart resolution (e.g. 2m) — live-fed and rebuilt
	// from the finest suitable built-in after Load / ReplayTrades.
	customDur  time.Duration
	customName string
	custom     *series

	vwap       indicators.VWAP // session/day-anchored, fed per trade
	vwapAnchor string          // "session" (default) or "day"

	lastTradePrice float64
	lastTradeAt    time.Time
	bid, ask       float64
	bidAt, askAt   time.Time // per-side timestamps (one-sided books)
	quoteAt        time.Time // min of valid sides — drives staleness checks
}

// NewAggregator creates an Aggregator with two EMA periods (fast, slow).
func NewAggregator(emaPeriod, ema2Period int) *Aggregator {
	a := &Aggregator{
		emaN:       emaPeriod,
		ema2N:      ema2Period,
		vwapAnchor: "session",
	}
	for tf := TF(0); tf < numTF; tf++ {
		a.series[tf] = &series{
			ring: newRing(ringCap),
			ema:  indicators.NewEMA(emaPeriod),
			ema2: indicators.NewEMA(ema2Period),
		}
	}
	return a
}

// SetVWAPAnchor sets reconstruction/live seeding mode: "session" (reset on
// each session-instance change) or "day" (reset only at the 20:00 ET
// trading-day boundary). Invalid values are ignored.
func (a *Aggregator) SetVWAPAnchor(anchor string) {
	if anchor != "session" && anchor != "day" {
		return
	}
	a.mu.Lock()
	a.vwapAnchor = anchor
	a.mu.Unlock()
}

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

// validTradePrice reports whether price is usable for OHLC / last / VWAP.
// Non-positive, NaN, and Inf prints must never enter the rings — a single
// zero low expands the chart scale to the origin and flattens every candle.
func validTradePrice(price float64) bool {
	return price > 0 && !math.IsNaN(price) && !math.IsInf(price, 0)
}

// maxLiveTradeJumpRatio rejects live prints that jump by more than this
// factor vs the last accepted trade (or below 1/ratio). Catches bad SIP
// ticks like 1e20 without blocking real multi-percent gaps after halts.
// Historical ReplayTrades does not apply this filter.
const maxLiveTradeJumpRatio = 10.0

// liveTradePlausibleLocked reports whether price is a sane live print given
// the last accepted trade. Caller holds a.mu. First print (no last) is always
// accepted when validTradePrice already passed.
func (a *Aggregator) liveTradePlausibleLocked(price float64) bool {
	last := a.lastTradePrice
	if last <= 0 {
		return true
	}
	hi := last * maxLiveTradeJumpRatio
	lo := last / maxLiveTradeJumpRatio
	return price <= hi && price >= lo
}

// OnTrade folds one trade into every timeframe. Must not block.
// Invalid or absurd live prices are ignored (no last-trade update, no bar mutation).
func (a *Aggregator) OnTrade(symbol string, price, size float64, ts time.Time) {
	if !validTradePrice(price) {
		return
	}
	if size < 0 || math.IsNaN(size) || math.IsInf(size, 0) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.liveTradePlausibleLocked(price) {
		return
	}
	a.lastTradePrice = price
	a.lastTradeAt = ts
	a.vwap.Update(price, size)
	for tf := TF(0); tf < numTF; tf++ {
		a.aggregateInto(tf, price, size, ts)
	}
	if a.custom != nil && a.customDur > 0 {
		a.aggregateIntoCustom(price, size, ts)
	}
}

// ReplayTrades feeds a historical trade sequence into the sub-minute
// timeframes (1s/5s/15s) only. Used to backfill those TFs from the REST
// trades endpoint, since the bars endpoint doesn't serve sub-minute
// resolutions. Trades must be in ascending timestamp order.
//
// Sub-minute series are reset before replay so reconnect / symbol switch
// cannot leave prior-symbol or already-live 1s bars in the ring (which would
// pollute 1s-sourced custom TFs such as 30s/90s). Live prints that arrived
// during the REST fetch and fall inside the replay window are re-derived
// from the trade history; later prints resume via OnTrade.
//
// It deliberately does NOT touch the 1m+ TFs (already backfilled from the
// bars endpoint) nor the session VWAP / last-trade cache (live-only
// state) — replaying into those would double-count volume and overwrite
// the live edge with stale prices.
//
// When a custom TF that resamples from 1s is active it is rebuilt from the
// refreshed 1s series after replay (sub-minute and non–minute-aligned
// durations like 90s).
func (a *Aggregator) ReplayTrades(trades []TradeReplay) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Clean slate so rebuildCustomLocked sees only this batch (plus nothing
	// if trades is empty — still clears stale sub-minute history).
	for _, tf := range []TF{TF1s, TF5s, TF15s} {
		a.resetSeriesLocked(a.series[tf])
	}
	for _, tr := range trades {
		if !validTradePrice(tr.Price) {
			continue
		}
		if tr.Size < 0 || math.IsNaN(tr.Size) || math.IsInf(tr.Size, 0) {
			continue
		}
		for _, tf := range []TF{TF1s, TF5s, TF15s} {
			a.aggregateInto(tf, tr.Price, tr.Size, tr.Timestamp)
		}
	}
	if a.custom != nil && a.customDur > 0 && customSourceTF(a.customDur) == TF1s {
		a.rebuildCustomLocked()
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
	a.foldTrade(a.series[tf], bucket(tf, ts), price, size, a.closeFormingBar)
}

// aggregateIntoCustom folds one trade into the active custom series.
// Caller holds the mutex; a.custom must be non-nil.
func (a *Aggregator) aggregateIntoCustom(price, size float64, ts time.Time) {
	a.foldTrade(a.custom, bucketDur(a.customDur, ts), price, size, a.closeFormingBar)
}

// foldTrade is the shared bucket-rollover logic for built-in and custom series.
// closeFn closes the current forming bar (EMA/EMA2 update + ring push).
func (a *Aggregator) foldTrade(s *series, b time.Time, price, size float64, closeFn func(*series)) {
	switch {
	case !s.open:
		// If the newest closed bar is this same bucket (e.g. REST partial was
		// closed into the ring), reopen it as forming so we never render two
		// candles for one minute and so EMA/EMA2/VWAP stay Peek'd on the live
		// close. Prefer Load leaving the current bucket as forming; this is a
		// safety net for races / older loads.
		if s.ring.count > 0 {
			li := s.ring.last()
			if s.ring.bars[li].Start.Equal(b) {
				bb, _ := s.ring.popLast()
				// Reverse the EMA updates applied at closeBar so the forming
				// bar is not double-counted when it closes again.
				s.ema.Undo(bb.Close)
				s.ema2.Undo(bb.Close)
				bb.add(price, size)
				s.forming = bb
				s.open = true
				return
			}
		}
		s.forming = newBar(b, price, size)
		s.open = true
	case b.Equal(s.forming.Start):
		s.forming.add(price, size)
	case b.After(s.forming.Start):
		closeFn(s)
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
				// Close is left alone for historical closed bars (tape repairs
				// rarely rewrite official close). Current incomplete bucket is
				// handled as forming or the !s.open same-bucket path above.
				break
			}
		}
	}
}

func (a *Aggregator) closeFormingBar(s *series) {
	b := s.forming
	emaV := s.ema.Update(b.Close)
	ema2V := s.ema2.Update(b.Close)
	s.ring.push(b, emaV, ema2V, a.vwap.Value())
	s.open = false
}

// bucketDur truncates ts to a custom bar duration (UTC, same as intraday built-ins).
func bucketDur(d time.Duration, ts time.Time) time.Time {
	if d <= 0 {
		return ts.UTC()
	}
	return ts.UTC().Truncate(d)
}

// OnQuote caches the latest NBBO. Each side is updated independently when
// valid so a one-sided or temporarily zeroed book still refreshes the good
// side (common in thin extended-hours quotes). A quote with both sides
// invalid is ignored so a zero print cannot wipe a good cache.
//
// quoteAt is the older of the two valid side timestamps so a fresh ask-only
// (or bid-only) print cannot make the retained opposite side look current for
// staleness checks (extended-hours Flatten prices sells off bid / buys off ask).
func (a *Aggregator) OnQuote(symbol string, bid, ask float64, ts time.Time) {
	bidOK := validTradePrice(bid)
	askOK := validTradePrice(ask)
	if !bidOK && !askOK {
		return
	}
	a.mu.Lock()
	if bidOK {
		a.bid = bid
		a.bidAt = ts
	}
	if askOK {
		a.ask = ask
		a.askAt = ts
	}
	a.recomputeQuoteAtLocked()
	a.mu.Unlock()
}

// recomputeQuoteAtLocked sets quoteAt to the older valid side timestamp.
// Caller holds a.mu.
func (a *Aggregator) recomputeQuoteAtLocked() {
	bidOK := a.bid > 0 && !a.bidAt.IsZero()
	askOK := a.ask > 0 && !a.askAt.IsZero()
	switch {
	case bidOK && askOK:
		if a.bidAt.Before(a.askAt) {
			a.quoteAt = a.bidAt
		} else {
			a.quoteAt = a.askAt
		}
	case bidOK:
		a.quoteAt = a.bidAt
	case askOK:
		a.quoteAt = a.askAt
	default:
		a.quoteAt = time.Time{}
	}
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

// MarketSnapshot bundles the live market cache the UI reads each frame so the
// render loop takes the aggregator mutex once instead of four times
// (LatestTrade / LatestQuote / SessionVWAP). Returned values are copies.
type MarketSnapshot struct {
	LastPrice float64
	LastAt    time.Time
	Bid       float64
	Ask       float64
	QuoteAt   time.Time
	VWAP      float64
}

// MarketSnapshot returns the cached last trade, NBBO, and session VWAP in a
// single locked read.
func (a *Aggregator) MarketSnapshot() MarketSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return MarketSnapshot{
		LastPrice: a.lastTradePrice,
		LastAt:    a.lastTradeAt,
		Bid:       a.bid,
		Ask:       a.ask,
		QuoteAt:   a.quoteAt,
		VWAP:      a.vwap.Value(),
	}
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
//
// It also clears sub-minute rings and the custom series (keeping custom
// duration/name) so live OnTrade during backfill cannot mix the previous
// symbol into 1s-sourced custom charts. Minute+ series are replaced by
// Load; custom rebuilds after Load(TF1m) or ReplayTrades as usual.
func (a *Aggregator) ResetMarket() {
	a.mu.Lock()
	a.lastTradePrice = 0
	a.lastTradeAt = time.Time{}
	a.bid, a.ask = 0, 0
	a.bidAt, a.askAt = time.Time{}, time.Time{}
	a.quoteAt = time.Time{}
	for _, tf := range []TF{TF1s, TF5s, TF15s} {
		a.resetSeriesLocked(a.series[tf])
	}
	if a.customDur > 0 {
		// Keep EnableCustom active; empty series until source history rebuilds.
		a.custom = &series{
			ring: newRing(ringCap),
			ema:  indicators.NewEMA(a.emaN),
			ema2: indicators.NewEMA(a.ema2N),
		}
	}
	a.mu.Unlock()
}

// resetSeriesLocked empties one series' ring, EMAs, and forming bar.
// Caller holds mu.
func (a *Aggregator) resetSeriesLocked(s *series) {
	s.ring = newRing(ringCap)
	s.ema = indicators.NewEMA(a.emaN)
	s.ema2 = indicators.NewEMA(a.ema2N)
	s.open = false
	s.forming = Bar{}
}

// Load replaces one timeframe's history with backfilled bars (e.g. from
// the REST bars endpoint) and resets its indicator state accordingly.
//
// Session VWAP is reconstructed across hist so the chart overlay is a
// continuous line. Per-bar contribution uses Bar.VWAP when set, otherwise
// typical price (H+L+C)/3. Reset policy matches the configured anchor:
//
//   - "session": reset on every session-instance change (session enum or
//     trading-day change), so multi-day RTH-only history restarts each day.
//   - "day": reset only when the 20:00 ET trading day changes.
//
// The incomplete current bucket (when present as the last hist bar) is left
// as the forming bar rather than a closed ring entry. Closing it would
// duplicate the minute once live trades open a new forming bar for the same
// start, which under/over-states volume vs REST/TradingView.
//
// For TF1m the live session VWAP is seeded from the reconstruction (including
// the forming bar's contribution) so SessionVWAP is continuous after
// reconnect. Live SIP prints that overlap the REST partial may briefly
// double-count that minute; that overshoot is preferable to a permanent
// under-count for the rest of the session.
func (a *Aggregator) Load(tf TF, hist []Bar) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.series[tf]
	a.resetSeriesLocked(s)

	// Incomplete current-bucket bar from REST must stay forming so live
	// trades extend it instead of creating a second candle for the same start.
	// Accept lastStart >= current bucket (clock skew / server slightly ahead)
	// so Snapshot does not briefly show a "closed" live candle.
	curBucket := bucket(tf, time.Now())
	var forming *Bar
	if n := len(hist); n > 0 && !hist[n-1].Start.Before(curBucket) {
		b := hist[n-1]
		forming = &b
		hist = hist[:n-1]
	}

	var sessVWAP indicators.VWAP
	var prevKey string
	haveKey := false
	for _, b := range hist {
		cur := session.At(b.Start)
		// Closed bars do not contribute to session VWAP (no session noise)
		// and do not force a reset; carry the prior value forward.
		if cur != session.Closed {
			key := vwapInstanceKey(a.vwapAnchor, b.Start, cur)
			if haveKey && key != prevKey {
				sessVWAP.Reset()
			}
			prevKey = key
			haveKey = true
		}

		emaV := s.ema.Update(b.Close)
		ema2V := s.ema2.Update(b.Close)
		var vwapV float64
		if cur != session.Closed {
			vwapV = sessVWAP.Update(barVWAPPrice(b), b.Volume)
		} else {
			vwapV = sessVWAP.Value()
		}
		s.ring.push(b, emaV, ema2V, vwapV)
	}

	if forming != nil {
		// Leave the incomplete bar as forming for OHLCV continuity, and seed
		// its volume into session VWAP so reconnect does not permanently
		// under-count the partial minute for the rest of the session.
		// Apply any session-key reset the forming bar would have triggered
		// so the live accumulator does not span an anchor change.
		cur := session.At(forming.Start)
		if cur != session.Closed {
			key := vwapInstanceKey(a.vwapAnchor, forming.Start, cur)
			if haveKey && key != prevKey {
				sessVWAP.Reset()
			}
			_ = sessVWAP.Update(barVWAPPrice(*forming), forming.Volume)
		}
		s.forming = *forming
		s.open = true
	}

	// Seed the live trade VWAP from the finest REST-backed series so the
	// right edge of the chart doesn't jump when live ticks start flowing.
	// Coarser TFs load after 1m in backfill; only TF1m seeds to avoid the
	// coarser reconstruction overwriting a better one.
	if tf == TF1m {
		a.vwap = sessVWAP
	}
	// Custom charts that resample from 1m rebuild after each 1m load.
	if tf == TF1m && a.custom != nil && a.customDur > 0 && customSourceTF(a.customDur) == TF1m {
		a.rebuildCustomLocked()
	}
}

// EnableCustom activates a non-built-in chart duration (e.g. 2m). History is
// resampled from the finest suitable built-in series; subsequent OnTrade
// calls feed the custom series live. Caller should have validated d via
// ParseChartTF. No-op for non-positive d.
func (a *Aggregator) EnableCustom(d time.Duration, name string) {
	if d <= 0 {
		return
	}
	if name == "" {
		name = d.String()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.customDur = d
	a.customName = name
	a.rebuildCustomLocked()
}

// ClearCustom disables the custom chart series (Tab cycle / built-in :tf).
func (a *Aggregator) ClearCustom() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.customDur = 0
	a.customName = ""
	a.custom = nil
}

// CustomActive reports whether a custom TF is enabled and its display name.
func (a *Aggregator) CustomActive() (name string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.custom == nil || a.customDur <= 0 {
		return "", false
	}
	return a.customName, true
}

// SnapshotCustom is Snapshot for the active custom series. Empty if none.
func (a *Aggregator) SnapshotCustom(n, offset int) Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n <= 0 {
		return Snapshot{}
	}
	if a.custom == nil || a.customDur <= 0 {
		return Snapshot{}
	}
	return a.snapshotSeriesLocked(a.custom, n, offset)
}

// HistoryDepthCustom returns closed-bar depth for the custom series (0 if none).
func (a *Aggregator) HistoryDepthCustom() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.custom == nil {
		return 0
	}
	return a.custom.ring.count
}

// customSourceTF picks the built-in series used to rebuild custom history.
// Exact multiples of 1m use TF1m; everything else (sub-minute and odd
// durations like 90s) needs second-level source bars.
func customSourceTF(d time.Duration) TF {
	if d >= time.Minute && d%time.Minute == 0 {
		return TF1m
	}
	return TF1s
}

// rebuildCustomLocked resamples the custom series from 1s or 1m built-in
// history (see customSourceTF). Caller holds mu; customDur must be set.
func (a *Aggregator) rebuildCustomLocked() {
	if a.customDur <= 0 {
		a.custom = nil
		return
	}
	srcTF := customSourceTF(a.customDur)
	src := a.series[srcTF]
	var srcBars []Bar
	if src.ring.count > 0 {
		srcBars = make([]Bar, 0, src.ring.count+1)
		for i := 0; i < src.ring.count; i++ {
			srcBars = append(srcBars, src.ring.bars[src.ring.at(i)])
		}
	}
	if src.open {
		srcBars = append(srcBars, src.forming)
	}
	hist := resampleTo(srcBars, a.customDur)
	a.loadCustomLocked(hist)
}

// resampleTo merges finer OHLCV bars into coarser buckets of duration dur.
// Per-bar VWAP is blended as volume-weighted average of barVWAPPrice (REST
// vw when present, else typical price) so loadCustomLocked can use it.
func resampleTo(src []Bar, dur time.Duration) []Bar {
	if len(src) == 0 || dur <= 0 {
		return nil
	}
	out := make([]Bar, 0, len(src))
	var cur Bar
	var have bool
	var curBucket time.Time
	var vwapPxVol, vwapVol float64 // Σ(price_i * vol_i), Σ(vol_i) for VWAP blend
	finish := func() {
		if vwapVol > 0 {
			cur.VWAP = vwapPxVol / vwapVol
		}
		out = append(out, cur)
	}
	for _, b := range src {
		bk := bucketDur(dur, b.Start)
		if !have || !bk.Equal(curBucket) {
			if have {
				finish()
			}
			cur = Bar{
				Start:  bk,
				Open:   b.Open,
				High:   b.High,
				Low:    b.Low,
				Close:  b.Close,
				Volume: b.Volume,
			}
			vwapPxVol, vwapVol = 0, 0
			if b.Volume > 0 {
				p := barVWAPPrice(b)
				vwapPxVol = p * b.Volume
				vwapVol = b.Volume
			}
			curBucket = bk
			have = true
			continue
		}
		if b.High > cur.High {
			cur.High = b.High
		}
		if b.Low < cur.Low {
			cur.Low = b.Low
		}
		cur.Close = b.Close
		cur.Volume += b.Volume
		if b.Volume > 0 {
			p := barVWAPPrice(b)
			vwapPxVol += p * b.Volume
			vwapVol += b.Volume
		}
	}
	if have {
		finish()
	}
	return out
}

// loadCustomLocked replaces the custom series with hist (same forming-bar
// rules as Load). Caller holds mu.
func (a *Aggregator) loadCustomLocked(hist []Bar) {
	s := &series{
		ring: newRing(ringCap),
		ema:  indicators.NewEMA(a.emaN),
		ema2: indicators.NewEMA(a.ema2N),
	}
	curBucket := bucketDur(a.customDur, time.Now())
	var forming *Bar
	if n := len(hist); n > 0 && !hist[n-1].Start.Before(curBucket) {
		b := hist[n-1]
		forming = &b
		hist = hist[:n-1]
	}

	var sessVWAP indicators.VWAP
	var prevKey string
	haveKey := false
	for _, b := range hist {
		cur := session.At(b.Start)
		if cur != session.Closed {
			key := vwapInstanceKey(a.vwapAnchor, b.Start, cur)
			if haveKey && key != prevKey {
				sessVWAP.Reset()
			}
			prevKey = key
			haveKey = true
		}
		emaV := s.ema.Update(b.Close)
		ema2V := s.ema2.Update(b.Close)
		var vwapV float64
		if cur != session.Closed {
			vwapV = sessVWAP.Update(barVWAPPrice(b), b.Volume)
		} else {
			vwapV = sessVWAP.Value()
		}
		s.ring.push(b, emaV, ema2V, vwapV)
	}
	if forming != nil {
		cur := session.At(forming.Start)
		if cur != session.Closed {
			key := vwapInstanceKey(a.vwapAnchor, forming.Start, cur)
			if haveKey && key != prevKey {
				sessVWAP.Reset()
			}
			_ = sessVWAP.Update(barVWAPPrice(*forming), forming.Volume)
		}
		s.forming = *forming
		s.open = true
	}
	a.custom = s
}

// vwapInstanceKey identifies a VWAP accumulation window for reconstruction.
// Day mode keys only on the 20:00 ET trading day; session mode also includes
// the session enum so overnight/pre/regular/AH each restart.
func vwapInstanceKey(anchor string, t time.Time, sess session.Session) string {
	day := tradingDayID(t)
	if anchor == "day" {
		return day
	}
	return day + "|" + sess.String()
}

// tradingDayID returns a stable id for the 20:00 ET–anchored trading day
// that contains t (same boundary as daily bars).
func tradingDayID(t time.Time) string {
	return bucket(TF1d, t).Format(time.RFC3339)
}

// barVWAPPrice picks the best single price to contribute to session VWAP
// reconstruction for one bar.
func barVWAPPrice(b Bar) float64 {
	if b.VWAP != 0 && !math.IsNaN(b.VWAP) {
		return b.VWAP
	}
	// Typical price is the usual OHLC stand-in for bar-level VWAP.
	tp := (b.High + b.Low + b.Close) / 3
	if tp != 0 && !math.IsNaN(tp) {
		return tp
	}
	return b.Close
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
//	  their own frozen ema/ema2/vwapAtClose captured at bar close, so panned
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
	return a.snapshotSeriesLocked(a.series[tf], n, offset)
}

// snapshotSeriesLocked is the shared Snapshot body for built-in and custom.
// Caller holds mu; s must be non-nil.
func (a *Aggregator) snapshotSeriesLocked(s *series, n, offset int) Snapshot {
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
			EMA:  make([]float64, count),
			EMA2: make([]float64, count),
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
			out.EMA[i] = r.ema[j]
			out.EMA2[i] = r.ema2[j]
			out.VWAP[i] = r.vwapAtClose[j]
		}
		f := s.forming
		i := count - 1
		out.Bars[i] = f
		out.EMA[i] = s.ema.Peek(f.Close)
		out.EMA2[i] = s.ema2.Peek(f.Close)
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
		EMA:  make([]float64, count),
		EMA2: make([]float64, count),
		VWAP: make([]float64, count),
	}
	startIdx := newest - count + 1
	for i := 0; i < count; i++ {
		j := r.at(startIdx + i)
		out.Bars[i] = r.bars[j]
		out.EMA[i] = r.ema[j]
		out.EMA2[i] = r.ema2[j]
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
