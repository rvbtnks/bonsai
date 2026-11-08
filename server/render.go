package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

// --- palette ----------------------------------------------------------------

// ColorID is the logical colour of a cell. The renderer maps it to a tcell
// style, or collapses everything to the default colour on a monochrome
// terminal (where the glyphs carry the meaning instead).
type ColorID int

const (
	ColNone ColorID = iota
	ColWood
	ColTwig
	ColDead
	ColLeafGood
	ColLeafWarn
	ColLeafCrit
	ColPot
	ColPotCracked
	ColMoss
	ColText
	ColTextDim
	ColAccent
	ColWarn
	ColCrit
)

// --- canvas -----------------------------------------------------------------

// Cell is one terminal position.
type Cell struct {
	R    rune
	C    ColorID
	Bold bool
}

// Canvas is an off-screen grid. Everything (tree, panels, status line) is drawn
// into it first, then blitted to tcell in one pass. That keeps the drawing code
// free of terminal concerns and makes -dump and the unit tests trivial.
type Canvas struct {
	W, H  int
	Cells []Cell
	Mono  bool

	// clip limits where drawing may write, so the tree can never spill over
	// the header or the status line on a short terminal.
	clip    Rect
	clipSet bool
}

func NewCanvas(w, h int, mono bool) *Canvas {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	c := &Canvas{W: w, H: h, Mono: mono}
	c.Cells = make([]Cell, w*h)
	c.Clear()
	return c
}

func (c *Canvas) Clear() {
	for i := range c.Cells {
		c.Cells[i] = Cell{R: ' ', C: ColNone}
	}
}

func (c *Canvas) idx(x, y int) int { return y*c.W + x }

func (c *Canvas) In(x, y int) bool {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		return false
	}
	if c.clipSet {
		return x >= c.clip.X && y >= c.clip.Y && x < c.clip.Right() && y < c.clip.Bottom()
	}
	return true
}

// SetClip restricts all further drawing to r. Every drawing primitive goes
// through In(), so one check covers the whole canvas.
func (c *Canvas) SetClip(r Rect) {
	if r.X < 0 {
		r.X = 0
	}
	if r.Y < 0 {
		r.Y = 0
	}
	if r.Right() > c.W {
		r.W = c.W - r.X
	}
	if r.Bottom() > c.H {
		r.H = c.H - r.Y
	}
	c.clip, c.clipSet = r, true
}

// ClearClip removes the restriction.
func (c *Canvas) ClearClip() { c.clipSet = false }

// Get reads a cell back, mirroring tcell.Screen.GetContent so tests can use one
// shape for both the canvas and the simulation screen.
func (c *Canvas) Get(x, y int) (rune, []rune, ColorID, int) {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		return ' ', nil, ColNone, 0
	}
	cell := c.Cells[c.idx(x, y)]
	return cell.R, nil, cell.C, 1
}

// Set writes a cell, overwriting whatever was there.
func (c *Canvas) Set(x, y int, r rune, col ColorID) {
	if !c.In(x, y) || r == 0 {
		return
	}
	c.Cells[c.idx(x, y)] = Cell{R: r, C: col}
}

// SetBold writes a bold cell.
func (c *Canvas) SetBold(x, y int, r rune, col ColorID) {
	if !c.In(x, y) || r == 0 {
		return
	}
	c.Cells[c.idx(x, y)] = Cell{R: r, C: col, Bold: true}
}

// SetF is the float-coordinate variant used by the tree code; it never
// overwrites existing foliage, so clusters layer naturally.
func (c *Canvas) SetF(x, y int, r rune, col ColorID) {
	if !c.In(x, y) {
		return
	}
	i := c.idx(x, y)
	if c.Cells[i].R != ' ' && c.Cells[i].R != 0 {
		// Wood wins over leaves; leaves win over nothing.
		if isWood(c.Cells[i].R) || isWood(r) {
			if isWood(r) {
				c.Cells[i] = Cell{R: r, C: col}
			}
			return
		}
	}
	c.Cells[i] = Cell{R: r, C: col}
}

// SetIfEmpty only writes blanks.
func (c *Canvas) SetIfEmpty(x, y int, r rune, col ColorID) {
	if !c.In(x, y) {
		return
	}
	i := c.idx(x, y)
	if c.Cells[i].R == ' ' {
		c.Cells[i] = Cell{R: r, C: col}
	}
}

// SetBlank erases a cell (used to clear a window behind a label).
func (c *Canvas) SetBlank(x, y int) {
	if !c.In(x, y) {
		return
	}
	c.Cells[c.idx(x, y)] = Cell{R: ' ', C: ColNone}
}

func isWood(r rune) bool {
	switch r {
	case '|', '-', '/', '\\', '│', '─', '╱', '╲':
		return true
	}
	return false
}

// Text writes a string, clipping at the canvas edges.
func (c *Canvas) Text(x, y int, s string, col ColorID) {
	if !c.In(x, y) && !(y >= 0 && y < c.H) {
		return
	}
	for _, r := range s {
		if x >= c.W {
			return
		}
		if x >= 0 {
			c.Set(x, y, r, col)
		}
		x++
	}
}

// TextClipped writes at most n runes.
func (c *Canvas) TextClipped(x, y int, s string, col ColorID) {
	c.Text(x, y, s, col)
}

// TextCentred writes s centred on cx, clipped to the canvas.
func (c *Canvas) TextCentred(cx, y int, s string, col ColorID) {
	c.Text(cx-len([]rune(s))/2, y, s, col)
}

// Branch rasterises a float-coordinate line, choosing wood glyphs from the
// segment direction. thick doubles the line for the trunk.
func (c *Canvas) Branch(x0, y0, x1, y1 float64, col ColorID, thick, unicode bool) {
	dx, dy := x1-x0, y1-y0
	r := woodRune(dx, dy, unicode)
	steps := int(math.Max(math.Abs(dx), math.Abs(dy))*2) + 1
	if steps > 400 {
		steps = 400
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(x0 + dx*t))
		y := int(math.Round(y0 + dy*t))
		c.SetF(x, y, r, col)
		if thick {
			c.SetF(x+1, y, r, col)
		}
	}
}

// Trunk draws a tapered trunk: two cells wide for the lower part of the run,
// one cell wide above. Without the taper a straight trunk reads as a ladder.
func (c *Canvas) Trunk(x0, y0, x1, y1 float64, col ColorID, unicode bool) {
	dx, dy := x1-x0, y1-y0
	r := woodRune(dx, dy, unicode)
	span := math.Abs(dy)
	if span < 0.0001 {
		span = math.Abs(dx)
	}
	if span < 0.0001 {
		c.SetF(int(math.Round(x0)), int(math.Round(y0)), r, col)
		return
	}
	steps := int(math.Max(math.Abs(dx), math.Abs(dy))*3) + 1
	if steps > 600 {
		steps = 600
	}
	// y0 is the base (larger y), y1 the crown (smaller y).
	base, crown := y0, y1
	if crown > base {
		base, crown = crown, base
	}
	thickBelow := base - (base-crown)*0.38
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(x0 + dx*t))
		y := int(math.Round(y0 + dy*t))
		c.SetF(x, y, r, col)
		if float64(y) >= thickBelow {
			c.SetF(x+1, y, r, col)
		}
	}
}

// Line is an integer-coordinate line with a fixed rune.
func (c *Canvas) Line(x0, y0, x1, y1 int, r rune, col ColorID) {
	dx, dy := abs(x1-x0), abs(y1-y0)
	sx, sy := -1, -1
	if x0 < x1 {
		sx = 1
	}
	if y0 < y1 {
		sy = 1
	}
	err := dx - dy
	for {
		c.Set(x0, y0, r, col)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

// HLine draws a run of the same rune, clipped.
func (c *Canvas) HLine(x, y, n int, r rune, col ColorID) {
	for i := 0; i < n; i++ {
		c.Set(x+i, y, r, col)
	}
}

// String renders the canvas as plain text. Used by -dump, by tests, and by the
// headless screenshot path.
func (c *Canvas) String() string {
	var b strings.Builder
	for y := 0; y < c.H; y++ {
		row := make([]rune, c.W)
		for x := 0; x < c.W; x++ {
			row[x] = c.Cells[c.idx(x, y)].R
		}
		b.WriteString(strings.TrimRight(string(row), " "))
		if y < c.H-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// --- tcell renderer ---------------------------------------------------------

// Renderer blits a canvas to the screen.
type Renderer struct {
	scr    tcell.Screen
	styles map[ColorID]tcell.Style
	mono   bool
}

func NewRenderer(scr tcell.Screen) *Renderer {
	mono := scr.Colors() <= 2
	r := &Renderer{scr: scr, mono: mono, styles: map[ColorID]tcell.Style{}}
	def := tcell.StyleDefault
	if mono {
		for _, id := range []ColorID{ColNone, ColWood, ColTwig, ColDead, ColLeafGood,
			ColLeafWarn, ColLeafCrit, ColPot, ColPotCracked, ColMoss, ColText,
			ColTextDim, ColAccent, ColWarn, ColCrit} {
			r.styles[id] = def
		}
		r.styles[ColLeafCrit] = def.Bold(true)
		r.styles[ColCrit] = def.Bold(true)
		r.styles[ColTextDim] = def.Dim(true)
		return r
	}
	r.styles[ColNone] = def
	r.styles[ColWood] = def.Foreground(tcell.ColorSandyBrown)
	r.styles[ColTwig] = def.Foreground(tcell.ColorMaroon)
	r.styles[ColDead] = def.Foreground(tcell.ColorGray)
	r.styles[ColLeafGood] = def.Foreground(tcell.ColorGreen)
	r.styles[ColLeafWarn] = def.Foreground(tcell.ColorYellow)
	r.styles[ColLeafCrit] = def.Foreground(tcell.ColorRed).Bold(true)
	r.styles[ColPot] = def.Foreground(tcell.ColorWhite)
	r.styles[ColPotCracked] = def.Foreground(tcell.ColorGray)
	r.styles[ColMoss] = def.Foreground(tcell.ColorDarkGreen)
	r.styles[ColText] = def.Foreground(tcell.ColorSilver)
	r.styles[ColTextDim] = def.Foreground(tcell.ColorGray)
	r.styles[ColAccent] = def.Foreground(tcell.ColorTeal).Bold(true)
	r.styles[ColWarn] = def.Foreground(tcell.ColorYellow)
	r.styles[ColCrit] = def.Foreground(tcell.ColorRed).Bold(true)
	return r
}

func (r *Renderer) Mono() bool { return r.mono }

// Draw pushes the canvas to the screen and shows it.
func (r *Renderer) Draw(c *Canvas) {
	r.scr.Clear()
	w, h := r.scr.Size()
	for y := 0; y < c.H && y < h; y++ {
		for x := 0; x < c.W && x < w; x++ {
			cell := c.Cells[c.idx(x, y)]
			st := r.styles[cell.C]
			if cell.Bold {
				st = st.Bold(true)
			}
			r.scr.SetContent(x, y, cell.R, nil, st)
		}
	}
	r.scr.Show()
}

// --- panels -----------------------------------------------------------------

// panel draws a bordered box over the canvas and returns the inner origin.
func panel(c *Canvas, x, y, w, h int, title string, uni bool, col ColorID) (int, int) {
	if w < 4 || h < 3 {
		return x, y
	}
	// Clear the interior first: an overlay must be readable over the canopy.
	for row := y; row < y+h; row++ {
		for col := x; col < x+w; col++ {
			c.SetBlank(col, row)
		}
	}
	hz, vt, tl, tr, bl, br := "-", "|", "+", "+", "+", "+"
	if uni {
		hz, vt, tl, tr, bl, br = "─", "│", "┌", "┐", "└", "┘"
	}
	c.Text(x, y, tl+rep(hz, w-2)+tr, col)
	c.Text(x, y+h-1, bl+rep(hz, w-2)+br, col)
	for i := 1; i < h-1; i++ {
		c.Text(x, y+i, vt, col)
		c.Text(x+w-1, y+i, vt, col)
	}
	if title != "" {
		c.Text(x+2, y, " "+trimTo(title, w-5)+" ", ColAccent)
	}
	return x + 2, y + 1
}

func trimTo(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// hostsOverlay is the `l` view: every host with its metrics, vitality and the
// metric that is dragging it down.
func hostsOverlay(c *Canvas, s Snapshot, uni bool) {
	height := len(s.Hosts) + 5
	if height > c.H-2 {
		height = c.H - 2
	}
	if height < 6 || c.W < 30 {
		return
	}
	width := 76
	if width > c.W-4 {
		width = c.W - 4
	}
	x := (c.W - width) / 2
	y := (c.H - height) / 2
	ix, iy := panel(c, x, y, width, height, hostsTitle(s, uni), uni, ColText)

	if len(s.Hosts) == 0 {
		c.Text(ix, iy+1, "no hosts yet - run bonsai-client on a machine", ColTextDim)
		c.Text(ix, y+height-2, "l - close", ColTextDim)
		return
	}

	header := pad("host", 17) + pad("cpu", 6) + pad("temp", 6) + pad("load", 7) +
		pad("up", 9) + pad("vit", 6) + "state"
	c.Text(ix, iy, trimTo(header, width-4), ColTextDim)

	for i, h := range s.Hosts {
		row := iy + 2 + i
		if row >= y+height-2 {
			break
		}
		line := pad(trimTo(h.Name, 16), 17) +
			pad(metricText(h.Metrics.CPU, "%.0f%%"), 6) +
			pad(metricText(h.Metrics.Temp, "%.0fC"), 6) +
			pad(metricText(h.Metrics.Load, "%.2f"), 7) +
			pad(upText(h.Metrics.Up), 9) +
			pad(itoa(int(math.Round(h.Vitality*100)))+"%", 6)
		state := h.Band.Short()
		if h.Offender != "" {
			state += " " + h.Offender
		}
		c.Text(ix, row, trimTo(line+state, width-4), bandColor(h.Band))
	}
	c.Text(ix, y+height-2, "l - close", ColTextDim)
}

func hostsTitle(s Snapshot, uni bool) string {
	sep := " "
	if uni {
		sep = " · "
	}
	return "hosts " + itoa(s.HostCount) + " (" + itoa(s.Good) + " ok" + sep +
		itoa(s.Warn) + " warn" + sep + itoa(s.Crit) + " crit" + sep + itoa(s.Stale) + " stale)"
}

func bandColor(b Band) ColorID {
	switch b {
	case BandGood:
		return ColLeafGood
	case BandWarn:
		return ColLeafWarn
	case BandCrit:
		return ColLeafCrit
	case BandStale:
		return ColDead
	}
	return ColText
}

func metricText(p *float64, format string) string {
	if p == nil {
		return "-"
	}
	return sprintf(format, *p)
}

func upText(p *float64) string {
	if p == nil {
		return "-"
	}
	return ShortDuration(time.Duration(*p * float64(time.Second)))
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + rep(" ", n-len(s))
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

func sprintf(format string, v float64) string { return fmt.Sprintf(format, v) }

// helpOverlay is the `?` view.
func helpOverlay(c *Canvas, uni bool) {
	lines := [][2]string{
		{"q", "quit"},
		{"l", "host list: metrics, vitality, offending metric"},
		{"?", "this help"},
		{"^L", "redraw"},
	}
	height := len(lines) + 5
	width := 58
	if width > c.W-2 {
		width = c.W - 2
	}
	if height > c.H-2 {
		height = c.H - 2
	}
	if width < 20 || height < 5 {
		return
	}
	x := (c.W - width) / 2
	y := (c.H - height) / 2
	ix, iy := panel(c, x, y, width, height, "bonsai-monitor", uni, ColText)
	c.Text(ix, iy, "one tree for the whole LAN", ColTextDim)
	for i, l := range lines {
		if iy+2+i >= y+height-2 {
			break
		}
		c.Text(ix, iy+2+i, pad(l[0], 4), ColAccent)
		c.Text(ix+4, iy+2+i, trimTo(l[1], width-8), ColText)
	}
	c.Text(ix, y+height-2, "any other key closes", ColTextDim)
}

// --- composition ------------------------------------------------------------

// Composer turns a Snapshot plus the tree state into a finished canvas. It has
// no terminal dependency, so the same code drives the live UI, the -dump mode
// and the unit tests.
type Composer struct {
	cfg     *Config
	tree    *Tree
	node    *Node
	addr    string
	Overlay int // 0 none, 1 hosts, 2 help
}

func NewComposer(cfg *Config, tree *Tree, node *Node, addr string) *Composer {
	return &Composer{cfg: cfg, tree: tree, node: node, addr: addr}
}

// Compose lays out header, tree, status bar and hint line.
func (co *Composer) Compose(c *Canvas, s Snapshot, now time.Time) {
	tiny := c.H < 9
	headerY := -1
	if !tiny {
		headerY = 0
		left := "bonsai-monitor"
		right := shortAddr(co.addr) + " tcp+udp"
		if len(left)+len(right)+4 <= c.W {
			c.Text(1, headerY, left, ColAccent)
			c.Text(c.W-1-len(right), headerY, right, ColTextDim)
		} else if c.W > len(right)+2 {
			c.Text(c.W-1-len(right), headerY, right, ColTextDim)
		}
	}

	statusY := c.H - 2
	hintY := c.H - 1
	top := 1
	if headerY < 0 {
		top = 0
	}
	bottom := statusY - 1
	if c.H < 6 {
		bottom = c.H - 1
		statusY, hintY = -1, -1
	}
	area := Rect{X: 0, Y: top, W: c.W, H: bottom - top + 1}
	if area.H > 0 {
		c.SetClip(area) // the tree stays inside its own rows
		co.tree.Draw(c, s, area)
		c.ClearClip()
	}

	if statusY >= 0 {
		co.statusLine(c, s, now, statusY)
	}
	if hintY >= 0 {
		co.hintLine(c, s, hintY)
	}

	switch co.Overlay {
	case 1:
		hostsOverlay(c, s, co.cfg.Unicode)
	case 2:
		helpOverlay(c, co.cfg.Unicode)
	}
}

// statusLine is the at-a-glance read: vitality percent + word, a bar, host
// counts and server uptime. It degrades gracefully - the bar shrinks and the
// host detail drops out - so nothing is ever cut off mid-word on a narrow
// terminal.
func (co *Composer) statusLine(c *Canvas, s Snapshot, now time.Time, y int) {
	v := co.tree.Vitality()
	pct := int(math.Round(v * 100))
	// Padding the word keeps the bar from sliding sideways as the state
	// changes between "ok" and "thriving".
	head := "network " + itoa(pct) + "% " + pad(co.cfg.Word(v), 8)
	uptime := "up " + ShortDuration(now.Sub(co.startedAt()))

	// The brief form is the default so the line does not change shape when the
	// status word changes length; the full tally only appears when there is
	// room to spare.
	brief := " hosts " + itoa(s.HostCount) + "  " + counts(s, true)
	minimal := " hosts " + itoa(s.HostCount)
	middles := []string{brief, minimal}
	if c.W >= 118 {
		middles = append([]string{" hosts " + itoa(s.HostCount) + "  (" + counts(s, false) + ")"}, middles...)
	}

	barCol := bandColor(co.cfg.BandOf(v))
	maxBar := c.W / 4
	if maxBar > 34 {
		maxBar = 34
	}
	for barW := maxBar; barW >= 6; barW -= 2 {
		filled := int(math.Round(v * float64(barW)))
		if filled > barW {
			filled = barW
		}
		left := head + "  [" + rep("#", filled) + rep("-", barW-filled) + "]"
		avail := c.W - 2 - len(left) - len(uptime) - 3
		for _, m := range middles {
			if len(m) <= avail {
				c.Text(1, y, left, barCol)
				c.Text(1+len(left), y, m, ColText)
				c.Text(c.W-len(uptime)-1, y, uptime, ColTextDim)
				return
			}
		}
	}
	// Truly tiny terminal: drop the bar entirely.
	c.Text(1, y, trimTo(head, c.W-2), barCol)
	if len(head)+len(minimal)+len(uptime)+3 <= c.W {
		c.Text(1+len(head), y, minimal, ColText)
		c.Text(c.W-len(uptime)-1, y, uptime, ColTextDim)
	}
}

// counts renders the healthy/warning/critical/stale tally, optionally dropping
// the bands that are empty.
func counts(s Snapshot, omitZero bool) string {
	parts := make([]string, 0, 4)
	add := func(n int, name string) {
		if n == 0 && omitZero {
			return
		}
		parts = append(parts, itoa(n)+" "+name)
	}
	add(s.Good, "ok")
	add(s.Warn, "warn")
	add(s.Crit, "crit")
	add(s.Stale, "stale")
	if len(parts) == 0 {
		return "0 ok"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "  " + p
	}
	return out
}

func (co *Composer) startedAt() time.Time {
	if co.node != nil {
		return co.node.start
	}
	return time.Now()
}

// hintLine shows the keys, or the waiting message when nothing has reported in.
func (co *Composer) hintLine(c *Canvas, s Snapshot, y int) {
	if s.HostCount == 0 {
		msg := "listening on " + shortAddr(co.addr) + " ... waiting for clients"
		c.TextCentred(c.W/2, y, trimTo(msg, c.W-2), ColAccent)
		return
	}
	keys := "q quit    l hosts    ? help"
	c.Text(1, y, keys, ColTextDim)
	prune := s.PruneTargets()
	if len(prune) > 0 {
		msg := itoa(len(prune)) + " need pruning - press l"
		if c.W-len(keys)-len(msg)-4 > 0 {
			c.Text(c.W-len(msg)-1, y, msg, ColCrit)
		}
	}
}

// shortAddr renders the bound address for humans: 0.0.0.0:1717 becomes :1717.
func shortAddr(a string) string {
	if strings.HasPrefix(a, "0.0.0.0:") || strings.HasPrefix(a, "[::]:") {
		if i := strings.LastIndex(a, ":"); i >= 0 {
			return a[i:]
		}
	}
	return a
}

// --- the UI loop ------------------------------------------------------------

// UI owns the screen, the render ticker and the key handling.
type UI struct {
	cfg   *Config
	store *Store
	tree  *Tree
	comp  *Composer
	log   *log.Logger
	scr   tcell.Screen
	rend  *Renderer
}

type keyEvent struct {
	quit   bool
	resize bool
	r      rune
}

// RunUI blocks until the user quits or ctx is cancelled.
func RunUI(ctx context.Context, cancel context.CancelFunc, scr tcell.Screen, cfg *Config,
	store *Store, tree *Tree, node *Node, addr string, lg *log.Logger) error {

	u := &UI{
		cfg: cfg, store: store, tree: tree, log: lg, scr: scr,
		rend: NewRenderer(scr),
		comp: NewComposer(cfg, tree, node, addr),
	}

	events := make(chan keyEvent, 32)
	go func() {
		defer func() {
			if r := recover(); r != nil && lg != nil {
				lg.Printf("panic in event pump: %v", r)
			}
			close(events)
		}()
		for {
			ev := scr.PollEvent()
			if ev == nil {
				return
			}
			switch e := ev.(type) {
			case *tcell.EventKey:
				ke := keyEvent{r: e.Rune()}
				switch e.Key() {
				case tcell.KeyCtrlC, tcell.KeyEscape:
					ke.quit = true
				case tcell.KeyCtrlL:
					ke.resize = true
				}
				switch e.Rune() {
				case 'q', 'Q':
					ke.quit = true
				case 'l', 'L':
					ke.r = 'l'
				case '?', 'h', 'H':
					ke.r = '?'
				}
				select {
				case events <- ke:
				default:
				}
				if ke.quit {
					return
				}
			case *tcell.EventResize:
				select {
				case events <- keyEvent{resize: true}:
				default:
				}
				scr.Sync()
			}
		}
	}()

	tick := time.NewTicker(cfg.RenderEvery())
	defer tick.Stop()

	u.frame() // draw at once, before the first tick

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ke, ok := <-events:
			if !ok { // screen closed under us
				cancel()
				return nil
			}
			switch {
			case ke.quit:
				cancel()
				return nil
			case ke.resize:
				u.comp.Overlay = 0
			case ke.r == 'l':
				u.comp.Overlay = flip(u.comp.Overlay, 1)
			case ke.r == '?':
				u.comp.Overlay = flip(u.comp.Overlay, 2)
			default:
				u.comp.Overlay = 0
			}
			u.frame()
		case <-tick.C:
			u.frame()
		}
	}
}

func flip(cur, want int) int {
	if cur == want {
		return 0
	}
	return want
}

// frame takes one snapshot, advances the tree and draws. It recovers so a
// drawing bug can never take the server down.
func (u *UI) frame() {
	defer func() {
		if r := recover(); r != nil && u.log != nil {
			u.log.Printf("panic while rendering: %v", r)
		}
	}()
	now := time.Now()
	snap := u.store.Snapshot(now)
	u.tree.Update(snap, now)

	w, h := u.scr.Size()
	c := NewCanvas(w, h, u.rend.Mono())
	u.comp.Compose(c, snap, now)
	u.rend.Draw(c)
}
