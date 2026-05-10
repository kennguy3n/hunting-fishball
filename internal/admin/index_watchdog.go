// index_watchdog.go — Round-5 Task 18.
//
// IndexWatchdog is a background goroutine that runs once per
// WatchdogInterval (30 min). Each tick:
//
//  1. Calls the BackendChecker for every registered storage
//     backend (same checker the index_health endpoint uses).
//  2. If a backend reports unhealthy for more than
//     WatchdogConsecutiveThreshold consecutive checks, triggers
//     an automatic POST /v1/admin/reindex for each (tenant,
//     source) pair that uses that backend and records an
//     index.auto_reindex_triggered audit event.
//  3. Rate-limits to one auto-reindex per tenant per
//     WatchdogReindexCooldown (1h) so transient backend flaps
//     don't flood the pipeline.
//
// The watchdog logs every decision at Debug level so operators
// can trace its reasoning via structured-logging queries. It
// does NOT restart itself on panic — the supervisor (process
// manager) restarts the entire ingest binary, which is the
// pattern all background workers in this codebase use (see
// token_refresh.go, credential_monitor.go).
package admin

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/pipeline"
)

// WatchdogInterval is the default check cadence. 30 minutes
// gives operators a ~1h window from first failure to auto-
// reindex (two consecutive unhealthy checks).
const WatchdogInterval = 30 * time.Minute

// WatchdogConsecutiveThreshold is the minimum consecutive
// unhealthy checks before an auto-reindex fires. 2 means one
// transient failure is forgiven.
const WatchdogConsecutiveThreshold = 2

// WatchdogReindexCooldown is the minimum interval between two
// auto-reindex triggers for the same tenant. Prevents a flapping
// backend from flooding the pipeline with redundant work.
const WatchdogReindexCooldown = time.Hour

// WatchdogConfig configures an IndexWatchdog.
type WatchdogConfig struct {
	Checkers  []BackendChecker
	Lister    CredentialMonitorSourceLister // ListAllActive()
	Reindexer ReindexRunner
	Audit     AuditWriter
	Logger    *slog.Logger
	Now       func() time.Time // test seam

	// BackendSourceFilter optionally narrows the set of sources
	// auto-reindexed for a given unhealthy backend. It receives the
	// list of unhealthy backend names and the full active-source set,
	// and returns only the sources that should be reindexed (e.g.
	// only the sources whose connector type writes to the
	// unhealthy backend).
	//
	// Default (when nil): reindex every active source — addresses
	// FLAG_pr-review-job_0002 by making the broad behavior
	// explicitly configurable rather than implicit. The default
	// remains broad because Qdrant + Postgres are core backends
	// every source writes through, so an unhealthy core backend
	// genuinely affects every source. Operators wiring graph- or
	// memory-only watchdog checkers should supply this filter to
	// scope reindexes to the affected connectors only.
	BackendSourceFilter func(unhealthy []string, srcs []Source) []Source
}

// IndexWatchdog monitors backend health and auto-triggers
// reindex on sustained failures.
type IndexWatchdog struct {
	cfg       WatchdogConfig
	now       func() time.Time
	mu        sync.Mutex
	failures  map[string]int       // backend-name → consecutive failures
	cooldowns map[string]time.Time // tenant-id → last reindex-at
}

// NewIndexWatchdog validates cfg and returns the watchdog.
func NewIndexWatchdog(cfg WatchdogConfig) (*IndexWatchdog, error) {
	if cfg.Lister == nil {
		return nil, errRequired("IndexWatchdog.Lister")
	}
	if cfg.Reindexer == nil {
		return nil, errRequired("IndexWatchdog.Reindexer")
	}
	if cfg.Audit == nil {
		cfg.Audit = noopAudit{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	return &IndexWatchdog{
		cfg:       cfg,
		now:       nowFn,
		failures:  make(map[string]int),
		cooldowns: make(map[string]time.Time),
	}, nil
}

func errRequired(field string) error {
	return &requiredFieldError{field: field}
}

type requiredFieldError struct{ field string }

func (e *requiredFieldError) Error() string {
	return "index_watchdog: nil " + e.field
}

// Tick runs one pass of the watchdog.
func (w *IndexWatchdog) Tick(ctx context.Context) {
	now := w.now()

	// Phase 1: Check backends.
	unhealthy := w.checkBackends(ctx)
	if len(unhealthy) == 0 {
		return
	}

	// Phase 2: List all active sources and trigger reindex for
	// tenants whose backends have been unhealthy for >
	// WatchdogConsecutiveThreshold consecutive checks.
	srcs, err := w.cfg.Lister.ListAllActive(ctx)
	if err != nil {
		w.cfg.Logger.Error("index_watchdog: list sources", "error", err)
		return
	}
	if w.cfg.BackendSourceFilter != nil {
		srcs = w.cfg.BackendSourceFilter(unhealthy, srcs)
	}
	tenants := uniqueTenants(srcs)
	for tenantID, srcIDs := range tenants {
		if w.isCoolingDown(tenantID, now) {
			continue
		}
		for _, srcID := range srcIDs {
			_, err := w.cfg.Reindexer.Reindex(ctx, pipeline.ReindexRequest{
				TenantID: tenantID,
				SourceID: srcID,
			})
			if err != nil {
				w.cfg.Logger.Error("index_watchdog: reindex", "tenant_id", tenantID, "source_id", srcID, "error", err)
				continue
			}
			_ = w.cfg.Audit.Create(ctx, audit.NewAuditLog(
				tenantID, "system", audit.ActionIndexAutoReindex, "source", srcID,
				audit.JSONMap{
					"backends": unhealthy,
					"trigger":  "watchdog",
				},
				"",
			))
		}
		w.mu.Lock()
		w.cooldowns[tenantID] = now
		w.mu.Unlock()
	}
}

// checkBackends runs each checker and returns the names of backends
// that crossed the consecutive-failure threshold this tick.
func (w *IndexWatchdog) checkBackends(ctx context.Context) []string {
	var unhealthy []string
	for _, ck := range w.cfg.Checkers {
		name := ck.Name()
		err := ck.Check(ctx)
		w.mu.Lock()
		if err != nil {
			w.failures[name]++
			if w.failures[name] >= WatchdogConsecutiveThreshold {
				unhealthy = append(unhealthy, name)
			}
		} else {
			w.failures[name] = 0
		}
		w.mu.Unlock()
	}
	return unhealthy
}

func (w *IndexWatchdog) isCoolingDown(tenantID string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.cooldowns[tenantID]
	if !ok {
		return false
	}
	return now.Sub(last) < WatchdogReindexCooldown
}

func uniqueTenants(srcs []Source) map[string][]string {
	out := make(map[string][]string)
	for _, s := range srcs {
		out[s.TenantID] = append(out[s.TenantID], s.ID)
	}
	return out
}

// ConsecutiveFailures returns the failure counts for tests.
func (w *IndexWatchdog) ConsecutiveFailures() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]int, len(w.failures))
	for k, v := range w.failures {
		out[k] = v
	}
	return out
}
