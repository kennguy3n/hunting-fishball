package admin_test

import (
	"context"
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

func TestCacheTTLStore_FallbackAndOverride(t *testing.T) {
	s := admin.NewInMemoryCacheTTLStore()
	if d := s.TTLFor(context.Background(), "ta", 5*time.Second); d != 5*time.Second {
		t.Fatalf("expected fallback; got %v", d)
	}
	_ = s.Put(context.Background(), &admin.CacheConfig{TenantID: "ta", TTLMS: 2000})
	if d := s.TTLFor(context.Background(), "ta", 5*time.Second); d != 2*time.Second {
		t.Fatalf("expected override; got %v", d)
	}
}

func TestCacheConfigHandler_PutGet(t *testing.T) {
	store := admin.NewInMemoryCacheTTLStore()
	h, _ := admin.NewCacheConfigHandler(store, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	body := `{"ttl_ms":1500,"notes":"prod"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v1/admin/tenants/ta/cache-config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT: %d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/ta/cache-config", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d", w.Code)
	}
	var c admin.CacheConfig
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.TTLMS != 1500 {
		t.Fatalf("expected 1500; got %d", c.TTLMS)
	}
}

func TestCacheConfigHandler_CrossTenantDenied(t *testing.T) {
	store := admin.NewInMemoryCacheTTLStore()
	h, _ := admin.NewCacheConfigHandler(store, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(audit.TenantContextKey, "ta"); c.Next() })
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tb/cache-config", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403; got %d", w.Code)
	}
}
