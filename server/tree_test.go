package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func testSnapshot(vitalities map[string]float64) Snapshot {
	cfg := DefaultConfig()
	now := time.Now()
	s := Snapshot{Taken: now}
	names := []string{}
	for n := range vitalities {
		names = append(names, n)
	}
	// stable order
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	vit := make([]float64, 0, len(names))
	for _, n := range names {
		v := vitalities[n]
		band := cfg.BandOf(v)
		if v <= 0 {
			band = BandStale
		}
		s.Hosts = append(s.Hosts, HostHealth{Name: n, Vitality: v, Band: band, Stale: v <= 0,
			Metrics: Report{Host: n}, LastSeen: now})
		vit = append(vit, v)
		switch band {
		case BandGood:
			s.Good++
		case BandWarn:
			s.Warn++
		case BandCrit:
			s.Crit++
		case BandStale:
			s.Stale++
		}
	}
	s.HostCount = len(s.Hosts)
	s.Network = NetworkVitality(&cfg, vit)
	s.Word = cfg.Word(s.Network)
	return s
}

// renderCanvas composes one frame and returns the canvas, so tests can inspect
// colours as well as glyphs.
func renderCanvas(t *testing.T, w, h int, growth, vitality float64, hosts map[string]float64, unicode, mono bool) *Canvas {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Unicode = unicode
	tree := NewTree(&cfg)
	tree.Growth = growth
	tree.display = vitality
	tree.last = time.Now()
	snap := testSnapshot(hosts)
	c := NewCanvas(w, h, mono)
	co := NewComposer(&cfg, tree, NewNode(&cfg, NewStore(&cfg), nil), ":1717")
	co.Compose(c, snap, time.Now())
	return c
}

func renderFrame(t *testing.T, w, h int, growth, vitality float64, hosts map[string]float64, unicode bool) string {
	t.Helper()
	return renderCanvas(t, w, h, growth, vitality, hosts, unicode, false).String()
}

// countColor counts cells painted with a given logical colour.
func countColor(c *Canvas, want ColorID) int {
	n := 0
	for _, cell := range c.Cells {
		if cell.C == want {
			n++
		}
	}
	return n
}

// TestPadAnchorsSpread checks the host clusters actually land in different
// places: a broken hash-to-position mapping collapses every host into one spot.
func TestPadAnchorsSpread(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	tree.Growth = 1
	tree.display = 1
	snap := testSnapshot(map[string]float64{
		"nas01": 1, "pihole": 1, "backup": 1, "dev01": 1,
		"printer": 1, "kodi": 1, "gw": 1, "sim-08": 1,
	})
	g := tree.geometry(snap, Rect{X: 0, Y: 1, W: 96, H: 27})

	type pt struct{ x, y int }
	seen := map[pt]string{}
	for _, h := range snap.Hosts {
		hh := hashName(h.Name)
		u1 := float64(hh>>40) / float64(uint64(1)<<24)
		u2 := float64((hh>>8)&0xFFFFFFFF) / float64(0xFFFFFFFF)
		if u1 < 0 || u1 >= 1 || u2 < 0 || u2 >= 1 {
			t.Fatalf("host %s: u1/u2 out of range (%v %v)", h.Name, u1, u2)
		}
		ang := math.Pi * (0.06 + 0.88*u1)
		rad := math.Sqrt(0.18 + 0.82*u2)
		px := g.cx + g.rx*rad*math.Cos(ang)
		py := g.cy - g.ry*rad*math.Sin(ang)
		p := pt{int(math.Round(px)), int(math.Round(py))}
		if other, dup := seen[p]; dup {
			t.Errorf("hosts %s and %s share pad anchor %v", other, h.Name, p)
		}
		seen[p] = h.Name
		if px < g.cx-g.rx-6 || px > g.cx+g.rx+6 {
			t.Errorf("host %s pad x=%.1f outside canopy [%.1f,%.1f]", h.Name, px, g.cx-g.rx, g.cx+g.rx)
		}
		if py < float64(g.area.Y) || py > g.baseY {
			t.Errorf("host %s pad y=%.1f outside tree area [%d,%.1f]", h.Name, py, g.area.Y, g.baseY)
		}
		t.Logf("%-8s anchor=(%.1f,%.1f) cell=(%d,%d) ang=%.2f rad=%.2f",
			h.Name, px, py, p.x, p.y, ang, rad)
	}
	if len(seen) != len(snap.Hosts) {
		t.Errorf("only %d distinct anchors for %d hosts", len(seen), len(snap.Hosts))
	}
	t.Logf("canopy centre=(%.1f,%.1f) rx=%.1f ry=%.1f pad=%.1fx%.1f baseY=%.1f area=%+v",
		g.cx, g.cy, g.rx, g.ry, g.padRX, g.padRY, g.baseY, g.area)
}

// TestDrawIsStable is the determinism guarantee: the same inputs must produce
// byte-identical frames, so the display never flickers between redraws.
func TestDrawIsStable(t *testing.T) {
	hosts := map[string]float64{"nas01": 1, "pihole": 0.9, "backup": 0.4, "dev01": 0.05}
	a := renderFrame(t, 90, 26, 0.7, 0.72, hosts, false)
	b := renderFrame(t, 90, 26, 0.7, 0.72, hosts, false)
	if a != b {
		t.Errorf("rendering is not deterministic:\n%s\n---\n%s", a, b)
	}
}

// TestSeedlingWithNoHosts covers acceptance criterion 1: an empty server shows
// a small seedling and the waiting message.
func TestSeedlingWithNoHosts(t *testing.T) {
	out := renderFrame(t, 80, 24, 0.08, 0.5, nil, false)
	if !strings.Contains(out, "waiting for clients") {
		t.Errorf("expected the waiting message, got:\n%s", out)
	}
	if strings.Contains(out, "hosts 8") {
		t.Errorf("unexpected host count in:\n%s", out)
	}
	// A seedling must be small: most of the canvas is empty.
	filled := 0
	for _, r := range strings.ReplaceAll(out, "\n", "") {
		if r != ' ' {
			filled++
		}
	}
	if filled > 80*24/3 {
		t.Errorf("seedling fills too much of the canvas (%d cells):\n%s", filled, out)
	}
	t.Logf("\n%s", out)
}

// TestSickTreeDiffersFromHealthyTree is the at-a-glance contract: a dying
// network must not look like a thriving one.
func TestSickTreeDiffersFromHealthyTree(t *testing.T) {
	healthy := map[string]float64{"a1": 1, "a2": 1, "a3": 1, "a4": 1, "a5": 1, "a6": 1}
	sick := map[string]float64{"a1": 0.1, "a2": 0.05, "a3": 0, "a4": 0.2, "a5": 0, "a6": 0.1}

	hc := renderCanvas(t, 90, 26, 1.0, 0.95, healthy, false, false)
	sc := renderCanvas(t, 90, 26, 1.0, 0.15, sick, false, false)
	h, s := hc.String(), sc.String()
	if h == s {
		t.Fatal("healthy and sick trees rendered identically")
	}

	hRed, sRed := countColor(hc, ColLeafCrit), countColor(sc, ColLeafCrit)
	hGreen, sGreen := countColor(hc, ColLeafGood), countColor(sc, ColLeafGood)
	hDead, sDead := countColor(hc, ColDead), countColor(sc, ColDead)

	if sRed <= hRed {
		t.Errorf("sick tree should show more red: sick=%d healthy=%d", sRed, hRed)
	}
	if sGreen >= hGreen {
		t.Errorf("sick tree should show less green: sick=%d healthy=%d", sGreen, hGreen)
	}
	if sDead <= hDead {
		t.Errorf("sick tree should show more dead wood and litter: sick=%d healthy=%d", sDead, hDead)
	}
	if !strings.Contains(s, "ailing") {
		t.Errorf("a network at 0.15 should read as ailing:\n%s", s)
	}
	if !strings.Contains(h, "thriving") {
		t.Errorf("a network at 0.95 should read as thriving:\n%s", h)
	}
	// A dying tree loses foliage: fewer painted cells overall.
	painted := func(frame string) int {
		n := 0
		for _, r := range frame {
			if r != ' ' && r != '\n' {
				n++
			}
		}
		return n
	}
	if painted(s) >= painted(h) {
		t.Errorf("the sick canopy should be sparser: sick=%d healthy=%d", painted(s), painted(h))
	}
	t.Logf("healthy: green=%d red=%d dead=%d painted=%d", hGreen, hRed, hDead, painted(h))
	t.Logf("sick:    green=%d red=%d dead=%d painted=%d", sGreen, sRed, sDead, painted(s))
}

// TestRedClusterAppearsForOneBadHost is acceptance criterion 6: degrading a
// single host must dull the tree slightly and show one red cluster.
func TestRedClusterAppearsForOneBadHost(t *testing.T) {
	all := map[string]float64{"a1": 1, "a2": 1, "a3": 1, "a4": 1, "a5": 1, "a6": 1, "a7": 1, "a8": 1}
	one := map[string]float64{"a1": 1, "a2": 1, "a3": 1, "a4": 1, "a5": 1, "a6": 1, "a7": 1, "a8": 0.02}

	cfg := DefaultConfig()
	good := NetworkVitality(&cfg, []float64{1, 1, 1, 1, 1, 1, 1, 1})
	bad := NetworkVitality(&cfg, []float64{1, 1, 1, 1, 1, 1, 1, 0.02})
	if bad >= good {
		t.Fatalf("one dying host should dull the network: %v -> %v", good, bad)
	}
	// "Slightly" means: out of the thriving band, but nowhere near dead. With
	// the spec's 0.6*mean + 0.4*min, one host at zero among eight lands just
	// above 0.5 - the canopy thins and growth pauses, the tree does not fall
	// apart. Losing several hosts is what wrecks it.
	if cfg.Word(bad) == "thriving" {
		t.Errorf("one dying host should knock the tree out of thriving: %v", bad)
	}
	if bad < 0.5 {
		t.Errorf("one dying host out of eight should not wreck the tree: %v", bad)
	}
	several := NetworkVitality(&cfg, []float64{1, 0.1, 0.05, 0, 1, 0.2, 0, 1})
	if several >= bad {
		t.Errorf("several dying hosts must be worse than one: %v vs %v", several, bad)
	}
	if w := cfg.Word(several); w != "ailing" && w != "stressed" {
		t.Errorf("several dying hosts should read as stressed or ailing, got %q (%v)", w, several)
	}

	ac := renderCanvas(t, 90, 26, 1.0, good, all, false, false)
	bc := renderCanvas(t, 90, 26, 1.0, bad, one, false, false)
	if countColor(ac, ColLeafCrit) != 0 {
		t.Errorf("an all-healthy network should have no red leaves, got %d", countColor(ac, ColLeafCrit))
	}
	if countColor(bc, ColLeafCrit) == 0 {
		t.Errorf("the failing host should show a red cluster:\n%s", bc.String())
	}
	if !strings.Contains(bc.String(), "a8") {
		t.Errorf("the red cluster should be labelled with its host:\n%s", bc.String())
	}
}

// TestStaleHostIsBrownAndBare checks the "gone quiet" rendering.
func TestStaleHostIsBrownAndBare(t *testing.T) {
	hosts := map[string]float64{"alive1": 1, "alive2": 1, "gone": 0}
	c := renderCanvas(t, 90, 26, 1.0, 0.7, hosts, false, false)
	if countColor(c, ColDead) == 0 {
		t.Errorf("a stale host should render brown and bare:\n%s", c.String())
	}
	if !strings.Contains(c.String(), "gone") {
		t.Errorf("a stale host should be labelled:\n%s", c.String())
	}
}

// TestMonochromeKeepsTheInformation covers the no-colour terminal fallback.
func TestMonochromeKeepsTheInformation(t *testing.T) {
	hosts := map[string]float64{"a1": 1, "a2": 0.5, "a3": 0.05}
	c := renderCanvas(t, 90, 26, 1.0, 0.6, hosts, false, true)
	frame := c.String()
	for _, r := range []rune{'o', '*', 'X'} {
		if !strings.ContainsRune(frame, r) {
			t.Errorf("monochrome frame lost the %q glyph:\n%s", r, frame)
		}
	}
	// The canvas keeps logical colours; the renderer is what collapses them
	// on a monochrome terminal (see TestRendererMonochromeFallback).
}

// TestGrowthChangesTheTree is the "growth is global" contract.
func TestGrowthChangesTheTree(t *testing.T) {
	hosts := map[string]float64{"a1": 1, "a2": 1, "a3": 1, "a4": 1}
	small := renderCanvas(t, 90, 28, 0.05, 0.95, hosts, false, false)
	big := renderCanvas(t, 90, 28, 1.0, 0.95, hosts, false, false)
	if small.String() == big.String() {
		t.Fatal("growth did not change the tree")
	}
	// Measure the tree only: rows between the header and the status line.
	heightOf := func(c *Canvas) int {
		top, bottom := c.H, 0
		for y := 1; y < c.H-2; y++ {
			for x := 0; x < c.W; x++ {
				if c.Cells[y*c.W+x].R != ' ' {
					if y < top {
						top = y
					}
					if y > bottom {
						bottom = y
					}
				}
			}
		}
		return bottom - top
	}
	if heightOf(big) <= heightOf(small) {
		t.Errorf("a grown tree should be taller: %d vs %d", heightOf(big), heightOf(small))
	}
	paintedIn := func(c *Canvas) int {
		n := 0
		for y := 1; y < c.H-2; y++ {
			for x := 0; x < c.W; x++ {
				if c.Cells[y*c.W+x].R != ' ' {
					n++
				}
			}
		}
		return n
	}
	if paintedIn(big) <= paintedIn(small) {
		t.Errorf("a grown tree should carry more foliage: %d vs %d", paintedIn(big), paintedIn(small))
	}
}

// TestMoreHostsWidenTheCanopy is the "each new host adds a shoot" contract.
func TestMoreHostsWidenTheCanopy(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	tree.Growth, tree.display = 1, 1
	few := tree.geometry(testSnapshot(map[string]float64{"a": 1}), Rect{X: 0, Y: 1, W: 96, H: 27})
	many := tree.geometry(testSnapshot(map[string]float64{
		"a": 1, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1, "g": 1, "h": 1}), Rect{X: 0, Y: 1, W: 96, H: 27})
	if many.rx <= few.rx || many.height <= few.height {
		t.Errorf("more hosts should mean a bigger canopy: rx %v->%v height %v->%v",
			few.rx, many.rx, few.height, many.height)
	}
	seedling := tree.geometry(testSnapshot(nil), Rect{X: 0, Y: 1, W: 96, H: 27})
	if seedling.height >= few.height {
		t.Errorf("a seedling should be the smallest: %v vs %v", seedling.height, few.height)
	}
}

// TestDroopAndLeanWhenSick checks the posture change.
func TestDroopAndLeanWhenSick(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	tree.Growth = 1
	hosts := map[string]float64{"a1": 1, "a2": 1, "a3": 1, "a4": 1}
	snap := testSnapshot(hosts)
	area := Rect{X: 0, Y: 1, W: 96, H: 27}

	tree.display = 1
	upright := tree.geometry(snap, area)
	tree.display = 0.1
	sick := tree.geometry(snap, area)

	if sick.droop <= upright.droop {
		t.Errorf("a sick tree should droop more: %v vs %v", sick.droop, upright.droop)
	}
	if sick.lean <= upright.lean {
		t.Errorf("a sick tree should lean: %v vs %v", sick.lean, upright.lean)
	}
	if sick.depth > upright.depth {
		t.Errorf("a sick tree should not gain branching: %d vs %d", sick.depth, upright.depth)
	}
}

// TestUnicodeAndAsciiBothRender covers the -unicode switch.
func TestUnicodeAndAsciiBothRender(t *testing.T) {
	hosts := map[string]float64{"a1": 1, "a2": 0.5, "a3": 0.05}
	ascii := renderFrame(t, 90, 26, 1.0, 0.8, hosts, false)
	uni := renderFrame(t, 90, 26, 1.0, 0.8, hosts, true)
	if ascii == uni {
		t.Error("-unicode produced an identical frame")
	}
	for _, r := range ascii {
		if r > 127 {
			t.Fatalf("the default renderer emitted a non-ASCII rune %q", r)
		}
	}
}

// TestHostPadLayoutIsStable: the same hostname always lands on the same pad,
// which is what makes the picture recognisable across restarts.
func TestHostPadLayoutIsStable(t *testing.T) {
	hosts := map[string]float64{"nas01": 1, "pihole": 1}
	a := renderFrame(t, 90, 26, 0.5, 0.9, hosts, false)
	b := renderFrame(t, 90, 26, 0.5, 0.9, hosts, false)
	if a != b {
		t.Error("layout changed between identical runs")
	}
	// A fleet of near-identical names must still spread out: FNV alone leaves
	// the high bits untouched, and the layout uses those for the angle.
	fleet := map[string]float64{}
	for i := 1; i <= 8; i++ {
		fleet[fmt.Sprintf("node-%02d", i)] = 1
	}
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	tree.Growth, tree.display = 1, 1
	g := tree.geometry(testSnapshot(fleet), Rect{X: 0, Y: 1, W: 96, H: 27})
	cells := map[[2]int]string{}
	for name := range fleet {
		hh := hashName(name)
		u1 := float64(hh>>40) / float64(float64(uint64(1)<<24))
		u2 := float64((hh>>8)&0xFFFFFFFF) / float64(0xFFFFFFFF)
		ang := math.Pi * (0.06 + 0.88*u1)
		rad := math.Sqrt(0.18 + 0.82*u2)
		key := [2]int{int(math.Round(g.cx + g.rx*rad*math.Cos(ang))), int(math.Round(g.cy - g.ry*rad*math.Sin(ang)))}
		if other, dup := cells[key]; dup {
			t.Errorf("node %s shares a pad with %s at %v", name, other, key)
		}
		cells[key] = name
	}
}

// TestTinyTerminalMustNotPanic covers acceptance criterion 9's worst case.
func TestTinyTerminalMustNotPanic(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {2, 3}, {10, 4}, {19, 7}, {30, 10}, {200, 60}, {7, 40}} {
		hosts := map[string]float64{"nas01": 1, "pihole": 0.5, "dev01": 0.1}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic at %dx%d: %v", size[0], size[1], r)
				}
			}()
			_ = renderCanvas(t, size[0], size[1], 0.9, 0.9, hosts, false, false)
			_ = renderCanvas(t, size[0], size[1], 0.9, 0.9, hosts, true, true)
		}()
	}
}

func TestGrowthModel(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	now := time.Now()
	tree.Update(Snapshot{Network: 1, HostCount: 3}, now)
	start := tree.Growth

	// Thriving: growth climbs.
	for i := 0; i < 120; i++ {
		now = now.Add(time.Second)
		tree.Update(Snapshot{Network: 0.95, HostCount: 3}, now)
	}
	if tree.Growth <= start {
		t.Errorf("growth did not increase while thriving: %v -> %v", start, tree.Growth)
	}
	grown := tree.Growth

	// Between the pause and full marks: growth holds.
	for i := 0; i < 60; i++ {
		now = now.Add(time.Second)
		tree.Update(Snapshot{Network: 0.65, HostCount: 3}, now)
	}
	if math.Abs(tree.Growth-grown) > 1e-9 {
		t.Errorf("growth should pause at 0.65 vitality: %v -> %v", grown, tree.Growth)
	}

	// Ailing: dieback.
	for i := 0; i < 300; i++ {
		now = now.Add(time.Second)
		tree.Update(Snapshot{Network: 0.1, HostCount: 3}, now)
	}
	if tree.Growth >= grown {
		t.Errorf("expected dieback: %v -> %v", grown, tree.Growth)
	}

	// No hosts at all: back to a seedling, never below zero.
	for i := 0; i < 600; i++ {
		now = now.Add(time.Second)
		tree.Update(Snapshot{Network: cfg.EmptyVitality, HostCount: 0}, now)
	}
	if tree.Growth < 0 || tree.Growth > 1 {
		t.Errorf("growth out of range: %v", tree.Growth)
	}
	if math.Abs(tree.Growth-cfg.SeedGrowth) > 0.01 {
		t.Errorf("expected the seedling size %v, got %v", cfg.SeedGrowth, tree.Growth)
	}
}

func TestGrowthNeverExceedsOne(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	now := time.Now()
	tree.Update(Snapshot{Network: 1, HostCount: 5}, now)
	for i := 0; i < 100000; i++ {
		now = now.Add(time.Second)
		tree.Update(Snapshot{Network: 1, HostCount: 5}, now)
	}
	if tree.Growth > 1 {
		t.Errorf("growth exceeded its cap: %v", tree.Growth)
	}
}

func TestVitalitySmoothingDoesNotOvershoot(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	now := time.Now()
	tree.Update(Snapshot{Network: 1, HostCount: 2}, now)
	for i := 0; i < 300; i++ {
		now = now.Add(time.Second)
		tree.Update(Snapshot{Network: 0, HostCount: 2}, now)
		if tree.Vitality() < 0 || tree.Vitality() > 1 {
			t.Fatalf("display vitality out of range: %v", tree.Vitality())
		}
	}
	if tree.Vitality() > 0.01 {
		t.Errorf("display vitality did not settle at 0: %v", tree.Vitality())
	}
}
