package connector_test

import (
	"context"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
)

// deltaConnector embeds the basic mockConnector and additionally satisfies
// DeltaSyncer.
type deltaConnector struct {
	mockConnector

	calls int
}

func (d *deltaConnector) DeltaSync(
	_ context.Context, _ connector.Connection, _ connector.Namespace, cursor string,
) ([]connector.DocumentChange, string, error) {
	d.calls++
	changes := []connector.DocumentChange{
		{Kind: connector.ChangeUpserted, Ref: connector.DocumentRef{ID: "a"}},
		{Kind: connector.ChangeDeleted, Ref: connector.DocumentRef{ID: "b"}},
	}

	return changes, cursor + ":next", nil
}

// webhookConnector additionally satisfies WebhookReceiver.
type webhookConnector struct {
	mockConnector
}

func (*webhookConnector) WebhookPath() string { return "/mock" }
func (*webhookConnector) HandleWebhook(_ context.Context, payload []byte) ([]connector.DocumentChange, error) {
	if len(payload) == 0 {
		return nil, nil
	}

	return []connector.DocumentChange{{
		Kind: connector.ChangeUpserted,
		Ref:  connector.DocumentRef{ID: string(payload)},
	}}, nil
}

// provisionerConnector additionally satisfies Provisioner.
type provisionerConnector struct {
	mockConnector

	provisioned   []connector.Grant
	deprovisioned []connector.Grant
}

func (p *provisionerConnector) Provision(_ context.Context, _ connector.Connection, grants []connector.Grant) error {
	p.provisioned = append(p.provisioned, grants...)

	return nil
}

func (p *provisionerConnector) Deprovision(_ context.Context, _ connector.Connection, grants []connector.Grant) error {
	p.deprovisioned = append(p.deprovisioned, grants...)

	return nil
}

func TestOptionalInterface_DeltaSyncerTypeAssertion(t *testing.T) {
	t.Parallel()

	var c connector.SourceConnector = &deltaConnector{}

	ds, ok := c.(connector.DeltaSyncer)
	if !ok {
		t.Fatalf("deltaConnector should implement DeltaSyncer")
	}

	changes, cur, err := ds.DeltaSync(context.Background(), &mockConnection{}, connector.Namespace{}, "v1")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(changes))
	}
	if cur != "v1:next" {
		t.Fatalf("unexpected cursor: %q", cur)
	}
}

func TestOptionalInterface_PlainConnectorDoesNotImplement(t *testing.T) {
	t.Parallel()

	var c connector.SourceConnector = &mockConnector{}

	if _, ok := c.(connector.DeltaSyncer); ok {
		t.Fatal("plain mockConnector must not implement DeltaSyncer")
	}
	if _, ok := c.(connector.WebhookReceiver); ok {
		t.Fatal("plain mockConnector must not implement WebhookReceiver")
	}
	if _, ok := c.(connector.Provisioner); ok {
		t.Fatal("plain mockConnector must not implement Provisioner")
	}
}

func TestOptionalInterface_WebhookReceiver(t *testing.T) {
	t.Parallel()

	var c connector.SourceConnector = &webhookConnector{}

	wr, ok := c.(connector.WebhookReceiver)
	if !ok {
		t.Fatalf("webhookConnector should implement WebhookReceiver")
	}
	if wr.WebhookPath() != "/mock" {
		t.Fatalf("unexpected path: %q", wr.WebhookPath())
	}

	changes, err := wr.HandleWebhook(context.Background(), []byte("doc-42"))
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(changes) != 1 || changes[0].Ref.ID != "doc-42" {
		t.Fatalf("unexpected changes: %+v", changes)
	}
}

func TestOptionalInterface_Provisioner(t *testing.T) {
	t.Parallel()

	pc := &provisionerConnector{}

	var c connector.SourceConnector = pc

	pr, ok := c.(connector.Provisioner)
	if !ok {
		t.Fatalf("provisionerConnector should implement Provisioner")
	}

	grants := []connector.Grant{
		{PrincipalID: "u1", PrincipalType: "user", ResourceID: "r1", Permission: "reader"},
	}
	if err := pr.Provision(context.Background(), &mockConnection{}, grants); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := pr.Deprovision(context.Background(), &mockConnection{}, grants); err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
	if len(pc.provisioned) != 1 || len(pc.deprovisioned) != 1 {
		t.Fatalf("unexpected counts: prov=%d deprov=%d", len(pc.provisioned), len(pc.deprovisioned))
	}
}
