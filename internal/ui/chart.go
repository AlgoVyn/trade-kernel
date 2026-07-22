package ui

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"trade-kernel/internal/bars"
	"trade-kernel/internal/session"
)

// Braille dot bit positions: [dx][dy], dx∈{0,1}, dy∈{0..3}.
// Used only for indicator overlays (EMA/EMA2/VWAP) so lines stay smooth at
// 4× vertical resolution. Candles use solid block characters instead.
var dotBit = [2][4]uint8{{0x01, 0x02, 0x04, 0x40}, {0x08, 0x10, 0x20, 0x80}}

// Foreground palette indices.
const (
	colNone uint8 = iota
	colUp
	colDown
	colEMA
	colEMA2
	colVWAP
)

// Background palette indices — one distinct tint per trading session so
// overnight / pre-market / after-hours are easy to tell apart when shading
// is on. Regular session keeps the default (no tint).
const (
	bgNone uint8 = iota
	bgOvernight  // 20:00–04:00 ET
	bgPreMarket  // 04:00–09:30 ET
	bgAfterHours // 16:00–20:00 ET
)

// Candle / volume palette matches TradingView's classic scheme (teal up,
// coral red down) so side-by-side screenshots read the same. Truecolor
// hex; lipgloss maps them to the nearest 256-color cell when needed.
var fgColors = map[uint8]lipgloss.Color{
	colUp:   lipgloss.Color("#26a69a"), // TV up / bullish
	colDown: lipgloss.Color("#ef5350"), // TV down / bearish
	colEMA:  lipgloss.Color("#42a5f5"), // fast EMA — soft blue
	colEMA2: lipgloss.Color("#ab47bc"), // slow EMA — soft purple
	colVWAP: lipgloss.Color("#ffa726"), // soft amber
}

// chartOpen/chartClose are pre-baked ANSI open/close sequences for every
// (fg, bg) the chart can emit. Built once at package init by calling
// lipgloss.Render a single time per entry (hex→terminal-profile conversion
// happens at startup). The 10–30 Hz path only concatenates these strings —
// never Style.Render, which re-parses hex colors via Sscanf every call.
// Index as chartOpen[bg*8+fg].
var (
	chartOpen  [numBg * 8]string
	chartClose [numBg * 8]string
)

const numBg = 4 // bgNone + 3 session tints (matches bg* iota count)

// bakeMarker is a private one-byte rune used only while splitting a styled
// lipgloss.Render result into open-prefix + close-suffix at init.
const bakeMarker = "\x01"

func init() {
	var bgs = [numBg]uint8{bgNone, bgOvernight, bgPreMarket, bgAfterHours}
	for bi, bg := range bgs {
		for fi := uint8(0); fi < 8; fi++ {
			st := lipgloss.NewStyle()
			if fc, ok := fgColors[fi]; ok {
				st = st.Foreground(fc)
			}
			if bi > 0 {
				if bc, ok := bgColors[bg]; ok {
					st = st.Background(bc)
				}
			}
			open, close := bakeANSI(st)
			chartOpen[int(bg)*8+int(fi)] = open
			chartClose[int(bg)*8+int(fi)] = close
		}
	}
}

// bakeANSI splits st.Render(marker) into the ANSI prefix/suffix so the hot
// path can wrap arbitrary run text without calling Render again.
func bakeANSI(st lipgloss.Style) (open, close string) {
	s := st.Render(bakeMarker)
	i := strings.Index(s, bakeMarker)
	if i < 0 {
		return "", ""
	}
	return s[:i], s[i+len(bakeMarker):]
}

// styleIndex maps (fg, bg) palette indices into the pre-baked tables.
func styleIndex(fg, bg uint8) int {
	i := int(bg)*8 + int(fg)
	if i < 0 || i >= len(chartOpen) {
		return 0
	}
	return i
}

// writeStyled appends text wrapped in the pre-baked (fg, bg) ANSI sequences.
// No lipgloss work; empty open means plain text (colNone + bgNone).
func writeStyled(sb *strings.Builder, fg, bg uint8, text string) {
	if text == "" {
		return
	}
	i := styleIndex(fg, bg)
	if chartOpen[i] == "" {
		sb.WriteString(text)
		return
	}
	sb.WriteString(chartOpen[i])
	sb.WriteString(text)
	sb.WriteString(chartClose[i])
}

// stDim is the shared faint style for axis labels / separators / the time
// ruler. Hoisted so the once-per-frame axis/ruler renderers don't allocate.
var stDim = lipgloss.NewStyle().Faint(true)

// stAxisSep is the "│" separator rendered at the left of each price-axis row.
var stAxisSep = stDim.Render("│")

// Session background colors — light, desaturated tints so they mark the
// session without competing with candle teal/red. Hex keeps them soft on
// truecolor terms; they fall back cleanly on 256-color terminals via lipgloss.
var bgColors = map[uint8]lipgloss.Color{
	// Keep tints very subtle so candle teal/red stays dominant (TV-like).
	bgOvernight:  lipgloss.Color("#22222c"), // faint indigo
	bgPreMarket:  lipgloss.Color("#22262a"), // faint steel
	bgAfterHours: lipgloss.Color("#242220"), // faint warm charcoal
}

// grid is a hybrid canvas:
//   - Candles → solid block runes (█ body, │ wick), continuous like volume
//   - Indicators → braille dots (2×4 per cell) for smooth high-res lines
// At render time indicator braille wins any shared cell so overlays read as
// thin continuous lines through candle bodies.
type grid struct {
	w, h int
	// Candle / background layer (one solid rune per cell).
	ch []rune
	fg []uint8
	bg []uint8
	// Indicator braille overlay (independent of candle glyphs).
	indDots  []uint8
	indColor []uint8
}

// gridPool reuses grid canvases across frames to avoid allocating five
// w×h slices on every render tick.
var gridPool = sync.Pool{New: func() any { return &grid{} }}

func acquireGrid(w, h int) *grid {
	g := gridPool.Get().(*grid)
	g.reset(w, h)
	return g
}

func releaseGrid(g *grid) {
	if g == nil {
		return
	}
	gridPool.Put(g)
}

func newGrid(w, h int) *grid {
	g := &grid{}
	g.reset(w, h)
	return g
}

// reset resizes (if needed) and clears the canvas for a fresh paint.
func (g *grid) reset(w, h int) {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	n := w * h
	g.w, g.h = w, h
	if cap(g.ch) < n {
		g.ch = make([]rune, n)
		g.fg = make([]uint8, n)
		g.bg = make([]uint8, n)
		g.indDots = make([]uint8, n)
		g.indColor = make([]uint8, n)
	} else {
		g.ch = g.ch[:n]
		g.fg = g.fg[:n]
		g.bg = g.bg[:n]
		g.indDots = g.indDots[:n]
		g.indColor = g.indColor[:n]
		clear(g.fg)
		clear(g.bg)
		clear(g.indDots)
		clear(g.indColor)
	}
	for i := range g.ch {
		g.ch[i] = ' '
	}
}

func (g *grid) idx(x, y int) int { return y*g.w + x }

// setCandle paints a solid block/line rune for a candle cell.
func (g *grid) setCandle(x, y int, ch rune, fg uint8) {
	if x < 0 || x >= g.w || y < 0 || y >= g.h {
		return
	}
	i := g.idx(x, y)
	g.ch[i] = ch
	g.fg[i] = fg
}

// setMarker paints a solid overlay glyph that wins over candles and
// indicator braille (used for working buy/sell order markers).
func (g *grid) setMarker(x, y int, ch rune, fg uint8) {
	if x < 0 || x >= g.w || y < 0 || y >= g.h {
		return
	}
	i := g.idx(x, y)
	g.ch[i] = ch
	g.fg[i] = fg
	g.indDots[i] = 0
	g.indColor[i] = 0
}

func (g *grid) setColBg(cx int, bg uint8) {
	if cx < 0 || cx >= g.w {
		return
	}
	for y := 0; y < g.h; y++ {
		g.bg[g.idx(cx, y)] = bg
	}
}

// setIndDot lights one braille dot at dot-coordinates (x: 0..2w-1, y: 0..4h-1).
func (g *grid) setIndDot(x, y int, col uint8) {
	if x < 0 || x >= g.w*2 || y < 0 || y >= g.h*4 {
		return
	}
	i := (y/4)*g.w + (x / 2)
	g.indDots[i] |= dotBit[x%2][y%4]
	g.indColor[i] = col
}

// drawIndLine connects (x0,y0)→(x1,y1) in braille-dot space with Bresenham
// so EMA/EMA2/VWAP read as smooth continuous lines.
func (g *grid) drawIndLine(x0, y0, x1, y1 int, col uint8) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx := stepSign(x0, x1)
	sy := stepSign(y0, y1)
	err := dx - dy
	x, y := x0, y0
	for {
		g.setIndDot(x, y, col)
		if x == x1 && y == y1 {
			return
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x += sx
		}
		if e2 < dx {
			err += dx
			y += sy
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func stepSign(a, b int) int {
	if b > a {
		return 1
	}
	if b < a {
		return -1
	}
	return 0
}

// render emits styled lines. Indicator braille takes priority over candle
// blocks so overlay lines stay thin and smooth; solid █/│ candles show
// everywhere else. Styling uses pre-baked ANSI sequences (writeStyled) —
// no lipgloss.Render on this path.
func (g *grid) render() []string {
	lines := make([]string, g.h)
	var sb strings.Builder
	var run strings.Builder
	// Rough capacity: ~3 bytes/rune for braille + ANSI open/close per run.
	sb.Grow(g.w * 24)
	run.Grow(g.w * 3)
	for y := 0; y < g.h; y++ {
		sb.Reset()
		run.Reset()
		var runFG, runBG uint8
		started := false
		flush := func() {
			if run.Len() == 0 {
				return
			}
			writeStyled(&sb, runFG, runBG, run.String())
			run.Reset()
		}
		for x := 0; x < g.w; x++ {
			i := g.idx(x, y)
			bg := g.bg[i]
			var fg uint8
			var ch rune
			if g.indDots[i] != 0 {
				// Smooth braille indicator overlay.
				ch = rune(0x2800) + rune(g.indDots[i])
				fg = g.indColor[i]
			} else if g.ch[i] != ' ' {
				// Solid candle body/wick.
				ch = g.ch[i]
				fg = g.fg[i]
			} else {
				ch = ' '
				fg = colNone
			}
			if started && (fg != runFG || bg != runBG) {
				flush()
			}
			if !started || run.Len() == 0 {
				runFG, runBG = fg, bg
				started = true
			}
			run.WriteRune(ch)
		}
		flush()
		lines[y] = sb.String()
	}
	return lines
}

// ChartOrder is a working order plotted on the candle pane at Price.
// Side is "buy" or "sell"; Price is typically the limit price.
type ChartOrder struct {
	Side  string
	Price float64
}

// ChartOpts controls rendering.
type ChartOpts struct {
	ShowEMA, ShowEMA2, ShowVWAP bool
	SessionShading              bool
	// Orders are open working orders for the active symbol. Each is drawn
	// as a dashed price level with a buy (▲) or sell (▼) glyph at the
	// live edge. Market orders without a limit price are omitted upstream.
	Orders []ChartOrder
}

// sessionBG maps a trading session to a chart background tint.
// Regular and Closed return bgNone (no tint).
func sessionBG(s session.Session) uint8 {
	switch s {
	case session.Overnight:
		return bgOvernight
	case session.PreMarket:
		return bgPreMarket
	case session.AfterHours:
		return bgAfterHours
	default:
		return bgNone
	}
}

// barStride is the horizontal pitch of one OHLC/volume bar in terminal
// cells: one solid body column plus one empty spacer so adjacent candles
// (and volume bars) are easy to tell apart.
const barStride = 2

// maxBars returns how many bars fit in a plot of width w cells with
// barStride spacing, right-aligned so the live edge sits on the last column.
func maxBars(w int) int {
	if w <= 0 {
		return 0
	}
	// Last bar at column w-1; previous bars every barStride cells leftward.
	return (w-1)/barStride + 1
}

// barCol maps bar index i (0 = oldest of n) to its plot column in a w-wide
// canvas. Bars are right-aligned with barStride spacing.
func barCol(w, n, i int) int {
	return w - 1 - (n-1-i)*barStride
}

// validOrderPrice reports whether p is a plottable working-order price.
// Shared by priceRange, paintOrderMarkers, and chartOrdersFor so invalid
// values never expand the axis without also drawing a marker (and vice versa).
func validOrderPrice(p float64) bool {
	return p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0)
}

// validScalePrice is the Y-axis counterpart: only positive finite prices may
// expand the candle scale. Zero / NaN indicator seeds and bad OHLC must not
// pin the axis to the origin (that is the usual "candles flattened to a line"
// failure mode when a live print of 0 lands in the forming bar).
func validScalePrice(p float64) bool {
	return validOrderPrice(p)
}

// orderScaleExpand is how far beyond the bar/indicator span a resting limit
// may pull the Y-axis, as a multiple of that span. Orders outside the window
// still paint (clamped to the top/bottom edge) but do not squash candle detail.
const orderScaleExpand = 1.0

// wickOutlierMult: a high (or low) is treated as a single wild wick when it
// sits more than this many typical bar-ranges beyond the next-most extreme
// high (or low). Only that isolated extreme is soft-clipped; a real step
// move that leaves several bars at the new level is kept.
const wickOutlierMult = 3.0

// priceRange returns the (min, max) price span across snap's bars and the
// enabled overlays, with 2% padding on each side — exactly the range the
// candle pane maps to its y-axis. Shared by renderCandles and the price
// axis so labels line up with the bars. Returns (0,0,false) for empty.
//
// Order of operations matters for indicator overlays (VWAP/EMA):
//  1. Span valid bar high/low only.
//  2. Soft-clip a single isolated wild wick (not a midprice percentile core —
//     that wrongly clipped legitimate recent tape and left VWAP above max,
//     which clamps to a flat line on the top edge).
//  3. Grow for enabled indicators so overlays always sit inside the scale.
//  4. Nearby working orders expand within orderScaleExpand× the bar span.
//
// Non-positive / non-finite OHLC and indicator values never expand the axis.
func priceRange(snap bars.Snapshot, opts ChartOpts) (min, max float64, ok bool) {
	if len(snap.Bars) == 0 {
		return 0, 0, false
	}
	min, max = math.Inf(1), math.Inf(-1)
	grow := func(v float64) {
		if !validScalePrice(v) {
			return
		}
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}

	lows := make([]float64, 0, len(snap.Bars))
	highs := make([]float64, 0, len(snap.Bars))
	ranges := make([]float64, 0, len(snap.Bars))
	bodyTops := make([]float64, 0, len(snap.Bars))
	bodyBots := make([]float64, 0, len(snap.Bars))
	for _, b := range snap.Bars {
		if validScalePrice(b.Low) {
			lows = append(lows, b.Low)
			grow(b.Low)
		}
		if validScalePrice(b.High) {
			highs = append(highs, b.High)
			grow(b.High)
		}
		if validScalePrice(b.Low) && validScalePrice(b.High) && b.High >= b.Low {
			ranges = append(ranges, b.High-b.Low)
		}
		// Body top/bottom (max/min of open, close) distinguish a pure wick
		// outlier from a real one-bar drive that closes near the extreme.
		if validScalePrice(b.Open) || validScalePrice(b.Close) {
			top, bot := b.Open, b.Close
			if !validScalePrice(top) {
				top = bot
			}
			if !validScalePrice(bot) {
				bot = top
			}
			if top < bot {
				top, bot = bot, top
			}
			if validScalePrice(top) {
				bodyTops = append(bodyTops, top)
			}
			if validScalePrice(bot) {
				bodyBots = append(bodyBots, bot)
			}
		}
	}
	if math.IsInf(min, 1) || math.IsInf(max, -1) {
		return 0, 0, false
	}
	if min >= max {
		max = min + 1
	}

	// Soft-clip only pure wick outliers — before indicators, so a clipped max
	// cannot leave VWAP/EMA above the scale (flat line glued to the top edge).
	// A one-bar news spike that closes near the extreme is kept.
	if lo, hi, clipped := softClipWickExtremes(lows, highs, ranges, bodyTops, bodyBots); clipped {
		min, max = lo, hi
	}

	// Indicators always expand the scale: session VWAP after a reset, and
	// EMAs, are real prices the user expects to see on-pane — never clamp them.
	for i := range snap.Bars {
		if opts.ShowEMA && i < len(snap.EMA) {
			grow(snap.EMA[i])
		}
		if opts.ShowEMA2 && i < len(snap.EMA2) {
			grow(snap.EMA2[i])
		}
		if opts.ShowVWAP && i < len(snap.VWAP) {
			grow(snap.VWAP[i])
		}
	}
	if min >= max {
		max = min + 1
	}

	// Cap order-driven expansion to a band around the bar/indicator range.
	barMin, barMax := min, max
	span := barMax - barMin
	loBound := barMin - span*orderScaleExpand
	hiBound := barMax + span*orderScaleExpand
	for _, o := range opts.Orders {
		if !validOrderPrice(o.Price) {
			continue
		}
		if o.Price < loBound || o.Price > hiBound {
			continue
		}
		grow(o.Price)
	}
	if min >= max {
		max = min + 1
	}
	pad := (max - min) * 0.02
	return min - pad, max + pad, true
}

// softClipWickExtremes drops a single isolated high and/or low when it sits
// more than wickOutlierMult× the typical bar range beyond the next extreme
// *and* the extreme is a pure wick (body top/bottom also far from the
// extreme). A genuine step (several bars at the new level) or a one-bar
// body-driven spike (close near the high/low) keeps the new extreme.
//
// bodyTops / bodyBots are max(open,close) / min(open,close) per bar; they
// may be unsorted and need not align 1:1 with lows/highs after filtering.
func softClipWickExtremes(lows, highs, barRanges, bodyTops, bodyBots []float64) (lo, hi float64, clipped bool) {
	if len(lows) == 0 || len(highs) == 0 {
		return 0, 0, false
	}
	slices.Sort(lows)
	slices.Sort(highs)
	lo, hi = lows[0], highs[len(highs)-1]
	if !(hi > lo) {
		return lo, hi, false
	}
	if len(lows) < 8 || len(highs) < 8 {
		return lo, hi, false
	}

	typical := medianFloats(barRanges)
	// Isolated high: max is far above the second-highest high, and no body
	// closes near that high (pure upper wick / bad print).
	if n := len(highs); n >= 2 {
		second := highs[n-2]
		thresh := wickThresh(typical, second)
		if hi-second > thresh && bodyFarFromExtreme(bodyTops, hi, thresh) {
			hi = second
			clipped = true
		}
	}
	// Isolated low: min is far below the second-lowest low, and no body
	// prints near that low (pure lower wick / bad print).
	if n := len(lows); n >= 2 {
		second := lows[1]
		thresh := wickThresh(typical, second)
		if second-lo > thresh && bodyFarFromExtremeLow(bodyBots, lo, thresh) {
			lo = second
			clipped = true
		}
	}
	if !(hi > lo) {
		return lows[0], highs[len(highs)-1], false
	}
	return lo, hi, clipped
}

// bodyFarFromExtreme reports whether every body top sits more than thresh
// below extreme (hi). Empty bodyTops means we cannot prove a body move —
// treat as wick-only (allow clip).
func bodyFarFromExtreme(bodyTops []float64, extreme, thresh float64) bool {
	if len(bodyTops) == 0 {
		return true
	}
	for _, t := range bodyTops {
		if extreme-t <= thresh {
			return false
		}
	}
	return true
}

// bodyFarFromExtremeLow reports whether every body bottom sits more than
// thresh above extreme (lo).
func bodyFarFromExtremeLow(bodyBots []float64, extreme, thresh float64) bool {
	if len(bodyBots) == 0 {
		return true
	}
	for _, b := range bodyBots {
		if b-extreme <= thresh {
			return false
		}
	}
	return true
}

// wickThresh is how far beyond the next extreme a high/low may sit before
// we call it a single wild wick. Prefer a multiple of typical bar range;
// when bars are flat (H==L dojis) fall back to 5% of the reference price
// so a real 1-point step on a $150 name is never clipped.
func wickThresh(typicalBarRange, refPrice float64) float64 {
	if typicalBarRange > 0 {
		return typicalBarRange * wickOutlierMult
	}
	if refPrice > 0 {
		return refPrice * 0.05
	}
	return 0
}

func medianFloats(a []float64) float64 {
	if len(a) == 0 {
		return 0
	}
	s := append([]float64(nil), a...)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 0 {
		return (s[mid-1] + s[mid]) / 2
	}
	return s[mid]
}

// renderCandles draws continuous block-character candlesticks (█ bodies,
// │ wicks — same solid look as volume) with braille indicator overlays
// (EMA/EMA2/VWAP) for smooth high-resolution lines.
// Bars are drawn left-to-right, oldest first, right-aligned, with barStride
// spacing so neighboring candles are visually separated.
func renderCandles(snap bars.Snapshot, w, h int, opts ChartOpts) []string {
	if w <= 0 || h <= 0 || len(snap.Bars) == 0 {
		return blankLines(h)
	}
	if mb := maxBars(w); len(snap.Bars) > mb {
		off := len(snap.Bars) - mb
		snap.Bars = snap.Bars[off:]
		snap.EMA = snap.EMA[off:]
		snap.EMA2 = snap.EMA2[off:]
		snap.VWAP = snap.VWAP[off:]
	}
	n := len(snap.Bars)

	min, max, ok := priceRange(snap, opts)
	if !ok {
		min, max = 0, 1
	}

	g := acquireGrid(w, h)
	defer releaseGrid(g)

	// Cell-row mapping for solid candles (row 0 = max, row h-1 = min).
	cellSpan := float64(h - 1)
	if cellSpan < 1 {
		cellSpan = 1
	}
	yOfCell := func(p float64) int {
		if h == 1 {
			return 0
		}
		y := int(math.Round((max - p) / (max - min) * cellSpan))
		if y < 0 {
			y = 0
		}
		if y > h-1 {
			y = h - 1
		}
		return y
	}

	// Braille-dot mapping for indicators (4× vertical resolution).
	dotSpan := float64(h*4 - 1)
	if dotSpan < 1 {
		dotSpan = 1
	}
	yOfDot := func(p float64) int {
		y := int(math.Round((max - p) / (max - min) * dotSpan))
		if y < 0 {
			y = 0
		}
		if y > h*4-1 {
			y = h*4 - 1
		}
		return y
	}

	// Session shading — only the bar column; spacer columns stay clear.
	// Each non-regular session gets its own background tint.
	for i, b := range snap.Bars {
		cx := barCol(w, n, i)
		if opts.SessionShading {
			if bg := sessionBG(session.At(b.Start)); bg != bgNone {
				g.setColBg(cx, bg)
			}
		}
	}

	// Candles: solid █ body + │ wick, colored like TradingView
	// (close < open → red, else teal). Wick first so the body overwrites.
	for i, b := range snap.Bars {
		cx := barCol(w, n, i)
		col := candleColor(b)
		yHi, yLo := yOfCell(b.High), yOfCell(b.Low)
		yO, yC := yOfCell(b.Open), yOfCell(b.Close)
		top, bot := yO, yC
		if top > bot {
			top, bot = bot, top
		}
		for y := yHi; y <= yLo; y++ {
			g.setCandle(cx, y, '│', col)
		}
		// Minimum 1-row body so dojis stay visible.
		if top == bot {
			g.setCandle(cx, top, '█', col)
		} else {
			for y := top; y <= bot; y++ {
				g.setCandle(cx, y, '█', col)
			}
		}
	}

	// Indicators: braille lines connecting consecutive samples (smooth).
	// Lines span the spacer columns so overlays stay continuous across the
	// bar pitch.
	plotSeries := func(vals []float64, col uint8) {
		prevX, prevY := -1, -1
		for i := range snap.Bars {
			if math.IsNaN(vals[i]) {
				prevX, prevY = -1, -1
				continue
			}
			cx := barCol(w, n, i)
			leftX := cx * 2
			rightX := cx*2 + 1
			y := yOfDot(vals[i])
			if prevX >= 0 {
				// Connect previous right edge → current left edge.
				g.drawIndLine(prevX, prevY, leftX, y, col)
			}
			// Solid horizontal segment across the bar's cell.
			g.setIndDot(leftX, y, col)
			g.setIndDot(rightX, y, col)
			prevX, prevY = rightX, y
		}
	}
	if opts.ShowEMA {
		plotSeries(snap.EMA, colEMA)
	}
	if opts.ShowEMA2 {
		plotSeries(snap.EMA2, colEMA2)
	}
	if opts.ShowVWAP {
		plotSeries(snap.VWAP, colVWAP)
	}

	// Working orders: dashed level + buy/sell glyph at the live edge.
	// Drawn last so markers win over candles and indicator braille.
	paintOrderMarkers(g, opts.Orders, yOfCell, w)

	return g.render()
}

// order marker glyphs — classic chart convention (up = buy, down = sell).
const (
	orderBuyMark  = '▲'
	orderSellMark = '▼'
	orderBothMark = '◆'
	orderLevelCh  = '┄' // light triple-dash horizontal guide
)

// paintOrderMarkers draws each working order as a dashed horizontal guide
// at its price and a buy/sell symbol on the rightmost column. Multiple
// orders that map to the same row are collapsed; mixed buy+sell uses ◆.
// Prices outside the current scale still paint (yOfCell clamps them to the
// top/bottom edge) so far limits remain visible without rescaling.
func paintOrderMarkers(g *grid, orders []ChartOrder, yOfCell func(float64) int, w int) {
	if g == nil || len(orders) == 0 || w <= 0 {
		return
	}
	type sides struct{ buy, sell bool }
	byY := make(map[int]sides, len(orders))
	for _, o := range orders {
		if !validOrderPrice(o.Price) {
			continue
		}
		y := yOfCell(o.Price)
		if y < 0 || y >= g.h {
			continue
		}
		s := byY[y]
		if strings.EqualFold(o.Side, "sell") {
			s.sell = true
		} else {
			// treat anything non-sell as buy (Alpaca uses "buy"/"sell")
			s.buy = true
		}
		byY[y] = s
	}
	for y, s := range byY {
		if y < 0 || y >= g.h {
			continue
		}
		col := colUp
		mark := orderBuyMark
		switch {
		case s.buy && s.sell:
			col = colVWAP
			mark = orderBothMark
		case s.sell:
			col = colDown
			mark = orderSellMark
		}
		// Dashed guide on empty cells only so candle bodies stay readable.
		for x := 0; x < w-1 && x < g.w; x++ {
			i := g.idx(x, y)
			if g.ch[i] == ' ' && g.indDots[i] == 0 {
				g.setMarker(x, y, orderLevelCh, col)
			}
		}
		// Live-edge glyph always wins the rightmost column.
		edge := w - 1
		if edge >= g.w {
			edge = g.w - 1
		}
		if edge >= 0 {
			g.setMarker(edge, y, mark, col)
		}
	}
}

var volBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// candleColor mirrors TradingView's body rule: close < open → down (red),
// otherwise up (teal). Dojis (close == open) count as up.
func candleColor(b bars.Bar) uint8 {
	if b.Close < b.Open {
		return colDown
	}
	return colUp
}

// volumeScale returns the volume used as 100% bar height for the visible
// window. A single auction/outlier bar (e.g. RTH close printed into the
// first AH minute with 50–100× normal size) must not flatten every other
// column — that is the usual reason AH volume looks empty vs TradingView
// zooms that exclude the auction print.
//
// When max > 3× the 95th percentile, scale to the 95th percentile and let
// outliers clip to full height (same idea as TV's auto-scale).
func volumeScale(vols []float64) float64 {
	if len(vols) == 0 {
		return 0
	}
	maxV := 0.0
	for _, v := range vols {
		if v > maxV {
			maxV = v
		}
	}
	if maxV <= 0 || len(vols) < 8 {
		return maxV
	}
	sorted := append([]float64(nil), vols...)
	// Insertion sort is fine for terminal-width counts (tens–hundreds).
	for i := 1; i < len(sorted); i++ {
		j := i
		for j > 0 && sorted[j-1] > sorted[j] {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
			j--
		}
	}
	// 95th percentile: ceil(0.95*n)-1, clamped.
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	p95 := sorted[idx]
	// Short windows often land the 95th rank on the max itself, which
	// disables outlier clipping. Step down to the highest value strictly
	// below max so a single auction print can still be scaled away.
	if p95 >= maxV && idx > 0 {
		for idx > 0 && sorted[idx] >= maxV {
			idx--
		}
		p95 = sorted[idx]
	}
	if p95 <= 0 {
		return maxV
	}
	if maxV > p95*3 {
		return p95
	}
	return maxV
}

// renderVolume draws volume bars for the newest bars into h lines, using
// the same barStride spacing as the candle pane so columns line up.
//
// Styling uses pre-baked ANSI sequences (writeStyled); the per-column (fg, bg)
// pair is computed once per bar. Cells are run-coalesced per row so a volume
// bar's same-color cells flush in one write.
func renderVolume(snap bars.Snapshot, w, h int, shading bool) []string {
	if w <= 0 || h <= 0 || len(snap.Bars) == 0 {
		return blankLines(h)
	}
	if mb := maxBars(w); len(snap.Bars) > mb {
		snap.Bars = snap.Bars[len(snap.Bars)-mb:]
	}
	n := len(snap.Bars)
	vols := make([]float64, n)
	for i, b := range snap.Bars {
		vols[i] = b.Volume
	}
	scale := volumeScale(vols)

	// perCol[i] = (fg, bg) for column of bar i, precomputed once.
	type colStyle struct{ fg, bg uint8 }
	perCol := make([]colStyle, n)
	if shading {
		for i, b := range snap.Bars {
			perCol[i] = colStyle{candleColor(b), sessionBG(session.At(b.Start))}
		}
	} else {
		for i, b := range snap.Bars {
			perCol[i] = colStyle{candleColor(b), bgNone}
		}
	}

	// heights[i] = how many eighth-block units bar i should occupy (0 = empty).
	heights := make([]int, n)
	if scale > 0 {
		for i, b := range snap.Bars {
			v := b.Volume
			if v > scale {
				v = scale
			}
			units := v / scale * float64(h*8)
			if b.Volume > 0 && units < 1 {
				units = 1
			}
			heights[i] = int(units)
		}
	}

	// Build one []uint8 grid per row holding the rune index (0=space, 1=full,
	// 2..9=eighth-block) so we can coalesce runs of the same (rune, fg, bg).
	const (
		cellSpace = 0
		cellFull  = 1
	)
	cellRune := func(rowIdx, colIdx int) uint8 {
		units := heights[colIdx]
		if units <= 0 {
			return cellSpace
		}
		full := units / 8
		// rowIdx 0 is the top; volume grows from the bottom (row h-1).
		fromBottom := h - 1 - rowIdx
		if fromBottom < full {
			return cellFull
		}
		rem := units % 8
		if fromBottom == full && rem > 0 {
			return uint8(2 + rem - 1) // 2..9 → volBlocks index 0..7
		}
		return cellSpace
	}

	// Precompute column-to-bar index mapping once per frame (O(W) instead of O(W*H*N)).
	barMap := make([]int, w)
	for x := 0; x < w; x++ {
		barMap[x] = -1
	}
	for i := 0; i < n; i++ {
		cx := barCol(w, n, i)
		if cx >= 0 && cx < w {
			barMap[cx] = i
		}
	}

	lines := make([]string, h)
	var sb strings.Builder
	var runSB strings.Builder
	sb.Grow(w * 24)
	runSB.Grow(w * 3)
	for y := 0; y < h; y++ {
		sb.Reset()
		runSB.Reset()
		var runFG, runBG uint8
		runActive := false
		flush := func() {
			if runSB.Len() == 0 {
				return
			}
			writeStyled(&sb, runFG, runBG, runSB.String())
			runSB.Reset()
			runActive = false
		}
		for x := 0; x < w; x++ {
			colIdx := barMap[x]
			var fg, bg uint8 = colNone, bgNone
			runeIdx := uint8(cellSpace)
			rn := ' '
			if colIdx >= 0 {
				fg, bg = perCol[colIdx].fg, perCol[colIdx].bg
				runeIdx = cellRune(y, colIdx)
				switch runeIdx {
				case cellSpace:
					rn = ' '
				case cellFull:
					rn = '█'
				default:
					rn = volBlocks[int(runeIdx)-2]
				}
			}
			if runActive && (fg != runFG || bg != runBG) {
				flush()
			}
			if !runActive {
				runFG, runBG = fg, bg
				runActive = true
			}
			runSB.WriteRune(rn)
		}
		flush()
		lines[y] = sb.String()
	}
	return lines
}

func blankLines(h int) []string {
	if h < 0 {
		h = 0
	}
	return make([]string, h)
}

// renderTimeRuler draws a single-line time axis under the candle pane,
// TradingView-style: sparse labels aligned to the columns of the bars they
// describe. Labels are left-aligned at their bar's column and never overlap
// (a candidate label is dropped if it would collide with the previous one).
// The newest (live-edge) label is right-aligned so tags like "15:04ET" fit
// instead of clipping to a single character at column w-1.
// Format depends on the TF: HH:MM for intraday, MMM DD for daily; a full
// "MMM DD HH:MM" label marks the first label and each day boundary.
// Intraday times are America/New_York (ET) — same as the US equity session
// clock. TradingView set to UTC will show labels 4–5 hours ahead (e.g. our
// 19:59 ET is 23:59 UTC); the trailing "ET" on the newest label avoids that
// mix-up when comparing charts.
// Returns exactly one line of width w.
func renderTimeRuler(snap bars.Snapshot, w int, tf bars.TF) string {
	if w <= 0 || len(snap.Bars) == 0 {
		return strings.Repeat(" ", max(w, 0))
	}
	n := len(snap.Bars)
	if mb := maxBars(w); n > mb {
		snap.Bars = snap.Bars[n-mb:]
		n = mb
	}

	daily := tf == bars.TF1d

	type lbl struct {
		cx    int
		text  string
		isDay bool
		right bool // right-align so live-edge text ends at cx
	}
	var labels []lbl
	prevDate := ""
	// Aim for roughly one label every ~10 cells of plot width.
	step := n / (w/10 + 1)
	if step < 1 {
		step = 1
	}
	for i := 0; i < n; i += step {
		cx := barCol(w, n, i)
		t := snap.Bars[i].Start.In(session.Location())
		date := t.Format("2006-01-02")
		isDay := date != prevDate
		prevDate = date
		var text string
		if daily {
			text = t.Format("Jan 02")
		} else if isDay {
			text = t.Format("Jan 02 15:04")
		} else {
			text = t.Format("15:04")
		}
		labels = append(labels, lbl{cx: cx, text: text, isDay: isDay})
	}
	// Always include the newest bar for live-edge orientation, tagged ET
	// so a UTC TradingView axis is not mistaken for the same clock.
	if last := n - 1; last >= 0 {
		lastCX := barCol(w, n, last)
		t := snap.Bars[last].Start.In(session.Location())
		var text string
		if daily {
			text = t.Format("Jan 02")
		} else {
			text = t.Format("15:04") + "ET"
		}
		if len(labels) == 0 || labels[len(labels)-1].cx != lastCX {
			labels = append(labels, lbl{cx: lastCX, text: text, right: true})
		} else {
			labels[len(labels)-1].text = text
			labels[len(labels)-1].right = true
		}
	}

	// Reserve space for the live-edge label so sparse left-aligned labels
	// cannot crowd it into a single clipped character at column w-1.
	liveText := ""
	liveCX := 0
	sparse := make([]lbl, 0, len(labels))
	for _, l := range labels {
		if l.right {
			liveText = l.text
			liveCX = l.cx
			continue
		}
		sparse = append(sparse, l)
	}
	liveReserve := 0
	if liveText != "" {
		liveReserve = utf8.RuneCountInString(liveText) + 1
		if liveReserve > w {
			liveReserve = w
		}
	}
	plotEnd := w - liveReserve

	runes := make([]rune, w)
	for i := range runes {
		runes[i] = ' '
	}
	nextFree := 0
	for _, l := range sparse {
		start := l.cx
		if start < nextFree || start >= plotEnd {
			continue
		}
		labelRunes := []rune(l.text)
		end := start + len(labelRunes)
		if end > plotEnd {
			labelRunes = labelRunes[:plotEnd-start]
			end = plotEnd
		}
		if len(labelRunes) == 0 {
			continue
		}
		for i, r := range labelRunes {
			runes[start+i] = r
		}
		nextFree = end + 1
	}
	if liveText != "" {
		textRunes := []rune(liveText)
		if len(textRunes) > w {
			textRunes = textRunes[len(textRunes)-w:]
		}
		// Prefer right-aligning to the live bar column; fall back to the
		// plot's right edge so the full "15:04ET" tag is visible.
		start := liveCX - len(textRunes) + 1
		if start < 0 || start+len(textRunes) > w {
			start = w - len(textRunes)
		}
		if start < 0 {
			start = 0
		}
		for i, r := range textRunes {
			runes[start+i] = r
		}
	}

	dim := stDim
	return dim.Render(strings.TrimRight(string(runes), " "))
}

// priceAxisWidth is the right-side gutter width for the vertical price
// scale, in terminal cells. Big enough for "│ NNNNN.NN" style labels on
// most equities (separator + space + price).
const priceAxisWidth = 10

// renderPriceAxis draws a TradingView-style vertical price scale for the
// candle pane. Emits h lines of width w with a left separator and
// right-aligned price labels. Labels use the same min/max mapping as
// renderCandles so they line up with the bars.
func renderPriceAxis(min, max float64, w, h int) []string {
	if w <= 0 || h <= 0 {
		return blankLines(h)
	}
	lines := make([]string, h)
	blank := priceAxisLine(w, "")
	for i := range lines {
		lines[i] = blank
	}
	if min >= max {
		return lines
	}

	// Same mapping as yOfCell in renderCandles: row 0 = max, row h-1 = min.
	priceAtRow := func(row int) float64 {
		if h == 1 {
			return (min + max) / 2
		}
		return max - (float64(row)/float64(h-1))*(max-min)
	}

	dim := stDim
	step := 3
	if h/step < 4 {
		step = 2
	}
	if h/step < 3 {
		step = 1
	}
	labelled := map[int]bool{}
	for row := 0; row < h; row += step {
		labelled[row] = true
	}
	labelled[h-1] = true

	for row := range labelled {
		p := priceAtRow(row)
		label := dim.Render(fmtPrice(p, w-2))
		lines[row] = priceAxisLine(w, label)
	}
	return lines
}

// priceAxisLine builds one axis cell: "│" separator + right-aligned label
// padded to width w. Empty label yields "│" + spaces.
func priceAxisLine(w int, label string) string {
	if w <= 0 {
		return ""
	}
	sep := stAxisSep
	if w == 1 {
		return sep
	}
	inner := w - 1
	if label == "" {
		return sep + strings.Repeat(" ", inner)
	}
	lw := visibleWidth(label)
	pad := inner - lw
	if pad < 0 {
		pad = 0
	}
	return sep + strings.Repeat(" ", pad) + label
}

// fmtPrice formats p into a string with 2 decimals, clipped to w runes.
// Oversized values keep the most significant digits (prefix truncate) rather
// than the least significant (which turned 123456.78 into "6.78").
func fmtPrice(p float64, w int) string {
	s := formatFloat(p)
	if w > 0 && len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		// Prefer compact scientific when still too wide after prefix cut.
		sci := strconv.FormatFloat(p, 'e', 1, 64)
		if len(sci) <= w {
			return sci
		}
		return s[:w]
	}
	return s
}

func formatFloat(p float64) string {
	return strconv.FormatFloat(p, 'f', 2, 64)
}

// visibleWidth returns the visible (non-ANSI) width of a styled string,
// counting Unicode code points (not bytes) so non-ASCII labels pad correctly.
func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i++
			for i < len(s) && s[i] != 'm' {
				i++
			}
			if i < len(s) {
				i++ // consume 'm'
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size <= 0 {
			size = 1
		}
		i += size
		w++
	}
	return w
}
