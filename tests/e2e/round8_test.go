//go:build e2e

// Package e2e — round8_test.go — Round-8 Task 14.
//
// Exercises the GORM-backed stores from Tasks 6–11 and the
// notification dispatcher wiring from Task 5 end-to-end through
// the admin HTTP handlers. Like round6/round7_test.go this file
// is docker-compose-free; it relies on the in-memory store
// implementations behind the same handler surface to keep the
// laptop loop fast.
package e2e

import (
	"bytes"
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

const round8Tenant = "tenant-r8"

func round8Router(register func(*gin.RouterGroup)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, round8Tenant)
		c.Set(audit.ActorContextKey, "actor-r8")
		c.Next()
	})
	register(&r.RouterGroup)
	return r
}

func round8DoJSON(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ---------- Task 6: query analytics round-trip ----------

func TestRound8_QueryAnalytics_RoundTrip(t *testing.T) {
	store := admin.NewInMemoryQueryAnalyticsStore()
	_ = store.Record(context.Background(), &admin.QueryAnalyticsRow{
		TenantID: round8Tenant, QueryText: "hello",
		TopK: 5, HitCount: 3, LatencyMS: 12,
	})
	h, err := admin.NewQueryAnalyticsHandler(store)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round8Router(h.Register)
	w := round8DoJSON(t, r, http.MethodGet, "/v1/admin/analytics/queries?limit=10", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("expected query in payload: %s", w.Body.String())
	}
}

// ---------- Task 7: pinned results CRUD ----------

func TestRound8_PinnedResults_CreateAndList(t *testing.T) {
	store := admin.NewInMemoryPinnedResultStore(nil)
	h, err := admin.NewPinnedResultsHandler(store, nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round8Router(h.Register)
	w := round8DoJSON(t, r, http.MethodPost, "/v1/admin/retrieval/pins",
		map[string]any{"query_pattern": "hello", "chunk_id": "c-1", "position": 0})
	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	w = round8DoJSON(t, r, http.MethodGet, "/v1/admin/retrieval/pins", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "c-1") {
		t.Fatalf("expected chunk_id in payload: %s", w.Body.String())
	}
}

// ---------- Task 8: sync history list ----------

func TestRound8_SyncHistory_RoundTrip(t *testing.T) {
	rec := admin.NewInMemorySyncHistoryRecorder()
	_ = rec.Start(context.Background(), round8Tenant, "src-1", "run-1")
	_ = rec.Finish(context.Background(), round8Tenant, "src-1", "run-1",
		admin.SyncStatusSucceeded, 42, 0)
	h, err := admin.NewSyncHistoryHandler(rec)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := round8Router(h.Register)
	w := round8DoJSON(t, r, http.MethodGet, "/v1/admin/sources/src-1/sync-history", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "run-1") {
		t.Fatalf("expected run-1 in payload: %s", w.Body.String())
	}
}

// ---------- Task 9: latency budget round-trip ----------

func TestRound8_LatencyBudget_RoundTrip(t *testing.T) {
	store := admin.NewInMemoryLatencyBudgetStore()
	h, _ := admin.NewLatencyBudgetHandler(store, nil)
	r := round8Router(h.Register)
	w := round8DoJSON(t, r, http.MethodPut, "/v1/admin/tenants/"+round8Tenant+"/latency-budget",
		map[string]any{"max_latency_ms": 750})
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("put: %d body=%s", w.Code, w.Body.String())
	}
	w = round8DoJSON(t, r, http.MethodGet, "/v1/admin/tenants/"+round8Tenant+"/latency-budget", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "750") {
		t.Fatalf("expected 750 in payload: %s", w.Body.String())
	}
}

// ---------- Task 10: cache config TTL round-trip ----------

func TestRound8_CacheConfig_RoundTrip(t *testing.T) {
	store := admin.NewInMemoryCacheTTLStore()
	h, _ := admin.NewCacheConfigHandler(store, nil)
	r := round8Router(h.Register)
	w := round8DoJSON(t, r, http.MethodPut, "/v1/admin/tenants/"+round8Tenant+"/cache-config",
		map[string]any{"ttl_ms": 60000})
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("put: %d body=%s", w.Code, w.Body.String())
	}
	w = round8DoJSON(t, r, http.MethodGet, "/v1/admin/tenants/"+round8Tenant+"/cache-config", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "60000") {
		t.Fatalf("expected 60000 in payload: %s", w.Body.String())
	}
}

// ---------- Task 5: notification dispatcher fires on audit ----------

type fakeDelivery struct {
	mu    sync.Mutex
	count int
	last  []byte
}

func (f *fakeDelivery) Send(_ context.Context, _ string, _ admin.NotificationChannel, body []byte) (admin.DeliveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	f.last = append([]byte(nil), body...)
	return admin.DeliveryResult{Attempts: 1, StatusCode: 200}, nil
}

type round8AuditWriter struct {
	dispatcher *admin.NotificationDispatcher
}

func (a *round8AuditWriter) Create(ctx context.Context, log *audit.AuditLog) error {
	if log == nil {
		return errors.New("nil audit")
	}
	if a.dispatcher != nil {
		_ = a.dispatcher.Dispatch(ctx, log.TenantID, string(log.Action), log.Metadata)
	}
	return nil
}

func TestRound8_NotificationDispatcher_FiresOnAudit(t *testing.T) {
	store := admin.NewInMemoryNotificationStore()
	if err := store.Create(&admin.NotificationPreference{
		TenantID:  round8Tenant,
		EventType: "source.connected",
		Channel:   admin.NotificationChannelWebhook,
		Target:    "https://example.com/hook",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("create pref: %v", err)
	}
	delivery := &fakeDelivery{}
	disp, err := admin.NewNotificationDispatcher(store, delivery)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}

	aw := &round8AuditWriter{dispatcher: disp}
	_ = aw.Create(context.Background(), &audit.AuditLog{
		TenantID: round8Tenant, Action: "source.connected",
		Metadata: map[string]any{"source_id": "s-1"},
	})

	delivery.mu.Lock()
	defer delivery.mu.Unlock()
	if delivery.count != 1 {
		t.Fatalf("expected 1 delivery; got %d", delivery.count)
	}
	if !strings.Contains(string(delivery.last), "source.connected") {
		t.Fatalf("expected event_type in body: %s", string(delivery.last))
	}
}
