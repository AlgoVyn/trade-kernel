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
	t = t.In(et)
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
// market closed during a window we'd classify as Regular (holiday or
// early close), the engine forces Closed until nextOpen. Sync is advisory
// only; it never overrides a locally-derived Closed into a trading
// session.
func (e *Engine) SyncClock(isOpen bool, nextOpen time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if isOpen {
		e.forcedClosedUntil = time.Time{}
		return
	}
	now := e.now()
	local := At(now)
	if local == Regular || local == PreMarket || local == AfterHours {
		if nextOpen.After(now) {
			e.forcedClosedUntil = nextOpen
		}
	}
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
