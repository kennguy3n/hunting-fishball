// integrity_worker.go — Round-14 Task 6.
//
// Periodic background verification of the audit-log hash chain.
// The worker re-computes the head hash for every configured
// tenant on a configurable interval (default 1h). When the
// previously-recorded head no longer matches the freshly-
// computed head, the worker:
//
//  1. Emits an `audit.integrity_violation` audit event so the
//     tamper attempt is itself recorded in the same append-only
//     log (the new event extends the chain and gives operators
//     a forensic anchor).
//  2. Increments the Prometheus counter
//     `context_engine_audit_integrity_violations_total`.
//  3. Writes a structured slog.Error so on-call gets paged
//     through the existing log-pipeline routing.
//
// The worker is gated on CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK=true
// so a deployment can opt in without redeploying. The interval
// is configurable via CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK_INTERVAL.
package audit

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// IntegrityCheckEnabled returns true when the worker is opted-in.
func IntegrityCheckEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK")))
	return v == "1" || v == "true" || v == "yes"
}

// IntegrityCheckInterval returns the configured worker interval
// or the default 1h.
func IntegrityCheckInterval() time.Duration {
	if raw := os.Getenv("CONTEXT_ENGINE_AUDIT_INTEGRITY_CHECK_INTERVAL"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

// TenantsFn returns the list of tenants whose chains should be
// re-checked. The worker is intentionally pull-based on the
// caller's tenant enumeration so the audit package does not
// have to import a tenants store.
type TenantsFn func(ctx context.Context) ([]string, error)

// IntegrityWorker periodically re-computes the chain head per
// tenant and reports mismatches.
type IntegrityWorker struct {
	repo     *Repository
	tenants  TenantsFn
	interval time.Duration

	mu sync.Mutex
	// observations tracks the most recent head + the latest
	// entry ID we computed over for each tenant. The latest
	// entry ID lets RunOnce filter out rows appended between
	// sweeps so natural append-only growth does not trip the
	// violation path.
	observations map[string]integrityObservation
	nowFn        func() time.Time
	emitFn       func(ctx context.Context, log *AuditLog) error
	observeFn    func()
}

// IntegrityWorkerConfig configures the worker.
type IntegrityWorkerConfig struct {
	Repo     *Repository
	Tenants  TenantsFn
	Interval time.Duration
	NowFn    func() time.Time
	// EmitFn defaults to Repo.Create. Tests can substitute a
	// stub that records emitted events without touching the DB.
	EmitFn func(ctx context.Context, log *AuditLog) error
	// ObserveFn is called once per detected violation so the
	// caller can increment its Prometheus counter without the
	// audit package depending on observability. cmd/api wires
	// observability.AuditIntegrityViolationsTotal.Inc() in.
	ObserveFn func()
}

// NewIntegrityWorker validates the config and constructs a worker.
func NewIntegrityWorker(cfg IntegrityWorkerConfig) (*IntegrityWorker, error) {
	if cfg.Repo == nil {
		return nil, errors.New("integrity_worker: Repo required")
	}
	if cfg.Tenants == nil {
		return nil, errors.New("integrity_worker: Tenants required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = IntegrityCheckInterval()
	}
	if cfg.NowFn == nil {
		cfg.NowFn = func() time.Time { return time.Now().UTC() }
	}
	if cfg.EmitFn == nil {
		cfg.EmitFn = cfg.Repo.Create
	}
	if cfg.ObserveFn == nil {
		cfg.ObserveFn = func() {}
	}
	return &IntegrityWorker{
		repo:         cfg.Repo,
		tenants:      cfg.Tenants,
		interval:     cfg.Interval,
		observations: map[string]integrityObservation{},
		nowFn:        cfg.NowFn,
		emitFn:       cfg.EmitFn,
		observeFn:    cfg.ObserveFn,
	}, nil
}

// RunOnce performs a single sweep across every configured tenant.
// Returns the number of mismatches detected — useful in tests.
func (w *IntegrityWorker) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.tenants(ctx)
	if err != nil {
		return 0, err
	}
	mismatches := 0
	for _, tenantID := range tenants {
		mismatched, ierr := w.checkOne(ctx, tenantID)
		if ierr != nil {
			slog.Warn("audit integrity check failed", "tenant_id", tenantID, "error", ierr)
			continue
		}
		if mismatched {
			mismatches++
		}
	}
	return mismatches, nil
}

func (w *IntegrityWorker) checkOne(ctx context.Context, tenantID string) (bool, error) {
	res, err := w.repo.List(ctx, ListFilter{TenantID: tenantID, PageSize: maxPageSize})
	if err != nil {
		return false, err
	}
	resp := ComputeIntegrity(tenantID, res.Items)
	obs := integrityObservation{
		HeadHash:   resp.HeadHash,
		FirstEntry: resp.FirstEntry,
		LastEntry:  resp.LastEntry,
		EntryCount: resp.EntryCount,
	}
	w.mu.Lock()
	prev, seen := w.observations[tenantID]
	w.observations[tenantID] = obs
	w.mu.Unlock()
	if !seen {
		// First observation — pin the head so subsequent runs
		// can compare. No mismatch claim on the bootstrap pass.
		return false, nil
	}
	if prev.HeadHash == resp.HeadHash {
		return false, nil
	}
	// Detect "append-only" growth: re-fetch exactly the
	// (FirstEntry, LastEntry) range the previous sweep saw and
	// recompute the head. Natural appends widen the chain so the
	// outer HeadHash diverges, but the historical prefix must
	// still hash to `prev.HeadHash`. Pagination handles tenants
	// whose chain grew beyond maxPageSize between sweeps —
	// without it, the most-recent-N window slides and older rows
	// fall off, guaranteeing a spurious mismatch.
	tailMatch, tailErr := w.tailMatches(ctx, tenantID, prev)
	if tailErr == nil && tailMatch {
		return false, nil
	}
	w.reportViolation(ctx, tenantID, prev.HeadHash, resp.HeadHash)
	return true, nil
}

// tailMatches returns true when the chain restricted to entries
// in the previously-observed [FirstEntry, LastEntry] range
// re-hashes to `prev.HeadHash`. This collapses the common
// "rows appended since last run" case to a no-op without
// flagging a violation. A genuine tamper changes the hash of
// an existing entry, so the re-computation over the same set of
// historical IDs diverges.
//
// Pagination matters here: chains larger than maxPageSize can't
// be re-fetched in a single List call. We paginate using the
// existing PageToken cursor (id < token, ordered DESC) bounded
// by IDMinInclusive=FirstEntry and IDMaxInclusive=LastEntry.
func (w *IntegrityWorker) tailMatches(ctx context.Context, tenantID string, prev integrityObservation) (bool, error) {
	if prev.LastEntry == "" || prev.HeadHash == "" || prev.FirstEntry == "" {
		return false, nil
	}
	collected := make([]AuditLog, 0, prev.EntryCount)
	pageToken := ""
	for {
		res, err := w.repo.List(ctx, ListFilter{
			TenantID:       tenantID,
			PageSize:       maxPageSize,
			PageToken:      pageToken,
			IDMinInclusive: prev.FirstEntry,
			IDMaxInclusive: prev.LastEntry,
		})
		if err != nil {
			return false, err
		}
		collected = append(collected, res.Items...)
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
		// Guard against unbounded pagination if the upstream
		// store ever returns a stable cursor.
		if len(collected) > prev.EntryCount*2+maxPageSize {
			return false, nil
		}
	}
	// Row count must match the previous observation. A short
	// page means rows were deleted in the historical window —
	// that is itself a tamper signal, so do not accept the tail.
	if prev.EntryCount > 0 && len(collected) != prev.EntryCount {
		return false, nil
	}
	tail := ComputeIntegrity(tenantID, collected)
	return tail.HeadHash == prev.HeadHash, nil
}

// integrityObservation is the per-tenant snapshot the worker
// keeps between sweeps. FirstEntry + LastEntry bracket the
// exact ID range the previous head was computed over; the
// next sweep paginates over that same range to validate the
// historical prefix even when the chain grew past maxPageSize.
type integrityObservation struct {
	HeadHash   string
	FirstEntry string
	LastEntry  string
	EntryCount int
}

func (w *IntegrityWorker) reportViolation(ctx context.Context, tenantID, prev, head string) {
	w.observeFn()
	slog.Error("audit integrity violation",
		"tenant_id", tenantID,
		"previous_head", prev,
		"current_head", head,
	)
	// Emit the integrity_violation audit event so the breach is
	// itself recorded in the chain. The new row extends the
	// chain and gives operators a forensic anchor pinned to
	// real wall-clock time.
	now := w.nowFn()
	log := &AuditLog{
		ID:           ulid.Make().String(),
		TenantID:     tenantID,
		ActorID:      "system",
		Action:       ActionAuditIntegrityViolation,
		ResourceType: "audit_log",
		ResourceID:   tenantID,
		Metadata: JSONMap{
			"previous_head": prev,
			"current_head":  head,
		},
		CreatedAt: now,
	}
	if err := w.emitFn(ctx, log); err != nil {
		slog.Warn("audit integrity violation emit failed", "error", err)
	}
}

// Run blocks until ctx is canceled, sweeping at the configured
// interval. It calls RunOnce immediately on entry so a tenant
// with a corrupted chain is detected on the first tick rather
// than waiting a full interval.
func (w *IntegrityWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	if _, err := w.RunOnce(ctx); err != nil {
		slog.Warn("audit integrity initial sweep failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.RunOnce(ctx); err != nil {
				slog.Warn("audit integrity sweep failed", "error", err)
			}
		}
	}
}
