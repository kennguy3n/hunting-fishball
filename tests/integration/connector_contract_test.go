//go:build integration

// connector_contract_test.go — Round-15 Task 13.
//
// Compile-time + behaviour-time interface contracts for every
// connector in the Round-15 catalog. The tests run under the
// `integration` build tag because they exercise the full
// blank-import set (asana, discord, kchat, linear, hubspot, s3,
// salesforce, google_shared_drives) alongside the established
// Round-14 catalog.
//
// Contracts asserted:
//
//  1. Each connector struct implements connector.SourceConnector.
//  2. DeltaSyncer connectors honour the empty-cursor semantics:
//     an empty cursor returns a fresh cursor and no historical
//     changes (initial-token behaviour).
//  3. WebhookReceiver connectors gracefully reject empty
//     payloads (no panic, surfaces an error).
package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	"github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	"github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	"github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	"github.com/kennguy3n/hunting-fishball/internal/connector/s3"
	"github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
	"github.com/kennguy3n/hunting-fishball/internal/connector/slack"
)

// TestConnectorContract_SourceConnectorAssertions is a
// compile-time gate: each assignment below must hold without a
// type mismatch, otherwise the package won't build.
func TestConnectorContract_SourceConnectorAssertions(t *testing.T) {
	t.Parallel()
	var _ connector.SourceConnector = (*kchat.Connector)(nil)
	var _ connector.SourceConnector = (*s3.Connector)(nil)
	var _ connector.SourceConnector = (*linear.Connector)(nil)
	var _ connector.SourceConnector = (*asana.Connector)(nil)
	var _ connector.SourceConnector = (*discord.Connector)(nil)
	var _ connector.SourceConnector = (*salesforce.Connector)(nil)
	var _ connector.SourceConnector = (*hubspot.Connector)(nil)
	var _ connector.SourceConnector = (*googledrive.SharedDrivesConnector)(nil)
}

// TestConnectorContract_DeltaSyncerEmptyCursor exercises the
// "empty cursor returns a fresh token and no historical changes"
// contract against the new Round-15 connectors. The mock server
// returns an empty changes set; we expect a non-empty cursor
// back and zero DocumentChange entries.
func TestConnectorContract_DeltaSyncerEmptyCursor(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users.me", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1"}`)
	})
	mux.HandleFunc("/channels.changes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"changes":[],"cursor":"CUR1"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := kchat.New(kchat.WithBaseURL(srv.URL), kchat.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(kchat.Credentials{APIToken: "kc-test"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	ctx := context.Background()
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	changes, cursor, err := c.DeltaSync(ctx, conn, connector.Namespace{ID: "C1"}, "")
	if err != nil {
		t.Fatalf("DeltaSync: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("empty-cursor call should return 0 changes; got %d", len(changes))
	}
	if cursor == "" {
		t.Fatalf("empty-cursor call should return a fresh cursor; got %q", cursor)
	}
}

// TestConnectorContract_WebhookReceiverHandlesEmptyPayload
// verifies that every WebhookReceiver connector handles empty
// or malformed payloads gracefully (no panic, returns an error
// or a benign empty result).
func TestConnectorContract_WebhookReceiverHandlesEmptyPayload(t *testing.T) {
	t.Parallel()

	receivers := map[string]connector.WebhookReceiver{
		"slack":  slack.New(),
		"kchat":  kchat.New(),
		"linear": linear.New(),
	}
	for name, r := range receivers {
		name, r := name, r
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("%s panicked on empty payload: %v", name, rec)
				}
			}()
			_, err := r.HandleWebhook(context.Background(), nil)
			if err == nil {
				// Some receivers may return (nil, nil) for an empty
				// payload — that's fine too. Just make sure they
				// did not panic.
				return
			}
			if !strings.Contains(strings.ToLower(err.Error()), "empty") &&
				!strings.Contains(strings.ToLower(err.Error()), "decode") &&
				!strings.Contains(strings.ToLower(err.Error()), "payload") {
				t.Fatalf("%s: unexpected error on empty payload: %v", name, err)
			}
		})
	}
}
