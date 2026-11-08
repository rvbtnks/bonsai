package main

import (
	"hash/fnv"
	"math"
	"strings"
	"time"
)

// The tree model. There is exactly one tree: its posture, canopy and growth are
// driven by network vitality, and every host owns a leaf pad inside it at a
// position derived from a hash of its hostname, so the layout is stable across
// restarts and across SSH sessions.
//
// Everything here is deterministic: no frame-to-frame randomness, no clock
// jitter. A given (vitality, growth, host set, canvas size) always draws the
// same picture, which keeps the display calm and makes it testable.

const treeSeed = uint64(0x5EED1E)

// Rect is a region of the canvas.
type Rect struct{ X, Y, W, H int }

func (r Rect) Right() int  { return r.X + r.W }
func (r Rect) Bottom() int { return r.Y + r.H }

// prng is a tiny deterministic xorshift generator. Seeding it from a hash of a
// hostname (or from the tree seed plus an index) is what makes the layout
// stable between frames.
type prng struct{ s uint64 }

func newPRNG(seed uint64) *prng {
	seed = mix64(seed)
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	return &prng{s: seed}
}

func (p *prng) next() uint64 {
	p.s ^= p.s << 13
	p.s ^= p.s >> 7
	p.s ^= p.s << 17
	return p.s
}

// float returns a stable value in [0,1).
func (p *prng) float() float64 { return float64(p.next()>>11) / float64(uint64(1)<<53) }

// span returns a stable value in [lo,hi).
func (p *prng) span(lo, hi float64) float64 { return lo + (hi-lo)*p.float() }

// hashName turns a hostname into a well-spread 64-bit value.
//
// The avalanche step matters: FNV-1a of "sim-01" and "sim-02" differs almost
// entirely in the low bits, and the layout uses the *high* bits to pick an
// angle. Without mix64 every host in a numbered fleet lands on the same pad.
func hashName(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return mix64(h.Sum64())
}

// mix64 is the MurmurHash3 finaliser.
func mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

func sign(v float64) float64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// Tree holds the global growth state and the smoothed vitality used for
// drawing. It belongs to the render loop; network goroutines never touch it, so
// it needs no lock.
type Tree struct {
	cfg     *Config
	Growth  float64
	display float64
	last    time.Time
}

func NewTree(cfg *Config) *Tree {
	return &Tree{cfg: cfg, Growth: cfg.SeedGrowth, display: cfg.EmptyVitality}
}

// Update advances growth and the smoothed display vitality.
//
// Growth is global: while network vitality stays above GrowthFullAbove the whole
// tree gains height, branching and leaf density; between the pause and full
// marks it holds still; below GrowthPauseBelow it dies back. With no hosts it
// settles at the seedling size.
func (t *Tree) Update(s Snapshot, now time.Time) {
	target := clamp01(s.Network)
	if t.last.IsZero() {
		t.last = now
		t.display = target
		return
	}
	dt := now.Sub(t.last).Seconds()
	if dt <= 0 {
		return
	}
	t.last = now
	if dt > 5 { // a stalled render loop must not cause one enormous jump
		dt = 5
	}

	c := t.cfg
	switch {
	case s.HostCount == 0:
		// Seedling: relax toward the starting size.
		k := 1 - math.Exp(-dt/(c.DisplayTau*4))
		t.Growth += (c.SeedGrowth - t.Growth) * k
	case target >= c.GrowthFullAbove:
		t.Growth += c.GrowthPerMinute * (dt / 60) * (0.6 + 0.4*target)
	case target < c.GrowthPauseBelow:
		t.Growth -= c.DiebackPerMinute * (dt / 60) * (1.4 - target)
	}
	t.Growth = clamp01(t.Growth)

	// Smooth the vitality so one bad packet does not twitch the tree.
	k := 1 - math.Exp(-dt/c.DisplayTau)
	t.display += (target - t.display) * k
	t.display = clamp01(t.display)
}

// Vitality is the smoothed value the drawing uses.
func (t *Tree) Vitality() float64 { return t.display }

// --- geometry ---------------------------------------------------------------

type geom struct {
	cx, cy float64 // canopy centre
	baseY  float64 // where the trunk meets the soil
	height float64 // whole tree, in rows
	trunkH float64
	rx, ry float64 // canopy radii
	padRX  float64 // leaf-pad radii
	padRY  float64
	lean   float64 // horizontal shear at the crown, in cells
	droop  float64 // 0..1
	potW   int
	potRow int
	v, g   float64 // vitality, growth
	depth  int
	area   Rect
}

// levelLen is the length of each branch order as a fraction of tree height.
var levelLen = [6]float64{0.42, 0.30, 0.20, 0.13, 0.09, 0.07}

func (t *Tree) geometry(s Snapshot, r Rect) geom {
	v, g := t.display, t.Growth
	potRows := 3

	availH := float64(r.H - potRows - 1)
	if availH < 5 {
		availH = 5
	}

	// More hosts, bigger canopy. Each machine is a pad of foliage, so the
	// crown has to grow to hold them.
	hostScale := clamp01(0.30 + 0.10*float64(s.HostCount))
	if s.HostCount == 0 {
		hostScale = 0.20
	}
	size := clampF(hostScale*(0.55+0.45*g), 0.16, 1)
	height := clampF(availH*size, 4, availH)

	trunkH := height * 0.42
	ry := height * 0.30
	rx := math.Min(height*1.05, float64(r.W-2)*0.44)

	// A pad is one host's worth of foliage. It shrinks a little as the canopy
	// gets crowded, so eight hosts still read as eight pads and not one blob.
	padRX := clampF(2.0+2.8*g, 1.6, 4.8)
	if s.HostCount > 7 {
		padRX *= math.Max(0.75, 7.0/float64(s.HostCount))
	}
	padRY := math.Max(1.1, padRX*0.52)

	gm := geom{
		cx:     float64(r.X) + float64(r.W)/2.0,
		baseY:  float64(r.Bottom() - potRows - 1),
		height: height,
		trunkH: trunkH,
		rx:     rx,
		ry:     ry,
		padRX:  padRX,
		padRY:  padRY,
		potRow: potRows,
		v:      v,
		g:      g,
		depth:  1 + int(math.Round(2*g)),
		area:   r,
	}
	gm.cy = gm.baseY - trunkH - ry*0.78
	gm.lean = (1 - v) * math.Min(float64(r.W)*0.05, 4.0)
	gm.droop = (1 - v)
	gm.depth = int(clampF(float64(gm.depth), 2, 5))

	// Pot width follows the canopy: a bonsai pot is never wider than its tree.
	potW := int(math.Round(rx * 1.15))
	if potW < 9 {
		potW = 9
	}
	maxPot := r.W - 6
	if maxPot < 9 {
		maxPot = 9
	}
	if potW > maxPot {
		potW = maxPot
	}
	if potW%2 == 0 {
		potW--
	}
	gm.potW = potW
	return gm
}

// transform applies the sick-tree posture: a downward bend that grows with
// distance from the trunk, plus a lean of the crown.
func (g geom) transform(x, y float64) (float64, float64) {
	if g.rx > 0 && g.droop > 0 {
		d := (x - g.cx) / g.rx
		y += g.droop * d * d * g.height * 0.075
	}
	if g.height > 0 && g.lean != 0 {
		up := (g.baseY - y) / g.height
		if up < 0 {
			up = 0
		}
		x += g.lean * up * up
	}
	return x, y
}

// --- skeleton ---------------------------------------------------------------

type seg struct {
	x0, y0, x1, y1 float64
	depth          int
}

type tip struct {
	x, y float64
}

// skeleton grows the branch structure. It is regenerated every frame from the
// same seed, so the shape never wanders.
func (t *Tree) skeleton(g geom) (segs []seg, tips []tip) {
	pr := newPRNG(treeSeed)
	droopAmt := g.droop * 0.85

	addSeg := func(x0, y0, x1, y1 float64, depth int) (float64, float64) {
		ax, ay := g.transform(x0, y0)
		bx, by := g.transform(x1, y1)
		segs = append(segs, seg{ax, ay, bx, by, depth})
		return bx, by
	}

	// The trunk is a gentle two-part curve rather than a ruler-straight pole.
	sway := pr.span(-0.16, 0.16)
	midX := g.cx + g.height*sway*0.35
	midY := g.baseY - g.trunkH*0.55
	topX := g.cx + g.height*sway*0.55
	topY := g.baseY - g.trunkH
	addSeg(g.cx, g.baseY, midX, midY, 0)
	addSeg(midX, midY, topX, topY, 0)
	ttx, tty := g.transform(topX, topY)
	tips = append(tips, tip{ttx, tty})

	// Main branches fan out from the upper trunk.
	n := 3
	if g.height < 7 {
		n = 2
	}
	if pr.float() < 0.3 {
		n++
	}
	baseSpread := pr.span(0.62, 0.92)

	var rec func(x, y, ang float64, depth int)
	rec = func(x, y, ang float64, depth int) {
		if depth > g.depth {
			return
		}
		length := g.height * levelLen[depth] * pr.span(0.9, 1.12)
		if length < 0.7 {
			tx, ty := g.transform(x, y)
			tips = append(tips, tip{tx, ty})
			return
		}
		x2 := x + length*math.Sin(ang)
		y2 := y - length*math.Cos(ang)
		bx, by := addSeg(x, y, x2, y2, depth)
		if depth == g.depth {
			tips = append(tips, tip{bx, by})
			return
		}
		kids := 2
		if pr.float() < 0.22 && depth < g.depth-1 {
			kids = 3
		}
		spread := baseSpread * (0.55 + 0.20*float64(depth))
		for i := 0; i < kids; i++ {
			var a float64
			switch kids {
			case 1:
				a = ang + spread*pr.span(-0.3, 0.3)
			case 2:
				if i == 0 {
					a = ang - spread*pr.span(0.70, 1.10)
				} else {
					a = ang + spread*pr.span(0.70, 1.10)
				}
			default:
				a = ang + spread*(float64(i)-1)*pr.span(0.80, 1.05)
			}
			// Droop: outer wood sags further as vitality drops.
			a += sign(a) * droopAmt * math.Min(math.Abs(a), 1.3) * (0.5 + 0.22*float64(depth))
			rec(x2, y2, a, depth+1)
		}
	}

	// Two or three leaders leave the trunk top; the rest hang off the trunk.
	for i := 0; i < n; i++ {
		f := float64(i) - float64(n-1)/2
		ang := baseSpread * f * 0.75
		ang += sign(ang+0.0001) * droopAmt * 0.5
		startY := topY
		if i > 0 && n > 2 {
			startY = topY + g.trunkH*0.22*float64(i%2)
		}
		rec(topX, startY, ang, 1)
	}
	return segs, tips
}

// --- drawing ----------------------------------------------------------------

// Draw renders the whole tree into the canvas region r.
func (t *Tree) Draw(c *Canvas, s Snapshot, r Rect) {
	if r.W < 8 || r.H < 7 {
		return // too small for a tree; the status line still tells the story
	}
	g := t.geometry(s, r)

	if s.HostCount == 0 {
		// Nothing has reported in yet: a seedling in a pot, and a message.
		t.drawPot(c, g)
		t.drawSeedling(c, g)
		return
	}

	segs, tips := t.skeleton(g)

	t.drawPot(c, g)
	t.drawWood(c, segs, g)
	t.drawDeadTwigs(c, tips, g)
	t.drawFoliage(c, tips, g)
	t.drawHosts(c, s, g)
	t.drawFallenLeaves(c, g)
}

func (t *Tree) drawPot(c *Canvas, g geom) {
	w := g.potW
	x0 := int(math.Round(g.cx)) - w/2
	y0 := int(math.Round(g.baseY)) + 1
	if y0+g.potRow > c.H {
		y0 = c.H - g.potRow
	}
	if x0 < g.area.X {
		x0 = g.area.X
	}
	col := ColPot
	if g.v < 0.35 {
		col = ColPotCracked
	}
	dash, corner := "-", "+"
	if t.cfg.Unicode {
		dash, corner = "─", "╭"
	}

	c.Text(x0, y0, corner+rep(dash, w-2)+cornerTopRight(t.cfg.Unicode), col)
	if t.cfg.Unicode {
		c.Text(x0+1, y0+1, "╲"+rep(" ", w-4)+"╱", col)
		c.Text(x0+2, y0+2, "╰"+rep(dash, w-6)+"╯", col)
	} else {
		c.Text(x0+1, y0+1, "\\"+rep(" ", w-4)+"/", col)
		c.Text(x0+2, y0+2, "`"+rep(dash, w-6)+"'", col)
	}

	// Cracks when the network is ailing.
	if g.v < 0.35 {
		c.Set(x0+1+w/4, y0, '/', ColPotCracked)
		c.Set(x0+w/2, y0+1, '/', ColPotCracked)
	}
	// Moss on the rim when things are good: a tended pot.
	if g.v > 0.72 {
		moss := ','
		if t.cfg.Unicode {
			moss = '·'
		}
		pr := newPRNG(treeSeed ^ 0xA11CE)
		for i := 0; i < 4; i++ {
			mx := x0 + 2 + int(pr.float()*float64(w-4))
			c.Set(mx, y0, moss, ColMoss)
		}
	}
	// The trunk foot meets the soil.
	c.Set(int(math.Round(g.cx)), y0, '|', ColWood)
}

func cornerTopRight(uni bool) string {
	if uni {
		return "╮"
	}
	return "+"
}

func rep(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(s)
	}
	return b.String()
}

// drawWood rasterises the branch structure. The trunk is two cells wide at the
// base and tapers to one, which is what stops it reading as a ladder.
func (t *Tree) drawWood(c *Canvas, segs []seg, g geom) {
	for _, s := range segs {
		col := ColWood
		switch {
		case s.depth >= 3:
			col = ColTwig
		case s.depth == 2:
			col = ColWood
		}
		if s.depth == 0 {
			c.Trunk(s.x0, s.y0, s.x1, s.y1, col, t.cfg.Unicode)
			continue
		}
		c.Branch(s.x0, s.y0, s.x1, s.y1, col, false, t.cfg.Unicode)
	}
}

// drawSeedling is the empty-network picture: a short stem with a couple of
// leaves, big enough to read as "alive, waiting" and nothing more.
func (t *Tree) drawSeedling(c *Canvas, g geom) {
	cx := int(math.Round(g.cx))
	baseY := int(math.Round(g.baseY))
	top := baseY - int(math.Round(math.Max(3, g.height*0.55)))
	if top < g.area.Y+1 {
		top = g.area.Y + 1
	}
	for y := baseY; y > top; y-- {
		c.Set(cx, y, '|', ColWood)
	}
	// Two small shoots and a tuft of new leaves.
	c.Set(cx-1, top+1, '\\', ColTwig)
	c.Set(cx+1, top+1, '/', ColTwig)
	leaves := []struct{ dx, dy int }{{-2, 0}, {-1, -1}, {0, -1}, {1, -1}, {2, 0}, {0, 0}, {-1, 1}, {1, 1}}
	pr := newPRNG(treeSeed ^ 0x5EED)
	for _, l := range leaves {
		col := ColLeafGood
		if pr.float() < 0.3 {
			col = ColLeafWarn
		}
		c.Set(cx+l.dx, top+l.dy, leafRune(col, pr.float(), c.Mono, t.cfg.Unicode), col)
	}
}

// drawDeadTwigs extends a few tips past the canopy when the tree is sick: bare,
// grey, obviously wanting the pruning shears.
func (t *Tree) drawDeadTwigs(c *Canvas, tips []tip, g geom) {
	if g.v > 0.55 {
		return
	}
	strength := clamp01((0.55 - g.v) / 0.55)
	for i, tp := range tips {
		pr := newPRNG(treeSeed ^ (uint64(i)+7)*0x9E3779B97F4A7C15)
		if pr.float() > strength*0.7 {
			continue
		}
		dx := pr.span(-2.4, 2.4)
		dy := pr.span(-0.4, 1.8)
		c.Branch(tp.x, tp.y, tp.x+dx, tp.y+dy, ColDead, false, t.cfg.Unicode)
		c.Set(int(math.Round(tp.x+dx)), int(math.Round(tp.y+dy)), '.', ColDead)
	}
}

// drawFoliage is the background leaf texture: it fills the canopy according to
// global growth and network vitality, which is what makes the tree read as
// "full and trim" or "sparse and dying" before you look at any single host.
func (t *Tree) drawFoliage(c *Canvas, tips []tip, g geom) {
	// Background texture only: the host pads carry the canopy, so this is
	// deliberately light. It is what makes a seedling look alive and what
	// thins out as the network sickens.
	density := clamp01(g.v * (0.15 + 0.65*g.g))
	if density <= 0.02 {
		return
	}
	for i, tp := range tips {
		pr := newPRNG(treeSeed ^ (uint64(i)+1)*0xD1B54A32D192ED03)
		if pr.float() > 0.15+0.55*density {
			continue
		}
		n := 1 + int(3*density)
		for k := 0; k < n; k++ {
			ox := pr.span(-g.padRX*0.8, g.padRX*0.8)
			oy := pr.span(-g.padRY*0.9, g.padRY*0.9)
			col := foliageColor(pr.float(), g.v)
			c.SetF(int(math.Round(tp.x+ox)), int(math.Round(tp.y+oy)),
				leafRune(col, pr.float(), c.Mono, t.cfg.Unicode), col)
		}
	}
}

// foliageColor mixes the palette by network vitality.
func foliageColor(u, v float64) ColorID {
	switch {
	case v >= 0.80:
		if u < 0.88 {
			return ColLeafGood
		}
		return ColLeafWarn
	case v >= 0.50:
		switch {
		case u < 0.52:
			return ColLeafGood
		case u < 0.92:
			return ColLeafWarn
		default:
			return ColLeafCrit
		}
	default:
		switch {
		case u < 0.14:
			return ColLeafWarn
		case u < 0.55:
			return ColLeafCrit
		default:
			return ColDead
		}
	}
}

// drawHosts places one leaf pad per host. The anchor comes from a hash of the
// hostname, so a host always occupies the same part of the canopy and the
// picture is identical after a restart.
func (t *Tree) drawHosts(c *Canvas, s Snapshot, g geom) {
	labels := make([]hostLabel, 0, 4)

	for _, h := range s.Hosts {
		hh := hashName(h.Name)
		pr := newPRNG(hh ^ treeSeed)

		u1 := float64(hh>>40) / float64(uint64(1)<<24)
		u2 := float64((hh>>8)&0xFFFFFFFF) / float64(0xFFFFFFFF)

		// Spread over the canopy ellipse, biased to the upper crown.
		ang := math.Pi * (0.06 + 0.88*u1)
		rad := math.Sqrt(0.18 + 0.82*u2)
		px := g.cx + g.rx*rad*math.Cos(ang)
		py := g.cy - g.ry*rad*math.Sin(ang)

		col := hostColor(h.Band)
		scale := clamp01((0.45 + 0.55*g.g) * (0.55 + 0.45*clamp01(h.Vitality)))

		// Keep the whole pad inside the tree area: a tall canopy on a short
		// terminal must not spill over the header or the status line.
		padRX := g.padRX*scale + 0.6
		padRY := g.padRY*scale + 0.4
		px = clampF(px, float64(g.area.X)+padRX, float64(g.area.Right())-padRX-1)
		py = clampF(py, float64(g.area.Y)+padRY, g.baseY-padRY-0.5)

		// A short shoot ties the pad to the canopy. Its side is a stable
		// function of the hostname, so it never jumps about.
		side := 1.0
		if hh&1 == 1 {
			side = -1
		}
		c.Branch(px, py+padRY*0.4, px+side*1.6, py+padRY*0.4+2.0, ColTwig, false, t.cfg.Unicode)

		if h.Stale {
			// Bare and brown: the shoot stays, the leaves are gone.
			bx, by := int(math.Round(px)), int(math.Round(py))
			c.Set(bx, by, '\'', ColDead)
			c.Set(bx+1, by, '.', ColDead)
			c.Set(bx-1, by+1, ',', ColDead)
		} else {
			t.drawPad(c, px, py, padRX, padRY, col, pr)
		}

		// Labels are collected and placed afterwards: only the worst few get
		// one, and they must not overlap each other. The full list is always
		// available in the `l` overlay.
		if h.Band == BandCrit || h.Band == BandStale {
			text := h.Name
			if h.Offender != "" {
				text += " " + h.Offender
			}
			labels = append(labels, hostLabel{text: text, x: px, y: py, vit: h.Vitality,
				col: hostLabelColor(h.Band)})
		}
	}
	t.placeLabels(c, labels, g)
}

// maxLabels bounds how many red pads get an inline name. Beyond that the tree
// would be more text than leaves; the `l` overlay has the full list.
const maxLabels = 4

// placeLabels draws the worst offenders next to their pads, nudging rows apart
// so two labels never overwrite each other.
// hostLabel is a pending inline annotation.
type hostLabel struct {
	text string
	x, y float64
	vit  float64
	col  ColorID
}

func (t *Tree) placeLabels(c *Canvas, labels []hostLabel, g geom) {
	// worst first
	for i := 1; i < len(labels); i++ {
		for j := i; j > 0 && labels[j].vit < labels[j-1].vit; j-- {
			labels[j], labels[j-1] = labels[j-1], labels[j]
		}
	}
	if len(labels) > maxLabels {
		labels = labels[:maxLabels]
	}

	type placed struct{ x0, x1, y int }
	used := make([]placed, 0, len(labels))
	overlaps := func(p placed) bool {
		for _, u := range used {
			if u.y == p.y && p.x0 <= u.x1 && u.x0 <= p.x1 {
				return true
			}
		}
		return false
	}

	for _, l := range labels {
		y := int(math.Round(l.y))
		for _, dy := range []int{0, -1, 1, -2, 2, -3, 3} {
			cand := y + dy
			if cand < g.area.Y || cand >= g.area.Bottom() {
				continue
			}
			x := int(math.Round(l.x)) + int(g.padRX) + 2
			if x+len(l.text) > g.area.Right()-1 {
				x = int(math.Round(l.x)) - int(g.padRX) - 2 - len(l.text)
			}
			if x < g.area.X {
				x = g.area.X
			}
			text := trimTo(l.text, g.area.Right()-x-1)
			if text == "" {
				break
			}
			p := placed{x0: x, x1: x + len(text) - 1, y: cand}
			if overlaps(p) {
				continue
			}
			used = append(used, p)
			for i := 0; i < len(text); i++ {
				c.SetBlank(x+i, cand) // clear a window so it stays readable
			}
			c.Text(x, cand, text, l.col)
			break
		}
	}
}

// drawPad fills a flattened ellipse of leaves: one canopy pad per host. The
// edge is thinned so pads look soft rather than stamped.
func (t *Tree) drawPad(c *Canvas, cx, cy, rx, ry float64, col ColorID, pr *prng) {
	if rx < 0.6 {
		rx = 0.6
	}
	if ry < 0.6 {
		ry = 0.6
	}
	maxX := int(math.Ceil(rx))
	maxY := int(math.Ceil(ry))
	for dy := -maxY; dy <= maxY; dy++ {
		for dx := -maxX; dx <= maxX; dx++ {
			e := (float64(dx)/rx)*(float64(dx)/rx) + (float64(dy)/ry)*(float64(dy)/ry)
			if e > 1.0 {
				continue
			}
			if e > 0.70 && pr.float() > 0.78 {
				continue // soft edge
			}
			lc := col
			if pr.float() < 0.20 {
				lc = mixDown(col) // a little variety inside the pad
			}
			c.Set(int(math.Round(cx))+dx, int(math.Round(cy))+dy,
				leafRune(lc, pr.float(), c.Mono, t.cfg.Unicode), lc)
		}
	}
}

// drawFallenLeaves scatters litter around the pot as the network declines.
func (t *Tree) drawFallenLeaves(c *Canvas, g geom) {
	// Litter only once the network is genuinely ailing; a neutral 0.5 with no
	// hosts is a seedling, not a sick tree.
	if g.v > 0.45 {
		return
	}
	n := int((0.45 - g.v) * 26)
	if n <= 0 {
		return
	}
	if n > 12 {
		n = 12
	}
	// Seed by vitality bucket so the litter does not shimmer every frame.
	pr := newPRNG(treeSeed ^ (uint64(g.v*8) * 0xC2B2AE3D27D4EB4F))
	potTop := int(math.Round(g.baseY)) + 1
	half := float64(g.potW) / 2
	for i := 0; i < n; i++ {
		side := -1.0
		if pr.float() < 0.5 {
			side = 1.0
		}
		dx := side * pr.span(half+1, half+5)
		dy := pr.span(-0.4, 2.6)
		col := foliageColor(pr.float(), g.v*0.75)
		c.Set(int(math.Round(g.cx+dx)), potTop+int(math.Round(dy)),
			leafRune(col, pr.float(), c.Mono, false), col)
	}
}

func hostColor(b Band) ColorID {
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
	return ColLeafGood
}

func hostLabelColor(b Band) ColorID {
	if b == BandStale {
		return ColDead
	}
	return ColCrit
}

// mixDown adds a little variety inside a pad. It only ever steps green to
// yellow: red is reserved for prune targets, so a healthy host must never
// grow red leaves by accident.
func mixDown(c ColorID) ColorID {
	if c == ColLeafGood {
		return ColLeafWarn
	}
	return c
}

// leafRune picks a glyph for a leaf. In monochrome the glyph carries the state
// instead of the colour, so the picture still reads on a terminal without
// colour support.
func leafRune(col ColorID, u float64, mono, unicode bool) rune {
	if mono {
		switch col {
		case ColLeafGood:
			return 'o'
		case ColLeafWarn:
			return '*'
		case ColLeafCrit:
			return 'X'
		case ColDead:
			return '.'
		}
		return 'o'
	}
	if unicode {
		switch col {
		case ColLeafGood:
			if u < 0.45 {
				return '●'
			}
			if u < 0.85 {
				return '❍'
			}
			return '·'
		case ColLeafWarn:
			if u < 0.5 {
				return '◉'
			}
			return '•'
		case ColLeafCrit:
			if u < 0.5 {
				return '✖'
			}
			return '✗'
		case ColDead:
			if u < 0.5 {
				return '·'
			}
			return '∙'
		}
		return '●'
	}
	switch col {
	case ColLeafGood:
		switch {
		case u < 0.30:
			return '@'
		case u < 0.60:
			return '#'
		case u < 0.85:
			return '*'
		default:
			return 'o'
		}
	case ColLeafWarn:
		switch {
		case u < 0.40:
			return '%'
		case u < 0.75:
			return '*'
		default:
			return 'o'
		}
	case ColLeafCrit:
		switch {
		case u < 0.45:
			return '#'
		case u < 0.80:
			return 'X'
		default:
			return '*'
		}
	case ColDead:
		if u < 0.5 {
			return ','
		}
		return '.'
	}
	return '*'
}

// woodRune picks a branch glyph from the segment direction.
func woodRune(dx, dy float64, unicode bool) rune {
	ax, ay := math.Abs(dx), math.Abs(dy)
	if unicode {
		switch {
		case ax < 0.45*ay:
			return '│'
		case ay < 0.45*ax:
			return '─'
		case dx > 0 && dy < 0, dx < 0 && dy > 0:
			return '╱'
		default:
			return '╲'
		}
	}
	switch {
	case ax < 0.45*ay:
		return '|'
	case ay < 0.45*ax:
		return '-'
	case dx > 0 && dy < 0, dx < 0 && dy > 0:
		return '/'
	default:
		return '\\'
	}
}
