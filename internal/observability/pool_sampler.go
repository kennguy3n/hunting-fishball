package observability

// pool_sampler.go — Round-9 Task 17.
//
// PoolSampler periodically reads connection-pool stats from
// Postgres / Redis / Qdrant clients and publishes them onto the
// package-level Prometheus gauges defined in metrics.go. cmd/api
// starts one sampler at boot; the goroutine exits when ctx is
// cancelled (graceful shutdown).
//
// Source interfaces are deliberately narrow — each client is
// fronted by a small adapter so the sampler stays decoupled from
// the gorm/redis/storage import graphs and remains unit-testable
// in isolation.

import (
	"context"
	"time"
)

// PostgresStatsSource exposes the subset of database/sql.DB.Stats
// the sampler needs. In production the adapter is
// `func() int { return sqlDB.Stats().OpenConnections }`.
type PostgresStatsSource interface {
	OpenConnections() int
}

// PostgresStatsFunc is a func-adapter that satisfies
// PostgresStatsSource.
type PostgresStatsFunc func() int

// OpenConnections implements PostgresStatsSource.
func (f PostgresStatsFunc) OpenConnections() int { return f() }

// RedisStatsSource exposes the subset of go-redis's PoolStats the
// sampler needs (active connections = TotalConns - IdleConns).
type RedisStatsSource interface {
	ActiveConnections() int
}

// RedisStatsFunc adapts a plain func into a RedisStatsSource.
type RedisStatsFunc func() int

// ActiveConnections implements RedisStatsSource.
func (f RedisStatsFunc) ActiveConnections() int { return f() }

// QdrantStatsSource exposes the configured idle-connection
// capacity (see QdrantClient.IdleConnCapacity).
type QdrantStatsSource interface {
	IdleConnCapacity() int
}

// QdrantStatsFunc adapts a plain func into a QdrantStatsSource.
type QdrantStatsFunc func() int

// IdleConnCapacity implements QdrantStatsSource.
func (f QdrantStatsFunc) IdleConnCapacity() int { return f() }

// PoolSamplerConfig configures a PoolSampler. Each source is
// optional; nil sources are simply not sampled.
type PoolSamplerConfig struct {
	// Postgres, when non-nil, is sampled every Interval.
	Postgres PostgresStatsSource
	// Redis, when non-nil, is sampled every Interval.
	Redis RedisStatsSource
	// Qdrant, when non-nil, is sampled every Interval.
	Qdrant QdrantStatsSource
	// Interval is the sampling period. Defaults to 30s when zero.
	Interval time.Duration
}

// PoolSampler runs the sampling loop.
type PoolSampler struct {
	cfg PoolSamplerConfig
}

// NewPoolSampler constructs a sampler. Run with Start (blocks)
// or pass to cmd/api which spawns its own goroutine.
func NewPoolSampler(cfg PoolSamplerConfig) *PoolSampler {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	return &PoolSampler{cfg: cfg}
}

// SampleOnce reads each configured source and publishes the
// reading to the matching Prometheus gauge. Exported so tests
// can drive a single sample synchronously (without a ticker).
func (s *PoolSampler) SampleOnce() {
	if s == nil {
		return
	}
	if s.cfg.Postgres != nil {
		SetPostgresPoolOpenConnections(s.cfg.Postgres.OpenConnections())
	}
	if s.cfg.Redis != nil {
		SetRedisPoolActiveConnections(s.cfg.Redis.ActiveConnections())
	}
	if s.cfg.Qdrant != nil {
		SetQdrantPoolIdleConnections(s.cfg.Qdrant.IdleConnCapacity())
	}
}

// Start blocks until ctx is cancelled, sampling on every tick.
// Designed to be invoked as `go sampler.Start(ctx)`.
func (s *PoolSampler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	// Emit one reading immediately so dashboards aren't blank for
	// the first Interval.
	s.SampleOnce()
	tk := time.NewTicker(s.cfg.Interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			s.SampleOnce()
		}
	}
}
