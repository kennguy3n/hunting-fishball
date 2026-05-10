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
