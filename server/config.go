package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Threshold grades one metric: score 1.0 at or below Healthy, 0.0 at or beyond
// Critical, linear in between.
type Threshold struct {
	Healthy  float64 `json:"healthy"`
	Critical float64 `json:"critical"`
}

// WordBand maps a vitality floor to the word printed on the status line.
// Bands are evaluated highest Min first; the lowest band is the catch-all.
type WordBand struct {
	Min  float64 `json:"min"`
	Word string  `json:"word"`
}

// Config holds every tunable number in the system: thresholds, decay times,
// weights, growth rates and the vitality-to-word cutoffs. DefaultConfig() is
// the whole zero-config story; an optional JSON file may override any subset.
type Config struct {
	// --- network ---
	Bind string `json:"bind"`
	Port int    `json:"port"`

	MaxPayloadBytes int     `json:"max_payload_bytes"`
	MaxHosts        int     `json:"max_hosts"`
	RatePerSecond   float64 `json:"rate_per_second"`
	TCPTimeout      float64 `json:"tcp_timeout_seconds"`

	// --- timing / staleness ---
	// ReportInterval is what clients use by default; FreshWindowSeconds is
	// derived from it (3x) unless set explicitly.
	ReportInterval float64 `json:"report_interval_seconds"`
	FreshWindow    float64 `json:"fresh_window_seconds"`
	DecayWindow    float64 `json:"decay_window_seconds"`
	EvictAfter     float64 `json:"evict_after_seconds"`

	// --- recent reboot ---
	RebootWindow float64 `json:"reboot_window_seconds"`
	RebootCap    float64 `json:"reboot_cap"`

	// --- metric thresholds ---
	CPU  Threshold `json:"cpu"`
	Temp Threshold `json:"temp"`
	Load Threshold `json:"load"`

	// --- network aggregate ---
	MeanWeight    float64 `json:"mean_weight"`
	MinWeight     float64 `json:"min_weight"`
	EmptyVitality float64 `json:"empty_vitality"`

	// --- host colour bands, by vitality ---
	HostGreen  float64 `json:"host_green_above"`
	HostYellow float64 `json:"host_yellow_above"`

	Words []WordBand `json:"vitality_words"`

	// --- tree growth ---
	GrowthPerMinute  float64 `json:"growth_per_minute"`
	DiebackPerMinute float64 `json:"dieback_per_minute"`
	GrowthFullAbove  float64 `json:"growth_full_above"`
	GrowthPauseBelow float64 `json:"growth_pause_below"`
	SeedGrowth       float64 `json:"seed_growth"`
	DisplayTau       float64 `json:"display_tau_seconds"`

	// --- rendering ---
	RenderInterval float64 `json:"render_interval_seconds"`
	Unicode        bool    `json:"unicode"`
	LogPath        string  `json:"log_path"`
}

// DefaultConfig is the zero-config baseline. Every number the spec mentions
// lives here and nowhere else.
func DefaultConfig() Config {
	return Config{
		Bind: "0.0.0.0",
		Port: 1717,

		MaxPayloadBytes: 512,
		MaxHosts:        256,
		RatePerSecond:   1.0,
		TCPTimeout:      1.0,

		ReportInterval: 10,
		FreshWindow:    0, // 0 => derived: 3 * ReportInterval
		DecayWindow:    300,
		EvictAfter:     3600,

		RebootWindow: 300,
		RebootCap:    0.6,

		CPU:  Threshold{Healthy: 70, Critical: 90},
		Temp: Threshold{Healthy: 65, Critical: 80},
		Load: Threshold{Healthy: 0.8, Critical: 1.5},

		MeanWeight:    0.6,
		MinWeight:     0.4,
		EmptyVitality: 0.5,

		HostGreen:  0.75,
		HostYellow: 0.40,

		Words: []WordBand{
			{Min: 0.80, Word: "thriving"},
			{Min: 0.50, Word: "ok"},
			{Min: 0.25, Word: "stressed"},
			{Min: 0.00, Word: "ailing"},
		},

		GrowthPerMinute:  0.11,
		DiebackPerMinute: 0.16,
		GrowthFullAbove:  0.80,
		GrowthPauseBelow: 0.50,
		SeedGrowth:       0.08,
		DisplayTau:       3.0,

		RenderInterval: 1.0,
		Unicode:        false,
		LogPath:        "", // resolved by defaultLogPath()
	}
}

// LoadConfig starts from the defaults and overlays the JSON file at path.
// Keys absent from the file keep their default, so a partial config works.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		cfg.normalize()
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return DefaultConfig(), fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// normalize clamps everything into usable ranges so a bad config file cannot
// produce NaNs, division by zero or a negative timer.
func (c *Config) normalize() {
	if c.Port <= 0 || c.Port > 65535 {
		c.Port = DefaultConfig().Port
	}
	if c.Bind == "" {
		c.Bind = "0.0.0.0"
	}
	if c.MaxPayloadBytes < 16 || c.MaxPayloadBytes > 4096 {
		c.MaxPayloadBytes = 512
	}
	if c.MaxHosts < 1 || c.MaxHosts > 4096 {
		c.MaxHosts = 256
	}
	if c.RatePerSecond <= 0 {
		c.RatePerSecond = 1
	}
	if c.TCPTimeout < 0.05 {
		c.TCPTimeout = 1
	}
	if c.ReportInterval <= 0 {
		c.ReportInterval = 10
	}
	if c.FreshWindow < 0 {
		c.FreshWindow = 0
	}
	if c.DecayWindow <= 0 {
		c.DecayWindow = 300
	}
	if c.EvictAfter <= 0 {
		c.EvictAfter = 3600
	}
	if c.RebootWindow < 0 {
		c.RebootWindow = 300
	}
	c.RebootCap = clamp01(c.RebootCap)

	c.CPU = normThreshold(c.CPU, 70, 90)
	c.Temp = normThreshold(c.Temp, 65, 80)
	c.Load = normThreshold(c.Load, 0.8, 1.5)

	if c.MeanWeight < 0 || c.MinWeight < 0 || c.MeanWeight+c.MinWeight <= 0 {
		c.MeanWeight, c.MinWeight = 0.6, 0.4
	}
	sum := c.MeanWeight + c.MinWeight
	c.MeanWeight /= sum
	c.MinWeight /= sum
	c.EmptyVitality = clamp01(c.EmptyVitality)

	c.HostGreen = clamp01(c.HostGreen)
	c.HostYellow = clamp01(c.HostYellow)
	if c.HostYellow > c.HostGreen {
		c.HostGreen, c.HostYellow = c.HostYellow, c.HostGreen
	}

	if len(c.Words) == 0 {
		c.Words = DefaultConfig().Words
	}
	for i := range c.Words {
		if c.Words[i].Word == "" {
			c.Words[i].Word = "unknown"
		}
		c.Words[i].Min = clamp01(c.Words[i].Min)
	}
	sortWordBands(c.Words)

	if c.GrowthPerMinute < 0 {
		c.GrowthPerMinute = 0
	}
	if c.DiebackPerMinute < 0 {
		c.DiebackPerMinute = 0
	}
	c.GrowthFullAbove = clamp01(c.GrowthFullAbove)
	c.GrowthPauseBelow = clamp01(c.GrowthPauseBelow)
	c.SeedGrowth = clamp01(c.SeedGrowth)
	if c.DisplayTau < 0.1 {
		c.DisplayTau = 3
	}
	if c.RenderInterval < 0.05 {
		c.RenderInterval = 1
	}
}

func normThreshold(t Threshold, dh, dc float64) Threshold {
	if t.Critical <= t.Healthy {
		return Threshold{Healthy: dh, Critical: dc}
	}
	return t
}

func sortWordBands(w []WordBand) {
	for i := 1; i < len(w); i++ {
		for j := i; j > 0 && w[j].Min > w[j-1].Min; j-- {
			w[j], w[j-1] = w[j-1], w[j]
		}
	}
}

// FreshWindowSeconds is the "still fresh" grace period: 3x the report interval
// unless the config pinned it explicitly.
func (c *Config) FreshWindowSeconds() float64 {
	if c.FreshWindow > 0 {
		return c.FreshWindow
	}
	return 3 * c.ReportInterval
}

// Word maps a vitality to its status word.
func (c *Config) Word(v float64) string {
	for _, b := range c.Words {
		if v >= b.Min {
			return b.Word
		}
	}
	if len(c.Words) > 0 {
		return c.Words[len(c.Words)-1].Word
	}
	return "unknown"
}

// RenderEvery is the snapshot/redraw period.
func (c *Config) RenderEvery() time.Duration { return secs(c.RenderInterval) }

func secs(f float64) time.Duration {
	if f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

func clamp01(v float64) float64 {
	switch {
	case v != v: // NaN
		return 0
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

func clampF(v, lo, hi float64) float64 {
	if v != v {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// DefaultLogPath resolves ~ for the server log: ~/.bonsai-server.log, falling
// back to /tmp when HOME is unset.
func DefaultLogPath(name string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "."+name+".log")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.log", name, os.Getuid()))
}
