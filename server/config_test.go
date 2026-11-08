package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigMatchesTheSpec(t *testing.T) {
	c := DefaultConfig()
	if c.Port != 1717 {
		t.Errorf("port = %d, want 1717", c.Port)
	}
	if c.Bind != "0.0.0.0" {
		t.Errorf("bind = %q, want 0.0.0.0", c.Bind)
	}
	if c.MaxPayloadBytes != 512 {
		t.Errorf("payload cap = %d, want 512", c.MaxPayloadBytes)
	}
	if c.MaxHosts != 256 {
		t.Errorf("host cap = %d, want 256", c.MaxHosts)
	}
	if c.CPU.Healthy != 70 || c.CPU.Critical != 90 {
		t.Errorf("cpu thresholds = %+v, want 70/90", c.CPU)
	}
	if c.Temp.Healthy != 65 || c.Temp.Critical != 80 {
		t.Errorf("temp thresholds = %+v, want 65/80", c.Temp)
	}
	if c.Load.Healthy != 0.8 || c.Load.Critical != 1.5 {
		t.Errorf("load thresholds = %+v, want 0.8/1.5", c.Load)
	}
	if c.MeanWeight != 0.6 || c.MinWeight != 0.4 {
		t.Errorf("weights = %v/%v, want 0.6/0.4", c.MeanWeight, c.MinWeight)
	}
	if c.EmptyVitality != 0.5 {
		t.Errorf("empty vitality = %v, want 0.5", c.EmptyVitality)
	}
	if c.RebootWindow != 300 || c.RebootCap != 0.6 {
		t.Errorf("reboot rule = %vs cap %v, want 300s cap 0.6", c.RebootWindow, c.RebootCap)
	}
	if c.DecayWindow != 300 || c.EvictAfter != 3600 {
		t.Errorf("decay %vs evict %vs, want 300/3600", c.DecayWindow, c.EvictAfter)
	}
	if got := c.FreshWindowSeconds(); got != 30 {
		t.Errorf("fresh window = %vs, want 3x the 10s interval", got)
	}
	if c.ReportInterval != 10 {
		t.Errorf("report interval = %v, want 10", c.ReportInterval)
	}
	if c.RatePerSecond != 1 {
		t.Errorf("rate limit = %v/s, want 1", c.RatePerSecond)
	}
	if c.GrowthPauseBelow != 0.5 || c.GrowthFullAbove != 0.8 {
		t.Errorf("growth bands = %v..%v, want 0.5..0.8", c.GrowthPauseBelow, c.GrowthFullAbove)
	}
	for _, want := range []string{"thriving", "ok", "stressed", "ailing"} {
		found := false
		for _, w := range c.Words {
			if w.Word == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing vitality word %q", want)
		}
	}
}

func TestLoadConfigPartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bonsai.json")
	body := `{"port": 2000, "cpu": {"healthy": 40, "critical": 60}, "mean_weight": 0.5}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != 2000 {
		t.Errorf("port = %d, want the override 2000", cfg.Port)
	}
	if cfg.CPU.Healthy != 40 || cfg.CPU.Critical != 60 {
		t.Errorf("cpu = %+v, want the override", cfg.CPU)
	}
	// Everything not mentioned keeps its default.
	if cfg.Bind != "0.0.0.0" || cfg.MaxHosts != 256 || cfg.Temp.Critical != 80 {
		t.Errorf("untouched defaults were clobbered: %+v", cfg)
	}
	// Weights are renormalised to a convex combination, so the 0.5 keeps its
	// 0.5:0.4 ratio against the untouched default and the aggregate stays in
	// 0..1 by construction.
	if sum := cfg.MeanWeight + cfg.MinWeight; sum < 0.999 || sum > 1.001 {
		t.Errorf("weights should sum to 1: %v + %v", cfg.MeanWeight, cfg.MinWeight)
	}
	wantRatio := 0.5 / 0.4
	if got := cfg.MeanWeight / cfg.MinWeight; got < wantRatio-1e-9 || got > wantRatio+1e-9 {
		t.Errorf("weight ratio changed: %v, want %v", got, wantRatio)
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"portt": 2000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("a typo'd key should be an error, not a silent no-op")
	}
}

func TestLoadConfigErrors(t *testing.T) {
	if _, err := LoadConfig("/nonexistent/bonsai.json"); err == nil {
		t.Error("expected an error for a missing file")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected an error for malformed JSON")
	}
	if _, err := LoadConfig(""); err != nil {
		t.Errorf("an empty path means defaults, got %v", err)
	}
}

func TestConfigNormalizeSurvivesNonsense(t *testing.T) {
	bad := Config{
		Port: -1, Bind: "", MaxPayloadBytes: 0, MaxHosts: 0, RatePerSecond: -3,
		TCPTimeout: 0, ReportInterval: -1, FreshWindow: -9, DecayWindow: 0,
		EvictAfter: 0, RebootWindow: -1, RebootCap: 5,
		CPU:        Threshold{Healthy: 90, Critical: 70}, // inverted
		Temp:       Threshold{Healthy: 0, Critical: 0},
		Load:       Threshold{Healthy: 1, Critical: 1},
		MeanWeight: 0, MinWeight: 0, EmptyVitality: 4,
		HostGreen: 0.2, HostYellow: 0.9, // inverted
		Words:           []WordBand{{Min: 0.4, Word: "low"}, {Min: 0.9, Word: "high"}, {Min: 2, Word: ""}},
		GrowthPerMinute: -1, DiebackPerMinute: -1,
		GrowthFullAbove: 3, GrowthPauseBelow: -2, SeedGrowth: 9,
		DisplayTau: 0, RenderInterval: 0,
	}
	bad.normalize()

	if bad.Port != 1717 || bad.Bind != "0.0.0.0" {
		t.Errorf("bind/port not repaired: %v %q", bad.Port, bad.Bind)
	}
	if bad.MaxPayloadBytes != 512 || bad.MaxHosts != 256 {
		t.Errorf("caps not repaired: %d %d", bad.MaxPayloadBytes, bad.MaxHosts)
	}
	if bad.CPU.Healthy != 70 || bad.CPU.Critical != 90 {
		t.Errorf("inverted thresholds should fall back to defaults: %+v", bad.CPU)
	}
	if bad.MeanWeight+bad.MinWeight != 1 {
		t.Errorf("weights must sum to 1: %v %v", bad.MeanWeight, bad.MinWeight)
	}
	if bad.EmptyVitality != 1 || bad.SeedGrowth != 1 {
		t.Errorf("vitality values must clamp to 1: %v %v", bad.EmptyVitality, bad.SeedGrowth)
	}
	if bad.HostGreen <= bad.HostYellow {
		t.Errorf("band cutoffs should be ordered: %v %v", bad.HostGreen, bad.HostYellow)
	}
	if bad.Words[0].Min < bad.Words[1].Min {
		t.Errorf("word bands should be sorted descending: %+v", bad.Words)
	}
	if bad.Words[2].Word == "" {
		t.Error("an empty word should be filled in")
	}
	if bad.GrowthPerMinute < 0 || bad.DiebackPerMinute < 0 {
		t.Error("growth rates must not be negative")
	}
	if bad.DisplayTau <= 0 || bad.RenderInterval <= 0 {
		t.Errorf("timers must be positive: %v %v", bad.DisplayTau, bad.RenderInterval)
	}
	if got := bad.FreshWindowSeconds(); got != 3*bad.ReportInterval {
		t.Errorf("fresh window should fall back to 3x the interval: %v", got)
	}
}

func TestFreshWindowCanBePinnedExplicitly(t *testing.T) {
	c := DefaultConfig()
	c.FreshWindow = 90
	c.normalize()
	if got := c.FreshWindowSeconds(); got != 90 {
		t.Errorf("explicit fresh window ignored: %v", got)
	}
	c.ReportInterval = 30
	if got := c.FreshWindowSeconds(); got != 90 {
		t.Errorf("explicit fresh window should not track the interval: %v", got)
	}
}

func TestWordBandOrderingIsFixedUp(t *testing.T) {
	c := DefaultConfig()
	c.Words = []WordBand{{Min: 0, Word: "ailing"}, {Min: 0.8, Word: "thriving"}, {Min: 0.5, Word: "ok"}}
	c.normalize()
	if c.Word(0.9) != "thriving" || c.Word(0.6) != "ok" || c.Word(0.1) != "ailing" {
		t.Errorf("unsorted bands broke Word(): %+v", c.Words)
	}
	if c.Word(-1) != "ailing" {
		t.Error("the lowest band must catch everything")
	}
}

func TestConfigRoundTripsThroughJSON(t *testing.T) {
	c := DefaultConfig()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "round.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	back, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Errorf("defaults did not survive a round trip:\n%s\n%s", a, b)
	}
}

func TestDefaultLogPath(t *testing.T) {
	got := DefaultLogPath("bonsai-server")
	if filepath.Base(got) != ".bonsai-server.log" && !filepath.IsAbs(got) {
		t.Errorf("unexpected log path %q", got)
	}
}

func TestClampHelpers(t *testing.T) {
	if clamp01(-1) != 0 || clamp01(2) != 1 || clamp01(0.5) != 0.5 {
		t.Error("clamp01 is wrong")
	}
	if v := clamp01(nan()); v != 0 {
		t.Errorf("clamp01(NaN) = %v, want 0", v)
	}
	if clampF(5, 1, 3) != 3 || clampF(0, 1, 3) != 1 || clampF(2, 1, 3) != 2 {
		t.Error("clampF is wrong")
	}
	if v := clampF(nan(), 1, 3); v != 1 {
		t.Errorf("clampF(NaN) = %v, want the floor", v)
	}
}

func nan() float64 {
	var z float64
	return z / z
}

func TestSecs(t *testing.T) {
	if secs(0) != 0 || secs(-1) != 0 {
		t.Error("non-positive durations should be zero")
	}
	if secs(1.5).Milliseconds() != 1500 {
		t.Errorf("secs(1.5) = %v", secs(1.5))
	}
}
