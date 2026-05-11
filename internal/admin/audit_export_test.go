package admin_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeAuditReader struct {
	rows []audit.AuditLog
}

func (f *fakeAuditReader) List(_ context.Context, filter audit.ListFilter) (*audit.ListResult, error) {
	var out []audit.AuditLog
	for _, r := range f.rows {
		if r.TenantID != filter.TenantID {
			continue
		}
		out = append(out, r)
	}
	return &audit.ListResult{Items: out}, nil
}

func TestAuditExport_CSV(t *testing.T) {
	r := &fakeAuditReader{rows: []audit.AuditLog{
		{ID: "1", TenantID: "ta", ActorID: "u1", Action: audit.ActionSourceConnected, ResourceType: "source", ResourceID: "s1", CreatedAt: time.Unix(100, 0).UTC()},
		{ID: "2", TenantID: "ta", ActorID: "u2", Action: audit.ActionSourcePaused, ResourceType: "source", ResourceID: "s2", CreatedAt: time.Unix(200, 0).UTC()},
		{ID: "3", TenantID: "tb", ActorID: "x", Action: audit.ActionSourceConnected, ResourceType: "source", ResourceID: "s3", CreatedAt: time.Unix(300, 0).UTC()},
	}}
	h, err := admin.NewAuditExportHandler(r, nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(router.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/export?format=csv", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	reader := csv.NewReader(strings.NewReader(w.Body.String()))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v", err)
	}
	if len(rows) != 3 { // header + 2 ta rows; tb filtered out
		t.Fatalf("expected 3 rows (header + 2); got %d", len(rows))
	}
	if rows[0][0] != "id" {
		t.Fatalf("expected header row first; got %v", rows[0])
	}
}

func TestAuditExport_JSONL(t *testing.T) {
	r := &fakeAuditReader{rows: []audit.AuditLog{
		{ID: "1", TenantID: "ta", Action: audit.ActionSourceConnected},
		{ID: "2", TenantID: "ta", Action: audit.ActionSourcePaused},
	}}
	h, _ := admin.NewAuditExportHandler(r, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(router.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/export?format=jsonl", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines; got %d", len(lines))
	}
	var log audit.AuditLog
	if err := json.Unmarshal([]byte(lines[0]), &log); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if log.ID != "1" {
		t.Fatalf("expected id=1; got %s", log.ID)
	}
}

func TestAuditExport_InvalidFormat(t *testing.T) {
	r := &fakeAuditReader{}
	h, _ := admin.NewAuditExportHandler(r, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(router.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit/export?format=xml", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", w.Code)
	}
}
