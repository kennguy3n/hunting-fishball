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

type stubSourceProvider struct {
	stats admin.AnalyticsSourceStats
}

func (s *stubSourceProvider) GlobalSourceStats(context.Context) (admin.AnalyticsSourceStats, error) {
	return s.stats, nil
}

type stubDocProvider struct{ total int64 }

func (s *stubDocProvider) TotalDocuments(context.Context) (int64, error) {
	return s.total, nil
}

type stubStorageProvider struct {
	stats admin.AnalyticsStorageStats
}

func (s *stubStorageProvider) GlobalStorageStats(context.Context) (admin.AnalyticsStorageStats, error) {
	return s.stats, nil
}

type stubDLQProvider struct{ depth int64 }

func (s *stubDLQProvider) DLQDepth(context.Context) (int64, error) {
	return s.depth, nil
}

func newAnalyticsRig(t *testing.T, role admin.Role) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewAnalyticsHandler(admin.AnalyticsConfig{
		Sources: &stubSourceProvider{stats: admin.AnalyticsSourceStats{
			TotalTenants: 5, TotalActiveSources: 12,
		}},
		Documents: &stubDocProvider{total: 9001},
		Storage: &stubStorageProvider{stats: admin.AnalyticsStorageStats{
			TotalChunks: 42000,
			PerBackend:  map[string]int64{"qdrant": 30000, "bleve": 12000},
		}},
		DLQ: &stubDLQProvider{depth: 7},
	})
	if err != nil {
		t.Fatalf("NewAnalyticsHandler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Set(admin.RoleContextKey, string(role))
		c.Next()
	})
	rg := r.Group("/v1/admin")
	h.Register(rg)
	return r
}

func TestAnalytics_SuperAdminHappyPath(t *testing.T) {
	t.Parallel()
	r := newAnalyticsRig(t, admin.RoleSuperAdmin)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/global", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp admin.GlobalAnalyticsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TotalTenants != 5 || resp.TotalActiveSources != 12 {
		t.Fatalf("source stats: %+v", resp)
	}
	if resp.TotalDocuments != 9001 {
		t.Fatalf("total_documents: %d", resp.TotalDocuments)
	}
	if resp.TotalChunks != 42000 {
		t.Fatalf("total_chunks: %d", resp.TotalChunks)
	}
	if resp.DLQDepth != 7 {
		t.Fatalf("dlq_depth: %d", resp.DLQDepth)
	}
	if resp.PerBackendStorage["qdrant"] != 30000 {
		t.Fatalf("per_backend qdrant: %d", resp.PerBackendStorage["qdrant"])
	}
}

func TestAnalytics_AdminForbidden(t *testing.T) {
	t.Parallel()
	r := newAnalyticsRig(t, admin.RoleAdmin)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/global", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAnalytics_ViewerForbidden(t *testing.T) {
	t.Parallel()
	r := newAnalyticsRig(t, admin.RoleViewer)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/global", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAnalytics_AnonymousForbidden(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	h, _ := admin.NewAnalyticsHandler(admin.AnalyticsConfig{
		Sources:   &stubSourceProvider{},
		Documents: &stubDocProvider{},
		Storage:   &stubStorageProvider{stats: admin.AnalyticsStorageStats{PerBackend: map[string]int64{}}},
		DLQ:       &stubDLQProvider{},
	})
	r := gin.New()
	rg := r.Group("/v1/admin")
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/analytics/global", nil))
	if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 403 or 401 for anonymous, got %d", w.Code)
	}
}
