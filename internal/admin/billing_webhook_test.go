package admin_test

// billing_webhook_test.go — Round-19 Task 26.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

type fakeBillingTenants struct {
	ids []string
}

func (f *fakeBillingTenants) ListTenantIDs(_ context.Context) ([]string, error) {
	return f.ids, nil
}

type fakeBillingUsage struct {
	rows map[string][]admin.TenantUsage
}

func (f *fakeBillingUsage) List(_ context.Context, tenantID string, _, _ time.Time) ([]admin.TenantUsage, error) {
	return f.rows[tenantID], nil
}

func TestBillingWebhookDispatcher_HappyPath(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		bodies   []string
		idempKey string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		idempKey = r.Header.Get("X-Idempotency-Key")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tenants := &fakeBillingTenants{ids: []string{"t1"}}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	usage := &fakeBillingUsage{rows: map[string][]admin.TenantUsage{
		"t1": {
			{TenantID: "t1", Day: day, Metric: "api_calls", Count: 100},
			{TenantID: "t1", Day: day, Metric: "ingested_docs", Count: 5},
		},
	}}
	disp, err := admin.NewBillingWebhookDispatcher(admin.BillingWebhookConfig{
		URL: srv.URL,
		Now: func() time.Time { return day.Add(24 * time.Hour) },
	}, tenants, usage)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := disp.DispatchOnce(context.Background(), day.Add(24*time.Hour))
	if err != nil || got != 1 {
		t.Fatalf("DispatchOnce: got=%d err=%v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("expected 1 webhook POST, got %d", len(bodies))
	}
	if !strings.Contains(idempKey, "billing:t1:") {
		t.Fatalf("missing idempotency-key, got %q", idempKey)
	}
	var p admin.BillingWebhookPayload
	if err := json.Unmarshal([]byte(bodies[0]), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.TenantID != "t1" || p.Usage["api_calls"] != 100 || p.Usage["ingested_docs"] != 5 {
		t.Fatalf("payload mismatch: %+v", p)
	}
}

func TestBillingWebhookDispatcher_SkipsTenantsWithNoUsage(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	tenants := &fakeBillingTenants{ids: []string{"t1", "t2"}}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	usage := &fakeBillingUsage{rows: map[string][]admin.TenantUsage{
		"t1": {{TenantID: "t1", Day: day, Metric: "api_calls", Count: 1}},
	}}
	disp, _ := admin.NewBillingWebhookDispatcher(admin.BillingWebhookConfig{
		URL: srv.URL,
		Now: func() time.Time { return day.Add(24 * time.Hour) },
	}, tenants, usage)
	got, err := disp.DispatchOnce(context.Background(), day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected 1 tenant dispatched (other had no usage), got %d", got)
	}
	if calls != 1 {
		t.Fatalf("expected 1 webhook call, got %d", calls)
	}
}

func TestBillingWebhookDispatcher_RejectsBlankURL(t *testing.T) {
	t.Parallel()
	if _, err := admin.NewBillingWebhookDispatcher(admin.BillingWebhookConfig{}, &fakeBillingTenants{}, &fakeBillingUsage{}); err == nil {
		t.Fatal("expected error on blank URL")
	}
}

func TestBillingWebhookDispatcher_DownstreamErrorBubbles(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tenants := &fakeBillingTenants{ids: []string{"t1"}}
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	usage := &fakeBillingUsage{rows: map[string][]admin.TenantUsage{
		"t1": {{TenantID: "t1", Day: day, Metric: "api_calls", Count: 1}},
	}}
	disp, _ := admin.NewBillingWebhookDispatcher(admin.BillingWebhookConfig{
		URL: srv.URL,
		Now: func() time.Time { return day.Add(24 * time.Hour) },
	}, tenants, usage)
	got, err := disp.DispatchOnce(context.Background(), day.Add(24*time.Hour))
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if got != 0 {
		t.Fatalf("expected 0 successful dispatches, got %d", got)
	}
}
