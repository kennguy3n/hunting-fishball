package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/audit"
)

func init() { gin.SetMode(gin.TestMode) }

type memSuites struct {
	store map[string]EvalSuite
}

func newMemSuites() *memSuites { return &memSuites{store: map[string]EvalSuite{}} }

func (m *memSuites) Save(_ context.Context, suite EvalSuite) error {
	m.store[suite.TenantID+"/"+suite.Name] = suite
	return nil
}
func (m *memSuites) Get(_ context.Context, tenantID, name string) (EvalSuite, error) {
	s, ok := m.store[tenantID+"/"+name]
	if !ok {
		return EvalSuite{}, ErrSuiteNotFound
	}
	return s, nil
}
func (m *memSuites) List(_ context.Context, tenantID string) ([]EvalSuite, error) {
	out := []EvalSuite{}
	for k, v := range m.store {
		if v.TenantID == tenantID {
			_ = k
			out = append(out, v)
		}
	}
	return out, nil
}

func tenantRouter(t *testing.T, h *Handler, tenantID string) *gin.Engine {
	t.Helper()
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, tenantID)
		c.Set(audit.ActorContextKey, "actor-1")
	})
	h.Register(rg)
	return r
}

func newHandler(t *testing.T, suites SuiteRepository, fn RetrieveFunc) *Handler {
	t.Helper()
	h, err := NewHandler(HandlerConfig{Suites: suites, Retrieve: fn})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func TestHandler_RunWithInlineSuite(t *testing.T) {
	t.Parallel()
	suites := newMemSuites()
	calls := 0
	h := newHandler(t, suites, func(_ context.Context, req RetrieveRequest) ([]RetrieveHit, error) {
		calls++
		if req.Query != "q1" {
			t.Fatalf("unexpected query: %q", req.Query)
		}
		return []RetrieveHit{{ID: "a", Score: 0.9}}, nil
	})
	r := tenantRouter(t, h, "tenant-a")

	body, _ := json.Marshal(runRequest{Suite: &EvalSuite{
		Name: "inline", TenantID: "tenant-a",
		Cases: []EvalCase{{Query: "q1", ExpectedChunkIDs: []string{"a"}}},
	}})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/eval/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var rep EvalReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if calls != 1 || rep.Aggregate.PrecisionAtK == 0 {
		t.Fatalf("rep: %+v calls=%d", rep, calls)
	}
}

func TestHandler_RunFromStoredSuite(t *testing.T) {
	t.Parallel()
	suites := newMemSuites()
	_ = suites.Save(context.Background(), EvalSuite{
		Name: "stored", TenantID: "tenant-a",
		Cases: []EvalCase{{Query: "q1", ExpectedChunkIDs: []string{"x"}}},
	})
	h := newHandler(t, suites, func(_ context.Context, _ RetrieveRequest) ([]RetrieveHit, error) {
		return []RetrieveHit{{ID: "x", Score: 0.5}}, nil
	})
	r := tenantRouter(t, h, "tenant-a")
	body, _ := json.Marshal(runRequest{SuiteName: "stored"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/eval/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_RunMissingSuite_404(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newMemSuites(), func(_ context.Context, _ RetrieveRequest) ([]RetrieveHit, error) {
		return nil, nil
	})
	r := tenantRouter(t, h, "tenant-a")
	body, _ := json.Marshal(runRequest{SuiteName: "nope"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/eval/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandler_RunUnauthenticated(t *testing.T) {
	t.Parallel()
	h := newHandler(t, newMemSuites(), func(_ context.Context, _ RetrieveRequest) ([]RetrieveHit, error) {
		return nil, nil
	})
	r := gin.New()
	rg := r.Group("/")
	h.Register(rg)
	body, _ := json.Marshal(runRequest{SuiteName: "x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/eval/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandler_UpsertAndList(t *testing.T) {
	t.Parallel()
	suites := newMemSuites()
	h := newHandler(t, suites, func(_ context.Context, _ RetrieveRequest) ([]RetrieveHit, error) {
		return nil, nil
	})
	r := tenantRouter(t, h, "tenant-a")

	body, _ := json.Marshal(EvalSuite{
		Name:  "demo",
		Cases: []EvalCase{{Query: "q1", ExpectedChunkIDs: []string{"a"}}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/eval/suites", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status: %d body=%s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/admin/eval/suites", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list status: %d", w.Code)
	}
	var listed struct {
		Suites []EvalSuite `json:"suites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed.Suites) != 1 {
		t.Fatalf("expected 1 suite: %+v", listed)
	}
}
