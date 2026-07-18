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

// canvas is a braille dot grid, one rune per 2×4 dots.
type canvas struct {
	w, h  int // cells
	dots  []uint8
	color []uint8
	bg    []uint8
}

func newCanvas(w, h int) *canvas {
	n := w * h
	return &canvas{w: w, h: h, dots: make([]uint8, n), color: make([]uint8, n), bg: make([]uint8, n)}
}

// setDot lights one dot at dot-coordinates (x: 0..2w-1, y: 0..4h-1).
// Existing color is only overwritten by non-indicator (candle) colors
// when force is set.
func (c *canvas) setDot(x, y int, col uint8) {
	if x < 0 || x >= c.w*2 || y < 0 || y >= c.h*4 {
		return
	}
	idx := (y/4)*c.w + (x / 2)
	c.dots[idx] |= dotBit[x%2][y%4]
	c.color[idx] = col
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
			cv.setDot(cx*2, y, colSMA)
			cv.setDot(cx*2+1, y, colSMA)
		}
		if opts.ShowEMA && !math.IsNaN(snap.EMA[i]) {
			y := yOf(snap.EMA[i])
			cv.setDot(cx*2, y, colEMA)
			cv.setDot(cx*2+1, y, colEMA)
		}
		if opts.ShowVWAP && !math.IsNaN(snap.VWAP[i]) {
			y := yOf(snap.VWAP[i])
			cv.setDot(cx*2, y, colVWAP)
			cv.setDot(cx*2+1, y, colVWAP)
		}
	}

	// Candles: wick in left dot-column, body across both.
	for i, b := range snap.Bars {
		cx := w - n + i
		col := colUp
		if b.Close < b.Open {
			col = colDown
		}
		yHi, yLo := yOf(b.High), yOf(b.Low)
		for y := yHi; y <= yLo; y++ {
			cv.setDot(cx*2, y, col)
		}
		yO, yC := yOf(b.Open), yOf(b.Close)
		top, bot := yO, yC
		if top > bot {
			top, bot = bot, top
		}
		for y := top; y <= bot; y++ {
			cv.setDot(cx*2, y, col)
			cv.setDot(cx*2+1, y, col)
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
			lines_ := styles[y][x]
			if rows[y][x] == ' ' {
				sb.WriteRune(' ')
			} else {
				sb.WriteString(lines_.Render(string(rows[y][x])))
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
