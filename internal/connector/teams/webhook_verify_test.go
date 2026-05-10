package teams_test

import (
	"errors"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/teams"
)

func TestTeams_VerifyWebhookRequest_Disabled(t *testing.T) {
	t.Parallel()
	c := teams.New()
	if err := c.VerifyWebhookRequest(nil, nil); err != nil {
		t.Fatalf("unset secret should bypass: %v", err)
	}
}

func TestTeams_VerifyWebhookRequest_HappyPath(t *testing.T) {
	t.Parallel()
	c := teams.New(teams.WithWebhookSecret("supersecret"))
	hdrs := map[string][]string{"Authorization": {"supersecret"}}
	if err := c.VerifyWebhookRequest(hdrs, nil); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestTeams_VerifyWebhookRequest_Mismatch(t *testing.T) {
	t.Parallel()
	c := teams.New(teams.WithWebhookSecret("supersecret"))
	hdrs := map[string][]string{"Authorization": {"wrong"}}
	if err := c.VerifyWebhookRequest(hdrs, nil); !errors.Is(err, connector.ErrWebhookTokenInvalid) {
		t.Fatalf("expected token invalid, got %v", err)
	}
}

func TestTeams_VerifyWebhookRequest_Missing(t *testing.T) {
	t.Parallel()
	c := teams.New(teams.WithWebhookSecret("supersecret"))
	if err := c.VerifyWebhookRequest(nil, nil); !errors.Is(err, connector.ErrWebhookSignatureMissing) {
		t.Fatalf("expected missing, got %v", err)
	}
}
