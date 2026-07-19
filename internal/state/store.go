// Package state keeps the local view of account, positions, and open
// orders, reconciled from REST at startup and updated from the trading
// WebSocket stream.
package state

import (
	"context"
	"sync"

	"trade-kernel/internal/alpaca"
)

// Store is a mutex-guarded cache of account state.
type Store struct {
	mu sync.RWMutex

	account   alpaca.Account
	hasAcct   bool
	positions map[string]alpaca.Position
	orders    map[string]alpaca.Order // keyed by order ID
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{
		positions: make(map[string]alpaca.Position),
		orders:    make(map[string]alpaca.Order),
	}
}

// Reconcile replaces the store with a fresh REST snapshot.
func (s *Store) Reconcile(acct alpaca.Account, positions []alpaca.Position, orders []alpaca.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.account = acct
	s.hasAcct = true
	s.positions = make(map[string]alpaca.Position, len(positions))
	for _, p := range positions {
		s.positions[p.Symbol] = p
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

// Refresher fetches a snapshot from REST.
type Refresher interface {
	Account(ctx context.Context) (alpaca.Account, error)
	Positions(ctx context.Context) ([]alpaca.Position, error)
	OpenOrders(ctx context.Context, symbol string) ([]alpaca.Order, error)
}

// Refresh pulls a full snapshot from REST into the store.
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
