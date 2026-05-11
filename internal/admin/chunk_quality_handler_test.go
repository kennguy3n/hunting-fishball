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

func TestChunkQualityHandler_AggregateAndIsolate(t *testing.T) {
	store := admin.NewInMemoryChunkQualityStore()
	store.Insert(context.Background(), admin.ChunkQualityRow{TenantID: "ta", SourceID: "s1", ChunkID: "c1", QualityScore: 0.9})
	store.Insert(context.Background(), admin.ChunkQualityRow{TenantID: "ta", SourceID: "s1", ChunkID: "c2", QualityScore: 0.3, Duplicate: true})
	store.Insert(context.Background(), admin.ChunkQualityRow{TenantID: "ta", SourceID: "s2", ChunkID: "c3", QualityScore: 0.8})
	store.Insert(context.Background(), admin.ChunkQualityRow{TenantID: "tb", SourceID: "s1", ChunkID: "x", QualityScore: 0.1})
	h, _ := admin.NewChunkQualityHandler(store)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/chunks/quality-report", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp admin.ChunkQualityReport
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Buckets) != 2 {
		t.Fatalf("expected 2 buckets for ta; got %d (%v)", len(resp.Buckets), resp.Buckets)
	}
	for _, b := range resp.Buckets {
		if b.SourceID == "s1" {
			if b.Count != 2 || b.BelowHalf != 1 || b.Duplicate != 1 {
				t.Fatalf("s1 wrong: %+v", b)
			}
		}
	}
}
