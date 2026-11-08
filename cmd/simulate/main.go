// Command simulate drives a bonsai-monitor server with fake clients so you can
// watch the tree grow, degrade, dull and recover without owning eight machines.
//
//	go run ./cmd/simulate -server localhost -n 8
//
// It speaks the same one-line QOTD payload the real client sends, over UDP by
// default, and prints what phase the script is in.
package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultPort = 1717

// role is a scripted behaviour for one fake host.
type role int

const (
	roleHealthy role = iota
	roleCPUSpike
	roleHot
	roleFlapping
	roleSilent
	roleRebooted
)

func (r role) String() string {
	switch r {
	case roleHealthy:
		return "healthy"
	case roleCPUSpike:
		return "cpu-spike"
	case roleHot:
		return "hot"
	case roleFlapping:
		return "flapping"
	case roleSilent:
		return "silent"
	case roleRebooted:
		return "rebooted"
	}
	return "?"
}

// host is one simulated machine.
type host struct {
	name string
	role role
	rnd  *rand.Rand
	up   float64 // seconds of simulated uptime
}

// phase is one act of the script.
type phase struct {
	name string
	dur  time.Duration
	// stress is how many hosts get an unhealthy role, and which ones.
	stress map[role]int
	// quiet is how many hosts stop reporting entirely.
	quiet int
}

func main() {
	var (
		server   = flag.String("server", "localhost", "bonsai-server host or host:port")
		port     = flag.Int("port", 0, "server port (default 1717, or the one in -server)")
		n        = flag.Int("n", 8, "number of fake clients")
		interval = flag.Float64("interval", 2, "seconds between reports")
		tcp      = flag.Bool("tcp", false, "use TCP instead of UDP")
		once     = flag.Bool("once", false, "run the timeline once instead of looping")
		static   = flag.Bool("static", false, "skip the script: keep every host healthy")
		prefix   = flag.String("prefix", "sim", "hostname prefix")
		seed     = flag.Int64("seed", 1, "PRNG seed, so runs are reproducible")
		speed    = flag.Float64("speed", 1, "run the timeline faster (>1) or slower (<1)")
		quiet    = flag.Bool("quiet", false, "do not print phase changes")
	)
	flag.Parse()

	if *n < 1 {
		fmt.Fprintln(os.Stderr, "simulate: -n must be at least 1")
		os.Exit(2)
	}
	if *interval < 0.2 {
		*interval = 0.2 // the server rate limits at ~1/s per source anyway
	}

	addr, err := resolveTarget(*server, *port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "simulate: %v\n", err)
		os.Exit(2)
	}

	hosts := makeHosts(*n, *prefix, *seed)
	stop := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		close(stop)
	}()

	if !*quiet {
		fmt.Printf("simulate: %d fake clients -> %s (%s) every %.1fs\n",
			*n, addr, protoName(*tcp), *interval)
		fmt.Printf("simulate: hosts: %s\n", hostSummary(hosts))
	}

	tick := time.NewTicker(time.Duration(*interval * float64(time.Second)))
	defer tick.Stop()

	var timeline []phase
	if *static {
		timeline = []phase{{name: "static: everything healthy", dur: 0, stress: nil, quiet: 0}}
	} else {
		timeline = script(*n)
		if *speed > 0 && *speed != 1 {
			for i := range timeline {
				timeline[i].dur = time.Duration(float64(timeline[i].dur) / *speed)
				if timeline[i].dur < time.Second {
					timeline[i].dur = time.Second
				}
			}
		}
	}

	pass := 0
	for {
		pass++
		for _, ph := range timeline {
			select {
			case <-stop:
				if !*quiet {
					fmt.Println("simulate: stopped")
				}
				return
			default:
			}
			if !*quiet {
				fmt.Printf("[%s] phase %d/%d: %s (%s)\n",
					time.Now().Format("15:04:05"), indexOf(timeline, ph)+1, len(timeline),
					ph.name, durString(ph.dur))
			}
			applyPhase(hosts, ph, *seed)
			runPhase(hosts, ph, addr, *tcp, tick.C, stop, *quiet)
			select {
			case <-stop:
				return
			default:
			}
		}
		if *once {
			if !*quiet {
				fmt.Println("simulate: timeline complete")
			}
			return
		}
		if !*quiet {
			fmt.Printf("simulate: --- pass %d complete, starting again ---\n", pass)
		}
	}
}

// script is the acceptance-criteria story: a full canopy, then one host
// degrading, then several, then recovery.
func script(n int) []phase {
	one := map[role]int{roleCPUSpike: 1}
	several := map[role]int{roleCPUSpike: 1, roleHot: 1, roleFlapping: 1}
	return []phase{
		{name: "all healthy - the tree grows toward a full canopy", dur: 40 * time.Second},
		{name: "one host degrades - a red cluster, the tree dulls slightly", dur: 30 * time.Second, stress: one},
		{name: "several degrade and one goes silent - droop, thin canopy, red and brown",
			dur: 45 * time.Second, stress: several, quiet: 1},
		{name: "recovery - the red clears and the tree regrows", dur: 50 * time.Second},
	}
}

func indexOf(ps []phase, p phase) int {
	for i := range ps {
		if ps[i].name == p.name {
			return i
		}
	}
	return 0
}

func durString(d time.Duration) string {
	if d <= 0 {
		return "until interrupted"
	}
	return d.String()
}

func makeHosts(n int, prefix string, seed int64) []*host {
	hosts := make([]*host, 0, n)
	for i := 0; i < n; i++ {
		hosts = append(hosts, &host{
			name: fmt.Sprintf("%s-%02d", prefix, i+1),
			role: roleHealthy,
			rnd:  rand.New(rand.NewSource(seed + int64(i)*7919)),
			up:   float64(3600 * (24 + i*7)),
		})
	}
	return hosts
}

func hostSummary(hosts []*host) string {
	parts := make([]string, 0, len(hosts))
	for _, h := range hosts {
		parts = append(parts, h.name)
	}
	return strings.Join(parts, " ")
}

// applyPhase assigns roles: the stressed ones go to the last hosts in the list,
// so the healthy majority stays healthy.
func applyPhase(hosts []*host, ph phase, seed int64) {
	for _, h := range hosts {
		h.role = roleHealthy
	}
	i := len(hosts) - 1
	// The silent host stops reporting altogether.
	for k := 0; k < ph.quiet && i >= 0; k, i = k+1, i-1 {
		hosts[i].role = roleSilent
	}
	for role, count := range ph.stress {
		for k := 0; k < count && i >= 0; k, i = k+1, i-1 {
			hosts[i].role = role
		}
	}
	if len(hosts) > 2 && ph.stress == nil && ph.quiet == 0 {
		// During the healthy phases keep one machine freshly rebooted so the
		// reboot cap is visible, then let it grow out of the window.
		hosts[0].role = roleRebooted
		hosts[0].up = 30
	}
	_ = seed
}

// runPhase reports for the phase duration, then returns.
func runPhase(hosts []*host, ph phase, addr string, tcp bool, ticks <-chan time.Time, stop chan struct{}, quiet bool) {
	report(hosts, addr, tcp, quiet) // immediately, so the phase shows at once
	if ph.dur <= 0 {
		for {
			select {
			case <-stop:
				return
			case <-ticks:
				report(hosts, addr, tcp, quiet)
			}
		}
	}
	done := time.After(ph.dur)
	for {
		select {
		case <-stop:
			return
		case <-done:
			return
		case <-ticks:
			report(hosts, addr, tcp, quiet)
		}
	}
}

// report builds and sends one payload per reporting host.
func report(hosts []*host, addr string, tcp, quiet bool) {
	for _, h := range hosts {
		if h.role == roleSilent {
			continue // says nothing at all - the server ages it out
		}
		h.up += 2
		cpu, temp, load := h.readings()
		line := fmt.Sprintf("host=%s cpu=%.1f temp=%.1f load=%.2f up=%.0f",
			h.name, cpu, temp, load, h.up)
		reply, err := send(addr, line, tcp)
		if err != nil {
			if !quiet {
				fmt.Printf("simulate: send %s: %v (is bonsai-server running?)\n", h.name, err)
				quiet = true // one complaint is enough
			}
			return
		}
		if !quiet && strings.Contains(reply, "status=") && strings.Contains(reply, "critical") {
			// nothing: the tree shows it
			_ = reply
		}
	}
}

// readings turns a role into plausible numbers.
func (h *host) readings() (cpu, temp, load float64) {
	jitter := func(base, amp float64) float64 {
		return math.Max(0, base+(h.rnd.Float64()-0.5)*amp)
	}
	switch h.role {
	case roleCPUSpike:
		return jitter(95, 8), jitter(62, 6), jitter(1.2, 0.4)
	case roleHot:
		return jitter(55, 12), jitter(88, 5), jitter(0.7, 0.3)
	case roleFlapping:
		// Alternate between fine and awful so the cluster blinks between bands.
		if int(h.up/6)%2 == 0 {
			return jitter(88, 10), jitter(78, 4), jitter(1.6, 0.4)
		}
		return jitter(18, 10), jitter(52, 6), jitter(0.3, 0.2)
	case roleRebooted:
		return jitter(40, 20), jitter(48, 6), jitter(0.9, 0.4)
	default:
		return jitter(14, 12), jitter(48, 8), jitter(0.25, 0.3)
	}
}

// send is a minimal client: UDP or TCP, one line out, one quote back.
func send(addr, line string, tcp bool) (string, error) {
	if tcp {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			return "", err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			return "", err
		}
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		return strings.TrimRight(string(buf[:n]), "\r\n"), nil
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return "", err
	}
	conn, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(1 * time.Second))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		return "", err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "", nil // UDP: a missing quote is not a failure
		}
		return "", err
	}
	return strings.TrimRight(string(buf[:n]), "\r\n"), nil
}

func resolveTarget(server string, portFlag int) (string, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return "", fmt.Errorf("empty -server")
	}
	if h, p, err := net.SplitHostPort(server); err == nil {
		if portFlag > 0 {
			return net.JoinHostPort(h, strconv.Itoa(portFlag)), nil
		}
		if _, err := strconv.Atoi(p); err != nil {
			return "", fmt.Errorf("bad port in %q", server)
		}
		return server, nil
	}
	port := defaultPort
	if portFlag > 0 {
		port = portFlag
	}
	if strings.Count(server, ":") > 0 {
		return net.JoinHostPort(server, strconv.Itoa(port)), nil
	}
	return net.JoinHostPort(server, strconv.Itoa(port)), nil
}

func protoName(tcp bool) string {
	if tcp {
		return "tcp"
	}
	return "udp"
}
