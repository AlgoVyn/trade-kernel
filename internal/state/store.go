// Package state keeps the local view of account, positions, and open
// orders, reconciled from REST at startup and updated from the trading
// WebSocket stream. Account day/week PnL is realized-oriented: equity
// deltas are snapshotted with open marks at REST time so WebSocket fills
// cannot desync the strip.
package state

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"trade-kernel/internal/alpaca"
)

// weekPnLStaleAfter hides week PnL when portfolio history has not been
// refreshed successfully for this long (avoids multi-hour frozen "wk").
const weekPnLStaleAfter = 30 * time.Minute

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
func (b BookSnapshot) SignedQty(symbol string) float64 {
	for i := range b.Positions {
		if b.Positions[i].Symbol != symbol {
			continue
		}
		q := float64(b.Positions[i].Qty)
		if b.Positions[i].Side == "short" {
			return -q
		}
		return q
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

// AccountPnL is cached day/week account profit-and-loss with open marks// stripped at the last REST snapshot (not recomputed from live WS positions).
//
//	Day  = (equity − last_equity) − Σ unrealized_intraday_pl
//	Week = (equity − week base_value) − Σ signedQty × (mark_now − mark_week_start)
//
// Day is realized since prior close (intraday open marks only). Week is
// realized over the trailing 1W portfolio-history window: the equity change
// across the window minus the unrealized that accrued inside it (per-position
// mark movement from the window-start daily close, at current size). It never
// subtracts lifetime unrealized vs cost — gains earned before the window are
// not this week's. Positions whose qty changed inside the window are
// approximated at current size (bounded error, sane sign). Figures move on
// REST reconcile / history refresh, not on quote drift or fill rewrites of
// position mark fields.
type AccountPnL struct {
	Day     float64
	Week    float64
	HasDay  bool
	HasWeek bool
}

// Store is a mutex-guarded cache of account state.
type Store struct {
	mu sync.RWMutex

	account alpaca.Account
	hasAcct bool
	// reconciled is true after at least one successful Reconcile. Order entry
	// is blocked until this flips so an operator never submits against an
	// empty/stale view (a failed startup Refresh would otherwise show "flat"
	// while real positions exist). Flatten/panic bypass the gate since they
	// read PositionQty live and fall back to DELETE /v2/positions.
	reconciled bool
	positions  map[string]alpaca.Position
	orders     map[string]alpaca.Order // keyed by order ID

	// Day: raw equity change and open-intraday snap from last Reconcile.
	dayChange        float64
	openIntradaySnap float64
	hasDayPnL        bool
	// Week: portfolio-history window base equity, equity at refresh, and
	// the window-accrued unrealized strip — all snapshotted at SetWeekPnL.
	weekBase      float64
	weekEquity    float64
	weekStrip     float64
	hasWeekPnL    bool
	weekUpdatedAt time.Time
	// restPositions is a copy of the positions map as of the last
	// Reconcile. RefreshWeekPnL prices the week strip from it rather than
	// the live map so a WS fill that zeroes mark fields between reconciles
	// cannot corrupt the strip.
	restPositions []alpaca.Position
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{
		positions: make(map[string]alpaca.Position),
		orders:    make(map[string]alpaca.Order),
	}
}

// Reconcile replaces the store with a fresh REST snapshot.
// Day equity change and open mark sums are snapshotted here so PnL() does
// not re-read mark fields that WebSocket fills zero out. The REST positions
// are also kept as a separate copy so RefreshWeekPnL never pairs history
// with a WS-zeroed positions map.
func (s *Store) Reconcile(acct alpaca.Account, positions []alpaca.Position, orders []alpaca.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = acct
	s.hasAcct = true
	s.reconciled = true
	s.positions = make(map[string]alpaca.Position, len(positions))
	for _, p := range positions {
		s.positions[p.Symbol] = p
	}
	s.restPositions = append([]alpaca.Position(nil), positions...)
	s.orders = make(map[string]alpaca.Order, len(orders))
	for _, o := range orders {
		s.orders[o.ID] = o
	}
	// Raw day equity change (realized + open intraday marks) + snap for strip.
	if float64(acct.LastEquity) > 0 {
		s.dayChange = float64(acct.Equity) - float64(acct.LastEquity)
		s.openIntradaySnap = openIntradayUnrealized(s.positions)
		s.hasDayPnL = true
	} else {
		// Missing/zero last_equity: hide day rather than show a stale figure.
		s.dayChange = 0
		s.openIntradaySnap = 0
		s.hasDayPnL = false
	}
}

// ApplyUpdate folds one trading-WS trade update into the store.
//
// Orders are updated immediately. On fill / partial_fill, the position for
// the order's symbol is also applied from PositionQty when Alpaca includes
// it, so risk checks and flatten sizing stay current without waiting for
// the 5 s REST reconcile.
func (s *Store) ApplyUpdate(u alpaca.TradeUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	case "fill", "canceled", "cancelled", "expired", "rejected", "done_for_day":
		delete(s.orders, o.ID)
	case "partial_fill":
		o.Status = u.Event
		s.orders[o.ID] = o
	default:
		// new, accepted, pending_new, replaced, etc.
		s.orders[o.ID] = o
	}

	// PositionQty is present on fill events (and often partial_fill).
	// Skip when the field is omitted/null — 0 would otherwise wipe size.
	if (u.Event == "fill" || u.Event == "partial_fill") && u.PositionQty.Valid {
		s.applyPositionFromFill(o.Symbol, u.PositionQty.V, u.Event, o, prevFilledQty, prevFilledAvg, float64(u.Price), float64(u.Qty))
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
		Account:    s.account,
		HasAccount: s.hasAcct,
		Reconciled: s.reconciled,
		Positions:  make([]alpaca.Position, 0, len(s.positions)),
		Orders:     make([]alpaca.Order, 0, len(s.orders)),
		PnL: AccountPnL{
			HasDay:  s.hasDayPnL,
			HasWeek: s.hasWeekPnL,
		},
	}
	for _, p := range s.positions {
		out.Positions = append(out.Positions, p)
	}
	for _, o := range s.orders {
		out.Orders = append(out.Orders, o)
	}
	// Re-derive PnL numbers under the same lock the existing PnL() uses,
	// including the week-staleness check.
	if s.hasDayPnL {
		out.PnL.Day = s.dayChange - s.openIntradaySnap
	}
	if s.hasWeekPnL {
		if !s.weekUpdatedAt.IsZero() && time.Since(s.weekUpdatedAt) > weekPnLStaleAfter {
			out.PnL.HasWeek = false
		} else {
			out.PnL.Week = (s.weekEquity - s.weekBase) - s.weekStrip
		}
	}
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
	if p.Side == "short" {
		return -float64(p.Qty)
	}
	return float64(p.Qty)
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

func openIntradayUnrealized(positions map[string]alpaca.Position) float64 {
	var u float64
	for _, p := range positions {
		u += float64(p.UnrealizedIntradayPL)
	}
	return u
}

// PnL returns day/week account PnL with open marks stripped using the
// snapshots taken at the last REST reconcile / week history refresh.
func (s *Store) PnL() AccountPnL {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := AccountPnL{HasDay: s.hasDayPnL, HasWeek: s.hasWeekPnL}
	if s.hasDayPnL {
		out.Day = s.dayChange - s.openIntradaySnap
	}
	if s.hasWeekPnL {
		if !s.weekUpdatedAt.IsZero() && time.Since(s.weekUpdatedAt) > weekPnLStaleAfter {
			out.HasWeek = false
		} else {
			out.Week = (s.weekEquity - s.weekBase) - s.weekStrip
		}
	}
	return out
}

// SetWeekPnL snapshots the week computation inputs: the portfolio-history
// window base equity, the account equity at refresh time, and the
// window-accrued unrealized strip. Snapshotting equity here (rather than
// reading it in PnL) keeps the figure frozen between history refreshes and
// immune to WS fill rewrites of account fields.
func (s *Store) SetWeekPnL(base, strip float64) {
	s.mu.Lock()
	s.weekBase = base
	s.weekStrip = strip
	s.weekEquity = float64(s.account.Equity)
	s.hasWeekPnL = true
	s.weekUpdatedAt = time.Now()
	s.mu.Unlock()
}

// ClearWeekPnL hides week PnL (history failure or empty series).
func (s *Store) ClearWeekPnL() {
	s.mu.Lock()
	s.weekBase = 0
	s.weekEquity = 0
	s.weekStrip = 0
	s.hasWeekPnL = false
	s.weekUpdatedAt = time.Time{}
	s.mu.Unlock()
}

// Refresher fetches a snapshot from REST.
type Refresher interface {
	Account(ctx context.Context) (alpaca.Account, error)
	Positions(ctx context.Context) ([]alpaca.Position, error)
	OpenOrders(ctx context.Context, symbol string) ([]alpaca.Order, error)
}

// PnLRefresher supplies week PnL (portfolio history) plus the daily bars
// used to price the window-start marks of open positions.
type PnLRefresher interface {
	PortfolioHistory(ctx context.Context, period, timeframe string) (alpaca.PortfolioHistory, error)
	Bars(ctx context.Context, symbol, timeframe string, start, end time.Time, limit int, feed string) ([]alpaca.Bar, error)
}

// Refresh pulls account/positions/orders from REST into the store.
// Week portfolio history is intentionally not fetched here — use
// RefreshWeekPnL on a slower cadence so the 5 s reconcile path stays light.
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

// RefreshWeekPnL updates week PnL from portfolio history (1W / 1D).
//
// Week is realized-only over the trailing history window:
//
//	Week = (equity_now − base_value) − Σ signedQty × (mark_now − mark_window_start)
//
// base_value is the window-start equity from Alpaca; mark_window_start is
// each held symbol's last daily close before the window (fetched from the
// 1Day bars endpoint). Only the unrealized that accrued *inside* the window
// is stripped — subtracting lifetime unrealized vs cost (the old behavior)
// double-counted prior weeks' gains and could dwarf the week's figure.
// Positions whose qty changed inside the window are approximated at current
// size. Positions are read from the last REST reconcile copy, never the
// live map (WS fills zero mark fields).
//
// Transient errors (history, marks, unreconciled account) leave the prior
// sample in place; PnL() hides week when the last successful sample is
// older than weekPnLStaleAfter. An empty series clears week (no data to
// show). Day reconcile is unaffected.
func (s *Store) RefreshWeekPnL(ctx context.Context, r PnLRefresher) error {
	h, err := r.PortfolioHistory(ctx, "1W", "1D")
	if err != nil {
		return err
	}
	base := float64(h.BaseValue)
	if len(h.Timestamp) == 0 || base == 0 {
		s.ClearWeekPnL()
		return nil
	}
	windowStart := time.Unix(h.Timestamp[0], 0)

	s.mu.RLock()
	positions := make([]alpaca.Position, len(s.restPositions))
	copy(positions, s.restPositions)
	reconciled := s.reconciled
	equity := float64(s.account.Equity)
	s.mu.RUnlock()
	if !reconciled || equity <= 0 {
		return fmt.Errorf("week pnl: no reconciled account snapshot yet")
	}

	// Window-start marks: base_value is the equity mark one trading day
	// before the first daily sample, so the matching price mark is the
	// last daily close before that day. Ending the bar fetch 24h before
	// the first sample lands on (or before) that prior trading day across
	// weekends and holidays.
	barsEnd := windowStart.Add(-24 * time.Hour)
	barsStart := windowStart.Add(-14 * 24 * time.Hour)
	var strip float64
	for _, p := range positions {
		qty := float64(p.Qty)
		if qty <= 0 {
			continue
		}
		markNow := math.Abs(float64(p.MarketValue)) / qty
		if markNow <= 0 {
			return fmt.Errorf("week pnl: %s missing market value", p.Symbol)
		}
		bars, err := r.Bars(ctx, p.Symbol, "1Day", barsStart, barsEnd, 10, "sip")
		if err != nil {
			return fmt.Errorf("week pnl: %s window-start bars: %w", p.Symbol, err)
		}
		if len(bars) == 0 {
			return fmt.Errorf("week pnl: %s: no daily bars before window start", p.Symbol)
		}
		markStart := float64(bars[len(bars)-1].Close)
		if markStart <= 0 {
			return fmt.Errorf("week pnl: %s: non-positive window-start close", p.Symbol)
		}
		signed := qty
		if p.Side == "short" {
			signed = -qty
		}
		strip += signed * (markNow - markStart)
	}
	s.SetWeekPnL(base, strip)
	return nil
}
