package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// The rendering path is exercised through tcell's simulation screen, so the
// whole UI loop - keys, resize, redraw, shutdown - is covered without a TTY.

func simScreen(t *testing.T, w, h int) tcell.SimulationScreen {
	t.Helper()
	sim := tcell.NewSimulationScreen("")
	sim.SetSize(w, h)
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen: %v", err)
	}
	return sim
}

// screenText reads the visible characters back out of a screen.
func screenText(scr tcell.Screen) string {
	w, h := scr.Size()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			mainc, _, _, _ := scr.GetContent(x, y)
			if mainc == 0 {
				mainc = ' '
			}
			b.WriteRune(mainc)
		}
		if y < h-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func testNode(t *testing.T) (*Node, *Store, *Tree, *Config) {
	t.Helper()
	cfg := DefaultConfig()
	store := NewStore(&cfg)
	node := NewNode(&cfg, store, log.New(io.Discard, "", 0))
	tree := NewTree(&cfg)
	return node, store, tree, &cfg
}

func TestRendererBlitsTheCanvas(t *testing.T) {
	sim := simScreen(t, 40, 10)
	defer sim.Fini()
	r := NewRenderer(sim)

	c := NewCanvas(40, 10, r.Mono())
	c.Text(2, 3, "hello bonsai", ColAccent)
	c.Set(0, 0, '@', ColLeafGood)
	r.Draw(c)

	got := screenText(sim)
	if !strings.Contains(got, "hello bonsai") {
		t.Errorf("text was not blitted:\n%s", got)
	}
	mainc, _, _, _ := sim.GetContent(0, 0)
	if mainc != '@' {
		t.Errorf("cell (0,0) = %q, want @", mainc)
	}
}

func TestRendererMapsEveryColourToAStyle(t *testing.T) {
	sim := simScreen(t, 20, 5)
	defer sim.Fini()
	r := NewRenderer(sim)
	for id := ColNone; id <= ColCrit; id++ {
		if _, ok := r.styles[id]; !ok {
			t.Errorf("colour %d has no style; it would render as the zero value", id)
		}
	}
}

func TestRunUIQuitsOnQ(t *testing.T) {
	sim := simScreen(t, 100, 30)
	defer sim.Fini()
	node, store, tree, cfg := testNode(t)

	store.Record(Report{Host: "nas01", CPU: F(10), Temp: F(50), Load: F(0.2), Up: F(9000)}, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunUI(ctx, cancel, sim, cfg, store, tree, node, ":1717", log.New(io.Discard, "", 0))
	}()

	// Let the first frame land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(screenText(sim), "bonsai-monitor") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := screenText(sim); !strings.Contains(got, "bonsai-monitor") {
		t.Fatalf("the header never appeared:\n%s", got)
	} else if !strings.Contains(got, "nas01") && !strings.Contains(got, "hosts 1") {
		t.Errorf("the host was not reflected in the display:\n%s", got)
	}

	sim.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("RunUI returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("q did not quit the UI")
	}
}

func TestRunUIOverlays(t *testing.T) {
	sim := simScreen(t, 100, 30)
	defer sim.Fini()
	node, store, tree, cfg := testNode(t)
	now := time.Now()
	store.Record(Report{Host: "nas01", CPU: F(10), Temp: F(50), Load: F(0.2), Up: F(9000)}, now)
	store.Record(Report{Host: "hot01", CPU: F(10), Temp: F(90), Load: F(0.2), Up: F(9000)}, now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunUI(ctx, cancel, sim, cfg, store, tree, node, ":1717", log.New(io.Discard, "", 0))
	}()
	time.Sleep(150 * time.Millisecond)

	sim.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
	if !waitForText(sim, "host", "cpu") {
		t.Errorf("the host overlay did not open:\n%s", screenText(sim))
	}
	if got := screenText(sim); !strings.Contains(got, "hot01") || !strings.Contains(got, "nas01") {
		t.Errorf("the overlay is missing hosts:\n%s", got)
	}
	// The offending metric must be visible in the overlay.
	if got := screenText(sim); !strings.Contains(got, "temp 90C") {
		t.Errorf("the overlay should name the offending metric:\n%s", got)
	}

	sim.InjectKey(tcell.KeyRune, 'l', tcell.ModNone) // toggle shut
	time.Sleep(100 * time.Millisecond)
	sim.InjectKey(tcell.KeyRune, '?', tcell.ModNone)
	if !waitForText(sim, "quit") {
		t.Errorf("the help overlay did not open:\n%s", screenText(sim))
	}
	sim.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Escape did not quit the UI")
	}
}

func waitForText(sim tcell.SimulationScreen, want ...string) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := screenText(sim)
		all := true
		for _, w := range want {
			if !strings.Contains(got, w) {
				all = false
				break
			}
		}
		if all {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestRunUISurvivesResize(t *testing.T) {
	sim := simScreen(t, 100, 30)
	defer sim.Fini()
	node, store, tree, cfg := testNode(t)
	cfg.RenderInterval = 0.05
	cfg.normalize()
	store.Record(Report{Host: "nas01", CPU: F(10)}, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunUI(ctx, cancel, sim, cfg, store, tree, node, ":1717", log.New(io.Discard, "", 0))
	}()

	// tcell's simulation screen has no resize injection, but the render loop
	// re-reads Size() every tick, so resizing and waiting a beat exercises the
	// same code path a real SIGWINCH does.
	sizes := [][2]int{{20, 8}, {40, 12}, {7, 5}, {200, 60}, {1, 1}, {120, 40}, {52, 17}}
	for _, s := range sizes {
		sim.SetSize(s[0], s[1])
		time.Sleep(90 * time.Millisecond)
		if got := screenText(sim); len(got) > 0 && strings.Contains(got, "\x00") {
			t.Fatalf("garbage on screen at %dx%d", s[0], s[1])
		}
	}
	// Still alive and still drawing?
	sim.InjectKey(tcell.KeyCtrlL, 0, tcell.ModNone)
	time.Sleep(150 * time.Millisecond)
	sim.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("the UI wedged after a resize storm")
	}
	cancel()
}

func TestWaitingMessageWithNoClients(t *testing.T) {
	sim := simScreen(t, 90, 26)
	defer sim.Fini()
	node, store, tree, cfg := testNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = RunUI(ctx, cancel, sim, cfg, store, tree, node, "0.0.0.0:1717", log.New(io.Discard, "", 0))
	}()
	if !waitForText(sim, "listening on :1717", "waiting for clients") {
		t.Errorf("missing the waiting message:\n%s", screenText(sim))
	}
}

func TestCanvasClipping(t *testing.T) {
	c := NewCanvas(20, 10, false)
	c.SetClip(Rect{X: 5, Y: 2, W: 10, H: 5})
	c.Set(0, 0, 'X', ColLeafCrit)                 // outside, above
	c.Set(19, 9, 'X', ColLeafCrit)                // outside, below
	c.Text(0, 4, "0123456789abcdefghij", ColText) // clipped to x=5..14
	c.Set(10, 4, 'O', ColLeafGood)                // inside the clip
	c.ClearClip()
	c.Set(0, 0, 'Y', ColText) // allowed again

	if got, _, _, _ := c.Get(0, 0); got != 'Y' {
		t.Errorf("clip was not cleared: %q", got)
	}
	if got, _, _, _ := c.Get(10, 4); got != 'O' {
		t.Errorf("drawing inside the clip failed: %q", got)
	}
	for x := 0; x < 5; x++ {
		if got, _, _, _ := c.Get(x, 4); got != ' ' {
			t.Errorf("clip leaked at x=%d: %q", x, got)
		}
	}
}

func TestCanvasStringAndBounds(t *testing.T) {
	c := NewCanvas(6, 3, false)
	c.Text(0, 0, "abcdef", ColText)
	c.Text(-2, 1, "uvwxyz", ColText)
	c.Set(100, 100, 'X', ColText) // ignored, no panic
	if got := c.String(); !strings.HasPrefix(got, "abcdef") {
		t.Errorf("unexpected canvas:\n%q", got)
	}
	if !strings.Contains(c.String(), "yz") {
		t.Errorf("negative x should clip, not vanish:\n%q", c.String())
	}
	if len(c.String()) > 6*3+2 {
		t.Errorf("canvas string too long: %d", len(c.String()))
	}
}

func TestStatusLineNeverOverflows(t *testing.T) {
	cfg := DefaultConfig()
	tree := NewTree(&cfg)
	tree.display = 0.87
	tree.Growth = 1
	node := NewNode(&cfg, NewStore(&cfg), log.New(io.Discard, "", 0))
	co := NewComposer(&cfg, tree, node, "0.0.0.0:1717")

	for _, w := range []int{20, 30, 46, 60, 80, 100, 140, 200} {
		snap := testSnapshot(map[string]float64{
			"a-very-long-hostname-01": 1, "b": 0.5, "c": 0.1, "d": 0, "e": 0,
		})
		c := NewCanvas(w, 12, false)
		co.Compose(c, snap, time.Now())
		line := c.String()
		for _, row := range strings.Split(line, "\n") {
			if len([]rune(row)) > w {
				t.Fatalf("row overflows a %d-wide canvas: %d runes: %q", w, len([]rune(row)), row)
			}
		}
		if !strings.Contains(line, "network") {
			t.Errorf("width %d lost the status line:\n%s", w, line)
		}
	}
}
