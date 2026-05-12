package audit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

const integritySQLiteSchema = `
CREATE TABLE audit_logs (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    actor_id      TEXT,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT,
    metadata      TEXT NOT NULL DEFAULT '{}',
    trace_id      TEXT,
    created_at    DATETIME NOT NULL,
    published_at  DATETIME
);
CREATE INDEX idx_audit_tenant_created_integrity ON audit_logs (tenant_id, id DESC);
`

func newAuditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Exec(integritySQLiteSchema).Error; err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestComputeIntegrity_EmptyChainIsDeterministic(t *testing.T) {
	t.Parallel()
	a := audit.ComputeIntegrity("tenant-1", nil)
	b := audit.ComputeIntegrity("tenant-1", nil)
	if a.HeadHash == "" || a.HeadHash != b.HeadHash {
		t.Fatalf("empty chain hashes diverged: %s vs %s", a.HeadHash, b.HeadHash)
	}
	if a.EntryCount != 0 {
		t.Fatalf("empty chain entry_count=%d", a.EntryCount)
	}
}

func TestComputeIntegrity_OrderInsensitive(t *testing.T) {
	t.Parallel()
	rows := []audit.AuditLog{
		{ID: "01H0000000000000000000000A", TenantID: "t", Action: "x", ResourceType: "r"},
		{ID: "01H0000000000000000000000B", TenantID: "t", Action: "y", ResourceType: "r"},
		{ID: "01H0000000000000000000000C", TenantID: "t", Action: "z", ResourceType: "r"},
	}
	rev := []audit.AuditLog{rows[2], rows[1], rows[0]}
	a := audit.ComputeIntegrity("t", rows)
	b := audit.ComputeIntegrity("t", rev)
	if a.HeadHash != b.HeadHash {
		t.Fatalf("hash should be order-insensitive: %s vs %s", a.HeadHash, b.HeadHash)
	}
	if a.EntryCount != 3 {
		t.Fatalf("entry_count=%d want 3", a.EntryCount)
	}
}

func TestComputeIntegrity_DetectsTampering(t *testing.T) {
	t.Parallel()
	a := audit.ComputeIntegrity("t", []audit.AuditLog{
		{ID: "01H0000000000000000000000A", TenantID: "t", Action: "x"},
		{ID: "01H0000000000000000000000B", TenantID: "t", Action: "y"},
	})
	b := audit.ComputeIntegrity("t", []audit.AuditLog{
		{ID: "01H0000000000000000000000A", TenantID: "t", Action: "x"},
		{ID: "01H0000000000000000000000B", TenantID: "t", Action: "y-tampered"},
	})
	if a.HeadHash == b.HeadHash {
		t.Fatalf("tampered chain produced identical hash: %s", a.HeadHash)
	}
}

func TestComputeIntegrity_NewRowChangesHeadButNotPrefix(t *testing.T) {
	t.Parallel()
	base := []audit.AuditLog{
		{ID: "01H0000000000000000000000A", TenantID: "t", Action: "x"},
		{ID: "01H0000000000000000000000B", TenantID: "t", Action: "y"},
	}
	extended := append(base, audit.AuditLog{ID: "01H0000000000000000000000C", TenantID: "t", Action: "z"})
	a := audit.ComputeIntegrity("t", base)
	b := audit.ComputeIntegrity("t", extended)
	if a.HeadHash == b.HeadHash {
		t.Fatalf("appended chain should change head hash")
	}
	if a.EntryCount != 2 || b.EntryCount != 3 {
		t.Fatalf("entry counts wrong: a=%d b=%d", a.EntryCount, b.EntryCount)
	}
}

func TestIntegrityHandler_Endpoint(t *testing.T) {
	t.Parallel()
	db := newAuditDB(t)
	repo := audit.NewRepository(db)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		_ = repo.Create(ctx, &audit.AuditLog{
			ID:           "01H000000000000000000000A" + string(rune('0'+i)),
			TenantID:     "t-a",
			Action:       "x.y",
			ResourceType: "thing",
			CreatedAt:    now,
		})
	}
	h, err := audit.NewIntegrityHandler(repo)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "t-a"); c.Next() })
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/integrity", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var body audit.IntegrityResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.EntryCount != 3 {
		t.Fatalf("entry_count=%d", body.EntryCount)
	}
	if body.HeadHash == "" {
		t.Fatalf("missing head_hash")
	}
}
