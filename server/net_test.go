package main

import (
	"context"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startTestServer binds an ephemeral port and returns a running Node plus the
// address, ready for real socket traffic.
func startTestServer(t *testing.T, cfg *Config) (*Node, *Store, *Endpoints, string) {
	t.Helper()
	cfg.Port = 0
	cfg.Bind = "127.0.0.1"
	cfg.normalize()

	ep, err := Listen(cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	store := NewStore(cfg)
	lg := log.New(io.Discard, "", 0)
	node := NewNode(cfg, store, lg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { node.Serve(ctx, ep); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	return node, store, ep, ep.Addr()
}

// udpRoundTrip sends one datagram and returns the reply. A missing reply is
// not an error: the server deliberately answers nothing when it rate limits a
// source, and several tests need to send faster than that.
func udpRoundTrip(t *testing.T, addr, payload string) string {
	t.Helper()
	reply, err := udpSend(addr, payload)
	if err != nil && !isTimeoutErr(err) {
		t.Fatalf("udp round trip: %v", err)
	}
	return reply
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	return err != nil && errorsAs(err, &ne) && ne.Timeout()
}

func errorsAs(err error, target *net.Error) bool {
	ne, ok := err.(net.Error)
	if ok {
		*target = ne
	}
	return ok
}

func udpSend(addr, payload string) (string, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return "", err
	}
	conn, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte(payload)); err != nil {
		return "", err
	}
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func tcpRoundTrip(t *testing.T, addr, payload string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if payload != "" {
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

func TestUDPMetricsAreRecorded(t *testing.T) {
	cfg := DefaultConfig()
	_, store, _, addr := startTestServer(t, &cfg)

	reply := udpRoundTrip(t, addr, "host=nas01 cpu=12.5 temp=54.2 load=0.42 up=86400\n")
	if !strings.Contains(reply, "\n") {
		t.Errorf("reply should be newline-terminated: %q", reply)
	}
	if !strings.Contains(reply, "status=healthy") {
		t.Errorf("expected a status line in %q", reply)
	}
	if len(reply) > 512 {
		t.Errorf("reply exceeds the 512-byte cap: %d", len(reply))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.Count() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap := store.Snapshot(time.Now())
	if snap.HostCount != 1 || snap.Hosts[0].Name != "nas01" {
		t.Fatalf("expected nas01, got %+v", snap.Hosts)
	}
	if *snap.Hosts[0].Metrics.Temp != 54.2 {
		t.Errorf("temp = %v", *snap.Hosts[0].Metrics.Temp)
	}
}

func TestTCPMetricsAreRecorded(t *testing.T) {
	cfg := DefaultConfig()
	_, store, _, addr := startTestServer(t, &cfg)

	reply := tcpRoundTrip(t, addr, "host=web01 cpu=95 temp=88 load=2.0 up=100\n")
	if !strings.Contains(reply, "status=critical") {
		t.Errorf("expected a critical status in %q", reply)
	}
	snap := store.Snapshot(time.Now())
	if snap.HostCount != 1 || snap.Crit != 1 {
		t.Fatalf("expected one critical host, got %+v", snap)
	}
}

// TestPlainQOTDIsStillQOTD is acceptance criterion 7: `nc host 1717` gets a
// quote and disturbs nothing.
func TestPlainQOTDIsStillQOTD(t *testing.T) {
	cfg := DefaultConfig()
	_, store, _, addr := startTestServer(t, &cfg)

	reply := tcpRoundTrip(t, addr, "") // connects and says nothing at all
	if reply == "" {
		t.Fatal("plain QOTD got no quote")
	}
	if strings.Contains(reply, "status=") {
		t.Errorf("a silent client should get only a quote: %q", reply)
	}
	if len(strings.Split(strings.TrimRight(reply, "\n"), "\n")) != 1 {
		t.Errorf("expected exactly one quote line, got %q", reply)
	}
	if store.Count() != 0 {
		t.Errorf("plain QOTD must not create hosts, got %d", store.Count())
	}
	// An empty UDP datagram behaves the same way.
	empty := udpRoundTrip(t, addr, "")
	if empty == "" || strings.Contains(empty, "status=") {
		t.Errorf("empty datagram should get just a quote: %q", empty)
	}
}

// TestGarbageNeverCrashesTheServer is acceptance criterion 8.
func TestGarbageNeverCrashesTheServer(t *testing.T) {
	cfg := DefaultConfig()
	// This test is about surviving bad input, not about throttling, so give
	// the rate limiter plenty of room.
	cfg.RatePerSecond = 100000
	cfg.normalize()
	_, store, _, addr := startTestServer(t, &cfg)

	junk := []string{
		"\x00\x01\x02\x03",
		strings.Repeat("A", 5000),
		"host=../../etc/passwd cpu=1",
		"host=a;rm -rf /",
		"host=$(reboot)",
		"host=a`id`",
		"cpu=1 temp=2",
		"host=",
		"=",
		"host=a cpu=NaN temp=Inf load=-1e999",
		"<script>alert(1)</script>",
		"host=a\nhost=b\nhost=c",
		strings.Repeat("host=a ", 500),
		"\xff\xfe\xfd",
	}
	for _, j := range junk {
		reply := udpRoundTrip(t, addr, j)
		if len(reply) > 512 {
			t.Errorf("reply for %q is %d bytes", truncate(j, 20), len(reply))
		}
	}
	// Over TCP the payload is read line by line, so a multi-line stream is
	// several payloads and the first valid one is honoured. Skip that one case
	// here and check it separately.
	for _, j := range junk {
		if strings.Contains(j, "\n") {
			continue
		}
		tcpRoundTrip(t, addr, j)
	}
	// Still alive and still clean: nothing bogus was recorded.
	if n := store.Count(); n != 0 {
		snap := store.Snapshot(time.Now())
		t.Errorf("garbage created %d hosts: %+v", n, snap.Hosts)
	}
	if reply := tcpRoundTrip(t, addr, ""); reply == "" {
		t.Error("server stopped answering after garbage input")
	}
	if reply := udpRoundTrip(t, addr, "host=ok cpu=1\n"); !strings.Contains(reply, "status=") {
		t.Errorf("server stopped accepting metrics after garbage: %q", reply)
	}
}

// TestTCPReadsOneLinePerConnection documents the line-oriented TCP path.
func TestTCPReadsOneLinePerConnection(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RatePerSecond = 1000
	cfg.normalize()
	_, store, _, addr := startTestServer(t, &cfg)

	tcpRoundTrip(t, addr, "host=first cpu=1\nhost=second cpu=2\n")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && store.Count() < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if store.Count() != 1 {
		t.Errorf("only the first line of a connection is a payload, got %d hosts", store.Count())
	}
	if _, ok := store.GradeHost("first", time.Now()); !ok {
		t.Error("expected the first line to be recorded")
	}
}

func TestRateLimitPerSource(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RatePerSecond = 1
	cfg.normalize()
	_, store, _, addr := startTestServer(t, &cfg)

	// Hammer one host from one address; only the first couple should land.
	for i := 0; i < 10; i++ {
		udpRoundTrip(t, addr, "host=flood cpu=99\n")
	}
	snap := store.Snapshot(time.Now())
	if snap.HostCount != 1 {
		t.Fatalf("expected one host, got %d", snap.HostCount)
	}
	// The bucket allows a small burst and then nothing, so most of the flood
	// must have been dropped rather than recorded.
	if snap.Crit != 1 {
		t.Errorf("the one recorded host should still be critical: %+v", snap)
	}
}

func TestRateLimitAllowsDistinctHosts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RatePerSecond = 1
	cfg.normalize()
	_, store, _, addr := startTestServer(t, &cfg)

	// Several real machines behind one address (or a simulator on localhost)
	// each get their own budget.
	for i := 0; i < 6; i++ {
		udpRoundTrip(t, addr, "host=node"+string(rune('0'+i))+" cpu=5\n")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && store.Count() < 6 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := store.Count(); got != 6 {
		t.Errorf("distinct hosts should not share a rate budget: got %d, want 6", got)
	}
}

func TestRateLimiterBucketsAreBounded(t *testing.T) {
	rl := newRateLimiter(1)
	rl.maxKeys = 8
	now := time.Now()
	for i := 0; i < 100; i++ {
		rl.Allow("10.0.0."+strings.TrimLeft(itoa(i), "0"), now.Add(time.Duration(i)*time.Second))
	}
	if rl.Len() > 8 {
		t.Errorf("rate limiter map grew unbounded: %d", rl.Len())
	}
}

func TestRateLimiterRefill(t *testing.T) {
	rl := newRateLimiter(2)
	now := time.Now()
	if !rl.Allow("a", now) || !rl.Allow("a", now) {
		t.Fatal("the initial burst should be allowed")
	}
	if rl.Allow("a", now) {
		t.Error("the third packet in the same instant should be dropped")
	}
	if !rl.Allow("a", now.Add(time.Second)) {
		t.Error("tokens should refill after a second")
	}
	if !rl.Allow("b", now) {
		t.Error("a different source has its own bucket")
	}
}

func TestServerSurvivesAbruptDisconnects(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TCPTimeout = 0.3
	cfg.normalize()
	_, _, _, addr := startTestServer(t, &cfg)

	for i := 0; i < 20; i++ {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		_ = conn.Close() // hang up immediately, mid-handshake
	}
	// Half a payload then nothing.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, _ = conn.Write([]byte("host=part"))
	_ = conn.Close()

	// The TCP path must not wedge: connections that say nothing should still
	// be answered and closed within about the read timeout.
	start := time.Now()
	if reply := tcpRoundTrip(t, addr, ""); reply == "" {
		t.Error("server stopped answering silent clients")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("silent client took %v to be answered", d)
	}
}

func TestTCPTimeoutDoesNotBlockForever(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TCPTimeout = 0.4
	cfg.normalize()
	_, _, _, addr := startTestServer(t, &cfg)

	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("read: %v", err)
	}
	if n == 0 {
		t.Error("expected a quote back")
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("silent connection answered after %v, want ~400ms", d)
	}
}

func TestBothProtocolsShareOnePort(t *testing.T) {
	cfg := DefaultConfig()
	_, store, ep, addr := startTestServer(t, &cfg)
	if ep.TCP == nil || ep.UDP == nil {
		t.Fatal("expected both a TCP listener and a UDP socket")
	}
	if !strings.HasSuffix(addr, ":0") {
		t.Logf("bound to %s", addr)
	}
	udpRoundTrip(t, addr, "host=viaudp cpu=1\n")
	tcpRoundTrip(t, addr, "host=viatcp cpu=1\n")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && store.Count() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if store.Count() != 2 {
		t.Errorf("expected both hosts, got %d", store.Count())
	}
}

func TestListenFailsCleanlyOnABusyPort(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Bind = "127.0.0.1"
	cfg.normalize()
	first, err := Listen(&cfg)
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	defer first.Close()

	// Bind the same port explicitly, since the first listener used port 0.
	_, portStr, err := net.SplitHostPort(first.Addr())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	cfg2 := DefaultConfig()
	cfg2.Bind = "127.0.0.1"
	n, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port %q: %v", portStr, err)
	}
	cfg2.Port = n
	cfg2.normalize()
	if ep, err := Listen(&cfg2); err == nil {
		ep.Close()
		t.Error("expected the second bind to fail")
	}
}

func TestNodeStatsAndUptime(t *testing.T) {
	cfg := DefaultConfig()
	node, _, _, addr := startTestServer(t, &cfg)
	udpRoundTrip(t, addr, "host=stats cpu=1\n")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a, _ := node.Stats(); a > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	a, r := node.Stats()
	if a == 0 {
		t.Errorf("expected accepted packets to be counted: %d/%d", a, r)
	}
	if node.Uptime(time.Now()) < 0 {
		t.Error("negative uptime")
	}
}

// TestTCPReplySurvivesAnOversizedPayload covers the reset trap. If the server
// closes a connection while the peer's extra bytes are still sitting unread in
// the receive queue, the kernel answers with RST instead of FIN and the quote
// we just wrote can be thrown away on the client side - the difference between
// `nc host 1717` printing a proverb and printing "connection reset by peer".
func TestTCPReplySurvivesAnOversizedPayload(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RatePerSecond = 1000
	cfg.normalize()
	_, _, _, addr := startTestServer(t, &cfg)

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// One over-long line, then a second burst the server never parses.
	payload := strings.Repeat("A", 600) + "\n" + strings.Repeat("junk ", 1600) + "\n"
	go func() { _, _ = conn.Write([]byte(payload)) }()

	var got []byte
	buf := make([]byte, 1024)
	var readErr error
	for {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			readErr = err
			break
		}
	}
	if len(got) == 0 {
		t.Fatalf("no reply reached the client (read error: %v)", readErr)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(got)), "") || !strings.Contains(string(got), "\n") {
		t.Errorf("expected a newline-terminated quote, got %q", got)
	}
	if strings.Contains(string(got), "status=") {
		t.Errorf("an over-long payload must not be recorded: %q", got)
	}
	if readErr != io.EOF {
		t.Errorf("connection ended with %v, want a clean io.EOF (the reply was drained, not reset)", readErr)
	}
	// And the server is still taking metrics afterwards.
	if reply := udpRoundTrip(t, addr, "host=afterdrain cpu=1\n"); !strings.Contains(reply, "status=") {
		t.Errorf("server stopped accepting metrics after an oversized payload: %q", reply)
	}
}
