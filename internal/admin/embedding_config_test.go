package admin_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

// authStubRouter is a tiny Gin engine with a fake auth middleware
// injecting tenantID via audit.TenantContextKey. The package's main
// `router(t, h, tenantID)` helper requires a real *admin.Handler;
// these handlers don't need that wiring.
func authStubRouter(tenantID string) *gin.Engine {
	r := gin.New()
	if tenantID != "" {
		r.Use(func(c *gin.Context) {
			c.Set(audit.TenantContextKey, tenantID)
			c.Set(audit.ActorContextKey, "actor-1")
			c.Next()
		})
	}
	return r
}

const sqliteEmbeddingSchema = `
CREATE TABLE source_embedding_config (
    tenant_id    TEXT NOT NULL,
    source_id    TEXT NOT NULL,
    model_name   TEXT NOT NULL,
    dimensions   INTEGER NOT NULL DEFAULT 0,
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NOT NULL,
    PRIMARY KEY (tenant_id, source_id)
);
`

func newSQLiteEmbeddingRepo(t *testing.T) *admin.EmbeddingConfigRepository {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	if err := db.Exec(sqliteEmbeddingSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return admin.NewEmbeddingConfigRepository(db)
}

func TestEmbeddingConfigRepository_UpsertGet(t *testing.T) {
	repo := newSQLiteEmbeddingRepo(t)
	ctx := t.Context()
	if _, err := repo.Get(ctx, "t", "s"); err == nil {
		t.Fatalf("expected not-found")
	}
	cfg := &admin.SourceEmbeddingConfig{TenantID: "t", SourceID: "s", ModelName: "openai-3-large", Dimensions: 1536}
	if err := repo.Upsert(ctx, cfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.Get(ctx, "t", "s")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ModelName != "openai-3-large" || got.Dimensions != 1536 {
		t.Fatalf("unexpected: %+v", got)
	}
	// Update
	cfg.ModelName = "voyage-2"
	cfg.Dimensions = 1024
	if err := repo.Upsert(ctx, cfg); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	got, _ = repo.Get(ctx, "t", "s")
	if got.ModelName != "voyage-2" {
		t.Fatalf("update didn't take: %+v", got)
	}
}

func TestEmbeddingConfigRepository_Validation(t *testing.T) {
	repo := newSQLiteEmbeddingRepo(t)
	cases := []*admin.SourceEmbeddingConfig{
		nil,
		{SourceID: "s", ModelName: "m"},
		{TenantID: "t", ModelName: "m"},
		{TenantID: "t", SourceID: "s"},
		{TenantID: "t", SourceID: "s", ModelName: "m", Dimensions: -1},
	}
	for i, tc := range cases {
		if err := repo.Upsert(t.Context(), tc); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
}

func TestEmbeddingConfigHandler_GetMissing(t *testing.T) {
	repo := newSQLiteEmbeddingRepo(t)
	h, err := admin.NewEmbeddingConfigHandler(repo, nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/embedding", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}

func TestEmbeddingConfigHandler_PutAndGet(t *testing.T) {
	repo := newSQLiteEmbeddingRepo(t)
	auditSink := &fakeAudit{}
	h, err := admin.NewEmbeddingConfigHandler(repo, auditSink)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	body, _ := json.Marshal(map[string]any{"model_name": "voyage-2", "dimensions": 1024})
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/sources/src-1/embedding", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", rr.Code, rr.Body)
	}
	if got := auditSink.actions(); len(got) != 1 || got[0] != audit.ActionPolicyEdited {
		t.Fatalf("expected one policy.edited audit; got %v", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/embedding", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body)
	}
	var cfg admin.SourceEmbeddingConfig
	_ = json.Unmarshal(rr.Body.Bytes(), &cfg)
	if cfg.ModelName != "voyage-2" {
		t.Fatalf("get returned wrong model: %v", cfg.ModelName)
	}
}

func TestEmbeddingConfigResolver(t *testing.T) {
	repo := newSQLiteEmbeddingRepo(t)
	if name, dims := repo.ResolveEmbeddingModel(t.Context(), "t", "s"); name != "" || dims != 0 {
		t.Fatalf("missing should fall back to defaults; got %q/%d", name, dims)
	}
	if err := repo.Upsert(t.Context(), &admin.SourceEmbeddingConfig{TenantID: "t", SourceID: "s", ModelName: "voyage", Dimensions: 1024}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if name, dims := repo.ResolveEmbeddingModel(t.Context(), "t", "s"); name != "voyage" || dims != 1024 {
		t.Fatalf("resolver returned wrong model: %v/%d", name, dims)
	}
	// Cross-tenant returns empty.
	if name, _ := repo.ResolveEmbeddingModel(t.Context(), "other", "s"); name != "" {
		t.Fatalf("cross-tenant must miss: got %v", name)
	}
}
