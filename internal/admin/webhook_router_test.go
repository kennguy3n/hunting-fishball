package admin_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// fakeReceiver implements WebhookReceiver. When verifyErr is set
// it also satisfies WebhookVerifier and rejects requests.
type fakeReceiver struct {
	path      string
	changes   []connector.DocumentChange
	handleErr error
	verifyErr error
	verified  bool
	handled   bool
}

func (f *fakeReceiver) WebhookPath() string { return f.path }

func (f *fakeReceiver) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	f.handled = true
	if f.handleErr != nil {
		return nil, f.handleErr
	}
	return f.changes, nil
}

// withVerifier promotes f to WebhookReceiver+WebhookVerifier.
type fakeVerifyingReceiver struct {
	fakeReceiver
}

func (f *fakeVerifyingReceiver) VerifyWebhookRequest(_ map[string][]string, _ []byte) error {
	f.verified = true
	return f.verifyErr
}

// fakeLookup is the in-memory ConnectorLookup the webhook router
// calls. Behaves like the real repo but without GORM.
type fakeLookup struct {
	rows map[string]*admin.Source
	err  error
}

func newFakeLookup(rows ...*admin.Source) *fakeLookup {
	m := make(map[string]*admin.Source, len(rows))
	for _, r := range rows {
		m[r.ID] = r
	}
	return &fakeLookup{rows: m}
}

func (f *fakeLookup) GetByID(_ context.Context, id string) (*admin.Source, error) {
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.rows[id]
	if !ok {
		return nil, admin.ErrSourceNotFound
	}
	return r, nil
}

func newWebhookRouter(t *testing.T, lookup admin.ConnectorLookup, resolver admin.ConnectorResolver, ad *fakeAudit) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	rt, err := admin.NewWebhookRouter(admin.WebhookRouterConfig{
		Lookup:   lookup,
		Resolver: resolver,
		Audit:    ad,
	})
	if err != nil {
		t.Fatalf("NewWebhookRouter: %v", err)
	}
	rt.Register(&r.RouterGroup)
	return r
}

func TestWebhookRouter_HappyPath_NoVerifier(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID:            "01HW1",
		TenantID:      "tenant-a",
		ConnectorType: "slack",
		Status:        admin.SourceStatusActive,
	}
	rcv := &fakeReceiver{path: "/slack", changes: []connector.DocumentChange{{Kind: connector.ChangeUpserted, Ref: connector.DocumentRef{ID: "doc-1"}}}}
	ad := &fakeAudit{}
	r := newWebhookRouter(t, newFakeLookup(src),
		func(name string) (connector.WebhookReceiver, error) {
			if name != "slack" {
				return nil, errors.New("unexpected connector " + name)
			}
			return rcv, nil
		}, ad)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/slack/01HW1", bytes.NewBufferString(`{"event":"created"}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !rcv.handled {
		t.Fatal("HandleWebhook never called")
	}
	wantActions(t, ad, "webhook.received", "webhook.processed")
}

func TestWebhookRouter_VerifierRejection(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID: "01HW2", TenantID: "tenant-a", ConnectorType: "notion",
		Status: admin.SourceStatusActive,
	}
	rcv := &fakeVerifyingReceiver{fakeReceiver: fakeReceiver{path: "/notion"}}
	rcv.verifyErr = errors.New("bad sig")
	ad := &fakeAudit{}
	r := newWebhookRouter(t, newFakeLookup(src),
		func(_ string) (connector.WebhookReceiver, error) { return rcv, nil }, ad)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/notion/01HW2", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", w.Code)
	}
	if !rcv.verified {
		t.Fatal("verifier never invoked")
	}
	if rcv.handled {
		t.Fatal("HandleWebhook must not run after verification failure")
	}
	wantActions(t, ad, "webhook.received", "webhook.failed")
}

func TestWebhookRouter_HandlerError_ProducesFailedEvent(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID: "01HW3", TenantID: "tenant-a", ConnectorType: "github",
		Status: admin.SourceStatusActive,
	}
	rcv := &fakeReceiver{path: "/github", handleErr: errors.New("boom")}
	ad := &fakeAudit{}
	r := newWebhookRouter(t, newFakeLookup(src),
		func(_ string) (connector.WebhookReceiver, error) { return rcv, nil }, ad)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/github/01HW3", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d", w.Code)
	}
	wantActions(t, ad, "webhook.received", "webhook.failed")
}

func wantActions(t *testing.T, ad *fakeAudit, want ...string) {
	t.Helper()
	got := ad.actions()
	if len(got) != len(want) {
		t.Fatalf("audit actions: got %v want %v", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("audit action[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestWebhookRouter_SourceNotFound(t *testing.T) {
	t.Parallel()
	r := newWebhookRouter(t, newFakeLookup(),
		func(_ string) (connector.WebhookReceiver, error) {
			t.Fatal("Resolver must not run when source is missing")
			return nil, nil
		}, &fakeAudit{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/slack/missing", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestWebhookRouter_ConnectorMismatch_404(t *testing.T) {
	t.Parallel()
	// Source row says it's slack, URL claims it's notion. Must
	// 404 (NOT 400) to avoid leaking source-id existence.
	src := &admin.Source{
		ID: "01HW4", TenantID: "tenant-a", ConnectorType: "slack",
		Status: admin.SourceStatusActive,
	}
	r := newWebhookRouter(t, newFakeLookup(src),
		func(_ string) (connector.WebhookReceiver, error) {
			t.Fatal("Resolver must not run on mismatch")
			return nil, nil
		}, &fakeAudit{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/notion/01HW4", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestWebhookRouter_PausedSource_409(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID: "01HW5", TenantID: "tenant-a", ConnectorType: "slack",
		Status: admin.SourceStatusPaused,
	}
	r := newWebhookRouter(t, newFakeLookup(src),
		func(_ string) (connector.WebhookReceiver, error) {
			t.Fatal("Resolver must not run for non-active sources")
			return nil, nil
		}, &fakeAudit{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/slack/01HW5", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status: %d", w.Code)
	}
}

func TestWebhookRouter_NotImplemented_When_NotReceiver(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID: "01HW6", TenantID: "tenant-a", ConnectorType: "static",
		Status: admin.SourceStatusActive,
	}
	r := newWebhookRouter(t, newFakeLookup(src),
		func(_ string) (connector.WebhookReceiver, error) {
			return nil, admin.ErrConnectorDoesNotReceiveWebhooks
		}, &fakeAudit{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/static/01HW6", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status: %d", w.Code)
	}
}

// --------------- per-connection dispatch (WebhookReceiverFor) ---------------

// fakeConnReceiverFor implements WebhookReceiver + WebhookReceiverFor +
// WebhookVerifierFor and also SourceConnector (Connect/Disconnect).
// It lets the webhook router tests exercise the full per-connection path
// without touching the real connector registry.
type fakeConnReceiverFor struct {
	path      string
	verifyErr error
	handleErr error
	changes   []connector.DocumentChange
	verified  bool
	handled   bool
	connected bool
	connCreds []byte // stashed from Connect so tests can assert credential propagation
}

func (f *fakeConnReceiverFor) WebhookPath() string { return f.path }

// HandleWebhook is the stateless fallback the router must NOT call
// when the connector satisfies WebhookReceiverFor.
func (f *fakeConnReceiverFor) HandleWebhook(_ context.Context, _ []byte) ([]connector.DocumentChange, error) {
	return nil, errors.New("HandleWebhook must not be called for a WebhookReceiverFor connector")
}

func (f *fakeConnReceiverFor) HandleWebhookFor(_ context.Context, _ connector.Connection, _ map[string][]string, _ []byte) ([]connector.DocumentChange, error) {
	f.handled = true
	if f.handleErr != nil {
		return nil, f.handleErr
	}
	return f.changes, nil
}

func (f *fakeConnReceiverFor) VerifyWebhookRequestFor(_ connector.Connection, _ map[string][]string, _ []byte) error {
	f.verified = true
	return f.verifyErr
}

// SourceConnector surface — minimal stubs for Connect/Disconnect.
type fakeConnSourceConnector struct {
	*fakeConnReceiverFor
}

type fakeConn struct{ tid, sid string }

func (c *fakeConn) TenantID() string { return c.tid }
func (c *fakeConn) SourceID() string { return c.sid }

func (f *fakeConnSourceConnector) Validate(_ context.Context, _ connector.ConnectorConfig) error {
	return nil
}

func (f *fakeConnSourceConnector) Connect(_ context.Context, cfg connector.ConnectorConfig) (connector.Connection, error) {
	f.connected = true
	f.connCreds = cfg.Credentials
	return &fakeConn{tid: cfg.TenantID, sid: cfg.SourceID}, nil
}

func (f *fakeConnSourceConnector) Disconnect(_ context.Context, _ connector.Connection) error {
	return nil
}

func (f *fakeConnSourceConnector) ListNamespaces(_ context.Context, _ connector.Connection) ([]connector.Namespace, error) {
	return nil, connector.ErrNotSupported
}

func (f *fakeConnSourceConnector) ListDocuments(_ context.Context, _ connector.Connection, _ connector.Namespace, _ connector.ListOpts) (connector.DocumentIterator, error) {
	return nil, connector.ErrNotSupported
}

func (f *fakeConnSourceConnector) Subscribe(_ context.Context, _ connector.Connection, _ connector.Namespace) (connector.Subscription, error) {
	return nil, connector.ErrNotSupported
}

func (f *fakeConnSourceConnector) FetchDocument(_ context.Context, _ connector.Connection, _ connector.DocumentRef) (*connector.Document, error) {
	return nil, connector.ErrNotSupported
}

func newPerConnWebhookRouter(t *testing.T, lookup admin.ConnectorLookup, rcvFor *fakeConnReceiverFor, ad *fakeAudit) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	sc := &fakeConnSourceConnector{fakeConnReceiverFor: rcvFor}
	r := gin.New()
	rt, err := admin.NewWebhookRouter(admin.WebhookRouterConfig{
		Lookup: lookup,
		Resolver: func(_ string) (connector.WebhookReceiver, error) {
			return rcvFor, nil
		},
		SourceResolver: func(_ string) (connector.SourceConnector, error) {
			return sc, nil
		},
		Credentials: func(src *admin.Source) ([]byte, error) {
			v, ok := src.Config["credentials"]
			if !ok || v == nil {
				return nil, nil
			}
			if b, ok := v.([]byte); ok {
				return b, nil
			}
			return nil, errors.New("unexpected credentials type")
		},
		Audit: ad,
	})
	if err != nil {
		t.Fatalf("NewWebhookRouter: %v", err)
	}
	rt.Register(&r.RouterGroup)
	return r
}

func TestWebhookRouter_PerConnection_HappyPath(t *testing.T) {
	t.Parallel()
	creds := []byte(`{"webhook_secret":"s"}`)
	src := &admin.Source{
		ID: "01HW7", TenantID: "tenant-a", ConnectorType: "upload_portal",
		Status: admin.SourceStatusActive,
		Config: admin.JSONMap{"credentials": creds},
	}
	rcvFor := &fakeConnReceiverFor{
		path:    "/upload_portal",
		changes: []connector.DocumentChange{{Kind: connector.ChangeUpserted, Ref: connector.DocumentRef{ID: "f1"}}},
	}
	ad := &fakeAudit{}
	r := newPerConnWebhookRouter(t, newFakeLookup(src), rcvFor, ad)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/upload_portal/01HW7", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !rcvFor.connected {
		t.Fatal("Connect never called — router must materialise a Connection for WebhookReceiverFor")
	}
	if !rcvFor.verified {
		t.Fatal("VerifyWebhookRequestFor never called")
	}
	if !rcvFor.handled {
		t.Fatal("HandleWebhookFor never called")
	}
	if string(rcvFor.connCreds) != string(creds) {
		t.Fatalf("credential propagation: got %q want %q", rcvFor.connCreds, creds)
	}
	wantActions(t, ad, "webhook.received", "webhook.processed")
}

func TestWebhookRouter_PerConnection_VerifierRejects(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID: "01HW8", TenantID: "tenant-a", ConnectorType: "upload_portal",
		Status: admin.SourceStatusActive,
		Config: admin.JSONMap{"credentials": []byte(`{"webhook_secret":"s"}`)},
	}
	rcvFor := &fakeConnReceiverFor{
		path:      "/upload_portal",
		verifyErr: errors.New("bad sig"),
	}
	ad := &fakeAudit{}
	r := newPerConnWebhookRouter(t, newFakeLookup(src), rcvFor, ad)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/upload_portal/01HW8", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !rcvFor.verified {
		t.Fatal("VerifyWebhookRequestFor never called")
	}
	if rcvFor.handled {
		t.Fatal("HandleWebhookFor must not run after verification failure")
	}
	wantActions(t, ad, "webhook.received", "webhook.failed")
}

func TestWebhookRouter_PerConnection_HandlerError(t *testing.T) {
	t.Parallel()
	src := &admin.Source{
		ID: "01HW9", TenantID: "tenant-a", ConnectorType: "upload_portal",
		Status: admin.SourceStatusActive,
		Config: admin.JSONMap{"credentials": []byte(`{}`)},
	}
	rcvFor := &fakeConnReceiverFor{
		path:      "/upload_portal",
		handleErr: errors.New("parse error"),
	}
	ad := &fakeAudit{}
	r := newPerConnWebhookRouter(t, newFakeLookup(src), rcvFor, ad)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/upload_portal/01HW9", bytes.NewBufferString(`{}`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	wantActions(t, ad, "webhook.received", "webhook.failed")
}
