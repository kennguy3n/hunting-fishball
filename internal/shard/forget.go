package shard

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
// equals the caller's token. Mirrors internal/admin/forget_worker.go;
// duplicated here so the shard package can be vendored without
// pulling the admin package's other types.
var releaseLeaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

// TenantSweeper is one storage tier the orchestrator cleans on a
// full-tenant key destruction. Implementations MUST be idempotent —
// the orchestrator may retry after a partial failure.
type TenantSweeper interface {
	// ForgetTenant removes every artefact associated with tenantID.
	// Returns nil on success.
	ForgetTenant(ctx context.Context, tenantID string) error
}

// SweeperFunc adapts a plain function to TenantSweeper.
type SweeperFunc func(ctx context.Context, tenantID string) error

// ForgetTenant implements TenantSweeper.
func (f SweeperFunc) ForgetTenant(ctx context.Context, tenantID string) error {
	return f(ctx, tenantID)
}

// AuditWriter is the narrow seam the orchestrator needs to emit
// audit events. *audit.Repository satisfies it through the
// Create method on the audit.Repository.
type AuditWriter interface {
	Create(ctx context.Context, log *audit.AuditLog) error
}

// ForgetConfig configures the cryptographic forgetting orchestrator.
type ForgetConfig struct {
	Repo *Repository

	// Sweepers is the ordered list of storage tiers to sweep. The
	// orchestrator invokes them sequentially; the first error
	// short-circuits and leaves the tenant in pending_deletion for
	// retry.
	Sweepers []TenantSweeper

	// Lease is the Redis client used for the fenced lease. The
	// lease key is `hf:forget:tenant:<tenant_id>`.
	Lease *redis.Client

	// LeaseTTL caps the orchestrator run time. Defaults to 10 min.
	LeaseTTL time.Duration

	// LeaseKeyPrefix overrides `hf:forget:tenant:`.
	LeaseKeyPrefix string

	// DrainDuration is how long the orchestrator waits between
	// flipping the tenant to pending_deletion and starting the
	// sweep. Allows the pipeline to drain in-flight writes.
	// Defaults to 0 in tests; production wires 30s.
	DrainDuration time.Duration

	// Audit, when non-nil, receives `source.purged`-style events
	// for the tenant lifecycle transitions.
	Audit AuditWriter
}

// ErrLeaseHeld is returned by Forget when another worker already
// holds the tenant lease. Mirrors admin.ErrLeaseHeld.
var ErrLeaseHeld = errors.New("shard: forget lease already held")

// Forget orchestrates the full tenant deletion workflow per
// docs/ARCHITECTURE.md §5:
//
//  1. Mark the tenant pending_deletion (audit event).
//  2. Drain the pipeline (sleep DrainDuration so in-flight writes
//     finish or fail).
//  3. Acquire the fenced lease.
//  4. Run every sweeper in order — Qdrant, FalkorDB, Tantivy, Redis,
//     Postgres chunks, DEKs.
//  5. Mark the tenant deleted (audit event).
//  6. Release the lease.
type Forget struct {
	cfg ForgetConfig
}

// NewForget validates cfg and returns a Forget orchestrator.
func NewForget(cfg ForgetConfig) (*Forget, error) {
	if cfg.Repo == nil {
		return nil, errors.New("shard: nil Repo")
	}
	if cfg.Lease == nil {
		return nil, errors.New("shard: nil Lease")
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 10 * time.Minute
	}
	if cfg.LeaseKeyPrefix == "" {
		cfg.LeaseKeyPrefix = "hf:forget:tenant:"
	}
	return &Forget{cfg: cfg}, nil
}

// Forget executes the workflow for a single tenant. requestedBy is
// the actor (admin user / service account) that initiated the
// destruction; it surfaces in the audit event and the
// tenant_lifecycle row.
func (f *Forget) Forget(ctx context.Context, tenantID, requestedBy string) error {
	if tenantID == "" {
		return ErrMissingTenantScope
	}

	// Step 1: mark pending_deletion. Idempotent — re-issuing the
	// API call simply re-stamps the row.
	if err := f.cfg.Repo.MarkLifecycle(ctx, tenantID, LifecyclePendingDeletion, requestedBy); err != nil {
		return fmt.Errorf("shard: mark pending: %w", err)
	}
	f.emitAudit(ctx, tenantID, requestedBy, "tenant.deletion_requested")

	// Step 2: drain.
	if f.cfg.DrainDuration > 0 {
		t := time.NewTimer(f.cfg.DrainDuration)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}

	// Step 3: acquire fenced lease so two replicas don't
	// double-sweep concurrently.
	leaseKey := f.cfg.LeaseKeyPrefix + tenantID
	tokenBuf := make([]byte, 16)
	if _, err := rand.Read(tokenBuf); err != nil {
		return fmt.Errorf("shard: lease token: %w", err)
	}
	leaseToken := hex.EncodeToString(tokenBuf)

	ok, err := f.cfg.Lease.SetNX(ctx, leaseKey, leaseToken, f.cfg.LeaseTTL).Result()
	if err != nil {
		return fmt.Errorf("shard: lease acquire: %w", err)
	}
	if !ok {
		return ErrLeaseHeld
	}
	defer func() {
		_ = releaseLeaseScript.Run(context.Background(), f.cfg.Lease, []string{leaseKey}, leaseToken).Err()
	}()

	// Step 4: sweepers, in order.
	for i, sw := range f.cfg.Sweepers {
		if err := sw.ForgetTenant(ctx, tenantID); err != nil {
			return fmt.Errorf("shard: sweeper[%d]: %w", i, err)
		}
	}

	// Step 5: mark deleted. The audit event captures the terminal
	// transition.
	if err := f.cfg.Repo.MarkLifecycle(ctx, tenantID, LifecycleDeleted, requestedBy); err != nil {
		return fmt.Errorf("shard: mark deleted: %w", err)
	}
	f.emitAudit(ctx, tenantID, requestedBy, "tenant.deleted")

	return nil
}

func (f *Forget) emitAudit(ctx context.Context, tenantID, requestedBy, action string) {
	if f.cfg.Audit == nil {
		return
	}
	_ = f.cfg.Audit.Create(ctx, audit.NewAuditLog(
		tenantID,
		requestedBy,
		audit.Action(action),
		"tenant",
		tenantID,
		audit.JSONMap{
			"phase": action,
		},
		"",
	))
}
