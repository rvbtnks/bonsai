package main

import (
	"math"
	"testing"
	"time"
)

func TestScoreMetric(t *testing.T) {
	cases := []struct {
		v, healthy, critical, want float64
	}{
		{0, 70, 90, 1},
		{50, 70, 90, 1},
		{70, 70, 90, 1},   // at healthy is still perfect
		{80, 70, 90, 0.5}, // halfway
		{75, 70, 90, 0.75},
		{85, 70, 90, 0.25},
		{90, 70, 90, 0}, // at critical is zero
		{100, 70, 90, 0},
		{-5, 70, 90, 1},
		{65, 65, 80, 1},
		{72.5, 65, 80, 0.5},
		{80, 65, 80, 0},
		{0.8, 0.8, 1.5, 1},
		{1.15, 0.8, 1.5, 0.5},
		{1.5, 0.8, 1.5, 0},
	}
	for _, c := range cases {
		if got := ScoreMetric(c.v, c.healthy, c.critical); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("ScoreMetric(%v, %v, %v) = %v, want %v", c.v, c.healthy, c.critical, got, c.want)
		}
	}
	// A degenerate threshold must not divide by zero.
	if got := ScoreMetric(5, 10, 10); got != 1 {
		t.Errorf("equal thresholds below healthy: got %v", got)
	}
	if got := ScoreMetric(15, 10, 10); got != 0 {
		t.Errorf("equal thresholds above healthy: got %v", got)
	}
	if got := ScoreMetric(math.NaN(), 70, 90); got != 0 {
		t.Errorf("NaN should score 0, got %v", got)
	}
}

func TestScoreMetricIsMonotonic(t *testing.T) {
	prev := 1.0
	for v := 60.0; v <= 100; v += 0.5 {
		got := ScoreMetric(v, 70, 90)
		if got > prev+1e-12 {
			t.Fatalf("score increased with the metric at %v: %v > %v", v, got, prev)
		}
		if got < 0 || got > 1 {
			t.Fatalf("score out of range at %v: %v", v, got)
		}
		prev = got
	}
}

func TestMetricBand(t *testing.T) {
	if b := MetricBand(69, 70, 90); b != BandGood {
		t.Errorf("69 should be good, got %v", b)
	}
	if b := MetricBand(70, 70, 90); b != BandGood {
		t.Errorf("70 (at healthy) should be good, got %v", b)
	}
	if b := MetricBand(80, 70, 90); b != BandWarn {
		t.Errorf("80 should warn, got %v", b)
	}
	if b := MetricBand(90, 70, 90); b != BandCrit {
		t.Errorf("90 (at critical) should be critical, got %v", b)
	}
	if b := MetricBand(99, 70, 90); b != BandCrit {
		t.Errorf("99 should be critical, got %v", b)
	}
}

func TestHostVitalityIsTheWorstMetric(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()

	r := Report{Host: "a", CPU: F(10), Temp: F(85), Load: F(0.1)} // only temp is bad
	h := cfg.grade(r, now, now)
	if math.Abs(h.Vitality-0) > 1e-9 {
		t.Errorf("a critical temp should zero the host, got %v", h.Vitality)
	}
	if h.Band != BandCrit {
		t.Errorf("expected BandCrit, got %v", h.Band)
	}
	if h.Offender != "temp 85C" {
		t.Errorf("offender = %q, want %q", h.Offender, "temp 85C")
	}

	r = Report{Host: "a", CPU: F(80), Temp: F(72.5), Load: F(1.15)}
	h = cfg.grade(r, now, now)
	// cpu 0.5, temp 0.5, load 0.5 -> all equal, min is 0.5
	if math.Abs(h.Vitality-0.5) > 1e-9 {
		t.Errorf("expected 0.5, got %v", h.Vitality)
	}

	r = Report{Host: "a", CPU: F(10), Temp: F(50), Load: F(0.1)}
	h = cfg.grade(r, now, now)
	if h.Vitality != 1 || h.Band != BandGood || h.Offender != "" {
		t.Errorf("an all-healthy host should be 1.0/good with no offender, got %v %v %q",
			h.Vitality, h.Band, h.Offender)
	}
}

func TestHostVitalityIgnoresMissingMetrics(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()
	h := cfg.grade(Report{Host: "a", CPU: F(95)}, now, now)
	if h.Vitality != 0 {
		t.Errorf("only cpu reported and it is critical: got %v", h.Vitality)
	}
	h = cfg.grade(Report{Host: "a"}, now, now)
	if h.Vitality != 1 {
		t.Errorf("a host with no metrics should not be penalised: got %v", h.Vitality)
	}
}

func TestRecentRebootCapsVitality(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()

	h := cfg.grade(Report{Host: "a", CPU: F(5), Up: F(60)}, now, now)
	if math.Abs(h.Vitality-cfg.RebootCap) > 1e-9 {
		t.Errorf("a 60s-old boot should cap at %v, got %v", cfg.RebootCap, h.Vitality)
	}
	if h.Offender == "" {
		t.Error("a reboot cap should name itself as the offender")
	}

	h = cfg.grade(Report{Host: "a", CPU: F(5), Up: F(cfg.RebootWindow + 1)}, now, now)
	if h.Vitality != 1 {
		t.Errorf("outside the reboot window vitality should be full, got %v", h.Vitality)
	}

	// A bad metric still wins over the reboot cap.
	h = cfg.grade(Report{Host: "a", CPU: F(99), Up: F(10)}, now, now)
	if h.Vitality != 0 {
		t.Errorf("critical cpu should beat the reboot cap, got %v", h.Vitality)
	}
}

func TestStalenessDecay(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()
	fresh := cfg.FreshWindowSeconds()

	seen := func(ageSec float64) HostHealth {
		return cfg.grade(Report{Host: "a", CPU: F(5)}, now.Add(-time.Duration(ageSec*float64(time.Second))), now)
	}

	if h := seen(fresh - 1); h.Vitality != 1 || h.Stale {
		t.Errorf("inside the fresh window: %v stale=%v", h.Vitality, h.Stale)
	}
	// Halfway through the decay window: half vitality.
	half := seen(fresh + cfg.DecayWindow/2)
	if math.Abs(half.Vitality-0.5) > 1e-9 {
		t.Errorf("mid-decay vitality = %v, want 0.5", half.Vitality)
	}
	if half.Stale {
		t.Error("mid-decay must not be stale yet")
	}
	// Past the window: stale, brown and bare.
	gone := seen(fresh + cfg.DecayWindow + 1)
	if !gone.Stale || gone.Vitality != 0 || gone.Band != BandStale {
		t.Errorf("expected a stale host, got %v %v %v", gone.Stale, gone.Vitality, gone.Band)
	}
	if gone.Offender == "" {
		t.Error("a stale host should say how long it has been silent")
	}
}

func TestStalenessMultipliesTheRealScore(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()
	age := cfg.FreshWindowSeconds() + cfg.DecayWindow/2 // factor 0.5
	h := cfg.grade(Report{Host: "a", CPU: F(80)},       // score 0.5
		now.Add(-time.Duration(age*float64(time.Second))), now)
	if math.Abs(h.Vitality-0.25) > 1e-9 {
		t.Errorf("half-faded and half-loaded should be 0.25, got %v", h.Vitality)
	}
}

func TestNetworkVitalityFormula(t *testing.T) {
	cfg := DefaultConfig()

	if got := NetworkVitality(&cfg, nil); got != cfg.EmptyVitality {
		t.Errorf("no hosts should give the neutral %v, got %v", cfg.EmptyVitality, got)
	}
	if got := NetworkVitality(&cfg, []float64{1}); got != 1 {
		t.Errorf("one healthy host should give 1.0, got %v", got)
	}
	if got := NetworkVitality(&cfg, []float64{0.5, 0.5}); got != 0.5 {
		t.Errorf("expected 0.5, got %v", got)
	}

	// The documented formula, checked by hand.
	vs := []float64{1, 1, 1, 0}
	want := 0.6*0.75 + 0.4*0.0 // mean 0.75, min 0
	if got := NetworkVitality(&cfg, vs); math.Abs(got-want) > 1e-9 {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestOneCriticalHostPullsTheTreeDown(t *testing.T) {
	cfg := DefaultConfig()
	healthy := []float64{1, 1, 1, 1, 1, 1, 1, 1}
	withOneBad := append([]float64{0}, healthy...)

	a := NetworkVitality(&cfg, healthy)
	b := NetworkVitality(&cfg, withOneBad)
	if b >= a {
		t.Fatalf("one dead host must lower network vitality: %v -> %v", a, b)
	}
	// ...and by enough to leave the "thriving" band, which is the whole point
	// of the min term: a single dying machine dulls the entire tree.
	if cfg.Word(b) == "thriving" {
		t.Errorf("one dead host among eight should not read as thriving: %v (%s)", b, cfg.Word(b))
	}
	// The mean alone would have hidden it.
	meanOnly := 0.0
	for _, v := range withOneBad {
		meanOnly += v
	}
	meanOnly /= float64(len(withOneBad))
	if b >= meanOnly {
		t.Errorf("the min term should pull below the plain mean: %v vs %v", b, meanOnly)
	}
}

func TestNetworkVitalityIsBounded(t *testing.T) {
	cfg := DefaultConfig()
	for _, vs := range [][]float64{{2, 5}, {-1, -2}, {0, 1}, {1e9}} {
		got := NetworkVitality(&cfg, vs)
		if got < 0 || got > 1 {
			t.Errorf("out of range for %v: %v", vs, got)
		}
	}
}

func TestStoreRecordAndSnapshot(t *testing.T) {
	cfg := DefaultConfig()
	st := NewStore(&cfg)
	now := time.Now()

	st.Record(Report{Host: "b", CPU: F(10)}, now)
	st.Record(Report{Host: "a", CPU: F(95)}, now)
	st.Record(Report{Host: "b", CPU: F(20)}, now) // update, not a new host

	if st.Count() != 2 {
		t.Fatalf("expected 2 hosts, got %d", st.Count())
	}
	snap := st.Snapshot(now)
	if snap.HostCount != 2 {
		t.Fatalf("snapshot has %d hosts", snap.HostCount)
	}
	if snap.Hosts[0].Name != "a" || snap.Hosts[1].Name != "b" {
		t.Errorf("hosts should be sorted by name: %v %v", snap.Hosts[0].Name, snap.Hosts[1].Name)
	}
	if *snap.Hosts[1].Metrics.CPU != 20 {
		t.Errorf("expected the newer reading, got %v", *snap.Hosts[1].Metrics.CPU)
	}
	if snap.Crit != 1 || snap.Good != 1 {
		t.Errorf("counts = %+v", snap)
	}
}

func TestStoreEvictsOldestAtTheCap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxHosts = 4
	cfg.normalize()
	st := NewStore(&cfg)
	now := time.Now()

	for i := 0; i < 10; i++ {
		st.Record(Report{Host: string(rune('a' + i))}, now.Add(time.Duration(i)*time.Second))
	}
	if st.Count() != 4 {
		t.Fatalf("expected the cap of 4, got %d", st.Count())
	}
	snap := st.Snapshot(now.Add(10 * time.Second))
	for _, h := range snap.Hosts {
		// The oldest-seen must be gone; the newest must remain.
		if h.Name == "a" || h.Name == "b" {
			t.Errorf("host %q should have been evicted", h.Name)
		}
	}
	if snap.Hosts[len(snap.Hosts)-1].Name != "j" {
		t.Errorf("the newest host should still be tracked: %v", snap.Hosts)
	}
}

func TestStoreDropsLongGoneHosts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EvictAfter = 60
	cfg.normalize()
	st := NewStore(&cfg)
	now := time.Now()
	st.Record(Report{Host: "ghost"}, now)

	if snap := st.Snapshot(now.Add(30 * time.Second)); snap.HostCount != 1 {
		t.Fatalf("host should still be tracked: %d", snap.HostCount)
	}
	if snap := st.Snapshot(now.Add(61 * time.Second)); snap.HostCount != 0 {
		t.Fatalf("host should have been dropped after EvictAfter: %d", snap.HostCount)
	}
	if st.Count() != 0 {
		t.Errorf("the map should have been cleaned: %d", st.Count())
	}
	// ...and it regrows if it comes back.
	st.Record(Report{Host: "ghost"}, now.Add(62*time.Second))
	if snap := st.Snapshot(now.Add(62 * time.Second)); snap.HostCount != 1 {
		t.Errorf("a returning host should regrow: %d", snap.HostCount)
	}
}

func TestStoreIsConcurrentSafe(t *testing.T) {
	cfg := DefaultConfig()
	st := NewStore(&cfg)
	now := time.Now()
	done := make(chan bool, 32)
	for i := 0; i < 16; i++ {
		go func(i int) {
			for k := 0; k < 200; k++ {
				st.Record(Report{Host: string(rune('a' + i%8)), CPU: F(float64(k))}, now)
			}
			done <- true
		}(i)
	}
	for i := 0; i < 8; i++ {
		go func() {
			for k := 0; k < 200; k++ {
				_ = st.Snapshot(now)
			}
			done <- true
		}()
	}
	for i := 0; i < 24; i++ {
		<-done
	}
	if st.Count() != 8 {
		t.Errorf("expected 8 distinct hosts, got %d", st.Count())
	}
}

func TestGradeHostForReply(t *testing.T) {
	cfg := DefaultConfig()
	st := NewStore(&cfg)
	now := time.Now()
	st.Record(Report{Host: "hot", Temp: F(95)}, now)
	h, ok := st.GradeHost("hot", now)
	if !ok {
		t.Fatal("host not found")
	}
	if h.Band != BandCrit {
		t.Errorf("expected critical, got %v", h.Band)
	}
	if _, ok := st.GradeHost("nope", now); ok {
		t.Error("an unknown host should not grade")
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{75 * time.Second, "1m15s"},
		{3600 * time.Second, "1h00m"},
		{90000 * time.Second, "1d01h"},
	}
	for _, c := range cases {
		if got := ShortDuration(c.d); got != c.want {
			t.Errorf("ShortDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestWordBands(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		v    float64
		want string
	}{
		{1.0, "thriving"}, {0.8, "thriving"}, {0.79, "ok"}, {0.5, "ok"},
		{0.49, "stressed"}, {0.25, "stressed"}, {0.24, "ailing"}, {0, "ailing"},
	}
	for _, c := range cases {
		if got := cfg.Word(c.v); got != c.want {
			t.Errorf("Word(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestBandOf(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.BandOf(0.9) != BandGood || cfg.BandOf(0.5) != BandWarn || cfg.BandOf(0.1) != BandCrit {
		t.Error("band cutoffs are wrong")
	}
}
