// Package admin's forget worker is the cleanup half of the source-
// removal flow. The DELETE handler flips status to `removing` and
// enqueues a job; the worker picks the job up, holds a fenced lease,
// and tears down every storage tier the source touched:
//
//   - Qdrant vectors (filtered by source_id)
//   - BM25 (Tantivy) index entries
//   - FalkorDB graph nodes
//   - Postgres metadata (chunks, documents)
//   - Redis cache keys (semantic cache prefix)
//
// The fenced lease is a Redis SET NX EX. Without the lease two
// workers could race when the user re-adds the same source ID — the
// new bucket would be wiped by the in-flight forget job.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// releaseLeaseScript releases a fenced lease iff the current value
// equals the caller's token. An unconditional DEL would let a worker
// whose lease TTL expired delete a freshly-acquired lease held by a
// different worker, defeating the fence.
var releaseLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

// ForgetSweeper is one storage tier the worker cleans. The contract
// is intentionally minimal so each backend (Qdrant, FalkorDB, …) can
// implement it directly without a wrapper. Implementations MUST be
// idempotent — the worker may retry on failure.
type ForgetSweeper interface {
	// ForgetSource removes every artefact for (tenantID, sourceID).
	// Returns nil on success. The worker fans out to all sweepers in
	// sequence; a failure short-circuits and the job is retried.
	ForgetSource(ctx context.Context, tenantID, sourceID string) error
}

// SweeperFunc adapts a plain function to ForgetSweeper. Convenient
// for tests and small inline cleanups.
type SweeperFunc func(ctx context.Context, tenantID, sourceID string) error

// ForgetSource implements ForgetSweeper.
func (f SweeperFunc) ForgetSource(ctx context.Context, tenantID, sourceID string) error {
	return f(ctx, tenantID, sourceID)
}

// ForgetWorkerConfig configures a ForgetWorker.
type ForgetWorkerConfig struct {
	// Repo is the sources repository. The worker calls MarkRemoved on
	// successful completion.
	Repo *SourceRepository

	// Sweepers is the ordered list of storage tiers to sweep. The
	// worker invokes them in order; the first error aborts the run
	// and the lease releases (the job is requeued).
	Sweepers []ForgetSweeper

	// Lease is the Redis client used for the fenced lease. The lease
	// key is `hf:forget:<tenant>:<source>`.
	Lease *redis.Client

	// LeaseTTL is the lease duration. Must comfortably exceed the
	// worst-case sweep time. Default 5 minutes.
	LeaseTTL time.Duration

	// LeaseKeyPrefix overrides the default `hf:forget:`.
	LeaseKeyPrefix string

	// Audit, when non-nil, receives a `source.purged` event when the
	// run completes successfully.
	Audit AuditWriter
}

// ForgetWorker carries out the cleanup. Workers are stateless apart
// from their configuration; multiple replicas can run concurrently
// and the lease prevents double-execution.
type ForgetWorker struct {
	cfg ForgetWorkerConfig
}

// NewForgetWorker validates cfg and returns a worker.
func NewForgetWorker(cfg ForgetWorkerConfig) (*ForgetWorker, error) {
	if cfg.Repo == nil {
		return nil, errors.New("forget: nil Repo")
	}
	if cfg.Lease == nil {
		return nil, errors.New("forget: nil Lease (Redis client)")
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 5 * time.Minute
	}
	if cfg.LeaseKeyPrefix == "" {
		cfg.LeaseKeyPrefix = "hf:forget:"
	}
	return &ForgetWorker{cfg: cfg}, nil
}

// ForgetJob is the unit of work passed to Run.
type ForgetJob struct {
	TenantID string
	SourceID string
	// Actor is the operator who initiated the removal; surfaces in
	// the audit event.
	Actor string
}

// ErrLeaseHeld is returned by Run when another worker already holds
// the lease for (tenant, source). The caller should NOT retry
// immediately — the in-flight worker is responsible for completion.
var ErrLeaseHeld = errors.New("admin: forget lease already held")

// Run executes one ForgetJob. The flow is:
//
//  1. Acquire Redis SET NX EX lease.
//  2. Run every sweeper in order.
//  3. Mark the source row `removed`.
//  4. Emit `source.purged` audit event.
//  5. Release the lease.
//
// Any error in step 2 or 3 leaves the source in `removing` and the
// job will be retried on the next poll. Step 4 errors are logged but
// do not roll back — the source IS removed; we just lost an audit
// emission.
func (w *ForgetWorker) Run(ctx context.Context, job ForgetJob) error {
	if job.TenantID == "" || job.SourceID == "" {
		return errors.New("forget: missing tenant/source")
	}
	leaseKey := w.cfg.LeaseKeyPrefix + job.TenantID + ":" + job.SourceID
	// Random token, not a timestamp: two workers acquiring within the
	// same nanosecond could otherwise share a token and defeat the
	// compare-and-delete on release.
	tokenBuf := make([]byte, 16)
	if _, err := rand.Read(tokenBuf); err != nil {
		return fmt.Errorf("forget: lease token: %w", err)
	}
	leaseToken := hex.EncodeToString(tokenBuf)

	ok, err := w.cfg.Lease.SetNX(ctx, leaseKey, leaseToken, w.cfg.LeaseTTL).Result()
	if err != nil {
		return fmt.Errorf("forget: lease acquire: %w", err)
	}
	if !ok {
		return ErrLeaseHeld
	}
	defer func() {
		// Compare-and-delete: only release if the lease still holds
		// our token. Guards against the case where this run exceeds
		// the lease TTL, a second worker acquires a fresh lease, and
		// our deferred release would otherwise wipe the new holder's
		// lease. Best-effort — the TTL reaps stale leases on crash.
		_ = releaseLeaseScript.Run(context.Background(), w.cfg.Lease, []string{leaseKey}, leaseToken).Err()
	}()

	// Verify the source is in `removing` — defends against the user
	// re-creating a source with the same ID while the job sat in the
	// queue.
	current, err := w.cfg.Repo.Get(ctx, job.TenantID, job.SourceID)
	if err != nil {
		return fmt.Errorf("forget: load source: %w", err)
	}
	if current.Status != SourceStatusRemoving {
		return fmt.Errorf("forget: source %s is %q, refusing to sweep", current.ID, current.Status)
	}

	for i, sw := range w.cfg.Sweepers {
		if err := sw.ForgetSource(ctx, job.TenantID, job.SourceID); err != nil {
			return fmt.Errorf("forget: sweeper[%d]: %w", i, err)
		}
	}

	if err := w.cfg.Repo.MarkRemoved(ctx, job.TenantID, job.SourceID); err != nil {
		return fmt.Errorf("forget: mark removed: %w", err)
	}

	if w.cfg.Audit != nil {
		_ = w.cfg.Audit.Create(ctx, &audit.AuditLog{
			TenantID:     job.TenantID,
			ActorID:      job.Actor,
			Action:       audit.ActionSourcePurged,
			ResourceType: "source",
			ResourceID:   job.SourceID,
			Metadata: audit.JSONMap{
				"phase":     "completed",
				"source_id": job.SourceID,
			},
		})
	}
	return nil
}
