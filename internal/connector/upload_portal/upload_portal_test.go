package uploadportal_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	uploadportal "github.com/kennguy3n/hunting-fishball/internal/connector/upload_portal"
)

func validCreds(t *testing.T) []byte {
	t.Helper()
	b, _ := json.Marshal(uploadportal.Credentials{WebhookSecret: "sec", AllowedMIME: []string{"application/pdf"}, MaxSizeBytes: 1024})

	return b
}

func TestUploadPortal_Validate(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	cases := []struct {
		name string
		cfg  connector.ConnectorConfig
		ok   bool
	}{
		{"happy", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)}, true},
		{"no creds", connector.ConnectorConfig{TenantID: "t", SourceID: "s"}, false},
		{"no secret", connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: []byte(`{}`)}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := c.Validate(context.Background(), tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.ok && !errors.Is(err, connector.ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestUploadPortal_Connect_AcceptsValidCreds(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	conn, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conn.TenantID() != "t" || conn.SourceID() != "s" {
		t.Fatalf("conn=%+v", conn)
	}
}

func TestUploadPortal_ErrNotSupported(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	if _, err := c.FetchDocument(context.Background(), conn, connector.DocumentRef{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported on Fetch, got %v", err)
	}
	if _, err := c.Subscribe(context.Background(), conn, connector.Namespace{}); !errors.Is(err, connector.ErrNotSupported) {
		t.Fatalf("expected ErrNotSupported on Subscribe, got %v", err)
	}
}

// buildMultipart returns a multipart body with one file part.
func buildMultipart(t *testing.T, mime, filename string, body []byte) ([]byte, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	return buf.Bytes(), w.Boundary()
}

func hmacHex(secret, payload []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)

	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestUploadPortal_HandleWebhookFor_HappyPath(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	c := uploadportal.New(uploadportal.WithNow(func() time.Time { return fixed }))
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	body, _ := buildMultipart(t, "application/pdf", "spec.pdf", []byte("PDF-CONTENT"))
	sig := hmacHex([]byte("sec"), body)
	headers := map[string][]string{"X-Upload-Signature-256": {sig}}
	changes, err := c.HandleWebhookFor(context.Background(), conn, headers, body)
	if err != nil {
		t.Fatalf("HandleWebhookFor: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "spec.pdf" || changes[0].Kind != connector.ChangeUpserted {
		t.Fatalf("changes=%+v", changes)
	}
	if !changes[0].Ref.UpdatedAt.Equal(fixed) {
		t.Fatalf("updated=%v want %v", changes[0].Ref.UpdatedAt, fixed)
	}
}

func TestUploadPortal_HandleWebhookFor_RejectsBadMIME(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	body, _ := buildMultipart(t, "image/png", "img.png", []byte("PNG"))
	sig := hmacHex([]byte("sec"), body)
	headers := map[string][]string{"X-Upload-Signature-256": {sig}}
	_, err := c.HandleWebhookFor(context.Background(), conn, headers, body)
	if err == nil || !strings.Contains(err.Error(), "not in allowed list") {
		t.Fatalf("expected MIME rejection, got %v", err)
	}
}

func TestUploadPortal_HandleWebhookFor_RejectsOversize(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	creds, _ := json.Marshal(uploadportal.Credentials{WebhookSecret: "sec", AllowedMIME: []string{"application/pdf"}, MaxSizeBytes: 4})
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
	body, _ := buildMultipart(t, "application/pdf", "big.pdf", []byte("MORE-THAN-FOUR-BYTES"))
	sig := hmacHex([]byte("sec"), body)
	headers := map[string][]string{"X-Upload-Signature-256": {sig}}
	_, err := c.HandleWebhookFor(context.Background(), conn, headers, body)
	if err == nil || !strings.Contains(err.Error(), "exceeds max_size_bytes") {
		t.Fatalf("expected size rejection, got %v", err)
	}
}

func TestUploadPortal_HandleWebhookFor_RejectsBadSignature(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	conn, _ := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: validCreds(t)})
	body, _ := buildMultipart(t, "application/pdf", "spec.pdf", []byte("PDF"))
	headers := map[string][]string{"X-Upload-Signature-256": {"sha256=deadbeef"}}
	_, err := c.HandleWebhookFor(context.Background(), conn, headers, body)
	if err == nil {
		t.Fatal("expected signature error")
	}
}

func TestUploadPortal_WebhookPath(t *testing.T) {
	t.Parallel()
	c := uploadportal.New()
	if c.WebhookPath() != "/upload_portal" {
		t.Fatalf("path=%q", c.WebhookPath())
	}
}
