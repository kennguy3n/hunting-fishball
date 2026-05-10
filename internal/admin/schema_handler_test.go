package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

type fakeSchemaLookup struct {
	src    *admin.Source
	getErr error
}

func (f *fakeSchemaLookup) GetByIDForTenant(_ context.Context, tenantID, sourceID string) (*admin.Source, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.src == nil || f.src.TenantID != tenantID || f.src.ID != sourceID {
		return nil, nil
	}
	return f.src, nil
}

type fakeSchemaResolver struct {
	out []connector.Namespace
	err error
}

func (f *fakeSchemaResolver) Resolve(_ context.Context, _ *admin.Source) ([]connector.Namespace, error) {
	return f.out, f.err
}

func TestSchemaHandler_Validation(t *testing.T) {
	if _, err := admin.NewSchemaHandler(admin.SchemaHandlerConfig{Resolver: &fakeSchemaResolver{}}); err == nil {
		t.Fatalf("nil lookup must error")
	}
	if _, err := admin.NewSchemaHandler(admin.SchemaHandlerConfig{Lookup: &fakeSchemaLookup{}}); err == nil {
		t.Fatalf("nil resolver must error")
	}
}

func TestSchemaHandler_NotFound(t *testing.T) {
	h, err := admin.NewSchemaHandler(admin.SchemaHandlerConfig{
		Lookup:   &fakeSchemaLookup{},
		Resolver: &fakeSchemaResolver{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/missing/schema", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}

func TestSchemaHandler_HappyPath(t *testing.T) {
	src := &admin.Source{ID: "src-1", TenantID: "tenant-1", ConnectorType: "google-drive"}
	resolver := &fakeSchemaResolver{out: []connector.Namespace{
		{ID: "drive-1", Name: "Engineering", Kind: "drive", Metadata: map[string]string{"owner": "eng"}},
		{ID: "drive-2", Name: "Marketing", Kind: "drive"},
	}}
	h, err := admin.NewSchemaHandler(admin.SchemaHandlerConfig{
		Lookup:   &fakeSchemaLookup{src: src},
		Resolver: resolver,
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/schema", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
	var resp admin.SchemaResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SourceID != "src-1" || resp.ConnectorType != "google-drive" {
		t.Fatalf("unexpected: %+v", resp)
	}
	if len(resp.Namespaces) != 2 {
		t.Fatalf("got %d namespaces; want 2", len(resp.Namespaces))
	}
	if resp.Namespaces[0].ID != "drive-1" || resp.Namespaces[0].Metadata["owner"] != "eng" {
		t.Fatalf("namespace metadata lost: %+v", resp.Namespaces[0])
	}
}

func TestSchemaHandler_ResolverError(t *testing.T) {
	src := &admin.Source{ID: "src-1", TenantID: "tenant-1", ConnectorType: "google-drive"}
	h, err := admin.NewSchemaHandler(admin.SchemaHandlerConfig{
		Lookup:   &fakeSchemaLookup{src: src},
		Resolver: &fakeSchemaResolver{err: errors.New("upstream busted")},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("tenant-1")
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/schema", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}

func TestSchemaHandler_MissingTenant(t *testing.T) {
	src := &admin.Source{ID: "src-1", TenantID: "tenant-1", ConnectorType: "google-drive"}
	h, err := admin.NewSchemaHandler(admin.SchemaHandlerConfig{
		Lookup:   &fakeSchemaLookup{src: src},
		Resolver: &fakeSchemaResolver{},
	})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	r := authStubRouter("") // no auth middleware → no tenant
	g := r.Group("/")
	h.Register(g)
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/sources/src-1/schema", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}
