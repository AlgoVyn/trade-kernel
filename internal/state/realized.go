package state

import (
	"sort"
	"strings"
	"time"

	"trade-kernel/internal/alpaca"
)

// maxExcludedListed caps how many excluded symbols appear in soft-warning text.
const maxExcludedListed = 8

// RealizedWindows holds realized P&L from closed fills for the current
// trading day and current trading week (not mark-to-market).
type RealizedWindows struct {
	Day  float64
	Week float64
}

// PositionSeed is open-position cost basis used to warm inventory when
// fill history does not reach the original open (e.g. long-held names
// opened before the lookback window).
type PositionSeed struct {
	Symbol string
	Qty    float64 // signed: +long, −short
	Avg    float64 // absolute average entry
}

// CostBasisHint is a retained average entry for a symbol that may no longer
// appear in REST open positions. Used to cost pure-exit fill books after a
// full close of a previously seeded long-held lot so partials already booked
// do not disappear when the residual position hits flat.
type CostBasisHint struct {
	Symbol string
	Avg    float64 // absolute average entry
}

// inv is per-symbol average-cost inventory (signed qty).
type inv struct {
	qty float64 // +long, −short
	avg float64 // absolute average entry
}

// invQtyTol is the absolute qty tolerance for inventory matching.
const invQtyTol = 1e-6

// RealizedFromFills walks fills oldest-first, maintaining per-symbol average
// cost inventory, and sums realized P&L when size is reduced or flipped.
//
//	day  += realized when fill.At >= dayStart
//	week += realized when fill.At >= weekStart
//
// Fills before dayStart/weekStart still update inventory so cost basis is
// correct for later closes. Open marks are never included.
//
// Prefer RealizedFromFillsWithSeed when open positions are known so
// pre-lookback lots are not mis-modeled as synthetic shorts/longs.
func RealizedFromFills(fills []alpaca.Fill, dayStart, weekStart time.Time) RealizedWindows {
	w, _, _ := RealizedFromFillsWithSeed(fills, dayStart, weekStart, nil)
	return w
}

// RealizedFromFillsWithSeed is RealizedFromFills plus open-position seeds.
// Equivalent to RealizedFromFillsWithHints with no cost-basis hints.
func RealizedFromFillsWithSeed(fills []alpaca.Fill, dayStart, weekStart time.Time, seeds []PositionSeed) (RealizedWindows, bool, []string) {
	return RealizedFromFillsWithHints(fills, dayStart, weekStart, seeds, nil)
}

// RealizedFromFillsWithHints is RealizedFromFills plus open-position seeds and
// optional retained cost-basis hints for symbols that are flat at REST.
//
// Seeds reconcile incomplete history: if the fill walk ends short of the
// REST open qty (same side), a synthetic open is prepended at the seed avg
// so mid-window closes realize against the broker's average entry.
//
// Seed synthesis bridges incomplete history whenever missing = seed − endQty
// shares the seed's sign (a single pre-window open at the seed avg). That covers
// long-held partials where the lookback only has the exit — including when the
// opposite-side fill book is larger than the remaining seed (sold 80 of 100,
// REST still long 20 → endOnly −80, seed +20, synth buy 100). Same-side
// "overshoot" (fill book larger than REST seed) is REST-ahead-of-FILL lag: the
// seed is ignored and the fill book is trusted until the next refresh so a
// normal post-fill race does not blank that symbol's realized totals.
//
// Ghost inventory (ending open fill-book qty with no seed — typically a full
// close of a lot opened before the lookback window) is excluded when no cost
// basis is known. When a CostBasisHint provides an avg for that symbol, a
// synthetic open of size |endQty| is prepended so pure-exit books (including
// partial then residual close of a long-held name) stay continuous after REST
// goes flat. Other symbols still contribute so one bad name does not blank the
// whole account.
//
// The bool is false when nothing trustworthy remains (every involved symbol
// was excluded, or ending inventory still disagrees with kept seeds). Callers
// should keep any prior sample rather than publish a confident wrong number
// (and should not clear a good prior on a transient inconsistency).
//
// excluded lists symbols dropped from the walk (ghost inventory / degenerate
// seed mismatch). When ok is true and excluded is non-empty, totals are a
// partial account sample — callers should surface a soft warning so operators
// know rday/rwk may undercount.
//
// Fees and corporate actions (splits, etc.) are not modeled; numbers can
// drift from broker "realized PL" after those events.
func RealizedFromFillsWithHints(fills []alpaca.Fill, dayStart, weekStart time.Time, seeds []PositionSeed, hints []CostBasisHint) (RealizedWindows, bool, []string) {
	// First pass: inventory from fills alone (needed to size synthetic opens
	// and to classify overshoot / ghost symbols).
	_, endOnly := walkFills(fills, dayStart, weekStart)

	seedBySym := make(map[string]PositionSeed, len(seeds))
	for _, s := range seeds {
		// Avg may be 0 when the broker omits cost basis: still track qty so a
		// complete in-window fill book is not misclassified as ghost inventory.
		if s.Symbol == "" || absFloat(s.Qty) < invQtyTol {
			continue
		}
		seedBySym[s.Symbol] = s
	}

	hintBySym := make(map[string]CostBasisHint, len(hints))
	for _, h := range hints {
		if h.Symbol == "" || h.Avg <= 0 {
			continue
		}
		// Live REST seeds win when both are present.
		if _, hasSeed := seedBySym[h.Symbol]; hasSeed {
			continue
		}
		hintBySym[h.Symbol] = h
	}

	// Symbols whose fill history cannot be reconciled safely.
	unreliable := make(map[string]struct{})
	// Same-side seed lag: REST open is smaller than fill book; trust fills.
	// Also used for qty-only seeds (Avg<=0) when we cannot cost-basis bridge.
	seedLag := make(map[string]struct{})
	// Ghost symbols with retained avg: synth open to flatten ending book.
	ghostFlatten := make(map[string]float64) // signed end qty to flatten

	// Ghost inventory: ending open qty with no seed (e.g. full close of a
	// pre-lookback lot). With a retained avg, synth an open that brings the
	// book flat so pure-exit fills realize continuously. Without avg, exclude.
	for sym, st := range endOnly {
		if st == nil || absFloat(st.qty) < invQtyTol {
			continue
		}
		if _, ok := seedBySym[sym]; ok {
			continue
		}
		if h, ok := hintBySym[sym]; ok && h.Avg > 0 {
			ghostFlatten[sym] = st.qty
			continue
		}
		unreliable[sym] = struct{}{}
	}

	synth := make([]alpaca.Fill, 0, len(seedBySym)+len(ghostFlatten))
	for sym, s := range seedBySym {
		endQty := 0.0
		if st := endOnly[sym]; st != nil {
			endQty = st.qty
		}
		missing := s.Qty - endQty
		if absFloat(missing) < invQtyTol {
			continue // fill book already matches seed
		}
		// Without cost basis we cannot synth a pre-window open. Same-side
		// overshoot (or open-only REST with empty fill book) trusts the fill
		// book / reports zero realized; other mismatches are unreliable.
		if s.Avg <= 0 {
			if absFloat(endQty) >= invQtyTol && endQty*s.Qty > 0 {
				seedLag[sym] = struct{}{}
				continue
			}
			if absFloat(endQty) < invQtyTol {
				// REST open, no in-window fills: nothing to realize for this name.
				seedLag[sym] = struct{}{}
				continue
			}
			unreliable[sym] = struct{}{}
			continue
		}
		// Only bridge toward the open seed: missing must share the seed's sign.
		// That covers long-held partials (sell-only → temporary short vs long
		// seed, including abs(end) > abs(seed) when sold more than remaining)
		// and the symmetric short-held path. A single synth open of size
		// |missing| always ends inventory at seed qty after the real fills.
		if missing*s.Qty <= 0 {
			// Same-side overshoot: fill book is larger than REST seed. Typical
			// REST-ahead-of-FILL race (position updated, matching FILL not yet
			// visible). Prefer the fill book; drop seed for consistency only.
			if absFloat(endQty) >= invQtyTol && endQty*s.Qty > 0 {
				seedLag[sym] = struct{}{}
				continue
			}
			// Seed present, fill book flat/near-zero with missing opposite the
			// seed sign in a degenerate way — mark unreliable.
			unreliable[sym] = struct{}{}
			continue
		}
		side := "buy"
		qty := missing
		if missing < 0 {
			side = "sell"
			qty = -missing
		}
		// Timestamp before any real fill so sort places seeds first.
		// Use Unix(0,1) so at.IsZero() is false but still sorts before live fills.
		// Seed IDs are skipped for day/week bucketing in walkFills (not via
		// timestamp); zero times on real fills are also skipped for bucketing.
		sf := alpaca.Fill{
			ID:        "seed:" + s.Symbol,
			Symbol:    s.Symbol,
			Side:      side,
			Timestamp: time.Unix(0, 1).UTC(),
		}
		sf.SetQty(qty)
		sf.SetPrice(s.Avg)
		synth = append(synth, sf)
	}

	// Retained-avg ghost flatten: pure-exit book ends open with no REST seed
	// (full close of a previously seeded long-held name). Synth opposite open
	// of |endQty| at retained avg so the walk ends flat and realizes exits.
	for sym, endQty := range ghostFlatten {
		h := hintBySym[sym]
		qty := absFloat(endQty)
		if qty < invQtyTol || h.Avg <= 0 {
			continue
		}
		side := "buy"
		if endQty > 0 {
			// Fill book ended long → synth short open (cover path for short-held).
			side = "sell"
		}
		sf := alpaca.Fill{
			ID:        "seed:" + sym,
			Symbol:    sym,
			Side:      side,
			Timestamp: time.Unix(0, 1).UTC(),
		}
		sf.SetQty(qty)
		sf.SetPrice(h.Avg)
		synth = append(synth, sf)
	}

	// Drop unreliable / seed-lag seeds from the consistency set; drop only
	// unreliable symbols' fills from the walk (seed-lag keeps its fills).
	goodSeeds := make(map[string]PositionSeed, len(seedBySym))
	for sym, s := range seedBySym {
		if _, bad := unreliable[sym]; bad {
			continue
		}
		if _, lag := seedLag[sym]; lag {
			continue
		}
		goodSeeds[sym] = s
	}

	filtered := fills
	if len(unreliable) > 0 {
		filtered = make([]alpaca.Fill, 0, len(fills))
		for _, f := range fills {
			if _, bad := unreliable[f.Symbol]; bad {
				continue
			}
			filtered = append(filtered, f)
		}
	}

	combined := filtered
	if len(synth) > 0 {
		// Drop synth for symbols marked unreliable (should not happen — we
		// only synth good seeds — but keep the combined path correct).
		combined = make([]alpaca.Fill, 0, len(synth)+len(filtered))
		combined = append(combined, synth...)
		combined = append(combined, filtered...)
	}
	out, endBook := walkFills(combined, dayStart, weekStart)
	ok := inventoryConsistent(endBook, goodSeeds, seedLag)

	// Had inputs but every involved symbol was excluded → nothing trustworthy.
	if ok && len(unreliable) > 0 && len(goodSeeds) == 0 && len(filtered) == 0 && len(seedLag) == 0 {
		if len(fills) > 0 || len(seedBySym) > 0 {
			ok = false
		}
	}
	return out, ok, sortedKeys(unreliable)
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// walkFills returns realized windows and ending inventory (caller must not
// mutate the map after return if shared — each call allocates a fresh book).
func walkFills(fills []alpaca.Fill, dayStart, weekStart time.Time) (RealizedWindows, map[string]*inv) {
	book := make(map[string]*inv)
	var out RealizedWindows
	if len(fills) == 0 {
		return out, book
	}
	ordered := make([]alpaca.Fill, len(fills))
	copy(ordered, fills)
	sort.SliceStable(ordered, func(i, j int) bool {
		ti, tj := ordered[i].Timestamp, ordered[j].Timestamp
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return ordered[i].ID < ordered[j].ID
	})

	for _, f := range ordered {
		qty := float64(f.Qty)
		px := float64(f.Price)
		if qty <= 0 || px <= 0 || f.Symbol == "" {
			continue
		}
		side, ok := normalizeTradeSide(f.Side)
		if !ok {
			continue
		}
		st := book[f.Symbol]
		if st == nil {
			st = &inv{}
			book[f.Symbol] = st
		}
		realized := applyFill(st, side, qty, px)
		if realized == 0 {
			continue
		}
		// Synthetic seed fills only warm inventory; never bucket as day/week.
		if strings.HasPrefix(f.ID, "seed:") {
			continue
		}
		at := f.Timestamp
		if at.IsZero() {
			continue
		}
		if !at.Before(weekStart) {
			out.Week += realized
		}
		if !at.Before(dayStart) {
			out.Day += realized
		}
	}
	return out, book
}

func inventoryConsistent(endBook map[string]*inv, seeds map[string]PositionSeed, seedLag map[string]struct{}) bool {
	for sym, s := range seeds {
		endQty := 0.0
		if st := endBook[sym]; st != nil {
			endQty = st.qty
		}
		if absFloat(endQty-s.Qty) > invQtyTol {
			return false
		}
	}
	// Ending open inventory with no seed ⇒ incomplete history for a name
	// that is flat at the broker (or an unknown symbol). Do not trust totals.
	// Exception: seedLag symbols intentionally trust the fill book while REST
	// catches up (REST-ahead-of-FILL); their open qty is allowed without a seed.
	for sym, st := range endBook {
		if st == nil || absFloat(st.qty) < invQtyTol {
			continue
		}
		if _, ok := seeds[sym]; ok {
			continue
		}
		if _, lag := seedLag[sym]; lag {
			continue
		}
		return false
	}
	return true
}

// SeedsFromPositions builds inventory seeds from REST open positions.
// Positions with zero/missing avg entry still contribute qty (Avg=0) so a
// complete in-window fill book is not treated as ghost inventory; cost-basis
// bridging is skipped until avg is available.
//
// Alpaca REST may return qty already signed (negative for shorts) or absolute
// with side="short". WS-applied positions store absolute qty. Always emit a
// signed seed (+long, −short).
func SeedsFromPositions(positions []alpaca.Position) []PositionSeed {
	if len(positions) == 0 {
		return nil
	}
	out := make([]PositionSeed, 0, len(positions))
	for _, p := range positions {
		signed := SignedPositionQty(p)
		avg := float64(p.AvgEntryPrice)
		if p.Symbol == "" || absFloat(signed) < invQtyTol {
			continue
		}
		if avg < 0 {
			avg = 0
		}
		out = append(out, PositionSeed{Symbol: p.Symbol, Qty: signed, Avg: avg})
	}
	return out
}

// SignedPositionQty returns position size as +long / −short.
// Handles both absolute qty+side and signed REST qty forms (Alpaca often
// returns already-negative qty for shorts). Used by the local book and by
// REST-first flatten/panic sizing so short exits never double-negate.
func SignedPositionQty(p alpaca.Position) float64 {
	q := float64(p.Qty)
	abs := absFloat(q)
	if abs < invQtyTol {
		return 0
	}
	switch strings.ToLower(p.Side) {
	case "short":
		return -abs
	case "long":
		return abs
	default:
		if q < 0 {
			return -abs
		}
		return abs
	}
}

// normalizeTradeSide maps Alpaca fill/order side strings to buy|sell.
// Activity FILL can emit sell_short for short opens; inventory only needs
// the signed direction.
func normalizeTradeSide(side string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "buy", "buy_minus":
		return "buy", true
	case "sell", "sell_short", "sell_short_exempt", "sell_plus":
		return "sell", true
	default:
		return "", false
	}
}

// RealizedFromOrders converts closed orders with fills into synthetic
// single-lot fills (filled_qty @ filled_avg_price at filled_at) then runs
// RealizedFromFills. Prefer activity FILLs when available; this is the
// order-list path the operator asked for.
func RealizedFromOrders(orders []alpaca.Order, dayStart, weekStart time.Time) RealizedWindows {
	w, _, _ := RealizedFromOrdersWithSeed(orders, dayStart, weekStart, nil)
	return w
}

// RealizedFromOrdersWithSeed is the seeded variant of RealizedFromOrders.
// See RealizedFromFillsWithHints for the meaning of ok and excluded.
func RealizedFromOrdersWithSeed(orders []alpaca.Order, dayStart, weekStart time.Time, seeds []PositionSeed) (RealizedWindows, bool, []string) {
	return RealizedFromOrdersWithHints(orders, dayStart, weekStart, seeds, nil)
}

// RealizedFromOrdersWithHints is RealizedFromOrdersWithSeed plus retained
// cost-basis hints (see RealizedFromFillsWithHints).
func RealizedFromOrdersWithHints(orders []alpaca.Order, dayStart, weekStart time.Time, seeds []PositionSeed, hints []CostBasisHint) (RealizedWindows, bool, []string) {
	fills := make([]alpaca.Fill, 0, len(orders))
	for _, o := range orders {
		fq := float64(o.FilledQty)
		px := float64(o.FilledAvgPrice)
		if fq <= 0 || px <= 0 {
			continue
		}
		side, ok := normalizeTradeSide(o.Side)
		if !ok {
			continue
		}
		at := o.FilledAt
		if at.IsZero() {
			at = o.SubmittedAt
		}
		if at.IsZero() {
			continue
		}
		fills = append(fills, alpaca.Fill{
			ID:        o.ID,
			Symbol:    o.Symbol,
			Side:      side,
			Qty:       o.FilledQty,
			Price:     o.FilledAvgPrice,
			OrderID:   o.ID,
			Timestamp: at,
		})
	}
	return RealizedFromFillsWithHints(fills, dayStart, weekStart, seeds, hints)
}

// applyFill updates inventory for one buy/sell and returns realized P&L
// generated by this fill (0 when it only opens/adds).
func applyFill(st *inv, side string, qty, price float64) float64 {
	signed := qty
	if side == "sell" {
		signed = -qty
	}
	// Flat or adding to the same side: no realization.
	if st.qty == 0 || (st.qty > 0 && signed > 0) || (st.qty < 0 && signed < 0) {
		newQty := st.qty + signed
		if st.qty == 0 {
			st.avg = price
		} else {
			// Weighted average of absolute size.
			st.avg = (st.avg*absFloat(st.qty) + price*qty) / absFloat(newQty)
		}
		st.qty = newQty
		return 0
	}

	// Reducing / closing / flipping through zero.
	closeQty := qty
	if absFloat(st.qty) < closeQty {
		closeQty = absFloat(st.qty)
	}
	var realized float64
	if st.qty > 0 {
		// Sell reduces a long.
		realized = (price - st.avg) * closeQty
	} else {
		// Buy reduces a short.
		realized = (st.avg - price) * closeQty
	}

	if absFloat(st.qty) > closeQty+1e-12 {
		// Still open same side.
		st.qty += signed
		return realized
	}

	// Closed or flipped.
	remainder := qty - closeQty
	if remainder > 1e-12 {
		if side == "buy" {
			st.qty = remainder
		} else {
			st.qty = -remainder
		}
		st.avg = price
	} else {
		st.qty = 0
		st.avg = 0
	}
	return realized
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
