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
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeOnboardingSources struct {
	rows []admin.Source
	err  error
}

func (f *fakeOnboardingSources) ListAllForTenant(_ context.Context, _ string) ([]admin.Source, error) {
	return f.rows, f.err
}

type fakeOnboardingPolicies struct {
	n   int
	err error
}

func (f *fakeOnboardingPolicies) PromotedPolicyVersionCount(_ context.Context, _ string) (int, error) {
	return f.n, f.err
}

type fakeOnboardingHealth struct {
	err error
}

func (f *fakeOnboardingHealth) CheckHealth(_ context.Context) error { return f.err }

func setupOnboardingRouter(t *testing.T, h *admin.OnboardingHandler, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Next()
	})
	h.Register(rg)

	return r
}

func TestOnboarding_ReadyWhenAllChecksPass(t *testing.T) {
	t.Parallel()
	h, err := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  &fakeOnboardingSources{rows: []admin.Source{{ID: "s1"}}},
		Policies: &fakeOnboardingPolicies{n: 1},
		Health:   &fakeOnboardingHealth{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := setupOnboardingRouter(t, h, "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t1/onboarding", strings.NewReader(`{}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.OnboardingResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Ready {
		t.Fatalf("expected ready, got %+v", resp)
	}
	if len(resp.Checks) != 3 {
		t.Fatalf("expected 3 checks, got %d", len(resp.Checks))
	}
}

func TestOnboarding_NotReadyWhenNoSource(t *testing.T) {
	t.Parallel()
	h, err := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  &fakeOnboardingSources{rows: nil},
		Policies: &fakeOnboardingPolicies{n: 1},
		Health:   &fakeOnboardingHealth{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := setupOnboardingRouter(t, h, "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t1/onboarding", strings.NewReader(`{}`)))
	var resp admin.OnboardingResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Ready {
		t.Fatalf("expected not ready, got %+v", resp)
	}
	if resp.Checks[0].Passed {
		t.Fatalf("expected sources check to fail, got %+v", resp.Checks[0])
	}
}

func TestOnboarding_NotReadyWhenHealthDown(t *testing.T) {
	t.Parallel()
	h, err := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  &fakeOnboardingSources{rows: []admin.Source{{ID: "s1"}}},
		Policies: &fakeOnboardingPolicies{n: 1},
		Health:   &fakeOnboardingHealth{err: errors.New("kafka down")},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := setupOnboardingRouter(t, h, "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t1/onboarding", strings.NewReader(`{}`)))
	var resp admin.OnboardingResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Ready {
		t.Fatalf("expected not ready when engine unhealthy, got %+v", resp)
	}
}

func TestOnboarding_RejectsTenantMismatch(t *testing.T) {
	t.Parallel()
	h, _ := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  &fakeOnboardingSources{},
		Policies: &fakeOnboardingPolicies{},
		Health:   &fakeOnboardingHealth{},
	})
	r := setupOnboardingRouter(t, h, "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t2/onboarding", strings.NewReader(`{}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestOnboarding_RejectsMissingContext(t *testing.T) {
	t.Parallel()
	h, _ := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  &fakeOnboardingSources{},
		Policies: &fakeOnboardingPolicies{},
		Health:   &fakeOnboardingHealth{},
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/t1/onboarding", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", w.Code, w.Body.String())
	}
}
