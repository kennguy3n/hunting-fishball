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
	"github.com/kennguy3n/hunting-fishball/internal/connector/airtable"
	"github.com/kennguy3n/hunting-fishball/internal/connector/asana"
	azureblob "github.com/kennguy3n/hunting-fishball/internal/connector/azure_blob"
	"github.com/kennguy3n/hunting-fishball/internal/connector/bamboohr"
	"github.com/kennguy3n/hunting-fishball/internal/connector/bitbucket"
	"github.com/kennguy3n/hunting-fishball/internal/connector/bookstack"
	"github.com/kennguy3n/hunting-fishball/internal/connector/clickup"
	"github.com/kennguy3n/hunting-fishball/internal/connector/coda"
	confluenceserver "github.com/kennguy3n/hunting-fishball/internal/connector/confluence_server"
	"github.com/kennguy3n/hunting-fishball/internal/connector/discord"
	"github.com/kennguy3n/hunting-fishball/internal/connector/egnyte"
	entraid "github.com/kennguy3n/hunting-fishball/internal/connector/entra_id"
	"github.com/kennguy3n/hunting-fishball/internal/connector/freshdesk"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gcs"
	"github.com/kennguy3n/hunting-fishball/internal/connector/gmail"
	googleworkspace "github.com/kennguy3n/hunting-fishball/internal/connector/google_workspace"
	"github.com/kennguy3n/hunting-fishball/internal/connector/googledrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/hubspot"
	"github.com/kennguy3n/hunting-fishball/internal/connector/intercom"
	"github.com/kennguy3n/hunting-fishball/internal/connector/kchat"
	"github.com/kennguy3n/hunting-fishball/internal/connector/linear"
	"github.com/kennguy3n/hunting-fishball/internal/connector/mattermost"
	"github.com/kennguy3n/hunting-fishball/internal/connector/monday"
	"github.com/kennguy3n/hunting-fishball/internal/connector/okta"
	"github.com/kennguy3n/hunting-fishball/internal/connector/outlook"
	"github.com/kennguy3n/hunting-fishball/internal/connector/personio"
	"github.com/kennguy3n/hunting-fishball/internal/connector/pipedrive"
	"github.com/kennguy3n/hunting-fishball/internal/connector/rss"
	"github.com/kennguy3n/hunting-fishball/internal/connector/s3"
	"github.com/kennguy3n/hunting-fishball/internal/connector/salesforce"
	"github.com/kennguy3n/hunting-fishball/internal/connector/servicenow"
	sharepointonprem "github.com/kennguy3n/hunting-fishball/internal/connector/sharepoint_onprem"
	"github.com/kennguy3n/hunting-fishball/internal/connector/sitemap"
	"github.com/kennguy3n/hunting-fishball/internal/connector/slack"
	"github.com/kennguy3n/hunting-fishball/internal/connector/trello"
	uploadportal "github.com/kennguy3n/hunting-fishball/internal/connector/upload_portal"
	"github.com/kennguy3n/hunting-fishball/internal/connector/webex"
	"github.com/kennguy3n/hunting-fishball/internal/connector/workday"
	"github.com/kennguy3n/hunting-fishball/internal/connector/zendesk"
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
	// Round-17 additions:
	var _ connector.SourceConnector = (*entraid.Connector)(nil)
	var _ connector.SourceConnector = (*googleworkspace.Connector)(nil)
	var _ connector.SourceConnector = (*outlook.Connector)(nil)
	var _ connector.SourceConnector = (*workday.Connector)(nil)
	var _ connector.SourceConnector = (*bamboohr.Connector)(nil)
	var _ connector.SourceConnector = (*personio.Connector)(nil)
	var _ connector.SourceConnector = (*sitemap.Connector)(nil)
	var _ connector.SourceConnector = (*coda.Connector)(nil)
	var _ connector.DeltaSyncer = (*entraid.Connector)(nil)
	var _ connector.DeltaSyncer = (*googleworkspace.Connector)(nil)
	var _ connector.DeltaSyncer = (*outlook.Connector)(nil)
	var _ connector.DeltaSyncer = (*workday.Connector)(nil)
	var _ connector.DeltaSyncer = (*bamboohr.Connector)(nil)
	var _ connector.DeltaSyncer = (*personio.Connector)(nil)
	var _ connector.DeltaSyncer = (*sitemap.Connector)(nil)
	var _ connector.DeltaSyncer = (*coda.Connector)(nil)

	// Round-18 Task 17 — compile-time assertions for the 6 new
	// connectors. Mirrors the per-round blocks above.
	var _ connector.SourceConnector = (*sharepointonprem.Connector)(nil)
	var _ connector.SourceConnector = (*azureblob.Connector)(nil)
	var _ connector.SourceConnector = (*gcs.Connector)(nil)
	var _ connector.SourceConnector = (*egnyte.Connector)(nil)
	var _ connector.SourceConnector = (*bookstack.Connector)(nil)
	var _ connector.SourceConnector = (*uploadportal.Connector)(nil)
	var _ connector.DeltaSyncer = (*sharepointonprem.Connector)(nil)
	var _ connector.DeltaSyncer = (*azureblob.Connector)(nil)
	var _ connector.DeltaSyncer = (*gcs.Connector)(nil)
	var _ connector.DeltaSyncer = (*egnyte.Connector)(nil)
	var _ connector.DeltaSyncer = (*bookstack.Connector)(nil)
	var _ connector.WebhookReceiver = (*uploadportal.Connector)(nil)

	// Round-20 Task 12 — compile-time assertions for the 8 new
	// connectors. Mirrors the per-round blocks above.
	var _ connector.SourceConnector = (*zendesk.Connector)(nil)
	var _ connector.SourceConnector = (*servicenow.Connector)(nil)
	var _ connector.SourceConnector = (*freshdesk.Connector)(nil)
	var _ connector.SourceConnector = (*airtable.Connector)(nil)
	var _ connector.SourceConnector = (*trello.Connector)(nil)
	var _ connector.SourceConnector = (*intercom.Connector)(nil)
	var _ connector.SourceConnector = (*webex.Connector)(nil)
	var _ connector.SourceConnector = (*bitbucket.Connector)(nil)
	var _ connector.DeltaSyncer = (*zendesk.Connector)(nil)
	var _ connector.DeltaSyncer = (*servicenow.Connector)(nil)
	var _ connector.DeltaSyncer = (*freshdesk.Connector)(nil)
	var _ connector.DeltaSyncer = (*airtable.Connector)(nil)
	var _ connector.DeltaSyncer = (*trello.Connector)(nil)
	var _ connector.DeltaSyncer = (*intercom.Connector)(nil)
	var _ connector.DeltaSyncer = (*webex.Connector)(nil)
	var _ connector.DeltaSyncer = (*bitbucket.Connector)(nil)
}

// TestConnectorContract_Round17_DeltaSyncerEmptyCursor exercises
// the bootstrap contract against heterogeneous Round-17 surfaces:
// Graph delta-token (Entra ID), changed-since (BambooHR), and
// sitemap lastmod (Sitemap). Each must return zero changes plus
// a non-empty cursor on an empty-cursor call.
func TestConnectorContract_Round17_DeltaSyncerEmptyCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"entra_id", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"value":[{"id":"O1"}]}`)
			})
			mux.HandleFunc("/users/delta", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"@odata.deltaLink":"https://graph.test/users/delta?$deltatoken=AAA","value":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(entraid.Credentials{AccessToken: "tok"})
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
		{"bamboohr", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"employees":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(bamboohr.Credentials{APIKey: "k", Subdomain: "acme"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "employees"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
		{"sitemap", func(t *testing.T) {
			var selfURL string
			mux := http.NewServeMux()
			mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>`+selfURL+`/a</loc><lastmod>2024-06-01T00:00:00Z</lastmod></url></urlset>`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			selfURL = srv.URL
			c := sitemap.New(sitemap.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(sitemap.Credentials{SitemapURLs: []string{srv.URL + "/sitemap.xml"}})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: srv.URL + "/sitemap.xml"}, "")
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

// TestConnectorContract_Round18_DeltaSyncerEmptyCursor exercises
// the bootstrap contract against heterogeneous Round-18 surfaces:
// Azure Blob (XML list + Last-Modified), GCS (JSON list + updated
// timestamp), and BookStack (changed-since + updated_at sort).
// Each must return zero changes plus a non-empty cursor on an
// empty-cursor call.
func TestConnectorContract_Round18_DeltaSyncerEmptyCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"azure_blob", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/ctr", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusOK)

					return
				}
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<?xml version="1.0"?><EnumerationResults><Blobs></Blobs></EnumerationResults>`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := azureblob.New(azureblob.WithBaseURL(srv.URL), azureblob.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(azureblob.Credentials{Account: "acct", Container: "ctr", SASToken: "sv=2024&sig=ok"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ctr"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
		{"gcs", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/storage/v1/b/b", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"name":"b"}`)
			})
			mux.HandleFunc("/storage/v1/b/b/o", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"items":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := gcs.New(gcs.WithBaseURL(srv.URL), gcs.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(gcs.Credentials{AccessToken: "t", Bucket: "b"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "b"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
		{"bookstack", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/books", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[]}`)
			})
			mux.HandleFunc("/api/pages", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[{"id":1,"updated_at":"2026-01-01T00:00:00Z"}]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := bookstack.New(bookstack.WithBaseURL(srv.URL), bookstack.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(bookstack.Credentials{TokenID: "id", TokenSecret: "secret"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "pages"}, "")
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

// TestConnectorContract_Round20_DeltaSyncerEmptyCursor exercises
// the bootstrap contract against three heterogeneous Round-20
// surfaces: Zendesk's incremental-export (unix watermark),
// ServiceNow's sys_updated_on (ServiceNow-format timestamp), and
// Bitbucket's q= query (RFC-3339 ISO 8601). Each must return
// zero changes plus a non-empty cursor on an empty-cursor call.
func TestConnectorContract_Round20_DeltaSyncerEmptyCursor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"zendesk", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"user":{"id":1}}`)
			})
			mux.HandleFunc("/api/v2/incremental/tickets.json", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"tickets":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(zendesk.Credentials{Subdomain: "acme", Email: "a@b", APIToken: "tok"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "tickets"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
		{"servicenow", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/now/table/sys_user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"result":[]}`)
			})
			mux.HandleFunc("/api/now/table/incident", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"result":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := servicenow.New(servicenow.WithBaseURL(srv.URL), servicenow.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(servicenow.Credentials{Instance: "x", Username: "u", Password: "p"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "incident"}, "")
			if err != nil || len(ch) != 0 || cur == "" {
				t.Fatalf("delta bootstrap: cur=%q ch=%v err=%v", cur, ch, err)
			}
		}},
		{"bitbucket", func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"slug":"r"}`)
			})
			mux.HandleFunc("/2.0/repositories/ws/r/pullrequests", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"values":[]}`)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
			creds, _ := json.Marshal(bitbucket.Credentials{Username: "u", AppPassword: "p", Workspace: "ws", Repo: "r"})
			cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
			conn, err := c.Connect(context.Background(), cfg)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			ch, cur, err := c.DeltaSync(context.Background(), conn, connector.Namespace{ID: "ws/r"}, "")
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
