package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

func TestNotificationPreference_Validation(t *testing.T) {
	cases := []*admin.NotificationPreference{
		nil,
		{EventType: "x", Target: "https://x", Channel: admin.NotificationChannelWebhook},
		{TenantID: "t", Target: "x", Channel: admin.NotificationChannelWebhook},
		{TenantID: "t", EventType: "x", Channel: admin.NotificationChannelWebhook},
		{TenantID: "t", EventType: "x", Target: "x", Channel: "weird"},
	}
	for i, tc := range cases {
		if err := tc.Validate(); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
}

func TestNotificationStore_CRUD(t *testing.T) {
	s := admin.NewInMemoryNotificationStore()
	p := &admin.NotificationPreference{
		TenantID: "t", EventType: "source.synced",
		Channel: admin.NotificationChannelWebhook, Target: "https://x", Enabled: true,
	}
	if err := s.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	rows, _ := s.List("t")
	if len(rows) != 1 {
		t.Fatalf("list len: %d", len(rows))
	}
	id := rows[0].ID
	subs, _ := s.ListByEvent("t", "source.synced")
	if len(subs) != 1 {
		t.Fatalf("by-event len: %d", len(subs))
	}
	if err := s.Delete("t", id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ = s.List("t")
	if len(rows) != 0 {
		t.Fatalf("expected 0 after delete; got %d", len(rows))
	}
}

func TestNotificationStore_TenantIsolation(t *testing.T) {
	s := admin.NewInMemoryNotificationStore()
	_ = s.Create(&admin.NotificationPreference{
		TenantID: "ta", EventType: "x", Channel: admin.NotificationChannelWebhook, Target: "u", Enabled: true,
	})
	rows, _ := s.List("tb")
	if len(rows) != 0 {
		t.Fatalf("cross-tenant leak: %v", rows)
	}
}

type fakeDelivery struct {
	mu    sync.Mutex
	hits  []string
	fails bool
}

func (f *fakeDelivery) Send(_ context.Context, target string, _ admin.NotificationChannel, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails {
		return errors.New("upstream broken")
	}
	f.hits = append(f.hits, target)
	return nil
}

func (f *fakeDelivery) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.hits))
	copy(out, f.hits)
	return out
}

func TestNotificationDispatcher_Dispatch(t *testing.T) {
	store := admin.NewInMemoryNotificationStore()
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "t", EventType: "source.synced", Channel: admin.NotificationChannelWebhook,
		Target: "https://hook.example.com/a", Enabled: true,
	})
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "t", EventType: "source.synced", Channel: admin.NotificationChannelWebhook,
		Target: "https://hook.example.com/b", Enabled: true,
	})
	_ = store.Create(&admin.NotificationPreference{
		TenantID: "t", EventType: "other", Channel: admin.NotificationChannelWebhook,
		Target: "https://hook.example.com/c", Enabled: true,
	})
	delivery := &fakeDelivery{}
	disp, err := admin.NewNotificationDispatcher(store, delivery)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if err := disp.Dispatch(context.Background(), "t", "source.synced", map[string]any{"src": "s1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	hits := delivery.snapshot()
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits; got %v", hits)
	}
}

func TestNotificationDispatcher_NoSubscribers(t *testing.T) {
	store := admin.NewInMemoryNotificationStore()
	disp, _ := admin.NewNotificationDispatcher(store, &fakeDelivery{})
	if err := disp.Dispatch(context.Background(), "t", "source.synced", nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestWebhookDelivery_HTTP(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(200)
	}))
	defer srv.Close()
	d := &admin.WebhookDelivery{}
	if err := d.Send(context.Background(), srv.URL, admin.NotificationChannelWebhook, []byte(`{"x":1}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case b := <-got:
		if !bytes.Contains(b, []byte(`"x":1`)) {
			t.Fatalf("unexpected body: %s", b)
		}
	default:
		t.Fatalf("server never received request")
	}
}

func TestNotificationHandler_HTTP(t *testing.T) {
	store := admin.NewInMemoryNotificationStore()
	h, err := admin.NewNotificationHandler(store, &fakeAudit{})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	body, _ := json.Marshal(admin.NotificationPreference{
		EventType: "source.synced", Channel: admin.NotificationChannelWebhook, Target: "https://hook.x",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/notifications", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post status=%d body=%s", rr.Code, rr.Body)
	}
	var created admin.NotificationPreference
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created.ID == "" || !created.Enabled {
		t.Fatalf("unexpected response: %+v", created)
	}
	// list
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/notifications", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
	// delete
	req = httptest.NewRequest(http.MethodDelete, "/v1/admin/notifications/"+created.ID, nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d", rr.Code)
	}
}
