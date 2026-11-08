package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// After the reply is written we swallow whatever else the client already had
// in flight, so closing sends FIN rather than RST. Without this, a client that
// piped in more than one line (`head -c 70000 /dev/urandom | nc host 1717`)
// can have the quote torn out of its receive buffer by the reset. Both bounds
// are small: this is politeness, not a service.
const (
	drainWindow = 250 * time.Millisecond
	drainCap    = 256 << 10
)

// Endpoints holds the two listeners that share one port.
type Endpoints struct {
	TCP net.Listener
	UDP net.PacketConn
}

// Listen binds TCP and UDP on the same address. Both are bound before anything
// touches the terminal, so "address already in use" is a clean one-line error.
func Listen(cfg *Config) (*Endpoints, error) {
	addr := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	tcpL, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	udpC, err := net.ListenPacket("udp", addr)
	if err != nil {
		tcpL.Close()
		return nil, err
	}
	return &Endpoints{TCP: tcpL, UDP: udpC}, nil
}

// Addr is the real bound address (useful when port 0 was requested).
func (e *Endpoints) Addr() string {
	if a := e.TCP.Addr(); a != nil {
		return a.String()
	}
	return ""
}

func (e *Endpoints) Close() {
	if e.TCP != nil {
		e.TCP.Close()
	}
	if e.UDP != nil {
		e.UDP.Close()
	}
}

// Node is the network half of the server: it owns the listeners, the rate
// limiter and the store, and knows nothing about rendering.
type Node struct {
	cfg    *Config
	store  *Store
	quotes *Quotes
	log    *log.Logger
	limit  *rateLimiter
	start  time.Time

	packets  counter
	rejected counter
}

type counter struct {
	mu sync.Mutex
	n  uint64
}

func (c *counter) inc() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

func (c *counter) get() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func NewNode(cfg *Config, store *Store, lg *log.Logger) *Node {
	return &Node{
		cfg:    cfg,
		store:  store,
		quotes: NewQuotes(DefaultQuotes),
		log:    lg,
		limit:  newRateLimiter(cfg.RatePerSecond),
		start:  time.Now(),
	}
}

// Uptime is how long the server has been listening.
func (n *Node) Uptime(now time.Time) time.Duration { return now.Sub(n.start) }

// Stats is a tiny window into the traffic, shown on the status line.
func (n *Node) Stats() (accepted, rejected uint64) {
	return n.packets.get(), n.rejected.get()
}

// Serve runs both listeners until ctx is cancelled, then closes them so the
// blocking reads return.
func (n *Node) Serve(ctx context.Context, e *Endpoints) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); n.serveTCP(ctx, e.TCP) }()
	go func() { defer wg.Done(); n.serveUDP(ctx, e.UDP) }()
	<-ctx.Done()
	e.Close()
	wg.Wait()
}

func (n *Node) serveTCP(ctx context.Context, l net.Listener) {
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.logf("tcp accept: %v", err)
			return
		}
		go n.handleTCP(conn)
	}
}

// handleTCP: accept, wait about a second for a payload line, parse it if one
// arrives, reply, close. A client that sends nothing gets a plain quote, which
// is exactly what RFC 865 asks for.
func (n *Node) handleTCP(c net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			n.logf("panic in tcp handler from %s: %v", safeRemote(c), r)
		}
		c.Close()
	}()

	timeout := secs(n.cfg.TCPTimeout)
	_ = c.SetDeadline(time.Now().Add(timeout))

	br := bufio.NewReaderSize(io.LimitReader(c, int64(n.cfg.MaxPayloadBytes)+1), n.cfg.MaxPayloadBytes+2)
	line, err := br.ReadString('\n')

	src := sourceIP(safeRemote(c))
	if err != nil && err != io.EOF && !isTimeout(err) {
		n.logf("tcp read from %s: %v", src, err)
	}

	now := time.Now()
	quote := n.quotes.Next()
	status := ""
	switch {
	case line == "":
		// Plain QOTD (e.g. `nc host 1717`): no metrics, just a quote.
		// Nothing is recorded, so the tree is undisturbed.
		n.limit.Allow(src, now)
	default:
		r, perr := ParsePayload(line, n.cfg.MaxPayloadBytes)
		if perr != nil {
			n.rejected.inc()
			n.logf("bad tcp payload from %s: %v", src, perr)
			n.limit.Allow(src, now)
			break
		}
		if !n.limit.Allow(src+"|"+r.Host, now) {
			n.rejected.inc()
			n.logf("rate limited tcp %s (%s)", src, r.Host)
			// Still answer the quote - QOTD stays honest - but do not
			// let a fast client rewrite the tree.
			break
		}
		n.packets.inc()
		r.ReceivedFrom = src
		n.store.Record(r, now)
		status = n.statusWord(r.Host, now)
	}

	out := BuildReply(quote, status, n.cfg.MaxPayloadBytes)
	_ = c.SetWriteDeadline(time.Now().Add(secs(n.cfg.TCPTimeout)))
	_, _ = c.Write(out)

	// Swallow whatever else the peer already had in flight before closing.
	// Closing a socket that still has unread data makes the kernel answer with
	// RST rather than FIN, and the peer can lose the quote we just wrote - the
	// difference between `nc host 1717` printing a proverb and printing
	// "connection reset by peer". The reply is already on the wire, so this is
	// pure politeness and is kept short and bounded.
	_ = c.SetReadDeadline(time.Now().Add(drainWindow))
	_, _ = io.Copy(io.Discard, io.LimitReader(c, drainCap))
}

// statusWord is the per-host health word folded into the QOTD reply, so a
// client can tell at a glance whether the server is happy with it.
func (n *Node) statusWord(host string, now time.Time) string {
	if host == "" {
		return ""
	}
	if h, ok := n.store.GradeHost(host, now); ok {
		return h.Band.String()
	}
	return ""
}

func (n *Node) serveUDP(ctx context.Context, pc net.PacketConn) {
	buf := make([]byte, n.cfg.MaxPayloadBytes+1)
	for {
		if ctx.Err() != nil {
			return
		}
		nread, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.logf("udp read: %v", err)
			return
		}
		n.handleDatagram(pc, addr, append([]byte(nil), buf[:nread]...))
	}
}

// handleDatagram parses one datagram and answers the sender. A panic here must
// not take the listener down.
func (n *Node) handleDatagram(pc net.PacketConn, addr net.Addr, data []byte) {
	defer func() {
		if r := recover(); r != nil {
			n.logf("panic in udp handler: %v", r)
		}
	}()

	src := sourceIP(addr.String())
	now := time.Now()

	r, err := ParsePayload(string(data), n.cfg.MaxPayloadBytes)
	if err != nil {
		// Garbage shares one per-IP bucket, so a flooder gets ~1 reply/s
		// and is never allowed to rewrite the tree.
		if !n.limit.Allow(src, now) {
			n.rejected.inc()
			return
		}
		n.rejected.inc()
		if !errors.Is(err, ErrEmpty) {
			n.logf("bad udp payload from %s: %v", src, err)
		}
		n.writeReply(pc, addr, n.quotes.Next(), "")
		return
	}
	if !n.limit.Allow(src+"|"+r.Host, now) {
		n.rejected.inc()
		return // silently ignored, per spec; also avoids a reflection amplifier
	}
	n.packets.inc()
	r.ReceivedFrom = src
	n.store.Record(r, now)
	n.writeReply(pc, addr, n.quotes.Next(), n.statusWord(r.Host, now))
}

func (n *Node) writeReply(pc net.PacketConn, addr net.Addr, quote, status string) {
	defer func() {
		if r := recover(); r != nil {
			n.logf("panic writing reply: %v", r)
		}
	}()
	_ = pc.SetWriteDeadline(time.Now().Add(secs(n.cfg.TCPTimeout)))
	_, _ = pc.WriteTo(BuildReply(quote, status, n.cfg.MaxPayloadBytes), addr)
}

func (n *Node) logf(format string, args ...any) {
	if n.log != nil {
		n.log.Printf(format, args...)
	}
}

func safeRemote(c net.Conn) string {
	if c == nil {
		return ""
	}
	if a := c.RemoteAddr(); a != nil {
		return a.String()
	}
	return ""
}

func sourceIP(hostport string) string {
	if hostport == "" {
		return "unknown"
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// --- per-source rate limiting -------------------------------------------------

type bucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a token bucket per key: about one payload per second with a
// small burst allowance. The map is bounded so a hostile LAN cannot grow it
// without limit.
//
// The key is the source IP for anything that did not parse, and "IP|host" for a
// valid report. Splitting it that way keeps a flooder to ~1 packet per second
// (all of its garbage shares one key) while still letting several real machines
// behind one NAT or a simulator on localhost report at their own pace.
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	perSec   float64
	maxBurst float64
	maxKeys  int
}

func newRateLimiter(perSec float64) *rateLimiter {
	if perSec <= 0 {
		perSec = 1
	}
	return &rateLimiter{
		buckets:  make(map[string]*bucket, 64),
		perSec:   perSec,
		maxBurst: 2,
		maxKeys:  4096,
	}
}

func (r *rateLimiter) Allow(ip string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, ok := r.buckets[ip]
	if !ok {
		if len(r.buckets) >= r.maxKeys {
			r.evictOldestLocked(now)
		}
		b = &bucket{tokens: r.maxBurst, last: now}
		r.buckets[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * r.perSec
		if b.tokens > r.maxBurst {
			b.tokens = r.maxBurst
		}
		b.last = now
	} else {
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (r *rateLimiter) evictOldestLocked(now time.Time) {
	var oldest string
	var oldestAge time.Duration
	for k, b := range r.buckets {
		if age := now.Sub(b.last); age > oldestAge {
			oldest, oldestAge = k, age
		}
	}
	delete(r.buckets, oldest)
}

// Len is used by tests.
func (r *rateLimiter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buckets)
}
