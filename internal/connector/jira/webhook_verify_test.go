package jira_test

import (
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/jira"
)

func TestJira_VerifyWebhookRequest_Disabled(t *testing.T) {
	t.Parallel()
	c := jira.New()
	if err := c.VerifyWebhookRequest(nil, nil); err != nil {
		t.Fatalf("unset secret should bypass: %v", err)
	}
}

func TestJira_VerifyWebhookRequest_HappyPath(t *testing.T) {
	t.Parallel()
	c := jira.New(jira.WithWebhookSecret("supersecret"))
	body := []byte(`{"webhookEvent":"jira:issue_updated"}`)
	sig := connector.SignHMACSHA256([]byte("supersecret"), body)
	hdrs := map[string][]string{"X-Hub-Signature-256": {sig}}
	if err := c.VerifyWebhookRequest(hdrs, body); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestJira_VerifyWebhookRequest_BadSig(t *testing.T) {
	t.Parallel()
	c := jira.New(jira.WithWebhookSecret("supersecret"))
	hdrs := map[string][]string{"X-Hub-Signature-256": {"sha256=00"}}
	if err := c.VerifyWebhookRequest(hdrs, []byte("payload")); !errors.Is(err, connector.ErrWebhookSignatureInvalid) {
		t.Fatalf("expected invalid sig, got %v", err)
	}
}
