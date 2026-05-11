// pool_leak_detector.go — Round-13 Task 18.
//
// PoolLeakDetector periodically reads db.Stats().OpenConnections
// and compares it against the configured MaxOpen. When utilisation
// stays above HighThresholdPercent for HighConsecutiveSamples
// consecutive samples, it logs a structured warning. The same
// reading is published to context_engine_postgres_pool_utilization_percent
// so the existing PostgresPoolSaturated alert (which fires off the
// raw open-connections gauge) is complemented by an application-
// side gauge that always renders a 0-100 percentage even when
// MaxOpen changes between releases.
//
// This is intentionally separate from PoolSampler — PoolSampler
// publishes raw connection counts, whereas this detector publishes
// the utilisation percentage and watches for sustained high water
// marks. The two coexist; the detector does NOT change the raw
// gauges PoolSampler maintains.
package observability

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// PoolLeakDetectorConfig configures the detector.
type PoolLeakDetectorConfig struct {
	// Pool is the source of OpenConnections readings. Required.
	Pool PostgresStatsSource
	// MaxOpen is the denominator for the percent calculation. If
	// MaxOpen is <= 0 the detector returns an error from
	// NewPoolLeakDetector — a sentinel that lets cmd/api skip
	// wiring when CONTEXT_ENGINE_PG_MAX_OPEN is unset.
	MaxOpen int
	// Interval is the sampling period. Defaults to 30s.
	Interval time.Duration
	// HighThresholdPercent (default 90) is the utilisation
	// percentage above which the detector starts counting
	// consecutive samples toward a warning.
	HighThresholdPercent int
	// HighConsecutiveSamples (default 3) is how many samples in
	// a row must exceed the threshold before the warning fires.
	HighConsecutiveSamples int
	// Logger receives the structured warning. Defaults to slog.Default().
	Logger *slog.Logger
}

// PoolLeakDetector watches for sustained Postgres pool saturation.
type PoolLeakDetector struct {
	cfg   PoolLeakDetectorConfig
	hits  int
	last  int // last published percent
}

// ErrNoMaxOpen is returned by NewPoolLeakDetector when the
// caller didn't configure a pool ceiling.
var ErrNoMaxOpen = errors.New("observability: PoolLeakDetector requires MaxOpen > 0")

// NewPoolLeakDetector validates cfg and returns a detector.
func NewPoolLeakDetector(cfg PoolLeakDetectorConfig) (*PoolLeakDetector, error) {
	if cfg.Pool == nil {
		return nil, errors.New("observability: PoolLeakDetector requires Pool source")
	}
	if cfg.MaxOpen <= 0 {
		return nil, ErrNoMaxOpen
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.HighThresholdPercent <= 0 {
		cfg.HighThresholdPercent = 90
	}
	if cfg.HighConsecutiveSamples <= 0 {
		cfg.HighConsecutiveSamples = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &PoolLeakDetector{cfg: cfg}, nil
}

// SampleOnce reads the pool, publishes the percent gauge, and
// fires a warning if the high-water-mark threshold has been
// breached HighConsecutiveSamples times in a row. Exported so
// tests can drive the detector synchronously.
func (d *PoolLeakDetector) SampleOnce() {
	if d == nil {
		return
	}
	open := d.cfg.Pool.OpenConnections()
	percent := int(float64(open) / float64(d.cfg.MaxOpen) * 100)
	PostgresPoolUtilizationPercent.Set(float64(percent))
	d.last = percent
	if percent >= d.cfg.HighThresholdPercent {
		d.hits++
		if d.hits >= d.cfg.HighConsecutiveSamples {
			d.cfg.Logger.Warn("postgres: pool saturation",
				slog.Int("open_connections", open),
				slog.Int("max_open", d.cfg.MaxOpen),
				slog.Int("percent", percent),
				slog.Int("consecutive_samples", d.hits),
			)
			// Reset to avoid log spam — re-arms after one sample
			// below threshold.
			d.hits = 0
		}
	} else {
		d.hits = 0
	}
}

// LastPercent returns the most recently sampled percent value.
// Exported for tests.
func (d *PoolLeakDetector) LastPercent() int { return d.last }

// Start blocks until ctx is cancelled, sampling on every tick.
func (d *PoolLeakDetector) Start(ctx context.Context) {
	if d == nil {
		return
	}
	d.SampleOnce()
	tk := time.NewTicker(d.cfg.Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			d.SampleOnce()
		}
	}
}
