package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler_AdminSearch_BindsSourceID(t *testing.T) {
	t.Parallel()
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			if f.ResourceID != "src-42" {
				t.Fatalf("ResourceID=%q", f.ResourceID)
			}
			return &ListResult{Items: []AuditLog{{ID: "1"}}}, nil
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit?source_id=src-42", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandler_AdminSearch_BindsResourceID(t *testing.T) {
	t.Parallel()
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			if f.ResourceID != "abc" {
				t.Fatalf("ResourceID=%q", f.ResourceID)
			}
			return &ListResult{}, nil
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit?resource_id=abc", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

func TestHandler_AdminSearch_BindsPayloadSearch(t *testing.T) {
	t.Parallel()
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			if f.PayloadSearch != "needle" {
				t.Fatalf("PayloadSearch=%q", f.PayloadSearch)
			}
			return &ListResult{Items: []AuditLog{}}, nil
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit?q=needle", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

func TestHandler_AdminSearch_BindsPagination(t *testing.T) {
	t.Parallel()
	h := newHandlerWithReader(fakeReader{
		listFn: func(_ context.Context, f ListFilter) (*ListResult, error) {
			if f.PageSize != 7 {
				t.Fatalf("PageSize=%d", f.PageSize)
			}
			if f.PageToken != "tok-1" {
				t.Fatalf("PageToken=%q", f.PageToken)
			}
			return &ListResult{Items: []AuditLog{}, NextPageToken: ""}, nil
		},
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit?page_size=7&page_token=tok-1", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Items         []AuditLog `json:"items"`
		NextPageToken string     `json:"next_page_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestHandler_AdminSearch_RejectsBadPageSize(t *testing.T) {
	t.Parallel()
	h := newHandlerWithReader(fakeReader{listFn: func(_ context.Context, _ ListFilter) (*ListResult, error) { return &ListResult{}, nil }})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/audit?page_size=-1", nil)
	router(h, "tenant-a").ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d", w.Code)
	}
}
