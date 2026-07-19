package session

import (
	"testing"
	"time"
)

// et builds a time in America/New_York.
func etTime(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, Location())
}

func TestAtBoundaries(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want Session
	}{
		// Regular session boundaries (Wed).
		{"regular open", etTime(2026, 7, 15, 9, 30), Regular},
		{"regular mid", etTime(2026, 7, 15, 12, 0), Regular},
		{"regular last minute", etTime(2026, 7, 15, 15, 59), Regular},
		{"afterhours open", etTime(2026, 7, 15, 16, 0), AfterHours},
		{"afterhours last minute", etTime(2026, 7, 15, 19, 59), AfterHours},
		{"overnight open", etTime(2026, 7, 15, 20, 0), Overnight},
		{"overnight late", etTime(2026, 7, 15, 23, 59), Overnight},
		{"overnight past midnight", etTime(2026, 7, 16, 0, 0), Overnight},
		{"overnight last minute", etTime(2026, 7, 16, 3, 59), Overnight},
		{"premarket open", etTime(2026, 7, 16, 4, 0), PreMarket},
		{"premarket last minute", etTime(2026, 7, 16, 9, 29), PreMarket},

		// Weekend / weekly open-close.
		{"friday 19:59 after-hours", etTime(2026, 7, 17, 19, 59), AfterHours},
		{"friday 20:00 closed", etTime(2026, 7, 17, 20, 0), Closed},
		{"saturday closed", etTime(2026, 7, 18, 12, 0), Closed},
		{"saturday 00:30 closed", etTime(2026, 7, 18, 0, 30), Closed},
		{"sunday afternoon closed", etTime(2026, 7, 19, 15, 0), Closed},
		{"sunday 20:00 weekly open", etTime(2026, 7, 19, 20, 0), Overnight},
		{"monday 02:00 overnight", etTime(2026, 7, 20, 2, 0), Overnight},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := At(tc.at); got != tc.want {
				t.Fatalf("At(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestAtUTCInput(t *testing.T) {
	// 13:30 UTC = 09:30 ET (EDT, July).
	at := time.Date(2026, 7, 15, 13, 30, 0, 0, time.UTC)
	if got := At(at); got != Regular {
		t.Fatalf("At(%v) = %v, want Regular", at, got)
	}
}

func TestDSTTransitions(t *testing.T) {
	// Spring forward 2026-03-08: EST→EDT. 14:30 UTC = 09:30 EST before,
	// 13:30 UTC = 09:30 EDT after.
	before := time.Date(2026, 3, 6, 14, 30, 0, 0, time.UTC) // Fri, EST
	after := time.Date(2026, 3, 9, 13, 30, 0, 0, time.UTC)  // Mon, EDT
	if got := At(before); got != Regular {
		t.Fatalf("pre-DST: got %v, want Regular", got)
	}
	if got := At(after); got != Regular {
		t.Fatalf("post-DST: got %v, want Regular", got)
	}

	// Fall back 2026-11-01: EDT→EST.
	beforeFall := time.Date(2026, 10, 30, 13, 30, 0, 0, time.UTC) // Fri, EDT
	afterFall := time.Date(2026, 11, 2, 14, 30, 0, 0, time.UTC)   // Mon, EST
	if got := At(beforeFall); got != Regular {
		t.Fatalf("pre-fallback: got %v, want Regular", got)
	}
	if got := At(afterFall); got != Regular {
		t.Fatalf("post-fallback: got %v, want Regular", got)
	}
}

func TestSyncClockHoliday(t *testing.T) {
	// Wednesday 10:00 ET would locally be Regular; Alpaca says closed
	// (holiday) until Thursday 09:30.
	now := etTime(2026, 7, 15, 10, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	if got := e.Current(); got != Closed {
		t.Fatalf("Current() = %v, want Closed (holiday override)", got)
	}

	// After nextOpen the override lapses.
	later := etTime(2026, 7, 16, 10, 0)
	e.now = func() time.Time { return later }
	if got := e.Current(); got != Regular {
		t.Fatalf("Current() after nextOpen = %v, want Regular", got)
	}
}

// TestSyncClockEarlyCloseAfternoon covers starting mid-RTH after an early
// close (local still Regular, is_open=false, next_open = next session).
func TestSyncClockEarlyCloseAfternoon(t *testing.T) {
	// Wednesday 14:00 ET — still in Regular wall-clock window.
	now := etTime(2026, 7, 15, 14, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	if got := e.Current(); got != Closed {
		t.Fatalf("Current() = %v, want Closed (early close during RTH window)", got)
	}
}

func TestSyncClockHolidayOvernight(t *testing.T) {
	// Tuesday 22:00 ET is Overnight locally; next_open Thursday 09:30 skips
	// Wednesday RTH ⇒ full holiday close must force Closed.
	now := etTime(2026, 7, 14, 22, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	if got := e.Current(); got != Closed {
		t.Fatalf("Current() = %v, want Closed (overnight holiday override)", got)
	}
}

func TestSyncClockNormalOvernightNotForced(t *testing.T) {
	// Tuesday 22:00 ET Overnight with next_open = Wednesday 09:30 is a normal
	// night (Alpaca is_open is false every overnight). Must stay Overnight.
	now := etTime(2026, 7, 14, 22, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 15, 9, 30))
	if got := e.Current(); got != Overnight {
		t.Fatalf("Current() = %v, want Overnight (normal night, not holiday)", got)
	}
}

func TestSyncClockNormalEarlyMorningOvernight(t *testing.T) {
	// Wednesday 02:00 ET: next RTH is same calendar day 09:30.
	now := etTime(2026, 7, 15, 2, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 15, 9, 30))
	if got := e.Current(); got != Overnight {
		t.Fatalf("Current() = %v, want Overnight", got)
	}
}

func TestNextExpectedRTHOpen(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"tue night → wed", etTime(2026, 7, 14, 22, 0), etTime(2026, 7, 15, 9, 30)},
		{"wed early → wed", etTime(2026, 7, 15, 2, 0), etTime(2026, 7, 15, 9, 30)},
		{"wed afternoon → thu", etTime(2026, 7, 15, 15, 0), etTime(2026, 7, 16, 9, 30)},
		{"fri afternoon → mon", etTime(2026, 7, 17, 15, 0), etTime(2026, 7, 20, 9, 30)},
		{"sun night → mon", etTime(2026, 7, 19, 22, 0), etTime(2026, 7, 20, 9, 30)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextExpectedRTHOpen(tc.at); !got.Equal(tc.want) {
				t.Fatalf("nextExpectedRTHOpen(%v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func TestSyncClockNeverOverridesLocalClosed(t *testing.T) {
	// Saturday: locally Closed; Alpaca is_open=false with nextOpen Monday.
	now := etTime(2026, 7, 18, 12, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 20, 9, 30))
	if got := e.Current(); got != Closed {
		t.Fatalf("Current() = %v, want Closed", got)
	}
}

func TestSyncClockOpenClearsOverride(t *testing.T) {
	now := etTime(2026, 7, 15, 10, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	e.SyncClock(true, time.Time{})
	if got := e.Current(); got != Regular {
		t.Fatalf("Current() = %v, want Regular after open sync", got)
	}
}

func TestSyncClockNormalAfterHoursNotForced(t *testing.T) {
	// Wednesday 16:30 ET AfterHours; next_open = Thursday 09:30 is normal
	// (Alpaca is_open=false all AH). Must stay AfterHours.
	now := etTime(2026, 7, 15, 16, 30)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	if got := e.Current(); got != AfterHours {
		t.Fatalf("Current() = %v, want AfterHours (normal AH, not holiday)", got)
	}
}

func TestSyncClockNormalPreMarketNotForced(t *testing.T) {
	// Thursday 08:00 ET PreMarket; next_open = today 09:30 is normal.
	now := etTime(2026, 7, 16, 8, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	if got := e.Current(); got != PreMarket {
		t.Fatalf("Current() = %v, want PreMarket (normal pre, not holiday)", got)
	}
}

func TestSyncClockHolidayPreMarketForced(t *testing.T) {
	// Wednesday 08:00 ET would be PreMarket locally, but next_open is
	// Thursday 09:30 (skips today's RTH) ⇒ holiday force Closed.
	now := etTime(2026, 7, 15, 8, 0)
	e := NewEngine(func() time.Time { return now })
	e.SyncClock(false, etTime(2026, 7, 16, 9, 30))
	if got := e.Current(); got != Closed {
		t.Fatalf("Current() = %v, want Closed (pre-market holiday override)", got)
	}
}
