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

	// changed is closed and replaced on every mutation (broadcast).
	changed chan struct{}
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{
		positions: make(map[string]alpaca.Position),
		orders:    make(map[string]alpaca.Order),
		changed:   make(chan struct{}),
	}
}

// Changed returns a channel closed on the next mutation. Callers should
// re-fetch after every wait (classic condition broadcast).
func (s *Store) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

func (s *Store) notify() {
	close(s.changed)
	s.changed = make(chan struct{})
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
	s.notify()
}

// ApplyUpdate folds one trading-WS trade update into the store.
func (s *Store) ApplyUpdate(u alpaca.TradeUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := u.Order
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
	s.notify()
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
		return -p.Qty
	}
	return p.Qty
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
