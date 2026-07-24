// Package state keeps the local view of account, positions, and open
// orders, reconciled from REST at startup and updated from the trading
// WebSocket stream. Account day/week PnL are realized-only from closed
// fills (average-cost inventory), not equity mark-to-market: open unrealized
// is already shown per position. Live trade_updates seed rday/rwk immediately;
// the activities FILL endpoint heals the cache on a slower cadence.
package state

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

// realizedPnLStaleAfter hides rday/rwk when fill history has not been
// refreshed successfully for this long (avoids multi-hour frozen figures).
const realizedPnLStaleAfter = 30 * time.Minute

// fillHistoryLookback is how far before the current week open we fetch
// closed orders / fills so average-cost inventory is warm for mid-week closes.
const fillHistoryLookback = 180 * 24 * time.Hour

// BookSnapshot is a single-lock-acquired copy of the store's render-relevant
// fields. The UI render loop takes one BookSnapshot per frame instead of
// calling Positions()/OpenOrders()/Account()/PnL() separately (each of which
// takes the RWMutex and allocates a slice). Positions, Orders, and Account are
// deep copies — the snapshot is safe to read off the lock for the whole frame.
type BookSnapshot struct {
	Account     alpaca.Account
	HasAccount  bool
	Reconciled  bool
	Positions   []alpaca.Position
	Orders      []alpaca.Order
	PnL         AccountPnL
	PositionQty map[string]float64 // signed qty per symbol; nil-safe lookups
}

// ActivePosition returns the snapshot entry for symbol (copy by value), or
// nil if no position is open. Safe to mutate the returned pointer.
func (b BookSnapshot) ActivePosition(symbol string) *alpaca.Position {
	for i := range b.Positions {
		if b.Positions[i].Symbol == symbol {
			p := b.Positions[i]
			return &p
		}
	}
	return nil
}

// SignedQty returns the signed position qty for symbol (negative = short).
// Prefers PositionQty when present (single source with Snapshot); falls back
// to walking Positions for hand-built snapshots.
func (b BookSnapshot) SignedQty(symbol string) float64 {
	if b.PositionQty != nil {
		return b.PositionQty[symbol]
	}
	for i := range b.Positions {
		if b.Positions[i].Symbol != symbol {
			continue
		}
		return SignedPositionQty(b.Positions[i])
	}
	return 0
}

// OtherOpenCount returns how many symbols other than active have a non-zero
// position and/or resting open order. Used for multi-name risk reminders.
func (b BookSnapshot) OtherOpenCount(active string) int {
	seen := make(map[string]struct{})
	for _, p := range b.Positions {
		if p.Symbol != "" && p.Symbol != active {
			seen[p.Symbol] = struct{}{}
		}
	}
	for _, o := range b.Orders {
		if o.Symbol != "" && o.Symbol != active {
			seen[o.Symbol] = struct{}{}
		}
	}
	return len(seen)
}

// ActiveOrders returns the open orders for symbol (active symbol only, per
// the trading-focus rule). Allocates a fresh slice only when there is at
// least one matching order.
func (b BookSnapshot) ActiveOrders(symbol string) []alpaca.Order {
	var out []alpaca.Order
	for _, o := range b.Orders {
		if o.Symbol == symbol {
			out = append(out, o)
		}
	}
	return out
}

// AccountPnL is cached day/week realized profit-and-loss from closed fills
// (average-cost inventory). Open marks are never included — per-position
// unrealized is already on the POS segment.
//
//	Day  = sum of realized P&L on fills with time ≥ DayStart (last real
//	       overnight open Sun–Thu 20:00 ET; weekend pins to Thu 20:00)
//	Week = sum of realized P&L on fills with time ≥ WeekStart (Sun 20:00 ET)
//
// Both are the current trading day / current 24/5 trading week only — not a
// trailing N-day equity window. Refreshed by RefreshRealizedPnL from the
// closed-order / FILL history.
//
// Partial is true when the last successful sample excluded one or more
// unreconcilable symbols (totals may undercount). PartialNote lists them.
type AccountPnL struct {
	Day         float64
	Week        float64
	HasDay      bool
	HasWeek     bool
	Partial     bool
	PartialNote string
}

// Store is a mutex-guarded cache of account state.
type Store struct {
	mu sync.RWMutex

	account alpaca.Account
	hasAcct bool
	// reconciled is true after at least one successful Reconcile. Order entry
	// is blocked until this flips so an operator never submits against an
	// empty/stale view (a failed startup Refresh would otherwise show "flat"
	// while real positions exist). Flatten/panic bypass the gate: they read
	// PositionQty live. ClosePosition (DELETE /v2/positions) is used only for
	// locally Closed sessions and fractional residuals — not extended-hours
	// whole-share exits (those use aggressive limit orders).
	reconciled bool
	positions  map[string]alpaca.Position
	// restPositions is the last REST Reconcile snapshot. Realized-PnL seeds
	// use this rather than live positions: trading-WS can shrink qty before
	// the FILL activity feed includes the trade, which races seed synthesis.
	restPositions map[string]alpaca.Position
	orders        map[string]alpaca.Order // keyed by order ID

	// Fill-based realized day/week (see RefreshRealizedPnL).
	dayRealized    float64
	weekRealized   float64
	hasDayPnL      bool
	hasWeekPnL     bool
	pnlUpdatedAt   time.Time
	pnlPartial     bool
	pnlPartialNote string

	// Cached FILL history for incremental RefreshRealizedPnL. Pruned to
	// [fillCacheAfter, …]; merged by activity id on each successful fetch.
	// fillCacheAfter is set on every successful fetch (including empty), so a
	// flat account does not re-scan the full lookback every tick.
	fillCache      []alpaca.Fill
	fillCacheAfter time.Time // the `after` bound used to populate the cache
	// fillCacheLastFull is when we last walked the full [after, until) window
	// (not a delta/high-water or empty-cache short recheck). Used to periodically
	// heal delayed FILLs that land after an empty poll or behind a warm HWM.
	fillCacheLastFull time.Time

	// wsFills are individual executions observed on the trading WebSocket.
	// They let rday/rwk update immediately after buy/sell without waiting for
	// the activities FILL endpoint (which can lag many seconds). Merged with
	// fillCache via mergeWSFills; dropped once REST coverage matches.
	wsFills []alpaca.Fill

	// retainedBasis keeps last-known REST avg entry per symbol after the
	// position goes flat so pure-exit fill books can still be costed (see
	// CostBasisHint / RealizedFromFillsWithHints).
	retainedBasis map[string]retainedBasis

	// Serializes RefreshRealizedPnL so concurrent callers cannot interleave
	// fill-cache merges (main runs one ticker; tests / future callers may not).
	realizedRefreshMu sync.Mutex

	// Coalesced async recompute of rday/rwk from fillCache+wsFills so the
	// trading-WS OnUpdate path stays O(1) under bursty partial fills.
	// realizedRecomputeMu guards the two flags only (not s.mu).
	realizedRecomputeMu   sync.Mutex
	realizedRecomputeNeed bool
	realizedRecomputeRun  bool
}

// retainedBasis is the last known absolute avg entry from REST for a symbol.
type retainedBasis struct {
	Avg     float64
	Updated time.Time
}

// fillCacheEmptyRecheck is how far back an empty-but-valid fill cache re-polls
// on the delta path instead of re-fetching the entire lookback window.
// Wide enough that a FILL delayed in Alpaca's activity feed after the first
// empty poll is still covered before the high-water mark moves permanently.
const fillCacheEmptyRecheck = 30 * time.Minute

// fillCacheFullRefreshEvery forces a full lookback walk at this interval so
// delayed FILLs are eventually discovered: on an empty cache, activities older
// than fillCacheEmptyRecheck; on a warm cache, activities whose
// transaction_time is older than the high-water mark (beyond the 2s overlap).
const fillCacheFullRefreshEvery = 15 * time.Minute

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{
		positions:     make(map[string]alpaca.Position),
		restPositions: make(map[string]alpaca.Position),
		orders:        make(map[string]alpaca.Order),
		retainedBasis: make(map[string]retainedBasis),
	}
}

// Reconcile replaces the store with a fresh REST snapshot of account,
// positions, and open orders. Realized day/week PnL is not derived here —
// see RefreshRealizedPnL (closed order / FILL history).
func (s *Store) Reconcile(acct alpaca.Account, positions []alpaca.Position, orders []alpaca.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = acct
	s.hasAcct = true
	s.reconciled = true
	now := time.Now()
	// Retain cost basis from the outgoing REST snapshot before replace so a
	// full exit still has avg for pure-exit fill books on the next realized tick.
	if s.retainedBasis == nil {
		s.retainedBasis = make(map[string]retainedBasis)
	}
	for _, p := range s.restPositions {
		if avg := float64(p.AvgEntryPrice); avg > 0 && p.Symbol != "" {
			s.retainedBasis[p.Symbol] = retainedBasis{Avg: avg, Updated: now}
		}
	}
	s.positions = make(map[string]alpaca.Position, len(positions))
	s.restPositions = make(map[string]alpaca.Position, len(positions))
	for _, p := range positions {
		// Normalize signed REST qty (negative shorts) to abs qty + side so
		// PositionQty/SeedsFromPositions never double-negate or drop shorts.
		p = normalizePosition(p)
		if p.Symbol == "" {
			continue
		}
		s.positions[p.Symbol] = p
		s.restPositions[p.Symbol] = p
		if avg := float64(p.AvgEntryPrice); avg > 0 {
			s.retainedBasis[p.Symbol] = retainedBasis{Avg: avg, Updated: now}
		}
	}
	// Drop retained basis older than the fill lookback (no longer needed).
	for sym, rb := range s.retainedBasis {
		if now.Sub(rb.Updated) > fillHistoryLookback {
			delete(s.retainedBasis, sym)
		}
	}
	s.orders = make(map[string]alpaca.Order, len(orders))
	for _, o := range orders {
		s.orders[o.ID] = o
	}
}

// ApplyUpdate folds one trading-WS trade update into the store.
//
// Orders are updated immediately. On fill / partial_fill, the position for
// the order's symbol is also applied from PositionQty when Alpaca includes
// it, so risk checks and flatten sizing stay current without waiting for
// the 5 s REST reconcile. Fills are also recorded into wsFills so realized
// day/week PnL can recompute immediately (activities FILL often lags).
//
// The O(1) order/position/wsFills updates hold the write lock; the full
// realized fill walk is scheduled on a coalesced worker after unlock so the
// trading-WS handler is not blocked behind display-only PnL work (and risk
// PositionQty/WorkingSideQty stay current under bursty partials).
func (s *Store) ApplyUpdate(u alpaca.TradeUpdate) {
	s.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			s.mu.Unlock()
		}
	}
	// Ensure the mutex is released even if apply/note path panics.
	defer unlock()

	o := u.Order
	// Capture prior filled state before overwriting/deleting the order so
	// scale-in avg can derive the incremental fill price from cumulative
	// FilledQty / FilledAvgPrice when the event-level price is missing.
	var prevFilledQty, prevFilledAvg float64
	if prev, ok := s.orders[o.ID]; ok {
		prevFilledQty = float64(prev.FilledQty)
		prevFilledAvg = float64(prev.FilledAvgPrice)
	}
	switch u.Event {
	case "fill", "canceled", "cancelled", "expired", "rejected", "done_for_day", "replaced":
		// "replaced" is terminal for the old order id; the replacement arrives
		// as a separate new/accepted update. Leaving replaced rows in the map
		// double-counts WorkingSideQty until the next REST open-order reconcile.
		delete(s.orders, o.ID)
	case "partial_fill":
		o.Status = u.Event
		s.orders[o.ID] = o
	default:
		// new, accepted, pending_new, pending_replace, etc.
		s.orders[o.ID] = o
	}

	// PositionQty is present on fill events (and often partial_fill).
	// Skip when the field is omitted/null — 0 would otherwise wipe size.
	if (u.Event == "fill" || u.Event == "partial_fill") && u.PositionQty.Valid {
		s.applyPositionFromFill(o.Symbol, u.PositionQty.V, u.Event, o, prevFilledQty, prevFilledAvg, float64(u.Price), float64(u.Qty))
	}
	needRealized := false
	if u.Event == "fill" || u.Event == "partial_fill" {
		if s.noteWSFillLocked(o, prevFilledQty, prevFilledAvg, float64(u.Price), float64(u.Qty)) {
			needRealized = true
		}
	}
	unlock()
	if needRealized {
		s.scheduleRealizedRecompute()
	}
}

// scheduleRealizedRecompute coalesces full fill-history walks onto one worker
// so bursty partial_fill streams do not queue N O(history) walks on the WS
// callback path. noteWSFillLocked remains synchronous on ApplyUpdate.
func (s *Store) scheduleRealizedRecompute() {
	s.realizedRecomputeMu.Lock()
	s.realizedRecomputeNeed = true
	if s.realizedRecomputeRun {
		s.realizedRecomputeMu.Unlock()
		return
	}
	s.realizedRecomputeRun = true
	s.realizedRecomputeMu.Unlock()
	go s.realizedRecomputeLoop()
}

func (s *Store) realizedRecomputeLoop() {
	for {
		s.realizedRecomputeMu.Lock()
		if !s.realizedRecomputeNeed {
			s.realizedRecomputeRun = false
			s.realizedRecomputeMu.Unlock()
			return
		}
		s.realizedRecomputeNeed = false
		s.realizedRecomputeMu.Unlock()
		s.recomputeRealizedFromCache()
	}
}

// WaitRealizedRecompute blocks until any scheduled WS-path realized recompute
// finishes. Tests use this after ApplyUpdate when asserting rday/rwk.
func (s *Store) WaitRealizedRecompute() {
	for {
		s.realizedRecomputeMu.Lock()
		run := s.realizedRecomputeRun
		s.realizedRecomputeMu.Unlock()
		if !run {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// applyPositionFromFill updates the positions map from a fill event.
// qty is the broker's remaining position quantity after the fill (signed:
// negative = short). Zero removes the position on a terminal fill. Caller
// holds s.mu.
//
// prevFilledQty/prevFilledAvg are the order's filled state before this event
// (0 if unknown). eventPrice is the individual fill price from the trade
// update when present (0 if omitted). eventQty is the individual fill size
// from the trade update when present (0 if omitted).
func (s *Store) applyPositionFromFill(symbol string, qty float64, event string, o alpaca.Order, prevFilledQty, prevFilledAvg, eventPrice, eventQty float64) {
	if symbol == "" {
		return
	}
	// Explicit zero on partial_fill can still be ambiguous on some payloads;
	// do not clear an existing position on an ambiguous zero partial.
	if qty == 0 {
		if event == "partial_fill" {
			return
		}
		// Keep cost basis for pure-exit realized PnL after the map drops the name.
		if prev, ok := s.positions[symbol]; ok {
			if avg := float64(prev.AvgEntryPrice); avg > 0 {
				if s.retainedBasis == nil {
					s.retainedBasis = make(map[string]retainedBasis)
				}
				s.retainedBasis[symbol] = retainedBasis{Avg: avg, Updated: time.Now()}
			}
		}
		delete(s.positions, symbol)
		return
	}
	side := "long"
	absQty := qty
	if qty < 0 {
		side = "short"
		absQty = -qty
	}
	prev, ok := s.positions[symbol]
	p := alpaca.Position{
		Symbol: symbol,
		Side:   side,
	}
	p.SetQty(absQty)

	orderAvg := float64(o.FilledAvgPrice)
	orderFilled := float64(o.FilledQty)
	if !ok || prev.Side != side {
		// New position or side flip: never keep prior avg/uPL — those belong
		// to the opposite book. Prefer the broker's cumulative order avg only
		// when this order fully explains the open size (no cover+open blend).
		// A flip that sells more than the prior long (orderFilled > absQty)
		// has a cumulative avg that mixes covering the old side with opening
		// the new — leave avg zero for REST reconcile rather than display a
		// misleading blend. Prefer event-level fill price when present.
		switch {
		case orderAvg > 0 && orderFilled > 0 && orderFilled == absQty:
			p.SetAvgEntryPrice(orderAvg)
		case eventPrice > 0:
			p.SetAvgEntryPrice(eventPrice)
		case orderAvg > 0 && orderFilled > 0 && orderFilled < absQty:
			// Partial open from this order only — cumulative avg is pure open.
			p.SetAvgEntryPrice(orderAvg)
		case orderAvg > 0 && orderFilled == 0:
			// No fill size info — best effort.
			p.SetAvgEntryPrice(orderAvg)
			// else orderFilled > absQty (overshoot flip) or unknown: leave 0
		}
		// MarketValue / UnrealizedPL stay zero until REST reconcile.
		s.positions[symbol] = p
		return
	}

	// Same side: size changed. Stale uPL/market value must not be shown as live.
	// Recompute avg entry on scale-in using the *incremental* fill price —
	// Order.FilledAvgPrice is cumulative and must not be used as the last print.
	prevAbs := float64(prev.Qty)
	prevAvg := float64(prev.AvgEntryPrice)
	switch {
	case absQty > prevAbs:
		// Prefer this event's fill size over broker position delta so concurrent
		// fills for the same symbol do not attribute the full jump to this price.
		posDelta := absQty - prevAbs
		added := scaleInAddedQty(posDelta, eventQty, orderFilled, prevFilledQty)
		if incr, ok := incrementalFillPrice(o, prevFilledQty, prevFilledAvg, eventPrice); ok && prevAvg > 0 && added > 0 {
			// Weight this fill at its incremental price. Concurrent residual
			// (posDelta > added) is attributed at prevAvg so displayed avg
			// stays consistent with broker absQty until REST reconcile.
			residual := posDelta - added
			p.SetAvgEntryPrice((prevAbs*prevAvg + added*incr + residual*prevAvg) / absQty)
		} else if orderAvg > 0 && orderFilled == absQty {
			// Position is entirely from this order — trust broker cumulative avg.
			p.SetAvgEntryPrice(orderAvg)
		} else if orderAvg > 0 && prevAvg == 0 {
			p.SetAvgEntryPrice(orderAvg)
		} else {
			p.SetAvgEntryPrice(prevAvg)
		}
	case orderAvg > 0 && prevAvg == 0:
		p.SetAvgEntryPrice(orderAvg)
	default:
		p.SetAvgEntryPrice(prevAvg)
	}
	s.positions[symbol] = p
}

// scaleInAddedQty returns the quantity to weight into a scale-in average.
// Prefers event fill size, then order filled delta, then position delta.
// Never attributes more than the observed position growth.
func scaleInAddedQty(posDelta, eventQty, orderFilled, prevFilledQty float64) float64 {
	added := posDelta
	switch {
	case eventQty > 0:
		added = eventQty
	case orderFilled > prevFilledQty:
		added = orderFilled - prevFilledQty
	}
	if added > posDelta {
		added = posDelta
	}
	if added < 0 {
		return 0
	}
	return added
}

// incrementalFillPrice returns the price of the print that produced this
// update. Prefers the event-level price; otherwise derives it from the
// change in the order's cumulative filled_avg_price / filled_qty.
func incrementalFillPrice(o alpaca.Order, prevFilledQty, prevFilledAvg, eventPrice float64) (float64, bool) {
	if eventPrice > 0 {
		return eventPrice, true
	}
	filled := float64(o.FilledQty)
	avg := float64(o.FilledAvgPrice)
	if avg <= 0 || filled <= 0 {
		return 0, false
	}
	// First observed fill on this order: cumulative avg equals the print price.
	if prevFilledQty <= 0 || filled <= prevFilledQty {
		return avg, true
	}
	incrQty := filled - prevFilledQty
	if incrQty <= 0 {
		return 0, false
	}
	// filled*avg = prevFilled*prevAvg + incrQty*incrPrice
	incrPrice := (filled*avg - prevFilledQty*prevFilledAvg) / incrQty
	if incrPrice <= 0 {
		return 0, false
	}
	return incrPrice, true
}

// Account returns the cached account (hasAcct=false until reconciled).
func (s *Store) Account() (alpaca.Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.account, s.hasAcct
}

// Reconciled reports whether at least one REST snapshot has landed. Order
// entry gates on this (see ui.Model.orderIntent); flatten/panic bypass it.
func (s *Store) Reconciled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.reconciled
}

// Snapshot returns a single-copy view of the store for rendering. The UI
// calls this once per frame instead of Positions()/OpenOrders()/Account()/
// PnL() separately (each takes the mutex and allocates). One copy here =
// one mutex acquisition and one set of allocations per frame.
func (s *Store) Snapshot() BookSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := BookSnapshot{
		Account:     s.account,
		HasAccount:  s.hasAcct,
		Reconciled:  s.reconciled,
		Positions:   make([]alpaca.Position, 0, len(s.positions)),
		Orders:      make([]alpaca.Order, 0, len(s.orders)),
		PositionQty: make(map[string]float64, len(s.positions)),
	}
	for _, p := range s.positions {
		out.Positions = append(out.Positions, p)
		out.PositionQty[p.Symbol] = SignedPositionQty(p)
	}
	for _, o := range s.orders {
		out.Orders = append(out.Orders, o)
	}
	out.PnL = s.pnlLocked()
	return out
}

// PositionQty returns the signed quantity for symbol (negative = short).
func (s *Store) PositionQty(symbol string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.positions[symbol]
	if !ok {
		return 0
	}
	return SignedPositionQty(p)
}

// normalizePosition stores abs qty with side long|short. Alpaca REST often
// returns negative qty for shorts; WS fill path already uses absolute qty.
func normalizePosition(p alpaca.Position) alpaca.Position {
	q := float64(p.Qty)
	if q < 0 {
		p.Side = "short"
		p.SetQty(-q)
		return p
	}
	if q > 0 && p.Side == "" {
		p.Side = "long"
	}
	return p
}

// WorkingSideQty returns remaining unfilled open-order qty for symbol on
// side ("buy" or "sell"). Implements risk.WorkingLookup so stacked resting
// orders on the same side cannot silently exceed max_position_qty.
// Only open-like statuses are counted (defense against terminal rows that
// have not yet been deleted from the map). Order sides are normalized
// (sell_short → sell) so non-canonical broker strings still count.
func (s *Store) WorkingSideQty(symbol, side string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workingSideQtyLocked(symbol, side)
}

// Exposure returns signed position qty and same-side working buy/sell under
// one store lock so risk projection cannot see a torn (position, working)
// pair between two separate RLock acquisitions. Implements risk.ExposureLookup.
func (s *Store) Exposure(symbol string) (pos, workingBuy, workingSell float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.positions[symbol]; ok {
		pos = SignedPositionQty(p)
	}
	workingBuy = s.workingSideQtyLocked(symbol, "buy")
	workingSell = s.workingSideQtyLocked(symbol, "sell")
	return pos, workingBuy, workingSell
}

// workingSideQtyLocked is the unlocked body of WorkingSideQty. Caller holds s.mu.
func (s *Store) workingSideQtyLocked(symbol, side string) float64 {
	want, ok := normalizeTradeSide(side)
	if !ok {
		want = strings.ToLower(strings.TrimSpace(side))
	}
	var total float64
	for _, o := range s.orders {
		if o.Symbol != symbol {
			continue
		}
		oSide, ok := normalizeTradeSide(o.Side)
		if !ok {
			oSide = strings.ToLower(strings.TrimSpace(o.Side))
		}
		if oSide != want {
			continue
		}
		if !isWorkingOrderStatus(o.Status) {
			continue
		}
		qty := float64(o.Qty) - float64(o.FilledQty)
		if qty > 0 {
			total += qty
		}
	}
	return total
}

// isWorkingOrderStatus reports whether an order status still represents
// resting / in-flight risk. Terminal statuses are denylisted so unknown
// open-like values (including broker/"open" fixtures) still count toward
// WorkingSideQty rather than failing open on the risk rail.
func isWorkingOrderStatus(status string) bool {
	switch strings.ToLower(status) {
	case "filled", "canceled", "cancelled", "expired", "rejected",
		"done_for_day", "replaced":
		return false
	default:
		// new, accepted, pending_*, partial_fill, held, suspended, open, "", …
		return true
	}
}

// WorkingQty returns net signed open-order quantity for symbol (+buy, −sell)
// using remaining unfilled size. Convenience for display; risk uses WorkingSideQty.
func (s *Store) WorkingQty(symbol string) float64 {
	return s.WorkingSideQty(symbol, "buy") - s.WorkingSideQty(symbol, "sell")
}

// Position returns the cached position for symbol, or nil.
func (s *Store) Position(symbol string) *alpaca.Position {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.positions[symbol]; ok {
		cp := p
		return &cp
	}
	return nil
}

// Positions returns all cached positions.
func (s *Store) Positions() []alpaca.Position {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]alpaca.Position, 0, len(s.positions))
	for _, p := range s.positions {
		out = append(out, p)
	}
	return out
}

// OpenOrders returns all cached open orders.
func (s *Store) OpenOrders() []alpaca.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]alpaca.Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out
}

// pnlLocked returns AccountPnL under s.mu (caller must hold at least RLock).
func (s *Store) pnlLocked() AccountPnL {
	out := AccountPnL{HasDay: s.hasDayPnL, HasWeek: s.hasWeekPnL}
	if !s.pnlUpdatedAt.IsZero() && time.Since(s.pnlUpdatedAt) > realizedPnLStaleAfter {
		out.HasDay = false
		out.HasWeek = false
		return out
	}
	if s.hasDayPnL {
		out.Day = s.dayRealized
	}
	if s.hasWeekPnL {
		out.Week = s.weekRealized
	}
	out.Partial = s.pnlPartial
	out.PartialNote = s.pnlPartialNote
	return out
}

// PnL returns fill-based realized day/week P&L from the last successful
// RefreshRealizedPnL. Stale samples (no refresh for realizedPnLStaleAfter)
// are hidden.
func (s *Store) PnL() AccountPnL {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pnlLocked()
}

// SetRealizedPnL installs fill-computed day/week realized totals (full sample).
func (s *Store) SetRealizedPnL(day, week float64) {
	s.SetRealizedPnLSample(day, week, nil)
}

// SetRealizedPnLSample installs fill-computed day/week realized totals.
// excluded, when non-empty, marks the sample as partial (some symbols dropped).
// Uses time.Now() as the sample clock; concurrent async paths should call
// setRealizedPnLSampleIfNewer with the compute-start timestamp so a slow
// REST refresh cannot overwrite a newer WS recompute.
func (s *Store) SetRealizedPnLSample(day, week float64, excluded []string) {
	s.mu.Lock()
	s.setRealizedPnLSampleIfNewerLocked(day, week, excluded, time.Now())
	s.mu.Unlock()
}

// setRealizedPnLSampleIfNewerLocked publishes day/week totals only when
// sampleAt is not strictly older than the last published sample. Caller holds s.mu.
// Equal timestamps still publish (same-tick refresh / recompute).
func (s *Store) setRealizedPnLSampleIfNewerLocked(day, week float64, excluded []string, sampleAt time.Time) bool {
	if sampleAt.IsZero() {
		sampleAt = time.Now()
	}
	if !s.pnlUpdatedAt.IsZero() && sampleAt.Before(s.pnlUpdatedAt) {
		return false
	}
	s.dayRealized = day
	s.weekRealized = week
	s.hasDayPnL = true
	s.hasWeekPnL = true
	s.pnlUpdatedAt = sampleAt
	s.pnlPartial = len(excluded) > 0
	if s.pnlPartial {
		s.pnlPartialNote = formatExcluded(excluded)
	} else {
		s.pnlPartialNote = ""
	}
	return true
}

// ClearRealizedPnL hides day/week realized figures.
func (s *Store) ClearRealizedPnL() {
	s.mu.Lock()
	s.dayRealized = 0
	s.weekRealized = 0
	s.hasDayPnL = false
	s.hasWeekPnL = false
	s.pnlUpdatedAt = time.Time{}
	s.pnlPartial = false
	s.pnlPartialNote = ""
	s.mu.Unlock()
}

// Refresher fetches a snapshot from REST.
type Refresher interface {
	Account(ctx context.Context) (alpaca.Account, error)
	Positions(ctx context.Context) ([]alpaca.Position, error)
	OpenOrders(ctx context.Context, symbol string) ([]alpaca.Order, error)
}

// RealizedPnLRefresher supplies closed-order / FILL history for realized PnL.
// Prefer Fills (per-execution); ClosedOrders is the fallback path.
type RealizedPnLRefresher interface {
	Fills(ctx context.Context, after, until time.Time) ([]alpaca.Fill, error)
	ClosedOrders(ctx context.Context, after, until time.Time) ([]alpaca.Order, error)
}

// Refresh pulls account/positions/orders from REST into the store.
// Realized day/week PnL is intentionally not fetched here — use
// RefreshRealizedPnL on a slower cadence (fill history is heavier).
func (s *Store) Refresh(ctx context.Context, r Refresher) error {
	acct, err := r.Account(ctx)
	if err != nil {
		return err
	}
	pos, err := r.Positions(ctx)
	if err != nil {
		return err
	}
	ord, err := r.OpenOrders(ctx, "")
	if err != nil {
		return err
	}
	s.Reconcile(acct, pos, ord)
	return nil
}

// RefreshRealizedPnL recomputes rday/rwk from the closed order / FILL stream.
//
//	1. Fetch fills (or closed orders) from WeekStart−lookback → now so
//	   average-cost inventory is warm for mid-week closes. Subsequent calls
//	   only fetch the delta past the last cached fill when the lookback
//	   window is unchanged (empty successful caches re-poll a short tail only).
//	2. Walk oldest→newest with REST open-position seeds for pre-lookback lots;
//	   realize on size reductions / flips. Seeds come from the last REST
//	   reconcile (not live WS positions) so fills and seeds stay aligned.
//	3. Bucket by DayStart / WeekStart (current 24/5 trading day & week).
//	4. Symbols that cannot be reconciled (ghost inventory from pre-lookback
//	   full closes without seeds, or degenerate seed/fill mismatch) are
//	   excluded from the sum; other symbols still publish. Same-side
//	   REST-vs-fill lag trusts fills; opposite-side books bridge when a
//	   single pre-window open explains the seed.
//	5. If nothing trustworthy remains, leave any prior sample in place (same
//	   as fetch errors) rather than clearing a good rday/rwk; PnL() still
//	   hides figures older than realizedPnLStaleAfter.
//
// Transient fetch errors and inventory inconsistency leave the prior sample.
// ClosedOrders is cold-start only: it filters on submitted_at (misses older
// GTC fills) and must not overwrite a good fill-derived sample. The fill
// cache is never cleared on Fills errors — only a successful Fills fetch
// replaces it.
func (s *Store) RefreshRealizedPnL(ctx context.Context, r RealizedPnLRefresher) error {
	s.realizedRefreshMu.Lock()
	defer s.realizedRefreshMu.Unlock()

	now := time.Now()
	weekStart := session.WeekStart(now)
	dayStart := session.DayStart(now)
	after := weekStart.Add(-fillHistoryLookback)
	until := now.Add(time.Minute)

	s.mu.RLock()
	// Prefer last REST snapshot for seeds; fall back to live positions only
	// before the first Reconcile (should not happen on the 60s path after boot).
	seedSrc := s.restPositions
	if len(seedSrc) == 0 && !s.reconciled {
		seedSrc = s.positions
	}
	seeds := SeedsFromPositions(positionsSlice(seedSrc))
	hints := costBasisHintsLocked(s.retainedBasis, seeds)
	s.mu.RUnlock()

	var windows RealizedWindows
	var ok bool
	var excluded []string
	// sampleAt is the start of this refresh so a slow REST walk cannot
	// overwrite a newer WS recompute that finished while we were fetching.
	sampleAt := now
	restFills, ferr := s.fetchFillsCached(ctx, r, after, until)
	if ferr == nil {
		// Merge trading-WS fills not yet present in the activities feed so a
		// just-closed round-trip updates rday/rwk without waiting for lag.
		s.mu.RLock()
		fills := mergeWSFills(restFills, s.wsFills)
		s.mu.RUnlock()
		// Empty FILL list with open positions that have cost basis is not
		// authoritative zeros — inventory cannot be correct without history
		// or seed-only path still needs a successful walk. Seeds alone with
		// zero fills yield zero realized and consistent inventory (ok).
		// Unreconcilable symbols are dropped from the walk so one bad name
		// does not blank account-wide rday/rwk.
		windows, ok, excluded = RealizedFromFillsWithHints(fills, dayStart, weekStart, seeds, hints)
		if !ok {
			// Keep prior sample (mirrors fetch-error path). A transient
			// reconcile skew must not blank trustworthy rday/rwk.
			return fmt.Errorf("realized pnl: inventory inconsistent with open positions (incomplete fill history)")
		}
		// Successful empty fill response while seeded open positions exist is
		// fine (long-held, no closes) — realized stays 0 with ok==true.
		s.mu.Lock()
		s.setRealizedPnLSampleIfNewerLocked(windows.Day, windows.Week, excluded, sampleAt)
		// Drop WS provisional fills fully covered by REST so they cannot
		// re-merge if a later fetch is sparse.
		s.wsFills = pruneWSFillsCovered(s.wsFills, restFills)
		s.mu.Unlock()
		if len(excluded) > 0 {
			// Soft warning: totals published but may undercount excluded names.
			// main's logRealizedErr rate-limits identical messages.
			return fmt.Errorf("realized pnl: partial sample (excluded unreconcilable: %s)", formatExcluded(excluded))
		}
		return nil
	}

	// Prefer a prior non-stale fill-derived sample over ClosedOrders, which
	// can miss fills whose submitted_at is outside the lookback and would
	// publish incomplete totals. Do not clear the fill cache: the next
	// successful Fills fetch reuses or refreshes it.
	s.mu.RLock()
	hasPrior := (s.hasDayPnL || s.hasWeekPnL) &&
		!s.pnlUpdatedAt.IsZero() &&
		time.Since(s.pnlUpdatedAt) <= realizedPnLStaleAfter
	// Partial Fills progress is checkpointed into fillCache; do not fall
	// through to ClosedOrders which would ignore that high-water mark and
	// may publish incomplete totals on cold start.
	partialFills := len(s.fillCache) > 0
	s.mu.RUnlock()
	if hasPrior {
		return fmt.Errorf("realized pnl fills: %w (kept prior sample; closed-order fallback skipped)", ferr)
	}
	if partialFills {
		return fmt.Errorf("realized pnl fills incomplete: %w (fill cache checkpointed; closed-order fallback skipped)", ferr)
	}

	// Cold start only: no trustworthy sample yet and no partial fill cache.
	ords, oerr := r.ClosedOrders(ctx, after, until)
	if oerr != nil {
		return fmt.Errorf("realized pnl fills: %v; orders: %w", ferr, oerr)
	}
	windows, ok, excluded = RealizedFromOrdersWithHints(ords, dayStart, weekStart, seeds, hints)
	if !ok {
		return fmt.Errorf("realized pnl: inventory inconsistent with open positions (incomplete order history)")
	}
	s.mu.Lock()
	s.setRealizedPnLSampleIfNewerLocked(windows.Day, windows.Week, excluded, sampleAt)
	s.mu.Unlock()
	if len(excluded) > 0 {
		return fmt.Errorf("realized pnl: partial sample (excluded unreconcilable: %s)", formatExcluded(excluded))
	}
	return nil
}

func formatExcluded(syms []string) string {
	if len(syms) <= maxExcludedListed {
		return strings.Join(syms, ", ")
	}
	head := strings.Join(syms[:maxExcludedListed], ", ")
	return fmt.Sprintf("%s, … (+%d more)", head, len(syms)-maxExcludedListed)
}

// costBasisHintsLocked builds retained-avg hints for symbols not already in
// seeds. Caller holds at least RLock on the store (or owns the maps).
func costBasisHintsLocked(retained map[string]retainedBasis, seeds []PositionSeed) []CostBasisHint {
	if len(retained) == 0 {
		return nil
	}
	seedSym := make(map[string]struct{}, len(seeds))
	for _, s := range seeds {
		seedSym[s.Symbol] = struct{}{}
	}
	out := make([]CostBasisHint, 0, len(retained))
	for sym, rb := range retained {
		if rb.Avg <= 0 {
			continue
		}
		if _, has := seedSym[sym]; has {
			continue
		}
		out = append(out, CostBasisHint{Symbol: sym, Avg: rb.Avg})
	}
	return out
}

// positionsSlice copies map values (caller holds at least RLock).
func positionsSlice(m map[string]alpaca.Position) []alpaca.Position {
	if len(m) == 0 {
		return nil
	}
	out := make([]alpaca.Position, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	return out
}

// fetchFillsCached returns FILL history for [after, until), reusing the
// store's buffer when the lookback window is unchanged and only requesting
// fills after the newest cached timestamp. The returned slice is always a
// dedicated snapshot (never aliases fillCache). Caller must hold
// realizedRefreshMu (or otherwise ensure single-flight) so cache merges do
// not race.
//
// Mid-pagination Fills errors still checkpoint any rows already received into
// fillCache (so the next tick's delta path resumes near the high-water mark)
// but this function returns (nil, err) on those paths — callers never walk
// incomplete history. Only a successful complete fetch returns a non-nil slice
// without error.
func (s *Store) fetchFillsCached(ctx context.Context, r RealizedPnLRefresher, after, until time.Time) ([]alpaca.Fill, error) {
	s.mu.RLock()
	cache := append([]alpaca.Fill(nil), s.fillCache...)
	cacheAfter := s.fillCacheAfter
	lastFull := s.fillCacheLastFull
	s.mu.RUnlock()

	// Full refetch when the week/lookback bound moved, or a periodic full heal
	// is due. Empty caches heal when lastFull is zero or aged (delayed first
	// FILL). Warm caches heal only after a successful full walk ages past the
	// interval — lastFull.IsZero() with a non-empty cache means a partial
	// checkpoint, and the delta path must resume from the high-water mark.
	// Between heals, use the delta path so we do not re-scan 180d every tick.
	forceFull := !cacheAfter.Equal(after)
	if !forceFull && cacheAfter.Equal(after) {
		aged := !lastFull.IsZero() && time.Since(lastFull) >= fillCacheFullRefreshEvery
		if len(cache) == 0 {
			forceFull = lastFull.IsZero() || aged
		} else {
			forceFull = aged
		}
	}

	if forceFull {
		fills, err := r.Fills(ctx, after, until)
		fills = dropEmptyFillIDs(fills)
		if len(fills) > 0 {
			// Checkpoint complete or partial progress so a timeout does not
			// discard multi-page work. Store owns a dedicated copy; caller gets
			// another so neither side can mutate the other via slice aliasing.
			owned := append([]alpaca.Fill(nil), fills...)
			s.mu.Lock()
			s.fillCache = owned
			s.fillCacheAfter = after
			if err == nil {
				s.fillCacheLastFull = time.Now()
			}
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return append([]alpaca.Fill(nil), owned...), nil
		}
		if err != nil {
			return nil, err
		}
		snap := []alpaca.Fill{}
		s.mu.Lock()
		s.fillCache = snap
		s.fillCacheAfter = after
		s.fillCacheLastFull = time.Now()
		s.mu.Unlock()
		return snap, nil
	}

	// Delta: fetch strictly after the last cached fill time (minus a small
	// overlap window so equal-timestamp pages are not missed). Empty cache
	// re-checks a recent tail (fillCacheEmptyRecheck), not the full lookback.
	deltaAfter := after
	if n := len(cache); n > 0 {
		last := cache[0].Timestamp
		for i := 1; i < n; i++ {
			if cache[i].Timestamp.After(last) {
				last = cache[i].Timestamp
			}
		}
		if !last.IsZero() {
			// Overlap 2s so concurrent same-second fills are re-fetched and de-duped.
			deltaAfter = last.Add(-2 * time.Second)
			if deltaAfter.Before(after) {
				deltaAfter = after
			}
		}
	} else {
		deltaAfter = until.Add(-fillCacheEmptyRecheck)
		if deltaAfter.Before(after) {
			deltaAfter = after
		}
	}
	delta, err := r.Fills(ctx, deltaAfter, until)
	delta = dropEmptyFillIDs(delta)
	if len(delta) > 0 || err == nil {
		merged := mergeFillsByID(cache, delta)
		// Drop anything older than the current lookback bound.
		pruned := pruneFillsBefore(merged, after)
		// Store and return dedicated copies (never alias each other).
		owned := append([]alpaca.Fill(nil), pruned...)
		s.mu.Lock()
		s.fillCache = owned
		s.fillCacheAfter = after
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return append([]alpaca.Fill(nil), owned...), nil
	}
	return nil, err
}

// mergeFillsByID appends newer into base, preferring the first occurrence of
// each activity id (stable). Fills with empty id are dropped (same policy as
// ClosedOrders): they cannot be de-duped and the delta path's overlap window
// would double-count them.
func mergeFillsByID(base, newer []alpaca.Fill) []alpaca.Fill {
	if len(newer) == 0 {
		// Still drop empty ids from base so a polluted cache is cleaned.
		return dropEmptyFillIDs(base)
	}
	seen := make(map[string]struct{}, len(base)+len(newer))
	out := make([]alpaca.Fill, 0, len(base)+len(newer))
	for _, f := range base {
		if f.ID == "" {
			continue
		}
		if _, ok := seen[f.ID]; ok {
			continue
		}
		seen[f.ID] = struct{}{}
		out = append(out, f)
	}
	for _, f := range newer {
		if f.ID == "" {
			continue
		}
		if _, ok := seen[f.ID]; ok {
			continue
		}
		seen[f.ID] = struct{}{}
		out = append(out, f)
	}
	return out
}

func dropEmptyFillIDs(fills []alpaca.Fill) []alpaca.Fill {
	if len(fills) == 0 {
		return fills
	}
	out := make([]alpaca.Fill, 0, len(fills))
	for _, f := range fills {
		if f.ID == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func pruneFillsBefore(fills []alpaca.Fill, after time.Time) []alpaca.Fill {
	if after.IsZero() || len(fills) == 0 {
		return fills
	}
	out := make([]alpaca.Fill, 0, len(fills))
	for _, f := range fills {
		// Drop fills strictly before the current lookback bound.
		if !f.Timestamp.IsZero() && f.Timestamp.Before(after) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// RefreshWeekPnL is a compatibility alias for RefreshRealizedPnL (both day
// and week are computed together from fills).
func (s *Store) RefreshWeekPnL(ctx context.Context, r RealizedPnLRefresher) error {
	return s.RefreshRealizedPnL(ctx, r)
}

// noteWSFillLocked records one trading-WS execution for immediate realized
// PnL. Returns true when a new fill was stored. Caller holds s.mu.
func (s *Store) noteWSFillLocked(o alpaca.Order, prevFilledQty, prevFilledAvg, eventPrice, eventQty float64) bool {
	if o.ID == "" || o.Symbol == "" {
		return false
	}
	side, ok := normalizeTradeSide(o.Side)
	if !ok {
		return false
	}
	qty := eventQty
	if qty <= 0 {
		delta := float64(o.FilledQty) - prevFilledQty
		if delta > 0 {
			qty = delta
		} else if float64(o.FilledQty) > 0 && prevFilledQty <= 0 {
			qty = float64(o.FilledQty)
		}
	}
	px := eventPrice
	if px <= 0 {
		if incr, ok := incrementalFillPrice(o, prevFilledQty, prevFilledAvg, eventPrice); ok {
			px = incr
		} else {
			px = float64(o.FilledAvgPrice)
		}
	}
	if qty <= 0 || px <= 0 {
		return false
	}
	at := o.FilledAt
	if at.IsZero() {
		at = o.SubmittedAt
	}
	if at.IsZero() {
		at = time.Now()
	}
	// Unique per execution so partial fills stack; REST de-dupes via order qty.
	// Use 'f' FormatFloat so distinct floats never collide via %.8g rounding.
	id := "ws:" + o.ID + ":" +
		strconv.FormatFloat(qty, 'f', -1, 64) + "@" +
		strconv.FormatFloat(px, 'f', -1, 64) + ":" +
		strconv.FormatInt(at.UnixNano(), 10)
	for _, existing := range s.wsFills {
		if existing.ID == id {
			return false
		}
	}
	f := alpaca.Fill{
		ID:        id,
		Symbol:    o.Symbol,
		Side:      side,
		OrderID:   o.ID,
		Timestamp: at,
	}
	f.SetQty(qty)
	f.SetPrice(px)
	s.wsFills = append(s.wsFills, f)
	// Bound growth: drop WS fills older than the fill lookback.
	cutoff := time.Now().Add(-fillHistoryLookback)
	if len(s.wsFills) > 0 {
		kept := s.wsFills[:0]
		for _, w := range s.wsFills {
			if w.Timestamp.IsZero() || !w.Timestamp.Before(cutoff) {
				kept = append(kept, w)
			}
		}
		s.wsFills = kept
	}
	return true
}

// recomputeRealizedFromCache walks fillCache + wsFills and publishes rday/rwk
// when inventory reconciles. Does not fetch REST. Snapshots under the lock
// and runs the sort/inventory walk outside so ApplyUpdate's O(1) path does
// not hold s.mu across the full history.
func (s *Store) recomputeRealizedFromCache() {
	now := time.Now()
	weekStart := session.WeekStart(now)
	dayStart := session.DayStart(now)

	s.mu.Lock()
	seedSrc := s.restPositions
	if len(seedSrc) == 0 && !s.reconciled {
		seedSrc = s.positions
	}
	// Prefer live position seeds for the WS-fast path: REST restPositions can
	// lag the fill by up to the 5s reconcile, and a just-opened name would
	// otherwise look like ghost inventory and block publishing.
	if len(s.positions) > 0 {
		seedSrc = s.positions
	}
	seeds := SeedsFromPositions(positionsSlice(seedSrc))
	// Also retain basis from live positions so a full close still costs exits
	// after the map drops the symbol (ApplyUpdate may have already deleted it).
	if s.retainedBasis == nil {
		s.retainedBasis = make(map[string]retainedBasis)
	}
	for _, p := range s.positions {
		if avg := float64(p.AvgEntryPrice); avg > 0 && p.Symbol != "" {
			s.retainedBasis[p.Symbol] = retainedBasis{Avg: avg, Updated: now}
		}
	}
	hints := costBasisHintsLocked(s.retainedBasis, seeds)
	// Copy fill slices so the walk can release the lock.
	restSnap := append([]alpaca.Fill(nil), s.fillCache...)
	wsSnap := append([]alpaca.Fill(nil), s.wsFills...)
	s.mu.Unlock()

	fills := mergeWSFills(restSnap, wsSnap)
	windows, ok, excluded := RealizedFromFillsWithHints(fills, dayStart, weekStart, seeds, hints)
	if !ok {
		return
	}
	// Publish only if no newer sample landed while we walked unlocked.
	s.mu.Lock()
	s.setRealizedPnLSampleIfNewerLocked(windows.Day, windows.Week, excluded, now)
	s.mu.Unlock()
}

// fillFingerprint is a coarse execution key used when REST FILL activities
// omit order_id (common on the activities endpoint) so WS provisional rows
// can still be de-duplicated against REST.
type fillFingerprint struct {
	symbol string
	side   string
	qty    float64
	price  float64
}

func fillFP(f alpaca.Fill) fillFingerprint {
	side, ok := normalizeTradeSide(f.Side)
	if !ok {
		side = strings.ToLower(f.Side)
	}
	return fillFingerprint{
		symbol: f.Symbol,
		side:   side,
		qty:    float64(f.Qty),
		price:  float64(f.Price),
	}
}

// fillSymSideKey is a residual-qty pool key for de-duping WS partials against
// REST aggregated fills when order_id is missing or prices do not match.
func fillSymSideKey(symbol, side string) string {
	if n, ok := normalizeTradeSide(side); ok {
		side = n
	} else {
		side = strings.ToLower(side)
	}
	return symbol + "|" + side
}

// residualMatchWindow bounds how far a WS fill timestamp may sit from a
// no-order_id REST fill when claiming residual symbol|side qty. Without this,
// concurrent same-side orders can silently undercount realized PnL: an earlier
// REST aggregate absorbs a later WS order's qty, and prune drops the provisional
// so the heal path never recovers it. Prefer temporary double-count over silent
// undercount when times do not line up.
const residualMatchWindow = 2 * time.Minute

// residualBucket is no-order_id REST qty for one symbol|side, with timestamps
// of the REST rows that contributed (for proximity gating).
type residualBucket struct {
	qty   float64
	times []time.Time
}

func addResidual(m map[string]*residualBucket, f alpaca.Fill) {
	key := fillSymSideKey(f.Symbol, f.Side)
	b := m[key]
	if b == nil {
		b = &residualBucket{}
		m[key] = b
	}
	b.qty += float64(f.Qty)
	if !f.Timestamp.IsZero() {
		b.times = append(b.times, f.Timestamp)
	}
}

// residualCovers reports whether residual can absorb qty for a WS fill at at.
// Requires remaining qty and, when both sides have timestamps, time proximity
// to at least one contributing REST row. Missing WS or REST timestamps refuse
// residual match (fingerprint / order_id paths still apply).
func residualCovers(b *residualBucket, qty float64, at time.Time) bool {
	if b == nil || b.qty < qty-invQtyTol {
		return false
	}
	if at.IsZero() || len(b.times) == 0 {
		// Cannot prove same execution envelope — keep WS (visible double-count
		// until order_id or fingerprint coverage lands).
		return false
	}
	for _, t := range b.times {
		d := at.Sub(t)
		if d < 0 {
			d = -d
		}
		if d <= residualMatchWindow {
			return true
		}
	}
	return false
}

func consumeResidual(b *residualBucket, qty float64) {
	if b == nil {
		return
	}
	b.qty -= qty
	if b.qty < 0 {
		b.qty = 0
	}
}

// mergeWSFills combines REST activity fills with trading-WS provisional fills.
//
//	- If REST order_id coverage is complete for a WS order, drop the WS rows.
//	- If REST has partial order_id coverage (some activities landed), prefer the
//	  full WS set for that order and drop REST rows for that order_id.
//	- Otherwise match WS rows to REST by symbol/side/qty/price fingerprint
//	  **only against REST rows without order_id** (order_id REST is already
//	  handled above; fingerprinting those would cross-consume a second concurrent
//	  same-size hotkey fill for a different order_id and undercount rday/rwk).
//	- Then residual symbol|side qty from no-order_id REST, only when timestamps
//	  fall within residualMatchWindow so concurrent same-side orders cannot
//	  undercount realized PnL.
func mergeWSFills(rest, ws []alpaca.Fill) []alpaca.Fill {
	if len(ws) == 0 {
		return rest
	}
	restQty := make(map[string]float64)
	for _, f := range rest {
		if f.OrderID != "" {
			restQty[f.OrderID] += float64(f.Qty)
		}
	}
	wsQty := make(map[string]float64)
	for _, f := range ws {
		if f.OrderID != "" {
			wsQty[f.OrderID] += float64(f.Qty)
		}
	}
	// Only prefer WS when REST already has *some* order_id rows for the order
	// but not enough qty (partial activity). Bare WS with no REST order_id
	// match falls through to fingerprint / residual de-dupe against REST rows.
	preferWS := make(map[string]struct{})
	for id, wq := range wsQty {
		rq := restQty[id]
		if rq > invQtyTol && wq > rq+invQtyTol {
			preferWS[id] = struct{}{}
		}
	}
	// Fingerprint + residual pools are only no-order_id REST so order_id-covered
	// REST cannot swallow WS fills for a different order (same symbol+side+qty+px).
	restFP := make(map[fillFingerprint]int)
	restSymSide := make(map[string]*residualBucket)
	out := make([]alpaca.Fill, 0, len(rest)+len(ws))
	for _, f := range rest {
		if f.OrderID != "" {
			if _, ok := preferWS[f.OrderID]; ok {
				continue
			}
		}
		out = append(out, f)
		if f.OrderID == "" {
			restFP[fillFP(f)]++
			addResidual(restSymSide, f)
		}
	}
	for _, f := range ws {
		if f.OrderID != "" {
			if _, ok := preferWS[f.OrderID]; ok {
				out = append(out, f)
				continue
			}
			if restQty[f.OrderID] >= wsQty[f.OrderID]-invQtyTol {
				continue
			}
		}
		fp := fillFP(f)
		key := fillSymSideKey(f.Symbol, f.Side)
		qty := float64(f.Qty)
		if restFP[fp] > 0 {
			restFP[fp]--
			// Fingerprint matched a no-order_id REST row already in residual.
			if b := restSymSide[key]; b != nil && b.qty > 0 {
				consumeResidual(b, qty)
			}
			continue
		}
		// Residual: REST aggregated fill without order_id covers WS partials
		// of the same execution envelope (time-gated).
		if b := restSymSide[key]; residualCovers(b, qty, f.Timestamp) {
			consumeResidual(b, qty)
			continue
		}
		out = append(out, f)
	}
	return out
}

// pruneWSFillsCovered drops WS fills covered by REST (order_id qty,
// fingerprint, or time-gated residual symbol|side qty from no-order_id REST).
// Fingerprint/residual index only no-order_id REST (same as mergeWSFills) so a
// REST fill for o1 cannot prune a concurrent identical-size WS fill for o2.
func pruneWSFillsCovered(ws, rest []alpaca.Fill) []alpaca.Fill {
	if len(ws) == 0 {
		return ws
	}
	restQty := make(map[string]float64)
	restFP := make(map[fillFingerprint]int)
	restSymSide := make(map[string]*residualBucket)
	for _, f := range rest {
		if f.OrderID != "" {
			restQty[f.OrderID] += float64(f.Qty)
			continue
		}
		restFP[fillFP(f)]++
		addResidual(restSymSide, f)
	}
	wsQty := make(map[string]float64)
	for _, f := range ws {
		if f.OrderID != "" {
			wsQty[f.OrderID] += float64(f.Qty)
		}
	}
	out := make([]alpaca.Fill, 0, len(ws))
	for _, f := range ws {
		if f.OrderID != "" && restQty[f.OrderID] >= wsQty[f.OrderID]-invQtyTol {
			continue
		}
		fp := fillFP(f)
		key := fillSymSideKey(f.Symbol, f.Side)
		qty := float64(f.Qty)
		if restFP[fp] > 0 {
			restFP[fp]--
			if b := restSymSide[key]; b != nil && b.qty > 0 {
				consumeResidual(b, qty)
			}
			continue
		}
		if b := restSymSide[key]; residualCovers(b, qty, f.Timestamp) {
			consumeResidual(b, qty)
			continue
		}
		out = append(out, f)
	}
	return out
}
