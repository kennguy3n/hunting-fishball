package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func TestSyncHistory_StartFinishList(t *testing.T) {
	rec := admin.NewInMemorySyncHistoryRecorder()
	if err := rec.Start(context.Background(), "ta", "s1", "run-1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := rec.Finish(context.Background(), "ta", "s1", "run-1", admin.SyncStatusSucceeded, 42, 1); err != nil {
		t.Fatalf("finish: %v", err)
	}
	rows, err := rec.List(context.Background(), "ta", "s1", 50)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != admin.SyncStatusSucceeded || rows[0].DocsProcessed != 42 {
		t.Fatalf("rows wrong: %+v", rows)
	}
}

func TestSyncHistory_TenantIsolation(t *testing.T) {
	rec := admin.NewInMemorySyncHistoryRecorder()
	_ = rec.Start(context.Background(), "ta", "s1", "r1")
	rows, _ := rec.List(context.Background(), "tb", "s1", 50)
	if len(rows) != 0 {
		t.Fatalf("cross-tenant leak: %d", len(rows))
	}
}

func TestSyncHistory_Handler(t *testing.T) {
	rec := admin.NewInMemorySyncHistoryRecorder()
	_ = rec.Start(context.Background(), "ta", "s1", "r1")
	_ = rec.Finish(context.Background(), "ta", "s1", "r1", admin.SyncStatusSucceeded, 5, 0)
	h, err := admin.NewSyncHistoryHandler(rec)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/s1/sync-history", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var body struct {
		Rows []admin.SyncHistoryRow `json:"rows"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Rows) != 1 {
		t.Fatalf("expected 1; got %d", len(body.Rows))
	}
}
