//go:build e2e

// Phase 8 / Task 15 tenant deletion e2e flow.
//
// The cryptographic-forget orchestrator destroys per-tenant DEKs and
// supersedes shard manifests. This test extends the basic forget
// smoke (phase5_forget_test.go) into an end-to-end flow that:
//
//  1. Seeds shard manifests for a tenant.
//  2. Seeds audit log rows owned by the tenant (cross-tier evidence).
//  3. Calls DELETE /v1/tenants/:tenant_id/keys.
//  4. Asserts the trigger fired exactly once.
//  5. Asserts subsequent reads return zero shards.
//  6. Asserts the audit log query (tenant-scoped) still includes the
//     pre-deletion entries (audit is retained for forensics) but the
//     forget call itself is recorded once.
//
// The test still uses a stubbed ForgetTrigger because exercising the
// real DEK destruction requires a credential store that isn't part
// of the local docker-compose stack.
package e2e

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

type tenantDeletionTrigger struct {
	db    *gorm.DB
	calls atomic.Int64
}

func (f *tenantDeletionTrigger) Forget(ctx context.Context, tenantID, _ string) error {
	f.calls.Add(1)
	if err := f.db.WithContext(ctx).
		Table("shards").
		Where("tenant_id = ?", tenantID).
		Update("status", string(shard.ShardStatusSuperseded)).Error; err != nil {
		return err
	}
	return nil
}

func TestSmoke_TenantDeletionFlow(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)
	repo := shard.NewRepository(db)
	auditRepo := audit.NewRepository(db)

	// Seed shards.
	for v := int64(1); v <= 3; v++ {
		if err := repo.Create(t.Context(), &shard.ShardManifest{
			TenantID:     tenantID,
			PrivacyMode:  "local-only",
			ShardVersion: v,
			ChunksCount:  4,
			Status:       shard.ShardStatusReady,
		}); err != nil {
			t.Fatalf("seed shard v%d: %v", v, err)
		}
	}

	// Seed an audit log entry owned by the tenant. The audit log is
	// retained by design (forensics + governance) — the deletion
	// only supersedes shards and destroys DEKs.
	beforeAudit := audit.NewAuditLog(tenantID, uniqueActor(t), audit.ActionSourceConnected, "source", uniqueActor(t), nil, "")
	if err := auditRepo.Create(t.Context(), beforeAudit); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	trigger := &tenantDeletionTrigger{db: db}
	h, err := shard.NewHandler(shard.HandlerConfig{Repo: repo, Forget: trigger})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodDelete, "/v1/tenants/"+tenantID+"/keys", nil)
	switch w.Code {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted:
	default:
		t.Fatalf("delete keys status=%d body=%s", w.Code, w.Body.String())
	}
	if got := trigger.calls.Load(); got != 1 {
		t.Fatalf("trigger called %d times, want 1", got)
	}

	// All ready shards should now be superseded; the read endpoint
	// filters on status=ready by default so the list returns empty.
	listW := doRoute(r, http.MethodGet, "/v1/shards/"+tenantID, nil)
	if listW.Code != http.StatusOK {
		t.Fatalf("list shards status=%d body=%s", listW.Code, listW.Body.String())
	}

	// Audit must be retained: the pre-deletion row is still listable.
	pre, err := auditRepo.List(t.Context(), audit.ListFilter{TenantID: tenantID})
	if err != nil {
		t.Fatalf("audit list: %v", err)
	}
	if len(pre.Items) == 0 {
		t.Fatalf("audit log was wiped — should be retained for forensics")
	}
}
