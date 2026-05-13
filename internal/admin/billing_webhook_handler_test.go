package admin_test

// billing_webhook_handler_test.go — Round-24 Task 24.
//
// Exercises the public surface — POST upsert + validation, GET
// round-trip, DELETE — and asserts the secret is never echoed.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

type fakeBillingStore struct {
	mu   sync.Mutex
	subs map[string]admin.BillingWebhookSubscription
}

func newFakeBillingStore() *fakeBillingStore {
	return &fakeBillingStore{subs: map[string]admin.BillingWebhookSubscription{}}
}

func (s *fakeBillingStore) Upsert(_ context.Context, sub admin.BillingWebhookSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs[sub.TenantID] = sub

	return nil
}

func (s *fakeBillingStore) Get(_ context.Context, tenantID string) (*admin.BillingWebhookSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subs[tenantID]
	if !ok {
		return nil, nil
	}

	return &sub, nil
}

func (s *fakeBillingStore) Delete(_ context.Context, tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, tenantID)

	return nil
}

func setupBillingRouter(t *testing.T, store admin.BillingWebhookStore, tenantID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewBillingWebhookHandler(store, nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Set(audit.ActorContextKey, "u1")
		c.Next()
	})
	h.Register(rg)

	return r
}

func TestBillingWebhook_PostUpsertGetRoundTrip(t *testing.T) {
	t.Parallel()
	store := newFakeBillingStore()
	r := setupBillingRouter(t, store, "t1")
	secret := strings.Repeat("k", 32)
	body := `{"url":"https://example.com/billing","shared_secret":"` + secret + `"}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/webhooks/billing", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("post: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("post: response must not echo the shared_secret, got %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/webhooks/billing", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var got admin.BillingWebhookResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("get: unmarshal: %v", err)
	}
	if got.URL != "https://example.com/billing" {
		t.Fatalf("get: url=%q, want https://example.com/billing", got.URL)
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/admin/webhooks/billing", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", w.Code)
	}
}

func TestBillingWebhook_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body string
		wantStatus int
	}{
		{"missing url", `{"shared_secret":"` + strings.Repeat("k", 32) + `"}`, http.StatusBadRequest},
		{"http scheme", `{"url":"http://foo","shared_secret":"` + strings.Repeat("k", 32) + `"}`, http.StatusBadRequest},
		{"short secret", `{"url":"https://x.com","shared_secret":"short"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := setupBillingRouter(t, newFakeBillingStore(), "t1")
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/webhooks/billing", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestBillingWebhook_GetReturns404WhenUnregistered(t *testing.T) {
	t.Parallel()
	r := setupBillingRouter(t, newFakeBillingStore(), "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/webhooks/billing", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when no subscription exists, got %d", w.Code)
	}
}

// errBillingStore returns the provided error from Get to verify
// the handler's 500 path.
type errBillingStore struct {
	err error
}

func (e *errBillingStore) Upsert(_ context.Context, _ admin.BillingWebhookSubscription) error {
	return e.err
}
func (e *errBillingStore) Get(_ context.Context, _ string) (*admin.BillingWebhookSubscription, error) {
	return nil, e.err
}
func (e *errBillingStore) Delete(_ context.Context, _ string) error { return e.err }

func TestBillingWebhook_StoreErrorBecomes500(t *testing.T) {
	t.Parallel()
	r := setupBillingRouter(t, &errBillingStore{err: errors.New("boom")}, "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/admin/webhooks/billing", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on store error, got %d", w.Code)
	}
}
