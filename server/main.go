package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
)

// Version is stamped by the Makefile: -ldflags "-X main.Version=...".
var Version = "dev"

const progName = "bonsai-server"

func main() {
	var (
		portFlag    = flag.Int("port", 0, "listen port (default 1717)")
		bindFlag    = flag.String("bind", "", "address to bind (default 0.0.0.0)")
		configFlag  = flag.String("config", "", "path to a JSON config overriding any default")
		unicodeFlag = flag.Bool("unicode", false, "use richer glyphs instead of plain ASCII")
		logFlag     = flag.String("log", "", "log file (default ~/.bonsai-server.log; \"none\" disables)")
		dumpFlag    = flag.Int("dump", 0, "headless: print N one-second frames as ASCII and exit")
		dumpEvery   = flag.Duration("dump-interval", time.Second, "spacing between -dump frames")
		dumpW       = flag.Int("dump-width", 100, "canvas width for -dump")
		dumpH       = flag.Int("dump-height", 32, "canvas height for -dump")
		dumpOverlay = flag.String("dump-overlay", "", "-dump overlay: hosts, help or empty")
		dumpGrowth  = flag.Float64("dump-growth", -1, "-dump: start with this growth (0..1) to preview sizes")
		verFlag     = flag.Bool("version", false, "print the version and exit")
		showCfg     = flag.Bool("show-config", false, "print the effective config as JSON and exit")
	)
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "bonsai-monitor server %s\n\n", Version)
		fmt.Fprintf(out, "usage: %s [flags]\n\nflags:\n", progName)
		flag.PrintDefaults()
		fmt.Fprintf(out, "\nkeys: q quit   l host list   ? help   ^L redraw\n")
		fmt.Fprintf(out, "headless: -dump N prints N ASCII frames (see README)\n")
		fmt.Fprintf(out, "LAN only: no auth, no TLS. Firewall the port from everything else.\n")
	}
	flag.Parse()

	if *verFlag {
		fmt.Println(Version)
		return
	}

	cfg, err := LoadConfig(*configFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonsai-server: %v\n", err)
		os.Exit(1)
	}

	// Flags beat the config file beat the defaults.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["port"] {
		cfg.Port = *portFlag
	}
	if set["bind"] {
		cfg.Bind = *bindFlag
	}
	if set["unicode"] {
		cfg.Unicode = *unicodeFlag
	}
	if set["log"] {
		cfg.LogPath = *logFlag
	}
	cfg.normalize()

	if *showCfg {
		printConfig(cfg)
		return
	}

	lg, closeLog := openLog(cfg.LogPath)
	defer closeLog()

	store := NewStore(&cfg)
	tree := NewTree(&cfg)

	// Bind first: a busy port must be a clean one-line error on stderr,
	// before the terminal is touched.
	ep, err := Listen(&cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonsai-server: cannot listen on %s:%d: %v\n",
			cfg.Bind, cfg.Port, friendlyBindError(err, cfg.Port))
		os.Exit(1)
	}
	addr := ep.Addr()
	lg.Printf("listening on %s (pid %d, version %s)", addr, os.Getpid(), Version)

	node := NewNode(&cfg, store, lg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go node.Serve(ctx, ep)

	if *dumpFlag > 0 {
		if *dumpGrowth >= 0 {
			tree.Growth = clamp01(*dumpGrowth)
		}
		dumpFrames(ctx, &cfg, store, tree, node, addr, *dumpFlag, *dumpEvery,
			*dumpW, *dumpH, *dumpOverlay)
		cancel()
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	if err := runTerminal(ctx, cancel, &cfg, store, tree, node, addr, lg); err != nil {
		cancel()
		// Quitting with q, Ctrl-C or SIGTERM is a clean shutdown, not a failure.
		if !errors.Is(err, context.Canceled) {
			// The screen is already torn down, so report on stderr.
			fmt.Fprintf(os.Stderr, "bonsai-server: %v\n", err)
			os.Exit(1)
		}
	}
	cancel()
	lg.Printf("shutdown after %s", ShortDuration(node.Uptime(time.Now())))
}

// runTerminal initialises tcell and hands over to the UI loop.
func runTerminal(ctx context.Context, cancel context.CancelFunc, cfg *Config, store *Store,
	tree *Tree, node *Node, addr string, lg *log.Logger) error {

	scr, err := newScreen()
	if err != nil {
		return err
	}
	defer scr.Fini()

	return RunUI(ctx, cancel, scr, cfg, store, tree, node, addr, lg)
}

// newScreen is split out so -dump never needs a terminal.
func newScreen() (tcell.Screen, error) {
	scr, err := tcell.NewScreen()
	if err != nil {
		return nil, fmt.Errorf("cannot open terminal (%v); try -dump N for a headless render", err)
	}
	if err := scr.Init(); err != nil {
		return nil, fmt.Errorf("cannot initialise terminal (%v); try -dump N for a headless render", err)
	}
	scr.Clear()
	return scr, nil
}

// dumpFrames prints headless ASCII frames. It is how you watch the tree without
// a TTY: CI checks, screenshots, and `bonsai-server -dump 12` next to the
// simulator.
func dumpFrames(ctx context.Context, cfg *Config, store *Store, tree *Tree, node *Node,
	addr string, frames int, every time.Duration, w, h int, overlay string) {

	comp := NewComposer(cfg, tree, node, addr)
	switch strings.ToLower(overlay) {
	case "hosts", "l":
		comp.Overlay = 1
	case "help", "?":
		comp.Overlay = 2
	}
	if w < 20 {
		w = 20
	}
	if h < 8 {
		h = 8
	}
	if every <= 0 {
		every = time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()

	out := os.Stdout
	for i := 0; i < frames; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		now := time.Now()
		snap := store.Snapshot(now)
		tree.Update(snap, now)
		c := NewCanvas(w, h, false)
		comp.Compose(c, snap, now)
		fmt.Fprintf(out, "--- frame %d/%d  %s  network %.0f%% (%s)  growth %.2f ---\n",
			i+1, frames, now.Format("15:04:05"), snap.Network*100, snap.Word, tree.Growth)
		fmt.Fprintln(out, c.String())
		if i < frames-1 {
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
	}
}

// openLog sets up the file logger. The screen is for the tree only, so logs
// always go to a file; if the file cannot be opened we discard rather than
// spew over the display.
func openLog(path string) (*log.Logger, func()) {
	if strings.EqualFold(path, "none") || strings.EqualFold(path, "off") {
		return log.New(io.Discard, "", 0), func() {}
	}
	if path == "" {
		path = DefaultLogPath(progName)
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return log.New(io.Discard, "", 0), func() {}
	}
	rotateIfNeeded(f, 1<<20)
	return log.New(f, "", log.LstdFlags), func() { f.Close() }
}

// rotateIfNeeded truncates the log when it grows past the cap, keeping the tail.
func rotateIfNeeded(f *os.File, cap int64) {
	st, err := f.Stat()
	if err != nil || st.Size() < cap {
		return
	}
	keep := cap / 2
	buf := make([]byte, keep)
	if _, err := f.ReadAt(buf, st.Size()-keep); err != nil && err != io.EOF {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = f.Seek(0, io.SeekStart)
	_, _ = f.Write(buf)
}

func friendlyBindError(err error, port int) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "address already in use"):
		return fmt.Sprintf("port %d is already in use (is another bonsai-server running?)", port)
	case strings.Contains(msg, "permission denied"):
		return fmt.Sprintf("port %d needs root; pick a port above 1024", port)
	}
	return ""
}

func printConfig(cfg Config) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "bonsai-server: %v\n", err)
		os.Exit(1)
	}
}
