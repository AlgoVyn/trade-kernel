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
// the market is locally Closed (weekend halt, holiday override), it falls
// back to DELETE /v2/positions/{symbol}, which liquidates regardless of
// order-form rules — the emergency exit must always work.
func (e *RESTExecutor) Flatten(ctx context.Context, symbol string, positionQty float64) (alpaca.Order, error) {
	if positionQty == 0 {
		return alpaca.Order{}, fmt.Errorf("no position in %s", symbol)
	}
	qty := int(math.Abs(positionQty))
	if qty == 0 {
		return alpaca.Order{}, fmt.Errorf("no position in %s", symbol)
	}
	// Closed locally ⇒ the builder would reject ("market is closed"). Use
	// the position-close endpoint, which works outside any session.
	if e.sessionNow() == session.Closed {
		if err := e.rest.ClosePosition(ctx, symbol); err != nil {
			return alpaca.Order{}, err
		}
		return alpaca.Order{Symbol: symbol, Status: "close_requested"}, nil
	}
	side := "sell"
	if positionQty < 0 {
		side = "buy"
	}
	return e.submit(ctx, BuildInput{
		Symbol: symbol, Side: side, Qty: qty,
		Session: e.sessionNow(), RegularTIF: e.regularTIF,
	})
}

func (e *RESTExecutor) CancelAll(ctx context.Context) error {
	return e.rest.CancelAll(ctx)
}
