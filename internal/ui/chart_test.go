package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"trade-kernel/internal/bars"
	"trade-kernel/internal/session"
)

// TestSessionBGDistinct ensures each non-regular session has its own tint
// so overnight / pre-market / after-hours don't share a single color.
func TestSessionBGDistinct(t *testing.T) {
	on := sessionBG(session.Overnight)
	pre := sessionBG(session.PreMarket)
	ah := sessionBG(session.AfterHours)
	reg := sessionBG(session.Regular)
	if on == bgNone || pre == bgNone || ah == bgNone {
		t.Fatal("non-regular sessions must have a background tint")
	}
	if on == pre || on == ah || pre == ah {
		t.Fatalf("session tints must be distinct: overnight=%d pre=%d after=%d", on, pre, ah)
	}
	if reg != bgNone {
		t.Fatalf("regular session should be unshaded, got %d", reg)
	}
	// Palette entries must exist for every tint we emit.
	for _, bg := range []uint8{on, pre, ah} {
		if _, ok := bgColors[bg]; !ok {
			t.Fatalf("bgColors missing entry for %d", bg)
		}
	}
}

// TestGridCandleSolidBlock confirms candles paint continuous block
// characters (█ / │), not braille.
func TestGridCandleSolidBlock(t *testing.T) {
	g := newGrid(3, 5)
	for y := 0; y <= 4; y++ {
		g.setCandle(1, y, '│', colUp)
	}
	for y := 1; y <= 3; y++ {
		g.setCandle(1, y, '█', colUp)
	}
	lines := g.render()
	plain := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(plain, "█") {
		t.Fatalf("expected solid body █, got %q", plain)
	}
	if !strings.Contains(plain, "│") {
		t.Fatalf("expected wick │, got %q", plain)
	}
	if containsBraille(plain) {
		t.Fatalf("candles alone must not use braille, got %q", plain)
	}
}

// TestGridIndicatorBrailleOverlay confirms indicators use braille and
// win the glyph over a solid candle body in the same cell.
func TestGridIndicatorBrailleOverlay(t *testing.T) {
	g := newGrid(4, 2)
	// Solid candle body in row 0 across all columns.
	for x := 0; x < 4; x++ {
		g.setCandle(x, 0, '█', colUp)
	}
	// Continuous braille indicator across the same area.
	g.drawIndLine(0, 1, 7, 1, colEMA)
	for x := 0; x < 4; x++ {
		i := g.idx(x, 0)
		if g.indDots[i] == 0 {
			t.Fatalf("col %d: expected indicator braille dots", x)
		}
		if g.indColor[i] != colEMA {
			t.Fatalf("col %d: color = %v, want colEMA", x, g.indColor[i])
		}
		// Candle body still stored underneath.
		if g.ch[i] != '█' {
			t.Fatalf("col %d: candle body should remain █, got %q", x, string(g.ch[i]))
		}
	}
	// Render must show braille (indicator wins), not █.
	lines := g.render()
	plain := stripANSI(lines[0])
	if !containsBraille(plain) {
		t.Fatalf("expected braille indicator glyphs in render, got %q", plain)
	}
	if strings.Contains(plain, "█") {
		t.Fatalf("indicator should cover candle body in shared cells, got %q", plain)
	}
}

// TestDrawIndLineConnects verifies Bresenham fills a continuous horizontal
// braille indicator across cells.
func TestDrawIndLineConnects(t *testing.T) {
	g := newGrid(6, 2)
	g.drawIndLine(0, 2, 11, 2, colEMA)
	for x := 0; x < 6; x++ {
		i := g.idx(x, 0) // y-dot 2 → cell row 0
		if g.indDots[i] == 0 {
			t.Fatalf("col %d: expected indicator dots along continuous line", x)
		}
		if g.indColor[i] != colEMA {
			t.Fatalf("col %d: color = %v, want colEMA", x, g.indColor[i])
		}
	}
}

// TestRenderCandlesHybridShape locks in solid candles + braille indicators.
func TestRenderCandlesHybridShape(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		px := 150 + float64(i%7) - 3
		agg.OnTrade("AAPL", px, 10, base.Add(time.Duration(i)*time.Minute))
		agg.OnTrade("AAPL", px+0.5, 5, base.Add(time.Duration(i)*time.Minute+30*time.Second))
	}
	snap := agg.Snapshot(bars.TF1m, 40, 0)

	// Candles only: solid blocks, no braille.
	noInd := renderCandles(snap, 40, 12, ChartOpts{})
	joined := stripANSI(strings.Join(noInd, ""))
	if containsBraille(joined) {
		t.Fatal("candles-only render must not emit braille")
	}
	if !strings.ContainsAny(joined, "█│") {
		t.Fatalf("expected █ or │ candle glyphs, got %q", truncate(joined, 80))
	}

	// With indicators: braille should appear for the overlays.
	withInd := renderCandles(snap, 40, 12, ChartOpts{ShowEMA: true, ShowEMA2: true, ShowVWAP: true})
	joinedInd := stripANSI(strings.Join(withInd, ""))
	if !containsBraille(joinedInd) {
		t.Fatal("indicators should render as braille glyphs")
	}
}

// TestRenderSparseData locks in right-aligned sparse bars with a blank left region.
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
	// 8 bars at barStride=2: oldest at col 60-1-7*2 = 45. Left of that is blank.
	leftLine := stripANSI(lines[5])
	if len(leftLine) > 45 {
		leftLine = leftLine[:45]
	}
	if strings.TrimSpace(leftLine) != "" {
		t.Fatalf("left region should be blank, got %q", leftLine)
	}
}

// TestBarSpacing puts space between adjacent candles so they don't merge.
func TestBarSpacing(t *testing.T) {
	if barStride < 2 {
		t.Fatalf("barStride=%d, want >= 2 for visual separation", barStride)
	}
	agg := bars.NewAggregator(3, 3)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	// Enough range so every bar paints a body in the middle rows.
	for i := 0; i < 10; i++ {
		agg.OnTrade("AAPL", 100, 10, base.Add(time.Duration(i)*time.Minute))
		agg.OnTrade("AAPL", 110, 10, base.Add(time.Duration(i)*time.Minute+10*time.Second))
		agg.OnTrade("AAPL", 105, 10, base.Add(time.Duration(i)*time.Minute+20*time.Second))
	}
	snap := agg.Snapshot(bars.TF1m, maxBars(40), 0)
	lines := renderCandles(snap, 40, 10, ChartOpts{})
	// Mid-row should alternate solid bar / spacer for consecutive bars.
	plain := stripANSI(lines[5])
	// Find two consecutive non-space glyphs; they must not be adjacent cells.
	var prev int = -1
	for i, r := range plain {
		if r == ' ' {
			continue
		}
		// Skip ANSI-stripped price-less plain; only count bar glyphs.
		if r != '█' && r != '│' && (r < 0x2800 || r > 0x28FF) {
			continue
		}
		if prev >= 0 && i-prev < barStride {
			t.Fatalf("bars too close: glyphs at cols %d and %d (stride=%d), line=%q", prev, i, barStride, plain)
		}
		if prev >= 0 {
			return // one pair is enough
		}
		prev = i
	}
	if prev < 0 {
		t.Fatalf("expected candle glyphs in mid row, got %q", plain)
	}
}

func TestCandleColor(t *testing.T) {
	if candleColor(bars.Bar{Open: 10, Close: 11}) != colUp {
		t.Fatal("close > open should be up")
	}
	if candleColor(bars.Bar{Open: 11, Close: 10}) != colDown {
		t.Fatal("close < open should be down")
	}
	if candleColor(bars.Bar{Open: 10, Close: 10}) != colUp {
		t.Fatal("doji should be up")
	}
}

// TestTimeRulerLabels verifies the ruler emits time labels for an intraday TF.
func TestTimeRulerLabels(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		agg.OnTrade("AAPL", 150, 10, base.Add(time.Duration(i)*time.Minute))
	}
	snap := agg.Snapshot(bars.TF1m, 60, 0)
	line := renderTimeRuler(snap, 60, bars.TF1m)
	plain := stripANSI(line)
	if !strings.Contains(plain, "Jul 17") {
		t.Fatalf("first label should include date, got %q", plain)
	}
	if !strings.Contains(plain, ":") {
		t.Fatalf("expected time labels with ':', got %q", plain)
	}
}

// TestTimeRulerLiveEdgeRightAlign ensures the newest label (…ET) is fully
// visible rather than clipped to one character at column w-1.
func TestTimeRulerLiveEdgeRightAlign(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	// Fixed wall clock far from "now" so the last bar is closed history;
	// label text still carries the ET suffix on the live edge.
	base := time.Date(2026, 7, 17, 19, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		agg.OnTrade("AAPL", 150, 10, base.Add(time.Duration(i)*time.Minute))
	}
	snap := agg.Snapshot(bars.TF1m, 20, 0)
	line := renderTimeRuler(snap, 40, bars.TF1m)
	plain := stripANSI(line)
	if !strings.Contains(plain, "ET") {
		t.Fatalf("live-edge ET tag should be visible, got %q", plain)
	}
	// Right-aligned live label should appear near the end of the line.
	idx := strings.LastIndex(plain, "ET")
	if idx < 0 || idx < len(plain)/2 {
		t.Fatalf("ET tag should be toward the right edge, got %q", plain)
	}
}

// TestTimeRulerDailyFormat confirms daily TF uses the date-only format.
func TestTimeRulerDailyFormat(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	loc, _ := time.LoadLocation("America/New_York")
	start := time.Date(2026, 7, 13, 20, 0, 0, 0, loc)
	for i := 0; i < 5; i++ {
		agg.OnTrade("AAPL", 150, 10, start.AddDate(0, 0, i))
	}
	snap := agg.Snapshot(bars.TF1d, 40, 0)
	line := renderTimeRuler(snap, 40, bars.TF1d)
	plain := stripANSI(line)
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
		t.Fatalf("empty snapshot should render blank ruler, got %q", stripANSI(line))
	}
}

// TestPriceAxisLabels verifies the right-side price scale emits numeric labels.
func TestPriceAxisLabels(t *testing.T) {
	min, max := 100.0, 110.0
	lines := renderPriceAxis(min, max, priceAxisWidth, 12)
	if len(lines) != 12 {
		t.Fatalf("want 12 lines, got %d", len(lines))
	}
	top := stripANSI(lines[0])
	bot := stripANSI(lines[len(lines)-1])
	if !strings.ContainsAny(top, "0123456789") {
		t.Fatalf("top label missing digits: %q", top)
	}
	if !strings.ContainsAny(bot, "0123456789") {
		t.Fatalf("bottom label missing digits: %q", bot)
	}
}

// TestPriceAxisEmptyRange degrades without panic.
func TestPriceAxisEmptyRange(t *testing.T) {
	lines := renderPriceAxis(0, 0, priceAxisWidth, 5)
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got %d", len(lines))
	}
}

// TestPriceRangeShared ensures candles and the price axis share padded min/max.
func TestPriceRangeShared(t *testing.T) {
	agg := bars.NewAggregator(3, 3)
	base := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		agg.OnTrade("AAPL", 150+float64(i), 10, base.Add(time.Duration(i)*time.Minute))
	}
	snap := agg.Snapshot(bars.TF1m, 40, 0)
	opts := ChartOpts{ShowEMA: true, ShowEMA2: true, ShowVWAP: true}
	min, max, ok := priceRange(snap, opts)
	if !ok {
		t.Fatal("expected ok range")
	}
	if min >= max {
		t.Fatalf("min=%v max=%v", min, max)
	}
	var lo, hi float64 = snap.Bars[0].Low, snap.Bars[0].High
	for _, b := range snap.Bars {
		if b.Low < lo {
			lo = b.Low
		}
		if b.High > hi {
			hi = b.High
		}
	}
	if min >= lo {
		t.Fatalf("expected 2%% pad below lows: min=%v lo=%v", min, lo)
	}
	if max <= hi {
		t.Fatalf("expected 2%% pad above highs: max=%v hi=%v", max, hi)
	}
}

// TestViewRendersWithRuler confirms the full View stack still renders.
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
	if !strings.Contains(out, "│") {
		t.Fatal("expected price axis / wick separator │ in view")
	}
}

// containsBraille reports whether s includes any braille-pattern runes (U+2800..U+28FF).
func containsBraille(s string) bool {
	for _, r := range s {
		if r >= 0x2800 && r <= 0x28FF {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// stripANSI removes ANSI escape sequences for plain-text content assertions.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			i++
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		out.WriteByte(s[i])
	}
	return strings.TrimRight(out.String(), " ")
}

func TestVolumeScaleClipsAuctionOutlier(t *testing.T) {
	// Typical AH minutes ~5k, one RTH-close print at 1.3M.
	vols := make([]float64, 60)
	for i := range vols {
		vols[i] = 5000
	}
	vols[0] = 1_300_000
	vols[len(vols)-1] = 16_000
	scale := volumeScale(vols)
	if scale >= 1_300_000 {
		t.Fatalf("scale should ignore auction outlier, got %v", scale)
	}
	if scale < 5000 || scale > 50_000 {
		t.Fatalf("scale=%v want near typical volume", scale)
	}
	// Uniform volumes: scale is max.
	u := []float64{100, 200, 150, 180, 120, 90, 110, 130, 140, 160}
	if got := volumeScale(u); got != 200 {
		t.Fatalf("uniform scale=%v want 200", got)
	}
	// Short window with one outlier: p95 must not collapse to max.
	short := []float64{100, 100, 100, 100, 100, 100, 100, 10_000}
	scaleS := volumeScale(short)
	if scaleS >= 10_000 {
		t.Fatalf("short-window scale should clip outlier, got %v", scaleS)
	}
	if scaleS < 100 {
		t.Fatalf("short-window scale=%v want near typical 100", scaleS)
	}
}
