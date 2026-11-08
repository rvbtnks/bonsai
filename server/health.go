package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Band is the colour/word state of a host.
type Band int

const (
	BandGood Band = iota
	BandWarn
	BandCrit
	BandStale
)

func (b Band) String() string {
	switch b {
	case BandGood:
		return "healthy"
	case BandWarn:
		return "warning"
	case BandCrit:
		return "critical"
	case BandStale:
		return "stale"
	}
	return "unknown"
}

// Short is the one-letter marker used in the overlay and in monochrome mode.
func (b Band) Short() string {
	switch b {
	case BandGood:
		return "ok"
	case BandWarn:
		return "warn"
	case BandCrit:
		return "CRIT"
	case BandStale:
		return "stale"
	}
	return "?"
}

// Report is one decoded client payload. Absent metrics are nil, and a nil
// metric contributes nothing to the score: the client omits what it cannot
// read rather than sending a zero.
type Report struct {
	Host            string
	CPU, Temp, Load *float64
	Up              *float64
	ReceivedFrom    string // source IP, for the overlay / logging
}

// F boxes a float so a Report literal stays readable.
func F(v float64) *float64 { return &v }

// HostHealth is a graded host, ready for display.
type HostHealth struct {
	Name     string
	Vitality float64
	Band     Band
	Stale    bool
	Age      time.Duration
	Offender string // e.g. "temp 84C" - the metric dragging this host down
	Metrics  Report
	LastSeen time.Time
}

// ScoreMetric grades one value linearly between its healthy and critical
// thresholds: 1.0 at or below healthy, 0.0 at or beyond critical.
func ScoreMetric(v, healthy, critical float64) float64 {
	if v != v { // NaN never scores well
		return 0
	}
	if critical <= healthy {
		if v <= healthy {
			return 1
		}
		return 0
	}
	if v <= healthy {
		return 1
	}
	if v >= critical {
		return 0
	}
	return (critical - v) / (critical - healthy)
}

// MetricBand is the raw per-metric colour: green below healthy, yellow between
// healthy and critical, red beyond critical.
func MetricBand(v, healthy, critical float64) Band {
	switch {
	case v <= healthy:
		return BandGood
	case v < critical:
		return BandWarn
	default:
		return BandCrit
	}
}

// BandOf maps a host's vitality to its band using the configured cutoffs.
func (c *Config) BandOf(v float64) Band {
	switch {
	case v >= c.HostGreen:
		return BandGood
	case v >= c.HostYellow:
		return BandWarn
	default:
		return BandCrit
	}
}

// grade turns one report into a HostHealth as of now.
//
// Host vitality is the minimum of its metric scores, so the worst metric
// defines the host. A recent reboot caps it, and staleness multiplies it down.
func (c *Config) grade(r Report, lastSeen, now time.Time) HostHealth {
	h := HostHealth{
		Name:     r.Host,
		Metrics:  r,
		LastSeen: lastSeen,
		Vitality: 1,
		Band:     BandGood,
		Age:      now.Sub(lastSeen),
	}
	if h.Age < 0 {
		h.Age = 0
	}

	best := 1.0
	offender := ""
	consider := func(label string, p *float64, th Threshold, unit string, prec int) {
		if p == nil {
			return
		}
		s := ScoreMetric(*p, th.Healthy, th.Critical)
		if s < best {
			best = s
			offender = fmt.Sprintf("%s %.*f%s", label, prec, *p, unit)
		}
	}
	consider("cpu", r.CPU, c.CPU, "%", 0)
	consider("temp", r.Temp, c.Temp, "C", 0)
	consider("load", r.Load, c.Load, "", 2)

	// Uptime is informational, except a fresh reboot caps vitality briefly.
	if r.Up != nil && *r.Up >= 0 && *r.Up < c.RebootWindow {
		if c.RebootCap < best {
			best = c.RebootCap
			offender = "rebooted " + ShortDuration(time.Duration(*r.Up*float64(time.Second)))
		}
	}

	// Staleness: fresh inside the grace window, then a linear decay to zero.
	ageSec := h.Age.Seconds()
	freshSec := c.FreshWindowSeconds()
	factor := 1.0
	if ageSec > freshSec {
		t := (ageSec - freshSec) / c.DecayWindow
		if t >= 1 {
			t = 1
		}
		factor = 1 - t
	}

	if factor <= 0 {
		h.Vitality = 0
		h.Stale = true
		h.Band = BandStale
		if offender == "" {
			h.Offender = "silent " + ShortDuration(h.Age)
		} else {
			h.Offender = offender
		}
		return h
	}

	h.Vitality = clamp01(best * factor)
	h.Band = c.BandOf(h.Vitality)
	h.Offender = offender
	if h.Offender == "" && h.Vitality < 1 {
		// Capped by staleness alone.
		h.Offender = "fading " + ShortDuration(h.Age)
	}
	return h
}

// NetworkVitality is the number that drives the tree:
//
//	network = meanWeight * mean(hosts) + minWeight * min(hosts)
//
// The mean makes every machine contribute; the min term stops one dying host
// from being averaged away. Zero hosts gives the neutral seedling value.
func NetworkVitality(c *Config, vitalities []float64) float64 {
	if len(vitalities) == 0 {
		return clamp01(c.EmptyVitality)
	}
	sum := 0.0
	min := 1.0
	for _, v := range vitalities {
		v = clamp01(v)
		sum += v
		if v < min {
			min = v
		}
	}
	mean := sum / float64(len(vitalities))
	return clamp01(c.MeanWeight*mean + c.MinWeight*min)
}

// Snapshot is an immutable view of the world, taken under the lock and then
// handed to the renderer so drawing never blocks the network.
type Snapshot struct {
	Hosts     []HostHealth // sorted by name
	Network   float64
	Word      string
	HostCount int
	Good      int
	Warn      int
	Crit      int
	Stale     int
	Taken     time.Time
}

// StaleCount reports how many hosts need pruning (critical) or have gone away.
func (s Snapshot) PruneTargets() []HostHealth {
	out := make([]HostHealth, 0, 4)
	for _, h := range s.Hosts {
		if h.Band == BandCrit || h.Band == BandStale {
			out = append(out, h)
		}
	}
	return out
}

// HostState is the mutable record kept per hostname.
type HostState struct {
	Report   Report
	LastSeen time.Time
}

// Store is the mutex-guarded map of everything the server knows.
type Store struct {
	mu    sync.Mutex
	cfg   *Config
	hosts map[string]*HostState
}

func NewStore(cfg *Config) *Store {
	return &Store{cfg: cfg, hosts: make(map[string]*HostState, cfg.MaxHosts)}
}

// Record stores a validated report. Unknown hosts are added; when the map is
// full the oldest-seen host is evicted to make room.
func (s *Store) Record(r Report, now time.Time) {
	if r.Host == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if h, ok := s.hosts[r.Host]; ok {
		h.Report = r
		h.LastSeen = now
		return
	}
	for len(s.hosts) >= s.cfg.MaxHosts {
		if !s.evictOldestLocked() {
			break
		}
	}
	s.hosts[r.Host] = &HostState{Report: r, LastSeen: now}
}

func (s *Store) evictOldestLocked() bool {
	var oldest string
	var oldestTime time.Time
	found := false
	for name, h := range s.hosts {
		if !found || h.LastSeen.Before(oldestTime) {
			oldest, oldestTime, found = name, h.LastSeen, true
		}
	}
	if !found {
		return false
	}
	delete(s.hosts, oldest)
	return true
}

// GradeHost grades a single host, used to build the QOTD reply's status line.
func (s *Store) GradeHost(name string, now time.Time) (HostHealth, bool) {
	s.mu.Lock()
	h, ok := s.hosts[name]
	if !ok {
		s.mu.Unlock()
		return HostHealth{}, false
	}
	rep, seen := h.Report, h.LastSeen
	s.mu.Unlock()
	return s.cfg.grade(rep, seen, now), true
}

// Snapshot grades every host, drops the ones that have been gone for over
// EvictAfter (they regrow if they come back) and aggregates the network.
func (s *Store) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	type raw struct {
		rep  Report
		seen time.Time
	}
	items := make(map[string]raw, len(s.hosts))
	var expired []string
	for name, h := range s.hosts {
		if now.Sub(h.LastSeen) > secs(s.cfg.EvictAfter) {
			expired = append(expired, name)
			continue
		}
		items[name] = raw{rep: h.Report, seen: h.LastSeen}
	}
	for _, name := range expired {
		delete(s.hosts, name)
	}
	s.mu.Unlock()

	snap := Snapshot{Taken: now, Hosts: make([]HostHealth, 0, len(items))}
	vit := make([]float64, 0, len(items))
	for _, it := range items {
		h := s.cfg.grade(it.rep, it.seen, now)
		snap.Hosts = append(snap.Hosts, h)
		vit = append(vit, h.Vitality)
		switch h.Band {
		case BandGood:
			snap.Good++
		case BandWarn:
			snap.Warn++
		case BandCrit:
			snap.Crit++
		case BandStale:
			snap.Stale++
		}
	}
	sort.Slice(snap.Hosts, func(i, j int) bool { return snap.Hosts[i].Name < snap.Hosts[j].Name })
	snap.HostCount = len(snap.Hosts)
	snap.Network = NetworkVitality(s.cfg, vit)
	snap.Word = s.cfg.Word(snap.Network)
	return snap
}

// Count is used by tests and the status line.
func (s *Store) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hosts)
}

// ShortDuration formats an uptime compactly: 45s, 12m, 3h04m, 5d02h.
func ShortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	default:
		return fmt.Sprintf("%dd%02dh", s/86400, (s%86400)/3600)
	}
}
