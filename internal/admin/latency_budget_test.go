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

func TestLatencyBudgetStore_PutGet(t *testing.T) {
	s := admin.NewInMemoryLatencyBudgetStore()
	if err := s.Put(context.Background(), &admin.LatencyBudget{TenantID: "ta", MaxLatencyMS: 800}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(context.Background(), "ta")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MaxLatencyMS != 800 {
		t.Fatalf("expected 800ms; got %d", got.MaxLatencyMS)
	}
}

func TestLatencyBudgetStore_NotFound(t *testing.T) {
	s := admin.NewInMemoryLatencyBudgetStore()
	_, err := s.Get(context.Background(), "ta")
	if err != admin.ErrLatencyBudgetNotFound {
		t.Fatalf("expected ErrLatencyBudgetNotFound; got %v", err)
	}
}

func TestLatencyBudgetHandler_GetPut(t *testing.T) {
	store := admin.NewInMemoryLatencyBudgetStore()
	h, _ := admin.NewLatencyBudgetHandler(store, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))

	// 404 before
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/ta/latency-budget", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", w.Code)
	}
	// put
	body := `{"max_latency_ms":750,"p95_target_ms":750,"notes":"production"}`
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/ta/latency-budget", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d body=%s", w.Code, w.Body.String())
	}
	// get
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/ta/latency-budget", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	var b admin.LatencyBudget
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if b.MaxLatencyMS != 750 {
		t.Fatalf("expected 750; got %d", b.MaxLatencyMS)
	}
}

func TestLatencyBudgetHandler_CrossTenantDenied(t *testing.T) {
	store := admin.NewInMemoryLatencyBudgetStore()
	h, _ := admin.NewLatencyBudgetHandler(store, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tb/latency-budget", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403; got %d", w.Code)
	}
}

func TestLatencyBudgetHandler_InvalidValue(t *testing.T) {
	store := admin.NewInMemoryLatencyBudgetStore()
	h, _ := admin.NewLatencyBudgetHandler(store, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	body := `{"max_latency_ms":-5}`
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/ta/latency-budget", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", w.Code)
	}
}
