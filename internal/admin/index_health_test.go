package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func TestIndexHealth_AllHealthyReturns200(t *testing.T) {
	t.Parallel()
	h := admin.NewIndexHealthHandler(
		admin.PingChecker{BackendName: "qdrant", PingFn: func(_ context.Context) error { return nil }},
		admin.PingChecker{BackendName: "falkordb", PingFn: func(_ context.Context) error { return nil }},
	)
	g := gin.New()
	rg := g.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/health/indexes", nil)
	g.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp admin.IndexHealthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Healthy {
		t.Fatalf("healthy = false; %+v", resp)
	}
	if len(resp.Backends) != 2 {
		t.Fatalf("backends len = %d, want 2", len(resp.Backends))
	}
}

func TestIndexHealth_OneUnhealthyReturns503(t *testing.T) {
	t.Parallel()
	h := admin.NewIndexHealthHandler(
		admin.PingChecker{BackendName: "qdrant", PingFn: func(_ context.Context) error { return nil }},
		admin.PingChecker{BackendName: "falkordb", PingFn: func(_ context.Context) error { return errors.New("connection refused") }},
	)
	g := gin.New()
	rg := g.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/health/indexes", nil)
	g.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var resp admin.IndexHealthResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Healthy {
		t.Fatalf("healthy = true; expected false")
	}
	if !strings.Contains(strings.Join(allErrors(resp), " "), "connection refused") {
		t.Fatalf("expected connection refused in body, got %+v", resp)
	}
}

func allErrors(r admin.IndexHealthResponse) []string {
	out := []string{}
	for _, b := range r.Backends {
		out = append(out, b.Error)
	}
	return out
}
