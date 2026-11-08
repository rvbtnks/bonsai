package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// The wire format is RFC 865 (QOTD) with a twist: the server *reads* the
// client's line as metrics and answers with a quote, so a plain `nc host 1717`
// still behaves exactly like QOTD.
//
//	client -> server:  host=nas01 cpu=12.5 temp=54.2 load=0.42 up=86400\n
//	server -> client:  <a bonsai proverb>\nstatus=healthy\n

const (
	// DefaultPort is RFC 865's port 17 shifted up so no root is needed.
	DefaultPort = 1717
	// MaxPayload is the hard cap on both payloads and replies.
	MaxPayload = 512
	// maxTokens bounds how much work one line can cause.
	maxTokens = 64
)

var (
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)
	keyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,15}$`)
)

var (
	ErrEmpty    = errors.New("empty payload")
	ErrOversize = errors.New("payload exceeds 512 bytes")
	ErrNoHost   = errors.New("missing or invalid host")
)

// ValidHost reports whether a hostname is acceptable on the wire.
func ValidHost(s string) bool { return hostPattern.MatchString(s) }

// SanitizeHost is the client-side cleaner: keep the allowed charset, cap at 32.
// The server does not need it (ParsePayload validates instead) but the two
// copies must agree, and the test suite uses this one to prove that whatever
// the client invents is something the server will accept.
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

// ParsePayload decodes one client line. It is strict: anything it does not
// understand is rejected, and the caller answers with a plain quote.
func ParsePayload(line string, maxBytes int) (Report, error) {
	if maxBytes <= 0 {
		maxBytes = MaxPayload
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) > maxBytes {
		return Report{}, ErrOversize
	}
	if strings.TrimSpace(line) == "" {
		return Report{}, ErrEmpty
	}
	for i := 0; i < len(line); i++ {
		if c := line[i]; c < 0x20 || c == 0x7f {
			return Report{}, fmt.Errorf("control byte 0x%02x at offset %d", c, i)
		}
	}
	if !utf8.ValidString(line) {
		return Report{}, errors.New("invalid utf-8")
	}

	var r Report
	seenHost := false
	for i, tok := range strings.Fields(line) {
		if i >= maxTokens {
			return Report{}, errors.New("too many tokens")
		}
		eq := strings.IndexByte(tok, '=')
		if eq <= 0 {
			return Report{}, fmt.Errorf("malformed token %q", truncate(tok, 32))
		}
		key, val := tok[:eq], tok[eq+1:]
		if !keyPattern.MatchString(key) {
			return Report{}, fmt.Errorf("malformed key %q", truncate(key, 32))
		}
		switch key {
		case "host":
			if !ValidHost(val) {
				return Report{}, fmt.Errorf("invalid host %q", truncate(val, 40))
			}
			r.Host = val
			seenHost = true
		case "cpu":
			v, err := numField(val, 0, 100)
			if err != nil {
				return Report{}, err
			}
			r.CPU = F(v)
		case "temp":
			v, err := numField(val, -100, 250)
			if err != nil {
				return Report{}, err
			}
			r.Temp = F(v)
		case "load":
			v, err := numField(val, 0, 10000)
			if err != nil {
				return Report{}, err
			}
			r.Load = F(v)
		case "up":
			v, err := numField(val, 0, 315360000) // 10 years
			if err != nil {
				return Report{}, err
			}
			r.Up = F(v)
		default:
			// Unknown keys are ignored, per spec.
		}
	}
	if !seenHost {
		return Report{}, ErrNoHost
	}
	return r, nil
}

// numField parses with strconv only. NaN and the infinities parse successfully
// but mean nothing here, so they are rejected rather than clamped into a value
// that would silently look like a real reading. Everything finite is clamped
// into a sane range.
func numField(val string, lo, hi float64) (float64, error) {
	if val == "" {
		return 0, errors.New("empty numeric field")
	}
	v, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, fmt.Errorf("bad number %q", truncate(val, 32))
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("non-finite number %q", truncate(val, 32))
	}
	return clampF(v, lo, hi), nil
}

// BuildReply assembles the QOTD answer: a quote, optionally followed by a
// status line, always newline-terminated and always under maxBytes.
func BuildReply(quote, status string, maxBytes int) []byte {
	if maxBytes <= 0 {
		maxBytes = MaxPayload
	}
	out := quote
	if status != "" {
		trailer := "\nstatus=" + status
		room := maxBytes - len(trailer) - 1 // -1 for the final newline
		if room < 0 {
			return append([]byte(truncate(quote, maxBytes-1)), '\n')
		}
		out = truncate(quote, room) + trailer
	}
	out = truncate(out, maxBytes-1)
	return append([]byte(out), '\n')
}

// truncate shortens s to at most n bytes without splitting a rune.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Quotes rotates through the built-in proverbs. It is safe for concurrent use
// and never repeats until the list wraps.
type Quotes struct {
	list []string
	i    atomic.Uint64
}

// DefaultQuotes are the bonsai-themed lines the server hands back. Anyone
// speaking plain QOTD gets one of these, which is the honest part of the
// protocol double-duty.
var DefaultQuotes = []string{
	"The patient gardener outlives the storm.",
	"A small tree, deeply rooted, outlasts a tall one.",
	"Prune what is dying so the rest may live.",
	"Every branch remembers the hand that shaped it.",
	"Stillness is not idleness; the roots are working.",
	"A bonsai is a forest that agreed to fit in a pot.",
	"Water the root, not the reputation.",
	"The tree that bends in wind does not argue with it.",
	"Growth is slow on purpose.",
	"Cut back the strong so the weak may catch up.",
	"A healthy tree needs no explanation.",
	"Wiring shapes the branch; time sets it.",
	"Do not mistake a quiet canopy for a dead one.",
	"The pot holds the tree; the tree holds the eye.",
	"Moss grows where nothing is hurried.",
	"One brown leaf tells you more than ten green ones.",
	"Repot in spring, prune in patience.",
	"What the roots cannot reach, the crown will not carry.",
}

func NewQuotes(list []string) *Quotes {
	if len(list) == 0 {
		list = DefaultQuotes
	}
	return &Quotes{list: list}
}

func (q *Quotes) Next() string {
	n := q.i.Add(1)
	return q.list[(n-1)%uint64(len(q.list))]
}

func (q *Quotes) Len() int { return len(q.list) }
