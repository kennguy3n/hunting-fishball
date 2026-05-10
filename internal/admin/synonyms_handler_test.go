package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
)

type fakeSynStore struct {
	mu   sync.Mutex
	data map[string]map[string][]string
}

func (f *fakeSynStore) Get(_ context.Context, tenantID string) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for k, v := range f.data[tenantID] {
		out[k] = append([]string(nil), v...)
	}
	return out, nil
}

func (f *fakeSynStore) Set(_ context.Context, tenantID string, syns map[string][]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.data == nil {
		f.data = map[string]map[string][]string{}
	}
	cp := map[string][]string{}
	for k, v := range syns {
		cp[k] = append([]string(nil), v...)
	}
	f.data[tenantID] = cp
	return nil
}

func TestSynonymsHandler_GetSet(t *testing.T) {
	store := &fakeSynStore{}
	h, err := admin.NewSynonymsHandler(store, &fakeAudit{})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)

	body, _ := json.Marshal(map[string]any{"synonyms": map[string][]string{"car": {"vehicle"}}})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/synonyms", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post status=%d body=%s", rr.Code, rr.Body)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/admin/synonyms", nil)
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", rr.Code, rr.Body)
	}
	var resp struct {
		Synonyms map[string][]string `json:"synonyms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v := resp.Synonyms["car"]; len(v) != 1 || v[0] != "vehicle" {
		t.Fatalf("unexpected: %v", resp.Synonyms)
	}

	// Cross-tenant: tenant-2 sees an empty map.
	r2 := authStubRouter("tenant-2")
	g2 := r2.Group("/")
	h.Register(g2)
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/synonyms", nil)
	rr = httptest.NewRecorder()
	r2.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-tenant get: %d", rr.Code)
	}
	resp.Synonyms = nil
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Synonyms) != 0 {
		t.Fatalf("cross-tenant leakage: %v", resp.Synonyms)
	}
}
