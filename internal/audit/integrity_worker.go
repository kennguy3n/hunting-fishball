// integrity_worker.go — Round-14 Task 6.
//
// Periodic background verification of the audit-log hash chain.
// The worker re-computes the head hash for every configured
// tenant on a configurable interval (default 1h). When the
// previously-recorded head no longer matches the freshly-
// computed head, the worker:
//
//   1. Emits an `audit.integrity_violation` audit event so the
//      tamper attempt is itself recorded in the same append-only
//      log (the new event extends the chain and gives operators
//      a forensic anchor).
//   2. Increments the Prometheus counter
//      `context_engine_audit_integrity_violations_total`.
//   3. Writes a structured slog.Error so on-call gets paged
//      through the existing log-pipeline routing.
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
	w.mu.Lock()
	prev, seen := w.observations[tenantID]
	w.observations[tenantID] = integrityObservation{HeadHash: resp.HeadHash, LastEntry: resp.LastEntry}
	w.mu.Unlock()
	if !seen {
		// First observation — pin the head so subsequent runs
		// can compare. No mismatch claim on the bootstrap pass.
		return false, nil
	}
	if prev.HeadHash == resp.HeadHash {
		return false, nil
	}
	// Detect "append-only" growth: the new chain extends a
	// known prefix when the recorded entry_count grew and the
	// previous head should match a prefix re-computation. To
	// keep the worker simple we treat any HEAD divergence as a
	// signal; production paginates over the same window so
	// natural growth between runs does not falsely trip.
	// However, when the LastEntry is a strict suffix of the
	// previous run (entry_count strictly increased) AND
	// ComputeIntegrity is stable across appends, we expect a
	// new head. To avoid false positives in steady-state, we
	// re-compute the chain through the previous lastEntry as
	// a tail check.
	tailMatch, tailErr := w.tailMatches(ctx, tenantID, prev.HeadHash, prev.LastEntry)
	if tailErr == nil && tailMatch {
		return false, nil
	}
	w.reportViolation(ctx, tenantID, prev.HeadHash, resp.HeadHash)
	return true, nil
}

// tailMatches returns true when the chain restricted to entries
// at-or-before the previously recorded `prevLastID` re-hashes to
// `prevHead`. This collapses the common "rows appended since
// last run" case to a no-op without flagging a violation. A
// genuine tamper changes the hash of an existing entry, so the
// re-computation over the same set of historical IDs diverges.
func (w *IntegrityWorker) tailMatches(ctx context.Context, tenantID, prevHead, prevLastID string) (bool, error) {
	if prevLastID == "" || prevHead == "" {
		return false, nil
	}
	res, err := w.repo.List(ctx, ListFilter{TenantID: tenantID, PageSize: maxPageSize})
	if err != nil {
		return false, err
	}
	// Keep only entries at-or-before the previously recorded
	// chain tip. ComputeIntegrity sorts internally, so the input
	// order does not matter.
	older := make([]AuditLog, 0, len(res.Items))
	for _, l := range res.Items {
		if l.ID <= prevLastID {
			older = append(older, l)
		}
	}
	tail := ComputeIntegrity(tenantID, older)
	return tail.HeadHash == prevHead, nil
}

// integrityObservation is the per-tenant snapshot the worker
// keeps between sweeps.
type integrityObservation struct {
	HeadHash  string
	LastEntry string
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
