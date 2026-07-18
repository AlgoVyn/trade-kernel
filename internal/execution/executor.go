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

// ResultHook receives the order request and warning before submission
// (UI confirmation / status line) and the result after.
type ResultHook interface {
	// BeforeSubmit is called synchronously before submission. Returning
	// false aborts the order.
	BeforeSubmit(req alpaca.OrderRequest, warning string) bool
	AfterSubmit(req alpaca.OrderRequest, order alpaca.Order, err error, latency time.Duration)
}

// RESTExecutor implements Executor against the Alpaca REST API.
type RESTExecutor struct {
	rest       *alpaca.REST
	builder    *Builder
	sessionNow func() session.Session
	regularTIF string // "day" | "ioc"
	hook       ResultHook
}

// NewRESTExecutor wires an executor. sessionNow reports the current
// session (session.Engine.Current).
func NewRESTExecutor(rest *alpaca.REST, builder *Builder, sessionNow func() session.Session, regularTIF string) *RESTExecutor {
	if regularTIF != "ioc" {
		regularTIF = "day"
	}
	return &RESTExecutor{rest: rest, builder: builder, sessionNow: sessionNow, regularTIF: regularTIF}
}

// SetHook installs the submission hook (confirmation, latency tracking).
func (e *RESTExecutor) SetHook(h ResultHook) { e.hook = h }

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
	req, warn, err := e.builder.Build(ctx, in)
	if err != nil {
		return alpaca.Order{}, err
	}
	req.ClientOrderID = clientOrderID()
	if e.hook != nil && !e.hook.BeforeSubmit(req, warn) {
		return alpaca.Order{}, fmt.Errorf("aborted by user")
	}
	start := time.Now()
	order, err := e.rest.PlaceOrder(ctx, req)
	if e.hook != nil {
		e.hook.AfterSubmit(req, order, err, time.Since(start))
	}
	return order, err
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

// Flatten submits the opposite-side order for the full position size.
func (e *RESTExecutor) Flatten(ctx context.Context, symbol string, positionQty float64) (alpaca.Order, error) {
	if positionQty == 0 {
		return alpaca.Order{}, fmt.Errorf("no position in %s", symbol)
	}
	qty := int(math.Abs(positionQty))
	if qty == 0 {
		return alpaca.Order{}, fmt.Errorf("no position in %s", symbol)
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
