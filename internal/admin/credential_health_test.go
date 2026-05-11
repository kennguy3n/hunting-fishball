package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

type credHealthLister struct{ srcs []admin.Source }

func (f *credHealthLister) ListAllActive(_ context.Context) ([]admin.Source, error) {
	return f.srcs, nil
}

type scriptedValidator struct {
	mu       sync.Mutex
	failures map[string]error
	calls    int
}

func (s *scriptedValidator) Validate(_ context.Context, _ string, cfg connector.ConnectorConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.failures[cfg.SourceID]
}

type credHealthAudit struct {
	mu  sync.Mutex
	row []*audit.AuditLog
}

func (r *credHealthAudit) Create(_ context.Context, log *audit.AuditLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.row = append(r.row, log)
	return nil
}

func TestCredentialHealthWorker_RecordsAndAuditsTransition(t *testing.T) {
	lister := &credHealthLister{srcs: []admin.Source{{ID: "s1", TenantID: "ta", ConnectorType: "github"}}}
	val := &scriptedValidator{failures: map[string]error{"s1": errors.New("token revoked")}}
	store := admin.NewInMemoryCredentialHealthStore()
	rec := &credHealthAudit{}
	w, err := admin.NewCredentialHealthWorker(admin.CredentialHealthConfig{
		Lister: lister, Validator: val, Health: store, Audit: rec,
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	w.Tick(context.Background())
	got, ok := store.Get(context.Background(), "ta", "s1")
	if !ok || got.Valid {
		t.Fatalf("expected invalid health row; got %+v ok=%v", got, ok)
	}
	if len(rec.row) != 1 || rec.row[0].Action != audit.ActionSourceCredentialInvalid {
		t.Fatalf("expected one invalid audit event; got %v", rec.row)
	}
	// Idempotent: second tick should not duplicate the audit.
	w.Tick(context.Background())
	if len(rec.row) != 1 {
		t.Fatalf("expected no duplicate audit; got %d", len(rec.row))
	}
	// Recovery: validator now succeeds; no audit event but health row flips.
	val.failures = map[string]error{}
	w.Tick(context.Background())
	got, _ = store.Get(context.Background(), "ta", "s1")
	if !got.Valid {
		t.Fatalf("expected valid after recovery; got %+v", got)
	}
}

func TestCredentialHealthHandler(t *testing.T) {
	store := admin.NewInMemoryCredentialHealthStore()
	_ = store.RecordCredentialCheck(context.Background(), "ta", "s1", false, "bad token")
	h, err := admin.NewCredentialHealthHandler(store)
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
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/s1/credential-health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d body=%s", w.Code, w.Body.String())
	}
	var body admin.CredentialCheckRow
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Valid || body.Message != "bad token" {
		t.Fatalf("unexpected body: %+v", body)
	}
	// 404 for unknown source
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/sources/unknown/credential-health", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", w.Code)
	}
}
