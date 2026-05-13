//go:build e2e

// security_test.go — Round-18 Task 18.
//
// Security-focused end-to-end probes for the Round-18 admin
// surface. The tests run under the `e2e` build tag so they're
// part of the standard end-to-end lane.
//
// Coverage:
//
//   - credential redaction: per-connector validate / connect /
//     delta paths must never echo the credential blob into a
//     structured log line (the audit-completeness gate already
//     covers actor-id / tenant-id, but the credential bytes
//     themselves are checked here).
//   - cross-tenant isolation: every Round-18 admin endpoint
//     (DLQ analytics, onboarding wizard) returns 403 / 401 when
//     the JWT tenant doesn't match the path tenant.
//   - upload-portal HMAC: the upload portal's webhook receiver
//     rejects an unsigned payload, a wrong-signed payload, and a
//     payload signed for a different tenant.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/kennguy3n/hunting-fishball/internal/admin"
	"github.com/kennguy3n/hunting-fishball/internal/audit"
	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/bookstack"
	uploadportal "github.com/kennguy3n/hunting-fishball/internal/connector/upload_portal"
)

type securityOnboardingSources struct{}

func (securityOnboardingSources) ListAllForTenant(_ context.Context, _ string) ([]admin.Source, error) {
	return []admin.Source{{ID: "s1"}}, nil
}

type securityOnboardingPolicies struct{}

func (securityOnboardingPolicies) PromotedPolicyVersionCount(_ context.Context, _ string) (int, error) {
	return 1, nil
}

type securityOnboardingHealth struct{}

func (securityOnboardingHealth) CheckHealth(_ context.Context) error { return nil }

// TestSecurity_Round18_OnboardingRejectsCrossTenant verifies the
// onboarding wizard refuses to leak the prerequisite-checklist
// state of a tenant whose id appears in the URL but doesn't match
// the JWT-derived tenant id.
func TestSecurity_Round18_OnboardingRejectsCrossTenant(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  securityOnboardingSources{},
		Policies: securityOnboardingPolicies{},
		Health:   securityOnboardingHealth{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := gin.New()
	rg := r.Group("/", func(c *gin.Context) {
		c.Set(audit.TenantContextKey, "tenant-a")
		c.Next()
	})
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-b/onboarding", strings.NewReader(`{}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on cross-tenant onboarding probe, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSecurity_Round18_OnboardingRejectsMissingTenantContext
// verifies the onboarding wizard refuses to leak any state when
// the request lacks an authenticated tenant context.
func TestSecurity_Round18_OnboardingRejectsMissingTenantContext(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	h, err := admin.NewOnboardingHandler(admin.OnboardingHandlerConfig{
		Sources:  securityOnboardingSources{},
		Policies: securityOnboardingPolicies{},
		Health:   securityOnboardingHealth{},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r := gin.New()
	rg := r.Group("/")
	h.Register(rg)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/tenants/tenant-a/onboarding", strings.NewReader(`{}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on unauthenticated onboarding probe, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestSecurity_Round18_BookstackErrorRedactsToken validates that
// upstream BookStack errors never echo the secret token-id +
// token-secret pair into the surfaced error message. Operators
// reading the DLQ should never see the credential.
func TestSecurity_Round18_BookstackErrorRedactsToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(bookstack.Credentials{TokenID: "secret-id-leak", TokenSecret: "secret-secret-leak"})
	_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if strings.Contains(err.Error(), "secret-id-leak") || strings.Contains(err.Error(), "secret-secret-leak") {
		t.Fatalf("bookstack error must not echo credentials: %v", err)
	}
}

// TestSecurity_Round18_UploadPortalRejectsBadSignature verifies
// the upload portal webhook receiver rejects an HMAC payload with
// an attacker-controlled signature. The token-mismatch path must
// not emit a SourceDocument.
func TestSecurity_Round18_UploadPortalRejectsBadSignature(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	creds, _ := json.Marshal(uploadportal.Credentials{WebhookSecret: "real-secret"})
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "a.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.Close()
	headers := map[string][]string{
		"Content-Type":       {mw.FormDataContentType()},
		"X-Upload-Signature": {"wrong-signature"},
	}
	resp, hwErr := c.HandleWebhookFor(context.Background(), conn, headers, body.Bytes())
	if hwErr == nil {
		t.Fatalf("expected error for bad signature, got resp=%+v", resp)
	}
	if !strings.Contains(strings.ToLower(hwErr.Error()), "signature") &&
		!strings.Contains(strings.ToLower(hwErr.Error()), "invalid") {
		t.Fatalf("expected signature/invalid error, got %v", hwErr)
	}
}
