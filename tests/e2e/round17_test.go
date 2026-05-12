//go:build e2e

// Round-17 Task 11 — Round-17 end-to-end smoke.
//
// Round 17 expands the connector catalog from 28 → 36 by adding
// entra_id, google_workspace, outlook, workday, bamboohr,
// personio, sitemap, and coda. This file exercises the Round-17
// surface as an integrated bundle:
//
//	(a) Registry now reports exactly 36 entries (28 → 36).
//	(b) Two heterogeneous Round-17 connector lifecycles
//	    (entra_id / bamboohr) walk Validate → Connect →
//	    ListNamespaces → ListDocuments → FetchDocument →
//	    DeltaSync.
//	(c) connector.ErrRateLimited propagates through DeltaSync
//	    when each new connector's upstream returns 429.
//	(d) DeltaSync bootstrap returns a "now" cursor without
//	    backfilling history (DESC+limit=1 contract from
//	    Round 15/16).
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
	"github.com/kennguy3n/hunting-fishball/internal/connector/bamboohr"
	"github.com/kennguy3n/hunting-fishball/internal/connector/coda"
	entraid "github.com/kennguy3n/hunting-fishball/internal/connector/entra_id"
	googleworkspace "github.com/kennguy3n/hunting-fishball/internal/connector/google_workspace"
	"github.com/kennguy3n/hunting-fishball/internal/connector/outlook"
	"github.com/kennguy3n/hunting-fishball/internal/connector/personio"
	"github.com/kennguy3n/hunting-fishball/internal/connector/sitemap"
	"github.com/kennguy3n/hunting-fishball/internal/connector/workday"
)

// TestRound17_RegistryCount verifies the catalog expansion
// contract: blank-imports from the e2e package populate exactly
// 36 connector entries after Round 17's entra_id/google_workspace/
// outlook/workday/bamboohr/personio/sitemap/coda additions.
func TestRound17_RegistryCount(t *testing.T) {
	t.Parallel()
	names := connector.ListSourceConnectors()
	if len(names) != 36 {
		t.Fatalf("registry: %d entries, want 36: %v", len(names), names)
	}
}

// TestRound17_EntraIDLifecycle walks the full SourceConnector +
// DeltaSyncer surface for the new Microsoft Entra ID connector.
// Entra ID is the canonical "Graph delta-token" delta connector
// and is Round 17's proof that @odata.deltaLink bootstrap
// composes with the registry.
func TestRound17_EntraIDLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/organization", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"id":"O1","displayName":"Org"}]}`)
	})
	mux.HandleFunc("/users", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[{"id":"U1","displayName":"Alice"}]}`)
	})
	mux.HandleFunc("/groups", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":[]}`)
	})
	mux.HandleFunc("/users/U1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"U1","displayName":"Alice","userPrincipalName":"alice@x"}`)
	})
	mux.HandleFunc("/users/delta", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"@odata.deltaLink":"https://graph.test/users/delta?$deltatoken=AAA","value":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := entraid.New(entraid.WithBaseURL(srv.URL), entraid.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(entraid.Credentials{AccessToken: "tok"})
	cfg := connector.ConnectorConfig{TenantID: "t1", SourceID: "src1", Credentials: creds}
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("list namespaces: %v / len=%d", err, len(ns))
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one document; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch document: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	// Bootstrap: empty cursor must return the Graph delta link
	// without backfilling history.
	changes, cur, err := c.DeltaSync(ctx, conn, connector.Namespace{ID: "users"}, "")
	if err != nil {
		t.Fatalf("delta bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur == "" {
		t.Fatal("delta bootstrap cursor empty")
	}
}

// TestRound17_BambooHRLifecycle exercises BambooHR's
// changed-since incremental surface. BambooHR uses basic auth
// (api_key + "x") and exposes /v1/employees/changed?since=… as
// the delta endpoint — distinct from the Graph delta token
// pattern.
func TestRound17_BambooHRLifecycle(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/employees/directory", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"employees":[{"id":"1","displayName":"Alice"}]}`)
	})
	mux.HandleFunc("/v1/employees/1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"1","displayName":"Alice","workEmail":"alice@x"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := bamboohr.New(bamboohr.WithBaseURL(srv.URL), bamboohr.WithHTTPClient(srv.Client()))
	creds, _ := json.Marshal(bamboohr.Credentials{APIKey: "k", Subdomain: "acme"})
	cfg := connector.ConnectorConfig{TenantID: "t1", SourceID: "src1", Credentials: creds}
	ctx := context.Background()
	if err := c.Validate(ctx, cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	conn, err := c.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	ns, err := c.ListNamespaces(ctx, conn)
	if err != nil || len(ns) == 0 {
		t.Fatalf("list namespaces: %v / len=%d", err, len(ns))
	}
	it, err := c.ListDocuments(ctx, conn, ns[0], connector.ListOpts{})
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next(ctx) {
		t.Fatalf("expected at least one document; err=%v", it.Err())
	}
	doc, err := c.FetchDocument(ctx, conn, it.Doc())
	if err != nil {
		t.Fatalf("fetch document: %v", err)
	}
	if doc.Content != nil {
		_ = doc.Content.Close()
	}
	changes, cur, err := c.DeltaSync(ctx, conn, ns[0], "")
	if err != nil {
		t.Fatalf("delta bootstrap: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("bootstrap emitted %d changes", len(changes))
	}
	if cur == "" {
		t.Fatal("bootstrap cursor empty")
	}
}

// TestRound17_RateLimitSweep runs all 8 new Round-17 connectors
// through a 429 fixture and verifies every one wraps the
// upstream rate-limit status with connector.ErrRateLimited.
func TestRound17_RateLimitSweep(t *testing.T) {
	rl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate", http.StatusTooManyRequests)
	}))
	defer rl.Close()

	type probe struct {
		name string
		run  func() error
	}
	for _, p := range []probe{
		{"entra_id", func() error {
			c := entraid.New(entraid.WithBaseURL(rl.URL), entraid.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(entraid.Credentials{AccessToken: "t"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"google_workspace", func() error {
			c := googleworkspace.New(googleworkspace.WithBaseURL(rl.URL), googleworkspace.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(googleworkspace.Credentials{AccessToken: "t"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"outlook", func() error {
			c := outlook.New(outlook.WithBaseURL(rl.URL), outlook.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(outlook.Credentials{AccessToken: "t"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"workday", func() error {
			c := workday.New(workday.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(workday.Credentials{AccessToken: "t", TenantURL: rl.URL})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"bamboohr", func() error {
			c := bamboohr.New(bamboohr.WithBaseURL(rl.URL), bamboohr.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(bamboohr.Credentials{APIKey: "k", Subdomain: "acme"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"personio", func() error {
			c := personio.New(personio.WithBaseURL(rl.URL), personio.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(personio.Credentials{ClientID: "c", ClientSecret: "s"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
		{"sitemap", func() error {
			c := sitemap.New(sitemap.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(sitemap.Credentials{SitemapURLs: []string{rl.URL + "/sitemap.xml"}})
			conn, cerr := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})
			if cerr != nil {
				return cerr
			}
			it, _ := c.ListDocuments(context.Background(), conn, connector.Namespace{ID: rl.URL + "/sitemap.xml"}, connector.ListOpts{})
			defer func() { _ = it.Close() }()
			for it.Next(context.Background()) {
			}

			return it.Err()
		}},
		{"coda", func() error {
			c := coda.New(coda.WithBaseURL(rl.URL), coda.WithHTTPClient(rl.Client()))
			creds, _ := json.Marshal(coda.Credentials{AccessToken: "t"})
			_, err := c.Connect(context.Background(), connector.ConnectorConfig{TenantID: "t", SourceID: "s", Credentials: creds})

			return err
		}},
	} {
		p := p
		t.Run(p.name, func(t *testing.T) {
			err := p.run()
			if !errors.Is(err, connector.ErrRateLimited) {
				t.Fatalf("%s: expected ErrRateLimited, got %v", p.name, err)
			}
		})
	}
}
