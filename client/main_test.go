package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- pidfile: the three cases the spec calls out -----------------------------

// withPidfileDir points the client at a scratch XDG_RUNTIME_DIR so tests never
// touch the real one.
func withPidfileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if got := pidfilePath(); got != filepath.Join(dir, pidfileName) {
		t.Fatalf("pidfilePath() = %q, want it inside %q", got, dir)
	}
	return dir
}

// TestHelperProcess is not a test: it is the body of the fake client that other
// tests spawn. The test binary is copied to a file named "bonsai-client" and run
// with this, so its /proc/<pid>/cmdline looks exactly like the real thing.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("BONSAI_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// spawnFakeClient copies this test binary to <tmp>/bonsai-client and starts it,
// returning a live pid whose cmdline really is "bonsai-client".
func spawnFakeClient(t *testing.T) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary: %v", err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, progName)
	in, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	if err := os.WriteFile(fake, in, 0o755); err != nil {
		t.Fatalf("copy: %v", err)
	}
	cmd := exec.Command(fake, "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "BONSAI_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	// Wait until /proc has caught up.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper pid %d never appeared in /proc", pid)
	return 0
}

// spawnForeignProcess starts something that is alive but is definitely not us:
// this is the recycled-PID case.
func spawnForeignProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a foreign process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return pid
}

func TestPidfileLiveInstanceIsRecognised(t *testing.T) {
	withPidfileDir(t)
	pid := spawnFakeClient(t)
	if err := writePID(pidfilePath(), pid); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	got, ok := runningInstance()
	if !ok || got != pid {
		t.Fatalf("runningInstance() = %d,%v; want %d,true", got, ok, pid)
	}
	if !processIsOurs(pid) {
		t.Error("a live bonsai-client should be recognised")
	}
}

func TestPidfileStaleIsCleanedUp(t *testing.T) {
	withPidfileDir(t)
	dead := deadPID(t)
	if err := writePID(pidfilePath(), dead); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	if pid, ok := runningInstance(); ok {
		t.Fatalf("a dead pid %d was reported as running", pid)
	}
	if _, err := os.Stat(pidfilePath()); !os.IsNotExist(err) {
		t.Errorf("the stale pidfile should have been removed: %v", err)
	}
}

func TestPidfileRecycledPIDDoesNotFoolUs(t *testing.T) {
	withPidfileDir(t)
	foreign := spawnForeignProcess(t)
	if err := writePID(pidfilePath(), foreign); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	// The pid is alive, but it is not a bonsai-client: we must not report it,
	// and above all we must not -stop it.
	if processIsOurs(foreign) {
		t.Error("a foreign live process was mistaken for the client")
	}
	if pid, ok := runningInstance(); ok {
		t.Fatalf("a recycled pid %d was reported as running", pid)
	}
	if _, err := os.Stat(pidfilePath()); !os.IsNotExist(err) {
		t.Error("the bogus pidfile should have been removed")
	}
	// And the foreign process must still be alive: nothing was killed.
	if !processAlive(foreign) {
		t.Error("the innocent process was killed")
	}
}

func TestPidfileGarbageIsIgnored(t *testing.T) {
	withPidfileDir(t)
	for _, body := range []string{"", "\n", "not-a-pid", "0", "-5", "999999999999999999999", "  \x00 "} {
		if err := os.WriteFile(pidfilePath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if pid, ok := runningInstance(); ok {
			t.Errorf("pidfile body %q reported pid %d", body, pid)
		}
	}
	if _, ok := runningInstance(); ok {
		t.Error("a missing pidfile should mean not running")
	}
}

func TestProcessAliveRejectsZombies(t *testing.T) {
	pid := spawnForeignProcess(t)
	if !processAlive(pid) {
		t.Fatal("a running process should be alive")
	}
	// Kill it but do not reap it: it is now a zombie, and signal 0 still
	// succeeds. processAlive must see through that.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("pid %d is a zombie but still reported alive", pid)
}

func TestProcessIsOursRejectsSelfAndPIDOne(t *testing.T) {
	if processIsOurs(os.Getpid()) {
		t.Error("we are not our own background instance")
	}
	if processIsOurs(1) {
		t.Error("pid 1 must never be claimed")
	}
	if processIsOurs(0) || processIsOurs(-1) {
		t.Error("non-positive pids must be rejected")
	}
}

func TestStopReportsCorrectlyWithNoInstance(t *testing.T) {
	withPidfileDir(t)
	if pid, ok := runningInstance(); ok {
		t.Fatalf("unexpected instance %d", pid)
	}
	// A stale pidfile must not block a fresh start.
	if err := writePID(pidfilePath(), deadPID(t)); err != nil {
		t.Fatal(err)
	}
	if _, ok := runningInstance(); ok {
		t.Fatal("stale pidfile still looks live")
	}
	pid := spawnFakeClient(t)
	if err := writePID(pidfilePath(), pid); err != nil {
		t.Fatal(err)
	}
	if _, ok := runningInstance(); !ok {
		t.Fatal("start after a stale pidfile should work")
	}
}

func TestWritePIDIsAtomicAndReadable(t *testing.T) {
	dir := withPidfileDir(t)
	path := pidfilePath()
	if err := writePID(path, 4242); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	pid, ok := readPID(path)
	if !ok || pid != 4242 {
		t.Errorf("readPID = %d,%v; want 4242,true", pid, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, pidfileName+".tmp")); !os.IsNotExist(err) {
		t.Error("the temporary file should have been renamed away")
	}
	// `kill $(cat pidfile)` must keep working: one number, nothing else.
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != "4242" {
		t.Errorf("pidfile body = %q, want a bare pid", raw)
	}
}

func TestTerminateStopsAProcess(t *testing.T) {
	withPidfileDir(t)
	pid := spawnFakeClient(t)
	if err := terminate(pid); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if processAlive(pid) {
		t.Errorf("pid %d is still alive after terminate", pid)
	}
	if err := terminate(pid); err != nil {
		t.Errorf("terminating an already-dead pid should be harmless: %v", err)
	}
}

func TestTargetForPID(t *testing.T) {
	withPidfileDir(t)
	pid := spawnFakeClient(t)
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"/usr/bin/bonsai-client", "-child", "-interval", "10", "-port", "1717", "192.168.1.1"}, "192.168.1.1:1717"},
		{[]string{"/usr/bin/bonsai-client", "-f", "10.0.0.5:2000"}, "10.0.0.5:2000"},
		{[]string{"bonsai-client", "-name", "nas01", "-port", "9999", "server.lan"}, "server.lan:9999"},
		{[]string{"bonsai-client", "-child", "-port", "1717", "-name", "x", "h"}, "h:1717"},
	}
	for _, c := range cases {
		procDir := fmt.Sprintf("/proc/%d", pid)
		if _, err := os.Stat(procDir); err != nil {
			t.Skip("no /proc")
		}
		got := targetFromArgv(c.argv)
		if got != c.want {
			t.Errorf("targetFromArgv(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
	if got := targetForPID(pid); got != "" {
		// The helper runs the test binary, which has no server argument.
		t.Logf("targetForPID(helper) = %q", got)
	}
}

// deadPID returns a pid that is guaranteed not to exist.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot run /bin/true: %v", err)
	}
	pid := cmd.Process.Pid
	// Reaped already, so it is dead; if the pid was recycled we would rather
	// skip than flake.
	if processIsOurs(pid) {
		t.Skip("pid was recycled immediately")
	}
	return pid
}

// --- argument handling -------------------------------------------------------

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		arg      string
		port     int
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"192.168.1.1", 0, "192.168.1.1", 1717, false},
		{"192.168.1.1:2000", 0, "192.168.1.1", 2000, false},
		{"nas.local", 0, "nas.local", 1717, false},
		{"nas.local:9999", 3000, "nas.local", 3000, false}, // -port wins
		{"192.168.1.1", 3000, "192.168.1.1", 3000, false},
		{"[fd00::1]:1717", 0, "fd00::1", 1717, false},
		{"  10.0.0.1  ", 0, "10.0.0.1", 1717, false},
		{"", 0, "", 0, true},
		{"host:0", 0, "", 0, true},
		{"host:99999", 0, "", 0, true},
		{"host:abc", 0, "", 0, true},
		{"host", 70000, "", 0, true},
		{"fd00::1", 0, "", 0, true}, // ambiguous IPv6 without brackets
		{":1717", 0, "", 0, true},
	}
	for _, c := range cases {
		host, port, err := splitTarget(c.arg, c.port)
		if c.wantErr {
			if err == nil {
				t.Errorf("splitTarget(%q, %d) should fail, got %s:%d", c.arg, c.port, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitTarget(%q, %d): %v", c.arg, c.port, err)
			continue
		}
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("splitTarget(%q, %d) = %s:%d, want %s:%d", c.arg, c.port, host, port, c.wantHost, c.wantPort)
		}
	}
}

func TestIntervalClamping(t *testing.T) {
	if interval(0) != defaultInterval {
		t.Error("0 should mean the default interval")
	}
	if interval(-5) != defaultInterval {
		t.Error("a negative interval should mean the default")
	}
	if interval(0.2) != time.Second {
		t.Errorf("sub-second intervals should be raised to 1s: %v", interval(0.2))
	}
	if interval(30) != 30*time.Second {
		t.Errorf("30 -> %v", interval(30))
	}
	if interval(1e9) != 86400*time.Second {
		t.Errorf("absurd intervals should be capped: %v", interval(1e9))
	}
}

func TestResolveName(t *testing.T) {
	if got := resolveName("My Host!"); got != "My_Host_" {
		t.Errorf("resolveName override = %q", got)
	}
	auto := resolveName("")
	if !ValidHostShape(auto) {
		t.Errorf("the automatic hostname %q is not a legal wire host", auto)
	}
	if len(auto) > 32 {
		t.Errorf("hostname too long: %d", len(auto))
	}
}

// ValidHostShape mirrors the server's [A-Za-z0-9._-]{1,32} check.
func ValidHostShape(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func TestSanitizeHostAlwaysProducesAValidName(t *testing.T) {
	inputs := []string{"", " ", "!!!", "a b/c\\d", strings.Repeat("é", 40),
		strings.Repeat("x", 100), "ok-host", "0", "-", "tab\tname"}
	for _, in := range inputs {
		got := SanitizeHost(in)
		if !ValidHostShape(got) {
			t.Errorf("SanitizeHost(%q) = %q, which the server would reject", in, got)
		}
	}
}

// --- payload and metrics -----------------------------------------------------

func TestBuildPayload(t *testing.T) {
	got := buildPayload("nas01", metrics{cpu: fp(12.5), temp: fp(54.25), load: fp(0.425), up: fp(86400)})
	want := "host=nas01 cpu=12.5 temp=54.2 load=0.42 up=86400"
	if got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
	// Missing metrics are omitted, not zero-filled: the first tick has no cpu.
	if got := buildPayload("h", metrics{}); got != "host=h" {
		t.Errorf("empty metrics payload = %q", got)
	}
	if got := buildPayload("h", metrics{temp: fp(50)}); got != "host=h temp=50.0" {
		t.Errorf("partial payload = %q", got)
	}
	if len(buildPayload(strings.Repeat("a", 32), metrics{cpu: fp(100), temp: fp(100), load: fp(100), up: fp(1e9)})) > 512 {
		t.Error("payload exceeded the 512-byte wire cap")
	}
	if !ValidHostShape(buildPayload("x", metrics{})[len("host="):]) {
		t.Error("payload does not start with a valid host")
	}
}

func TestCollectorOmitsTheFirstCPUSample(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if _, err := os.Stat("/proc/stat"); err != nil {
		t.Skip("no /proc/stat")
	}
	var c collector
	m := c.collect()
	if m.cpu != nil {
		t.Errorf("the first tick has no delta, so cpu must be omitted: %v", *m.cpu)
	}
	if m.up == nil || *m.up <= 0 {
		t.Error("uptime should be readable from /proc/uptime")
	}
	if m.load == nil || *m.load < 0 {
		t.Error("load should be readable from /proc/loadavg")
	}
	// Busy the CPU a little so the second sample has a real delta.
	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		_ = deadline.Sub(time.Now()).Nanoseconds()
	}
	time.Sleep(150 * time.Millisecond)
	m2 := c.collect()
	if m2.cpu == nil {
		t.Fatal("the second tick should report cpu")
	}
	if *m2.cpu < 0 || *m2.cpu > 100 {
		t.Errorf("cpu out of range: %v", *m2.cpu)
	}
}

func TestMetricReadersNeverPanicOnMissingFiles(t *testing.T) {
	// These read /proc and /sys directly; on a machine without them they must
	// report "not available", never crash.
	for _, fn := range []func() (float64, bool){readTemp, readLoad, readUptime} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("metric reader panicked: %v", r)
				}
			}()
			v, ok := fn()
			if ok && (v != v) {
				t.Error("a metric reader returned NaN")
			}
		}()
	}
	var c collector
	if v, ok := c.cpuPercent(); ok && (v < 0 || v > 100) {
		t.Errorf("cpuPercent out of range: %v", v)
	}
}

func TestReadTempIgnoresNonsense(t *testing.T) {
	dir := t.TempDir()
	zone := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	// parseTempFile is the unit under test; the glob-based reader is exercised
	// on real hardware by TestCollectorOmitsTheFirstCPUSample.
	for _, tc := range []struct {
		body string
		want float64
		ok   bool
	}{
		{"54200\n", 54.2, true},
		{"0\n", 0, true},
		{"-5000\n", -5, true},
		{"garbage\n", 0, false},
		{"999000\n", 0, false},  // 999C is not a temperature
		{"-200000\n", 0, false}, // -200C neither
		{"", 0, false},
	} {
		if err := os.WriteFile(filepath.Join(zone, "temp"), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := parseTempFile(filepath.Join(zone, "temp"))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseTempFile(%q) = %v,%v; want %v,%v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

// --- systemd unit ------------------------------------------------------------

func TestSystemdUnit(t *testing.T) {
	o := options{host: "192.168.1.1", port: 1717, name: "nas01",
		interval: 10 * time.Second, tcp: false}
	unit := systemdUnit(o)
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Restart=always", "WantedBy=multi-user.target",
		"ExecStart=", " -f ", "192.168.1.1:1717", "-name nas01",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit)
		}
	}
	// systemd does its own backgrounding, so the unit must use -f and must
	// never re-exec a child.
	if strings.Contains(unit, "-child") {
		t.Errorf("the unit must not use the hidden -child flag:\n%s", unit)
	}
	// The binary path must be absolute or systemd will refuse the unit.
	for _, line := range strings.Split(unit, "\n") {
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		bin := strings.Fields(strings.TrimPrefix(line, "ExecStart="))[0]
		if !filepath.IsAbs(bin) {
			t.Errorf("ExecStart must use an absolute path, got %q", bin)
		}
	}
	tcpUnit := systemdUnit(options{host: "h", port: 2000, interval: 5 * time.Second, tcp: true})
	if !strings.Contains(tcpUnit, "-tcp") || !strings.Contains(tcpUnit, "h:2000") {
		t.Errorf("tcp unit is wrong:\n%s", tcpUnit)
	}
}

func TestChildArgsRoundTrip(t *testing.T) {
	o := options{host: "10.0.0.9", port: 2222, name: "web-01",
		interval: 15 * time.Second, tcp: true}
	args := childArgs(o)
	joined := strings.Join(args, " ")
	for _, want := range []string{"-child", "-tcp", "-name web-01", "-port 2222", "-interval 15", "10.0.0.9"} {
		if !strings.Contains(joined, want) {
			t.Errorf("child args are missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "-f ") {
		t.Errorf("the child must not re-daemonise: %s", joined)
	}
	// The trailing positional must be the server, so the child parses it the
	// same way the parent did.
	if args[len(args)-1] != o.host {
		t.Errorf("the last argument should be the host: %v", args)
	}
}

// --- logging -----------------------------------------------------------------

func TestLoggerWritesAndRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.log")
	lg := newLogger(path, false)
	if lg.f == nil {
		t.Fatal("logger did not open its file")
	}
	lg.printf("hello %d", 1)
	lg.verbosef("this should not appear")
	lg.close()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello 1") {
		t.Errorf("log body = %q", body)
	}
	if strings.Contains(string(body), "this should not appear") {
		t.Error("verbosef logged while not verbose")
	}

	// Over the cap: the tail is kept, the head is dropped.
	big := strings.Repeat("x", (1<<20)+10)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rotateLog(f, 1<<20)
	f.Close()
	st, _ := os.Stat(path)
	if st.Size() > 1<<19+16 {
		t.Errorf("rotation left %d bytes", st.Size())
	}
	if st.Size() < 1<<18 {
		t.Errorf("rotation threw away too much: %d bytes", st.Size())
	}
}

func TestSelfPathIsAbsolute(t *testing.T) {
	p, err := selfPath()
	if err != nil {
		t.Skipf("cannot resolve the binary path: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("selfPath() = %q, want an absolute path", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("selfPath() points at nothing: %v", err)
	}
}

func TestPidfilePathFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := pidfilePath()
	want := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.pid", progName, os.Getuid()))
	if got != want {
		t.Errorf("pidfilePath() = %q, want %q", got, want)
	}
	// A non-existent XDG_RUNTIME_DIR must not be used blindly.
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent/runtime/dir")
	if got := pidfilePath(); got != want {
		t.Errorf("pidfilePath() = %q with a bogus XDG_RUNTIME_DIR, want %q", got, want)
	}
}

// --- keeping the resident set small ------------------------------------------

func TestThpAlways(t *testing.T) {
	cases := []struct {
		enabled string
		want    bool
	}{
		{"always [madvise] never\n", false},
		{"[always] madvise never\n", true},
		{"always madvise [never]\n", false},
		{"", false},
		{"nonsense", false},
	}
	for _, c := range cases {
		if got := thpAlways(c.enabled); got != c.want {
			t.Errorf("thpAlways(%q) = %v, want %v", c.enabled, got, c.want)
		}
	}
}

func TestWithGoDebug(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{"no env", nil, []string{"GODEBUG=disablethp=1"}},
		{"no GODEBUG", []string{"PATH=/bin"}, []string{"PATH=/bin", "GODEBUG=disablethp=1"}},
		{"appends to an existing value", []string{"GODEBUG=asyncpreemptoff=1"},
			[]string{"GODEBUG=asyncpreemptoff=1,disablethp=1"}},
		{"fills in an empty value", []string{"GODEBUG="}, []string{"GODEBUG=disablethp=1"}},
		{"an explicit disablethp=0 wins", []string{"GODEBUG=disablethp=0"}, []string{"GODEBUG=disablethp=0"}},
		{"already set", []string{"GODEBUG=disablethp=1"}, []string{"GODEBUG=disablethp=1"}},
		{"leaves other variables alone", []string{"HOME=/root", "GODEBUG=x=1", "TERM=xterm"},
			[]string{"HOME=/root", "GODEBUG=x=1,disablethp=1", "TERM=xterm"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := append([]string(nil), c.env...)
			got := withGoDebug(c.env, noHugePages)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("withGoDebug(%v) = %v, want %v", before, got, c.want)
			}
			if strings.Join(before, "|") != strings.Join(c.env, "|") {
				t.Errorf("withGoDebug mutated its argument: %v became %v", before, c.env)
			}
		})
	}
}

// TestTuneRuntimeIsConservative documents the two knobs the client turns and
// guards against someone raising them by accident: the reporter must never ask
// for more scheduler width than it has work for.
func TestTuneRuntimeIsConservative(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))
	tuneRuntime()
	if got := runtime.GOMAXPROCS(0); got != 1 {
		t.Errorf("GOMAXPROCS after tuneRuntime() = %d, want 1", got)
	}
}

// --- the orphan sweep --------------------------------------------------------

func TestIsBackgroundClient(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"detached child", []string{"/usr/local/bin/bonsai-client", "-child", "10.0.0.1"}, true},
		{"child with flags", []string{"bonsai-client", "-child", "-interval", "10", "-port", "1717", "h"}, true},
		{"relative path", []string{"./bonsai-client", "-child", "h"}, true},
		{"versioned name", []string{"bonsai-client-1.0.0", "-child", "h"}, true},
		{"foreground is systemd's, not ours", []string{"bonsai-client", "-f", "10.0.0.1"}, false},
		{"child that is also foreground", []string{"bonsai-client", "-child", "-f", "h"}, false},
		{"verbose foreground", []string{"bonsai-client", "-v", "h"}, false},
		{"parent, not yet detached", []string{"bonsai-client", "10.0.0.1"}, false},
		{"some other program", []string{"/bin/sh", "-c", "sleep 1"}, false},
		{"name only", []string{"bonsai-client"}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isBackgroundClient(c.argv); got != c.want {
				t.Errorf("isBackgroundClient(%q) = %v, want %v", c.argv, got, c.want)
			}
		})
	}
}

func TestSweepList(t *testing.T) {
	cases := []struct {
		name       string
		pidfilePID int
		found      []int
		except     int
		self       int
		want       []int
	}{
		{"pidfile first, then the scan", 100, []int{200, 300}, 0, 1, []int{100, 200, 300}},
		{"no pidfile entry", 0, []int{200}, 0, 1, []int{200}},
		{"nothing at all", 0, nil, 0, 1, nil},
		{"duplicates collapse", 200, []int{200, 300}, 0, 1, []int{200, 300}},
		{"the spare pid is skipped", 100, []int{200}, 200, 1, []int{100}},
		{"we never stop ourselves", 100, []int{7, 200}, 0, 7, []int{100, 200}},
		{"init and nonsense are skipped", 1, []int{0, -5, 1, 200}, 0, 999, []int{200}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sweepList(c.pidfilePID, c.found, c.except, c.self)
			if len(got) != len(c.want) {
				t.Fatalf("sweepList(%d, %v, %d, %d) = %v, want %v",
					c.pidfilePID, c.found, c.except, c.self, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("sweepList(%d, %v, %d, %d) = %v, want %v",
						c.pidfilePID, c.found, c.except, c.self, got, c.want)
				}
			}
		})
	}
}

func TestProcArgvAndSameUID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	argv := procArgv(os.Getpid())
	if len(argv) == 0 {
		t.Fatalf("procArgv(self) returned nothing")
	}
	if !strings.Contains(argv[0], "client.test") && !strings.Contains(argv[0], "go-build") {
		t.Logf("procArgv(self)[0] = %q", argv[0])
	}
	if !sameUID(os.Getpid(), os.Getuid()) {
		t.Errorf("sameUID(self, %d) = false, want true", os.Getuid())
	}
	if sameUID(os.Getpid(), os.Getuid()+4242) {
		t.Error("sameUID accepted a uid that is not ours")
	}
	if procArgv(deadPID(t)) != nil {
		t.Error("procArgv of a dead pid returned something")
	}
	if sameUID(deadPID(t), os.Getuid()) {
		t.Error("sameUID of a dead pid returned true")
	}
}

// spawnFakeClientArgs is spawnFakeClient with a chosen command line, so a test
// can put "-child" or "-f" where the real client would. Everything after "--"
// is ignored by the testing package's own flag parsing but still shows up in
// /proc/<pid>/cmdline, which is all the sweep cares about.
func spawnFakeClientArgs(t *testing.T, extra ...string) int {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot find the test binary: %v", err)
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, progName)
	in, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	if err := os.WriteFile(fake, in, 0o755); err != nil {
		t.Fatalf("copy: %v", err)
	}
	args := append([]string{"-test.run=TestHelperProcess", "--"}, extra...)
	cmd := exec.Command(fake, args...)
	cmd.Env = append(os.Environ(), "BONSAI_HELPER=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(procArgv(pid)) > 0 {
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper pid %d never appeared in /proc", pid)
	return 0
}

func TestFindBackgroundInstancesSpotsDetachedClientsOnly(t *testing.T) {
	child := spawnFakeClientArgs(t, "-child", "10.0.0.1")
	foreground := spawnFakeClientArgs(t, "-f", "10.0.0.1")

	found := findBackgroundInstances()
	has := func(pid int) bool {
		for _, p := range found {
			if p == pid {
				return true
			}
		}
		return false
	}
	if !has(child) {
		t.Errorf("findBackgroundInstances() = %v, missing the detached client %d", found, child)
	}
	if has(foreground) {
		t.Errorf("findBackgroundInstances() = %v, must not claim the foreground client %d (systemd owns it)", found, foreground)
	}
	if has(os.Getpid()) {
		t.Error("findBackgroundInstances() listed the test process itself")
	}
}

func TestStopAllInstancesSweepsOrphansAndSparesForeground(t *testing.T) {
	before := findBackgroundInstances()
	if len(before) > 0 {
		t.Skipf("the machine already has %d detached client(s); refusing to signal strangers", len(before))
	}
	child := spawnFakeClientArgs(t, "-child", "10.0.0.1")
	foreground := spawnFakeClientArgs(t, "-f", "10.0.0.1")

	stopped, failed := stopAllInstances(0)
	if failed != 0 {
		t.Errorf("stopAllInstances reported %d failures", failed)
	}
	if stopped < 1 {
		t.Errorf("stopAllInstances stopped %d processes, want at least 1", stopped)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && processAlive(child) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(child) {
		t.Errorf("the orphaned client %d is still alive after the sweep", child)
	}
	if !processAlive(foreground) {
		t.Errorf("the foreground client %d was killed; systemd-managed instances must be left alone", foreground)
	}
}
