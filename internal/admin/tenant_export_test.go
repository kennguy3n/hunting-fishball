package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

type stubExportCollector struct {
	payload *admin.TenantExportPayload
	err     error
}

func (s *stubExportCollector) Collect(_ context.Context, tenantID string) (*admin.TenantExportPayload, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.payload != nil {
		s.payload.TenantID = tenantID
		return s.payload, nil
	}
	raw, _ := json.Marshal(map[string]any{"id": "src-1"})
	return &admin.TenantExportPayload{
		TenantID: tenantID, GeneratedAt: time.Now().UTC(),
		Sources: []json.RawMessage{raw},
	}, nil
}

type stubExportPublisher struct {
	url     string
	publish atomic.Int32
	err     error
}

func (s *stubExportPublisher) Publish(_ context.Context, jobID string, _ []byte) (string, error) {
	s.publish.Add(1)
	if s.err != nil {
		return "", s.err
	}
	return s.url + "/" + jobID, nil
}

func TestTenantExport_Validation(t *testing.T) {
	cases := []admin.TenantExportConfig{
		{Collector: &stubExportCollector{}, Publisher: &stubExportPublisher{}},
		{JobStore: admin.NewInMemoryTenantExportJobStore(), Publisher: &stubExportPublisher{}},
		{JobStore: admin.NewInMemoryTenantExportJobStore(), Collector: &stubExportCollector{}},
	}
	for i, tc := range cases {
		if _, err := admin.NewTenantExportHandler(tc); err == nil {
			t.Fatalf("case %d should fail", i)
		}
	}
}

func TestTenantExport_HappyPath(t *testing.T) {
	store := admin.NewInMemoryTenantExportJobStore()
	pub := &stubExportPublisher{url: "https://blob/"}
	h, err := admin.NewTenantExportHandler(admin.TenantExportConfig{
		JobStore: store, Collector: &stubExportCollector{}, Publisher: pub,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-1/export", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("post status=%d body=%s", rr.Code, rr.Body)
	}
	var job admin.TenantExportJob
	_ = json.Unmarshal(rr.Body.Bytes(), &job)

	// Drive synchronous execution on top of the same job to make
	// the test deterministic; the goroutine spawned in `create`
	// races with the `get`.
	if err := h.RunJobSync(context.Background(), job.ID, "tenant-1"); err != nil {
		t.Fatalf("RunJobSync: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/admin/tenants/tenant-1/export/"+job.ID, nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body)
	}
	var got admin.TenantExportJob
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Status != admin.TenantExportStatusCompleted {
		t.Fatalf("status=%v body=%s", got.Status, rr.Body)
	}
	if got.DownloadURL == "" || got.PayloadBytes == 0 {
		t.Fatalf("export incomplete: %+v", got)
	}
}

func TestTenantExport_TenantMismatchForbidden(t *testing.T) {
	store := admin.NewInMemoryTenantExportJobStore()
	h, _ := admin.NewTenantExportHandler(admin.TenantExportConfig{
		JobStore: store, Collector: &stubExportCollector{}, Publisher: &stubExportPublisher{},
	})
	r := authStubRouter("tenant-a")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-b/export", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403; got %d", rr.Code)
	}
}

func TestTenantExport_CollectorErrorMarksFailed(t *testing.T) {
	store := admin.NewInMemoryTenantExportJobStore()
	h, _ := admin.NewTenantExportHandler(admin.TenantExportConfig{
		JobStore:  store,
		Collector: &stubExportCollector{err: errors.New("collect failed")},
		Publisher: &stubExportPublisher{},
	})
	job := &admin.TenantExportJob{TenantID: "tenant-1", Status: admin.TenantExportStatusPending}
	_ = store.Create(context.Background(), job)
	if err := h.RunJobSync(context.Background(), job.ID, "tenant-1"); err == nil {
		t.Fatalf("expected error from RunJobSync")
	}
	got, _ := store.Get(context.Background(), "tenant-1", job.ID)
	if got.Status != admin.TenantExportStatusFailed {
		t.Fatalf("expected failed; got %s", got.Status)
	}
	if got.Error == "" {
		t.Fatalf("expected error message recorded")
	}
}
