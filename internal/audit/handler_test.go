package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// fakeReader implements repoReader without touching a database.
type fakeReader struct {
	listFn func(ctx context.Context, f ListFilter) (*ListResult, error)
	getFn  func(ctx context.Context, tenantID, id string) (*AuditLog, error)
}

func (f fakeReader) List(ctx context.Context, filter ListFilter) (*ListResult, error) {
	return f.listFn(ctx, filter)
}

func (f fakeReader) Get(ctx context.Context, tenantID, id string) (*AuditLog, error) {
	return f.getFn(ctx, tenantID, id)
}

// router builds a minimal Gin engine with the supplied handler mounted
// and a fake auth middleware that injects tenantID. tenantID == "" means
// no tenant context (simulates an unauthenticated request).
func router(h *Handler, tenantID string) *gin.Engine {
	r := gin.New()
	if tenantID != "" {
		r.Use(func(c *gin.Context) {
			c.Set(TenantContextKey, tenantID)
			c.Next()
		})
	}
	rg := r.Group("/")
	h.Register(rg)

	return r
}

func TestHandler_List_Happy(t *testing.T) {
	t.Parallel()

	wantItems := []AuditLog{
		{ID: "01H", TenantID: "tenant-a", Action: ActionSourceSynced},
	}
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			if f.TenantID != "tenant-a" {
				t.Fatalf("tenant leak: %q", f.TenantID)
			}

			return &ListResult{Items: wantItems, NextPageToken: "tok-x"}, nil
		},
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs", nil)
	router(h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Items         []AuditLog `json:"items"`
		NextPageToken string     `json:"next_page_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if len(body.Items) != 1 || body.NextPageToken != "tok-x" {
		t.Fatalf("body mismatch: %+v", body)
	}
}

func TestHandler_List_Unauthenticated(t *testing.T) {
	t.Parallel()

	h := newHandlerWithReader(fakeReader{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs", nil)
	router(h, "").ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestHandler_List_TenantIsolation_RejectsQueryParam(t *testing.T) {
	t.Parallel()

	// The handler must use the context tenant, not anything from the URL.
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			if f.TenantID != "tenant-ctx" {
				t.Fatalf("handler used tenant from query: %q", f.TenantID)
			}

			return &ListResult{}, nil
		},
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs?tenant_id=tenant-evil", nil)
	router(h, "tenant-ctx").ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
}

// TestHandler_List_CursorAlias asserts the Round-5 Task 3 alias
// behaviour: clients can pass `cursor` / `limit` and the handler
// forwards them as PageToken / PageSize, with a `next_cursor` key
// emitted alongside the legacy `next_page_token`.
func TestHandler_List_CursorAlias(t *testing.T) {
	t.Parallel()
	var seenSize int
	var seenToken string
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			seenSize = f.PageSize
			seenToken = f.PageToken
			return &ListResult{Items: []AuditLog{{ID: "01H", TenantID: "tenant-a"}}, NextPageToken: "next-tok"}, nil
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs?cursor=cur-x&limit=42", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if seenSize != 42 || seenToken != "cur-x" {
		t.Fatalf("forwarded: size=%d token=%s", seenSize, seenToken)
	}
	var body struct {
		NextCursor    string `json:"next_cursor"`
		NextPageToken string `json:"next_page_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if body.NextCursor != "next-tok" || body.NextPageToken != "next-tok" {
		t.Fatalf("body: %+v", body)
	}
}

// TestHandler_List_LimitClamp asserts the limit ceiling matches
// internal/admin/pagination.go's MaxPageLimit.
func TestHandler_List_LimitClamp(t *testing.T) {
	t.Parallel()
	var seenSize int
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			seenSize = f.PageSize
			return &ListResult{}, nil
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs?limit=999", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if seenSize != 200 {
		t.Fatalf("limit must be clamped to 200, got %d", seenSize)
	}
}

func TestHandler_List_BadSinceParam(t *testing.T) {
	t.Parallel()

	h := newHandlerWithReader(fakeReader{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs?since=not-a-date", nil)
	router(h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_List_BadPageSize(t *testing.T) {
	t.Parallel()

	h := newHandlerWithReader(fakeReader{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs?page_size=0", nil)
	router(h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_Get_Happy(t *testing.T) {
	t.Parallel()

	want := &AuditLog{
		ID:        "01H",
		TenantID:  "tenant-a",
		Action:    ActionAuditRead,
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	h := newHandlerWithReader(fakeReader{
		getFn: func(_ context.Context, tenantID, id string) (*AuditLog, error) {
			if tenantID != "tenant-a" || id != "01H" {
				t.Fatalf("unexpected args: tenant=%q id=%q", tenantID, id)
			}

			return want, nil
		},
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs/01H", nil)
	router(h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"01H"`) {
		t.Fatalf("body missing id: %s", w.Body.String())
	}
}

func TestHandler_Get_NotFound(t *testing.T) {
	t.Parallel()

	h := newHandlerWithReader(fakeReader{
		getFn: func(_ context.Context, _, _ string) (*AuditLog, error) {
			return nil, ErrAuditLogNotFound
		},
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs/01H", nil)
	router(h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestHandler_Get_TenantIsolation(t *testing.T) {
	t.Parallel()

	h := newHandlerWithReader(fakeReader{
		getFn: func(_ context.Context, tenantID, _ string) (*AuditLog, error) {
			if tenantID != "tenant-a" {
				t.Fatalf("tenant leak: %q", tenantID)
			}

			return nil, ErrAuditLogNotFound
		},
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs/01H", nil)
	router(h, "tenant-a").ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestHandler_Get_Unauthenticated(t *testing.T) {
	t.Parallel()

	h := newHandlerWithReader(fakeReader{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/audit-logs/01H", nil)
	router(h, "").ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}
