package jira_test

import (
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/jira"
)

// Atlassian Cloud sends the SHA-256 signature in the
// `X-Atlassian-Webhook-Signature` header; this test pins that
// behaviour so a refactor that drops the header silently is caught.
func TestJira_VerifyWebhookRequest_AtlassianHeader(t *testing.T) {
	t.Parallel()
	c := jira.New(jira.WithWebhookSecret("supersecret"))
	body := []byte(`{"webhookEvent":"jira:issue_updated"}`)
	sig := connector.SignHMACSHA256([]byte("supersecret"), body)
	hdrs := map[string][]string{"X-Atlassian-Webhook-Signature": {sig}}
	if err := c.VerifyWebhookRequest(hdrs, body); err != nil {
		t.Fatalf("verify: %v", err)
	}
}
