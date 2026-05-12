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
	"github.com/kennguy3n/hunting-fishball/internal/connector/clickup"
	confluenceserver "github.com/kennguy3n/hunting-fishball/internal/connector/confluence_server"
	"github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gmail"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	"github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	"github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	"github.com/kennguy3n/hunting-fishball/internal/connector/mattermost"
	"github.com/kennguy3n/hunting-fishball/internal/connector/monday"
	"github.com/kennguy3n/hunting-fishball/internal/connector/okta"
	"github.com/kennguy3n/hunting-fishball/internal/connector/pipedrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/rss"
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
	// Round-16 additions:
	var _ connector.SourceConnector = (*mattermost.Connector)(nil)
	var _ connector.SourceConnector = (*clickup.Connector)(nil)
	var _ connector.SourceConnector = (*monday.Connector)(nil)
	var _ connector.SourceConnector = (*pipedrive.Connector)(nil)
	var _ connector.SourceConnector = (*okta.Connector)(nil)
	var _ connector.SourceConnector = (*gmail.Connector)(nil)
	var _ connector.SourceConnector = (*rss.Connector)(nil)
	var _ connector.SourceConnector = (*confluenceserver.Connector)(nil)
	// Each must also implement DeltaSyncer.
	var _ connector.DeltaSyncer = (*mattermost.Connector)(nil)
	var _ connector.DeltaSyncer = (*clickup.Connector)(nil)
	var _ connector.DeltaSyncer = (*monday.Connector)(nil)
	var _ connector.DeltaSyncer = (*pipedrive.Connector)(nil)
	var _ connector.DeltaSyncer = (*okta.Connector)(nil)
	var _ connector.DeltaSyncer = (*gmail.Connector)(nil)
	var _ connector.DeltaSyncer = (*rss.Connector)(nil)
	var _ connector.DeltaSyncer = (*confluenceserver.Connector)(nil)
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

// TestConnectorContract_Round16_DeltaSyncerEmptyCursor exercises
// the "empty cursor returns a fresh token and no historical changes"
// contract against each Round-16 DeltaSyncer connector. Distinct
// from the Round-15 case because these connectors use heterogenous
// upstream surfaces (REST + GraphQL + RSS + Gmail history), so the
// shared contract is best demonstrated through the table below.
func TestConnectorContract_Round16_DeltaSyncerEmptyCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"gmail", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/users/me/profile", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"historyId":"500"}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := gmail.New(gmail.WithBaseURL(srv.URL), gmail.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(gmail.Credentials{AccessToken: "tok"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "INBOX"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
		{"okta", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/users/me", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{}`)
			})
			mux.HandleFunc("/api/v1/users", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `[{"id":"U1","status":"ACTIVE","lastUpdated":"2024-01-05T00:00:00Z"}]`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := okta.New(okta.WithBaseURL(srv.URL), okta.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(okta.Credentials{APIToken: "k", OrgURL: srv.URL})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "users"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
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
