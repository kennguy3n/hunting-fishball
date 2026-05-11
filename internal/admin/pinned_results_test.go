package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func TestPinnedResultStore_CRUD(t *testing.T) {
	s := admin.NewInMemoryPinnedResultStore(nil)
	p, err := s.Create(context.Background(), admin.PinnedResult{TenantID: "ta", QueryPattern: "hello", ChunkID: "c1", Position: 0})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.ID == "" {
		t.Fatalf("expected generated id")
	}
	rows, _ := s.List(context.Background(), "ta")
	if len(rows) != 1 {
		t.Fatalf("expected 1; got %d", len(rows))
	}
	if err := s.Delete(context.Background(), "ta", p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestPinnedResultStore_TenantIsolation(t *testing.T) {
	s := admin.NewInMemoryPinnedResultStore(nil)
	_, _ = s.Create(context.Background(), admin.PinnedResult{TenantID: "ta", QueryPattern: "q", ChunkID: "c1"})
	rows, _ := s.List(context.Background(), "tb")
	if len(rows) != 0 {
		t.Fatalf("cross-tenant leak: %d", len(rows))
	}
}

func TestPinnedResultsHandler_CRUD(t *testing.T) {
	store := admin.NewInMemoryPinnedResultStore(nil)
	h, _ := admin.NewPinnedResultsHandler(store, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"query_pattern":"foo","chunk_id":"c1","position":0}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/retrieval/pins", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d body=%s", w.Code, w.Body.String())
	}
	var pin admin.PinnedResult
	_ = json.Unmarshal(w.Body.Bytes(), &pin)
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/retrieval/pins", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/admin/retrieval/pins/"+pin.ID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
}
