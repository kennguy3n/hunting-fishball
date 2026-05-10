package gitlab_test

import (
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gitlab"
)

func TestGitLab_VerifyWebhookRequest_Disabled(t *testing.T) {
	t.Parallel()
	c := gitlab.New()
	if err := c.VerifyWebhookRequest(nil, nil); err != nil {
		t.Fatalf("unset secret should bypass: %v", err)
	}
}

func TestGitLab_VerifyWebhookRequest_HappyPath(t *testing.T) {
	t.Parallel()
	c := gitlab.New(gitlab.WithWebhookSecret("supersecret"))
	hdrs := map[string][]string{"X-Gitlab-Token": {"supersecret"}}
	if err := c.VerifyWebhookRequest(hdrs, nil); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestGitLab_VerifyWebhookRequest_BadToken(t *testing.T) {
	t.Parallel()
	c := gitlab.New(gitlab.WithWebhookSecret("supersecret"))
	hdrs := map[string][]string{"X-Gitlab-Token": {"wrong"}}
	if err := c.VerifyWebhookRequest(hdrs, nil); !errors.Is(err, connector.ErrWebhookTokenInvalid) {
		t.Fatalf("expected token invalid, got %v", err)
	}
}

func TestGitLab_VerifyWebhookRequest_Missing(t *testing.T) {
	t.Parallel()
	c := gitlab.New(gitlab.WithWebhookSecret("supersecret"))
	if err := c.VerifyWebhookRequest(nil, nil); !errors.Is(err, connector.ErrWebhookSignatureMissing) {
		t.Fatalf("expected missing, got %v", err)
	}
}
