// Package session classifies wall-clock time into US equity trading
// sessions in America/New_York and emits session-transition events.
//
// Sessions (ET):
//
//	Overnight   20:00–04:00 (Sun 20:00 open through Fri 04:00 close)
//	PreMarket   04:00–09:30  (Mon–Fri)
//	Regular     09:30–16:00  (Mon–Fri)
//	AfterHours  16:00–20:00  (Mon–Fri)
//	Closed      weekends and Fri 20:00 → Sun 20:00, plus holidays
//	            (holidays only via SyncClock override)
//
// All classification happens in the ET timezone via the tz database, so
// DST transitions are handled implicitly — never by hardcoded UTC offsets.
package session

import (
	"context"
	"sync"
	"time"
	// Embed the tzdata database so America/New_York is always available,
	// even on scratch/distroless images without zoneinfo installed. This
	// backs up the init() panic-safety claim in this file.
	_ "time/tzdata"
)

// Session identifies one of the 24/5 trading sessions.
type Session int

const (
	Closed Session = iota
	Overnight
	PreMarket
	Regular
	AfterHours
)

func (s Session) String() string {
	switch s {
	case Overnight:
		return "OVERNIGHT"
	case PreMarket:
		return "PRE-MARKET"
	case Regular:
		return "REGULAR"
	case AfterHours:
		return "AFTER-HOURS"
	default:
		return "CLOSED"
	}
}

// Extended reports whether s is a non-regular trading session in which
// orders must be limit orders with extended_hours=true.
func Extended(s Session) bool {
	return s == Overnight || s == PreMarket || s == AfterHours
}

// Tradable reports whether orders may be submitted in session s.
func Tradable(s Session) bool { return s != Closed }

var et *time.Location

func init() {
	var err error
	et, err = time.LoadLocation("America/New_York")
	if err != nil {
		// Embedded tzdata fallback should make this unreachable; fail loudly.
		panic("session: load America/New_York: " + err.Error())
	}
}

// Location returns the America/New_York timezone.
func Location() *time.Location { return et }

// At classifies t into a session using wall-clock time in ET.
func At(t time.Time) Session {
	// Fast path: session boundaries align on whole minutes in ET, so every
	// timestamp within the same (ET weekday, minute) shares a result. Memoize
	// per-minute to avoid re-running weekday arithmetic for each visible bar
	// on every render frame (the chart renderer calls At once per bar).
	t = t.In(et)
	key := uint64(t.Unix())/60 + uint64(t.Weekday())*1e6
	if v, ok := atCache.Load(key); ok {
		return v.(Session)
	}
	s := classifyAt(t)
	atCache.Store(key, s)
	return s
}

var atCache sync.Map

// classifyAt is the pure wall-clock classification (no cache).
func classifyAt(t time.Time) Session {
	wd := t.Weekday()
	mins := t.Hour()*60 + t.Minute()

	switch wd {
	case time.Saturday:
		return Closed
	case time.Sunday:
		if mins >= 20*60 {
			return Overnight
		}
		return Closed
	case time.Friday:
		if mins >= 20*60 {
			return Closed // weekend halt
		}
	}
	switch {
	case mins >= 9*60+30 && mins < 16*60:
		return Regular
	case mins >= 4*60 && mins < 9*60+30:
		return PreMarket
	case mins >= 16*60 && mins < 20*60:
		return AfterHours
	default:
		// 20:00–24:00 (Mon–Thu, Sun handled above) and 00:00–04:00.
		return Overnight
	}
}

// Event describes a session transition.
type Event struct {
	From, To Session
	At       time.Time
}

// Engine tracks the current session, applies Alpaca /v2/clock overrides
// (holidays), and emits Events on transitions.
type Engine struct {
	now func() time.Time

	mu sync.Mutex
	// forcedClosedUntil, when in the future, forces Closed (holiday or
	// early close per Alpaca clock).
	forcedClosedUntil time.Time

	ch chan Event
}

// NewEngine creates an Engine. now is injectable for tests; pass nil for
// time.Now.
func NewEngine(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{now: now, ch: make(chan Event, 8)}
}

// Current returns the session for the engine's clock right now.
func (e *Engine) Current() Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.classify(e.now())
}

func (e *Engine) classify(t time.Time) Session {
	if !e.forcedClosedUntil.IsZero() && t.Before(e.forcedClosedUntil) {
		return Closed
	}
	return At(t)
}

// Events returns the channel of session-transition events.
func (e *Engine) Events() <-chan Event { return e.ch }

// SyncClock applies an Alpaca /v2/clock reading. When Alpaca reports the
// market closed during a window we'd classify as tradable, the engine may
// force Closed until nextOpen. Alpaca's is_open is the *regular* session
// flag — false every pre-market, after-hours, and overnight on a normal day —
// so it must not force Closed for PreMarket/AfterHours/Overnight unless
// nextOpen skips past the next expected RTH open (multi-day gap ⇒ holiday).
// During local Regular, is_open=false means a holiday or early close, so we
// force Closed until nextOpen (covers process start mid-afternoon on an
// early-close day while wall clock is still in the 09:30–16:00 window).
//
// Gap: if the process starts only after local Regular has ended (AH/overnight)
// on an early-close day, next_open looks like a normal next-session open and
// we intentionally stay in AfterHours/Overnight — extended-hours books are
// often still open after an early RTH close; the broker rejects if not.
// Sync is advisory only; it never overrides a locally-derived Closed into a
// trading session.
func (e *Engine) SyncClock(isOpen bool, nextOpen time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if isOpen {
		e.forcedClosedUntil = time.Time{}
		return
	}
	now := e.now()
	local := At(now)
	if !Tradable(local) || !nextOpen.After(now) {
		return
	}
	switch local {
	case Regular:
		// RTH window but Alpaca says closed → holiday / early close.
		// Sticky until nextOpen so a later tick still after early close
		// but still in the local Regular wall-clock window stays Closed.
		e.forcedClosedUntil = nextOpen
	default:
		// PreMarket / AfterHours / Overnight: is_open is false every normal
		// day. Only force when next_open skips the next expected RTH open
		// (full-session holiday gap).
		expected := nextExpectedRTHOpen(now)
		const slack = 2 * time.Hour
		if nextOpen.After(expected.Add(slack)) {
			e.forcedClosedUntil = nextOpen
		}
	}
}

// nextExpectedRTHOpen returns the next 09:30 ET Monday–Friday open strictly
// after t (or at t if t is exactly 09:30 on a weekday — callers use After).
// Weekend candidates roll forward to Monday.
func nextExpectedRTHOpen(t time.Time) time.Time {
	t = t.In(et)
	candidate := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, et)
	if !t.Before(candidate) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	for candidate.Weekday() == time.Saturday || candidate.Weekday() == time.Sunday {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// Run polls once per second and emits an Event whenever the session
// changes. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	prev := e.Current()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			cur := e.Current()
			if cur != prev {
				ev := Event{From: prev, To: cur, At: e.now()}
				select {
				case e.ch <- ev:
				default: // never block the engine on a slow consumer
				}
				prev = cur
			}
		}
	}
}
