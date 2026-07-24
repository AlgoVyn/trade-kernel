package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

// Executor is the order-execution boundary. v1 = REST; a FIX engine
// (quickfixgo) can implement the same interface later.
//
// Session is always supplied by the caller (captured at intent/confirm time)
// so a session boundary during y/n confirmation cannot change order class
// (e.g. extended limit → regular market) without a re-prompt.
type Executor interface {
	// Buy/Sell submit a hotkey-style order: market in regular hours,
	// aggressive limit in extended sessions.
	Buy(ctx context.Context, symbol string, qty int, sess session.Session) (alpaca.Order, error)
	Sell(ctx context.Context, symbol string, qty int, sess session.Session) (alpaca.Order, error)
	// LimitBuy/LimitSell submit explicit-limit orders (from ':'
	// commands); in extended sessions they carry extended_hours=true.
	LimitBuy(ctx context.Context, symbol string, qty int, price float64, sess session.Session) (alpaca.Order, error)
	LimitSell(ctx context.Context, symbol string, qty int, price float64, sess session.Session) (alpaca.Order, error)
	// Flatten closes the entire position in symbol using the
	// session-appropriate order form. positionQty is signed. sess is the
	// pinned session from intent time.
	Flatten(ctx context.Context, symbol string, positionQty float64, sess session.Session) (alpaca.Order, error)
	// CancelAll cancels every open order.
	CancelAll(ctx context.Context) error
	// CancelSymbol cancels open orders for one symbol only. Returns the
	// number of per-order DELETEs that failed and the first error; every
	// cancel is still attempted so a single failure does not leave siblings
	// open. failures>0 tells the caller (panic path) some may remain resting.
	CancelSymbol(ctx context.Context, symbol string) (failures int, err error)
}

// RESTExecutor implements Executor against the Alpaca REST API.
type RESTExecutor struct {
	rest       *alpaca.REST
	builder    *Builder
	sessionNow func() session.Session // fallback only when tests omit sess
	regularTIF string                 // "day" | "ioc"
}

// NewRESTExecutor wires an executor. sessionNow is retained for tests that
// call helper paths without an explicit session; production UI always pins
// session at intent time.
func NewRESTExecutor(rest *alpaca.REST, builder *Builder, sessionNow func() session.Session, regularTIF string) *RESTExecutor {
	if regularTIF != "ioc" {
		regularTIF = "day"
	}
	return &RESTExecutor{rest: rest, builder: builder, sessionNow: sessionNow, regularTIF: regularTIF}
}

// SetRegularTIF switches the regular-hours time-in-force ("day"/"ioc").
func (e *RESTExecutor) SetRegularTIF(tif string) {
	if tif == "ioc" {
		e.regularTIF = "ioc"
	} else {
		e.regularTIF = "day"
	}
}

func clientOrderID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "tk-" + hex.EncodeToString(b[:]) + "-" + time.Now().UTC().Format("150405.000")
}

func (e *RESTExecutor) submit(ctx context.Context, in BuildInput) (alpaca.Order, error) {
	req, _, err := e.builder.Build(ctx, in)
	if err != nil {
		return alpaca.Order{}, err
	}
	req.ClientOrderID = clientOrderID()
	return e.rest.PlaceOrder(ctx, req)
}

func (e *RESTExecutor) Buy(ctx context.Context, symbol string, qty int, sess session.Session) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "buy", Qty: qty,
		Session: sess, RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) Sell(ctx context.Context, symbol string, qty int, sess session.Session) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "sell", Qty: qty,
		Session: sess, RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) LimitBuy(ctx context.Context, symbol string, qty int, price float64, sess session.Session) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "buy", Qty: qty, LimitPrice: price,
		Session: sess, RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) LimitSell(ctx context.Context, symbol string, qty int, price float64, sess session.Session) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "sell", Qty: qty, LimitPrice: price,
		Session: sess, RegularTIF: e.regularTIF,
	})
}

// Flatten submits the opposite-side order for the full position size using
// the session-appropriate order form: market in Regular, aggressive limit
// with extended_hours=true in PreMarket/AfterHours/Overnight — for both
// long (sell) and short (buy) exits. Market orders are not allowed outside
// regular hours, so extended-session flattens never fall back to
// DELETE /v2/positions/{symbol} (which liquidates at market); builder or
// pricing errors are returned to the operator instead.
//
// ClosePosition is used when the order path cannot work: the market is
// locally Closed (weekend halt, holiday override), or the quantity is
// fractional during Regular (fractionals cannot be truncated without a
// residual). Fractional qty in PreMarket/AfterHours/Overnight is rejected
// with an error — ClosePosition would also submit a market liquidation,
// which is not allowed outside regular hours.
//
// AllowStaleLastTrade is set so quiet extended/overnight tape still prices
// exits off a last trade within a few minutes rather than hard-failing.
func (e *RESTExecutor) Flatten(ctx context.Context, symbol string, positionQty float64, sess session.Session) (alpaca.Order, error) {
	if positionQty == 0 {
		return alpaca.Order{}, fmt.Errorf("no position in %s", symbol)
	}
	abs := math.Abs(positionQty)
	qty := int(abs)
	// Fractional / sub-share: truncating would leave a residual open.
	if qty == 0 || abs != float64(qty) {
		switch sess {
		case session.Regular, session.Closed:
			// Regular: broker accepts market liquidate. Closed: endpoint
			// queues for the next open (same as whole-share Closed path).
			return e.closePosition(ctx, symbol)
		default:
			return alpaca.Order{}, fmt.Errorf(
				"cannot flatten fractional position in %s outside regular hours (qty=%g); wait for RTH or close the residual in the broker UI",
				symbol, positionQty)
		}
	}
	// Closed locally ⇒ the builder would reject ("market is closed"). Use
	// the position-close endpoint, which works outside any session.
	if sess == session.Closed {
		return e.closePosition(ctx, symbol)
	}
	side := "sell"
	if positionQty < 0 {
		side = "buy"
	}
	// No ClosePosition fallback here: in extended sessions that endpoint
	// submits a market order, which is not allowed outside regular hours.
	// Broker transport/reject errors are surfaced as-is too — the order
	// may already be live, and the operator needs the original error.
	//
	// Always day TIF for flatten/panic exits: orders.regular_tif (incl. ioc)
	// applies to hotkey/':' buy-sell only. An IOC market exit can partial-fill
	// and leave residual risk — the opposite of flatten.
	o, err := e.submit(ctx, BuildInput{
		Symbol: symbol, Side: side, Qty: qty,
		Session: sess, RegularTIF: "day",
		AllowStaleLastTrade: true,
	})
	if err != nil && session.Extended(sess) {
		return alpaca.Order{}, fmt.Errorf(
			"flatten %s in %s: %w (wait for RTH or use broker UI if pricing/eligibility failed)",
			symbol, sess, err)
	}
	return o, err
}

func (e *RESTExecutor) closePosition(ctx context.Context, symbol string) (alpaca.Order, error) {
	if err := e.rest.ClosePosition(ctx, symbol); err != nil {
		return alpaca.Order{}, err
	}
	return alpaca.Order{Symbol: symbol, Status: "close_requested"}, nil
}

func (e *RESTExecutor) CancelAll(ctx context.Context) error {
	return e.rest.CancelAll(ctx)
}

func (e *RESTExecutor) CancelSymbol(ctx context.Context, symbol string) (int, error) {
	return e.rest.CancelSymbol(ctx, symbol)
}
