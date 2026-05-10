package slack_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/slack"
)

const fixedSecret = "test-signing-secret"

func slackSig(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:"))
	mac.Write([]byte(ts))
	mac.Write([]byte{':'})
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSlack_VerifyWebhookRequest_Disabled(t *testing.T) {
	t.Parallel()
	c := slack.New()
	if err := c.VerifyWebhookRequest(nil, nil); err != nil {
		t.Fatalf("unset secret should bypass: %v", err)
	}
}

func TestSlack_VerifyWebhookRequest_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"event":"x"}`)
	c := slack.New(
		slack.WithSigningSecret(fixedSecret),
		slack.WithNow(func() time.Time { return now }),
	)
	hdrs := map[string][]string{
		"X-Slack-Signature":         {slackSig(fixedSecret, ts, body)},
		"X-Slack-Request-Timestamp": {ts},
	}
	if err := c.VerifyWebhookRequest(hdrs, body); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestSlack_VerifyWebhookRequest_BadSig(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := strconv.FormatInt(now.Unix(), 10)
	c := slack.New(
		slack.WithSigningSecret(fixedSecret),
		slack.WithNow(func() time.Time { return now }),
	)
	hdrs := map[string][]string{
		"X-Slack-Signature":         {"v0=00"},
		"X-Slack-Request-Timestamp": {ts},
	}
	err := c.VerifyWebhookRequest(hdrs, []byte("payload"))
	if !errors.Is(err, connector.ErrWebhookSignatureInvalid) {
		t.Fatalf("expected invalid sig, got %v", err)
	}
}

func TestSlack_VerifyWebhookRequest_MissingHeaders(t *testing.T) {
	t.Parallel()
	c := slack.New(slack.WithSigningSecret(fixedSecret))
	err := c.VerifyWebhookRequest(map[string][]string{}, []byte("x"))
	if !errors.Is(err, connector.ErrWebhookSignatureMissing) {
		t.Fatalf("expected missing sig, got %v", err)
	}
}

func TestSlack_VerifyWebhookRequest_StaleTimestamp(t *testing.T) {
	t.Parallel()
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	stale := strconv.FormatInt(now.Add(-1*time.Hour).Unix(), 10)
	body := []byte(`{}`)
	c := slack.New(
		slack.WithSigningSecret(fixedSecret),
		slack.WithNow(func() time.Time { return now }),
	)
	hdrs := map[string][]string{
		"X-Slack-Signature":         {slackSig(fixedSecret, stale, body)},
		"X-Slack-Request-Timestamp": {stale},
	}
	err := c.VerifyWebhookRequest(hdrs, body)
	if !errors.Is(err, connector.ErrWebhookSignatureInvalid) {
		t.Fatalf("stale timestamp must be rejected, got %v", err)
	}
}
