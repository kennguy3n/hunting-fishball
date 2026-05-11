package shard

// scheduler.go — Round-6 Task 12.
//
// Pre-generates shards for active tenants on a configurable
// interval. The scheduler reads the tenant list from the supplied
// TenantLister, asks the ManifestStore which shards each tenant
// already has, and submits a `shard.requested` event for every
// stale or missing shard via the configured Submitter port.
//
// The scheduler is wired in cmd/ingest/main.go behind
// CONTEXT_ENGINE_SHARD_PREGENERATE=true. It runs independently of
// the streaming sync loop so admin operations (purge, reindex)
// don't starve pre-generation.

import (
	"context"
	"errors"
	"sync"
	"time"
)

// TenantLister returns the list of tenants the scheduler should
// service. Implementations live in `internal/admin` (Postgres) or
// in tests.
type TenantLister interface {
	ActiveTenants(ctx context.Context) ([]string, error)
}

// ManifestSnapshot is the read-side view a scheduler needs:
// for a given tenant, the set of shards and their last refresh
// timestamps.
type ManifestSnapshot interface {
	LatestByTenant(ctx context.Context, tenantID string) ([]*ManifestSummary, error)
}

// ManifestSummary is the minimal struct the scheduler inspects.
type ManifestSummary struct {
	TenantID  string
	ShardKind string
	UpdatedAt time.Time
}

// ShardSubmitter accepts a "shard.requested" event for upstream
// processing. The production wiring forwards to the ingest
// coordinator's Submit method.
type ShardSubmitter interface {
	SubmitShardRequest(ctx context.Context, tenantID, shardKind string) error
}

// SchedulerConfig wires the scheduler.
type SchedulerConfig struct {
	// TenantLister enumerates the tenants to pre-generate for.
	TenantLister TenantLister
	// Manifests reads existing shard freshness.
	Manifests ManifestSnapshot
	// Submitter forwards shard.requested events.
	Submitter ShardSubmitter
	// FreshnessWindow is the maximum age a shard may have before
	// the scheduler treats it as stale and requests a refresh.
	FreshnessWindow time.Duration
	// Tick is the interval between scheduler passes.
	Tick time.Duration
	// ShardKinds enumerates the shard kinds to pre-generate per
	// tenant. Defaults to ["base"].
	ShardKinds []string
	// Now overrides time.Now in tests.
	Now func() time.Time
}

// Scheduler runs the pre-generation loop.
type Scheduler struct {
	cfg     SchedulerConfig
	mu      sync.Mutex
	stats   SchedulerStats
	stopped chan struct{}
}

// SchedulerStats tracks per-pass counts for monitoring.
type SchedulerStats struct {
	Passes        int
	StaleDetected int
	Submitted     int
	SkippedFresh  int
}

// NewScheduler validates and constructs a Scheduler.
func NewScheduler(cfg SchedulerConfig) (*Scheduler, error) {
	if cfg.TenantLister == nil {
		return nil, errors.New("scheduler: nil TenantLister")
	}
	if cfg.Manifests == nil {
		return nil, errors.New("scheduler: nil Manifests")
	}
	if cfg.Submitter == nil {
		return nil, errors.New("scheduler: nil Submitter")
	}
	if cfg.FreshnessWindow <= 0 {
		cfg.FreshnessWindow = 24 * time.Hour
	}
	if cfg.Tick <= 0 {
		cfg.Tick = 1 * time.Hour
	}
	if len(cfg.ShardKinds) == 0 {
		cfg.ShardKinds = []string{"base"}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Scheduler{cfg: cfg, stopped: make(chan struct{})}, nil
}

// RunOnce executes a single pre-generation pass and returns. Tests
// drive the scheduler via RunOnce; the production wiring uses Run.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	tenants, err := s.cfg.TenantLister.ActiveTenants(ctx)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	cutoff := now.Add(-s.cfg.FreshnessWindow)
	var stale, fresh, submitted int
	for _, tenantID := range tenants {
		summaries, err := s.cfg.Manifests.LatestByTenant(ctx, tenantID)
		if err != nil {
			continue
		}
		freshness := map[string]time.Time{}
		for _, m := range summaries {
			if m == nil {
				continue
			}
			if existing, ok := freshness[m.ShardKind]; !ok || m.UpdatedAt.After(existing) {
				freshness[m.ShardKind] = m.UpdatedAt
			}
		}
		for _, kind := range s.cfg.ShardKinds {
			ts, ok := freshness[kind]
			if ok && ts.After(cutoff) {
				fresh++
				continue
			}
			stale++
			if err := s.cfg.Submitter.SubmitShardRequest(ctx, tenantID, kind); err == nil {
				submitted++
			}
		}
	}
	s.mu.Lock()
	s.stats.Passes++
	s.stats.StaleDetected += stale
	s.stats.SkippedFresh += fresh
	s.stats.Submitted += submitted
	s.mu.Unlock()
	return nil
}

// Run starts the scheduler loop and returns when ctx fires.
func (s *Scheduler) Run(ctx context.Context) error {
	defer close(s.stopped)
	tk := time.NewTicker(s.cfg.Tick)
	defer tk.Stop()
	if err := s.RunOnce(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C:
			_ = s.RunOnce(ctx)
		}
	}
}

// Stats returns a snapshot of cumulative scheduler counters.
func (s *Scheduler) Stats() SchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
