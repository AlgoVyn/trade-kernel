package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"trade-kernel/internal/bars"
)

// TestCanvasLayeringIndicatorWinsCandle is the direct regression test for the
// "pixelated indicators" bug. Before the layering fix, setDot overwrote the
// cell color unconditionally, so a candle body absorbed an indicator dot.
// Now indicators layer above candles: drawing an indicator dot into a cell
// the candle already filled must leave the cell's color set to the indicator.
func TestCanvasLayeringIndicatorWinsCandle(t *testing.T) {
	c := newCanvas(2, 2)
	// Candle body fills cell (0,0) at the candle layer (green).
	c.setDot(0, 0, colUp, layerCandle)
	c.setDot(1, 0, colUp, layerCandle)
	idx := 0 // cell (0,0)
	if c.color[idx] != colUp {
		t.Fatalf("after candle: color = %v, want colUp", c.color[idx])
	}
	// SMA dot lands in the same cell at the indicator layer (blue).
	c.setDot(0, 0, colSMA, layerIndicator)
	if c.color[idx] != colSMA {
		t.Fatalf("after indicator: color = %v, want colSMA (indicator must win)", c.color[idx])
	}
	// Another candle dot after the indicator must NOT clobber it (lower layer).
	c.setDot(1, 0, colUp, layerCandle)
	if c.color[idx] != colSMA {
		t.Fatalf("later candle dot clobbered indicator: color = %v, want colSMA", c.color[idx])
	}
}

// TestCanvasCandleWinsEmptyCell confirms a candle dot colors an untouched
// cell normally (the layering rule only protects higher layers).
func TestCanvasCandleWinsEmptyCell(t *testing.T) {
	c := newCanvas(2, 2)
	c.setDot(0, 0, colUp, layerCandle)
	if c.color[0] != colUp {
		t.Fatalf("candle on empty cell: color = %v, want colUp", c.color[0])
	}
}

// TestRenderSparseData locks in the chosen behavior for sparse bars: 8 bars
// on a 60-wide chart render right-aligned without panic, and the left
// region stays blank.
func TestRenderSparseData(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		agg.OnTrade("AAPL", 150+float64(i), 10, base.Add(time.Duration(i)*time.Minute))
	}
	snap := agg.Snapshot(bars.TF1m, 60, 0)
	lines := renderCandles(snap, 60, 10, ChartOpts{})
	if len(lines) != 10 {
		t.Fatalf("want 10 lines, got %d", len(lines))
	}
	// The left region should be blank (spaces only — no ANSI in tests).
	leftLine := lines[5][:52]
	if strings.TrimSpace(leftLine) != "" {
		t.Fatalf("left region should be blank, got %q", leftLine)
	}
}

// TestTimeRulerLabels verifies the ruler emits time labels for an intraday
// TF and a date-marked label at the first position.
func TestTimeRulerLabels(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	// 60 one-minute bars starting 11:00 ET (15:00 UTC).
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		agg.OnTrade("AAPL", 150, 10, base.Add(time.Duration(i)*time.Minute))
	}
	snap := agg.Snapshot(bars.TF1m, 60, 0)
	line := renderTimeRuler(snap, 60, bars.TF1m)
	plain := stripANSI(line)
	// The first label should carry the date (Jul 17) since it's the first.
	if !strings.Contains(plain, "Jul 17") {
		t.Fatalf("first label should include date, got %q", plain)
	}
	// At least a couple of HH:MM labels should appear.
	if !strings.Contains(plain, ":") {
		t.Fatalf("expected time labels with ':', got %q", plain)
	}
}

// TestTimeRulerDailyFormat confirms daily TF uses the date-only format.
func TestTimeRulerDailyFormat(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	loc, _ := time.LoadLocation("America/New_York")
	// 5 daily bars anchored at 20:00 ET (overnight open) across 5 days.
	start := time.Date(2026, 7, 13, 20, 0, 0, 0, loc)
	for i := 0; i < 5; i++ {
		agg.OnTrade("AAPL", 150, 10, start.AddDate(0, 0, i))
	}
	snap := agg.Snapshot(bars.TF1d, 40, 0)
	line := renderTimeRuler(snap, 40, bars.TF1d)
	plain := stripANSI(line)
	// Daily format is "Jan 02" — no HH:MM should appear.
	if strings.Contains(plain, ":") {
		t.Fatalf("daily ruler should not show time, got %q", plain)
	}
	if !strings.Contains(plain, "Jul") {
		t.Fatalf("daily ruler should show month, got %q", plain)
	}
}

// TestTimeRulerEmpty verifies the ruler degrades gracefully with no bars.
func TestTimeRulerEmpty(t *testing.T) {
	line := renderTimeRuler(bars.Snapshot{}, 40, bars.TF1m)
	if stripANSI(line) != "" {
		t.Fatalf("empty snapshot should render blank ruler, got %q", line)
	}
}

// TestViewRendersWithRuler confirms the full View stack (candles + ruler +
// volume) still renders with the ruler inserted.
func TestViewRendersWithRuler(t *testing.T) {
	d, _, _, agg := testDeps(t)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		agg.OnTrade("AAPL", 150+float64(i%5), 10, base.Add(time.Duration(i)*time.Minute))
	}
	m := NewModel(d)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	out := m.View()
	if out == "" {
		t.Fatal("empty view")
	}
}

// stripANSI removes ANSI escape sequences for plain-text content assertions.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			i++ // skip '['
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return strings.TrimRight(out.String(), " ")
}
