package ui

import (
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"trade-kernel/internal/bars"
	"trade-kernel/internal/session"
)

// Braille dot bit positions: [dx][dy], dx∈{0,1}, dy∈{0..3}.
var dotBit = [2][4]uint8{{0x01, 0x02, 0x04, 0x40}, {0x08, 0x10, 0x20, 0x80}}

// Foreground palette indices.
const (
	colNone uint8 = iota
	colUp
	colDown
	colSMA
	colEMA
	colVWAP
)

// Background palette indices.
const (
	bgNone uint8 = iota
	bgExtended
	bgOvernight
)

var fgColors = map[uint8]lipgloss.Color{
	colUp:   lipgloss.Color("10"),
	colDown: lipgloss.Color("9"),
	colSMA:  lipgloss.Color("12"),
	colEMA:  lipgloss.Color("13"),
	colVWAP: lipgloss.Color("11"),
}

var bgColors = map[uint8]lipgloss.Color{
	bgExtended:  lipgloss.Color("234"),
	bgOvernight: lipgloss.Color("17"),
}

// Render layers, lowest to highest. A dot's color wins a cell only if its
// layer is >= the cell's current layer. Indicators layer above candles so
// an SMA/EMA/VWAP line reads continuously through candle bodies instead of
// being absorbed into the candle's color.
const (
	layerCandle uint8 = iota
	layerIndicator
)

// canvas is a braille dot grid, one rune per 2×4 dots.
type canvas struct {
	w, h  int // cells
	dots  []uint8
	color []uint8
	layer []uint8 // per-cell highest layer that has written color so far
	bg    []uint8
}

func newCanvas(w, h int) *canvas {
	n := w * h
	return &canvas{w: w, h: h, dots: make([]uint8, n), color: make([]uint8, n), layer: make([]uint8, n), bg: make([]uint8, n)}
}

// setDot lights one dot at dot-coordinates (x: 0..2w-1, y: 0..4h-1). The
// cell's color is set to col only when layer >= the cell's current layer,
// so higher layers (indicators) override lower ones (candles) and the
// most-informative color wins. The dot itself is always lit regardless.
func (c *canvas) setDot(x, y int, col uint8, layer uint8) {
	if x < 0 || x >= c.w*2 || y < 0 || y >= c.h*4 {
		return
	}
	idx := (y/4)*c.w + (x / 2)
	c.dots[idx] |= dotBit[x%2][y%4]
	if layer >= c.layer[idx] {
		c.color[idx] = col
		c.layer[idx] = layer
	}
}

func (c *canvas) setColBg(cx int, bg uint8) {
	if cx < 0 || cx >= c.w {
		return
	}
	for y := 0; y < c.h; y++ {
		c.bg[y*c.w+cx] = bg
	}
}

// render emits the canvas as styled lines, grouping runs of equally
// styled cells into one lipgloss segment.
func (c *canvas) render() []string {
	lines := make([]string, c.h)
	for y := 0; y < c.h; y++ {
		var sb strings.Builder
		var runFG, runBG uint8
		var run strings.Builder
		flush := func() {
			if run.Len() == 0 {
				return
			}
			st := lipgloss.NewStyle()
			if f, ok := fgColors[runFG]; ok {
				st = st.Foreground(f)
			}
			if b, ok := bgColors[runBG]; ok {
				st = st.Background(b)
			}
			sb.WriteString(st.Render(run.String()))
			run.Reset()
		}
		started := false
		for x := 0; x < c.w; x++ {
			idx := y*c.w + x
			fg, bg := c.color[idx], c.bg[idx]
			if c.dots[idx] == 0 {
				fg = colNone
			}
			if started && (fg != runFG || bg != runBG) {
				flush()
			}
			if !started || run.Len() == 0 {
				runFG, runBG = fg, bg
				started = true
			}
			if c.dots[idx] == 0 {
				run.WriteRune(' ')
			} else {
				run.WriteRune(rune(0x2800) + rune(c.dots[idx]))
			}
		}
		flush()
		lines[y] = sb.String()
	}
	return lines
}

// ChartOpts controls rendering.
type ChartOpts struct {
	ShowSMA, ShowEMA, ShowVWAP bool
	SessionShading             bool
}

// renderCandles draws the candlestick chart for snap into a canvas of
// w×h cells and returns the rendered lines. Bars are drawn
// left-to-right, oldest first, right-aligned.
func renderCandles(snap bars.Snapshot, w, h int, opts ChartOpts) []string {
	if w <= 0 || h <= 0 || len(snap.Bars) == 0 {
		return blankLines(h)
	}
	if len(snap.Bars) > w {
		// Right-align: keep the newest w bars.
		off := len(snap.Bars) - w
		snap.Bars = snap.Bars[off:]
		snap.SMA = snap.SMA[off:]
		snap.EMA = snap.EMA[off:]
		snap.VWAP = snap.VWAP[off:]
	}
	n := len(snap.Bars)

	// Price range across bars and enabled overlays.
	min, max := math.Inf(1), math.Inf(-1)
	grow := func(v float64) {
		if math.IsNaN(v) {
			return
		}
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	for i, b := range snap.Bars {
		grow(b.Low)
		grow(b.High)
		if opts.ShowSMA {
			grow(snap.SMA[i])
		}
		if opts.ShowEMA {
			grow(snap.EMA[i])
		}
		if opts.ShowVWAP {
			grow(snap.VWAP[i])
		}
	}
	if min >= max {
		max = min + 1
	}
	pad := (max - min) * 0.02
	min, max = min-pad, max+pad

	cv := newCanvas(w, h)
	yOf := func(p float64) int {
		y := int(math.Round((max - p) / (max - min) * float64(h*4-1)))
		if y < 0 {
			y = 0
		}
		if y > h*4-1 {
			y = h*4 - 1
		}
		return y
	}

	// Session shading + indicator overlays first (candles overwrite).
	for i, b := range snap.Bars {
		cx := w - n + i
		if opts.SessionShading {
			s := session.At(b.Start)
			switch {
			case s == session.Overnight:
				cv.setColBg(cx, bgOvernight)
			case session.Extended(s):
				cv.setColBg(cx, bgExtended)
			}
		}
		if opts.ShowSMA && !math.IsNaN(snap.SMA[i]) {
			y := yOf(snap.SMA[i])
			cv.setDot(cx*2, y, colSMA, layerIndicator)
			cv.setDot(cx*2+1, y, colSMA, layerIndicator)
		}
		if opts.ShowEMA && !math.IsNaN(snap.EMA[i]) {
			y := yOf(snap.EMA[i])
			cv.setDot(cx*2, y, colEMA, layerIndicator)
			cv.setDot(cx*2+1, y, colEMA, layerIndicator)
		}
		if opts.ShowVWAP && !math.IsNaN(snap.VWAP[i]) {
			y := yOf(snap.VWAP[i])
			cv.setDot(cx*2, y, colVWAP, layerIndicator)
			cv.setDot(cx*2+1, y, colVWAP, layerIndicator)
		}
	}

	// Candles: wick in left dot-column, body across both. Drawn at the
	// lower layer so indicator overlays (drawn above) win shared cells.
	for i, b := range snap.Bars {
		cx := w - n + i
		col := colUp
		if b.Close < b.Open {
			col = colDown
		}
		yHi, yLo := yOf(b.High), yOf(b.Low)
		for y := yHi; y <= yLo; y++ {
			cv.setDot(cx*2, y, col, layerCandle)
		}
		yO, yC := yOf(b.Open), yOf(b.Close)
		top, bot := yO, yC
		if top > bot {
			top, bot = bot, top
		}
		for y := top; y <= bot; y++ {
			cv.setDot(cx*2, y, col, layerCandle)
			cv.setDot(cx*2+1, y, col, layerCandle)
		}
	}
	return cv.render()
}

var volBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// renderVolume draws volume bars for the newest w bars into h lines.
func renderVolume(snap bars.Snapshot, w, h int, shading bool) []string {
	if w <= 0 || h <= 0 || len(snap.Bars) == 0 {
		return blankLines(h)
	}
	if len(snap.Bars) > w {
		snap.Bars = snap.Bars[len(snap.Bars)-w:]
	}
	n := len(snap.Bars)
	max := 0.0
	for _, b := range snap.Bars {
		if b.Volume > max {
			max = b.Volume
		}
	}
	rows := make([][]rune, h)
	styles := make([][]lipgloss.Style, h)
	for i := range rows {
		rows[i] = make([]rune, w)
		styles[i] = make([]lipgloss.Style, w)
		for x := range rows[i] {
			rows[i][x] = ' '
		}
	}
	if max > 0 {
		for i, b := range snap.Bars {
			cx := w - n + i
			units := b.Volume / max * float64(h*8) // eighth-block units
			fg := fgColors[colUp]
			if b.Close < b.Open {
				fg = fgColors[colDown]
			}
			st := lipgloss.NewStyle().Foreground(fg)
			if shading {
				s := session.At(b.Start)
				if s == session.Overnight {
					st = st.Background(bgColors[bgOvernight])
				} else if session.Extended(s) {
					st = st.Background(bgColors[bgExtended])
				}
			}
			full := int(units) / 8
			rem := int(units) % 8
			for y := 0; y < full && y < h; y++ {
				rows[h-1-y][cx] = '█'
				styles[h-1-y][cx] = st
			}
			if full < h && rem > 0 {
				rows[h-1-full][cx] = volBlocks[rem-1]
				styles[h-1-full][cx] = st
			}
		}
	}
	lines := make([]string, h)
	for y := 0; y < h; y++ {
		var sb strings.Builder
		for x := 0; x < w; x++ {
			st := styles[y][x]
			if rows[y][x] == ' ' {
				sb.WriteRune(' ')
			} else {
				sb.WriteString(st.Render(string(rows[y][x])))
			}
		}
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
// Format depends on the TF: HH:MM for intraday, MMM DD for daily; a full
// "MMM DD HH:MM" label marks the first label and each day boundary.
// Returns exactly one line of width w.
func renderTimeRuler(snap bars.Snapshot, w int, tf bars.TF) string {
	if w <= 0 || len(snap.Bars) == 0 {
		return strings.Repeat(" ", w)
	}
	n := len(snap.Bars)
	if n > w {
		// Mirror renderCandles' right-align: keep newest w bars.
		snap.Bars = snap.Bars[n-w:]
		n = w
	}

	daily := tf == bars.TF1d

	// Build the candidate label list, oldest→newest.
	type lbl struct {
		cx    int
		text  string
		isDay bool // marks a day-boundary / first label (gets the date)
	}
	var labels []lbl
	prevDate := ""
	// Aim for one label every ~10 cells. step = max(1, n / (w/10)).
	step := n / (w/10 + 1)
	if step < 1 {
		step = 1
	}
	for i := 0; i < n; i += step {
		cx := w - n + i
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
	// Always include the newest bar for live-edge orientation.
	if last := n - 1; last >= 0 && (len(labels) == 0 || labels[len(labels)-1].cx != w-1) {
		t := snap.Bars[last].Start.In(session.Location())
		var text string
		if daily {
			text = t.Format("Jan 02")
		} else {
			text = t.Format("15:04")
		}
		labels = append(labels, lbl{cx: w - 1, text: text})
	}

	// Render into a rune buffer, left-aligning each label at its column and
	// skipping any label that would overlap the previous one's tail.
	runes := make([]rune, w)
	for i := range runes {
		runes[i] = ' '
	}
	nextFree := 0 // next writable column
	for _, l := range labels {
		start := l.cx
		if start < nextFree {
			continue // would collide — skip
		}
		end := start + len(l.text)
		if end > w {
			// Clip the label rather than skip — the live edge should always show.
			l.text = l.text[:w-start]
			end = w
		}
		for i, r := range l.text {
			runes[start+i] = r
		}
		nextFree = end + 1 // one-space gap
	}

	dim := lipgloss.NewStyle().Faint(true)
	return dim.Render(strings.TrimRight(string(runes), " "))
}
