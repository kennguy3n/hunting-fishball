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
	"time"

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

func (f *fakeDelivery) Send(_ context.Context, target string, _ admin.NotificationChannel, _ []byte) (admin.DeliveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails {
		return admin.DeliveryResult{Attempts: 1}, errors.New("upstream broken")
	}
	f.hits = append(f.hits, target)
	return admin.DeliveryResult{StatusCode: 200, Attempts: 1}, nil
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
	res, err := d.Send(context.Background(), srv.URL, admin.NotificationChannelWebhook, []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("expected 200; got %d", res.StatusCode)
	}
	if res.Attempts != 1 {
		t.Fatalf("expected 1 attempt; got %d", res.Attempts)
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

// TestNotificationDispatcher_ConcurrentDispatch exercises the
// concurrent-Dispatch pattern that surfaced the previous race on
// WebhookDelivery.LastStatusCode / LastAttempts. Multiple
// goroutines share one *WebhookDelivery and one dispatcher;
// `go test -race` must not report a data race, and every
// goroutine must observe a status code matching the response it
// actually received.
func TestNotificationDispatcher_ConcurrentDispatch(t *testing.T) {
	t.Parallel()

	// httptest server alternates 200/500 by tenant so each
	// goroutine sees a deterministic distinct outcome.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"tenant_id":"odd"`)) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	store := admin.NewInMemoryNotificationStore()
	for _, tn := range []string{"odd", "even"} {
		if err := store.Create(&admin.NotificationPreference{
			TenantID:  tn,
			EventType: "ev",
			Channel:   admin.NotificationChannelWebhook,
			Target:    srv.URL,
			Enabled:   true,
		}); err != nil {
			t.Fatalf("seed pref: %v", err)
		}
	}

	wh := &admin.WebhookDelivery{
		Client:  http.DefaultClient,
		Backoff: nil,
		// Single attempt — we are stressing concurrent state,
		// not retry behavior.
		Sleep: func(time.Duration) {},
	}
	// Override the package default backoff for this test to a
	// single zero-duration retry slot via an empty slice. We
	// want the dispatcher's Send call to make exactly one HTTP
	// hit per goroutine.
	wh.Backoff = []time.Duration{}

	disp, err := admin.NewNotificationDispatcher(store, wh)
	if err != nil {
		t.Fatalf("dispatcher: %v", err)
	}
	log := admin.NewInMemoryNotificationDeliveryLog()
	disp.SetDeliveryLog(log)

	var wg sync.WaitGroup
	const N = 32
	for i := 0; i < N; i++ {
		tn := "odd"
		if i%2 == 0 {
			tn = "even"
		}
		wg.Add(1)
		go func(tn string) {
			defer wg.Done()
			_ = disp.Dispatch(context.Background(), tn, "ev", map[string]any{"i": tn})
		}(tn)
	}
	wg.Wait()

	// Every Dispatch records exactly one row, so totals must
	// match. Per-row Attempt is always 1; ResponseCode is 200
	// for "even" tenants and 500 for "odd" tenants.
	oddRows, _ := log.List(context.Background(), "odd", 1000)
	evenRows, _ := log.List(context.Background(), "even", 1000)
	if len(oddRows)+len(evenRows) != N {
		t.Fatalf("expected %d rows total; got odd=%d even=%d", N, len(oddRows), len(evenRows))
	}
	for _, r := range oddRows {
		if r.ResponseCode != http.StatusInternalServerError {
			t.Fatalf("odd tenant row has wrong status: %+v", r)
		}
		if r.Attempt != 1 {
			t.Fatalf("odd row should be 1 attempt; got %d", r.Attempt)
		}
	}
	for _, r := range evenRows {
		if r.ResponseCode != http.StatusOK {
			t.Fatalf("even tenant row has wrong status: %+v", r)
		}
		if r.Attempt != 1 {
			t.Fatalf("even row should be 1 attempt; got %d", r.Attempt)
		}
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
