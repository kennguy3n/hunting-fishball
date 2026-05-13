//go:build e2e

// Round-20 Task 10 — end-to-end smoke for the Round-20 surface.
//
// Round 20/21 expands the connector catalog from 42 → 50 by adding
// zendesk, servicenow, freshdesk, airtable, trello, intercom,
// webex, and bitbucket. This file exercises the Round-20 surface
// as an integrated bundle:
//
//	(a) Registry now reports exactly 50 entries (42 → 50) and the
//	    eight new connectors are all present.
//	(b) Two heterogeneous Round-20 connector lifecycles
//	    (zendesk incremental ticket export + bitbucket pull
//	    request query) walk Validate → Connect → ListNamespaces →
//	    ListDocuments → FetchDocument → DeltaSync.
//	(c) connector.ErrRateLimited propagates through Connect when
//	    each new connector's upstream returns 429 — a 429
//	    propagation sweep across all eight new connectors.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/hunting-fishball/internal/connector"
	"github.com/kennguy3n/hunting-fishball/internal/connector/airtable"
	"github.com/kennguy3n/hunting-fishball/internal/connector/bitbucket"
	"github.com/kennguy3n/hunting-fishball/internal/connector/freshdesk"
	"github.com/kennguy3n/hunting-fishball/internal/connector/intercom"
	"github.com/kennguy3n/hunting-fishball/internal/connector/servicenow"
	"github.com/kennguy3n/hunting-fishball/internal/connector/trello"
	"github.com/kennguy3n/hunting-fishball/internal/connector/webex"
	"github.com/kennguy3n/hunting-fishball/internal/connector/zendesk"
)

// TestRound20_RegistryCount verifies the catalog expansion contract:
// blank-imports from the e2e package populate exactly 50 connector
// entries after Round 20's eight new connectors land.
func TestRound20_RegistryCount(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) != 50 {
		t.Fatalf("registry: %d entries, want 50: %v", len(names), names)
	}
	required := []string{
		"zendesk", "servicenow", "freshdesk", "airtable",
		"trello", "intercom", "webex", "bitbucket",
	}
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	for _, r := range required {
		if !have[r] {
			t.Fatalf("registry missing Round-20 connector %q: %v", r, names)
		}
	}
}

// TestRound20_ZendeskLifecycle walks the full Zendesk lifecycle:
// Validate → Connect → ListNamespaces → ListDocuments →
// FetchDocument → DeltaSync (empty cursor + advancing cursor).
func TestRound20_ZendeskLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/users/me.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"user":{"id":1}}`)
	})
	mux.HandleFunc("/api/v2/tickets.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tickets":[{"id":1,"subject":"Down","description":"x","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}]}`)
	})
	mux.HandleFunc("/api/v2/tickets/1.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ticket":{"id":1,"subject":"Down","description":"x","updated_at":"2024-06-01T00:00:00Z","created_at":"2024-05-01T00:00:00Z"}}`)
	})
	mux.HandleFunc("/api/v2/incremental/tickets.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"tickets":[{"id":2,"subject":"Up","description":"y","updated_at":"2024-06-02T00:00:00Z","created_at":"2024-05-02T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := zendesk.New(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(zendesk.Credentials{Subdomain: "acme", Email: "a@b", APIToken: "tok"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("ListNamespaces: %v ns=%v", err, ns)
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one doc; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	_ = doc.Content.Close()
	bootstrap, cursor, err := c.DeltaSync(ctx, conn, ns[0], "")
	if err != nil || len(bootstrap) != 0 || cursor == "" {
		t.Fatalf("DeltaSync bootstrap: changes=%v cursor=%q err=%v", bootstrap, cursor, err)
	}
	changes, _, err := c.DeltaSync(ctx, conn, ns[0], "1")
	if err != nil {
		t.Fatalf("DeltaSync incremental: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("DeltaSync incremental: changes=%v", changes)
	}
}

// TestRound20_BitbucketLifecycle walks the full Bitbucket lifecycle:
// Validate → Connect → ListNamespaces → ListDocuments →
// FetchDocument → DeltaSync.
func TestRound20_BitbucketLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/repositories/ws/r", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"slug":"r"}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"values":[{"id":42,"title":"Bump deps","updated_on":"2024-06-01T00:00:00Z","created_on":"2024-05-01T00:00:00Z","state":"OPEN","summary":{"raw":"body"}}]}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/r/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":42,"title":"Bump deps","updated_on":"2024-06-01T00:00:00Z","created_on":"2024-05-01T00:00:00Z","state":"OPEN","summary":{"raw":"body"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bitbucket.New(bitbucket.WithBaseURL(srv.URL), bitbucket.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(bitbucket.Credentials{Username: "u", AppPassword: "p", Workspace: "ws", Repo: "r"})
	cfg := connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds}
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("ListNamespaces: %v ns=%v", err, ns)
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one doc; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("FetchDocument: %v", err)
	}
	_ = doc.Content.Close()
	if _, cursor, err := c.DeltaSync(ctx, conn, ns[0], ""); err != nil || cursor == "" {
		t.Fatalf("DeltaSync bootstrap: cursor=%q err=%v", cursor, err)
	}
}

// TestRound20_RateLimitSweep verifies all 8 new connectors map a
// 429 from their primary endpoint to connector.ErrRateLimited via
// the Connect surface.
func TestRound20_RateLimitSweep(t *testing.T) {
	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer rl.Close()

	type probe struct {
		name string
		run  func() error
	}
	probes := []probe{
		{"zendesk", func() error {
			c := zendesk.New(zendesk.WithBaseURL(rl.URL), zendesk.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(zendesk.Credentials{Subdomain: "x", Email: "a@b", APIToken: "tok"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"servicenow", func() error {
			c := servicenow.New(servicenow.WithBaseURL(rl.URL), servicenow.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(servicenow.Credentials{Instance: "x", Username: "u", Password: "p"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"freshdesk", func() error {
			c := freshdesk.New(freshdesk.WithBaseURL(rl.URL), freshdesk.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(freshdesk.Credentials{Domain: "x", APIKey: "k"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"airtable", func() error {
			c := airtable.New(airtable.WithBaseURL(rl.URL), airtable.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(airtable.Credentials{AccessToken: "tok", BaseID: "B", Table: "T"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"trello", func() error {
			c := trello.New(trello.WithBaseURL(rl.URL), trello.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(trello.Credentials{APIKey: "k", Token: "tok", BoardID: "B"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"intercom", func() error {
			c := intercom.New(intercom.WithBaseURL(rl.URL), intercom.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(intercom.Credentials{AccessToken: "oat"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"webex", func() error {
			c := webex.New(webex.WithBaseURL(rl.URL), webex.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(webex.Credentials{AccessToken: "oat", RoomID: "R1"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"bitbucket", func() error {
			c := bitbucket.New(bitbucket.WithBaseURL(rl.URL), bitbucket.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(bitbucket.Credentials{Username: "u", AppPassword: "p", Workspace: "ws", Repo: "r"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
	}
	for _, p := range probes {
		p := p
		t.Run(p.name, func(t *testing.T) {
			err := p.run()
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("%s: expected ErrRateLimited, got %v", p.name, err)
			}
		})
	}
}
