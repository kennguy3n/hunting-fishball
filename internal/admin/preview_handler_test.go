package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// fakePreviewConnector is a minimal SourceConnector implementation
// used to drive the preview handler from tests.
type fakePreviewConnector struct {
	validateErr   error
	connectErr    error
	listNSErr     error
	namespaces    []connector.Namespace
	docsByNS      map[string][]connector.DocumentRef
	listDocsErrNS string
	connectCalls  int
}

type fakePreviewConn struct {
	tenantID string
	sourceID string
}

func (f *fakePreviewConn) TenantID() string { return f.tenantID }
func (f *fakePreviewConn) SourceID() string { return f.sourceID }

func (f *fakePreviewConnector) Validate(_ context.Context, cfg connector.ConnectorConfig) error {
	if cfg.TenantID == "" {
		return errors.New("missing tenant")
	}
	return f.validateErr
}

func (f *fakePreviewConnector) Connect(_ context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	f.connectCalls++
	if f.connectErr != nil {
		return nil, f.connectErr
	}
	return &fakePreviewConn{tenantID: cfg.TenantID, sourceID: cfg.SourceID}, nil
}

func (f *fakePreviewConnector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	if f.listNSErr != nil {
		return nil, f.listNSErr
	}
	return f.namespaces, nil
}

type fakePreviewIterator struct {
	docs   []connector.DocumentRef
	idx    int
	closed bool
}

func (it *fakePreviewIterator) Next(_ context.Context) bool {
	if it.idx >= len(it.docs) {
		return false
	}
	it.idx++
	return true
}

func (it *fakePreviewIterator) Doc() connector.DocumentRef {
	if it.idx == 0 || it.idx > len(it.docs) {
		return connector.DocumentRef{}
	}
	return it.docs[it.idx-1]
}

func (it *fakePreviewIterator) Err() error { return nil }

func (it *fakePreviewIterator) Close() error {
	it.closed = true
	return nil
}

func (f *fakePreviewConnector) ListDocuments(_ context.Context, _ connector.Connection, ns connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	if ns.ID == f.listDocsErrNS {
		return nil, errors.New("list docs failed")
	}
	docs := f.docsByNS[ns.ID]
	return &fakePreviewIterator{docs: docs}, nil
}

func (f *fakePreviewConnector) FetchDocument(_ context.Context, _ connector.Connection, _ connector.DocumentRef) (*connector.Document, error) {
	return nil, errors.New("fetch not used in preview")
}

func (f *fakePreviewConnector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

func (f *fakePreviewConnector) Disconnect(_ context.Context, _ connector.Connection) error {
	return nil
}

func newPreviewRouter(t *testing.T, fake *fakePreviewConnector, tenantID string, limits admin.PreviewLimits) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if tenantID != "" {
			c.Set("tenant_id", tenantID)
		}
		c.Next()
	})
	h := admin.NewPreviewHandler(admin.PreviewHandlerConfig{
		Resolver: admin.PreviewConnectorResolverFunc(func(name string) (connector.SourceConnector, error) {
			if name != "fake" {
				return nil, errors.New("unknown connector")
			}
			return fake, nil
		}),
		Limits: limits,
	})
	rg := &r.RouterGroup
	h.Register(rg)
	return r
}

func postPreview(t *testing.T, r *gin.Engine, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/sources/preview", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPreview_HappyPath(t *testing.T) {
	t.Parallel()
	fake := &fakePreviewConnector{
		namespaces: []connector.Namespace{
			{ID: "ns-1", Name: "Engineering", Kind: "drive"},
			{ID: "ns-2", Name: "HR", Kind: "drive"},
		},
		docsByNS: map[string][]connector.DocumentRef{
			"ns-1": {{ID: "d1"}, {ID: "d2"}, {ID: "d3"}},
			"ns-2": {{ID: "d4"}},
		},
	}
	r := newPreviewRouter(t, fake, "tenant-a", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake", Credentials: "secret"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.PreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ConnectorType != "fake" || len(resp.Namespaces) != 2 {
		t.Fatalf("response: %+v", resp)
	}
	got := map[string]int{}
	for _, ns := range resp.Namespaces {
		got[ns.ID] = ns.DocsCount
	}
	if got["ns-1"] != 3 || got["ns-2"] != 1 {
		t.Fatalf("counts: %v", got)
	}
	if fake.connectCalls != 1 {
		t.Fatalf("expected exactly one Connect call, got %d", fake.connectCalls)
	}
}

func TestPreview_NamespaceTruncation(t *testing.T) {
	t.Parallel()
	fake := &fakePreviewConnector{}
	for i := 0; i < 60; i++ {
		fake.namespaces = append(fake.namespaces, connector.Namespace{ID: "ns-" + string(rune('a'+i%26))})
	}
	r := newPreviewRouter(t, fake, "tenant-a", admin.PreviewLimits{MaxNamespaces: 10, MaxDocsPerNamespace: 5})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp admin.PreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.NamespacesTruncated || len(resp.Namespaces) != 10 {
		t.Fatalf("truncation: ns=%d truncated=%v", len(resp.Namespaces), resp.NamespacesTruncated)
	}
}

func TestPreview_PerNamespaceTruncation(t *testing.T) {
	t.Parallel()
	docs := []connector.DocumentRef{}
	for i := 0; i < 50; i++ {
		docs = append(docs, connector.DocumentRef{ID: "d"})
	}
	fake := &fakePreviewConnector{
		namespaces: []connector.Namespace{{ID: "ns-1"}},
		docsByNS:   map[string][]connector.DocumentRef{"ns-1": docs},
	}
	r := newPreviewRouter(t, fake, "tenant-a", admin.PreviewLimits{MaxDocsPerNamespace: 10})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.PreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Namespaces) != 1 || resp.Namespaces[0].DocsCount != 10 || !resp.Namespaces[0].Truncated {
		t.Fatalf("got: %+v", resp)
	}
}

func TestPreview_PerNamespaceListErrorStaysSoft(t *testing.T) {
	t.Parallel()
	fake := &fakePreviewConnector{
		namespaces: []connector.Namespace{
			{ID: "ns-1"},
			{ID: "ns-2"},
		},
		docsByNS: map[string][]connector.DocumentRef{
			"ns-1": {{ID: "d1"}},
		},
		listDocsErrNS: "ns-2",
	}
	r := newPreviewRouter(t, fake, "tenant-a", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake"})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	var resp admin.PreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Namespaces) != 2 {
		t.Fatalf("namespaces: %d", len(resp.Namespaces))
	}
	got := map[string]int{}
	for _, n := range resp.Namespaces {
		got[n.ID] = n.DocsCount
	}
	if got["ns-1"] != 1 {
		t.Fatalf("ns-1: %d", got["ns-1"])
	}
	if got["ns-2"] != 0 {
		t.Fatalf("ns-2 expected soft 0, got %d", got["ns-2"])
	}
}

func TestPreview_ValidateError(t *testing.T) {
	t.Parallel()
	fake := &fakePreviewConnector{validateErr: errors.New("bad config")}
	r := newPreviewRouter(t, fake, "tenant-a", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestPreview_ConnectError(t *testing.T) {
	t.Parallel()
	fake := &fakePreviewConnector{connectErr: errors.New("auth failure")}
	r := newPreviewRouter(t, fake, "tenant-a", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestPreview_UnknownConnector(t *testing.T) {
	t.Parallel()
	r := newPreviewRouter(t, &fakePreviewConnector{}, "tenant-a", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "nope"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestPreview_MissingTenantContext(t *testing.T) {
	t.Parallel()
	r := newPreviewRouter(t, &fakePreviewConnector{}, "", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: "fake"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestPreview_EmptyConnectorType(t *testing.T) {
	t.Parallel()
	r := newPreviewRouter(t, &fakePreviewConnector{}, "tenant-a", admin.PreviewLimits{})
	w := postPreview(t, r, admin.PreviewRequest{ConnectorType: ""})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: %d", w.Code)
	}
}
