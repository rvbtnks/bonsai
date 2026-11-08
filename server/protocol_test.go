package main

import (
	"strings"
	"testing"
)

func TestParsePayloadHappyPath(t *testing.T) {
	r, err := ParsePayload("host=nas01 cpu=12.5 temp=54.2 load=0.42 up=86400\n", 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Host != "nas01" {
		t.Errorf("host = %q", r.Host)
	}
	if r.CPU == nil || *r.CPU != 12.5 {
		t.Errorf("cpu = %v", r.CPU)
	}
	if r.Temp == nil || *r.Temp != 54.2 {
		t.Errorf("temp = %v", r.Temp)
	}
	if r.Load == nil || *r.Load != 0.42 {
		t.Errorf("load = %v", r.Load)
	}
	if r.Up == nil || *r.Up != 86400 {
		t.Errorf("up = %v", r.Up)
	}
}

func TestParsePayloadOptionalAndUnknownKeys(t *testing.T) {
	r, err := ParsePayload("host=web01 future_key=1 nonsense=xyz", 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Host != "web01" || r.CPU != nil || r.Temp != nil || r.Load != nil || r.Up != nil {
		t.Errorf("expected only host to be set, got %+v", r)
	}
}

func TestParsePayloadHostCharset(t *testing.T) {
	// The spec's charset is [A-Za-z0-9._-]{1,32}; nothing else is special-cased.
	// Names are only ever used as map keys and display text, never as paths.
	good := []string{"a", "A9", "nas-01", "web_02", "a.b.c", "..", strings.Repeat("x", 32)}
	for _, h := range good {
		if _, err := ParsePayload("host="+h, 512); err != nil {
			t.Errorf("host %q should be valid: %v", h, err)
		}
	}
	bad := []string{
		"", " ", strings.Repeat("x", 33), "a b", "a;b", "a|b", "a`b", "a$b",
		"a&b", "$(rm -rf /)", "; rm -rf /", "a\nb", "héllo", "a=b",
		"a/b", `a\b`, "a\"b", "a'b", "a<b>", "../../../etc/passwd",
	}
	for _, h := range bad {
		if _, err := ParsePayload("host="+h, 512); err == nil {
			t.Errorf("host %q should be rejected", h)
		}
	}
}

func TestParsePayloadGarbageNeverPanics(t *testing.T) {
	inputs := []string{
		"", " ", "\n", "\r\n", "=", "===", "host", "host=", "=cpu", "cpu=5",
		"host=a cpu=", "host=a cpu=nan", "host=a cpu=inf", "host=a cpu=-inf",
		"host=a cpu=1e400", "host=a cpu=0x10", "host=a cpu=1,5",
		strings.Repeat("a", 4096), strings.Repeat("host=a ", 500),
		strings.Repeat("\x00", 100), "\xff\xfe\x00\x01", "host=a\x00cpu=1",
		"host=" + strings.Repeat("é", 200), "HOST=a", "Host=a", " host=a",
		"host=a  cpu=1   temp=2", "host=a\tcpu=1",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on input %q: %v", in, r)
				}
			}()
			_, _ = ParsePayload(in, 512)
		}()
	}
}

func TestParsePayloadOversize(t *testing.T) {
	long := "host=valid cpu=1 " + strings.Repeat("x", 600)
	if _, err := ParsePayload(long, 512); err != ErrOversize {
		t.Errorf("expected ErrOversize, got %v", err)
	}
	exact := "host=valid" + strings.Repeat(" ", 512-len("host=valid"))
	if _, err := ParsePayload(exact, 512); err != nil {
		t.Errorf("a payload exactly at the cap should parse: %v", err)
	}
	oneOver := "host=valid" + strings.Repeat(" ", 513-len("host=valid"))
	if _, err := ParsePayload(oneOver, 512); err != ErrOversize {
		t.Errorf("expected ErrOversize one byte over the cap, got %v", err)
	}
}

func TestParsePayloadRejectsNonFiniteNumbers(t *testing.T) {
	for _, in := range []string{
		"host=a cpu=NaN", "host=a cpu=nan", "host=a cpu=Inf", "host=a cpu=+Inf",
		"host=a cpu=-Inf", "host=a temp=1e999", "host=a load=NaN",
	} {
		if _, err := ParsePayload(in, 512); err == nil {
			t.Errorf("%q should be rejected", in)
		}
	}
}

func TestParsePayloadClampsNumbers(t *testing.T) {
	r, err := ParsePayload("host=a cpu=9999 temp=-9999 load=1e9 up=-5", 512)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *r.CPU != 100 {
		t.Errorf("cpu should clamp to 100, got %v", *r.CPU)
	}
	if *r.Temp != -100 {
		t.Errorf("temp should clamp to -100, got %v", *r.Temp)
	}
	if *r.Load != 10000 {
		t.Errorf("load should clamp to 10000, got %v", *r.Load)
	}
	if *r.Up != 0 {
		t.Errorf("up should clamp to 0, got %v", *r.Up)
	}
}

func TestParsePayloadRejectsControlBytesAndBadNumbers(t *testing.T) {
	for _, in := range []string{"host=a\x01", "host=a cpu=abc", "host=a cpu=1.2.3", "host=a up=xyz"} {
		if _, err := ParsePayload(in, 512); err == nil {
			t.Errorf("expected an error for %q", in)
		}
	}
}

func TestSanitizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nas01", "nas01"},
		{"my host!", "my_host_"},
		{"héllo", "h_llo"},
		{"", "unknown"},
		{"   ", "unknown"},
		{strings.Repeat("a", 50), strings.Repeat("a", 32)},
		{"a/b\\c", "a_b_c"},
		{"web-01.example.com", "web-01.example.com"},
	}
	for _, c := range cases {
		if got := SanitizeHost(c.in); got != c.want {
			t.Errorf("SanitizeHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := SanitizeHost("x"); !ValidHost(got) {
		t.Errorf("SanitizeHost produced an invalid host %q", got)
	}
}

func TestQuoteRotation(t *testing.T) {
	q := NewQuotes(nil)
	if q.Len() != len(DefaultQuotes) {
		t.Fatalf("expected the built-in quotes, got %d", q.Len())
	}
	seen := map[string]bool{}
	for i := 0; i < q.Len(); i++ {
		line := q.Next()
		if line == "" {
			t.Fatal("empty quote")
		}
		if seen[line] {
			t.Errorf("quote repeated before the list wrapped: %q", line)
		}
		seen[line] = true
	}
	if q.Next() == "" {
		t.Error("rotation broke after wrapping")
	}
	if len(DefaultQuotes) < 8 {
		t.Errorf("want a decent rotation list, got %d quotes", len(DefaultQuotes))
	}
}

func TestQuoteRotationIsConcurrentSafe(t *testing.T) {
	q := NewQuotes(nil)
	done := make(chan string, 64)
	for i := 0; i < 64; i++ {
		go func() { done <- q.Next() }()
	}
	for i := 0; i < 64; i++ {
		if <-done == "" {
			t.Fatal("empty quote under concurrency")
		}
	}
}

func TestBuildReplyStaysUnderCap(t *testing.T) {
	quote := strings.Repeat("é", 400) // multi-byte: truncation must not split a rune
	for _, status := range []string{"", "healthy", strings.Repeat("x", 900)} {
		out := BuildReply(quote, status, 512)
		if len(out) > 512 {
			t.Errorf("reply is %d bytes with status %q", len(out), status)
		}
		if out[len(out)-1] != '\n' {
			t.Error("reply must be newline-terminated")
		}
		if !utf8Valid(string(out)) {
			t.Errorf("reply is not valid utf-8 with status %q", status)
		}
	}
	if got := string(BuildReply("hello", "healthy", 512)); got != "hello\nstatus=healthy\n" {
		t.Errorf("unexpected reply %q", got)
	}
	if got := string(BuildReply("hello", "", 512)); got != "hello\n" {
		t.Errorf("unexpected reply %q", got)
	}
}

func TestBuildReplyTinyCap(t *testing.T) {
	for _, limit := range []int{1, 2, 5, 9} {
		out := BuildReply("a rather long quote", "healthy", limit)
		if len(out) > limit {
			t.Errorf("cap %d produced %d bytes", limit, len(out))
		}
	}
}

func TestTruncateKeepsRunesWhole(t *testing.T) {
	s := "aé日b"
	for n := 0; n <= len(s); n++ {
		if !utf8Valid(truncate(s, n)) {
			t.Errorf("truncate(%q, %d) split a rune", s, n)
		}
	}
	if truncate(s, 100) != s {
		t.Error("truncate grew or changed a short string")
	}
}

func utf8Valid(s string) bool {
	for i := 0; i < len(s); {
		if s[i] < 0x80 {
			i++
			continue
		}
		n := 0
		switch {
		case s[i]&0xE0 == 0xC0:
			n = 2
		case s[i]&0xF0 == 0xE0:
			n = 3
		case s[i]&0xF8 == 0xF0:
			n = 4
		default:
			return false
		}
		if i+n > len(s) {
			return false
		}
		for k := 1; k < n; k++ {
			if s[i+k]&0xC0 != 0x80 {
				return false
			}
		}
		i += n
	}
	return true
}
