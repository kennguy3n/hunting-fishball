package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func TestWebhookDelivery_RetriesThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			http.Error(w, "later", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	wh := &admin.WebhookDelivery{Sleep: func(time.Duration) {}, Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}}
	if err := wh.Send(context.Background(), srv.URL, admin.NotificationChannelWebhook, []byte(`{}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if wh.LastAttempts != 3 {
		t.Fatalf("expected 3 attempts; got %d", wh.LastAttempts)
	}
	if wh.LastStatusCode != http.StatusOK {
		t.Fatalf("expected 200; got %d", wh.LastStatusCode)
	}
}

func TestWebhookDelivery_DeadLettersAfterMaxRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "always failing", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	wh := &admin.WebhookDelivery{Sleep: func(time.Duration) {}, Backoff: []time.Duration{time.Millisecond, time.Millisecond}}
	err := wh.Send(context.Background(), srv.URL, admin.NotificationChannelWebhook, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if wh.LastAttempts != 3 {
		t.Fatalf("expected 3 attempts; got %d", wh.LastAttempts)
	}
	if wh.LastStatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500; got %d", wh.LastStatusCode)
	}
}

func TestNotificationDispatcher_RecordsAttempts(t *testing.T) {
	store := admin.NewInMemoryNotificationStore()
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "ta", EventType: "source.synced",
		Channel: admin.NotificationChannelWebhook, Target: "http://invalid.example.invalid/x",
		Enabled: true,
	})
	wh := &admin.WebhookDelivery{
		Sleep:   func(time.Duration) {},
		Backoff: []time.Duration{time.Millisecond},
		Client: &http.Client{Transport: failingRoundTripper{}},
	}
	disp, err := admin.NewNotificationDispatcher(store, wh)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	log := admin.NewInMemoryNotificationDeliveryLog()
	disp.SetDeliveryLog(log)
	_ = disp.Dispatch(context.Background(), "ta", "source.synced", map[string]any{"foo": "bar"})
	rows, _ := log.List(context.Background(), "ta", 10)
	if len(rows) != 1 {
		t.Fatalf("expected 1 attempt log; got %d", len(rows))
	}
	if rows[0].Status != admin.NotificationDeliveryStatusFailed {
		t.Fatalf("expected failed; got %s", rows[0].Status)
	}
	if rows[0].Attempt != 2 { // 1 initial + 1 retry
		t.Fatalf("expected 2 attempts; got %d", rows[0].Attempt)
	}
}

func TestNotificationDeliveryLog_TenantIsolation(t *testing.T) {
	log := admin.NewInMemoryNotificationDeliveryLog()
	_ = log.Append(context.Background(), &admin.NotificationDeliveryAttempt{
		TenantID: "ta", PreferenceID: "p", EventType: "x", Channel: admin.NotificationChannelWebhook, Target: "u",
		Status: admin.NotificationDeliveryStatusDelivered,
	})
	rows, _ := log.List(context.Background(), "tb", 10)
	if len(rows) != 0 {
		t.Fatalf("cross-tenant leak: %d", len(rows))
	}
}

func TestNotificationDeliveryLog_Handler(t *testing.T) {
	log := admin.NewInMemoryNotificationDeliveryLog()
	_ = log.Append(context.Background(), &admin.NotificationDeliveryAttempt{
		TenantID: "ta", PreferenceID: "p", EventType: "x", Channel: admin.NotificationChannelWebhook,
		Target: "u", Status: admin.NotificationDeliveryStatusDelivered, Attempt: 1, ResponseCode: 200,
	})
	h, err := admin.NewNotificationDeliveryLogHandler(log)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "ta")
		c.Next()
	})
	h.Register(r.Group("/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/notifications/delivery-log", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d", w.Code)
	}
	var body struct {
		Attempts []*admin.NotificationDeliveryAttempt `json:"attempts"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Attempts) != 1 {
		t.Fatalf("expected 1; got %d", len(body.Attempts))
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, http.ErrAbortHandler
}
