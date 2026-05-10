//go:build e2e

// Phase 5 cryptographic-forget e2e surface. Exercises the
// `DELETE /v1/tenants/:tenant_id/keys` endpoint end-to-end:
// indexes one document, asserts the tenant has at least one shard
// manifest, calls the forget endpoint, and asserts the subsequent
// GET /v1/shards/:tenant_id query returns empty.
//
// The test uses a stub ForgetTrigger that flips the manifest's
// status to "superseded" so the read path returns no rows; the
// real production trigger destroys per-tenant DEKs which is a
// side-effect we cannot exercise without a credential store.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// recordingForget tracks calls so the test can assert the trigger
// fired exactly once with the expected tenant_id. After Forget it
// flips every manifest for the tenant to status="superseded" via
// the shared *gorm.DB, simulating the production sweep.
type recordingForget struct {
	db    *gorm.DB
	calls atomic.Int64
}

func (f *recordingForget) Forget(ctx context.Context, tenantID, _ string) error {
	f.calls.Add(1)
	// Production sweepers destroy the per-tenant DEKs — we cannot
	// exercise that here, but flipping the manifest status to
	// "superseded" mirrors the read-path effect (the default
	// list filter only returns ready rows).
	return f.db.WithContext(ctx).
		Table("shards").
		Where("tenant_id = ?", tenantID).
		Update("status", string(shard.ShardStatusSuperseded)).
		Error
}

// TestSmoke_CryptographicForget seeds shard manifests, calls the
// forget endpoint, and asserts the subsequent list returns empty.
func TestSmoke_CryptographicForget(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)
	repo := shard.NewRepository(db)

	// Seed two manifests so the post-forget assertion is
	// non-trivial (a tenant with zero rows always lists empty).
	for v := int64(1); v <= 2; v++ {
		if err := repo.Create(t.Context(), &shard.ShardManifest{
			TenantID:     tenantID,
			PrivacyMode:  "local-only",
			ShardVersion: v,
			ChunksCount:  3,
			Status:       shard.ShardStatusReady,
		}); err != nil {
			t.Fatalf("seed v%d: %v", v, err)
		}
	}

	rf := &recordingForget{db: db}
	h, err := shard.NewHandler(shard.HandlerConfig{Repo: repo, Forget: rf})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodDelete, "/v1/tenants/"+tenantID+"/keys", nil)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent && w.Code != http.StatusAccepted {
		t.Fatalf("forget status=%d body=%s", w.Code, w.Body.String())
	}
	if rf.calls.Load() != 1 {
		t.Fatalf("Forget called %d times; want 1", rf.calls.Load())
	}

	// After forget, the list endpoint defaults to status=ready,
	// so superseded rows are filtered out.
	listW := doRoute(r, http.MethodGet, "/v1/shards/"+tenantID, nil)
	if listW.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listW.Code, listW.Body.String())
	}
	var resp struct {
		Shards []shard.ShardManifest `json:"shards"`
	}
	if err := json.Unmarshal(listW.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(resp.Shards) != 0 {
		t.Fatalf("post-forget list should be empty, got %d rows", len(resp.Shards))
	}
}
