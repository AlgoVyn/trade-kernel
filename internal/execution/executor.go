package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"trade-kernel/internal/alpaca"
	"trade-kernel/internal/session"
)

// Executor is the order-execution boundary. v1 = REST; a FIX engine
// (quickfixgo) can implement the same interface later.
type Executor interface {
	// Buy/Sell submit a hotkey-style order: market in regular hours,
	// aggressive limit in extended sessions.
	Buy(ctx context.Context, symbol string, qty int) (alpaca.Order, error)
	Sell(ctx context.Context, symbol string, qty int) (alpaca.Order, error)
	// LimitBuy/LimitSell submit explicit-limit orders (from ':'
	// commands); in extended sessions they carry extended_hours=true.
	LimitBuy(ctx context.Context, symbol string, qty int, price float64) (alpaca.Order, error)
	LimitSell(ctx context.Context, symbol string, qty int, price float64) (alpaca.Order, error)
	// Flatten closes the entire position in symbol using the
	// session-appropriate order form. positionQty is signed.
	Flatten(ctx context.Context, symbol string, positionQty float64) (alpaca.Order, error)
	// CancelAll cancels every open order.
	CancelAll(ctx context.Context) error
	// CancelSymbol cancels open orders for one symbol only.
	CancelSymbol(ctx context.Context, symbol string) error
}

// RESTExecutor implements Executor against the Alpaca REST API.
type RESTExecutor struct {
	rest       *alpaca.REST
	builder    *Builder
	sessionNow func() session.Session
	regularTIF string // "day" | "ioc"
}

// NewRESTExecutor wires an executor. sessionNow reports the current
// session (session.Engine.Current).
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

func (e *RESTExecutor) Buy(ctx context.Context, symbol string, qty int) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "buy", Qty: qty,
		Session: e.sessionNow(), RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) Sell(ctx context.Context, symbol string, qty int) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "sell", Qty: qty,
		Session: e.sessionNow(), RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) LimitBuy(ctx context.Context, symbol string, qty int, price float64) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "buy", Qty: qty, LimitPrice: price,
		Session: e.sessionNow(), RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) LimitSell(ctx context.Context, symbol string, qty int, price float64) (alpaca.Order, error) {
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: "sell", Qty: qty, LimitPrice: price,
		Session: e.sessionNow(), RegularTIF: e.regularTIF,
	})
}

// Flatten submits the opposite-side order for the full position size. If
// the market is locally Closed (weekend halt, holiday override), or the
// session-aware builder cannot price an extended-hours exit (no NBBO /
// last trade), it falls back to DELETE /v2/positions/{symbol} — the
// emergency exit must always work.
//
// Non-integral quantities (fractional shares or sub-share residuals) use
// ClosePosition directly so a single flatten fully exits; whole-share
// positions use the session-aware order path.
func (e *RESTExecutor) Flatten(ctx context.Context, symbol string, positionQty float64) (alpaca.Order, error) {
	if positionQty == 0 {
		return alpaca.Order{}, fmt.Errorf("no position in %s", symbol)
	}
	abs := math.Abs(positionQty)
	qty := int(abs)
	// Fractional / sub-share: liquidate fully via the positions endpoint.
	// Truncating to whole shares would leave a residual open.
	if qty == 0 || abs != float64(qty) {
		return e.closePosition(ctx, symbol)
	}
	// Capture session once so a boundary between checks cannot pick
	// ClosePosition for one branch and the order path for the other.
	sess := e.sessionNow()
	// Closed locally ⇒ the builder would reject ("market is closed"). Use
	// the position-close endpoint, which works outside any session.
	if sess == session.Closed {
		return e.closePosition(ctx, symbol)
	}
	side := "sell"
	if positionQty < 0 {
		side = "buy"
	}
	o, err := e.submit(ctx, BuildInput{
		Symbol: symbol, Side: side, Qty: qty,
		Session: sess, RegularTIF: e.regularTIF,
	})
	if err != nil {
		// Only fall back for known builder/pricing failures. Do not call
		// ClosePosition after a PlaceOrder transport/reject error — the
		// order may already be live, and operators need the original error.
		if isFlattenPricingFallback(err) {
			o2, err2 := e.closePosition(ctx, symbol)
			if err2 == nil {
				// Keep original failure visible on the synthetic order so
				// the UI/status line is not a silent close_requested alone.
				o2.Status = "close_requested (" + err.Error() + ")"
				return o2, nil
			}
			return alpaca.Order{}, fmt.Errorf("%w (close position: %v)", err, err2)
		}
		return alpaca.Order{}, err
	}
	return o, nil
}

// isFlattenPricingFallback reports whether err is a session/pricing failure
// from the order builder (safe to liquidate via DELETE positions) rather
// than a broker submit failure after the order may have been accepted.
func isFlattenPricingFallback(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrMarketClosed) ||
		errors.Is(err, ErrNoExtendedPrice) ||
		errors.Is(err, ErrNotOvernightTradable) ||
		errors.Is(err, ErrOvernightEligibility)
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

func (e *RESTExecutor) CancelSymbol(ctx context.Context, symbol string) error {
	return e.rest.CancelSymbol(ctx, symbol)
}
