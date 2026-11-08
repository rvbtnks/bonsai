// Command bonsai-client reports a few metrics to a bonsai-monitor server.
//
//	./bonsai-client 192.168.1.1
//
// That is the whole interface. It prints one line, returns the shell prompt and
// keeps reporting in the background. Standard library only: no dependencies.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is stamped by the Makefile: -ldflags "-X main.Version=...".
var Version = "dev"

const (
	progName        = "bonsai-client"
	defaultPort     = 1717
	defaultInterval = 10 * time.Second
	replyTimeout    = 1 * time.Second
	dialTimeout     = 2 * time.Second
	maxLogBytes     = 1 << 20
	pidfileName     = "bonsai-client.pid"
	logName         = ".bonsai-client.log"
)

// options is the fully-resolved set of things the report loop needs.
type options struct {
	host     string // server host or IP, as typed
	port     int
	name     string // reported hostname, already sanitized
	interval time.Duration
	tcp      bool
	verbose  bool
}

// target is what the status line and the systemd unit show.
func (o options) target() string { return net.JoinHostPort(o.host, strconv.Itoa(o.port)) }

// tuneRuntime keeps the resident set of a background reporter small. It sends
// one datagram every few seconds, so it needs neither one scheduler thread per
// CPU (each with its own stack and caches) nor the default garbage-collector
// headroom, which is sized for throughput rather than footprint.
func tuneRuntime() {
	runtime.GOMAXPROCS(1)
	debug.SetGCPercent(40)
}

// noHugePages is the GODEBUG setting that keeps the heap off transparent huge
// pages. A kernel booted with THP "always" (the RHEL default, and common in
// containers) rounds every heap arena up to a 2 MB page, which costs a process
// that barely allocates anything about 2 MB of resident set it never touches.
const noHugePages = "disablethp=1"

// hugePagesForced reports whether the kernel hands out transparent huge pages
// whether we ask for them or not.
func hugePagesForced() bool {
	b, err := os.ReadFile("/sys/kernel/mm/transparent_hugepage/enabled")
	if err != nil {
		return false
	}
	return thpAlways(string(b))
}

// thpAlways reads the bracketed selection out of the kernel's THP file, e.g.
// "always [madvise] never". A missing or unreadable file means "not forced".
func thpAlways(enabled string) bool {
	return strings.Contains(enabled, "[always]")
}

// withGoDebug returns a copy of env with setting folded into GODEBUG, keeping
// anything already there. If the caller already has an opinion about the
// setting, that opinion wins and env is returned unchanged.
func withGoDebug(env []string, setting string) []string {
	key := setting
	if i := strings.IndexByte(setting, '='); i >= 0 {
		key = setting[:i]
	}
	out := make([]string, len(env), len(env)+1)
	copy(out, env)
	for i, kv := range out {
		val, ok := strings.CutPrefix(kv, "GODEBUG=")
		if !ok {
			continue
		}
		if strings.Contains(val, key) {
			return out
		}
		if val == "" {
			out[i] = "GODEBUG=" + setting
		} else {
			out[i] = "GODEBUG=" + val + "," + setting
		}
		return out
	}
	return append(out, "GODEBUG="+setting)
}

// reExecWithoutHugePages replaces this process image with itself, with the
// GODEBUG setting above added, so the runtime reserves its heap with
// MADV_NOHUGEPAGE. Pid, file descriptors and argv are all unchanged, which
// keeps the pidfile and systemd's notion of the main process correct. It is a
// no-op when huge pages are not being forced on us or when the caller already
// has an opinion about disablethp; if the re-exec fails we just carry on with
// the image we have.
func reExecWithoutHugePages() {
	if !hugePagesForced() || strings.Contains(os.Getenv("GODEBUG"), "disablethp") {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	_ = syscall.Exec(exe, os.Args, withGoDebug(os.Environ(), noHugePages))
}

func main() {
	reExecWithoutHugePages()
	tuneRuntime()

	var (
		stopFlag      = flag.Bool("stop", false, "stop the running instance")
		statusFlag    = flag.Bool("status", false, "print whether an instance is running")
		fgFlag        = flag.Bool("f", false, "run in the foreground (for systemd or debugging)")
		verboseFlag   = flag.Bool("v", false, "verbose: print each send and reply (implies -f)")
		portFlag      = flag.Int("port", 0, "server port (default 1717)")
		intervalFlag  = flag.Float64("interval", 0, "report interval in seconds (default 10)")
		tcpFlag       = flag.Bool("tcp", false, "use TCP instead of UDP")
		nameFlag      = flag.String("name", "", "override the reported hostname")
		printUnitFlag = flag.Bool("print-unit", false, "print a ready-to-use systemd unit and exit")
		verFlag       = flag.Bool("version", false, "print the version and exit")
		childFlag     = flag.Bool("child", false, "") // hidden: set when re-exec'd in the background
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"usage: %s [-f] [-v] [-tcp] [-port N] [-interval S] [-name NAME] HOST[:PORT]\n"+
				"       %s -stop | -status | -print-unit HOST[:PORT] | -version\n",
			progName, progName)
	}
	flag.Parse()

	switch {
	case *verFlag:
		fmt.Println(Version)
		return
	case *stopFlag:
		os.Exit(runStop())
	case *statusFlag:
		os.Exit(runStatus())
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "%s: unexpected argument %q\n", progName, args[1])
		flag.Usage()
		os.Exit(2)
	}

	host, port, err := splitTarget(args[0], *portFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		os.Exit(2)
	}

	o := options{
		host:     host,
		port:     port,
		name:     resolveName(*nameFlag),
		interval: interval(*intervalFlag),
		tcp:      *tcpFlag,
		verbose:  *verboseFlag,
	}

	if *printUnitFlag {
		fmt.Print(systemdUnit(o))
		return
	}

	switch {
	case *childFlag:
		// We are the backgrounded child: own session, no controlling terminal,
		// silent except for the log file.
		signal.Ignore(syscall.SIGHUP)
		os.Exit(runChild(o))
	case *fgFlag || *verboseFlag:
		os.Exit(runForeground(o))
	default:
		os.Exit(runDaemon(o))
	}
}

func interval(sec float64) time.Duration {
	if sec <= 0 {
		return defaultInterval
	}
	if sec < 1 {
		sec = 1 // never hammer the LAN
	}
	if sec > 86400 {
		sec = 86400
	}
	return time.Duration(sec * float64(time.Second))
}

// splitTarget accepts HOST or HOST:PORT; -port wins over an inline port.
func splitTarget(arg string, portOverride int) (string, int, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", 0, errors.New("empty server address")
	}
	host := arg
	port := defaultPort
	if h, p, err := net.SplitHostPort(arg); err == nil {
		n, convErr := strconv.Atoi(p)
		if convErr != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("bad port in %q", arg)
		}
		host, port = h, n
	} else if strings.Count(arg, ":") > 1 {
		return "", 0, fmt.Errorf("bad address %q (wrap IPv6 in brackets, e.g. [fd00::1]:1717)", arg)
	}
	if portOverride > 0 {
		if portOverride > 65535 {
			return "", 0, fmt.Errorf("bad -port %d", portOverride)
		}
		port = portOverride
	}
	if host == "" {
		return "", 0, fmt.Errorf("bad address %q", arg)
	}
	return host, port, nil
}

// resolveName picks the reported hostname: -name, else os.Hostname(), always
// sanitized to [A-Za-z0-9._-]{1,32} so the server never has to trust us.
func resolveName(override string) string {
	raw := override
	if raw == "" {
		h, err := os.Hostname()
		if err != nil || strings.TrimSpace(h) == "" {
			raw = fmt.Sprintf("host-%d", os.Getpid())
		} else {
			raw = h
		}
	}
	return SanitizeHost(raw)
}

// SanitizeHost keeps the allowed charset and caps the name at 32 characters.
// Splitting a domain name off is deliberate: "web01.example.com" reports as
// "web01.example.com" when it fits, and the dots are legal anyway.
func SanitizeHost(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if b.Len() >= 32 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	return out
}

// --- start / stop / status ---------------------------------------------------

func runStop() int {
	stopped, failed := stopAllInstances(-1)
	removePID(pidfilePath())
	switch {
	case failed > 0:
		fmt.Fprintf(os.Stderr, "%s: could not stop %d instance(s)\n", progName, failed)
		return 1
	case stopped == 0:
		fmt.Println("not running")
	default:
		fmt.Println("stopped")
	}
	return 0
}

func runStatus() int {
	pid, ok := runningInstance()
	if !ok {
		fmt.Println("not running")
		return 1
	}
	target := targetForPID(pid)
	if target == "" {
		fmt.Printf("running (pid %d)\n", pid)
	} else {
		fmt.Printf("running (pid %d) -> %s\n", pid, target)
	}
	return 0
}

// runDaemon stops any older instance (the newest invocation wins), re-execs
// itself detached, records the pid and prints exactly one line.
func runDaemon(o options) int {
	// A different server address is fine: the new instance simply takes over.
	stopAllInstances(-1)

	self, err := selfPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot open %s: %v\n", progName, os.DevNull, err)
		return 1
	}
	defer devNull.Close()

	cmd := exec.Command(self, childArgs(o)...)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	// Setsid detaches the child from this session, so closing the SSH window
	// that started it (and its SIGHUP) does not take the client down.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot start background client: %v\n", progName, err)
		return 1
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	if err := writePID(pidfilePath(), pid); err != nil {
		fmt.Fprintf(os.Stderr, "%s: started (pid %d) but could not write pidfile: %v\n",
			progName, pid, err)
		return 1
	}
	fmt.Printf("%s started (pid %d) -> %s\n", progName, pid, o.target())
	return 0
}

// childArgs rebuilds an equivalent command line for the detached child.
func childArgs(o options) []string {
	args := []string{"-child", "-interval", strconv.Itoa(int(o.interval / time.Second))}
	if o.tcp {
		args = append(args, "-tcp")
	}
	if o.name != "" {
		args = append(args, "-name", o.name)
	}
	args = append(args, "-port", strconv.Itoa(o.port), o.host)
	return args
}

// runChild is the detached background loop. Everything goes to the log file.
func runChild(o options) int {
	lg := newLogger(logPath(), o.verbose)
	lg.printf("started (pid %d) -> %s name=%s interval=%s tcp=%t",
		os.Getpid(), o.target(), o.name, o.interval, o.tcp)
	_ = writePID(pidfilePath(), os.Getpid())
	code := runLoop(o, lg)
	lg.printf("stopped (pid %d)", os.Getpid())
	lg.close()
	removePID(pidfilePath())
	return code
}

// runForeground is -f / -v: the same loop, but logging to the terminal too and
// keeping the pidfile so -status and -stop still work (systemd uses -f).
func runForeground(o options) int {
	lg := newLogger(logPath(), true)
	lg.stdout = os.Stderr
	lg.printf("started in foreground (pid %d) -> %s name=%s interval=%s tcp=%t",
		os.Getpid(), o.target(), o.name, o.interval, o.tcp)
	_ = writePID(pidfilePath(), os.Getpid())
	code := runLoop(o, lg)
	lg.printf("stopped (pid %d)", os.Getpid())
	lg.close()
	removePID(pidfilePath())
	return code
}

// runLoop collects and reports until a signal arrives. It never returns because
// of a network problem: a dead server just means quiet retries.
func runLoop(o options, lg *logger) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	var col collector
	sender := newSender(o)

	send := func() {
		m := col.collect()
		payload := buildPayload(o.name, m)
		reply, err := sender.Send(payload)
		if err != nil {
			lg.printf("send to %s failed: %v", o.target(), err)
			return
		}
		lg.verbosef("sent %q -> reply %q", payload, reply)
	}

	send() // report immediately so the tree fills in at once

	// Startup (flag parsing, DNS, the first /proc read) leaves a few hundred kB
	// of garbage behind. Hand it back to the kernel now instead of waiting for
	// the scavenger, so the reported footprint is the steady-state one.
	debug.FreeOSMemory()

	tick := time.NewTicker(o.interval)
	defer tick.Stop()
	for {
		select {
		case <-sigs:
			return 0
		case <-tick.C:
			send()
		}
	}
}

// --- sending ----------------------------------------------------------------

// sender holds a cached resolution of the server address and drops it after any
// failure, so a DNS or IP change recovers on its own.
type sender struct {
	o       options
	udpAddr *net.UDPAddr
	tcpAddr string
}

func newSender(o options) *sender { return &sender{o: o} }

func (s *sender) Send(payload string) (string, error) {
	if s.o.tcp {
		return s.sendTCP(payload)
	}
	return s.sendUDP(payload)
}

func (s *sender) sendUDP(payload string) (string, error) {
	if s.udpAddr == nil {
		addr, err := net.ResolveUDPAddr("udp", s.o.target())
		if err != nil {
			return "", err
		}
		s.udpAddr = addr
	}
	conn, err := net.DialUDP("udp", nil, s.udpAddr)
	if err != nil {
		s.udpAddr = nil // force a re-resolve next tick
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(replyTimeout))
	if _, err := conn.Write(append([]byte(payload), '\n')); err != nil {
		s.udpAddr = nil
		return "", err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		// UDP is fire-and-forget: a missing reply is not a failure, but a
		// network error means we should re-resolve before the next tick.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "", nil
		}
		s.udpAddr = nil
		return "", err
	}
	return strings.TrimRight(string(buf[:n]), "\r\n"), nil
}

func (s *sender) sendTCP(payload string) (string, error) {
	conn, err := net.DialTimeout("tcp", s.o.target(), dialTimeout)
	if err != nil {
		s.tcpAddr = ""
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(replyTimeout + dialTimeout))
	if _, err := conn.Write(append([]byte(payload), '\n')); err != nil {
		return "", err
	}
	buf := make([]byte, 512)
	n, _ := conn.Read(buf) // a missing reply is not an error
	return strings.TrimRight(string(buf[:n]), "\r\n"), nil
}

// --- payload ----------------------------------------------------------------

func buildPayload(name string, m metrics) string {
	var b strings.Builder
	b.WriteString("host=")
	b.WriteString(name)
	if m.cpu != nil {
		fmt.Fprintf(&b, " cpu=%.1f", *m.cpu)
	}
	if m.temp != nil {
		fmt.Fprintf(&b, " temp=%.1f", *m.temp)
	}
	if m.load != nil {
		fmt.Fprintf(&b, " load=%.2f", *m.load)
	}
	if m.up != nil {
		fmt.Fprintf(&b, " up=%.0f", *m.up)
	}
	line := b.String()
	if len(line) > 512 {
		line = line[:512]
	}
	return line
}

// --- metrics ----------------------------------------------------------------

type metrics struct {
	cpu, temp, load, up *float64
}

func fp(v float64) *float64 { return &v }

// collector remembers the previous /proc/stat sample so CPU usage is a delta.
type collector struct {
	prevIdle  uint64
	prevTotal uint64
	havePrev  bool
}

// collect reads every metric it can and omits the rest. Nothing here may fail
// the whole report: a machine without thermal zones simply sends no temp.
func (c *collector) collect() metrics {
	var m metrics
	if v, ok := c.cpuPercent(); ok {
		m.cpu = fp(v)
	}
	if v, ok := readTemp(); ok {
		m.temp = fp(v)
	}
	if v, ok := readLoad(); ok {
		m.load = fp(v)
	}
	if v, ok := readUptime(); ok {
		m.up = fp(v)
	}
	return m
}

// cpuPercent is the delta of the aggregate "cpu" line in /proc/stat. The first
// call has no previous sample, so it reports nothing rather than a bogus 0.
func (c *collector) cpuPercent() (float64, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, false
		}
		var total, idle uint64
		for i, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		if !c.havePrev {
			c.prevIdle, c.prevTotal, c.havePrev = idle, total, true
			return 0, false
		}
		dTotal := total - c.prevTotal
		dIdle := idle - c.prevIdle
		c.prevIdle, c.prevTotal = idle, total
		if dTotal == 0 {
			return 0, false
		}
		pct := 100 * (1 - float64(dIdle)/float64(dTotal))
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		return pct, true
	}
	return 0, false
}

// readTemp takes the maximum of every plausible thermal zone, in degrees C.
// No zones, no readable zones or no sane values means the metric is omitted.
func readTemp() (float64, bool) {
	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(zones) == 0 {
		return 0, false
	}
	best := 0.0
	found := false
	for _, z := range zones {
		c, ok := parseTempFile(z)
		if !ok {
			continue
		}
		if !found || c > best {
			best, found = c, true
		}
	}
	if !found {
		return 0, false
	}
	return best, true
}

// parseTempFile reads one thermal zone. The kernel reports millidegrees, and
// some drivers report nonsense; anything outside a plausible range is ignored
// so a broken sensor cannot poison the reading.
func parseTempFile(path string) (float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, false
	}
	c := v / 1000.0
	if c < -100 || c > 250 {
		return 0, false
	}
	return c, true
}

func readLoad() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	cores := runtime.NumCPU()
	if cores < 1 {
		cores = 1
	}
	return v / float64(cores), true
}

func readUptime() (float64, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

// --- pidfile ----------------------------------------------------------------

// pidfilePath is $XDG_RUNTIME_DIR/bonsai-client.pid, falling back to
// /tmp/bonsai-client-<uid>.pid.
func pidfilePath() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return filepath.Join(d, pidfileName)
		}
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.pid", progName, os.Getuid()))
}

func logPath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, logName)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.log", progName, os.Getuid()))
}

func writePID(path string, pid int) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removePID(path string) { _ = os.Remove(path) }

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// runningInstance returns the pid of a live bonsai-client, cleaning up the
// pidfile if it is stale. Trusting a pid number alone is how you end up
// killing an innocent process after a PID recycle, so we also check that
// /proc/<pid>/cmdline really is us.
func runningInstance() (int, bool) {
	path := pidfilePath()
	pid, ok := readPID(path)
	if !ok {
		removePID(path)
		return 0, false
	}
	if !processIsOurs(pid) {
		removePID(path)
		return 0, false
	}
	return pid, true
}

// processAlive is signal 0 plus a zombie check. A process that has exited but
// not yet been reaped still answers signal 0, which would make -status report
// a client that is already gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no /proc to consult: trust the signal
	}
	if i := strings.LastIndexByte(string(data), ')'); i >= 0 && i+2 < len(data) {
		return data[i+2] != 'Z'
	}
	return true
}

// processIsOurs is liveness plus a cmdline identity check.
func processIsOurs(pid int) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	if !processAlive(pid) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// No /proc: fall back to liveness only rather than refusing to start.
		return true
	}
	fields := strings.FieldsFunc(string(cmdline), func(r rune) bool { return r == 0 })
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	if base == progName || strings.HasPrefix(base, progName) {
		return true
	}
	// Some supervisors rewrite argv[0]; check the resolved exe as a fallback.
	if target, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		b := filepath.Base(target)
		if b == progName || strings.HasPrefix(b, progName) {
			return true
		}
	}
	return false
}

// targetForPID digs the server address back out of the running client's
// command line so -status can print where it is reporting.
func targetForPID(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	fields := strings.FieldsFunc(string(data), func(r rune) bool { return r == 0 })
	return targetFromArgv(fields)
}

// targetFromArgv reconstructs HOST:PORT from a client command line. Split out
// from targetForPID so the parsing can be tested without a live process.
func targetFromArgv(fields []string) string {
	var host string
	port := 0
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "-port" && i+1 < len(fields):
			port, _ = strconv.Atoi(fields[i+1])
			i++
		case strings.HasPrefix(f, "-port="):
			port, _ = strconv.Atoi(strings.TrimPrefix(f, "-port="))
		case f == "-name" || f == "-interval":
			i++ // these take a value that is not the server
		case strings.HasPrefix(f, "-"):
			// flags: -child, -f, -tcp, -v, ...
		default:
			base := filepath.Base(f)
			if base != progName && !strings.HasPrefix(base, progName) {
				host = f
			}
		}
	}
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		if port == 0 {
			return host
		}
		return net.JoinHostPort(h, strconv.Itoa(port))
	}
	if port == 0 {
		port = defaultPort
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// terminate asks a pid to exit, escalating once if it ignores us.
func terminate(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(50 * time.Millisecond)
	return nil
}

// stopAllInstances enforces "the newest invocation wins". It stops the process
// named in the pidfile and, as a safety net, any other detached instance owned
// by this user. The net matters because the pidfile location can move between
// logins ($XDG_RUNTIME_DIR is only set for real sessions), and an orphan left
// behind would otherwise report forever.
//
// Foreground instances (-f, which is what systemd runs) are never touched:
// their supervisor owns them and would just restart them.
//
// It returns how many processes were stopped and how many failed.
func stopAllInstances(except int) (stopped, failed int) {
	pidfilePID := 0
	if pid, ok := runningInstance(); ok {
		pidfilePID = pid
	}
	for _, pid := range sweepList(pidfilePID, findBackgroundInstances(), except, os.Getpid()) {
		if err := terminate(pid); err != nil {
			failed++
			continue
		}
		stopped++
	}
	return stopped, failed
}

// sweepList is the decision half of stopAllInstances, kept pure so it can be
// tested without signalling anything: the pidfile instance first, then whatever
// /proc turned up, minus ourselves, minus the pid we were told to spare, minus
// anything that is not a plausible process, and with duplicates removed.
func sweepList(pidfilePID int, found []int, except, self int) []int {
	var out []int
	seen := map[int]bool{}
	add := func(pid int) {
		if pid <= 1 || pid == except || pid == self || seen[pid] {
			return
		}
		seen[pid] = true
		out = append(out, pid)
	}
	add(pidfilePID)
	for _, pid := range found {
		add(pid)
	}
	return out
}

// findBackgroundInstances scans /proc for detached clients owned by this user.
func findBackgroundInstances() []int {
	if runtime.GOOS != "linux" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	uid := os.Getuid()
	var out []int
	for _, e := range entries {
		name := e.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		if !sameUID(pid, uid) {
			continue
		}
		argv := procArgv(pid)
		if !isBackgroundClient(argv) {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// isBackgroundClient reports whether argv is a detached bonsai-client child.
func isBackgroundClient(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	base := filepath.Base(argv[0])
	if base != progName && !strings.HasPrefix(base, progName) {
		return false
	}
	child := false
	foreground := false
	for _, a := range argv[1:] {
		switch a {
		case "-child":
			child = true
		case "-f", "-v":
			foreground = true
		}
	}
	return child && !foreground
}

func procArgv(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	return strings.FieldsFunc(string(data), func(r rune) bool { return r == 0 })
}

// sameUID compares against the real uid in /proc/<pid>/status.
func sameUID(pid, uid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false
		}
		real, err := strconv.Atoi(fields[1])
		return err == nil && real == uid
	}
	return false
}

// selfPath is the absolute path of this binary, symlinks resolved.
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		if len(os.Args) > 0 && strings.Contains(os.Args[0], "/") {
			abs, err2 := filepath.Abs(os.Args[0])
			if err2 == nil {
				return abs, nil
			}
		}
		return "", fmt.Errorf("cannot locate own binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, nil
	}
	return p, nil
}

// --- systemd ----------------------------------------------------------------

func systemdUnit(o options) string {
	bin, err := selfPath()
	if err != nil {
		bin = "/usr/local/bin/" + progName
	}
	args := []string{"-f", "-interval", strconv.Itoa(int(o.interval / time.Second))}
	if o.tcp {
		args = append(args, "-tcp")
	}
	if o.name != "" {
		args = append(args, "-name", o.name)
	}
	args = append(args, "-port", strconv.Itoa(o.port), o.target())
	return fmt.Sprintf(`[Unit]
Description=bonsai-monitor client for %s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s %s
Restart=always
RestartSec=5
# Keeps the log small and off the journal
StandardOutput=null
StandardError=null

[Install]
WantedBy=multi-user.target
`, o.target(), bin, strings.Join(args, " "))
}

// --- logging ----------------------------------------------------------------

// logger writes to a file (rotating at about 1 MB) and optionally echoes to a
// stream for -v. The background client is otherwise completely silent.
type logger struct {
	f       *os.File
	stdout  io.Writer
	verbose bool
	path    string
}

func newLogger(path string, verbose bool) *logger {
	l := &logger{verbose: verbose, path: path}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		rotateLog(f, maxLogBytes)
		l.f = f
	}
	return l
}

func (l *logger) printf(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	if l.f != nil {
		_, _ = l.f.WriteString(line)
	}
	if l.stdout != nil {
		_, _ = io.WriteString(l.stdout, line)
	}
}

func (l *logger) verbosef(format string, args ...any) {
	if !l.verbose {
		return
	}
	l.printf(format, args...)
}

func (l *logger) close() {
	if l.f != nil {
		_ = l.f.Close()
	}
}

// rotateLog keeps the tail half when the file grows past the cap.
func rotateLog(f *os.File, cap int64) {
	st, err := f.Stat()
	if err != nil || st.Size() < cap {
		return
	}
	keep := cap / 2
	buf := make([]byte, keep)
	n, err := f.ReadAt(buf, st.Size()-keep)
	if err != nil && err != io.EOF {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	_, _ = f.Write(buf[:n])
}
