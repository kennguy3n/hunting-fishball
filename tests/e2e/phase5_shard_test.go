//go:build e2e

// Phase 5 shard-sync e2e surfaces. Exercises the three GET endpoints
// the on-device shard sync client polls (`/v1/shards/:tenant_id`,
// `/v1/shards/:tenant_id/delta?since=`, `/v1/shards/:tenant_id/coverage`)
// against a real Postgres + the GORM repositories that back them.
//
// These tests sit between smoke_test.go (which only writes through
// the storer) and phase234_test.go (which only exercises admin /
// retrieval surfaces). They use unique ULID tenants so they can run
// in parallel against the shared docker-compose Postgres without
// interfering with each other.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/shard"
)

// TestSmoke_ShardManifest writes one manifest through the GORM
// repository and verifies GET /v1/shards/:tenant_id returns it.
// The tenant guard from the gin context middleware must allow the
// query through (matching path-supplied tenant id).
func TestSmoke_ShardManifest(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)
	repo := shard.NewRepository(db)

	manifest := &shard.ShardManifest{
		TenantID:     tenantID,
		PrivacyMode:  "local-only",
		ShardVersion: 1,
		ChunksCount:  3,
		Status:       shard.ShardStatusReady,
	}
	if err := repo.Create(t.Context(), manifest); err != nil {
		t.Fatalf("Create manifest: %v", err)
	}

	h, err := shard.NewHandler(shard.HandlerConfig{Repo: repo})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/shards/"+tenantID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Shards []shard.ShardManifest `json:"shards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Shards) == 0 {
		t.Fatalf("expected ≥1 shard for tenant=%s body=%s", tenantID, w.Body.String())
	}
	got := resp.Shards[0]
	if got.TenantID != tenantID || got.ShardVersion != 1 || got.PrivacyMode != "local-only" {
		t.Errorf("unexpected manifest: %+v", got)
	}
}

// TestSmoke_ShardDelta seeds two manifest versions for the same
// (tenant, privacy_mode) tuple and asserts the delta endpoint
// returns "add" ops for the newer version when queried with
// ?since=<older>. The contract: the client only pulls the new
// shard, never re-syncs an old one.
func TestSmoke_ShardDelta(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)
	repo := shard.NewRepository(db)

	// Seed v1 → v2 for the same scope. Each version must use a
	// distinct status to satisfy the unique constraint on
	// (tenant, user, channel, privacy_mode, version).
	for v := int64(1); v <= 2; v++ {
		m := &shard.ShardManifest{
			TenantID:     tenantID,
			PrivacyMode:  "local-only",
			ShardVersion: v,
			ChunksCount:  int(v) * 5,
			Status:       shard.ShardStatusReady,
		}
		if err := repo.Create(t.Context(), m); err != nil {
			t.Fatalf("Create v%d: %v", v, err)
		}
	}

	h, err := shard.NewHandler(shard.HandlerConfig{Repo: repo})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/shards/"+tenantID+"/delta?since=1&privacy_mode=local-only", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	// The delta payload is implementation-defined; we only assert
	// that the response references shard_version 2 — the only
	// version a client at v1 should learn about.
	if !contains(w.Body.String(), `"shard_version":2`) {
		t.Fatalf("delta did not include v2: %s", w.Body.String())
	}
}

// TestSmoke_ShardCoverage exercises the coverage endpoint. Wires
// the CoverageRepoGORM that cmd/api/main.go uses in production so
// the response carries IsAuthoritative=true (a corpus side count
// is observable). Without this wiring the endpoint returns
// reason=corpus_size_unknown, which would mask a regression in the
// chunks-table query.
func TestSmoke_ShardCoverage(t *testing.T) {
	requireE2E(t)
	t.Parallel()

	db := openE2EDB(t)
	tenantID := uniqueTenant(t)
	repo := shard.NewRepository(db)

	// Seed one shard manifest so the response surface has a
	// non-zero shard side. The corpus side counts the
	// `chunks` table the storer writes through; smoke_test.go's
	// happy-path leaves >= 1 row from a previous run if it
	// shares the database, but we don't depend on that here.
	if err := repo.Create(t.Context(), &shard.ShardManifest{
		TenantID:     tenantID,
		PrivacyMode:  "local-only",
		ShardVersion: 1,
		ChunksCount:  1,
		Status:       shard.ShardStatusReady,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	coverageRepo := shard.NewCoverageRepoGORM(db)
	h, err := shard.NewHandler(shard.HandlerConfig{
		Repo:         repo,
		CoverageRepo: coverageRepo,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := newTenantRouter(t, tenantID)
	h.Register(&r.RouterGroup)

	w := doRoute(r, http.MethodGet, "/v1/shards/"+tenantID+"/coverage?privacy_mode=local-only", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Validate the structural contract of the response. The
	// coverage handler's exact JSON shape is documented in
	// internal/shard/handler.go's coverage method.
	for _, want := range []string{`"shard_chunks"`, `"corpus_chunks"`} {
		if !contains(body, want) {
			t.Errorf("coverage response missing %s; body=%s", want, body)
		}
	}
}

// contains is a tiny helper so the e2e tests do not pull in a
// strings import for one-line substring checks.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// helpers below: simple wrappers around fmt/time to make assertions
// in the tests above readable. Kept at file scope so other phase5_*
// tests can reuse them.

func mustFmtTenant(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// _ keeps gin imported even on platforms where the helpers above
// don't reach for the gin context directly. Removing the alias
// would make the import unused on a stripped-down build.
var _ = gin.HandlerFunc(nil)
